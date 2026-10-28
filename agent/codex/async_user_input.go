package codex

import (
	"encoding/json"
	"strings"

	"github.com/chenhg5/cc-connect/core"
)

func (s *appServerSession) handleAsyncUserInput(item map[string]any) bool {
	if item["delivery"] != "async" {
		return false
	}
	var input []struct {
		Title   string   `json:"title"`
		Options []string `json:"options"`
	}
	raw, err := json.Marshal(item["questions"])
	if err == nil {
		err = json.Unmarshal(raw, &input)
	}
	var questions []core.UserQuestion
	if err == nil {
		for _, in := range input {
			if strings.TrimSpace(in.Title) == "" {
				continue
			}
			q := core.UserQuestion{Question: in.Title}
			for _, option := range in.Options {
				q.Options = append(q.Options, core.UserQuestionOption{Label: option})
			}
			questions = append(questions, q)
		}
	}
	id, _ := item["id"].(string)
	if id != "" && len(questions) > 0 {
		s.emit(core.Event{Type: core.EventUserInputRequest, RequestID: id, Questions: questions})
	} else if text, _ := item["text"].(string); strings.TrimSpace(text) != "" {
		s.emit(core.Event{Type: core.EventText, Content: text})
	}
	return true
}

func (s *appServerSession) SteerUserInput(prompt string) (bool, error) {
	return s.SteerMessage(prompt, "", nil, nil)
}

var _ core.UserInputSteerer = (*appServerSession)(nil)
