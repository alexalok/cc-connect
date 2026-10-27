package telegram

import (
	"context"
	"net/http"
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
