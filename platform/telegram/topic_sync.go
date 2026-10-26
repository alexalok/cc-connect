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
	if err != nil || id <= 0 {
		return 0, 0, fmt.Errorf("telegram: topic_sync_chat_id must be a positive private chat/user ID")
	}
	// Requiring an explicit permitted user prevents a config typo from
	// publishing private session titles to a destination that cannot reply.
	if !core.AllowList(allowFrom, strconv.FormatInt(id, 10)) {
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
	p.mu.RLock()
	enabled := p.selfUser != nil && p.selfUser.HasTopicsEnabled
	p.mu.RUnlock()
	if !enabled {
		return "", fmt.Errorf("telegram: enable private chat topics in BotFather before using topic sync")
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
	topic, err := creator.CreateForumTopic(ctx, &tgbot.CreateForumTopicParams{ChatID: p.topicSyncChatID, Name: title})
	if err != nil {
		return "", fmt.Errorf("telegram: create session topic: %s", core.RedactToken(err.Error(), p.token))
	}
	if topic == nil || topic.MessageThreadID <= 0 {
		return "", fmt.Errorf("telegram: create session topic returned no topic ID")
	}
	// In a private chat the chat ID is also the user ID. Use the same key
	// builder as incoming updates, including share_session_in_channel mode.
	return p.buildSessionKey(p.topicSyncChatID, topic.MessageThreadID, p.topicSyncChatID), nil
}
