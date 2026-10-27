package codex

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

type nativeStatusSnapshot struct {
	status core.ConversationStatus
	at     time.Time
	owned  bool
}
type nativeStatusCache struct {
	size     int64
	modified time.Time
	snapshot nativeStatusSnapshot
}

// ReadSessionStatus reads local persisted events without resuming a thread.
// Bridge-owned turns return no status: their live event reader is authoritative.
func (a *Agent) ReadSessionStatus(ctx context.Context, sessionID string) (core.ConversationStatus, error) {
	if sessionID == "" {
		return "", fmt.Errorf("codex: empty conversation ID")
	}
	a.mu.RLock()
	home, err := a.sessionCodexHomeLocked()
	a.mu.RUnlock()
	if err != nil {
		return "", err
	}
	a.statusCacheMu.Lock()
	defer a.statusCacheMu.Unlock()
	var latest nativeStatusSnapshot
	var found bool
	err = filepath.WalkDir(filepath.Join(resolveCodexHomeDir(home), "sessions"), func(path string, entry fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if os.IsNotExist(walkErr) {
			return nil
		}
		if walkErr != nil {
			return walkErr
		}
		if !entry.Type().IsRegular() || !strings.HasSuffix(entry.Name(), ".jsonl") || !strings.Contains(entry.Name(), sessionID) {
			return nil
		}
		info, err := entry.Info()
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		key := path + "\x00" + sessionID
		cached, ok := a.statusCache[key]
		if !ok || cached.size != info.Size() || !cached.modified.Equal(info.ModTime()) {
			snapshot, err := readNativeStatus(ctx, path, sessionID)
			if os.IsNotExist(err) {
				return nil
			}
			if err != nil {
				return err
			}
			cached = nativeStatusCache{info.Size(), info.ModTime(), snapshot}
			if a.statusCache == nil || len(a.statusCache) >= 128 {
				a.statusCache = make(map[string]nativeStatusCache)
			}
			a.statusCache[key] = cached
		}
		if cached.snapshot.status != "" && (!found || !cached.snapshot.at.Before(latest.at)) {
			latest = cached.snapshot
			found = true
		}
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("codex: read topic status: %w", err)
	}
	if latest.owned {
		return "", nil
	}
	return latest.status, nil
}

type nativeStatusRecord struct {
	Timestamp time.Time `json:"timestamp"`
	Type      string    `json:"type"`
	Payload   struct {
		ID              string          `json:"id"`
		Source          json.RawMessage `json:"source"`
		Type            string          `json:"type"`
		TurnID          string          `json:"turn_id"`
		Name            string          `json:"name"`
		CallID          string          `json:"call_id"`
		Output          json.RawMessage `json:"output"`
		WillRetry       bool            `json:"will_retry"`
		TurnAttribution struct {
			TurnTrigger  string `json:"turn_trigger"`
			ParentTurnID string `json:"parent_turn_id"`
			RootTurnID   string `json:"root_turn_id"`
		} `json:"turn_attribution"`
		Item struct {
			Type     string `json:"type"`
			ClientID string `json:"client_id"`
		} `json:"item"`
	} `json:"payload"`
}

func readNativeStatus(ctx context.Context, path, id string) (nativeStatusSnapshot, error) {
	file, err := os.Open(path)
	if err != nil {
		return nativeStatusSnapshot{}, err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), maxCodexJSONLLineSize)
	// A writer can be in the middle of appending a line; only consume complete records.
	scanner.Split(func(data []byte, _ bool) (int, []byte, error) {
		if end := bytes.IndexByte(data, '\n'); end >= 0 {
			return end + 1, data[:end], nil
		}
		return 0, nil, nil
	})
	if !scanner.Scan() {
		return nativeStatusSnapshot{}, scanner.Err()
	}
	var record nativeStatusRecord
	if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
		return nativeStatusSnapshot{}, err
	}
	if record.Type != "session_meta" || record.Payload.ID != id || isSubagentSessionSource(record.Payload.Source) {
		return nativeStatusSnapshot{}, nil
	}
	snapshot := nativeStatusSnapshot{}
	base := core.ConversationStatus("")
	turn := ""
	question := ""
	async := false
	owned := map[string]bool{}
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return nativeStatusSnapshot{}, err
		}
		if len(bytes.TrimSpace(scanner.Bytes())) == 0 {
			continue
		}
		record = nativeStatusRecord{}
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			return nativeStatusSnapshot{}, err
		}
		p := record.Payload
		changed := false
		if record.Type == "event_msg" {
			switch p.Type {
			case "task_started":
				turn = p.TurnID
				origin := p.TurnAttribution
				owned[turn] = origin.TurnTrigger == mirrorTurnOrigin || owned[origin.ParentTurnID] || owned[origin.RootTurnID]
				snapshot.owned = owned[turn]
				base = core.ConversationWorking
				changed = true
			case "task_complete", "turn_aborted", "error":
				if p.TurnID != "" && turn != "" && p.TurnID != turn {
					continue
				}
				if p.Type == "task_complete" {
					if base != core.ConversationFailed && base != core.ConversationStopped {
						base = core.ConversationFinished
					}
				} else if p.Type == "turn_aborted" {
					base = core.ConversationStopped
					question = ""
				} else {
					if p.WillRetry {
						continue
					}
					base = core.ConversationFailed
				}
				changed = true
			case "user_message":
				question = ""
				changed = true
			case "item_completed":
				if strings.EqualFold(p.Item.Type, "UserMessage") {
					question = ""
					changed = true
					if strings.HasPrefix(p.Item.ClientID, mirrorTurnOrigin+":") {
						owned[turn] = true
						snapshot.owned = true
					}
				}
			}
		} else if record.Type == "response_item" {
			switch p.Type {
			case "function_call":
				name := strings.TrimPrefix(p.Name, "functions.")
				if name == "request_user_input" || name == "request_user_input_async" {
					question = p.CallID
					async = name == "request_user_input_async"
					changed = true
				}
			case "function_call_output":
				if question != "" && p.CallID == question {
					// Async tools acknowledge delivery immediately. Only a user answer or a
					// failed delivery resolves them; the turn may complete while still waiting.
					output := p.Output
					var text string
					if json.Unmarshal(output, &text) == nil {
						output = []byte(text)
					}
					var result struct {
						Accepted bool `json:"accepted"`
					}
					if !async || (json.Unmarshal(output, &result) == nil && !result.Accepted) {
						question = ""
						changed = true
					}
				}
			}
		}
		if changed {
			snapshot.status = base
			if question != "" && base != core.ConversationFailed && base != core.ConversationStopped {
				snapshot.status = core.ConversationQuestion
			}
			snapshot.at = record.Timestamp
		}
	}
	return snapshot, scanner.Err()
}
