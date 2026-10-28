package telegram

import (
	"context"
	"fmt"
	tgbot "github.com/go-telegram/bot"
	"net/http"
	"strings"
	"testing"
)

func TestRichMessagesRestoreTypingAfterSendAndEdit(t *testing.T) {
	rc := replyContext{chatID: 1, threadID: 7, messageID: 3}
	for _, method := range []string{"send", "reply", "preview", "edit", "buttons"} {
		t.Run(method, func(t *testing.T) {
			p, bot, _ := newTypingTestPlatform(t)
			p.richMessages = true
			p.httpClient = &http.Client{Transport: richTransport(func(*http.Request) (*http.Response, error) {
				return richResponse(200, `{"ok":true,"result":{"message_id":123}}`), nil
			})}
			stop := p.StartTyping(context.Background(), rc)
			defer stop()
			awaitTyping(t, bot, rc)
			var err error
			switch method {
			case "send":
				err = p.Send(context.Background(), rc, "**Progress**")
			case "reply":
				err = p.Reply(context.Background(), rc, "**Progress**")
			case "preview":
				_, err = p.SendPreviewStart(context.Background(), rc, "**Progress**")
			case "edit":
				err = p.UpdateMessage(context.Background(), &telegramPreviewHandle{chatID: 1, threadID: 7, messageID: 123}, "**Progress**")
			case "buttons":
				err = p.SendWithButtons(context.Background(), rc, "**Progress**", nil)
			}
			if err != nil {
				t.Fatal(err)
			}
			awaitTyping(t, bot, rc)
		})
	}
}

func TestTurnCardRestoresTypingAfterSendAndEdit(t *testing.T) {
	for _, regular := range []bool{false, true} {
		t.Run(fmt.Sprintf("regular=%v", regular), func(t *testing.T) {
			p, bot, _ := newTypingTestPlatform(t)
			p.richMessages = true
			p.httpClient = &http.Client{Transport: richTransport(func(*http.Request) (*http.Response, error) {
				return richResponse(200, `{"ok":true,"result":{"message_id":123}}`), nil
			})}
			rc := replyContext{chatID: 1, threadID: 7, messageID: 3}
			stop := p.StartTyping(context.Background(), rc)
			defer stop()
			awaitTyping(t, bot, rc)
			c := &turnCard{p: p, rc: rc, regular: regular}
			if err := c.Update(context.Background(), "Initial progress"); err != nil {
				t.Fatal(err)
			}
			awaitTyping(t, bot, rc)
			if err := c.Finalize(context.Background(), "Final answer"); err != nil {
				t.Fatal(err)
			}
			awaitTyping(t, bot, rc)
		})
	}
}

func TestRichMessagesRestoreTypingBetweenChunks(t *testing.T) {
	for _, fallback := range []bool{false, true} {
		t.Run(fmt.Sprintf("fallback=%v", fallback), func(t *testing.T) {
			p, bot, _ := newTypingTestPlatform(t)
			p.richMessages = true
			rc := replyContext{chatID: 42, threadID: 7}
			chunks := 0
			p.bot = &typingButtonTestBot{typingTestBot: bot, messages: &buttonMessageLimitBot{}, beforeSend: func(*tgbot.SendMessageParams) {
				if chunks > 0 {
					awaitTyping(t, bot, rc)
				}
				chunks++
			}}
			p.httpClient = &http.Client{Transport: richTransport(func(*http.Request) (*http.Response, error) {
				if fallback {
					return richResponse(400, `{"ok":false,"error_code":400,"description":"can't parse rich message"}`), nil
				}
				if chunks > 0 {
					awaitTyping(t, bot, rc)
				}
				chunks++
				return richResponse(200, fmt.Sprintf(`{"ok":true,"result":{"message_id":%d}}`, chunks)), nil
			})}
			stop := p.StartTyping(context.Background(), rc)
			defer stop()
			awaitTyping(t, bot, rc)
			length := telegramMaxRichMessageLen + 100
			if fallback {
				length = 5000
			}
			if err := p.Send(context.Background(), rc, strings.Repeat("x", length)); err != nil {
				t.Fatal(err)
			}
			awaitTyping(t, bot, rc)
			if chunks < 2 {
				t.Fatal("test did not exercise multiple chunks")
			}
		})
	}
}
