package core

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

type receiptSteerSession struct {
	*dispatchSession
	entered chan struct{}
	ack     chan error
}

func (s *receiptSteerSession) SteerMessage(prompt, id string, images []ImageAttachment, files []FileAttachment) (bool, error) {
	close(s.entered)
	if err := <-s.ack; err != nil {
		return false, err
	}
	return s.dispatchSession.SteerMessage(prompt, id, images, files)
}

func TestBusyMessageMode_ReadReceiptWaitsForSteeringConfirmation(t *testing.T) {
	for _, rejected := range []bool{false, true} {
		t.Run(map[bool]string{false: "accepted", true: "rejected"}[rejected], func(t *testing.T) {
			env := newDispatchEnv(t, "steer")
			s := &receiptSteerSession{dispatchSession: env.session, entered: make(chan struct{}), ack: make(chan error, 1)}
			t.Cleanup(func() {
				select {
				case s.ack <- errors.New("test finished"):
				default:
				}
			})
			env.engine.agent = &controllableAgent{nextSession: s}
			env.send("first", "first task", 1000)
			env.awaitSends(1)
			var reads atomic.Int32
			done := make(chan struct{})
			go func() {
				defer close(done)
				env.engine.ReceiveMessage(env.plat, &Message{SessionKey: "test:receipt-user", Platform: "test", MessageID: "supplement", Content: "adjust task", ReplyCtx: "ctx", OnDelivered: func() { reads.Add(1) }})
			}()
			select {
			case <-s.entered:
			case <-time.After(3 * time.Second):
				t.Fatal("steer not attempted")
			}
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("steering confirmation blocked the platform callback")
			}
			if reads.Load() != 0 {
				t.Fatal("message marked read before native steering confirmation")
			}
			var err error
			if rejected {
				err = errors.New("native steer rejected")
			}
			s.ack <- err
			confirmation := MsgMessageSteered
			if rejected {
				confirmation = MsgMessageSteerFailed
			}
			env.awaitVisible(env.engine.i18n.T(confirmation))
			want := int32(1)
			if rejected {
				want = 0
			}
			if reads.Load() != want {
				t.Fatalf("read receipts = %d, want %d", reads.Load(), want)
			}
			env.finish("first result")
		})
	}
}

func TestQueueCommand_ReadReceiptWaitsForQueuedDelivery(t *testing.T) {
	env := newDeliveryEnv(t, nil)
	env.engine.SetBusyMessageMode("steer")
	env.send("first", "first task")
	env.startup()
	close(env.agent.release)
	first := env.attempt("first")
	first.ack <- nil
	env.visible("read:first")
	env.send("later", "/queue later task")
	env.visible(env.engine.i18n.T(MsgMessageQueued))
	env.receipts("first")
	env.agent.session.events <- Event{Type: EventResult, Content: "first done", Done: true}
	later := env.attempt("later")
	env.receipts("first")
	later.ack <- nil
	env.visible("read:later")
	env.receipts("first", "later")
	env.agent.session.events <- Event{Type: EventResult, Content: "later done", Done: true}
	env.visible("later done")
}
