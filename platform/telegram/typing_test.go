package telegram

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
	tgbot "github.com/go-telegram/bot"
)

type typingTestBot struct {
	*stubTelegramBot
	actions chan tgbot.SendChatActionParams
	action  func(context.Context) error
}

func (b *typingTestBot) SendChatAction(ctx context.Context, params *tgbot.SendChatActionParams) (bool, error) {
	b.actions <- *params
	if b.action != nil {
		if err := b.action(ctx); err != nil {
			return false, err
		}
	}
	return true, nil
}

func awaitTyping(t *testing.T, b *typingTestBot, rc replyContext) {
	t.Helper()
	select {
	case params := <-b.actions:
		if params.ChatID != rc.chatID || params.MessageThreadID != rc.threadID || params.Action != "typing" {
			t.Fatalf("typing target = %+v, want chat %d topic %d", params, rc.chatID, rc.threadID)
		}
	case <-time.After(time.Second):
		t.Fatal("typing indicator was not refreshed")
	}
}

func newTypingTestPlatform(t *testing.T) (*Platform, *typingTestBot, *stubTypingTicker) {
	t.Helper()
	b := &typingTestBot{stubTelegramBot: newStubTelegramBot(), actions: make(chan tgbot.SendChatActionParams, 32)}
	ticker := newStubTypingTicker()
	p := &Platform{bot: b, newTypingTicker: func(time.Duration) typingTicker { return ticker }}
	t.Cleanup(func() { _ = p.Stop() })
	return p, b, ticker
}

func TestTypingRefreshesBeforeTelegramExpiry(t *testing.T) {
	p, b, ticker := newTypingTestPlatform(t)
	intervals := make(chan time.Duration, 1)
	p.newTypingTicker = func(d time.Duration) typingTicker { intervals <- d; return ticker }
	rc := replyContext{chatID: 1, threadID: 7}
	stop := p.StartTyping(context.Background(), rc)
	defer stop()
	awaitTyping(t, b, rc)
	if d := <-intervals; d > 3*time.Second {
		t.Fatalf("typing interval %s leaves no margin before Telegram's 5-second expiry", d)
	}
	for range 3 {
		ticker.ch <- time.Now()
		awaitTyping(t, b, rc)
	}
}

func TestTypingRestoredAfterProgressMessages(t *testing.T) {
	rc := replyContext{chatID: 1, threadID: 7, messageID: 3}
	tests := []struct {
		name string
		send func(*Platform) error
	}{
		{"send", func(p *Platform) error { return p.Send(context.Background(), rc, "Working") }},
		{"reply", func(p *Platform) error { return p.Reply(context.Background(), rc, "Working") }},
		{"preview", func(p *Platform) error { _, err := p.SendPreviewStart(context.Background(), rc, "Working"); return err }},
		{"edit", func(p *Platform) error {
			return p.UpdateMessage(context.Background(), &telegramPreviewHandle{chatID: 1, threadID: 7, messageID: 9}, "Working")
		}},
		{"buttons", func(p *Platform) error { return p.SendWithButtons(context.Background(), rc, "Choose", nil) }},
		{"image", func(p *Platform) error {
			return p.SendImage(context.Background(), rc, core.ImageAttachment{Data: []byte("image")})
		}},
		{"file", func(p *Platform) error {
			return p.SendFile(context.Background(), rc, core.FileAttachment{Data: []byte("file")})
		}},
		{"voice", func(p *Platform) error { return p.SendAudio(context.Background(), rc, []byte("voice"), "ogg") }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, b, _ := newTypingTestPlatform(t)
			stop := p.StartTyping(context.Background(), rc)
			defer stop()
			awaitTyping(t, b, rc)
			if err := tt.send(p); err != nil {
				t.Fatal(err)
			}
			// No timer tick: a progress message must restore typing immediately.
			awaitTyping(t, b, rc)
		})
	}
}

type observedTypingTicker struct {
	ch      chan time.Time
	waiting chan struct{}
	stopped chan struct{}
	once    sync.Once
}

func (t *observedTypingTicker) C() <-chan time.Time { t.waiting <- struct{}{}; return t.ch }
func (t *observedTypingTicker) Stop()               { t.once.Do(func() { close(t.stopped) }) }

func TestTypingSurvivesDisconnectedHeartbeat(t *testing.T) {
	for _, initiallyConnected := range []bool{true, false} {
		t.Run(map[bool]string{true: "reconnect", false: "initially disconnected"}[initiallyConnected], func(t *testing.T) {
			p, b, _ := newTypingTestPlatform(t)
			ticker := &observedTypingTicker{ch: make(chan time.Time), waiting: make(chan struct{}, 8), stopped: make(chan struct{})}
			p.newTypingTicker = func(time.Duration) typingTicker { return ticker }
			if !initiallyConnected {
				p.bot = nil
			}
			rc := replyContext{chatID: 1, threadID: 7}
			stop := p.StartTyping(context.Background(), rc)
			defer stop()
			if initiallyConnected {
				awaitTyping(t, b, rc)
			}
			select {
			case <-ticker.waiting:
			case <-time.After(time.Second):
				t.Fatal("typing loop never started")
			}
			p.mu.Lock()
			p.bot = nil
			p.mu.Unlock()
			ticker.ch <- time.Now()
			select {
			case <-ticker.waiting:
			case <-ticker.stopped:
				t.Fatal("temporary disconnect permanently stopped typing")
			case <-time.After(time.Second):
				t.Fatal("typing loop did not retry")
			}
			p.publishBot(b, nil)
			ticker.ch <- time.Now()
			awaitTyping(t, b, rc)
		})
	}
}

func TestTypingStopCancelsInFlightRequestAndIsIdempotent(t *testing.T) {
	p, b, ticker := newTypingTestPlatform(t)
	var calls atomic.Int32
	blocked := make(chan struct{})
	b.action = func(ctx context.Context) error {
		if calls.Add(1) == 1 {
			return nil
		}
		close(blocked)
		<-ctx.Done()
		return ctx.Err()
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rc := replyContext{chatID: 1, threadID: 7}
	stop := p.StartTyping(ctx, rc)
	awaitTyping(t, b, rc)
	ticker.ch <- time.Now()
	awaitTyping(t, b, rc)
	<-blocked
	done := make(chan struct{})
	go func() { stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("stop did not cancel in-flight typing")
	}
	stop()
	if err := p.Send(context.Background(), rc, "Final answer"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-b.actions:
		t.Fatal("typing restarted after stop")
	case <-time.After(20 * time.Millisecond):
	}
}

func TestTypingRefreshIsScopedToActiveChatAndTopic(t *testing.T) {
	p, b, _ := newTypingTestPlatform(t)
	rc := replyContext{chatID: 1, threadID: 7}
	stop := p.StartTyping(context.Background(), rc)
	defer stop()
	awaitTyping(t, b, rc)
	for _, other := range []replyContext{{chatID: 2, threadID: 7}, {chatID: 1, threadID: 8}} {
		if err := p.Send(context.Background(), other, "Elsewhere"); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case <-b.actions:
		t.Fatal("unrelated topic refreshed typing")
	case <-time.After(20 * time.Millisecond):
	}
	if err := p.Send(context.Background(), replyContext{chatID: 1, threadID: 7, messageID: 99}, "Progress"); err != nil {
		t.Fatal(err)
	}
	awaitTyping(t, b, rc)
}

func TestTypingRetriesTransientAPIFailure(t *testing.T) {
	p, b, ticker := newTypingTestPlatform(t)
	var calls atomic.Int32
	b.action = func(context.Context) error {
		if calls.Add(1) == 1 {
			return errors.New("temporary failure")
		}
		return nil
	}
	rc := replyContext{chatID: 1, threadID: 7}
	stop := p.StartTyping(context.Background(), rc)
	defer stop()
	awaitTyping(t, b, rc)
	ticker.ch <- time.Now()
	awaitTyping(t, b, rc)
}

func TestTypingCleansUpOnCancellationAndPlatformStop(t *testing.T) {
	for _, shutdown := range []bool{false, true} {
		t.Run(map[bool]string{false: "context", true: "platform"}[shutdown], func(t *testing.T) {
			p, b, _ := newTypingTestPlatform(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			rc := replyContext{chatID: 1, threadID: 7}
			stop := p.StartTyping(ctx, rc)
			awaitTyping(t, b, rc)
			p.mu.RLock()
			var done <-chan struct{}
			for s := range p.typing {
				done = s.done
			}
			p.mu.RUnlock()
			if shutdown {
				_ = p.Stop()
			} else {
				cancel()
			}
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("typing loop survived cancellation")
			}
			p.mu.RLock()
			remaining := len(p.typing)
			p.mu.RUnlock()
			if remaining != 0 {
				t.Fatal("finished typing loop leaked in registry")
			}
			stop()
		})
	}
}
