package telegram

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestRichMessagesAutoKeepsShortRepliesRegular(t *testing.T) {
	for _, tc := range []struct {
		name    string
		content string
		rich    bool
	}{
		{"short", "Done — the service is healthy.", false},
		{"inline", "**Done.** See [the docs](https://example.com) and run `go test`.", false},
		{"simple_list", "- Build passed\n- Tests passed", false},
		{"short_unicode", strings.Repeat("😀", 400), false},
		{"long", strings.Repeat("x", 1200), true},
		{"below_length_threshold", strings.Repeat("x", 1199), false},
		{"long_unicode", strings.Repeat("😀", 1200), true},
		{"table", "| Item | Status |\n| --- | --- |\n| Build | Done |", true},
		{"table_without_outer_pipes", "Item | Status\n--- | ---\nBuild | Done", true},
		{"heading", "Report\n\n## Results\nEverything passed.", true},
		{"checklist", "- [x] Built\n- [ ] Deployed", true},
		{"nested_list", "- First\n  - Second", true},
		{"code", "```go\nfmt.Println(42)\n```", true},
		{"tilde_code", "~~~go\nfmt.Println(42)\n~~~", true},
		{"display_math", "$$\nx^2\n$$", true},
		{"latex_display", "\\[x^2\\]", true},
		{"details", "<details><summary>More</summary>Details</details>", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			p, bot := richTestPlatform(t, func(*http.Request) (*http.Response, error) {
				calls++
				return richResponse(200, `{"ok":true,"result":{"message_id":123}}`), nil
			})
			p.richMessagesAuto = true
			if err := p.Send(context.Background(), replyContext{chatID: 42}, tc.content); err != nil {
				t.Fatal(err)
			}
			if (calls == 1) != tc.rich || (len(bot.sends) == 1) == tc.rich {
				t.Fatalf("rich calls=%d regular calls=%d want rich=%v", calls, len(bot.sends), tc.rich)
			}
		})
	}
}

func TestRichMessagesAutoPromotesStructuredPreview(t *testing.T) {
	calls := 0
	p, bot := richTestPlatform(t, func(req *http.Request) (*http.Response, error) {
		calls++
		var body richMessageParams
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if req.URL.Path != "/bottest-secret/editMessageText" || body.MessageID != 88 || body.ChatID != 42 {
			t.Fatal("rich promotion changed the preview target")
		}
		return richResponse(200, `{"ok":true,"result":{"message_id":88}}`), nil
	})
	p.richMessagesAuto = true
	handle, err := p.SendPreviewStart(context.Background(), replyContext{chatID: 42, threadID: 7}, "Working…")
	if err != nil || len(bot.sends) != 1 || calls != 0 {
		t.Fatalf("short preview: err=%v regular=%d rich=%d", err, len(bot.sends), calls)
	}
	if err := p.UpdateMessage(context.Background(), handle, "## Results\n\nAll tests passed."); err != nil || calls != 1 || len(bot.edits) != 0 {
		t.Fatalf("structured edit: err=%v regular=%d rich=%d", err, len(bot.edits), calls)
	}
}

func TestRichMessageOptionValidation(t *testing.T) {
	for _, tc := range []struct {
		value   any
		enabled bool
		auto    bool
		valid   bool
	}{
		{nil, false, false, true},
		{false, false, false, true},
		{true, true, false, true},
		{"auto", true, true, true},
		{" AUTO ", true, true, true},
		{"sometimes", false, false, false},
		{123, false, false, false},
	} {
		platform, err := New(map[string]any{"token": "test", "allow_from": "*", "rich_messages": tc.value})
		if (err == nil) != tc.valid {
			t.Fatalf("option %v: unexpected error %v", tc.value, err)
		}
		if err == nil {
			p := platform.(*Platform)
			if p.richMessages != tc.enabled || p.richMessagesAuto != tc.auto {
				t.Fatalf("option %v: enabled=%v auto=%v", tc.value, p.richMessages, p.richMessagesAuto)
			}
		}
	}
}
