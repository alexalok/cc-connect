package core

import (
	"errors"
	"sync"
	"testing"
	"time"
)

type failingTurnCanceller struct {
	stubAgentSession
	onCancel  func()
	closed    chan struct{}
	closeOnce sync.Once
}

func (s *failingTurnCanceller) CancelTurn() error {
	if s.onCancel != nil {
		s.onCancel()
	}
	return errors.New("interrupt failed")
}
func (s *failingTurnCanceller) Close() error { s.closeOnce.Do(func() { close(s.closed) }); return nil }

func TestStopInteractiveSessionCancelErrorReacquiresLock(t *testing.T) {
	for _, replace := range []bool{false, true} {
		name := "same session"
		if replace {
			name = "replaced during cancel"
		}
		t.Run(name, func(t *testing.T) {
			e := NewEngine("test", &stubAgent{}, nil, t.TempDir()+"/sessions.json", LangEnglish)
			defer e.Stop()
			s := &failingTurnCanceller{closed: make(chan struct{})}
			busy := &Session{}
			busy.TryLock()
			old := &interactiveState{agentSession: s, busySession: busy}
			e.interactiveStates["chat"] = old
			replacement := &interactiveState{agentSession: &stubAgentSession{}, busySession: busy}
			if replace {
				s.onCancel = func() {
					e.interactiveMu.Lock()
					e.interactiveStates["chat"] = replacement
					e.interactiveMu.Unlock()
					s.Close()
				}
			}
			if !e.stopInteractiveSessionWithOptions("chat", false) {
				t.Fatal("stop was not handled")
			}
			e.interactiveMu.Lock()
			got := e.interactiveStates["chat"]
			e.interactiveMu.Unlock()
			if replace {
				if got != replacement || !busy.Busy() {
					t.Fatal("failed old cancel removed or unlocked replacement")
				}
			} else {
				if got != nil || busy.Busy() {
					t.Fatal("failed cancel did not clean up stopped session")
				}
			}
			select {
			case <-s.closed:
			case <-time.After(time.Second):
				t.Fatal("old session not closed")
			}
		})
	}
}

type replacingApprovalSession struct {
	stubAgentSession
	respond func()
}

func (s *replacingApprovalSession) RespondPermission(string, PermissionResult) error {
	s.respond()
	return nil
}

func TestPendingPermissionResponsePreservesNewerRequest(t *testing.T) {
	for _, question := range []bool{false, true} {
		name := "approval"
		if question {
			name = "question"
		}
		t.Run(name, func(t *testing.T) {
			p := &stubPlatformEngine{n: "test"}
			e := NewEngine("test", &stubAgent{}, []Platform{p}, t.TempDir()+"/sessions.json", LangEnglish)
			defer e.Stop()
			old := &pendingPermission{RequestID: "old", ToolName: "Bash", Resolved: make(chan struct{})}
			if question {
				old.Questions = []UserQuestion{{Question: "Choose"}}
			}
			next := &pendingPermission{RequestID: "new", ToolName: "Bash", Resolved: make(chan struct{})}
			state := &interactiveState{pending: old}
			state.agentSession = &replacingApprovalSession{respond: func() {
				// An agent-side resolution can let the event reader publish the next
				// approval while this handler is still confirming the old answer.
				state.mu.Lock()
				state.pending = next
				state.mu.Unlock()
			}}
			e.interactiveStates["test:user"] = state
			if !e.handlePendingPermission(p, &Message{SessionKey: "test:user", Content: "allow", ReplyCtx: "ctx"}, "allow", "test:user") {
				t.Fatal("answer not handled")
			}
			state.mu.Lock()
			pending := state.pending
			state.mu.Unlock()
			if pending != next {
				t.Fatal("old answer erased newer approval")
			}
			select {
			case <-old.Resolved:
			default:
				t.Fatal("old approval not resolved")
			}
			select {
			case <-next.Resolved:
				t.Fatal("newer approval resolved by old answer")
			default:
			}
		})
	}
}
