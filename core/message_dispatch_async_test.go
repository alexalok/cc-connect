package core

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type orderedSteerAttempt struct {
	id  string
	ack chan bool
}

type orderedSteerSession struct {
	*dispatchSession
	attempts chan orderedSteerAttempt
	closed   chan struct{}
	once     sync.Once
}

func (s *orderedSteerSession) SteerMessage(prompt, id string, images []ImageAttachment, files []FileAttachment) (bool, error) {
	attempt := orderedSteerAttempt{id: id, ack: make(chan bool, 1)}
	s.attempts <- attempt
	select {
	case accepted := <-attempt.ack:
		if !accepted {
			return false, nil
		}
		return s.dispatchSession.SteerMessage(prompt, id, images, files)
	case <-s.closed:
		return false, errors.New("session stopped")
	}
}

func (s *orderedSteerSession) Close() error {
	s.once.Do(func() { close(s.closed) })
	return s.dispatchSession.Close()
}

func newOrderedDispatchEnv(t *testing.T) (*dispatchEnv, *orderedSteerSession) {
	env := newDispatchEnv(t, "steer")
	s := &orderedSteerSession{dispatchSession: env.session, attempts: make(chan orderedSteerAttempt, 8), closed: make(chan struct{})}
	env.engine.agent = &controllableAgent{nextSession: s}
	return env, s
}

func awaitSteerAttempt(t *testing.T, s *orderedSteerSession, id string) orderedSteerAttempt {
	t.Helper()
	select {
	case attempt := <-s.attempts:
		if attempt.id != id {
			t.Fatalf("steering order: got %q, want %q", attempt.id, id)
		}
		return attempt
	case <-time.After(3 * time.Second):
		t.Fatalf("no steering request for %s", id)
		return orderedSteerAttempt{}
	}
}

func responsiveDispatch(t *testing.T, send func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { defer close(done); send() }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("platform callback blocked by a pending steering confirmation")
	}
}

func TestBusyMessageMode_AsyncSteeringPreservesOrder(t *testing.T) {
	env, s := newOrderedDispatchEnv(t)
	env.send("first", "first task", 1000)
	env.awaitSends(1)
	responsiveDispatch(t, func() { env.send("steer1", "first adjustment", 2000) })
	first := awaitSteerAttempt(t, s, "steer1")
	responsiveDispatch(t, func() { env.send("steer2", "second adjustment", 3000) })
	responsiveDispatch(t, func() { env.send("queued", "/queue next task", 4000) })
	select {
	case attempt := <-s.attempts:
		t.Fatalf("%s overtook pending confirmation", attempt.id)
	default:
	}
	env.assertReceipts("first")
	first.ack <- true
	second := awaitSteerAttempt(t, s, "steer2")
	second.ack <- true
	env.awaitVisible(env.engine.i18n.T(MsgMessageQueued))
	env.assertReceipts("first", "steer1", "steer2", "queued")
	env.assertCalls(1, 2)
	env.finish("first done")
	env.awaitSends(2)
	env.finish("next done")
}

func TestBusyMessageMode_AsyncFallbackCannotBeOvertakenByQueue(t *testing.T) {
	env, s := newOrderedDispatchEnv(t)
	env.send("first", "first task", 1000)
	env.awaitSends(1)
	responsiveDispatch(t, func() { env.send("late", "late adjustment", 2000) })
	attempt := awaitSteerAttempt(t, s, "late")
	responsiveDispatch(t, func() { env.send("queued", "/queue next task", 3000) })
	env.finish("first done")
	env.waitFor("original turn idle", time.Second, func() bool { return !env.engine.sessions.GetOrCreateActive("test:receipt-user").Busy() })
	attempt.ack <- false
	env.awaitSends(2)
	env.awaitVisible("receipt:queued")
	env.finish("adjustment done")
	env.awaitSends(3)
	env.session.mu.Lock()
	sent := append([]string(nil), env.session.sent...)
	env.session.mu.Unlock()
	if len(sent) != 3 || sent[1] != "late adjustment" || sent[2] != "next task" {
		t.Fatalf("fallback reordered or dropped: %v", sent)
	}
	env.assertReceipts("first", "late", "queued")
	env.finish("next done")
}

func TestBusyMessageMode_AsyncSteeringAllowsOtherChatsAndStop(t *testing.T) {
	env, s := newOrderedDispatchEnv(t)
	other := &asyncInputSession{events: make(chan Event, 8)}
	starts := 0
	env.engine.agent = &controllableAgent{startSessionFn: func(context.Context, string) (AgentSession, error) {
		starts++
		if starts == 1 {
			return s, nil
		}
		return other, nil
	}}
	env.send("first", "first task", 1000)
	env.awaitSends(1)
	responsiveDispatch(t, func() { env.send("blocked", "first adjustment", 2000) })
	awaitSteerAttempt(t, s, "blocked")
	responsiveDispatch(t, func() { env.send("waiting", "second adjustment", 3000) })
	responsiveDispatch(t, func() {
		env.engine.ReceiveMessage(env.plat, &Message{SessionKey: "test:other", Platform: "test", MessageID: "other", Content: "other task", ReplyCtx: "other"})
	})
	env.waitFor("other chat reaches agent", time.Second, func() bool {
		other.mu.Lock()
		defer other.mu.Unlock()
		return len(other.sent) == 1
	})
	responsiveDispatch(t, func() { env.send("stop", "/stop", 4000) })
	env.awaitVisible(env.engine.i18n.T(MsgMessageSteerFailed))
	env.waitFor("stopped", time.Second, func() bool { return !s.Alive() })
	env.assertReceipts("first")
	select {
	case attempt := <-s.attempts:
		t.Fatalf("canceled message %s was sent", attempt.id)
	default:
	}
	other.events <- Event{Type: EventResult, Content: "other done", Done: true}
	env.awaitVisible("other done")
}

func TestBusyMessageMode_AsyncConfirmationSurvivesIdleTimeout(t *testing.T) {
	env, s := newOrderedDispatchEnv(t)
	env.engine.SetAgentSessionIdleTimeout(20 * time.Millisecond)
	env.send("first", "first task", 1000)
	env.awaitSends(1)
	responsiveDispatch(t, func() { env.send("late", "late adjustment", 2000) })
	attempt := awaitSteerAttempt(t, s, "late")
	env.finish("first done")
	time.Sleep(60 * time.Millisecond)
	if !s.Alive() {
		t.Fatal("idle timeout closed a session with a pending confirmation")
	}
	attempt.ack <- false
	env.awaitSends(2)
	env.finish("late done")
	env.waitFor("idle close after delivery", time.Second, func() bool { return !s.Alive() })
}
