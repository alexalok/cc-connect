package codex

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestUnixSocketResumeUsesExistingServer(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	resumed := make(chan bool, 1)
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			var req struct {
				ID     int64          `json:"id"`
				Method string         `json:"method"`
				Params map[string]any `json:"params"`
			}
			if conn.ReadJSON(&req) != nil {
				return
			}
			if req.ID == 0 {
				continue
			}
			result := map[string]any{}
			if req.Method == "thread/resume" {
				resumed <- req.Params["threadId"] == "existing"
				result["thread"] = map[string]any{"id": "existing"}
			}
			if conn.WriteJSON(map[string]any{"id": req.ID, "result": result}) != nil {
				return
			}
		}
	})}
	go server.Serve(listener)
	defer server.Close()
	s, err := newAppServerSession(context.Background(), "/nonexistent-codex", nil, "unix://"+path, t.TempDir(), "", "", "suggest", "existing", "", "", nil, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	select {
	case ok := <-resumed:
		if !ok {
			t.Fatal("wrong resume ID")
		}
	case <-time.After(time.Second):
		t.Fatal("no resume")
	}
	if s.CurrentSessionID() != "existing" {
		t.Fatal("lost identity")
	}
	s.handleNotification("turn/started", json.RawMessage(`{"threadId":"other","turn":{"id":"wrong"}}`))
	if s.currentTurn != "" {
		t.Fatal("cross-thread event accepted")
	}
	s.handleNotification("turn/started", json.RawMessage(`{"threadId":"existing","turn":{"id":"right"}}`))
	if s.currentTurn != "right" {
		t.Fatal("own event lost")
	}
	s.handleServerRequest(map[string]json.RawMessage{
		"id": json.RawMessage(`99`), "method": json.RawMessage(`"item/commandExecution/requestApproval"`),
		"params": json.RawMessage(`{"threadId":"other","command":"do not execute"}`),
	})
	if len(s.pendingApprovals) != 0 {
		t.Fatal("cross-thread approval accepted")
	}
	s.cancel()
	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cancel did not release connection")
	}
	s.Close()
	c, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal("closing session stopped shared server", err)
	}
	c.Close()
}
