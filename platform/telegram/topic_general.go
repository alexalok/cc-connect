package telegram

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/chenhg5/cc-connect/core"
	tgbot "github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

type topicMessageCopier interface {
	CopyMessage(context.Context, *tgbot.CopyMessageParams) (*models.MessageID, error)
}

const (
	generalTopicRouteLimit = 256
	generalTopicRouteTTL   = 10 * time.Minute
)

// A message ID identifies a retry; an album ID groups its separate updates.
// The routing cache belongs to the one configured destination supergroup.
type generalTopicKey struct {
	messageID int
	userID    int64
	albumID   string
}

type generalTopicRoute struct {
	threadID   int
	dispatched bool
	seenAt     time.Time
}

type generalTopicRouting struct {
	mu     sync.Mutex
	routes map[generalTopicKey]*generalTopicRoute
}

func parseGeneralTopicOption(opts map[string]any, chatID int64) (bool, error) {
	value, exists := opts["general_topic_auto_create"]
	if !exists {
		return false, nil
	}
	enabled, ok := value.(bool)
	if !ok {
		return false, fmt.Errorf("telegram: general_topic_auto_create must be a boolean")
	}
	if enabled && chatID >= 0 {
		return false, fmt.Errorf("telegram: general_topic_auto_create requires a supergroup topic_sync_chat_id")
	}
	return enabled, nil
}

// Run after access checks and successful attachment downloads, before the
// engine can create a General session. Keep the original update unchanged:
// receipts belong to the user's original message, replies to its topic copy.
func (p *Platform) routeGeneralTopicMessage(ctx context.Context, msg *core.Message, tg *models.Message) bool {
	if !p.autoGeneralTopics || tg.Chat.ID != p.topicSyncChatID ||
		tg.Chat.Type != models.ChatTypeSupergroup || !tg.Chat.IsForum || tg.MessageThreadID > 1 {
		return true
	}
	if tg.From == nil || tg.From.IsBot || tg.ID <= 0 {
		return false
	}
	// Management commands remain usable in General without creating empty
	// conversations. Text commands also work when Telegram omits entities.
	if isCommand(tg) || strings.HasPrefix(strings.TrimSpace(msg.Content), "/") {
		return true
	}

	rc, duplicate, err := p.generalTopicReplyContext(ctx, msg, tg)
	if err != nil {
		slog.Warn("telegram: route General message to topic failed", "error", core.RedactToken(err.Error(), p.token))
		original := replyContext{chatID: tg.Chat.ID, messageID: tg.ID}
		if err := p.Reply(ctx, original, p.noticeI18n().T(core.MsgGeneralTopicFailed)); err != nil {
			slog.Warn("telegram: send topic creation notice failed", "error", core.RedactToken(err.Error(), p.token))
		}
		return false
	}
	if duplicate {
		return false
	}
	msg.SessionKey = p.buildSessionKey(rc.chatID, rc.threadID, tg.From.ID)
	msg.ChannelKey = buildChannelKey(rc.chatID, rc.threadID)
	msg.ReplyCtx = rc
	return true
}

func (p *Platform) generalTopicReplyContext(ctx context.Context, msg *core.Message, tg *models.Message) (replyContext, bool, error) {
	r := &p.generalTopicRouting
	// Serialize the create/copy boundary, including album members received
	// concurrently. Never publish a destination before its opener is copied.
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	r.expire(now)
	messageKey := generalTopicKey{messageID: tg.ID}
	route := r.routes[messageKey]
	if route != nil && route.dispatched {
		return replyContext{}, true, nil
	}

	bot, err := p.connectedBot("copy topic opener")
	if err != nil {
		return replyContext{}, false, err
	}
	copier, ok := bot.(topicMessageCopier)
	if !ok {
		return replyContext{}, false, core.ErrNotSupported
	}
	if route == nil {
		route = &generalTopicRoute{seenAt: now}
		albumKey := generalTopicKey{userID: tg.From.ID, albumID: tg.MediaGroupID}
		if tg.MediaGroupID != "" {
			if album := r.routes[albumKey]; album != nil {
				route.threadID = album.threadID
			}
		}
		if route.threadID == 0 {
			threadID, err := p.createGeneralTopic(ctx, msg, tg)
			if err != nil {
				return replyContext{}, false, err
			}
			route.threadID = threadID
		}
		// Keep successful creation even if copying fails, so a retry of the
		// same update does not create another empty topic.
		r.remember(messageKey, route)
		if tg.MediaGroupID != "" {
			r.remember(albumKey, &generalTopicRoute{threadID: route.threadID, seenAt: now})
		}
	}
	copied, err := copier.CopyMessage(ctx, &tgbot.CopyMessageParams{
		ChatID: tg.Chat.ID, FromChatID: tg.Chat.ID, MessageID: tg.ID,
		MessageThreadID: route.threadID,
	})
	if err != nil {
		return replyContext{}, false, fmt.Errorf("telegram: copy topic opener: %w", err)
	}
	if copied == nil || copied.ID <= 0 {
		return replyContext{}, false, fmt.Errorf("telegram: copy topic opener returned no message ID")
	}
	route.dispatched, route.seenAt = true, now
	return replyContext{chatID: tg.Chat.ID, threadID: route.threadID, messageID: copied.ID}, false, nil
}

func (p *Platform) createGeneralTopic(ctx context.Context, msg *core.Message, tg *models.Message) (int, error) {
	title := strings.TrimSpace(msg.Content)
	if title == "" && tg.Document != nil {
		title = tg.Document.FileName
	}
	if strings.TrimSpace(title) == "" {
		title = p.noticeI18n().T(core.MsgUntitled)
	}
	key, err := p.CreateSessionTopic(ctx, title)
	if err != nil {
		return 0, err
	}
	reply, err := p.ReconstructReplyCtx(key)
	if err != nil {
		return 0, err
	}
	threadID := reply.(replyContext).threadID
	if threadID <= 1 {
		return 0, fmt.Errorf("telegram: new topic returned a General topic ID")
	}
	return threadID, nil
}

func (r *generalTopicRouting) expire(now time.Time) {
	for key, route := range r.routes {
		if now.Sub(route.seenAt) > generalTopicRouteTTL {
			delete(r.routes, key)
		}
	}
}

func (r *generalTopicRouting) remember(key generalTopicKey, route *generalTopicRoute) {
	if r.routes == nil {
		r.routes = make(map[generalTopicKey]*generalTopicRoute)
	}
	if _, exists := r.routes[key]; !exists && len(r.routes) >= generalTopicRouteLimit {
		var oldestKey generalTopicKey
		var oldest time.Time
		for candidate, record := range r.routes {
			if oldest.IsZero() || record.seenAt.Before(oldest) {
				oldestKey, oldest = candidate, record.seenAt
			}
		}
		delete(r.routes, oldestKey)
	}
	r.routes[key] = route
}
