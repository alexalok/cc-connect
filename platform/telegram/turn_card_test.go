package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/chenhg5/cc-connect/core"
	tgbot "github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

func TestTurnCardProgressAndAnswerShareMessage(t *testing.T) {
	var requests []richMessageParams
	p, bot := richTestPlatform(t, func(req *http.Request) (*http.Response, error) {
		var body richMessageParams
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		requests = append(requests, body)
		return richResponse(200, `{"ok":true,"result":{"message_id":88}}`), nil
	})
	p.richMessagesAuto, p.collapseProgress = true, true
	card, err := p.CreateStreamingCard(context.Background(), replyContext{chatID: 42, threadID: 7})
	if err != nil {
		t.Fatal(err)
	}
	c := card.(*turnCard)
	if err := c.AppendProgress(context.Background(), core.ProgressCardEntry{Kind: core.ProgressEntryThinking, Text: "Checking the bridge."}); err != nil {
		t.Fatal(err)
	}
	// A short first update uses a regular message, then promotes that same ID.
	if len(bot.sends) != 1 || len(requests) != 0 {
		t.Fatal("short progress must use a single regular message")
	}
	long := "Earlier summary: " + strings.Repeat("kept ", 1300) + "end of summary"
	if err := c.AppendProgress(context.Background(), core.ProgressCardEntry{Kind: core.ProgressEntryThinking, Text: long}); err != nil {
		t.Fatal(err)
	}
	answer := "| Check | Result |\n| --- | --- |\n| Tests | Pass |\n\n```go\nfmt.Println(\"done\")\n```"
	if err := c.Finalize(context.Background(), answer); err != nil {
		t.Fatal(err)
	}
	if len(bot.sends) != 1 || len(requests) != 1 {
		t.Fatalf("regular sends=%d rich requests=%d", len(bot.sends), len(requests))
	}
	body := requests[0]
	if body.ChatID != 42 || body.MessageID != 88 || body.MessageThreadID != 7 {
		t.Fatalf("lost routing: %+v", body)
	}
	text := body.RichMessage.Markdown
	if !strings.Contains(text, "<details><summary>Read more</summary>") || !strings.Contains(text, long) || !strings.Contains(text, "Checking the bridge.") {
		t.Fatal("missing collapsed progress or earlier text")
	}
	if !strings.HasSuffix(text, "</details>\n\n---\n\n"+answer) {
		t.Fatalf("answer must remain formatted and outside details: %s", text)
	}
}

func TestTurnCardShortProgressStaysExpanded(t *testing.T) {
	p, bot := richTestPlatform(t, func(*http.Request) (*http.Response, error) { t.Fatal("short turn used rich API"); return nil, nil })
	p.richMessagesAuto = true
	c := &turnCard{p: p, rc: replyContext{chatID: 42}}
	if err := c.AppendProgress(context.Background(), core.ProgressCardEntry{Kind: core.ProgressEntryThinking, Text: "Checked <file>."}); err != nil {
		t.Fatal(err)
	}
	if err := c.Finalize(context.Background(), "Done."); err != nil {
		t.Fatal(err)
	}
	if len(bot.sends) != 1 || len(bot.edits) != 1 {
		t.Fatal("short summary and answer must share one message")
	}
	if text := bot.edits[0].Text; strings.Contains(text, "details") || !strings.Contains(text, "Checked &lt;file&gt;.") || !strings.Contains(text, "Done.") {
		t.Fatalf("unexpected short turn: %s", text)
	}
}

func TestTurnCardCollapseThresholdAndFenceIsolation(t *testing.T) {
	p := &Platform{collapseProgressAfter: 600}
	for _, tc := range []struct {
		summary string
		folded  bool
	}{
		{strings.Repeat("界", 599), false},
		{strings.Repeat("界", 600), true},
		{strings.Repeat("line\n", 7) + "last", true},
		{"tiny", false},
	} {
		c := &turnCard{p: p, entries: []core.ProgressCardEntry{{Kind: core.ProgressEntryThinking, Text: tc.summary}}, answer: "Answer"}
		if got := strings.Contains(c.renderPages()[0], "<details>"); got != tc.folded {
			t.Fatalf("collapse=%v for %q", got, tc.summary)
		}
	}
	c := &turnCard{p: p, entries: []core.ProgressCardEntry{{Kind: core.ProgressEntryThinking, Text: strings.Repeat("a", 600) + "\n</details><div>\n```go\nx := 1"}}, answer: "**Visible answer**"}
	text := c.renderPages()[0]
	if strings.Count(text, "</details>") != 1 || !strings.Contains(text, "&lt;/details&gt;&lt;div&gt;") || !strings.Contains(text, "x := 1\n```\n\n</details>") || !strings.HasSuffix(text, "**Visible answer**") {
		t.Fatalf("summary escaped its section: %s", text)
	}
	for _, sample := range []string{"```xml\n<item />\n```", "~~~xml\n<item />\n~~~", "    ```\n    <item />"} {
		if got := escapeProgressHTML(closeProgressFence(sample)); got != sample {
			t.Fatalf("code changed while isolating summary: %q", got)
		}
	}
}

func TestTurnCardOverflowPreservesBeginningAndAnswer(t *testing.T) {
	var sent []string
	p, _ := richTestPlatform(t, func(req *http.Request) (*http.Response, error) {
		var body richMessageParams
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body.MessageID != 0 {
			t.Fatal("unexpected edit")
		}
		sent = append(sent, body.RichMessage.Markdown)
		return richResponse(200, fmt.Sprintf(`{"ok":true,"result":{"message_id":%d}}`, len(sent))), nil
	})
	progress := "BEGIN " + strings.Repeat("界", 70000) + " END"
	c := &turnCard{p: p, rc: replyContext{chatID: 42}, entries: []core.ProgressCardEntry{{Kind: core.ProgressEntryThinking, Text: progress}}}
	if err := c.Finalize(context.Background(), "Final answer"); err != nil {
		t.Fatal(err)
	}
	if len(sent) < 3 {
		t.Fatal("oversized progress was not paginated")
	}
	for _, text := range sent {
		if utf8.RuneCountInString(text) > telegramMaxRichMessageLen || strings.Count(text, "<details>") != strings.Count(text, "</details>") {
			t.Fatal("oversized or unbalanced page")
		}
	}
	all := strings.Join(sent, "")
	if !strings.Contains(sent[0], "BEGIN") || !strings.Contains(all, "END") || strings.Count(all, "界") != 70000 || !strings.HasSuffix(all, "Final answer") {
		t.Fatal("overflow lost progress or answer")
	}
}

func TestTurnCardExplicitRejectionReflowsRegularFallback(t *testing.T) {
	calls := 0
	p, bot := richTestPlatform(t, func(*http.Request) (*http.Response, error) {
		calls++
		return richResponse(400, `{"ok":false,"error_code":400,"description":"can't parse rich message"}`), nil
	})
	c := &turnCard{p: p, rc: replyContext{chatID: 42, threadID: 7}, entries: []core.ProgressCardEntry{{Kind: core.ProgressEntryThinking, Text: strings.Repeat("界", 7000)}}}
	if err := c.Finalize(context.Background(), "Final answer"); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || len(bot.sends) < 4 {
		t.Fatalf("rich calls=%d regular sends=%d", calls, len(bot.sends))
	}
	var all string
	for _, send := range bot.sends {
		if utf8.RuneCountInString(send.Text) > telegramMaxMessageLen/2 || send.ChatID != int64(42) || send.MessageThreadID != 7 || strings.Contains(send.Text, "details") {
			t.Fatalf("invalid fallback: %+v", send)
		}
		all += send.Text
	}
	if strings.Count(all, "界") != 7000 || !strings.HasSuffix(all, "Final answer") {
		t.Fatal("fallback lost content")
	}
}

func TestTurnCardAmbiguousSendIsNotDuplicated(t *testing.T) {
	calls := 0
	p, bot := richTestPlatform(t, func(*http.Request) (*http.Response, error) { calls++; return nil, errors.New("connection lost") })
	c := &turnCard{p: p, rc: replyContext{chatID: 42}}
	if err := c.AppendProgress(context.Background(), core.ProgressCardEntry{Kind: core.ProgressEntryThinking, Text: strings.Repeat("x", 600)}); !errors.Is(err, core.ErrDeliveryUncertain) {
		t.Fatalf("err=%v", err)
	}
	c.last = time.Time{}
	if err := c.Finalize(context.Background(), "Final answer"); !errors.Is(err, core.ErrDeliveryUncertain) {
		t.Fatalf("err=%v", err)
	}
	if calls != 1 || len(bot.sends) != 0 {
		t.Fatal("uncertain delivery was duplicated")
	}
}

func TestCollapseProgressOptions(t *testing.T) {
	for _, opts := range []map[string]any{
		{"collapse_progress": true},
		{"rich_messages": "auto", "collapse_progress": "yes"},
		{"rich_messages": true, "collapse_progress_after": 0},
		{"rich_messages": true, "collapse_progress_after": "600"},
	} {
		opts["token"] = "test"
		if _, err := New(opts); err == nil {
			t.Fatalf("accepted invalid options: %#v", opts)
		}
	}
	platform, err := New(map[string]any{"token": "test", "rich_messages": "auto", "collapse_progress": true, "collapse_progress_after": int64(400)})
	if err != nil {
		t.Fatal(err)
	}
	if platform.(*Platform).collapseProgressAfter != 400 {
		t.Fatal("custom threshold ignored")
	}
	p := &Platform{}
	if card, err := p.CreateStreamingCard(context.Background(), nil); err != nil || card != nil {
		t.Fatal("unconfigured platform opted into combined turns")
	}
}

type rateLimitedTurnBot struct {
	telegramBot
	rejected bool
	calls    int
}

func (b *rateLimitedTurnBot) SendMessage(ctx context.Context, params *tgbot.SendMessageParams) (*models.Message, error) {
	b.calls++
	if b.rejected {
		return nil, &tgbot.TooManyRequestsError{Message: "Too Many Requests", RetryAfter: 30}
	}
	return b.telegramBot.SendMessage(ctx, params)
}

func TestTurnCardRateLimitRejectionAllowsFinalDelivery(t *testing.T) {
	for _, regular := range []bool{false, true} {
		for _, deadline := range []bool{false, true} {
			t.Run(fmt.Sprintf("regular=%v/deadline=%v", regular, deadline), func(t *testing.T) {
				original := sleepCtx
				t.Cleanup(func() { sleepCtx = original })
				sleepCtx = func(context.Context, time.Duration) error {
					if deadline {
						return context.DeadlineExceeded
					}
					return nil
				}
				rejected, calls := true, 0
				p, bot := richTestPlatform(t, func(*http.Request) (*http.Response, error) {
					calls++
					if rejected {
						return richResponse(429, `{"ok":false,"error_code":429,"description":"Too Many Requests","parameters":{"retry_after":30}}`), nil
					}
					return richResponse(200, `{"ok":true,"result":{"message_id":123}}`), nil
				})
				limited := &rateLimitedTurnBot{telegramBot: bot, rejected: true}
				p.bot = limited
				c := &turnCard{p: p, regular: regular, rc: replyContext{chatID: 42}}
				err := c.AppendProgress(context.Background(), core.ProgressCardEntry{Kind: core.ProgressEntryThinking, Text: "Working"})
				if err == nil || errors.Is(err, core.ErrDeliveryUncertain) {
					t.Fatalf("an explicit rejection must remain retryable, got %v", err)
				}
				rejected, limited.rejected = false, false
				if err := c.Finalize(context.Background(), "Final answer"); err != nil {
					t.Fatal(err)
				}
				wantCalls := rateLimitMaxAttempts + 1
				if deadline {
					wantCalls = 2
				}
				if regular {
					if limited.calls != wantCalls || len(bot.sends) != 1 || !strings.HasSuffix(bot.sends[0].Text, "Final answer") {
						t.Fatalf("final delivery missing after %d calls: %+v", limited.calls, bot.sends)
					}
				} else if calls != wantCalls || len(c.pages) != 1 || !strings.HasSuffix(c.pages[0].text, "Final answer") {
					t.Fatalf("final delivery missing after %d calls: %+v", calls, c.pages)
				}
			})
		}
	}
}

func TestTurnCardFlushesThrottledUpdatesWithoutAnotherEvent(t *testing.T) {
	for _, answer := range []bool{false, true} {
		t.Run(fmt.Sprintf("answer=%v", answer), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			requests := make(chan richMessageParams, 4)
			p, _ := richTestPlatform(t, func(req *http.Request) (*http.Response, error) {
				var body richMessageParams
				if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
					return nil, err
				}
				requests <- body
				return richResponse(200, `{"ok":true,"result":{"message_id":88}}`), nil
			})
			c := &turnCard{p: p, rc: replyContext{chatID: 42}}
			if err := c.AppendProgress(ctx, core.ProgressCardEntry{Kind: core.ProgressEntryThinking, Text: "Initial summary"}); err != nil {
				t.Fatal(err)
			}
			<-requests
			if err := c.AppendProgress(ctx, core.ProgressCardEntry{Kind: core.ProgressEntryThinking, Text: "Latest summary"}); err != nil {
				t.Fatal(err)
			}
			if answer {
				if err := c.Update(ctx, "Explanation before permission"); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case body := <-requests:
				if body.MessageID != 88 || !strings.Contains(body.RichMessage.Markdown, "Latest summary") ||
					(answer && !strings.HasSuffix(body.RichMessage.Markdown, "Explanation before permission")) {
					t.Fatalf("latest content was not flushed into the same message: %+v", body)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("throttled update remained invisible without a subsequent event")
			}
			if err := c.Finalize(ctx, "Done"); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestTurnCardTrailingFlushStopsAfterFinalizeOrCancellation(t *testing.T) {
	for _, finalize := range []bool{false, true} {
		t.Run(fmt.Sprintf("finalize=%v", finalize), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			requests := make(chan string, 4)
			p, _ := richTestPlatform(t, func(req *http.Request) (*http.Response, error) {
				var body richMessageParams
				if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
					return nil, err
				}
				requests <- body.RichMessage.Markdown
				return richResponse(200, `{"ok":true,"result":{"message_id":88}}`), nil
			})
			c := &turnCard{p: p, rc: replyContext{chatID: 42}}
			if err := c.Update(ctx, "Initial answer"); err != nil {
				t.Fatal(err)
			}
			<-requests
			if err := c.Update(ctx, "Pending answer"); err != nil {
				t.Fatal(err)
			}
			if finalize {
				if err := c.Finalize(ctx, "Final answer"); err != nil {
					t.Fatal(err)
				}
				if text := <-requests; text != "Final answer" {
					t.Fatalf("final text = %q", text)
				}
			} else {
				cancel()
			}
			select {
			case text := <-requests:
				t.Fatalf("stale flush after finalization or cancellation: %q", text)
			case <-time.After(1200 * time.Millisecond):
			}
		})
	}
}
