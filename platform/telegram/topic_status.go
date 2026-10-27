package telegram

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/chenhg5/cc-connect/core"
	tgbot "github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

type forumTopicStatusAPI interface {
	GetForumTopicIconStickers(context.Context) ([]*models.Sticker, error)
	EditForumTopic(context.Context, *tgbot.EditForumTopicParams) (bool, error)
}

// Resolve allowed IDs from Telegram instead of assuming custom emoji IDs are
// portable across bot deployments. Emoji are configurable without changing titles.
var defaultTopicStatusEmoji = map[core.ConversationStatus]string{
	core.ConversationWorking:  "⚡",
	core.ConversationQuestion: "❓",
	core.ConversationApproval: "👀",
	core.ConversationFinished: "✅",
	core.ConversationFailed:   "❗",
	core.ConversationStopped:  "🏁",
}

type topicStatusTarget struct {
	chatID   int64
	threadID int
}
type topicStatusUpdate struct {
	status  core.ConversationStatus
	version uint64
}
type topicStatusUpdater struct {
	mu      sync.Mutex
	pending map[topicStatusTarget]topicStatusUpdate
	applied map[topicStatusTarget]core.ConversationStatus
	version uint64
	wake    chan struct{}
	emoji   map[core.ConversationStatus]string
	icons   map[core.ConversationStatus]string // owned by worker
}

func newTopicStatusUpdater(opts map[string]any) (*topicStatusUpdater, error) {
	enabled := false
	if value, exists := opts["topic_status_icons"]; exists {
		var ok bool
		enabled, ok = value.(bool)
		if !ok {
			return nil, fmt.Errorf("telegram: topic_status_icons must be a boolean")
		}
	}
	if !enabled {
		return nil, nil
	}
	updater := &topicStatusUpdater{
		pending: make(map[topicStatusTarget]topicStatusUpdate),
		applied: make(map[topicStatusTarget]core.ConversationStatus),
		wake:    make(chan struct{}, 1), emoji: make(map[core.ConversationStatus]string),
	}
	for status, fallback := range defaultTopicStatusEmoji {
		emoji := fallback
		if value, exists := opts["topic_status_"+string(status)+"_emoji"]; exists {
			var ok bool
			emoji, ok = value.(string)
			if !ok || strings.TrimSpace(emoji) == "" {
				return nil, fmt.Errorf("telegram: topic_status_%s_emoji must be a nonempty string", status)
			}
		}
		updater.emoji[status] = normalizeTopicEmoji(emoji)
	}
	return updater, nil
}

func normalizeTopicEmoji(emoji string) string {
	return strings.ReplaceAll(strings.TrimSpace(emoji), "\ufe0f", "")
}

func (p *Platform) SetConversationStatus(ctx context.Context, replyCtx any, status core.ConversationStatus) {
	updater := p.topicStatus
	rc, ok := replyCtx.(replyContext)
	if updater == nil || !ok || rc.threadID <= 0 || ctx.Err() != nil {
		return
	}
	if updater.emoji[status] == "" {
		return
	}
	target := topicStatusTarget{rc.chatID, rc.threadID}
	updater.mu.Lock()
	if current, exists := updater.pending[target]; exists && current.status == status {
		updater.mu.Unlock()
		return
	}
	// A newer state must supersede an in-flight edit even if it matches the
	// previously applied icon; the older API request may still succeed.
	if _, pending := updater.pending[target]; !pending && updater.applied[target] == status {
		updater.mu.Unlock()
		return
	}
	updater.version++
	updater.pending[target] = topicStatusUpdate{status, updater.version}
	updater.mu.Unlock()
	select {
	case updater.wake <- struct{}{}:
	default:
	}
}

func (u *topicStatusUpdater) next() (topicStatusTarget, topicStatusUpdate, bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	var target topicStatusTarget
	var update topicStatusUpdate
	for key, candidate := range u.pending {
		if update.version == 0 || candidate.version < update.version {
			target, update = key, candidate
		}
	}
	return target, update, update.version != 0
}

func (u *topicStatusUpdater) complete(target topicStatusTarget, update topicStatusUpdate) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.applied[target] = update.status
	if u.pending[target].version == update.version {
		delete(u.pending, target)
	}
}

func (p *Platform) runTopicStatus(ctx context.Context) {
	updater := p.topicStatus
	for {
		select {
		case <-ctx.Done():
			return
		case <-updater.wake:
		}
		// Coalesce very short turns. All topic edits share this worker so they stay
		// ordered and never block message processing, including rate-limit waits.
		if err := sleepCtx(ctx, 300*time.Millisecond); err != nil {
			return
		}
		for {
			target, update, ok := updater.next()
			if !ok {
				break
			}
			err := p.applyTopicStatus(ctx, target, update.status)
			if err == nil {
				updater.complete(target, update)
			} else {
				if ctx.Err() != nil {
					return
				}
				slog.Warn("telegram: update topic status failed", "chat_id", target.chatID, "thread_id", target.threadID, "error", core.RedactToken(err.Error(), p.token))
				// Missing/deleted topics and denied topic-management rights need
				// an external change. Do not let them stall all other topics.
				if errors.Is(err, tgbot.ErrorBadRequest) || errors.Is(err, tgbot.ErrorForbidden) || errors.Is(err, core.ErrNotSupported) {
					updater.mu.Lock()
					if updater.pending[target].version == update.version {
						delete(updater.pending, target)
					}
					updater.mu.Unlock()
					continue
				}
				wait := 30 * time.Second
				var limited *tgbot.TooManyRequestsError
				if errors.As(err, &limited) && limited.RetryAfter > 0 {
					wait = time.Duration(limited.RetryAfter) * time.Second
				}
				// Rotate retries behind other topics; retain the latest desired status.
				updater.mu.Lock()
				if current := updater.pending[target]; current.version == update.version {
					updater.version++
					current.version = updater.version
					updater.pending[target] = current
				}
				updater.mu.Unlock()
				if err := sleepCtx(ctx, wait); err != nil {
					return
				}
			}
			if err := sleepCtx(ctx, time.Second); err != nil {
				return
			}
		}
	}
}

func (p *Platform) applyTopicStatus(ctx context.Context, target topicStatusTarget, status core.ConversationStatus) error {
	bot, err := p.connectedBot("update topic status")
	if err != nil {
		return err
	}
	api, ok := bot.(forumTopicStatusAPI)
	if !ok {
		return core.ErrNotSupported
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	updater := p.topicStatus
	if updater.icons == nil {
		stickers, err := api.GetForumTopicIconStickers(ctx)
		if err != nil {
			return err
		}
		icons := make(map[core.ConversationStatus]string)
		for _, sticker := range stickers {
			if sticker == nil || sticker.CustomEmojiID == "" {
				continue
			}
			for state, emoji := range updater.emoji {
				if normalizeTopicEmoji(sticker.Emoji) == emoji {
					icons[state] = sticker.CustomEmojiID
				}
			}
		}
		for state := range updater.emoji {
			if icons[state] == "" {
				return fmt.Errorf("telegram: no allowed topic icon for %s (%s)", state, updater.emoji[state])
			}
		}
		updater.icons = icons
	}
	success, err := api.EditForumTopic(ctx, &tgbot.EditForumTopicParams{
		ChatID: target.chatID, MessageThreadID: target.threadID, IconCustomEmojiID: updater.icons[status],
	})
	if err != nil && strings.Contains(strings.ToUpper(err.Error()), "TOPIC_NOT_MODIFIED") {
		return nil
	}
	if err != nil {
		return err
	}
	if !success {
		return fmt.Errorf("telegram: topic status edit returned false")
	}
	return nil
}

// The retry wrapper deliberately forwards these optional methods without
// sleeping. The status worker retries the latest state after Telegram's delay.
func (b *rateLimitRetryBot) GetForumTopicIconStickers(ctx context.Context) ([]*models.Sticker, error) {
	api, ok := b.telegramBot.(forumTopicStatusAPI)
	if !ok {
		return nil, core.ErrNotSupported
	}
	return api.GetForumTopicIconStickers(ctx)
}
func (b *rateLimitRetryBot) EditForumTopic(ctx context.Context, params *tgbot.EditForumTopicParams) (bool, error) {
	api, ok := b.telegramBot.(forumTopicStatusAPI)
	if !ok {
		return false, core.ErrNotSupported
	}
	return api.EditForumTopic(ctx, params)
}

func (p *Platform) ConversationStatusEnabled() bool { return p.topicStatus != nil }
