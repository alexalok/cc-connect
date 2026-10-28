package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ArchivedSessionIDs reconciles the durable result of thread/archive. Each
// bridge session has its own app-server process, so notifications from another
// client are not a reliable subscription, especially across bridge downtime.
// Absence from sessions is never evidence of an archive.
func (a *Agent) ArchivedSessionIDs(ctx context.Context, ids []string) ([]string, error) {
	if len(ids) == 0 {
		return nil, ctx.Err()
	}
	a.mu.RLock()
	home, err := a.sessionCodexHomeLocked()
	a.mu.RUnlock()
	if err != nil {
		return nil, fmt.Errorf("codex: archived sessions profile: %w", err)
	}
	wanted := make(map[string]bool, len(ids))
	for _, id := range ids {
		if id != "" {
			wanted[id] = true
		}
	}
	root := resolveCodexHomeDir(home)
	archived, err := rolloutSessionIDs(ctx, filepath.Join(root, "archived_sessions"), wanted)
	if err != nil {
		return nil, fmt.Errorf("codex: read archived sessions: %w", err)
	}
	if len(archived) == 0 {
		return nil, nil
	}
	// A resumed conversation can have several rollout segments. An old
	// archived segment must not remove a topic for a currently live session.
	active, err := rolloutSessionIDs(ctx, filepath.Join(root, "sessions"), archived)
	if err != nil {
		return nil, fmt.Errorf("codex: verify archived sessions: %w", err)
	}
	var result []string
	for id := range archived {
		if !active[id] {
			result = append(result, id)
		}
	}
	sort.Strings(result)
	return result, nil
}

// Read only the first record: forks can contain copied parent metadata later
// in their transcripts. Names, mtimes, and the discovery cutoff do not identify
// an archive; chats created through the bridge must also be recognized here.
func rolloutSessionIDs(ctx context.Context, root string, wanted map[string]bool) (map[string]bool, error) {
	found := make(map[string]bool)
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if !entry.Type().IsRegular() || !strings.HasSuffix(entry.Name(), ".jsonl") {
			return nil
		}
		id, err := rolloutSessionID(path)
		if os.IsNotExist(err) {
			return nil
		} // concurrently archived/unarchived
		if err != nil {
			return fmt.Errorf("read metadata %s: %w", path, err)
		}
		if wanted[id] {
			found[id] = true
		}
		return nil
	})
	return found, err
}

func rolloutSessionID(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 4096), 1024*1024)
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return "", err
		}
		return "", fmt.Errorf("empty rollout")
	}
	var record struct {
		Type    string `json:"type"`
		Payload struct {
			ID string `json:"id"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
		return "", err
	}
	if record.Type != "session_meta" || record.Payload.ID == "" {
		return "", fmt.Errorf("missing session identity")
	}
	return record.Payload.ID, nil
}
