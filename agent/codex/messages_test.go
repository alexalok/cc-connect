package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func visibleMessageEvent(kind, turn, client, phase, text string) string {
	data, _ := json.Marshal(map[string]any{
		"timestamp": "2026-10-26T20:23:14Z", "type": "event_msg",
		"payload": map[string]any{"type": "item_completed", "turn_id": turn,
			"item": map[string]any{"type": kind, "id": "native-id", "client_id": client, "phase": phase,
				"content": []map[string]any{{"type": "Text", "text": text}}}},
	})
	return string(data)
}

func visibleTurnEvent(turn, origin string) string {
	return fmt.Sprintf(`{"type":"event_msg","payload":{"type":"task_started","turn_id":%q,"turn_attribution":{"turn_trigger":%q}}}`, turn, origin)
}

func writeVisibleMessages(t *testing.T, home, fileID, metaID string, records ...string) string {
	t.Helper()
	path := filepath.Join(home, "sessions", "rollout-"+fileID+".jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	meta := fmt.Sprintf(`{"type":"session_meta","payload":{"id":%q,"source":"vscode"}}`, metaID)
	if err := os.WriteFile(path, []byte(meta+"\n"+strings.Join(records, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestReadSessionMessages_VisibleConversationWithoutInstructionsToolsOrBotEcho(t *testing.T) {
	home := t.TempDir()
	writeVisibleMessages(t, home, "chat", "chat",
		`{"type":"response_item","payload":{"role":"user","content":[{"type":"input_text","text":"SECRET injected instructions"}]}}`,
		`{"type":"event_msg","payload":{"type":"item_completed","item":{"type":"Reasoning","content":"SECRET private reasoning"}}}`,
		`{"type":"event_msg","payload":{"type":"item_completed","item":{"type":"CommandExecution","content":"SECRET tool output"}}}`,
		visibleTurnEvent("external", "composer"),
		visibleMessageEvent("UserMessage", "external", "desktop-1", "", "<literal user text>"),
		`{"timestamp":"2026-10-26T20:23:14Z","type":"event_msg","payload":{"type":"user_message","message":"<literal user text>"}}`,
		visibleMessageEvent("AgentMessage", "external", "", "commentary", "Working on it"),
		visibleMessageEvent("AgentMessage", "external", "", "analysis", "SECRET hidden phase"),
		visibleMessageEvent("AgentMessage", "external", "", "final_answer", "Answer"),
		visibleMessageEvent("AgentMessage", "external", "", "final_answer", "Answer"),
		visibleTurnEvent("bot", mirrorTurnOrigin),
		visibleMessageEvent("UserMessage", "bot", "", "", "Same question"),
		visibleMessageEvent("AgentMessage", "bot", "", "final_answer", "Bot already delivered this"),
		visibleTurnEvent("steered", "composer"),
		visibleMessageEvent("UserMessage", "steered", "desktop-original", "", "Before the bot joined"),
		visibleMessageEvent("AgentMessage", "steered", "", "commentary", "Original progress"),
		visibleMessageEvent("UserMessage", "steered", "cc-connect:telegram:10", "", "Bot input"),
		visibleMessageEvent("AgentMessage", "steered", "", "final_answer", "Already delivered steered answer"),
		visibleMessageEvent("UserMessage", "steered", "desktop-followup", "", "Desktop input to the same turn"),
		visibleTurnEvent("external-again", "composer"),
		visibleMessageEvent("UserMessage", "external-again", "desktop-2", "", "Same question"),
	)
	a := &Agent{codexHome: home}
	messages, err := a.ReadSessionMessages(context.Background(), "chat")
	if err != nil {
		t.Fatal(err)
	}
	var contents []string
	ids := make(map[string]bool)
	for _, m := range messages {
		contents = append(contents, m.Content)
		if m.ID == "" || ids[m.ID] {
			t.Fatalf("message identity is missing or reused: %s", m.ID)
		}
		ids[m.ID] = true
	}
	want := "<literal user text>|Working on it|Answer|Answer|Before the bot joined|Original progress|Desktop input to the same turn|Same question"
	if got := strings.Join(contents, "|"); got != want {
		t.Fatalf("visible messages = %q, want %q", got, want)
	}
}

func TestReadSessionMessages_PartialAppendAndCopiedSegmentsDoNotDuplicate(t *testing.T) {
	home := t.TempDir()
	question := visibleMessageEvent("UserMessage", "t1", "desktop", "", "Question")
	answer := visibleMessageEvent("AgentMessage", "t1", "", "final_answer", "Answer")
	path := writeVisibleMessages(t, home, "chat", "chat", visibleTurnEvent("t1", "composer"), question)
	appendBytes := func(text string) {
		t.Helper()
		f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.WriteString(text); err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
	}
	appendBytes(answer[:len(answer)/2])
	a := &Agent{codexHome: home}
	before, err := a.ReadSessionMessages(context.Background(), "chat")
	if err != nil || len(before) != 1 {
		t.Fatalf("partial record was consumed: %v, %v", before, err)
	}
	appendBytes(answer[len(answer)/2:] + "\n")
	writeVisibleMessages(t, home, "chat-resumed", "chat", visibleTurnEvent("t1", "composer"), question, answer,
		visibleTurnEvent("t2", "composer"), visibleMessageEvent("UserMessage", "t2", "desktop", "", "Question"))
	after, err := a.ReadSessionMessages(context.Background(), "chat")
	if err != nil || len(after) != 3 {
		t.Fatalf("append/copy handling returned %d messages: %v", len(after), err)
	}
	if before[0].ID != after[0].ID || after[0].ID == after[2].ID {
		t.Fatal("unstable identity or repeated user prompt was deduplicated")
	}
}

func TestReadSessionMessages_RejectsCorruptCompletedRecordAndWrongIdentity(t *testing.T) {
	home := t.TempDir()
	writeVisibleMessages(t, home, "chat-wrong", "different-chat", visibleMessageEvent("UserMessage", "t", "", "", "Wrong chat"))
	a := &Agent{codexHome: home}
	messages, err := a.ReadSessionMessages(context.Background(), "chat")
	if err != nil || len(messages) != 0 {
		t.Fatalf("read a different chat: %v, %v", messages, err)
	}
	writeVisibleMessages(t, home, "chat", "chat", "{broken}")
	if _, err := a.ReadSessionMessages(context.Background(), "chat"); err == nil {
		t.Fatal("corrupt record silently ignored")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := a.ReadSessionMessages(ctx, "chat"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation ignored: %v", err)
	}
}

func TestValidateSessionMessageSync_RequiresAppServerOriginMetadata(t *testing.T) {
	for _, backend := range []string{"", "exec", "app_server"} {
		a := &Agent{backend: backend}
		if err := a.ValidateSessionMessageSync(); (err == nil) != (backend == "app_server") {
			t.Fatalf("backend %q: %v", backend, err)
		}
	}
}

type mirrorRPCWriter struct{ write func([]byte) (int, error) }

func (w mirrorRPCWriter) Write(p []byte) (int, error) { return w.write(p) }
func (w mirrorRPCWriter) Close() error                { return nil }

func TestAppServerSession_SendTagsBotOriginWithoutChangingUserText(t *testing.T) {
	s := &appServerSession{ctx: context.Background(), pending: make(map[int64]chan rpcResponseEnvelope)}
	s.alive.Store(true)
	s.threadID.Store("native")
	captured := make(chan map[string]any, 1)
	s.stdin = mirrorRPCWriter{write: func(data []byte) (int, error) {
		var request struct {
			ID     int64          `json:"id"`
			Method string         `json:"method"`
			Params map[string]any `json:"params"`
		}
		if err := json.Unmarshal(data, &request); err != nil {
			return 0, err
		}
		captured <- request.Params
		s.handleResponse(rpcResponseEnvelope{ID: request.ID, Result: json.RawMessage(`{"turn":{"id":"turn-bot"}}`)})
		return len(data), nil
	}}
	if err := s.Send("Unchanged user question", "telegram-42", nil, nil); err != nil {
		t.Fatal(err)
	}
	params := <-captured
	if params["turnTrigger"] != mirrorTurnOrigin || !strings.HasPrefix(params["clientUserMessageId"].(string), "cc-connect:telegram-42:") {
		t.Fatalf("missing origin tags: %v", params)
	}
	input := params["input"].([]any)[0].(map[string]any)
	if input["text"] != "Unchanged user question" {
		t.Fatalf("origin leaked into user prompt: %v", input)
	}
}
