package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

// DiscoverSessions reads metadata rather than scanning complete transcripts on
// every poll. In particular it must not call ListSessions, which normalizes the
// source of exec transcripts as a side effect.
func (a *Agent) DiscoverSessions(ctx context.Context, since time.Time) ([]core.AgentSessionInfo, error) {
	a.mu.RLock()
	home, err := a.sessionCodexHomeLocked()
	workDir := a.workDir
	if a.listAllSessions {
		workDir = ""
	}
	a.mu.RUnlock()
	if err != nil {
		return nil, err
	}
	if workDir != "" {
		workDir, err = filepath.Abs(workDir)
		if err != nil {
			return nil, fmt.Errorf("codex: discovery work directory: %w", err)
		}
	}
	return discoverCodexSessions(ctx, home, workDir, since)
}

type discoveredCodexSession struct {
	info      core.AgentSessionInfo
	createdAt time.Time
	inScope   bool
}

func discoverCodexSessions(ctx context.Context, home, workDir string, since time.Time) ([]core.AgentSessionInfo, error) {
	titles := loadCodexSessionTitles(home)
	byID := make(map[string]discoveredCodexSession)
	root := filepath.Join(resolveCodexHomeDir(home), "sessions")
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			if os.IsNotExist(err) { // An empty profile or concurrently archived chat.
				return nil
			}
			return err
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") {
			return nil
		}
		info, err := readDiscoveredSession(path, workDir, since, titles)
		if err != nil {
			// A writer may not have finished its first record yet. Retry next poll.
			slog.Warn("codex: discovery metadata unavailable; will retry", "path", path, "error", err)
			return nil
		}
		if info != nil {
			// A resumed chat can have several rollout segments. A new segment
			// must not make an old logical conversation eligible for import.
			previous, exists := byID[info.info.ID]
			if exists {
				if previous.createdAt.Before(info.createdAt) {
					info.createdAt = previous.createdAt
				}
				info.inScope = info.inScope || previous.inScope
				if info.info.Summary == "" {
					info.info.Summary = previous.info.Summary
				}
			}
			byID[info.info.ID] = *info
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("codex: discover sessions: %w", err)
	}
	sessions := make([]core.AgentSessionInfo, 0, len(byID))
	for _, info := range byID {
		if info.inScope && !info.createdAt.Before(since) {
			sessions = append(sessions, info.info)
		}
	}
	sort.Slice(sessions, func(i, j int) bool { return sessions[i].ID < sessions[j].ID })
	return sessions, nil
}

func readDiscoveredSession(path, workDir string, since time.Time, titles map[string]string) (*discoveredCodexSession, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	// Titles need only the beginning of the rollout, even for very long chats.
	scanner := bufio.NewScanner(io.LimitReader(f, 1024*1024))
	scanner.Buffer(make([]byte, 4096), 1024*1024)
	// Identity always comes from the first record; later records can contain
	// copied parent metadata in forks and subagents.
	if !scanner.Scan() {
		return nil, scanner.Err()
	}
	var record struct {
		Type    string `json:"type"`
		Payload struct {
			ID         string          `json:"id"`
			Timestamp  time.Time       `json:"timestamp"`
			Cwd        string          `json:"cwd"`
			Source     json.RawMessage `json:"source"`
			Originator string          `json:"originator"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
		return nil, err
	}
	meta := record.Payload
	// Use the session's creation time, not file mtime or a resumed segment's
	// event timestamp: touching an old conversation must never import it.
	if record.Type != "session_meta" || meta.ID == "" || isSubagentSessionSource(meta.Source) ||
		strings.HasPrefix(meta.Originator, "cc-connect") {
		return nil, nil
	}
	var source string
	if json.Unmarshal(meta.Source, &source) != nil || source == "exec" {
		return nil, nil
	}
	info := &discoveredCodexSession{info: core.AgentSessionInfo{ID: meta.ID}, createdAt: meta.Timestamp,
		inScope: workDir == "" || filepath.Clean(meta.Cwd) == workDir}
	if !info.inScope || meta.Timestamp.Before(since) {
		return info, nil
	}
	title := strings.TrimSpace(titles[meta.ID])
	if title == "" {
		title = discoveredSessionPreview(scanner)
	}
	info.info.Summary = title
	return info, nil
}

func discoveredSessionPreview(scanner *bufio.Scanner) string {
	for scanner.Scan() {
		var record struct {
			Type    string `json:"type"`
			Payload struct {
				Role    string `json:"role"`
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"payload"`
		}
		if json.Unmarshal(scanner.Bytes(), &record) != nil || record.Type != "response_item" || record.Payload.Role != "user" {
			continue
		}
		for _, item := range record.Payload.Content {
			if item.Type == "input_text" && isUserPrompt(item.Text) {
				preview := []rune(strings.Join(strings.Fields(item.Text), " "))
				if len(preview) > 128 {
					preview = preview[:128]
				}
				return string(preview)
			}
		}
	}
	return ""
}
