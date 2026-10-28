package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/chenhg5/cc-connect/core"
	tgbot "github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

const telegramMaxRichMessageLen = 32768

const richMessageAutoMinLen = 1200

var richBlockPattern = regexp.MustCompile("(?mi)^(?:#{1,6}[ \\t]+\\S|[ \\t]*(?:```|~~~)|[ \\t]*[-+*][ \\t]+\\[[ x]\\]|[ \\t]*\\|?[ \\t]*:?-{3,}:?[ \\t]*\\|[ \\t]*:?-{3,}|[ \\t]*\\$\\$|[ \\t]*\\\\\\[|[ \\t]{2,}(?:[-+*]|[0-9]+[.)])[ \\t]+)")
var richHTMLPattern = regexp.MustCompile("(?i)<(?:details|table|figure|tg-math|tg-tex|h[1-6])(?:[ >])")

func parseRichMessageOption(value any) (enabled, automatic bool, err error) {
	switch v := value.(type) {
	case nil:
		return false, false, nil
	case bool:
		return v, false, nil
	case string:
		if strings.EqualFold(strings.TrimSpace(v), "auto") {
			return true, true, nil
		}
	}
	return false, false, fmt.Errorf("telegram: rich_messages must be false, true, or \"auto\"")
}

// Automatic mode leaves short conversational replies on the lightweight API.
// Inline emphasis, links, and inline code already work in regular messages.
func (p *Platform) useRichMessage(content string) bool {
	return p.richMessages && (!p.richMessagesAuto ||
		utf8.RuneCountInString(content) >= richMessageAutoMinLen ||
		richBlockPattern.MatchString(content) || richHTMLPattern.MatchString(content))
}

// The pinned Bot API client predates rich messages (Bot API 10.1). Keep the
// additional request fields here until the client supports them natively.
type richMessageContent struct {
	Markdown string `json:"markdown"`
}

type richMessageParams struct {
	ChatID          int64                        `json:"chat_id"`
	MessageThreadID int                          `json:"message_thread_id,omitempty"`
	MessageID       int                          `json:"message_id,omitempty"`
	RichMessage     richMessageContent           `json:"rich_message"`
	ReplyParameters *models.ReplyParameters      `json:"reply_parameters,omitempty"`
	ReplyMarkup     *models.InlineKeyboardMarkup `json:"reply_markup,omitempty"`
}

type richMessageAPIError struct {
	code        int
	description string
}

func (e *richMessageAPIError) Error() string {
	return fmt.Sprintf("telegram: rich message rejected (API %d): %s", e.code, e.description)
}

func richMessageFallback(err error) bool {
	var apiErr *richMessageAPIError
	if !errors.As(err, &apiErr) {
		return false
	}
	if apiErr.code == http.StatusNotFound {
		return true // Older Bot API servers do not implement sendRichMessage.
	}
	if apiErr.code != http.StatusBadRequest {
		return false
	}
	description := strings.ToLower(apiErr.description)
	for _, reason := range []string{"parse", "rich_message", "rich message", "too long", "too many blocks", "text must be non-empty", "message text is empty"} {
		if strings.Contains(description, reason) {
			return true
		}
	}
	return false
}

// tryRichMessage falls back only after an explicit rejection. A timeout or 5xx
// may occur after delivery, so retrying as a regular message could duplicate it.
func (p *Platform) tryRichMessage(ctx context.Context, method string, params richMessageParams) (*models.Message, bool, error) {
	if !p.useRichMessage(params.RichMessage.Markdown) {
		return nil, false, nil
	}
	message, err := retryOnRateLimit(ctx, method, func() (*models.Message, error) {
		return p.requestRichMessage(ctx, method, params)
	})
	if method == "editMessageText" {
		var apiErr *richMessageAPIError
		if errors.As(err, &apiErr) && apiErr.code == http.StatusBadRequest && strings.Contains(apiErr.description, "not modified") {
			return nil, true, nil
		}
	}
	if richMessageFallback(err) {
		slog.Warn("telegram: rich message rejected, using regular formatting", "method", method, "error", err)
		return nil, false, nil
	}
	return message, true, err
}

func (p *Platform) requestRichMessage(ctx context.Context, method string, params richMessageParams) (*models.Message, error) {
	body, err := json.Marshal(params)
	if err != nil {
		return nil, fmt.Errorf("telegram: encode rich message: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.telegram.org/bot"+p.token+"/"+method, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("telegram: prepare rich message: %s", core.RedactToken(err.Error(), p.token))
	}
	req.Header.Set("Content-Type", "application/json")
	client := p.httpClient
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("telegram: request rich message: %s", core.RedactToken(err.Error(), p.token))
	}
	defer resp.Body.Close()
	var result struct {
		OK          bool            `json:"ok"`
		ErrorCode   int             `json:"error_code"`
		Description string          `json:"description"`
		Message     *models.Message `json:"result"`
		Parameters  struct {
			RetryAfter int `json:"retry_after"`
		} `json:"parameters"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&result); err != nil {
		return nil, fmt.Errorf("telegram: decode rich message response: %w", err)
	}
	if resp.StatusCode != http.StatusOK || !result.OK {
		code := result.ErrorCode
		if code == 0 {
			code = resp.StatusCode
		}
		description := core.RedactToken(result.Description, p.token)
		if code == http.StatusTooManyRequests {
			return nil, &tgbot.TooManyRequestsError{Message: description, RetryAfter: result.Parameters.RetryAfter}
		}
		return nil, &richMessageAPIError{code: code, description: description}
	}
	if result.Message == nil || result.Message.ID == 0 {
		return nil, fmt.Errorf("telegram: rich message response missing message ID")
	}
	return result.Message, nil
}

func (p *Platform) sendRichChunks(ctx context.Context, bot telegramBot, rc replyContext, content string, reply bool, keyboard *models.InlineKeyboardMarkup) error {
	for i, chunk := range core.SplitMessageCodeFenceAware(content, telegramMaxRichMessageLen) {
		params := richMessageParams{
			ChatID: rc.chatID, MessageThreadID: rc.threadID,
			RichMessage: richMessageContent{Markdown: chunk},
		}
		if i == 0 {
			params.ReplyMarkup = keyboard
			if reply && rc.messageID != 0 {
				params.ReplyParameters = &models.ReplyParameters{MessageID: rc.messageID}
			}
		}
		_, handled, err := p.tryRichMessage(ctx, "sendRichMessage", params)
		if err != nil {
			return fmt.Errorf("telegram: send rich chunk %d: %w", i, err)
		}
		if !handled {
			if err := p.sendRegularRichFallback(ctx, bot, params, chunk); err != nil {
				return err
			}
		} else {
			p.refreshTyping(rc)
		}
	}
	return nil
}

func (p *Platform) sendRegularRichFallback(ctx context.Context, bot telegramBot, rich richMessageParams, content string) error {
	// Split the original Markdown before converting it, so fallback never
	// exposes chunks of broken HTML tags or loses code-fence boundaries.
	// A Unicode character may occupy two UTF-16 units in regular messages.
	for i, chunk := range core.SplitMessageCodeFenceAware(content, telegramMaxMessageLen/2) {
		params := &tgbot.SendMessageParams{
			ChatID: rich.ChatID, MessageThreadID: rich.MessageThreadID,
			Text: core.MarkdownToSimpleHTML(chunk), ParseMode: models.ParseModeHTML,
		}
		if i == 0 {
			params.ReplyParameters = rich.ReplyParameters
			if rich.ReplyMarkup != nil {
				params.ReplyMarkup = rich.ReplyMarkup
			}
		}
		if _, err := bot.SendMessage(ctx, params); err != nil {
			if !strings.Contains(err.Error(), "can't parse") && !strings.Contains(err.Error(), "too long") {
				return fmt.Errorf("telegram: send rich fallback: %s", core.RedactToken(err.Error(), p.token))
			}
			params.Text, params.ParseMode = chunk, ""
			if _, err := bot.SendMessage(ctx, params); err != nil {
				return fmt.Errorf("telegram: send plain rich fallback: %s", core.RedactToken(err.Error(), p.token))
			}
		}
		p.refreshTyping(replyContext{chatID: rich.ChatID, threadID: rich.MessageThreadID})
	}
	return nil
}

// Rich Markdown does not parse Markdown inside paragraph HTML. Isolate literal
// choices there while leaving the original prompt's tables/code untouched.
func richNumberedButtonContent(prompt string, original, numbered [][]core.ButtonOption) string {
	var body strings.Builder
	body.WriteString(prompt)
	lineBreaks := strings.NewReplacer("\r\n", "<br>", "\n", "<br>", "\r", "<br>")
	for i, row := range original {
		for j, button := range row {
			if body.Len() > 0 {
				body.WriteString("\n\n")
			}
			fmt.Fprintf(&body, "<p><b>%s:</b> %s</p>", html.EscapeString(numbered[i][j].Text), lineBreaks.Replace(html.EscapeString(button.Text)))
		}
	}
	return body.String()
}
