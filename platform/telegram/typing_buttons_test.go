package telegram

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"unicode/utf16"

	"github.com/chenhg5/cc-connect/core"
	tgbot "github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

type typingButtonTestBot struct {
	*typingTestBot
	messages   *buttonMessageLimitBot
	beforeSend func(*tgbot.SendMessageParams)
}

func (b *typingButtonTestBot) SendMessage(ctx context.Context, params *tgbot.SendMessageParams) (*models.Message, error) {
	b.beforeSend(params)
	return b.messages.SendMessage(ctx, params)
}

func TestTypingRestoredBetweenLiteralButtonChunks(t *testing.T) {
	for _, parseErrorFirst := range []bool{false, true} {
		t.Run(fmt.Sprintf("parse_error_first=%v", parseErrorFirst), func(t *testing.T) {
			p, typingBot, _ := newTypingTestPlatform(t)
			messages := &buttonMessageLimitBot{parseErrorFirst: parseErrorFirst}
			rc := replyContext{chatID: 42, threadID: 7}
			chunks := 0
			p.bot = &typingButtonTestBot{
				typingTestBot: typingBot,
				messages:      messages,
				beforeSend: func(params *tgbot.SendMessageParams) {
					if params.ParseMode != "" || len(utf16.Encode([]rune(params.Text))) > telegramMaxMessageLen {
						return
					}
					// A previous successful chunk clears Telegram's indicator.
					// Require its refresh before completing another chunk,
					// with no heartbeat ticks supplied by the test.
					if chunks > 0 {
						awaitTyping(t, typingBot, rc)
					}
					chunks++
				},
			}
			stop := p.StartTyping(context.Background(), rc)
			defer stop()
			awaitTyping(t, typingBot, rc)
			label := "```\n" + strings.Repeat("__init__.py & <tag> `literal` 🙂\n", 260) + "```"
			buttons := [][]core.ButtonOption{{{Text: label, Data: "choose"}}, {{Text: "Keep current", Data: "keep"}}}
			if err := p.SendWithButtons(context.Background(), rc, "Question?", buttons); err != nil {
				t.Fatal(err)
			}
			awaitTyping(t, typingBot, rc)
			var content strings.Builder
			keyboards := 0
			for _, sent := range messages.sends {
				if sent.ParseMode != "" || sent.ChatID != rc.chatID || sent.MessageThreadID != rc.threadID {
					t.Fatalf("lost literal fallback or topic routing: %+v", sent)
				}
				content.WriteString(sent.Text)
				if sent.ReplyMarkup != nil {
					keyboards++
					keyboard := sent.ReplyMarkup.(*models.InlineKeyboardMarkup).InlineKeyboard
					if keyboard[0][0].Text != "Option 1" || keyboard[0][0].CallbackData != "choose" || keyboard[1][0].Text != "Option 2" || keyboard[1][0].CallbackData != "keep" {
						t.Fatal("lost numbered options or callbacks")
					}
				}
			}
			want := "Question?\n\n**Option 1:** " + label + "\n\n**Option 2:** Keep current"
			if chunks < 2 || keyboards != 1 || content.String() != want {
				t.Fatalf("lost literal options: chunks=%d keyboards=%d text length=%d want=%d", chunks, keyboards, content.Len(), len(want))
			}
		})
	}
}
