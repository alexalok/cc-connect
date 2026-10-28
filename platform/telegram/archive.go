package telegram

import (
	"context"
	"fmt"

	"github.com/chenhg5/cc-connect/core"
	tgbot "github.com/go-telegram/bot"
)

var _ core.TopicDeleter = (*Platform)(nil)

// DeleteTopic removes a forum or private-chat topic, including its messages.
// Topic-less chats and the General topic of a supergroup are left intact.
func (p *Platform) DeleteTopic(ctx context.Context, replyCtx any) (bool, error) {
	rc, ok := replyCtx.(replyContext)
	if !ok {
		return false, fmt.Errorf("telegram: delete topic: invalid reply context type %T", replyCtx)
	}
	if rc.threadID <= 0 || (rc.chatID < 0 && rc.threadID == 1) {
		return false, nil
	}
	bot, err := p.connectedBot("delete topic")
	if err != nil {
		return false, err
	}
	deleted, err := bot.DeleteForumTopic(ctx, &tgbot.DeleteForumTopicParams{
		ChatID:          rc.chatID,
		MessageThreadID: rc.threadID,
	})
	if err != nil {
		return false, fmt.Errorf("telegram: delete topic: %s", core.RedactToken(err.Error(), p.token))
	}
	if !deleted {
		return false, fmt.Errorf("telegram: delete topic: API returned false")
	}
	return true, nil
}
