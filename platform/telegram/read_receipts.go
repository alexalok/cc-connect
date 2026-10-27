package telegram

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/chenhg5/cc-connect/core"
	tgbot "github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

func (p *Platform) readReceipt(chatID int64, messageID int) func() {
	return sync.OnceFunc(func() {
		// Do not hold up agent events on a slow Telegram request. The update's
		// context may already be canceled when a queued message is delivered.
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 130*time.Second)
			defer cancel()
			if err := p.markMessageReceived(ctx, chatID, messageID); err != nil {
				slog.Warn("telegram: read receipt failed", "chat_id", chatID,
					"message_id", messageID, "error", core.RedactToken(err.Error(), p.token))
			}
		}()
	})
}

func (p *Platform) markMessageReceived(ctx context.Context, chatID int64, messageID int) error {
	bot, err := p.connectedBot("read receipt")
	if err != nil {
		return err
	}
	params := &tgbot.SetMessageReactionParams{
		ChatID: chatID, MessageID: messageID,
		Reaction: []models.ReactionType{{
			Type:              models.ReactionTypeTypeEmoji,
			ReactionTypeEmoji: &models.ReactionTypeEmoji{Emoji: "👀"},
		}},
	}
	ok, err := retryOnRateLimit(ctx, "setMessageReaction", func() (bool, error) {
		return bot.SetMessageReaction(ctx, params)
	})
	if err != nil {
		return fmt.Errorf("telegram: set read receipt: %w", err)
	}
	if !ok {
		return fmt.Errorf("telegram: read receipt was not accepted")
	}
	return nil
}
