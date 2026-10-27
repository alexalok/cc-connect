package telegram

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/chenhg5/cc-connect/core"
	tgbot "github.com/go-telegram/bot"
)

type forumTopicDeleter interface {
	DeleteForumTopic(context.Context, *tgbot.DeleteForumTopicParams) (bool, error)
}

// DeleteSessionTopic is scoped to the explicitly configured sync destination.
// It also accepts native topics first created by a message through Telegram.
func (p *Platform) DeleteSessionTopic(ctx context.Context, key string) (bool, error) {
	if p.topicSyncChatID == 0 || !strings.HasPrefix(key, p.Name()+":") {
		return false, nil
	}
	reply, err := p.ReconstructReplyCtx(key)
	if err != nil {
		return false, err
	}
	rc := reply.(replyContext)
	if rc.chatID != p.topicSyncChatID || rc.threadID <= 0 {
		return false, nil
	}
	bot, err := p.connectedBot("delete synced topic")
	if err != nil {
		return false, err
	}
	deleter, ok := bot.(forumTopicDeleter)
	if !ok {
		return false, core.ErrNotSupported
	}
	deleted, err := deleter.DeleteForumTopic(ctx, &tgbot.DeleteForumTopicParams{
		ChatID: rc.chatID, MessageThreadID: rc.threadID,
	})
	// A previous deletion may have succeeded before its acknowledgment or
	// local journal was saved. Only the explicit missing-topic error is success.
	if errors.Is(err, tgbot.ErrorBadRequest) && strings.Contains(err.Error(), "TOPIC_ID_INVALID") {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("telegram: delete synced topic: %s", core.RedactToken(err.Error(), p.token))
	}
	if !deleted {
		return false, fmt.Errorf("telegram: delete synced topic: API returned false")
	}
	return true, nil
}

func (b *startupBot) DeleteForumTopic(ctx context.Context, params *tgbot.DeleteForumTopicParams) (bool, error) {
	deleter, ok := b.telegramBot.(forumTopicDeleter)
	if !ok {
		return false, core.ErrNotSupported
	}
	return deleter.DeleteForumTopic(ctx, params)
}

func (b *rateLimitRetryBot) DeleteForumTopic(ctx context.Context, params *tgbot.DeleteForumTopicParams) (bool, error) {
	deleter, ok := b.telegramBot.(forumTopicDeleter)
	if !ok {
		return false, core.ErrNotSupported
	}
	return retryOnRateLimit(ctx, "deleteForumTopic", func() (bool, error) {
		return deleter.DeleteForumTopic(ctx, params)
	})
}
