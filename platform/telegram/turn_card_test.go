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
