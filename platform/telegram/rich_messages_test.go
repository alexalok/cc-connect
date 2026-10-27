package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/chenhg5/cc-connect/core"
	tgbot "github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

type richTransport func(*http.Request) (*http.Response, error)

func (f richTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type richFallbackBot struct {
	stubTelegramBot
	sends []*tgbot.SendMessageParams
	edits []*tgbot.EditMessageTextParams
}

func (b *richFallbackBot) SendMessage(_ context.Context, params *tgbot.SendMessageParams) (*models.Message, error) {
	copy := *params
	b.sends = append(b.sends, &copy)
	return &models.Message{ID: 88}, nil
}

func (b *richFallbackBot) EditMessageText(_ context.Context, params *tgbot.EditMessageTextParams) (*models.Message, error) {
	copy := *params
	b.edits = append(b.edits, &copy)
	return &models.Message{ID: 88}, nil
}

func richResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}
}

func richTestPlatform(t *testing.T, transport richTransport) (*Platform, *richFallbackBot) {
	t.Helper()
	platform, err := New(map[string]any{"token": "test-secret", "allow_from": "*", "rich_messages": true})
	if err != nil {
		t.Fatal(err)
	}
	p := platform.(*Platform)
	bot := &richFallbackBot{}
	p.bot = bot
	p.httpClient = &http.Client{Transport: transport}
	return p, bot
}

func TestRichMessagesPreserveMarkdownRoutingAndButtons(t *testing.T) {
	content := "| Item | State |\n| --- | --- |\n| **Build** | Done |\n\n- [x] Tested\n\n<details><summary>Logs</summary>All good</details>"
	rc := replyContext{chatID: 42, threadID: 7, messageID: 19}
	for _, method := range []string{"send", "reply", "buttons", "preview"} {
		t.Run(method, func(t *testing.T) {
			calls := 0
			p, bot := richTestPlatform(t, func(req *http.Request) (*http.Response, error) {
				calls++
				if req.URL.Path != "/bottest-secret/sendRichMessage" || req.Method != http.MethodPost || req.Header.Get("Content-Type") != "application/json" {
					t.Fatal("incorrect rich message API request")
				}
				var body richMessageParams
				if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
					t.Fatal(err)
				}
				if body.ChatID != 42 || body.MessageThreadID != 7 || body.RichMessage.Markdown != content {
					t.Fatalf("lost routing or formatting: %+v", body)
				}
				if (body.ReplyParameters != nil) != (method == "reply") || (body.ReplyParameters != nil && body.ReplyParameters.MessageID != 19) {
					t.Fatalf("unexpected reply reference: %+v", body.ReplyParameters)
				}
				if method == "buttons" && (body.ReplyMarkup == nil || body.ReplyMarkup.InlineKeyboard[0][0].CallbackData != "approve") {
					t.Fatal("lost callback button")
				}
				return richResponse(200, `{"ok":true,"result":{"message_id":123}}`), nil
			})
			var err error
			switch method {
			case "send":
				err = p.Send(context.Background(), rc, content)
			case "reply":
				err = p.Reply(context.Background(), rc, content)
			case "buttons":
				err = p.SendWithButtons(context.Background(), rc, content, [][]core.ButtonOption{{{Text: "Approve", Data: "approve"}}})
			case "preview":
				var handle any
				handle, err = p.SendPreviewStart(context.Background(), rc, content)
				if err == nil && *handle.(*telegramPreviewHandle) != (telegramPreviewHandle{chatID: 42, threadID: 7, messageID: 123}) {
					t.Fatalf("incorrect preview handle: %+v", handle)
				}
			}
			if err != nil || calls != 1 || len(bot.sends) != 0 {
				t.Fatalf("err=%v rich sends=%d regular sends=%d", err, calls, len(bot.sends))
			}
		})
	}
}

func TestRichMessageEditsUseRichContent(t *testing.T) {
	p, bot := richTestPlatform(t, func(req *http.Request) (*http.Response, error) {
		var body map[string]json.RawMessage
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if req.URL.Path != "/bottest-secret/editMessageText" || string(body["message_id"]) != "123" || string(body["chat_id"]) != "42" {
			t.Fatal("incorrect rich edit target")
		}
		if body["rich_message"] == nil || body["text"] != nil || body["parse_mode"] != nil || body["message_thread_id"] != nil {
			t.Fatalf("incorrect rich edit fields: %v", body)
		}
		return richResponse(200, `{"ok":true,"result":{"message_id":123}}`), nil
	})
	if err := p.UpdateMessage(context.Background(), &telegramPreviewHandle{chatID: 42, messageID: 123}, "**Updated**"); err != nil {
		t.Fatal(err)
	}
	if len(bot.edits) != 0 {
		t.Fatal("unexpected regular edit")
	}
}

func TestRichMessagesFallbackOnlyOnExplicitRejection(t *testing.T) {
	for _, tc := range []struct {
		name     string
		status   int
		response string
		fallback bool
	}{
		{"parse", 400, `{"ok":false,"error_code":400,"description":"Bad Request: can't parse rich message"}`, true},
		{"old_api", 404, `{"ok":false,"error_code":404,"description":"Not Found"}`, true},
		{"forbidden", 403, `{"ok":false,"error_code":403,"description":"Forbidden"}`, false},
		{"bad_topic", 400, `{"ok":false,"error_code":400,"description":"message thread not found"}`, false},
		{"server", 500, `{"ok":false,"error_code":500,"description":"Internal Server Error"}`, false},
		{"invalid_json", 200, `{`, false},
		{"missing_id", 200, `{"ok":true,"result":{}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, bot := richTestPlatform(t, func(*http.Request) (*http.Response, error) {
				return richResponse(tc.status, tc.response), nil
			})
			err := p.Reply(context.Background(), replyContext{chatID: 42, threadID: 7, messageID: 19}, "**Hello**")
			if (err == nil) != tc.fallback || (len(bot.sends) == 1) != tc.fallback {
				t.Fatalf("err=%v regular sends=%d", err, len(bot.sends))
			}
			if tc.fallback {
				got := bot.sends[0]
				if got.Text != "<b>Hello</b>" || got.ParseMode != models.ParseModeHTML || got.MessageThreadID != 7 || got.ReplyParameters.MessageID != 19 || got.ReplyMarkup != nil {
					t.Fatalf("incorrect fallback: %+v", got)
				}
			}
		})
	}
}

func TestRichMessagePreviewAndEditFallback(t *testing.T) {
	p, bot := richTestPlatform(t, func(*http.Request) (*http.Response, error) {
		return richResponse(400, `{"ok":false,"error_code":400,"description":"can't parse rich message"}`), nil
	})
	handle, err := p.SendPreviewStart(context.Background(), replyContext{chatID: 42, threadID: 7}, "**Preview**")
	if err != nil {
		t.Fatal(err)
	}
	if err := p.UpdateMessage(context.Background(), handle, "**Final**"); err != nil {
		t.Fatal(err)
	}
	if len(bot.sends) != 1 || len(bot.edits) != 1 || bot.edits[0].MessageID != 88 || bot.edits[0].Text != "<b>Final</b>" {
		t.Fatalf("fallback did not retain preview identity: sends=%v edits=%v", bot.sends, bot.edits)
	}
}

func TestRichMessageUnchangedEditSucceeds(t *testing.T) {
	p, bot := richTestPlatform(t, func(*http.Request) (*http.Response, error) {
		return richResponse(400, `{"ok":false,"error_code":400,"description":"Bad Request: message is not modified"}`), nil
	})
	if err := p.UpdateMessage(context.Background(), &telegramPreviewHandle{chatID: 42, messageID: 123}, "Same"); err != nil || len(bot.edits) != 0 {
		t.Fatalf("err=%v regular edits=%d", err, len(bot.edits))
	}
}

func TestRichMessageRateLimitRetries(t *testing.T) {
	original := sleepCtx
	t.Cleanup(func() { sleepCtx = original })
	waits := 0
	sleepCtx = func(_ context.Context, duration time.Duration) error {
		waits++
		if duration != 2*time.Second {
			t.Fatalf("unexpected retry delay %s", duration)
		}
		return nil
	}
	calls := 0
	p, bot := richTestPlatform(t, func(*http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			return richResponse(429, `{"ok":false,"error_code":429,"description":"Too Many Requests","parameters":{"retry_after":2}}`), nil
		}
		return richResponse(200, `{"ok":true,"result":{"message_id":123}}`), nil
	})
	if err := p.Send(context.Background(), replyContext{chatID: 42}, "Hello"); err != nil || calls != 2 || waits != 1 || len(bot.sends) != 0 {
		t.Fatalf("err=%v requests=%d waits=%d regular sends=%d", err, calls, waits, len(bot.sends))
	}
}

func TestRichMessageTransportErrorRedactsTokenAndAvoidsDuplicate(t *testing.T) {
	p, bot := richTestPlatform(t, func(*http.Request) (*http.Response, error) {
		return nil, errors.New("network failed")
	})
	err := p.Send(context.Background(), replyContext{chatID: 42}, "Hello")
	if err == nil || strings.Contains(err.Error(), "test-secret") || len(bot.sends) != 0 {
		t.Fatalf("unsafe failure: err=%v regular sends=%d", err, len(bot.sends))
	}
}

func TestRichMessagesSplitLongUnicodeAndPreserveFirstReply(t *testing.T) {
	var chunks []string
	p, bot := richTestPlatform(t, func(req *http.Request) (*http.Response, error) {
		var body richMessageParams
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body.MessageThreadID != 7 || (body.ReplyParameters != nil) != (len(chunks) == 0) {
			t.Fatal("lost thread or repeated reply reference")
		}
		chunks = append(chunks, body.RichMessage.Markdown)
		return richResponse(200, fmt.Sprintf(`{"ok":true,"result":{"message_id":%d}}`, len(chunks))), nil
	})
	content := strings.Repeat("😀", telegramMaxRichMessageLen+100)
	if err := p.Reply(context.Background(), replyContext{chatID: 42, threadID: 7, messageID: 19}, content); err != nil {
		t.Fatal(err)
	}
	if len(chunks) < 2 || strings.Join(chunks, "") != content || len(bot.sends) != 0 {
		t.Fatal("long reply lost content or used regular messages")
	}
	for _, chunk := range chunks {
		if !utf8.ValidString(chunk) || utf8.RuneCountInString(chunk) > telegramMaxRichMessageLen {
			t.Fatal("chunk exceeds rich message limit or breaks Unicode")
		}
	}
}

func TestRichMessageLongFallbackRetainsKeyboardOnce(t *testing.T) {
	p, bot := richTestPlatform(t, func(*http.Request) (*http.Response, error) {
		return richResponse(404, `{"ok":false,"error_code":404,"description":"Not Found"}`), nil
	})
	content := strings.Repeat("x", 10000)
	if err := p.SendWithButtons(context.Background(), replyContext{chatID: 42, threadID: 7}, content, [][]core.ButtonOption{{{Text: "Yes", Data: "yes"}}}); err != nil {
		t.Fatal(err)
	}
	var sent strings.Builder
	for i, params := range bot.sends {
		if params.MessageThreadID != 7 || (params.ReplyMarkup != nil) != (i == 0) || len(params.Text) > telegramMaxMessageLen {
			t.Fatal("invalid fallback chunk routing, length, or keyboard")
		}
		sent.WriteString(params.Text)
	}
	if sent.String() != content {
		t.Fatal("fallback lost long message content")
	}
}

func TestRichMessagesOptIn(t *testing.T) {
	p, bot := richTestPlatform(t, func(*http.Request) (*http.Response, error) {
		t.Fatal("rich endpoint called with rich messages disabled")
		return nil, nil
	})
	p.richMessages = false
	if err := p.Send(context.Background(), replyContext{chatID: 42}, "**Hello**"); err != nil || len(bot.sends) != 1 {
		t.Fatalf("err=%v regular sends=%d", err, len(bot.sends))
	}
	platform, err := New(map[string]any{"token": "test", "allow_from": "*"})
	if err != nil || platform.(*Platform).richMessages {
		t.Fatal("rich messages should default to disabled")
	}
}
