package codex

import (
	"fmt"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

// SteerMessage preserves the active turn and its buffered assistant output.
// It never starts a turn if startup or final response delivery is still pending.
func (s *appServerSession) SteerMessage(prompt, messageID string, images []core.ImageAttachment, files []core.FileAttachment) (bool, error) {
	s.stateMu.Lock()
	turnID := s.currentTurn
	s.stateMu.Unlock()
	if turnID == "" {
		return false, nil
	}
	if len(files) > 0 {
		paths := core.SaveFilesToDisk(s.workDir, messageID, files)
		prompt = core.AppendFileRefs(prompt, paths)
	}
	prompt, imagePaths, err := s.stageImages(prompt, images)
	if err != nil {
		return false, err
	}
	input := []map[string]any{{"type": "text", "text": prompt, "text_elements": []any{}}}
	for _, path := range imagePaths {
		input = append(input, map[string]any{"type": "localImage", "path": path})
	}
	params := map[string]any{
		"threadId":            s.CurrentSessionID(),
		"expectedTurnId":      turnID,
		"input":               input,
		"clientUserMessageId": fmt.Sprintf("%s:%s:%d", mirrorTurnOrigin, messageID, time.Now().UnixNano()),
	}
	var response struct {
		TurnID string `json:"turnId"`
	}
	if err := s.request("turn/steer", params, &response); err != nil {
		return false, fmt.Errorf("codex app-server turn/steer: %w", err)
	}
	if response.TurnID != turnID {
		return false, fmt.Errorf("codex app-server steer returned turn %q, want %q", response.TurnID, turnID)
	}
	return true, nil
}

var _ core.MessageSteerer = (*appServerSession)(nil)
