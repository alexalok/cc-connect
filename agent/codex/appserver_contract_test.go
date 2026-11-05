package codex

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

// Opt-in protocol contract test for each deployed Codex version. Uses a fresh
// profile and a local fake inference endpoint; no account, credentials or live
// conversations are involved. The default suite uses the fault-injection peer.
func TestSharedServerRealCodexContract(t *testing.T) {
	bin := os.Getenv("CC_CONNECT_TEST_CODEX_BIN")
	if bin == "" {
		t.Skip("set CC_CONNECT_TEST_CODEX_BIN to test a deployed Codex build")
	}
	var blockNext atomic.Bool
	blocked := make(chan struct{}, 1)
	release := make(chan struct{})
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if blockNext.Swap(false) {
			blocked <- struct{}{}
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"test-response\"}}\n\n")
		fmt.Fprint(w, "event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"id\":\"msg-test\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"contract test answer\"}]}}\n\n")
		fmt.Fprint(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"test-response\",\"status\":\"completed\",\"usage\":{\"input_tokens\":1,\"output_tokens\":3,\"total_tokens\":4}}}\n\n")
	}))
	defer model.Close()
	defer close(release)
	dir, err := os.MkdirTemp("", "cc-codex-contract-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	profile := filepath.Join(dir, "profile")
	if err := os.Mkdir(profile, 0700); err != nil {
		t.Fatal(err)
	}
	config := fmt.Sprintf("model = 'gpt-5.1'\nmodel_provider = 'contract'\n[model_providers.contract]\nname = 'Contract'\nbase_url = '%s/v1'\nwire_api = 'responses'\nrequires_openai_auth = false\n", model.URL)
	if err := os.WriteFile(filepath.Join(profile, "config.toml"), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(dir, "server.sock")
	logfile, err := os.Create(filepath.Join(dir, "server.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer logfile.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "app-server", "--listen", "unix://"+socket)
	cmd.Env = []string{"HOME=" + dir, "CODEX_HOME=" + profile, "PATH=" + os.Getenv("PATH")}
	cmd.Dir = dir
	cmd.Stdout, cmd.Stderr = logfile, logfile
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		cmd.Process.Kill()
		cmd.Wait()
		if t.Failed() {
			data, _ := os.ReadFile(logfile.Name())
			t.Logf("isolated Codex: %s", data)
		}
	}()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(socket); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Codex did not open control socket")
		}
		time.Sleep(20 * time.Millisecond)
	}
	create := func(id string) *appServerSession {
		s, err := newAppServerSession(ctx, "", nil, "unix://"+socket, dir, "", "", "suggest", id, "", "", nil, profile, "", "")
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	desktop := create("")
	defer desktop.Close()
	waitAnswer := func(s *appServerSession) {
		t.Helper()
		text := ""
		for {
			select {
			case ev := <-s.events:
				if ev.Type == core.EventError {
					t.Fatal(ev.Error)
				}
				if ev.Type == core.EventText {
					text += ev.Content
				}
				if ev.Type == core.EventResult && ev.Done {
					if text != "contract test answer" {
						t.Fatalf("unexpected answer: %q", text)
					}
					return
				}
			case <-ctx.Done():
				t.Fatal("Codex did not complete test turn")
			}
		}
	}
	// Codex creates the durable rollout on the first turn, not thread/start.
	if err := desktop.Send("seed conversation", "seed", nil, nil); err != nil {
		t.Fatal(err)
	}
	waitAnswer(desktop)
	bridge := create(desktop.CurrentSessionID())
	defer bridge.Close()
	if bridge.GetWorkDir() != dir || bridge.GetModel() != desktop.GetModel() {
		t.Fatal("attachment changed thread settings")
	}
	if err := desktop.Send("local test", "desktop-1", nil, nil); err != nil {
		t.Fatal(err)
	}
	waitAnswer(desktop)
	if len(bridge.events) != 0 {
		t.Fatal("desktop output also streamed through the bridge")
	}
	if err := bridge.Send("local test", "telegram-1", nil, nil); err != nil {
		t.Fatal(err)
	}
	waitAnswer(bridge)
	if len(desktop.events) != 0 {
		t.Fatal("bridge output also streamed through the other client")
	}
	bridge.Close()
	reattached := create(desktop.CurrentSessionID())
	defer reattached.Close()
	if err := reattached.Send("after reconnect", "telegram-2", nil, nil); err != nil {
		t.Fatal(err)
	}
	waitAnswer(reattached)
	// Resume during inference, submit from a second client, and interrupt the
	// accepted turn. This verifies actual multi-client behavior, not a fake's
	// assumptions about turn/start on an already active thread.
	blockNext.Store(true)
	if err := desktop.Send("slow local test", "desktop-active", nil, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case <-blocked:
	case <-ctx.Done():
		t.Fatal("model request did not start")
	}
	desktop.stateMu.Lock()
	activeTurn := desktop.currentTurn
	desktop.stateMu.Unlock()
	activeBridge := create(desktop.CurrentSessionID())
	defer activeBridge.Close()
	if err := activeBridge.Send("adjust running task", "telegram-active", nil, nil); err != nil {
		t.Fatal(err)
	}
	activeBridge.stateMu.Lock()
	acceptedTurn := activeBridge.currentTurn
	activeBridge.stateMu.Unlock()
	if activeTurn != acceptedTurn {
		t.Fatalf("active submission changed turn: %s -> %s", activeTurn, acceptedTurn)
	}
	if err := (&sharedAppServerSession{activeBridge}).CancelTurn(); err != nil {
		t.Fatal(err)
	}
	for {
		select {
		case ev := <-activeBridge.events:
			if ev.Type == core.EventError {
				t.Fatal(ev.Error)
			}
			if ev.Type == core.EventResult && ev.Done {
				return
			}
		case <-ctx.Done():
			t.Fatal("targeted interrupt did not complete active turn")
		}
	}

}
