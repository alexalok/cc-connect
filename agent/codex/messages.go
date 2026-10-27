package codex

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

const mirrorTurnOrigin = "cc-connect"

func (a *Agent) ValidateSessionMessageSync() error {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.backend != "app_server" {
		return fmt.Errorf("codex: conversation mirroring requires backend = %q", "app_server")
	}
	return nil
}

// ReadSessionMessages uses UI events, not model response items: the latter
// also contain injected user-role instructions, compaction, and tool context.
func (a *Agent) ReadSessionMessages(ctx context.Context, sessionID string) ([]core.SessionMessage, error) {
	a.mu.RLock()
	home, err := a.sessionCodexHomeLocked()
	a.mu.RUnlock()
	if err != nil {
		return nil, err
	}
	if sessionID == "" {
		return nil, fmt.Errorf("codex: empty conversation ID")
	}
	// Inactive chats must not require rescanning their entire transcripts on
	// every tick. The cache is private to the reader and bounded in size.
	a.messageCacheMu.Lock()
	defer a.messageCacheMu.Unlock()
	var records []visibleCodexMessage
	root := filepath.Join(resolveCodexHomeDir(home), "sessions")
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if os.IsNotExist(walkErr) {
			return nil
		} // concurrently archived, or an empty profile
		if walkErr != nil {
			return walkErr
		}
		if !entry.Type().IsRegular() || !strings.HasSuffix(entry.Name(), ".jsonl") || !strings.Contains(entry.Name(), sessionID) {
			return nil
		}
		messages, err := a.cachedVisibleMessages(ctx, path, sessionID)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		records = append(records, messages...)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("codex: read conversation messages: %w", err)
	}
	sort.SliceStable(records, func(i, j int) bool { return records[i].at.Before(records[j].at) })
	seen := make(map[string]int)
	var messages []core.SessionMessage
	for _, record := range records {
		if index, exists := seen[record.message.ID]; exists {
			// The legacy event may arrive first and omit attachment details.
			// Keep one message, enriching it with the UI event's media count.
			if record.message.Attachments > messages[index].Attachments {
				messages[index].Attachments = record.message.Attachments
			}
			continue
		}
		seen[record.message.ID] = len(messages)
		messages = append(messages, record.message)
	}
	return messages, nil
}

type visibleCodexMessage struct {
	message core.SessionMessage
	at      time.Time
}

type visibleMessageCache struct {
	size     int64
	modified time.Time
	messages []visibleCodexMessage
}

// Caller holds messageCacheMu. Only fully parsed snapshots are cached.
func (a *Agent) cachedVisibleMessages(ctx context.Context, path, sessionID string) ([]visibleCodexMessage, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	key := path + "\x00" + sessionID
	if cached, ok := a.messageCache[key]; ok && cached.size == info.Size() && cached.modified.Equal(info.ModTime()) {
		return cached.messages, nil
	}
	messages, err := readVisibleCodexMessages(ctx, path, sessionID)
	if err != nil {
		return nil, err
	}
	if a.messageCache == nil || len(a.messageCache) >= 128 {
		a.messageCache = make(map[string]visibleMessageCache)
	}
	a.messageCache[key] = visibleMessageCache{size: info.Size(), modified: info.ModTime(), messages: messages}
	return messages, nil
}

type visibleCodexRecord struct {
	Timestamp time.Time `json:"timestamp"`
	Type      string    `json:"type"`
	Payload   struct {
		ID              string            `json:"id"`
		Source          json.RawMessage   `json:"source"`
		Type            string            `json:"type"`
		TurnID          string            `json:"turn_id"`
		Message         string            `json:"message"`
		Phase           string            `json:"phase"`
		Images          []json.RawMessage `json:"images"`
		LocalImages     []json.RawMessage `json:"local_images"`
		TurnAttribution struct {
			TurnTrigger  string `json:"turn_trigger"`
			ParentTurnID string `json:"parent_turn_id"`
			RootTurnID   string `json:"root_turn_id"`
		} `json:"turn_attribution"`
		Item struct {
			Type     string `json:"type"`
			ID       string `json:"id"`
			ClientID string `json:"client_id"`
			Phase    string `json:"phase"`
			Content  []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"item"`
	} `json:"payload"`
}

func readVisibleCodexMessages(ctx context.Context, path, sessionID string) ([]visibleCodexMessage, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), maxCodexJSONLLineSize)
	// Never acknowledge a partially written final record, even if its JSON
	// happens to parse before the writer appends the newline.
	scanner.Split(func(data []byte, _ bool) (int, []byte, error) {
		if end := bytes.IndexByte(data, '\n'); end >= 0 {
			return end + 1, data[:end], nil
		}
		return 0, nil, nil
	})
	if !scanner.Scan() {
		return nil, scanner.Err()
	}
	var meta visibleCodexRecord
	if err := json.Unmarshal(scanner.Bytes(), &meta); err != nil {
		return nil, fmt.Errorf("decode conversation identity: %w", err)
	}
	if meta.Type != "session_meta" || meta.Payload.ID != sessionID || isSubagentSessionSource(meta.Payload.Source) {
		return nil, nil
	}
	return readVisibleCodexEvents(ctx, scanner)
}

func readVisibleCodexEvents(ctx context.Context, scanner *bufio.Scanner) ([]visibleCodexMessage, error) {
	var messages []visibleCodexMessage
	owned := make(map[string]bool)
	occurrences := make(map[string]int)
	turnID := ""
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		line := scanner.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		record, err := decodeVisibleCodexEvent(line)
		if err != nil {
			return nil, err
		}
		if record == nil {
			continue
		}
		p := record.Payload
		if p.Type == "task_started" {
			turnID = p.TurnID
			origin := p.TurnAttribution
			owned[turnID] = origin.TurnTrigger == mirrorTurnOrigin || owned[origin.ParentTurnID] || owned[origin.RootTurnID]
			continue
		}
		messageTurn := p.TurnID
		if messageTurn == "" {
			messageTurn = turnID
		}
		botInput := strings.HasPrefix(p.Item.ClientID, mirrorTurnOrigin+":")
		if botInput {
			owned[messageTurn] = true
		}
		message, format := visibleMessageContent(*record)
		if message.Role == "" {
			continue
		}
		// A client can add input to an active turn. Do not retroactively hide
		// earlier external messages, or a desktop user's input to a bot turn.
		if (message.Role == "assistant" && owned[messageTurn]) ||
			(message.Role == "user" && (botInput || (owned[messageTurn] && p.Item.ClientID == ""))) {
			continue
		}
		identityTurn := messageTurn
		if identityTurn == "" {
			identityTurn = record.Timestamp.Format(time.RFC3339Nano)
		}
		// Legacy events can omit media details, so attachments must not be
		// part of the identity shared with the richer UI representation.
		fingerprint := fmt.Sprintf("%x", sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%s", identityTurn, message.Role, message.Content))))
		// Separate counters for legacy and current UI event formats allow
		// dual-written events to deduplicate while preserving repeated text.
		occurrences[format+fingerprint]++
		message.ID = fmt.Sprintf("%s:%d", fingerprint, occurrences[format+fingerprint])
		messages = append(messages, visibleCodexMessage{message: message, at: record.Timestamp})
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return messages, nil
}

func decodeVisibleCodexEvent(line []byte) (*visibleCodexRecord, error) {
	// Tool and model-response payloads have different shapes and may contain
	// private context. Decode only the UI events used for conversation display.
	var envelope struct {
		Type    string `json:"type"`
		Payload struct {
			Type string `json:"type"`
			Item struct {
				Type string `json:"type"`
			} `json:"item"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(line, &envelope); err != nil {
		return nil, fmt.Errorf("decode conversation record: %w", err)
	}
	if envelope.Type != "event_msg" {
		return nil, nil
	}
	switch envelope.Payload.Type {
	case "task_started", "user_message", "agent_message":
	case "item_completed":
		kind := strings.ToLower(envelope.Payload.Item.Type)
		if kind != "usermessage" && kind != "agentmessage" {
			return nil, nil
		}
	default:
		return nil, nil
	}
	var record visibleCodexRecord
	if err := json.Unmarshal(line, &record); err != nil {
		return nil, fmt.Errorf("decode conversation event: %w", err)
	}
	return &record, nil
}

func visibleMessageContent(record visibleCodexRecord) (core.SessionMessage, string) {
	p := record.Payload
	message := core.SessionMessage{}
	if p.Type == "user_message" {
		message.Role, message.Content = "user", p.Message
		message.Attachments = len(p.Images) + len(p.LocalImages)
	}
	if p.Type == "agent_message" && visibleMessagePhase(p.Phase) {
		message.Role, message.Content = "assistant", p.Message
	}
	if p.Type != "item_completed" {
		return message, "legacy"
	}
	switch strings.ToLower(p.Item.Type) {
	case "usermessage":
		message.Role = "user"
	case "agentmessage":
		if visibleMessagePhase(p.Item.Phase) {
			message.Role = "assistant"
		}
	}
	if message.Role == "" {
		return message, "ui"
	}
	var text []string
	for _, part := range p.Item.Content {
		switch strings.ToLower(part.Type) {
		case "text", "input_text", "output_text":
			text = append(text, part.Text)
		case "image", "localimage", "file", "localfile", "input_image", "input_file":
			message.Attachments++
		}
	}
	message.Content = strings.Join(text, "\n\n")
	return message, "ui"
}

func visibleMessagePhase(phase string) bool {
	return phase == "" || phase == "commentary" || phase == "final_answer"
}
