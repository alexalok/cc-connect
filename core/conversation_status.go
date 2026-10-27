package core

import "context"

// ConversationStatus describes the latest turn and any outstanding user action.
// Finished means the turn ended, not that the user has read its response.
type ConversationStatus string

const (
	ConversationWorking  ConversationStatus = "working"
	ConversationQuestion ConversationStatus = "question"
	ConversationApproval ConversationStatus = "approval"
	ConversationFinished ConversationStatus = "finished"
	ConversationFailed   ConversationStatus = "failed"
	ConversationStopped  ConversationStatus = "stopped"
)

// ConversationStatusSetter is an optional presentation capability. Implementations
// must enqueue updates promptly so a slow status API cannot delay agent messages.
type ConversationStatusSetter interface {
	SetConversationStatus(context.Context, any, ConversationStatus)
}

// updateConversationStatus serializes publication with async question answers.
// An unanswered question outlives a completed turn; background progress must not
// make it disappear. Failures and explicit stops override outstanding requests.
func (e *Engine) updateConversationStatus(state *interactiveState, status ConversationStatus) {
	state.statusMu.Lock()
	defer state.statusMu.Unlock()
	state.mu.Lock()
	if status != "" && (state.conversationStatus != ConversationStopped || status == ConversationWorking) {
		state.conversationStatus = status
	}
	visible := state.conversationStatus
	if visible != ConversationFailed && visible != ConversationStopped {
		if state.stopped {
			visible = ConversationStopped
		} else if state.pending != nil {
			if len(state.pending.Questions) > 0 {
				visible = ConversationQuestion
			} else {
				visible = ConversationApproval
			}
		} else if state.pendingUserInput != nil {
			visible = ConversationQuestion
		}
	}
	p, replyCtx := state.platform, state.replyCtx
	state.mu.Unlock()
	if setter, ok := p.(ConversationStatusSetter); ok && visible != "" {
		setter.SetConversationStatus(e.ctx, replyCtx, visible)
	}
}
