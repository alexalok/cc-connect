package core

import (
	"context"
	"errors"
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
