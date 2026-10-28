package core

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

type typingDelivery struct {
	content string
	typing  bool
}

type typingPlatform struct {
	stubPlatformEngine
	active     map[any]bool
	stops      int
	deliveries []typingDelivery
}

func (p *typingPlatform) StartTyping(_ context.Context, replyCtx any) func() {
	p.mu.Lock()
	if p.active == nil {
		p.active = make(map[any]bool)
	}
	p.active[replyCtx] = true
	p.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			p.mu.Lock()
			p.active[replyCtx] = false
			p.stops++
			p.mu.Unlock()
		})
	}
}

func (p *typingPlatform) Send(_ context.Context, replyCtx any, content string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sent = append(p.sent, content)
	p.deliveries = append(p.deliveries, typingDelivery{content: content, typing: p.active[replyCtx]})
	return nil
}

func (p *typingPlatform) Reply(ctx context.Context, replyCtx any, content string) error {
	return p.Send(ctx, replyCtx, content)
}

func (p *typingPlatform) isTyping(replyCtx any) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.active[replyCtx]
}

func (p *typingPlatform) assertDeliveredWhileTyping(t *testing.T, content string, want bool) {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, d := range p.deliveries {
		if strings.Contains(d.content, content) {
			if d.typing != want {
				t.Fatalf("typing while delivering %q = %v, want %v", content, d.typing, want)
			}
			return
		}
	}
	t.Fatalf("no delivery containing %q", content)
}

func TestProcessInteractiveEvents_StopsTypingOnEveryExit(t *testing.T) {
	for _, exit := range []string{"result", "agent error", "send error", "stop", "cancel", "closed", "idle timeout"} {
		t.Run(exit, func(t *testing.T) {
			p := &typingPlatform{stubPlatformEngine: stubPlatformEngine{n: "test"}}
			s := &receiptAgentSession{queuingAgentSession: newQueuingSession("typing-session")}
			e := NewEngine("typing", &stubAgent{}, []Platform{p}, "", LangEnglish)
			defer e.cancel()
			key := "test:typing"
			session := e.sessions.GetOrCreateActive(key)
			state := &interactiveState{agentSession: s, platform: p, replyCtx: "ctx"}
			var sendDone chan error
			var final string
			switch exit {
			case "result":
				final = "Final answer"
				s.events <- Event{Type: EventResult, Content: final, Done: true}
			case "agent error":
				final = "generation failed"
				s.events <- Event{Type: EventError, Error: errors.New(final)}
			case "send error":
				final = "send failed"
				sendDone = make(chan error, 1)
				sendDone <- errors.New(final)
			case "stop":
				state.markStopped()
			case "cancel":
				e.cancel()
			case "closed":
				final = "Partial answer"
				s.events <- Event{Type: EventText, Content: final}
				_ = s.Close()
			case "idle timeout":
				e.eventIdleTimeout = time.Millisecond
			}
			e.processInteractiveEvents(state, session, e.sessions, key, "m1", time.Now(), p.StartTyping(context.Background(), "ctx"), sendDone, "ctx", 0)
			if p.isTyping("ctx") || p.stops != 1 {
				t.Fatalf("typing stopped %d times, want exactly once", p.stops)
			}
			if final != "" {
				p.assertDeliveredWhileTyping(t, final, false)
			}
		})
	}
}

func TestProcessInteractiveMessage_StopsTypingBeforeStartupError(t *testing.T) {
	p := &typingPlatform{stubPlatformEngine: stubPlatformEngine{n: "test"}}
	a := &receiptStartAgent{entered: make(chan struct{}), release: make(chan struct{}), err: errors.New("startup failed")}
	close(a.release)
	e := NewEngine("typing", a, []Platform{p}, "", LangEnglish)
	t.Cleanup(func() { _ = e.Stop() })
	e.ReceiveMessage(p, &Message{SessionKey: "test:startup", Platform: "test", Content: "Hello", ReplyCtx: "ctx"})
	env := &cujEnv{t: t, engine: e, plat: &p.stubPlatformEngine}
	env.waitFor("startup failure", time.Second, func() bool { return len(p.getSent()) > 0 })
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, d := range p.deliveries {
		if d.typing {
			t.Fatalf("typing was still active while delivering startup error: %s", d.content)
		}
	}
}
