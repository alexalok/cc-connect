package telegram

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"strings"
	"testing"

	"github.com/chenhg5/cc-connect/core"
	"github.com/go-telegram/bot/models"
)

func TestRichButtonsPreserveLiteralNumberedChoicesAndFallback(t *testing.T) {
	labels := []string{
		"Rename __init__.py without changing package behavior",
		"Keep **literal** stars, `backticks`, and ~~tildes~~",
		"Keep [label](https://example.com) and [[wiki|label]]",
		"Keep <config> & quoted \"values\" unchanged",
		"Keep line breaks\n# Heading\n- list\n> quote\n```\n__code__\n```",
	}
	for _, reject := range []bool{false, true} {
		t.Run(fmt.Sprintf("reject=%v", reject), func(t *testing.T) {
			requests := 0
			prompt := "## Choose carefully\n\n| Item | Result |\n| --- | --- |\n| Tests | Pass |"
			p, bot := richTestPlatform(t, func(req *http.Request) (*http.Response, error) {
				requests++
				var body richMessageParams
				if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
					t.Fatal(err)
				}
				if body.ChatID != 42 || body.MessageThreadID != 7 || !strings.HasPrefix(body.RichMessage.Markdown, prompt+"\n\n") {
					t.Fatalf("rich prompt/routing changed: %+v", body)
				}
				for i, label := range labels {
					// Block HTML keeps literal Markdown punctuation and line breaks out of
					// Telegram's Markdown parser while the original prompt stays rich.
					literal := strings.NewReplacer("\r\n", "<br>", "\n", "<br>", "\r", "<br>").Replace(html.EscapeString(label))
					want := fmt.Sprintf("<p><b>Option %d:</b> %s</p>", i+1, literal)
					if !strings.Contains(body.RichMessage.Markdown, want) {
						t.Fatalf("literal choice missing: want %q in %q", want, body.RichMessage.Markdown)
					}
					if body.ReplyMarkup == nil || body.ReplyMarkup.InlineKeyboard[i][0].Text != fmt.Sprintf("Option %d", i+1) || body.ReplyMarkup.InlineKeyboard[i][0].CallbackData != fmt.Sprintf("choice:%d", i) {
						t.Fatal("rich choice callbacks changed")
					}
				}
				if reject {
					return richResponse(400, `{"ok":false,"error_code":400,"description":"can't parse rich message"}`), nil
				}
				return richResponse(200, `{"ok":true,"result":{"message_id":88}}`), nil
			})
			p.richMessagesAuto = true
			var rows [][]core.ButtonOption
			for i, label := range labels {
				rows = append(rows, []core.ButtonOption{{Text: label, Data: fmt.Sprintf("choice:%d", i)}})
			}
			if err := p.SendWithButtons(context.Background(), replyContext{chatID: 42, threadID: 7}, prompt, rows); err != nil {
				t.Fatal(err)
			}
			if requests != 1 {
				t.Fatalf("requests=%d", requests)
			}
			if !reject {
				if len(bot.sends) != 0 {
					t.Fatal("duplicate regular message")
				}
				return
			}
			if len(bot.sends) != 1 {
				t.Fatalf("fallback sends=%d", len(bot.sends))
			}
			sent := bot.sends[0]
			if sent.ParseMode != models.ParseModeHTML {
				t.Fatal("lost existing HTML fallback")
			}
			for i, label := range labels {
				if !strings.Contains(sent.Text, html.EscapeString(label)) {
					t.Fatalf("literal choice missing in fallback: %q", sent.Text)
				}
				button := sent.ReplyMarkup.(*models.InlineKeyboardMarkup).InlineKeyboard[i][0]
				if button.CallbackData != fmt.Sprintf("choice:%d", i) {
					t.Fatal("fallback callback changed")
				}
			}
		})
	}
}

func TestRichButtonsOversizedChoicesKeepExactPlainText(t *testing.T) {
	label := "```\n" + strings.Repeat("🙂 &<tag> __init__.py `literal`\n", 2000) + "```"
	p, bot := richTestPlatform(t, func(*http.Request) (*http.Response, error) {
		t.Fatal("oversized literal document must use exact plain-text splitting")
		return nil, nil
	})
	rows := [][]core.ButtonOption{{{Text: label, Data: "choose"}}, {{Text: "Keep current", Data: "keep"}}}
	if err := p.SendWithButtons(context.Background(), replyContext{chatID: 42, threadID: 7}, "## Choose", rows); err != nil {
		t.Fatal(err)
	}
	var all strings.Builder
	keyboards := 0
	for _, sent := range bot.sends {
		if sent.ParseMode != "" || sent.ChatID != int64(42) || sent.MessageThreadID != 7 {
			t.Fatalf("invalid split message: %+v", sent)
		}
		all.WriteString(sent.Text)
		if sent.ReplyMarkup != nil {
			keyboards++
			buttons := sent.ReplyMarkup.(*models.InlineKeyboardMarkup).InlineKeyboard
			if buttons[0][0].Text != "Option 1" || buttons[0][0].CallbackData != "choose" || buttons[1][0].Text != "Option 2" || buttons[1][0].CallbackData != "keep" {
				t.Fatal("lost split choice mapping")
			}
		}
	}
	want := "## Choose\n\n**Option 1:** " + label + "\n\n**Option 2:** Keep current"
	if all.String() != want || keyboards != 1 || len(bot.sends) < 2 {
		t.Fatal("oversized choices lost literal text or duplicated keyboard")
	}
}

func TestRichButtonsRejectedFallbackPreservesExactPlainChoices(t *testing.T) {
	for _, repetitions := range []int{1, 300} {
		t.Run(fmt.Sprint(repetitions), func(t *testing.T) {
			calls := 0
			p, _ := richTestPlatform(t, func(*http.Request) (*http.Response, error) {
				calls++
				return richResponse(400, `{"ok":false,"error_code":400,"description":"can't parse rich message"}`), nil
			})
			bot := &buttonMessageLimitBot{parseErrorFirst: true}
			p.bot = bot
			label := strings.Repeat("__init__.py & <tag> `literal` 🙂\n", repetitions)
			rows := [][]core.ButtonOption{{{Text: label, Data: "choose"}}, {{Text: "Keep current", Data: "keep"}}}
			if err := p.SendWithButtons(context.Background(), replyContext{chatID: 42, threadID: 7}, "## Choose", rows); err != nil {
				t.Fatal(err)
			}
			var text strings.Builder
			keyboards := 0
			for _, sent := range bot.sends {
				if sent.ParseMode != "" {
					t.Fatal("HTML rejection did not fall back to plain text")
				}
				text.WriteString(sent.Text)
				if sent.ReplyMarkup != nil {
					keyboards++
					buttons := sent.ReplyMarkup.(*models.InlineKeyboardMarkup).InlineKeyboard
					if buttons[0][0].Text != "Option 1" || buttons[0][0].CallbackData != "choose" || buttons[1][0].CallbackData != "keep" {
						t.Fatal("fallback changed callbacks")
					}
				}
			}
			if calls != 1 || keyboards != 1 || text.String() != "## Choose\n\n**Option 1:** "+label+"\n\n**Option 2:** Keep current" {
				t.Fatal("rich rejection changed literal choices or duplicated keyboard")
			}
		})
	}
}
