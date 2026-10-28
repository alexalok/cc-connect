package telegram

import (
	"context"
	"errors"
	"fmt"
	"html"
	"log/slog"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/chenhg5/cc-connect/core"
	tgbot "github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

const defaultCollapseProgressAfter = 600

func parseCollapseProgressOptions(opts map[string]any, rich bool) (bool, int, error) {
	enabled := false
	if value, exists := opts["collapse_progress"]; exists {
		var ok bool
		if enabled, ok = value.(bool); !ok {
			return false, 0, fmt.Errorf("telegram: collapse_progress must be a boolean")
		}
	}
	if enabled && !rich {
		return false, 0, fmt.Errorf("telegram: collapse_progress requires rich_messages = true or \"auto\"")
	}
	threshold := defaultCollapseProgressAfter
	if value, exists := opts["collapse_progress_after"]; exists {
		switch v := value.(type) {
		case int:
			threshold = v
		case int64:
			threshold = int(v)
		default:
			return false, 0, fmt.Errorf("telegram: collapse_progress_after must be a positive integer")
		}
		if threshold <= 0 || threshold > telegramMaxRichMessageLen {
			return false, 0, fmt.Errorf("telegram: collapse_progress_after must be between 1 and %d", telegramMaxRichMessageLen)
		}
	}
	return enabled, threshold, nil
}

type turnCardPage struct {
	id   int
	text string
}

// turnCard retains the complete public progress until the turn finishes. Only
// the first page is streamed; overflow is delivered at completion, with balanced
// details blocks on each page and the answer outside all collapsed sections.
type turnCard struct {
	mu        sync.Mutex
	p         *Platform
	rc        replyContext
	entries   []core.ProgressCardEntry
	answer    string
	pages     []turnCardPage
	last      time.Time
	pending   *time.Timer
	regular   bool
	finished  bool
	uncertain error
}

var _ core.ProgressStreamingCard = (*turnCard)(nil)

func (p *Platform) CreateStreamingCard(_ context.Context, rctx any) (core.StreamingCard, error) {
	if !p.collapseProgress {
		return nil, nil
	}
	rc, ok := rctx.(replyContext)
	if !ok {
		return nil, fmt.Errorf("telegram: invalid turn reply context type %T", rctx)
	}
	return &turnCard{p: p, rc: rc}, nil
}

// A failed update retains its card and retries edits at finalization. Switching
// back to independent progress messages would duplicate the accumulated turn.
func (c *turnCard) Failed() bool { return false }

func (c *turnCard) AppendProgress(ctx context.Context, entry core.ProgressCardEntry) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.finished {
		return nil
	}
	if entry.Kind == core.ProgressEntryToolResult {
		for i := len(c.entries) - 1; i >= 0; i-- {
			if c.entries[i].Kind == core.ProgressEntryToolUse && c.entries[i].Tool == entry.Tool {
				c.entries[i] = entry
				break
			}
		}
	} else {
		c.entries = append(c.entries, entry)
	}
	return c.flush(ctx, false)
}

func (c *turnCard) Update(ctx context.Context, answer string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.finished {
		return nil
	}
	c.answer = answer
	return c.flush(ctx, false)
}

func (c *turnCard) Finalize(ctx context.Context, answer string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.finished {
		return nil
	}
	c.answer = answer
	if err := c.flush(ctx, true); err != nil {
		return err
	}
	c.finished = true
	return nil
}

func (c *turnCard) progressText() string {
	var parts []string
	for _, entry := range c.entries {
		switch entry.Kind {
		case core.ProgressEntryThinking, core.ProgressEntryInfo, core.ProgressEntryError:
			if text := strings.TrimSpace(entry.Text); text != "" {
				parts = append(parts, closeProgressFence(text))
			}
		case core.ProgressEntryToolUse, core.ProgressEntryToolResult:
			icon := "🔧"
			if entry.Kind == core.ProgressEntryToolResult {
				icon = "✓"
				if (entry.Success != nil && !*entry.Success) || (entry.ExitCode != nil && *entry.ExitCode != 0) ||
					entry.Status == "failed" || entry.Status == "error" {
					icon = "✗"
				}
			}
			parts = append(parts, icon+" `"+strings.ReplaceAll(entry.Tool, "`", "'")+"`")
		}
	}
	return strings.Join(parts, "\n\n")
}

// Close an unfinished code sample before inserting the details closing tag, so
// a partial summary cannot accidentally pull the final answer into its fence.
func closeProgressFence(text string) string {
	fence := ""
	for _, line := range strings.Split(text, "\n") {
		fence = progressFenceAfter(fence, line)
	}
	if fence != "" {
		return text + "\n" + fence
	}
	return text
}

func progressFenceAfter(fence, line string) string {
	// Four spaces (or a tab) starts indented code, not a fenced block.
	if strings.HasPrefix(line, "    ") || strings.HasPrefix(line, "\t") {
		return fence
	}
	line = strings.TrimSpace(line)
	if len(line) < 3 || (line[0] != '`' && line[0] != '~') {
		return fence
	}
	n := 0
	for n < len(line) && line[n] == line[0] {
		n++
	}
	if n < 3 {
		return fence
	}
	if fence == "" {
		if line[0] == '`' && strings.Contains(line[n:], "`") {
			return ""
		}
		return line[:n]
	}
	if line[0] == fence[0] && n >= len(fence) && strings.TrimSpace(line[n:]) == "" {
		return ""
	}
	return fence
}

func escapeProgressHTML(text string) string {
	lines := strings.Split(text, "\n")
	fence := ""
	replacer := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	for i, line := range lines {
		next := progressFenceAfter(fence, line)
		if fence == "" && next == "" && !strings.HasPrefix(line, "    ") && !strings.HasPrefix(line, "\t") {
			lines[i] = replacer.Replace(line)
		}
		fence = next
	}
	return strings.Join(lines, "\n")
}

func (c *turnCard) renderPages() []string {
	progress := c.progressText()
	limit := telegramMaxRichMessageLen
	collapsed := !c.regular && (utf8.RuneCountInString(progress) >= c.p.collapseProgressAfter || strings.Count(progress, "\n") >= 7)
	if c.regular {
		limit = telegramMaxMessageLen / 2 // Safe for non-BMP characters, too.
	} else if collapsed {
		// Treat HTML in summaries as text so it cannot close the wrapper or
		// introduce an HTML block that consumes the visible answer.
		progress = escapeProgressHTML(progress)
	}
	prefix, suffix := "", ""
	if collapsed {
		prefix = "<details><summary>" + html.EscapeString(c.p.noticeI18n().T(core.MsgProgressDetails)) + "</summary>\n\n"
		suffix = "\n\n</details>"
	}
	wrap := func(text string) string {
		if text == "" {
			return ""
		}
		return prefix + text + suffix
	}
	join := func(a, b string) string {
		if a == "" {
			return b
		}
		if b == "" {
			return a
		}
		return a + "\n\n---\n\n" + b
	}
	combined := join(wrap(progress), c.answer)
	if strings.TrimSpace(combined) == "" {
		return nil
	}
	if utf8.RuneCountInString(combined) <= limit {
		return []string{combined}
	}
	var pages []string
	if progress != "" {
		for _, chunk := range core.SplitMessageCodeFenceAware(progress, limit-utf8.RuneCountInString(prefix+suffix)-32) {
			pages = append(pages, wrap(closeProgressFence(chunk)))
		}
	}
	if c.answer != "" {
		if len(pages) > 0 && utf8.RuneCountInString(join(pages[len(pages)-1], c.answer)) <= limit {
			pages[len(pages)-1] = join(pages[len(pages)-1], c.answer)
		} else {
			pages = append(pages, core.SplitMessageCodeFenceAware(c.answer, limit)...)
		}
	}
	return pages
}

var errTurnRegularFallback = errors.New("use regular turn formatting")

func (c *turnCard) flush(ctx context.Context, final bool) error {
	if final {
		c.cancelPendingFlush()
	}
	if c.uncertain != nil {
		return c.uncertain
	}
	if err := ctx.Err(); err != nil {
		c.cancelPendingFlush()
		return err
	}
	if remaining := time.Second - time.Since(c.last); !final && remaining > 0 {
		c.scheduleFlush(ctx, remaining)
		return nil
	}
	c.cancelPendingFlush()
	c.last = time.Now()
	pages := c.renderPages()
	if !final && len(pages) > 1 {
		pages = pages[:1]
	}
	for i := 0; i < len(pages); i++ {
		if err := c.writePage(ctx, i, pages[i]); err != nil {
			if errors.Is(err, errTurnRegularFallback) {
				c.regular = true
				pages = c.renderPages()
				if !final && len(pages) > 1 {
					pages = pages[:1]
				}
				i = -1 // Reflow existing pages at the regular API's smaller limit.
				continue
			}
			return err
		}
	}
	return nil
}

// Called with c.mu held. Coalesce updates into one trailing write so a tool
// or permission wait cannot leave the latest public progress hidden indefinitely.
func (c *turnCard) scheduleFlush(ctx context.Context, delay time.Duration) {
	if c.pending != nil {
		return
	}
	var timer *time.Timer
	timer = time.AfterFunc(delay, func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.pending != timer {
			return // A finalization or newer write canceled this callback.
		}
		c.pending = nil
		if c.finished || ctx.Err() != nil {
			return
		}
		if err := c.flush(ctx, false); err != nil {
			slog.Warn("telegram: trailing turn update failed", "error", err)
		}
	})
	c.pending = timer
}

func (c *turnCard) cancelPendingFlush() {
	if c.pending != nil {
		c.pending.Stop()
		c.pending = nil
	}
}

func (c *turnCard) writePage(ctx context.Context, index int, text string) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	page := turnCardPage{}
	if index < len(c.pages) {
		page = c.pages[index]
	}
	if page.id != 0 && page.text == text {
		return nil
	}
	bot, err := c.p.connectedBot("update turn")
	if err != nil {
		return err
	}
	defer c.p.refreshTyping(c.rc)
	var sent *models.Message
	if !c.regular && c.p.useRichMessage(text) {
		method := "sendRichMessage"
		if page.id != 0 {
			method = "editMessageText"
		}
		var handled bool
		sent, handled, err = c.p.tryRichMessage(ctx, method, richMessageParams{
			ChatID: c.rc.chatID, MessageThreadID: c.rc.threadID, MessageID: page.id,
			RichMessage: richMessageContent{Markdown: text},
		})
		if !handled {
			return errTurnRegularFallback
		}
	} else {
		sent, err = c.writeRegular(ctx, bot, page.id, text)
	}
	if err == nil && page.id == 0 && (sent == nil || sent.ID == 0) {
		err = errors.New("turn response missing message ID")
	}
	if err != nil {
		var rateLimit *tgbot.TooManyRequestsError
		if errors.As(err, &rateLimit) {
			// A 429 confirms that nothing was delivered. A later event or
			// finalization must be allowed to retry even if this retry budget
			// or request deadline was exhausted.
			return fmt.Errorf("telegram: turn rate limited: %s", core.RedactToken(err.Error(), c.p.token))
		}
		if page.id == 0 {
			// A failed send has no safe edit target. Do not duplicate it by
			// sending another message on a subsequent event or finalization.
			c.uncertain = fmt.Errorf("%w: %s", core.ErrDeliveryUncertain, core.RedactToken(err.Error(), c.p.token))
			return c.uncertain
		}
		// Retrying an edit on the next event is safe, but a separate fallback
		// send could duplicate an edit whose response was lost.
		return fmt.Errorf("%w: telegram edit: %s", core.ErrDeliveryUncertain, core.RedactToken(err.Error(), c.p.token))
	}
	if page.id == 0 {
		page.id = sent.ID
	}
	page.text = text
	if index < len(c.pages) {
		c.pages[index] = page
	} else {
		c.pages = append(c.pages, page)
	}
	return nil
}

func (c *turnCard) writeRegular(ctx context.Context, bot telegramBot, id int, text string) (*models.Message, error) {
	formatted, mode := core.MarkdownToSimpleHTML(text), models.ParseModeHTML
	send := func() (*models.Message, error) {
		if id != 0 {
			return bot.EditMessageText(ctx, &tgbot.EditMessageTextParams{ChatID: c.rc.chatID, MessageID: id, Text: formatted, ParseMode: mode})
		}
		return bot.SendMessage(ctx, &tgbot.SendMessageParams{ChatID: c.rc.chatID, MessageThreadID: c.rc.threadID, Text: formatted, ParseMode: mode})
	}
	message, err := retryOnRateLimit(ctx, "turn message", send)
	if err != nil && (strings.Contains(err.Error(), "can't parse") || strings.Contains(err.Error(), "too long")) {
		formatted, mode = text, ""
		message, err = retryOnRateLimit(ctx, "plain turn message", send)
	}
	if id != 0 && err != nil && strings.Contains(err.Error(), "not modified") {
		return nil, nil
	}
	return message, err
}
