package core

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

type statusPlatform struct {
	stubPlatformEngine
	statuses []ConversationStatus
}

func (p *statusPlatform) SetConversationStatus(_ context.Context, _ any, status ConversationStatus) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.statuses = append(p.statuses, status)
}
func (p *statusPlatform) lastStatus() ConversationStatus {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.statuses) == 0 {
		return ""
	}
	return p.statuses[len(p.statuses)-1]
}
func waitConversationStatus(t *testing.T, p *statusPlatform, want ConversationStatus) {
	t.Helper()
	waitAsyncInput(t, func() bool { return p.lastStatus() == want })
}

type statusSession struct {
	asyncInputSession
	permissionMu sync.Mutex
	permissions  int
}

func (s *statusSession) RespondPermission(string, PermissionResult) error {
	s.permissionMu.Lock()
	defer s.permissionMu.Unlock()
	s.permissions++
	return nil
}
func newStatusEngine(t *testing.T) (*Engine, *statusPlatform, *statusSession, func(string)) {
	t.Helper()
	p := &statusPlatform{stubPlatformEngine: stubPlatformEngine{n: "status"}}
	s := &statusSession{asyncInputSession: asyncInputSession{events: make(chan Event, 16)}}
	e := NewEngine("test", &controllableAgent{nextSession: s}, []Platform{p}, "", LangEnglish)
	t.Cleanup(func() { _ = e.Stop() })
	send := func(content string) {
		e.ReceiveMessage(p, &Message{SessionKey: "status:user", Platform: "status", UserID: "user", Content: content, ReplyCtx: "topic"})
	}
	return e, p, s, send
}

func TestConversationStatus_ApprovalAndQuestionAnswerResumeWork(t *testing.T) {
	for _, tc := range []struct {
		name   string
		event  Event
		answer string
		want   ConversationStatus
	}{
		{"approval", Event{Type: EventPermissionRequest, RequestID: "p", ToolName: "Bash"}, "allow", ConversationApproval},
		{"question", Event{Type: EventPermissionRequest, RequestID: "q", ToolName: "AskUserQuestion", Questions: testQuestions()}, "2", ConversationQuestion},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, p, s, send := newStatusEngine(t)
			send("start")
			waitConversationStatus(t, p, ConversationWorking)
			s.events <- tc.event
			waitConversationStatus(t, p, tc.want)
			send(tc.answer)
			waitConversationStatus(t, p, ConversationWorking)
			s.events <- Event{Type: EventResult, Content: "done", Done: true}
			waitConversationStatus(t, p, ConversationFinished)
		})
	}
}

func TestConversationStatus_AsyncAnswerWhileWorking(t *testing.T) {
	_, p, s, send := newStatusEngine(t)
	send("start")
	waitConversationStatus(t, p, ConversationWorking)
	s.events <- Event{Type: EventUserInputRequest, RequestID: "q", Questions: testQuestions()}
	waitConversationStatus(t, p, ConversationQuestion)
	s.events <- Event{Type: EventText, Content: "Still working"}
	s.events <- Event{Type: EventResult, Done: false}
	send("2")
	waitConversationStatus(t, p, ConversationWorking)
	s.events <- Event{Type: EventError, Error: errors.New("failed")}
	waitConversationStatus(t, p, ConversationFailed)
}

func TestConversationStatus_AbnormalExitIsNotFinished(t *testing.T) {
	for _, mode := range []string{"error", "closed", "timeout"} {
		t.Run(mode, func(t *testing.T) {
			e, p, s, send := newStatusEngine(t)
			if mode == "timeout" {
				e.eventIdleTimeout = 30 * time.Millisecond
			}
			send("start")
			waitConversationStatus(t, p, ConversationWorking)
			switch mode {
			case "error":
				s.events <- Event{Type: EventError, Error: errors.New("failed")}
			case "closed":
				close(s.events)
			}
			waitConversationStatus(t, p, ConversationFailed)
		})
	}
}

func TestConversationStatus_StopWinsOverLateResult(t *testing.T) {
	e, p, _, _ := newStatusEngine(t)
	state := &interactiveState{platform: p, replyCtx: "topic"}
	e.updateConversationStatus(state, ConversationWorking)
	e.updateConversationStatus(state, ConversationStopped)
	e.updateConversationStatus(state, ConversationFinished)
	if p.lastStatus() != ConversationStopped {
		t.Fatal("late completion overwrote stop")
	}
	e.updateConversationStatus(state, ConversationWorking)
	if p.lastStatus() != ConversationWorking {
		t.Fatal("new turn did not clear stop")
	}
}

// Block only the old final reply so a replacement turn can start before the
// previous reader reaches its deferred status update.
type blockingStatusReplyPlatform struct {
	statusPlatform
	blocked chan struct{}
	release chan struct{}
}

func (p *blockingStatusReplyPlatform) Send(ctx context.Context, reply any, content string) error {
	if strings.Contains(content, "old final reply") {
		close(p.blocked)
		select {
		case <-p.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return p.statusPlatform.Send(ctx, reply, content)
}

func TestConversationStatus_StoppedReaderCannotOverwriteReplacement(t *testing.T) {
	p := &blockingStatusReplyPlatform{blocked: make(chan struct{}), release: make(chan struct{})}
	p.n = "status"
	oldAgent := &statusSession{asyncInputSession: asyncInputSession{events: make(chan Event, 16)}}
	newAgent := &statusSession{asyncInputSession: asyncInputSession{events: make(chan Event, 16)}}
	e := NewEngine("test", &controllableAgent{nextSession: newAgent}, []Platform{p}, "", LangEnglish)
	release := sync.OnceFunc(func() { close(p.release) })
	t.Cleanup(func() { release(); _ = e.Stop() })
	key := "status:user"
	session := e.sessions.GetOrCreateActive(key)
	gen, locked := session.TryLock()
	if !locked {
		t.Fatal("initial turn did not acquire the busy lock")
	}
	old := &interactiveState{agentSession: oldAgent, platform: p, replyCtx: "topic", busySession: session}
	e.interactiveStates[key] = old
	oldDone := make(chan struct{})
	go func() {
		defer close(oldDone)
		e.processInteractiveEvents(old, session, e.sessions, key, "old", time.Now(), nil, nil, "topic", gen)
	}()
	oldAgent.events <- Event{Type: EventResult, Content: "old final reply", Done: true}
	select {
	case <-p.blocked:
	case <-time.After(3 * time.Second):
		t.Fatal("old reply did not block")
	}
	send := func(content string) {
		e.ReceiveMessage(p, &Message{SessionKey: key, Platform: "status", UserID: "user", Content: content, ReplyCtx: "topic"})
	}
	send("/stop")
	waitConversationStatus(t, &p.statusPlatform, ConversationStopped)
	send("start replacement")
	waitAsyncInput(t, func() bool {
		newAgent.mu.Lock()
		defer newAgent.mu.Unlock()
		return len(newAgent.sent) == 1
	})
	waitConversationStatus(t, &p.statusPlatform, ConversationWorking)
	release()
	select {
	case <-oldDone:
	case <-time.After(3 * time.Second):
		t.Fatal("old reader did not finish")
	}
	if got := p.lastStatus(); got != ConversationWorking {
		t.Fatalf("stale terminal update replaced the new working icon: %s", got)
	}
	newAgent.events <- Event{Type: EventResult, Content: "replacement finished", Done: true}
	waitConversationStatus(t, &p.statusPlatform, ConversationFinished)
}

func TestConversationStatus_RejectsSupersededStateAndGeneration(t *testing.T) {
	for _, mode := range []string{"retained state", "replacement session"} {
		t.Run(mode, func(t *testing.T) {
			e, p, _, _ := newStatusEngine(t)
			key := "status:user"
			session := e.sessions.GetOrCreateActive(key)
			gen, _ := session.TryLock()
			old := &interactiveState{platform: p, replyCtx: "topic"}
			e.interactiveStates[key] = old
			e.updateConversationStatusForTurn(old, session, key, gen, ConversationStopped)
			current, currentSession := old, session
			if mode == "retained state" {
				session.ForceUnlock()
			} else {
				current = &interactiveState{platform: p, replyCtx: "topic"}
				currentSession = e.sessions.NewSession(key, "")
				e.interactiveStates[key] = current
			}
			currentGen, _ := currentSession.TryLock()
			e.updateConversationStatusForTurn(current, currentSession, key, currentGen, ConversationWorking)
			for _, stale := range []ConversationStatus{ConversationStopped, ConversationFailed, ConversationFinished, ConversationWorking} {
				e.updateConversationStatusForTurn(old, session, key, gen, stale)
				if got := p.lastStatus(); got != ConversationWorking {
					t.Fatalf("stale %s overwrote replacement working state: %s", stale, got)
				}
			}
			// Rejected updates must not mutate the retained state's stored base.
			e.updateConversationStatus(current, "")
			if p.lastStatus() != ConversationWorking {
				t.Fatal("stale generation mutated the active status")
			}
			e.updateConversationStatusForTurn(current, currentSession, key, currentGen, ConversationFinished)
			if p.lastStatus() != ConversationFinished {
				t.Fatal("current turn could not finish")
			}
		})
	}
}
