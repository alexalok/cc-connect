package core

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

type deliveryAttempt struct {
	id  string
	ack chan error
}

type deliverySession struct {
	*receiptAgentSession
	attempts chan deliveryAttempt
}

func newDeliverySession() *deliverySession {
	return &deliverySession{
		receiptAgentSession: &receiptAgentSession{queuingAgentSession: newQueuingSession("delivered")},
		attempts:            make(chan deliveryAttempt, 8),
	}
}

func (s *deliverySession) SendWithReceipt(prompt, id string, images []ImageAttachment, files []FileAttachment, delivered func()) error {
	if err := s.Send(prompt, id, images, files); err != nil {
		return err
	}
	attempt := deliveryAttempt{id: id, ack: make(chan error, 1)}
	s.attempts <- attempt
	select {
	case err := <-attempt.ack:
		if err != nil {
			return err
		}
		delivered()
		delivered() // The engine also protects against duplicate acknowledgments.
		return nil
	case <-s.closed:
		return errors.New("session closed")
	}
}

type deliveryStartAgent struct {
	stubAgent
	session *deliverySession
	entered chan struct{}
	release chan struct{}
	err     error
}

func (a *deliveryStartAgent) StartSession(ctx context.Context, _ string) (AgentSession, error) {
	close(a.entered)
	select {
	case <-a.release:
		return a.session, a.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

type deliveryEnv struct {
	*cujEnv
	agent *deliveryStartAgent
}

func newDeliveryEnv(t *testing.T, startErr error) *deliveryEnv {
	p := &stubPlatformEngine{n: "test"}
	a := &deliveryStartAgent{session: newDeliverySession(), entered: make(chan struct{}), release: make(chan struct{}), err: startErr}
	e := NewEngine("delivery", a, []Platform{p}, t.TempDir()+"/sessions.json", LangEnglish)
	t.Cleanup(func() { _ = e.Stop(); _ = a.session.Close() })
	return &deliveryEnv{cujEnv: &cujEnv{t: t, engine: e, plat: p}, agent: a}
}

func (env *deliveryEnv) message(id, text string) *Message {
	return &Message{SessionKey: "test:delivery", Platform: "test", UserID: "user", MessageID: id, Content: text, ReplyCtx: id,
		OnDelivered: func() { _ = env.plat.Reply(context.Background(), id, "read:"+id) },
	}
}
func (env *deliveryEnv) send(id, text string) {
	env.engine.ReceiveMessage(env.plat, env.message(id, text))
}
func (env *deliveryEnv) startup() {
	env.t.Helper()
	select {
	case <-env.agent.entered:
	case <-time.After(time.Second):
		env.t.Fatal("startup not reached")
	}
}
func (env *deliveryEnv) attempt(id string) deliveryAttempt {
	env.t.Helper()
	select {
	case a := <-env.agent.session.attempts:
		if a.id != id {
			env.t.Fatalf("sent message %q, want %q", a.id, id)
		}
		return a
	case <-time.After(3 * time.Second):
		env.t.Fatalf("message %q was not sent", id)
		return deliveryAttempt{}
	}
}
func (env *deliveryEnv) visible(text string) {
	env.waitFor(text, 3*time.Second, func() bool {
		for _, m := range env.plat.getSent() {
			if strings.Contains(m, text) {
				return true
			}
		}
		return false
	})
}
func (env *deliveryEnv) receipts(ids ...string) {
	env.t.Helper()
	var got []string
	for _, m := range env.plat.getSent() {
		if strings.HasPrefix(m, "read:") {
			got = append(got, strings.TrimPrefix(m, "read:"))
		}
	}
	if fmt.Sprint(got) != fmt.Sprint(ids) {
		env.t.Fatalf("read indicators = %v, want %v", got, ids)
	}
}

// Extends CUJ A8 with the stricter, agent-confirmed receipt rather than the
// pre-existing bridge-acceptance receipt.
func testAgentConfirmedReceipts(t *testing.T) {
	env := newDeliveryEnv(t, nil)
	env.engine.maxQueuedMessages = 1
	env.send("first", "first task")
	env.startup()
	env.receipts()
	close(env.agent.release)
	first := env.attempt("first")
	env.send("queued", "next task")
	env.send("overflow", "one too many")
	env.send("command", "/status")
	env.receipts()
	first.ack <- nil
	env.visible("read:first")
	env.receipts("first")
	env.agent.session.events <- Event{Type: EventResult, Content: "first answer", Done: true}
	second := env.attempt("queued")
	env.receipts("first")
	second.ack <- nil
	env.visible("read:queued")
	env.agent.session.events <- Event{Type: EventResult, Content: "second answer", Done: true}
	env.visible("second answer")
	env.receipts("first", "queued")
}

func TestReadReceipt_NotSentOnStartupOrSendFailure(t *testing.T) {
	for _, startup := range []bool{false, true} {
		t.Run(fmt.Sprintf("startup=%v", startup), func(t *testing.T) {
			var startErr error
			if startup {
				startErr = errors.New("startup rejected")
			}
			env := newDeliveryEnv(t, startErr)
			env.send("failed", "task")
			env.startup()
			close(env.agent.release)
			if startup {
				env.visible("failed to start agent session")
			} else {
				env.send("queued", "next task")
				env.attempt("failed").ack <- errors.New("send rejected")
				env.visible("send rejected")
			}
			env.receipts()
		})
	}
}

func TestReadReceipt_UnconfirmedSendDoesNotImplyDelivery(t *testing.T) {
	s := &receiptAgentSession{queuingAgentSession: newQueuingSession("legacy")}
	defer s.Close()
	called := false
	if err := sendAgentMessage(s, "task", "1", nil, nil, func() { called = true }); err != nil {
		t.Fatal(err)
	}
	if called {
		t.Fatal("legacy pipe write produced a read indicator")
	}
}

func TestReadReceipt_VoiceWaitsForAgentAcknowledgment(t *testing.T) {
	for _, transcript := range []string{"voice task", ""} {
		t.Run("transcript="+transcript, func(t *testing.T) {
			env := newDeliveryEnv(t, nil)
			env.engine.SetSpeechConfig(SpeechCfg{Enabled: true, STT: &originalAudioSTT{transcript: transcript}})
			msg := env.message("voice", "")
			msg.Audio = &AudioAttachment{Data: []byte("ogg"), Format: "ogg"}
			env.engine.ReceiveMessage(env.plat, msg)
			env.receipts()
			if transcript == "" {
				env.visible(env.engine.i18n.T(MsgVoiceEmpty))
				return
			}
			env.startup()
			close(env.agent.release)
			attempt := env.attempt("voice")
			env.receipts()
			attempt.ack <- nil
			env.visible("read:voice")
			env.receipts("voice")
			env.agent.session.events <- Event{Type: EventResult, Content: "voice accepted", Done: true}
			env.visible("voice accepted")
		})
	}
}

func TestReadReceipt_AsyncAnswersWaitForCompleteDelivery(t *testing.T) {
	for _, mode := range []string{"active", "rejected", "completed"} {
		t.Run(mode, func(t *testing.T) {
			p := &stubPlatformEngine{n: "test"}
			s := &asyncInputSession{active: mode != "completed", events: make(chan Event, 8)}
			if mode == "rejected" {
				s.err = errors.New("steer failed")
			}
			e := NewEngine("test", &stubAgent{}, []Platform{p}, "", LangEnglish)
			state := &interactiveState{agentSession: s, platform: p}
			e.interactiveStates["test:answers"] = state
			e.showAsyncUserInput(state, p, "ctx", Event{RequestID: "questions", Questions: []UserQuestion{{Question: "One?"}, {Question: "Two?"}}})
			var receipts []string
			answer := func(id, text string) (*Message, bool) {
				msg := &Message{SessionKey: "test:answers", ReplyCtx: id, OnDelivered: func() { receipts = append(receipts, id) }}
				_, handled := e.handleAsyncUserInput(p, msg, text, "test:answers", e.sessions)
				return msg, handled
			}
			answer("first", "one")
			if len(receipts) != 0 {
				t.Fatal("partial answer marked before delivery")
			}
			msg, handled := answer("second", "two")
			if mode == "rejected" {
				if len(receipts) != 0 {
					t.Fatal("failed steering marked received")
				}
				return
			}
			if mode == "completed" {
				if handled || len(receipts) != 0 {
					t.Fatal("queued answer marked received")
				}
				msg.OnDelivered()
				msg.OnDelivered()
			}
			if fmt.Sprint(receipts) != "[first second]" {
				t.Fatalf("read indicators = %v", receipts)
			}
		})
	}
}

func TestReadReceipt_OrphanedQueueWaitsForAcknowledgment(t *testing.T) {
	env := newDeliveryEnv(t, nil)
	key := "test:delivery"
	session := env.engine.sessions.GetOrCreateActive(key)
	lockGen, locked := session.TryLock()
	if !locked {
		t.Fatal("session unexpectedly busy")
	}
	msg := env.message("orphan", "queued task")
	state := &interactiveState{agentSession: env.agent.session, platform: env.plat, pendingMessages: []queuedMessage{{
		messageID: msg.MessageID, platform: env.plat, replyCtx: msg.ReplyCtx, content: msg.Content, onDelivered: msg.OnDelivered,
	}}}
	done := make(chan bool, 1)
	go func() { done <- env.engine.drainPendingMessages(state, session, env.engine.sessions, key, lockGen) }()
	attempt := env.attempt("orphan")
	env.receipts()
	attempt.ack <- nil
	env.visible("read:orphan")
	env.agent.session.events <- Event{Type: EventResult, Content: "orphan answered", Done: true}
	select {
	case unlocked := <-done:
		if !unlocked {
			t.Fatal("queue did not finish")
		}
	case <-time.After(time.Second):
		t.Fatal("queue drain hung")
	}
	env.receipts("orphan")
}
