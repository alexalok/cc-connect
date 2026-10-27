package telegram

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/chenhg5/cc-connect/core"
	tgbot "github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

// Kept separate from telegramBot so platforms/tests that do not enable sync
// need not implement the optional topic API.
type forumTopicCreator interface {
	CreateForumTopic(context.Context, *tgbot.CreateForumTopicParams) (*models.ForumTopic, error)
}

type topicSyncChatInspector interface {
	GetChat(context.Context, *tgbot.GetChatParams) (*models.ChatFullInfo, error)
	GetChatMember(context.Context, *tgbot.GetChatMemberParams) (*models.ChatMember, error)
}

func parseTopicSyncOptions(opts map[string]any, allowFrom string) (int64, time.Duration, error) {
	interval := 10 * time.Second
	if value, exists := opts["topic_sync_interval_seconds"]; exists {
		seconds, err := strconv.ParseInt(fmt.Sprint(value), 10, 64)
		if err != nil || seconds < 1 || seconds > 3600 {
			return 0, 0, fmt.Errorf("telegram: topic_sync_interval_seconds must be an integer between 1 and 3600")
		}
		interval = time.Duration(seconds) * time.Second
	}
	value, exists := opts["topic_sync_chat_id"]
	if !exists {
		return 0, interval, nil
	}
	id, err := strconv.ParseInt(strings.TrimSpace(fmt.Sprint(value)), 10, 64)
	if err != nil || id == 0 {
		return 0, 0, fmt.Errorf("telegram: topic_sync_chat_id must be a non-zero private chat/user or supergroup ID")
	}
	// A private destination must be able to reply. Group IDs are not users:
	// allow_from still controls which humans can interact with group topics.
	if id > 0 && !core.AllowList(allowFrom, strconv.FormatInt(id, 10)) {
		return 0, 0, fmt.Errorf("telegram: topic_sync_chat_id must be permitted by allow_from")
	}
	return id, interval, nil
}

func (p *Platform) SessionTopicSyncConfig() (string, time.Duration) {
	if p.topicSyncChatID == 0 {
		return "", 0
	}
	return strconv.FormatInt(p.topicSyncChatID, 10), p.topicSyncInterval
}

func (p *Platform) CreateSessionTopic(ctx context.Context, title string) (string, error) {
	if p.topicSyncChatID == 0 {
		return "", fmt.Errorf("telegram: session topic sync is disabled")
	}
	bot, err := p.connectedBot("create session topic")
	if err != nil {
		return "", err
	}
	creator, ok := bot.(forumTopicCreator)
	if !ok {
		return "", core.ErrNotSupported
	}
	title = strings.Join(strings.Fields(title), " ")
	if title == "" {
		return "", fmt.Errorf("telegram: session topic title is empty")
	}
	if runes := []rune(title); len(runes) > 128 {
		title = string(runes[:128])
	}
	if err := p.validateTopicSyncChat(ctx, bot); err != nil {
		return "", err
	}
	topic, err := creator.CreateForumTopic(ctx, &tgbot.CreateForumTopicParams{ChatID: p.topicSyncChatID, Name: title})
	if err != nil {
		return "", fmt.Errorf("telegram: create session topic: %s", core.RedactToken(err.Error(), p.token))
	}
	if topic == nil || topic.MessageThreadID <= 0 {
		return "", fmt.Errorf("telegram: create session topic returned no topic ID")
	}
	// A DM's chat ID is its user ID. The supergroup destination uses a shared
	// topic key, matching incoming messages and callbacks from every allowed user.
	return p.buildSessionKey(p.topicSyncChatID, topic.MessageThreadID, p.topicSyncChatID), nil
}

// Query Telegram instead of requiring a separate destination-type setting.
// Recheck before each creation so changed forum settings or admin rights are
// reported without restarting; a group does not require DM topic mode.
func (p *Platform) validateTopicSyncChat(ctx context.Context, bot telegramBot) error {
	inspector, ok := bot.(topicSyncChatInspector)
	if !ok {
		return core.ErrNotSupported
	}
	chat, err := inspector.GetChat(ctx, &tgbot.GetChatParams{ChatID: p.topicSyncChatID})
	if err != nil {
		return fmt.Errorf("telegram: inspect topic sync destination: %s", core.RedactToken(err.Error(), p.token))
	}
	if chat == nil || chat.ID != p.topicSyncChatID {
		return fmt.Errorf("telegram: topic sync destination lookup returned no matching chat")
	}
	p.mu.RLock()
	me := p.selfUser
	p.mu.RUnlock()
	switch chat.Type {
	case models.ChatTypePrivate:
		if me == nil || !me.HasTopicsEnabled {
			return fmt.Errorf("telegram: enable private chat topics in BotFather before using topic sync")
		}
		return nil
	case models.ChatTypeSupergroup:
		if !chat.IsForum {
			return fmt.Errorf("telegram: enable Topics in the supergroup before using topic sync")
		}
		if me == nil {
			return fmt.Errorf("telegram: bot identity unavailable for topic sync permission check")
		}
		member, err := inspector.GetChatMember(ctx, &tgbot.GetChatMemberParams{ChatID: chat.ID, UserID: me.ID})
		if err != nil {
			return fmt.Errorf("telegram: inspect topic sync permissions: %s", core.RedactToken(err.Error(), p.token))
		}
		if member == nil || member.Type != models.ChatMemberTypeAdministrator || member.Administrator == nil || !member.Administrator.CanManageTopics {
			return fmt.Errorf("telegram: topic sync requires the bot to be a supergroup administrator with Manage Topics permission")
		}
		return nil
	default:
		return fmt.Errorf("telegram: topic sync destination must be a private chat or a supergroup with Topics enabled (got %s)", chat.Type)
	}
}
