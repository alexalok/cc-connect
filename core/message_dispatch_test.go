package core

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

type dispatchSession struct {
	*asyncInputSession
	steerMessages []Message
	attachments   []Message
}

func (s *dispatchSession) Send(prompt, id string, images []ImageAttachment, files []FileAttachment) error {
	s.mu.Lock()
	s.attachments = append(s.attachments, Message{MessageID: id, Images: images, Files: files})
	s.mu.Unlock()
	return s.asyncInputSession.Send(prompt, id, images, files)
}

func (s *dispatchSession) SteerMessage(prompt, id string, images []ImageAttachment, files []FileAttachment) (bool, error) {
	accepted, err := s.SteerUserInput(prompt)
	if accepted {
		s.mu.Lock()
		s.steerMessages = append(s.steerMessages, Message{Content: prompt, MessageID: id, Images: images, Files: files})
		s.mu.Unlock()
	}
	return accepted, err
}

type dispatchEnv struct {
	*receiptAckEnv
	session *dispatchSession
}

func newDispatchEnv(t *testing.T, mode string) *dispatchEnv {
	p := &stubPlatformEngine{n: "test"}
	s := &dispatchSession{asyncInputSession: &asyncInputSession{events: make(chan Event, 16)}}
	e := NewEngine("dispatch", &controllableAgent{nextSession: s}, []Platform{p}, t.TempDir()+"/sessions.json", LangEnglish)
	e.SetBusyMessageMode(mode)
	t.Cleanup(func() { _ = e.Stop() })
	return &dispatchEnv{receiptAckEnv: &receiptAckEnv{cujEnv: &cujEnv{t: t, engine: e, plat: p}}, session: s}
}

func (env *dispatchEnv) awaitSends(n int) {
	env.t.Helper()
	env.waitFor("send count", 3*time.Second, func() bool {
		env.session.mu.Lock()
		defer env.session.mu.Unlock()
		return len(env.session.sent) == n
	})
}

func (env *dispatchEnv) finish(answer string) {
	env.session.mu.Lock()
	env.session.active = false
	env.session.mu.Unlock()
	env.session.events <- Event{Type: EventResult, Content: answer, Done: true}
	env.awaitVisible(answer)
}

func (env *dispatchEnv) assertCalls(sends, steers int) {
	env.t.Helper()
	env.session.mu.Lock()
	defer env.session.mu.Unlock()
	if len(env.session.sent) != sends || len(env.session.steered) != steers {
		env.t.Fatalf("sends=%v steers=%v, want %d/%d", env.session.sent, env.session.steered, sends, steers)
	}
}

func TestBusyMessageMode_QueueDefaultAndExplicit(t *testing.T) {
	for _, mode := range []string{"", "queue"} {
		t.Run(mode, func(t *testing.T) {
			env := newDispatchEnv(t, mode)
			env.send("first", "first task", 1000)
			env.awaitSends(1)
			env.send("next", "next task", 2000)
			env.awaitVisible(env.engine.i18n.T(MsgMessageQueued))
			env.assertCalls(1, 0)
			env.finish("first result")
			env.awaitSends(2)
			env.finish("next result")
			env.assertReceipts("first", "next")
		})
	}
}

func TestBusyMessageMode_SteerPreservesMetadataAndHistory(t *testing.T) {
	env := newDispatchEnv(t, " STEER ")
	env.send("first", "first task", 1000)
	env.awaitSends(1)
	msg := &Message{SessionKey: "test:receipt-user", Platform: "test", UserID: "receipt-user", MessageID: "supplement", ReplyCtx: "supplement", Content: "use these", ExtraContent: "quoted context", UserMessageTimeMs: 2000,
		Images: []ImageAttachment{{MimeType: "image/png", Data: []byte("image")}}, Files: []FileAttachment{{FileName: "notes.txt", Data: []byte("notes")}},
		OnAccepted: func() { _ = env.plat.Reply(context.Background(), "supplement", "receipt:supplement") },
	}
	env.engine.ReceiveMessage(env.plat, msg)
	env.awaitVisible(env.engine.i18n.T(MsgMessageSteered))
	env.assertCalls(1, 1)
	env.assertReceipts("first", "supplement")
	env.session.mu.Lock()
	got := env.session.steerMessages[0]
	env.session.mu.Unlock()
	if got.Content != "quoted context\nuse these" || got.MessageID != "supplement" || len(got.Images) != 1 || len(got.Files) != 1 {
		t.Fatalf("supplement lost content: %#v", got)
	}
	env.send("stale", "older redelivery", 1500)
	env.assertCalls(1, 1)
	env.assertReceipts("first", "supplement")
	history := env.engine.sessions.GetOrCreateActive("test:receipt-user").GetHistory(10)
	if len(history) < 2 || history[len(history)-1].Content != "quoted context\nuse these" {
		t.Fatalf("supplement missing from history: %#v", history)
	}
	env.finish("done with supplement")
}

func TestBusyMessageMode_SteerFailureDoesNotDuplicate(t *testing.T) {
	env := newDispatchEnv(t, "steer")
	env.send("first", "first task", 1000)
	env.awaitSends(1)
	env.session.mu.Lock()
	env.session.err = errors.New("delivery response timed out")
	env.session.mu.Unlock()
	env.send("uncertain", "supplement", 2000)
	env.awaitVisible(env.engine.i18n.T(MsgMessageSteerFailed))
	env.assertReceipts("first")
	env.finish("first answer")
	env.waitFor("idle", 3*time.Second, func() bool { return !env.engine.sessions.GetOrCreateActive("test:receipt-user").Busy() })
	env.assertCalls(1, 0)
}

func TestBusyMessageMode_StartupUnsupportedAndCompletedTurnsQueue(t *testing.T) {
	for _, phase := range []string{"startup", "unsupported", "completed"} {
		t.Run(phase, func(t *testing.T) {
			if phase == "completed" {
				env := newDispatchEnv(t, "steer")
				env.send("first", "first task", 1000)
				env.awaitSends(1)
				env.session.mu.Lock()
				env.session.active = false // Agent completed; bridge still delivering its result.
				env.session.mu.Unlock()
				env.send("late", "late follow-up", 2000)
				env.awaitVisible(env.engine.i18n.T(MsgMessageQueued))
				env.finish("first result")
				env.awaitSends(2)
				env.finish("late result")
				env.assertCalls(2, 0)
				return
			}
			env := newReceiptAckEnv(t, nil)
			env.engine.SetBusyMessageMode("steer")
			env.send("first", "first task", 1000)
			env.awaitStartup()
			if phase == "unsupported" {
				close(env.startup.release)
				env.awaitSendCount(1)
			}
			env.send("next", "next task", 2000)
			env.awaitVisible(env.engine.i18n.T(MsgMessageQueued))
			if phase == "startup" {
				close(env.startup.release)
				env.awaitSendCount(1)
			}
			env.startup.session.events <- Event{Type: EventResult, Content: "first answer", Done: true}
			env.awaitSendCount(2)
			env.startup.session.events <- Event{Type: EventResult, Content: "next answer", Done: true}
			env.awaitVisible("next answer")
			env.assertReceipts("first", "next")
		})
	}
}

func TestQueueCommand_PreservesLiteralPayloadAndAttachmentsWhenIdle(t *testing.T) {
	for _, separator := range []string{" ", "\n", "\t"} {
		t.Run(fmt.Sprintf("separator=%q", separator), func(t *testing.T) {
			env := newDispatchEnv(t, "steer")
			payload := "/new \"keep  both spaces\"\nsecond line"
			env.engine.ReceiveMessage(env.plat, &Message{SessionKey: "test:receipt-user", Platform: "test", UserID: "receipt-user", MessageID: "literal", Content: "/queue" + separator + payload, ExtraContent: "reply quote", ReplyCtx: "ctx", Images: []ImageAttachment{{MimeType: "image/png", Data: []byte("image")}}})
			env.awaitSends(1)
			env.session.mu.Lock()
			got := env.session.sent[0]
			attachments := env.session.attachments[0]
			env.session.mu.Unlock()
			if got != "reply quote\n"+payload {
				t.Fatalf("literal payload changed: %q", got)
			}
			if attachments.MessageID != "literal" || len(attachments.Images) != 1 {
				t.Fatalf("queued attachment lost: %#v", attachments)
			}
			env.finish("literal response")
		})
	}
}

func TestQueueCommand_UsageDisabledAndQueueLimit(t *testing.T) {
	env := newDispatchEnv(t, "steer")
	env.send("empty", "/queue", 1000)
	env.awaitVisible(env.engine.i18n.T(MsgQueueEmpty))
	env.engine.SetDisabledCommands([]string{"queue"})
	env.send("disabled", "/queue task", 2000)
	env.awaitVisible(fmt.Sprintf(env.engine.i18n.T(MsgCommandDisabled), "/queue"))
	env.assertReceipts()
	env.engine.SetDisabledCommands(nil)
	env.engine.SetMaxQueuedMessages(1)
	env.send("first", "first task", 3000)
	env.awaitSends(1)
	env.send("queued", "/queue next task", 4000)
	env.send("full", "/queue one too many", 5000)
	env.awaitVisible(fmt.Sprintf(env.engine.i18n.T(MsgQueueFull), 1))
	env.assertReceipts("first", "queued")
	env.assertCalls(1, 0)
	env.finish("first answer")
	env.awaitSends(2)
	env.finish("queued answer")
	for _, command := range env.engine.GetAllCommands() {
		if command.Command == "queue" && strings.Contains(command.Description, "Queue") {
			return
		}
	}
	t.Fatal("queue missing from command menu")
}

func TestQueueCommand_DoesNotAnswerPendingQuestion(t *testing.T) {
	env := newDispatchEnv(t, "steer")
	env.send("first", "first task", 1000)
	env.awaitSends(1)
	env.engine.interactiveMu.Lock()
	state := env.engine.interactiveStates["test:receipt-user"]
	env.engine.interactiveMu.Unlock()
	state.mu.Lock()
	question := &pendingUserInput{id: "pending", questions: []UserQuestion{{Question: "Which database?"}}, answers: make([]string, 1)}
	state.pendingUserInput = question
	state.mu.Unlock()
	env.send("queued", "/queue later task", 2000)
	env.awaitVisible(env.engine.i18n.T(MsgMessageQueued))
	env.assertCalls(1, 0)
	state.mu.Lock()
	unchanged := state.pendingUserInput == question && question.answers[0] == ""
	state.mu.Unlock()
	if !unchanged {
		t.Fatal("queue payload was consumed as a question answer")
	}
	env.finish("first answer")
	env.awaitSends(2)
	env.finish("later answer")
}

func TestBusyMessageMode_StopDuringStartupClosesLateAgent(t *testing.T) {
	env := newReceiptAckEnv(t, nil)
	env.engine.SetBusyMessageMode("steer")
	env.send("first", "first task", 1000)
	env.awaitStartup()
	env.send("queued", "/queue later task", 2000)
	env.send("stop", "/stop", 3000)
	close(env.startup.release)
	env.waitFor("late agent closed", 3*time.Second, func() bool { return !env.startup.session.Alive() })
	env.startup.session.sendMu.Lock()
	count := len(env.startup.session.sendCalls)
	env.startup.session.sendMu.Unlock()
	if count != 0 {
		t.Fatalf("stopped startup sent %d prompts", count)
	}
}

func TestBusyMessageMode_StopDuringStartupAllowsImmediateReplacement(t *testing.T) {
	env := newReceiptAckEnv(t, nil)
	env.engine.SetBusyMessageMode("steer")
	replacement := &receiptAgentSession{queuingAgentSession: newQueuingSession("replacement")}
	starts := 0
	env.engine.agent = &controllableAgent{startSessionFn: func(ctx context.Context, id string) (AgentSession, error) {
		starts++
		if starts == 1 {
			return env.startup.StartSession(ctx, id)
		}
		return replacement, nil
	}}
	env.send("first", "first task", 1000)
	env.awaitStartup()
	env.send("stop", "/stop", 2000)
	env.send("replacement", "replacement task", 3000)
	close(env.startup.release)
	env.waitFor("replacement reaches the agent", 3*time.Second, func() bool {
		replacement.sendMu.Lock()
		defer replacement.sendMu.Unlock()
		return len(replacement.sendCalls) == 1
	})
	if env.startup.session.Alive() {
		t.Fatal("canceled startup agent was not closed")
	}
	replacement.events <- Event{Type: EventResult, Content: "replacement done", Done: true}
	env.awaitVisible("replacement done")
}
