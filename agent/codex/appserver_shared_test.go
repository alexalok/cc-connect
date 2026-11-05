package codex

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
	"github.com/gorilla/websocket"
)

type sharedTestRequest struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params map[string]any  `json:"params"`
}

// A real Unix/WebSocket boundary, with independently attached clients and no
// Codex binary or model provider. The callback owns writes for each request.
func sharedTestServer(t *testing.T, handle func(*websocket.Conn, sharedTestRequest) bool) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "cc-shared-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	path := filepath.Join(dir, "server.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			var req sharedTestRequest
			if conn.ReadJSON(&req) != nil {
				return
			}
			if handle != nil && handle(conn, req) {
				continue
			}
			if len(req.ID) == 0 {
				continue
			}
			result := map[string]any{}
			if req.Method == "thread/start" || req.Method == "thread/resume" {
				result = map[string]any{"thread": map[string]any{"id": "shared-thread"}, "cwd": dir, "model": "desktop-model", "reasoningEffort": "high"}
			}
			if req.Method == "turn/start" {
				result = map[string]any{"turn": map[string]any{"id": "bridge-turn"}}
			}
			if conn.WriteJSON(map[string]any{"id": req.ID, "result": result}) != nil {
				return
			}
		}
	})}
	go server.Serve(listener)
	t.Cleanup(func() { server.Close() })
	return "unix://" + path
}

func sharedTestSession(t *testing.T, url, resumeID string) *appServerSession {
	t.Helper()
	s, err := newAppServerSession(context.Background(), "/no/codex/binary", nil, url, t.TempDir(), "", "", "inherit", resumeID, "", "", nil, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestSharedServerResumePreservesSettingsAndEarlyOutput(t *testing.T) {
	var starts atomic.Int32
	url := sharedTestServer(t, func(c *websocket.Conn, req sharedTestRequest) bool {
		switch req.Method {
		case "thread/resume":
			if len(req.Params) != 1 || req.Params["threadId"] != "shared-thread" {
				t.Errorf("resume overrides existing thread: %v", req.Params)
			}
		case "turn/start":
			starts.Add(1)
			for _, k := range []string{"model", "effort", "approvalPolicy", "sandboxPolicy"} {
				if _, ok := req.Params[k]; ok {
					t.Errorf("cached %s sent as override", k)
				}
			}
			if req.Params["clientUserMessageId"] != "cc-connect:42:shared-thread" {
				t.Errorf("unstable attribution: %v", req.Params)
			}
			for _, raw := range []string{
				`{"method":"turn/started","params":{"threadId":"shared-thread","turn":{"id":"desktop-turn"}}}`,
				`{"method":"item/completed","params":{"threadId":"shared-thread","turnId":"desktop-turn","item":{"type":"agentMessage","text":"external output"}}}`,
				`{"method":"turn/started","params":{"threadId":"shared-thread","turn":{"id":"bridge-turn"}}}`,
				`{"method":"thread/status/changed","params":{"threadId":"shared-thread","status":{"type":"idle"}}}`,
				`{"method":"item/completed","params":{"threadId":"shared-thread","turnId":"bridge-turn","item":{"type":"agentMessage","text":"final answer"}}}`,
				`{"method":"turn/completed","params":{"threadId":"other-thread","turn":{"id":"bridge-turn","status":"completed"}}}`,
				`{"method":"turn/completed","params":{"threadId":"shared-thread","turn":{"id":"bridge-turn","status":"completed"}}}`,
			} {
				if c.WriteMessage(websocket.TextMessage, []byte(raw)) != nil {
					return true
				}
			}
		}
		return false
	})
	s := sharedTestSession(t, url, "shared-thread")
	for i := 0; i < 2; i++ {
		if err := s.Send("continue", "42", nil, nil); err != nil {
			t.Fatal(err)
		}
		select {
		case ev := <-s.events:
			if ev.Type != core.EventText || ev.Content != "final answer" {
				t.Fatalf("wrong output: %+v", ev)
			}
		case <-time.After(time.Second):
			t.Fatal("lost early output")
		}
		select {
		case ev := <-s.events:
			if ev.Type != core.EventResult || !ev.Done {
				t.Fatalf("wrong terminal event: %+v", ev)
			}
		case <-time.After(time.Second):
			t.Fatal("lost completion")
		}
		s.stateMu.Lock()
		turn := s.currentTurn
		s.stateMu.Unlock()
		if turn != "" {
			t.Fatal("late acknowledgement resurrected completed turn")
		}
	}
	if starts.Load() != 2 || len(s.events) != 0 {
		t.Fatal("duplicate output or replay")
	}
}

func TestSharedServerStartSetsWorkingDirectoryAndInterruptTargetsTurn(t *testing.T) {
	interrupts := make(chan map[string]any, 1)
	starts := make(chan map[string]any, 1)
	url := sharedTestServer(t, func(c *websocket.Conn, req sharedTestRequest) bool {
		if req.Method == "thread/start" {
			starts <- req.Params
		}
		if req.Method == "turn/interrupt" {
			interrupts <- req.Params
		}
		return false
	})
	s := sharedTestSession(t, url, "")
	params := <-starts
	if cwd, ok := params["cwd"].(string); !ok || !filepath.IsAbs(cwd) {
		t.Fatalf("missing cwd: %v", params)
	}
	if _, ok := params["approvalPolicy"]; ok {
		t.Fatalf("native approval policy overwritten: %v", params)
	}
	if _, ok := params["sandbox"]; ok {
		t.Fatalf("native sandbox overwritten: %v", params)
	}
	if err := s.Send("work", "id", nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := (&sharedAppServerSession{s}).CancelTurn(); err != nil {
		t.Fatal(err)
	}
	got := <-interrupts
	if got["threadId"] != "shared-thread" || got["turnId"] != "bridge-turn" {
		t.Fatalf("wrong interrupt: %v", got)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	// Closing one subscriber cannot kill the server or prevent another attach.
	sharedTestSession(t, url, "shared-thread")
}

func TestSharedServerLostAcknowledgementDoesNotReplay(t *testing.T) {
	var starts atomic.Int32
	url := sharedTestServer(t, func(c *websocket.Conn, req sharedTestRequest) bool {
		if req.Method == "turn/start" {
			starts.Add(1)
			c.Close()
			return true
		}
		return false
	})
	s := sharedTestSession(t, url, "shared-thread")
	receipt := false
	err := s.SendWithReceipt("work", "id", nil, nil, func() { receipt = true })
	if err == nil || !strings.Contains(err.Error(), "may have been accepted") || receipt || s.Alive() {
		t.Fatalf("ambiguous send not surfaced: %v, receipt=%v alive=%v", err, receipt, s.Alive())
	}
	sharedTestSession(t, url, "shared-thread")
	if starts.Load() != 1 {
		t.Fatal("reconnected by replaying input")
	}
}

func TestSharedServerIgnoresExternalApprovalsAndResolvesOtherClientAnswer(t *testing.T) {
	s := sharedTestSession(t, sharedTestServer(t, nil), "shared-thread")
	// Merely subscribing must not deny the desktop's approvals or tool calls.
	for _, method := range []string{"item/commandExecution/requestApproval", "item/tool/call"} {
		s.handleServerRequest(serverRequestProbe(t, `99`, method, map[string]any{"threadId": "shared-thread", "turnId": "desktop-turn", "command": "test"}))
	}
	if len(s.events) != 0 {
		t.Fatal("external requests reached the interactive engine")
	}
	if err := s.Send("work", "id", nil, nil); err != nil {
		t.Fatal(err)
	}
	s.handleServerRequest(serverRequestProbe(t, `"approval"`, "item/commandExecution/requestApproval", map[string]any{"threadId": "shared-thread", "turnId": "bridge-turn", "command": "test"}))
	event := <-s.events
	if event.Type != core.EventPermissionRequest || event.PermissionResolved == nil {
		t.Fatalf("missing approval lifecycle: %+v", event)
	}
	s.handleNotification("serverRequest/resolved", json.RawMessage(`{"threadId":"shared-thread","requestId":"approval"}`))
	select {
	case <-event.PermissionResolved:
	case <-time.After(time.Second):
		t.Fatal("engine would wait forever on an answered approval")
	}
	if err := s.RespondPermission(event.RequestID, core.PermissionResult{Behavior: "allow"}); err == nil {
		t.Fatal("stale approval accepted")
	}
}

func TestSharedServerConfigRejectsUnsupportedTransportAndChildOptions(t *testing.T) {
	for _, opts := range []map[string]any{
		{"app_server_url": "ws://localhost:1234"},
		{"app_server_url": "unix://relative"},
		{"app_server_url": "unix:///tmp/test.sock", "env": map[string]any{"TEST": "value"}},
		{"app_server_url": "unix:///tmp/test.sock", "cmd": []any{"/no/codex/binary", "--profile", "x"}},
	} {
		opts["backend"] = "app_server"
		opts["mode"] = "inherit"
		if _, ok := opts["cmd"]; !ok {
			opts["cmd"] = "/no/codex/binary"
		}
		if _, err := New(opts); err == nil {
			t.Fatalf("unsupported configuration accepted: %v", opts)
		}
	}
	a, err := New(map[string]any{"backend": "app_server", "mode": "inherit", "app_server_url": "unix:///tmp/test.sock", "cmd": "/no/codex/binary"})
	if err != nil {
		t.Fatalf("shared mode required local CLI: %v", err)
	}
	a.(*Agent).SetProviders([]core.ProviderConfig{{Name: "other", APIKey: "not-a-real-key"}})
	a.(*Agent).SetActiveProvider("other")
	if _, err := a.StartSession(context.Background(), ""); err == nil || !strings.Contains(err.Error(), "owns its provider") {
		t.Fatalf("provider override was not rejected: %v", err)
	}
}

func TestSharedServerArchiveUsesManagementConnection(t *testing.T) {
	archived := make(chan map[string]any, 1)
	url := sharedTestServer(t, func(c *websocket.Conn, req sharedTestRequest) bool {
		if req.Method == "thread/start" || req.Method == "thread/resume" || req.Method == "turn/start" {
			t.Errorf("archive submitted %s", req.Method)
		}
		if req.Method == "thread/archive" {
			archived <- req.Params
		}
		return false
	})
	a, err := New(map[string]any{"backend": "app_server", "mode": "inherit", "app_server_url": url, "cmd": "/no/codex/binary"})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.(*Agent).ArchiveSession(context.Background(), "shared-thread"); err != nil {
		t.Fatal(err)
	}
	if (<-archived)["threadId"] != "shared-thread" {
		t.Fatal("wrong thread archived")
	}
}

func TestSharedServerRequiresExplicitNativePolicy(t *testing.T) {
	for _, mode := range []string{"", "suggest", "yolo", "full-auto"} {
		if _, err := New(map[string]any{"backend": "app_server", "app_server_url": "unix:///tmp/test.sock", "mode": mode}); err == nil {
			t.Fatalf("mode %q silently inherited native privileges", mode)
		}
	}
	a, err := New(map[string]any{"backend": "app_server", "app_server_url": "unix:///tmp/test.sock", "mode": "inherit"})
	if err != nil {
		t.Fatal(err)
	}
	agent := a.(*Agent)
	agent.SetMode("suggest")
	if agent.GetMode() != "inherit" {
		t.Fatal("mode command claimed an unenforced restriction")
	}
	modes := agent.PermissionModes()
	if len(modes) != 1 || modes[0].Key != "inherit" {
		t.Fatal("shared mode menu advertises unenforced policies")
	}
}

func TestSharedServerSkillCatalogWithoutLocalCLI(t *testing.T) {
	dir := t.TempDir()
	skillPath := filepath.Join(dir, "SKILL.md")
	if err := os.WriteFile(skillPath, []byte("---\nname: example\ndescription: Test skill\n---\nTest instructions\n"), 0600); err != nil {
		t.Fatal(err)
	}
	url := sharedTestServer(t, func(c *websocket.Conn, req sharedTestRequest) bool {
		switch req.Method {
		case "thread/start", "thread/resume", "turn/start":
			t.Errorf("skill lookup called %s", req.Method)
		case "skills/list":
			cwds, _ := req.Params["cwds"].([]any)
			if len(cwds) != 1 || cwds[0] != dir || req.Params["forceReload"] != true {
				t.Errorf("wrong catalog scope: %v", req.Params)
			}
			c.WriteJSON(map[string]any{"id": req.ID, "result": map[string]any{"data": []any{map[string]any{"cwd": dir, "skills": []any{map[string]any{"name": "plugin:example", "description": "Native skill", "path": skillPath, "enabled": true}}}}}})
			return true
		}
		return false
	})
	a, err := New(map[string]any{"backend": "app_server", "app_server_url": url, "mode": "inherit", "cmd": "/no/codex/binary", "work_dir": dir})
	if err != nil {
		t.Fatal(err)
	}
	skills, err := a.(*Agent).ListSkills(context.Background())
	if err != nil || len(skills) != 1 || skills[0].Name != "plugin:example" || !strings.Contains(skills[0].Prompt, "Test instructions") {
		t.Fatalf("native catalog unavailable: %v, %v", skills, err)
	}
}
