package codex

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/chenhg5/cc-connect/core"
)

func TestAppServerSession_SteerMessagePreservesTurnAndAttachments(t *testing.T) {
	s := &appServerSession{ctx: context.Background(), workDir: t.TempDir(), currentTurn: "active", pendingMsgs: []string{"buffered answer"}, pending: make(map[int64]chan rpcResponseEnvelope)}
	s.threadID.Store("parent")
	w := &asyncSteerWriter{s: s}
	s.stdin = w
	accepted, err := s.SteerMessage("use the attachment", "telegram-42", []core.ImageAttachment{{MimeType: "image/png", Data: []byte("image")}}, []core.FileAttachment{{FileName: "notes.txt", Data: []byte("notes")}})
	if err != nil || !accepted {
		t.Fatalf("steering failed: %v, %v", accepted, err)
	}
	if w.request.Method != "turn/steer" || w.request.Params["expectedTurnId"] != "active" || w.request.Params["threadId"] != "parent" {
		t.Fatalf("wrong RPC: %#v", w.request)
	}
	if !strings.HasPrefix(w.request.Params["clientUserMessageId"].(string), "cc-connect:telegram-42:") {
		t.Fatal("missing mirror origin")
	}
	for _, key := range []string{"model", "approvalPolicy", "effort"} {
		if _, ok := w.request.Params[key]; ok {
			t.Fatalf("steer must not override %s", key)
		}
	}
	input := w.request.Params["input"].([]any)
	if len(input) != 2 || !strings.Contains(input[0].(map[string]any)["text"].(string), "notes.txt") {
		t.Fatalf("attachments missing: %#v", input)
	}
	image := input[1].(map[string]any)
	data, err := os.ReadFile(image["path"].(string))
	if err != nil || string(data) != "image" || image["type"] != "localImage" {
		t.Fatalf("image not staged: %v, %q", err, data)
	}
	if s.currentTurn != "active" || len(s.pendingMsgs) != 1 || s.pendingMsgs[0] != "buffered answer" {
		t.Fatal("steer reset active turn output")
	}
}

type rejectedSteerWriter struct{ s *appServerSession }

func (w *rejectedSteerWriter) Write(p []byte) (int, error) {
	var req struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(p, &req); err != nil {
		return 0, err
	}
	w.s.handleResponse(rpcResponseEnvelope{ID: req.ID, Error: &rpcError{Code: -32602, Message: "active turn changed"}})
	return len(p), nil
}
func (*rejectedSteerWriter) Close() error { return nil }

func TestAppServerSession_SteerRejectDoesNotStartAnotherTurn(t *testing.T) {
	s := &appServerSession{ctx: context.Background(), currentTurn: "active", pending: make(map[int64]chan rpcResponseEnvelope)}
	s.threadID.Store("parent")
	s.stdin = &rejectedSteerWriter{s: s}
	accepted, err := s.SteerMessage("supplement", "id", nil, nil)
	if accepted || err == nil || !strings.Contains(err.Error(), "active turn changed") {
		t.Fatalf("rejection not surfaced: %v %v", accepted, err)
	}
	if s.nextID.Load() != 1 {
		t.Fatal("steer rejection must not retry through turn/start")
	}
	s.currentTurn = ""
	accepted, err = s.SteerMessage("after completion", "id", nil, nil)
	if accepted || err != nil || s.nextID.Load() != 1 {
		t.Fatalf("idle steering attempted RPC: %v %v", accepted, err)
	}
}
