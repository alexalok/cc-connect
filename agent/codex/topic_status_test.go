package codex

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/chenhg5/cc-connect/core"
)

func TestReadSessionStatus_NativeLifecycleAndPendingQuestion(t *testing.T) {
	for _, tc := range []struct {
		name   string
		events []string
		want   core.ConversationStatus
	}{
		{"working", []string{visibleTurnEvent("t", "composer")}, core.ConversationWorking},
		{"finished", []string{visibleTurnEvent("t", "composer"), `{"type":"event_msg","payload":{"type":"task_complete","turn_id":"t"}}`}, core.ConversationFinished},
		{"stopped", []string{visibleTurnEvent("t", "composer"), `{"type":"event_msg","payload":{"type":"turn_aborted","turn_id":"t"}}`, `{"type":"event_msg","payload":{"type":"task_complete","turn_id":"t"}}`}, core.ConversationStopped},
		{"failed", []string{visibleTurnEvent("t", "composer"), `{"type":"event_msg","payload":{"type":"error","will_retry":false}}`}, core.ConversationFailed},
		{"transient error", []string{visibleTurnEvent("t", "composer"), `{"type":"event_msg","payload":{"type":"error","will_retry":true}}`}, core.ConversationWorking},
		{"late old completion", []string{visibleTurnEvent("t", "composer"), `{"type":"event_msg","payload":{"type":"task_complete","turn_id":"old"}}`}, core.ConversationWorking},
		{"async question", []string{visibleTurnEvent("t", "composer"), `{"type":"response_item","payload":{"type":"function_call","name":"request_user_input_async","call_id":"q"}}`, `{"type":"response_item","payload":{"type":"function_call_output","call_id":"q","output":"{\"accepted\":true}"}}`, `{"type":"event_msg","payload":{"type":"task_complete","turn_id":"t"}}`}, core.ConversationQuestion},
		{"answer", []string{visibleTurnEvent("t", "composer"), `{"type":"response_item","payload":{"type":"function_call","name":"request_user_input_async","call_id":"q"}}`, visibleMessageEvent("UserMessage", "t", "desktop", "", "answer")}, core.ConversationWorking},
		{"blocking answer", []string{visibleTurnEvent("t", "composer"), `{"type":"response_item","payload":{"type":"function_call","name":"functions.request_user_input","call_id":"q"}}`, `{"type":"response_item","payload":{"type":"function_call_output","call_id":"q","output":"answers"}}`}, core.ConversationWorking},
		{"bridge owned", []string{visibleTurnEvent("t", mirrorTurnOrigin), `{"type":"event_msg","payload":{"type":"task_complete","turn_id":"t"}}`}, ""},
		{"bridge steer", []string{visibleTurnEvent("t", "composer"), visibleMessageEvent("UserMessage", "t", "cc-connect:telegram:1", "", "answer")}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			writeVisibleMessages(t, home, "chat", "chat", tc.events...)
			a := &Agent{codexHome: home}
			got, err := a.ReadSessionStatus(context.Background(), "chat")
			if err != nil || got != tc.want {
				t.Fatalf("status=%q err=%v want=%q", got, err, tc.want)
			}
		})
	}
}

func TestReadSessionStatus_ValidatesIdentityPartialWritesAndCache(t *testing.T) {
	home := t.TempDir()
	path := writeVisibleMessages(t, home, "chat", "chat", visibleTurnEvent("t", "composer"))
	a := &Agent{codexHome: home}
	read := func(want core.ConversationStatus) {
		t.Helper()
		got, err := a.ReadSessionStatus(context.Background(), "chat")
		if err != nil || got != want {
			t.Fatalf("status=%s err=%v want=%s", got, err, want)
		}
	}
	read(core.ConversationWorking)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprint(f, `{"type":"event_msg","payload":{"type":"task_complete","turn_id":"t"}}`)
	f.Close()
	read(core.ConversationWorking)
	f, _ = os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	fmt.Fprintln(f)
	f.Close()
	read(core.ConversationFinished)
	writeVisibleMessages(t, home, "chat", "other", visibleTurnEvent("t", "composer"))
	read("")
	if err := os.WriteFile(path, []byte(`{"type":"session_meta","payload":{"id":"chat","source":{"subagent":{}}}}`+"\n"+visibleTurnEvent("t", "composer")+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	read("")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := a.ReadSessionStatus(ctx, "chat"); err == nil {
		t.Fatal("ignored cancellation")
	}
}
