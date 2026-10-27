package telegram

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/chenhg5/cc-connect/core"
	tgbot "github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

func TestSendWithButtons_LongLabelsBecomeNumberedOptions(t *testing.T) {
	longLabel := "Use rich formatting for all agent replies (Recommended)"
	tests := []struct {
		name     string
		labels   []string
		numbered bool
	}{
		{"screenshot", []string{longLabel, "Use rich formatting only for longer, structured replies"}, true},
		{"mixed lengths", []string{"Keep current", longLabel}, true},
		{"at limit", []string{strings.Repeat("a", 32), "No"}, false},
		{"over limit", []string{strings.Repeat("a", 33), "No"}, true},
		{"unicode at limit", []string{strings.Repeat("界", 32), "No"}, false},
		{"unicode over limit", []string{strings.Repeat("界", 33), "No"}, true},
		{"emoji at limit", []string{strings.Repeat("🙂", 32), "No"}, false},
		{"multiline", []string{"First line\nSecond line", "No"}, true},
		{"carriage return", []string{"First\rSecond", "No"}, true},
		{"no buttons", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bot := &richFallbackBot{}
			p := &Platform{bot: bot}
			rows := make([][]core.ButtonOption, len(tt.labels))
			for i, label := range tt.labels {
				rows[i] = []core.ButtonOption{{Text: label, Data: fmt.Sprintf("askq:request:0:%d", i+1)}}
			}
			original, _ := json.Marshal(rows)
			if err := p.SendWithButtons(context.Background(), replyContext{chatID: 42, threadID: 7}, "Choose an option", rows); err != nil {
				t.Fatal(err)
			}
			if len(bot.sends) != 1 {
				t.Fatalf("sent %d messages", len(bot.sends))
			}
			sent := bot.sends[0]
			if sent.ChatID != int64(42) || sent.MessageThreadID != 7 {
				t.Fatalf("lost routing: %+v", sent)
			}
			keyboard, ok := sent.ReplyMarkup.(*models.InlineKeyboardMarkup)
			if !ok || len(keyboard.InlineKeyboard) != len(rows) {
				t.Fatalf("lost button rows: %+v", sent.ReplyMarkup)
			}
			for i, label := range tt.labels {
				button := keyboard.InlineKeyboard[i][0]
				want := label
				if tt.numbered {
					want = fmt.Sprintf("Option %d", i+1)
					if !strings.Contains(sent.Text, want) || !strings.Contains(sent.Text, label) {
						t.Errorf("message lost full option %d: %q", i+1, sent.Text)
					}
				}
				if button.Text != want || button.CallbackData != rows[i][0].Data {
					t.Errorf("button = %+v, want label %q and callback %q", button, want, rows[i][0].Data)
				}
			}
			if !tt.numbered && sent.Text != "Choose an option" {
				t.Errorf("short labels changed content: %q", sent.Text)
			}
			after, _ := json.Marshal(rows)
			if string(after) != string(original) {
				t.Fatal("mutated caller's buttons")
			}
		})
	}
}

func TestSendWithButtons_LongLabelsRichAndFallback(t *testing.T) {
	for _, fallback := range []bool{false, true} {
		t.Run(fmt.Sprintf("fallback=%v", fallback), func(t *testing.T) {
			label := "Use rich formatting for all agent replies (Recommended)"
			var body richMessageParams
			p, bot := richTestPlatform(t, func(req *http.Request) (*http.Response, error) {
				if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
					t.Fatal(err)
				}
				if fallback {
					return richResponse(404, `{"ok":false,"error_code":404,"description":"Not Found"}`), nil
				}
				return richResponse(200, `{"ok":true,"result":{"message_id":123}}`), nil
			})
			rows := [][]core.ButtonOption{{{Text: "Keep current", Data: "keep"}, {Text: label, Data: "choose"}}}
			if err := p.SendWithButtons(context.Background(), replyContext{chatID: 42, threadID: 7}, "Question?", rows); err != nil {
				t.Fatal(err)
			}
			content, keyboard := body.RichMessage.Markdown, body.ReplyMarkup
			if fallback {
				if len(bot.sends) != 1 {
					t.Fatalf("fallback sends = %d", len(bot.sends))
				}
				content = bot.sends[0].Text
				keyboard = bot.sends[0].ReplyMarkup.(*models.InlineKeyboardMarkup)
			}
			if !strings.Contains(content, "Option 1") || !strings.Contains(content, "Keep current") || !strings.Contains(content, "Option 2") || !strings.Contains(content, label) {
				t.Fatalf("full options missing: %q", content)
			}
			want := [][]models.InlineKeyboardButton{{{Text: "Option 1", CallbackData: "keep"}, {Text: "Option 2", CallbackData: "choose"}}}
			if keyboard == nil || !reflect.DeepEqual(keyboard.InlineKeyboard, want) {
				t.Fatalf("keyboard = %+v, want %+v", keyboard, want)
			}
		})
	}
}

func TestSendWithButtons_NumberedOptionsUseConfiguredLanguage(t *testing.T) {
	for lang, prefix := range map[core.Language]string{
		core.LangEnglish: "Option", core.LangChinese: "选项",
		core.LangTraditionalChinese: "選項", core.LangJapanese: "選択肢", core.LangSpanish: "Opción",
	} {
		t.Run(string(lang), func(t *testing.T) {
			bot := &richFallbackBot{}
			p := &Platform{bot: bot}
			p.SetI18n(core.NewI18n(lang))
			rows := [][]core.ButtonOption{{{Text: strings.Repeat("a", 33), Data: "choose"}}}
			if err := p.SendWithButtons(context.Background(), replyContext{chatID: 42}, "", rows); err != nil {
				t.Fatal(err)
			}
			sent := bot.sends[0]
			want := prefix + " 1"
			keyboard := sent.ReplyMarkup.(*models.InlineKeyboardMarkup)
			if keyboard.InlineKeyboard[0][0].Text != want || !strings.Contains(sent.Text, want) {
				t.Fatalf("localized option missing: text=%q keyboard=%+v", sent.Text, keyboard)
			}
			if strings.HasPrefix(sent.Text, "\n") {
				t.Fatalf("empty content added a leading blank line: %q", sent.Text)
			}
		})
	}
}

func TestNew_ButtonTextLimit(t *testing.T) {
	for _, limit := range []any{nil, 16, int64(40), float64(40)} {
		t.Run(fmt.Sprint(limit), func(t *testing.T) {
			opts := map[string]any{"token": "test-token", "allow_from": "*"}
			want := 32
			if limit != nil {
				opts["button_text_limit"] = limit
				if limit == 16 {
					want = 16
				} else {
					want = 40
				}
			}
			platform, err := New(opts)
			if err != nil {
				t.Fatal(err)
			}
			p := platform.(*Platform)
			bot := &richFallbackBot{}
			p.bot = bot
			for _, length := range []int{want, want + 1} {
				label := strings.Repeat("a", length)
				if err := p.SendWithButtons(context.Background(), replyContext{chatID: 42}, "Question?", [][]core.ButtonOption{{{Text: label, Data: "choose"}}}); err != nil {
					t.Fatal(err)
				}
				sent := bot.sends[len(bot.sends)-1]
				button := sent.ReplyMarkup.(*models.InlineKeyboardMarkup).InlineKeyboard[0][0]
				expected := label
				if length > want {
					expected = "Option 1"
				}
				if button.Text != expected {
					t.Fatalf("label = %q, want %q", button.Text, expected)
				}
			}
		})
	}
	for _, limit := range []any{0, -1, 15, 16.5, true, "invalid"} {
		t.Run("invalid_"+fmt.Sprint(limit), func(t *testing.T) {
			_, err := New(map[string]any{"token": "test-token", "allow_from": "*", "button_text_limit": limit})
			if err == nil || !strings.Contains(err.Error(), "button_text_limit") {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

type buttonMessageLimitBot struct{ richFallbackBot }

func (b *buttonMessageLimitBot) SendMessage(ctx context.Context, params *tgbot.SendMessageParams) (*models.Message, error) {
	if utf8.RuneCountInString(params.Text) > telegramMaxMessageLen {
		return nil, fmt.Errorf("message is too long")
	}
	return b.richFallbackBot.SendMessage(ctx, params)
}

func TestSendWithButtons_LongOptionsSurviveMessageSplitting(t *testing.T) {
	for _, mode := range []string{"regular", "rich", "auto"} {
		t.Run(mode, func(t *testing.T) {
			bot := &buttonMessageLimitBot{}
			p, _ := richTestPlatform(t, func(*http.Request) (*http.Response, error) {
				return richResponse(404, `{"ok":false,"error_code":404,"description":"Not Found"}`), nil
			})
			p.richMessages, p.richMessagesAuto, p.bot = mode == "rich", mode == "auto", bot
			rows := [][]core.ButtonOption{{{Text: strings.Repeat("x", 6000), Data: "long"}}, {{Text: "Keep current", Data: "keep"}}}
			if err := p.SendWithButtons(context.Background(), replyContext{chatID: 42, threadID: 7}, "Question?", rows); err != nil {
				t.Fatal(err)
			}
			if len(bot.sends) < 2 {
				t.Fatalf("expected split messages, got %d", len(bot.sends))
			}
			var content strings.Builder
			keyboards := 0
			for _, sent := range bot.sends {
				if sent.ChatID != int64(42) || sent.MessageThreadID != 7 {
					t.Fatal("lost topic routing")
				}
				content.WriteString(sent.Text)
				if sent.ReplyMarkup != nil {
					keyboards++
					buttons := sent.ReplyMarkup.(*models.InlineKeyboardMarkup).InlineKeyboard
					if buttons[0][0].Text != "Option 1" || buttons[0][0].CallbackData != "long" || buttons[1][0].Text != "Option 2" || buttons[1][0].CallbackData != "keep" {
						t.Fatalf("incorrect split-message buttons: %+v", buttons)
					}
				}
			}
			if keyboards != 1 || strings.Count(content.String(), "x") != 6000 || !strings.Contains(content.String(), "Keep current") {
				t.Fatalf("lost or duplicated options: keyboards=%d option characters=%d", keyboards, strings.Count(content.String(), "x"))
			}
		})
	}
}
