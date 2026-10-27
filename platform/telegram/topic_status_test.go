package telegram

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
	tgbot "github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

type statusBot struct {
	*stubTelegramBot
	statusMu   sync.Mutex
	iconsCalls int
	edits      []*tgbot.EditForumTopicParams
	edit       func(context.Context, *tgbot.EditForumTopicParams) (bool, error)
}

func (b *statusBot) GetForumTopicIconStickers(context.Context) ([]*models.Sticker, error) {
	b.statusMu.Lock()
	b.iconsCalls++
	b.statusMu.Unlock()
	var icons []*models.Sticker
	for status, emoji := range defaultTopicStatusEmoji {
		icons = append(icons, &models.Sticker{Emoji: emoji + "\ufe0f", CustomEmojiID: string(status) + "-id"})
	}
	return icons, nil
}
func (b *statusBot) EditForumTopic(ctx context.Context, params *tgbot.EditForumTopicParams) (bool, error) {
	b.statusMu.Lock()
	copy := *params
	b.edits = append(b.edits, &copy)
	b.statusMu.Unlock()
	if b.edit != nil {
		return b.edit(ctx, params)
	}
	return true, nil
}
func statusTestPlatform(t *testing.T) (*Platform, *statusBot) {
	t.Helper()
	u, err := newTopicStatusUpdater(map[string]any{"topic_status_icons": true})
	if err != nil {
		t.Fatal(err)
	}
	b := &statusBot{stubTelegramBot: newStubTelegramBot()}
	return &Platform{topicStatus: u, bot: newRateLimitRetryBot(b)}, b
}
func TestTopicStatus_UsesAllowedIconWithoutChangingName(t *testing.T) {
	p, b := statusTestPlatform(t)
	for status := range defaultTopicStatusEmoji {
		if err := p.applyTopicStatus(context.Background(), topicStatusTarget{42, 7}, status); err != nil {
			t.Fatal(err)
		}
	}
	b.statusMu.Lock()
	defer b.statusMu.Unlock()
	if b.iconsCalls != 1 {
		t.Fatalf("catalog requests=%d", b.iconsCalls)
	}
	for _, edit := range b.edits {
		if edit.ChatID != int64(42) || edit.MessageThreadID != 7 || edit.Name != "" || edit.IconCustomEmojiID == "" {
			t.Fatalf("invalid edit: %+v", edit)
		}
	}
}
func TestTopicStatus_CoalescesAndPreservesNewStateDuringEdit(t *testing.T) {
	p, _ := statusTestPlatform(t)
	ctx := context.Background()
	rc := replyContext{chatID: 42, threadID: 7}
	p.SetConversationStatus(ctx, rc, core.ConversationWorking)
	p.SetConversationStatus(ctx, rc, core.ConversationQuestion)
	target, update, ok := p.topicStatus.next()
	if !ok || update.status != core.ConversationQuestion {
		t.Fatal("did not coalesce latest state")
	}
	// A working update arrives while the question edit is in flight.
	p.SetConversationStatus(ctx, rc, core.ConversationWorking)
	p.topicStatus.complete(target, update)
	target, update, ok = p.topicStatus.next()
	if !ok || update.status != core.ConversationWorking {
		t.Fatal("in-flight edit erased newer state")
	}
	p.topicStatus.complete(target, update)
	p.SetConversationStatus(ctx, rc, core.ConversationWorking)
	if _, _, ok := p.topicStatus.next(); ok {
		t.Fatal("identical successful state was requeued")
	}
	p.SetConversationStatus(ctx, replyContext{chatID: 42}, core.ConversationFailed)
	if _, _, ok := p.topicStatus.next(); ok {
		t.Fatal("non-topic chat queued an icon edit")
	}
}
func TestTopicStatus_RateLimitRetriesLatestStateAndCancels(t *testing.T) {
	p, b := statusTestPlatform(t)
	calls := make(chan string, 4)
	b.edit = func(_ context.Context, params *tgbot.EditForumTopicParams) (bool, error) {
		calls <- params.IconCustomEmojiID
		return false, &tgbot.TooManyRequestsError{RetryAfter: 1}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); p.runTopicStatus(ctx) }()
	rc := replyContext{chatID: 42, threadID: 7}
	p.SetConversationStatus(ctx, rc, core.ConversationWorking)
	select {
	case <-calls:
	case <-time.After(3 * time.Second):
		t.Fatal("first edit missing")
	}
	p.SetConversationStatus(ctx, rc, core.ConversationQuestion)
	select {
	case got := <-calls:
		if got != "question-id" {
			t.Fatalf("retried stale state: %s", got)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("retry missing")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("worker did not cancel retry wait")
	}
}
func TestTopicStatus_ConfigAndOptionalWrapper(t *testing.T) {
	if u, err := newTopicStatusUpdater(nil); err != nil || u != nil {
		t.Fatal("must default to disabled")
	}
	if _, err := newTopicStatusUpdater(map[string]any{"topic_status_icons": "true"}); err == nil {
		t.Fatal("accepted invalid flag")
	}
	if _, err := newTopicStatusUpdater(map[string]any{"topic_status_icons": true, "topic_status_working_emoji": ""}); err == nil {
		t.Fatal("accepted empty emoji")
	}
	wrapper := newRateLimitRetryBot(newStubTelegramBot()).(forumTopicStatusAPI)
	if _, err := wrapper.GetForumTopicIconStickers(context.Background()); !errors.Is(err, core.ErrNotSupported) {
		t.Fatalf("optional API: %v", err)
	}
}
