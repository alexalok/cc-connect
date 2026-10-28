package telegram

import (
	"fmt"
	"html"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/chenhg5/cc-connect/core"
)

const defaultButtonTextLimit = 32

func parseButtonTextLimit(opts map[string]any) (int, error) {
	value, exists := opts["button_text_limit"]
	if !exists {
		return defaultButtonTextLimit, nil
	}
	limit, err := strconv.Atoi(fmt.Sprint(value))
	// Leave room for localized numbered labels such as "Option 1".
	if err != nil || limit < 16 {
		return 0, fmt.Errorf("telegram: button_text_limit must be an integer of at least 16")
	}
	return limit, nil
}

// prepareButtonLabels keeps full choices readable when a client would truncate
// their buttons. Number the whole keyboard so its labels match the message in
// order, preserving callback data and the caller's slices. Build the HTML body
// separately so literal button labels never pass through the Markdown parser.
func (p *Platform) prepareButtonLabels(content string, buttons [][]core.ButtonOption) (string, string, [][]core.ButtonOption) {
	limit := p.buttonTextLimit
	if limit == 0 {
		limit = defaultButtonTextLimit
	}
	numbered := false
	for _, row := range buttons {
		for _, button := range row {
			if utf8.RuneCountInString(button.Text) > limit || strings.ContainsAny(button.Text, "\r\n") {
				numbered = true
				break
			}
		}
		if numbered {
			break
		}
	}
	if !numbered {
		return content, core.MarkdownToSimpleHTML(content), buttons
	}

	var body, htmlBody strings.Builder
	body.WriteString(content)
	htmlBody.WriteString(core.MarkdownToSimpleHTML(content))
	rows := make([][]core.ButtonOption, len(buttons))
	i18n := p.noticeI18n()
	number := 0
	for i, row := range buttons {
		rows[i] = make([]core.ButtonOption, len(row))
		for j, button := range row {
			number++
			label := i18n.Tf(core.MsgButtonOption, number)
			if body.Len() > 0 {
				body.WriteString("\n\n")
				htmlBody.WriteString("\n\n")
			}
			fmt.Fprintf(&body, "**%s:** %s", label, button.Text)
			fmt.Fprintf(&htmlBody, "<b>%s:</b> %s", html.EscapeString(label), html.EscapeString(button.Text))
			rows[i][j] = core.ButtonOption{Text: label, Data: button.Data}
		}
	}
	return body.String(), htmlBody.String(), rows
}
