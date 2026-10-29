package telegram

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
	tgbot "github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

type readReceiptBot struct {
	*stubTelegramBot
	calls     chan *tgbot.SetMessageReactionParams
	err       error
	ok        bool
	failFirst bool
}

func (b *readReceiptBot) SetMessageReaction(_ context.Context, params *tgbot.SetMessageReactionParams) (bool, error) {
	b.calls <- params
	if b.failFirst {
		b.failFirst = false
		return false, &tgbot.TooManyRequestsError{RetryAfter: 1}
	}
	return b.ok, b.err
}

func TestReadReceipts_DefaultAndOptOut(t *testing.T) {
	for _, disabled := range []bool{false, true} {
		opts := map[string]any{"token": "test-token", "allow_from": "*"}
		if disabled {
			opts["read_receipts"] = false
		}
		platform, err := New(opts)
		if err != nil {
			t.Fatal(err)
		}
		p := platform.(*Platform)
		p.handler = func(_ core.Platform, msg *core.Message) {
			if (msg.OnDelivered != nil) == disabled {
				t.Fatalf("receipt callback, disabled=%v", disabled)
			}
		}
		p.dispatchMessage(context.Background(), &core.Message{}, &models.Message{ID: 42, Chat: models.Chat{ID: 17}})
	}
}

func TestReadReceipt_MarksOriginalMessageOnlyAfterDelivery(t *testing.T) {
	bot := &readReceiptBot{stubTelegramBot: newStubTelegramBot(), calls: make(chan *tgbot.SetMessageReactionParams, 4), ok: true}
	p := &Platform{bot: bot, readReceipts: true, enableReactions: true}
	var delivered func()
	p.handler = func(_ core.Platform, msg *core.Message) { delivered = msg.OnDelivered }
	p.handleUpdate(context.Background(), &models.Update{Message: &models.Message{
		ID: 42, Date: int(time.Now().Unix()), Text: "hello", From: &models.User{ID: 7},
		Chat: models.Chat{ID: 17, Type: models.ChatTypePrivate}, MessageThreadID: 21,
	}})
	if delivered == nil {
		t.Fatal("missing delivery callback")
	}
	select {
	case <-bot.calls:
		t.Fatal("reaction sent on arrival, before Codex acknowledgment")
	default:
	}
	delivered()
	delivered()
	select {
	case call := <-bot.calls:
		if call.ChatID != int64(17) || call.MessageID != 42 || len(call.Reaction) != 1 || call.Reaction[0].ReactionTypeEmoji.Emoji != "👀" {
			t.Fatalf("reaction target or emoji = %+v", call)
		}
	case <-time.After(time.Second):
		t.Fatal("missing read indicator")
	}
	select {
	case <-bot.calls:
		t.Fatal("duplicate or legacy reaction overwrote the read indicator")
	case <-time.After(25 * time.Millisecond):
	}
}

func TestReadReceipt_ReportsTelegramFailure(t *testing.T) {
	for _, apiErr := range []error{nil, errors.New("reaction not allowed")} {
		bot := &readReceiptBot{stubTelegramBot: newStubTelegramBot(), calls: make(chan *tgbot.SetMessageReactionParams, 1), err: apiErr}
		p := &Platform{bot: bot}
		if err := p.markMessageReceived(context.Background(), 17, 42); err == nil {
			t.Fatal("Telegram failure was ignored")
		}
	}
}

func TestReadReceipt_RetriesTelegramRateLimit(t *testing.T) {
	previousSleep := sleepCtx
	t.Cleanup(func() { sleepCtx = previousSleep })
	var waits []time.Duration
	sleepCtx = func(_ context.Context, delay time.Duration) error {
		waits = append(waits, delay)
		return nil
	}
	bot := &readReceiptBot{stubTelegramBot: newStubTelegramBot(), calls: make(chan *tgbot.SetMessageReactionParams, 2), ok: true, failFirst: true}
	p := &Platform{bot: bot}
	if err := p.markMessageReceived(context.Background(), 17, 42); err != nil {
		t.Fatal(err)
	}
	if len(waits) != 1 || waits[0] != time.Second || len(bot.calls) != 2 {
		t.Fatalf("retry waits=%v, attempts=%d", waits, len(bot.calls))
	}
}
