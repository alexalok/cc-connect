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
	e.publishConversationStatus(state, status, nil, 0)
}

// A stopped reader may still be unwinding a slow platform send after another
// turn starts. Validate both the state and busy-lock generation, keeping their
// locks until the status is enqueued so replacement cannot race publication.
func (e *Engine) updateConversationStatusForTurn(state *interactiveState, session *Session, key string, gen uint64, status ConversationStatus) {
	e.interactiveMu.Lock()
	defer e.interactiveMu.Unlock()
	if current := e.interactiveStates[key]; current != nil && current != state {
		return
	}
	e.publishConversationStatus(state, status, session, gen)
}

func (e *Engine) publishConversationStatus(state *interactiveState, status ConversationStatus, session *Session, gen uint64) {
	state.statusMu.Lock()
	defer state.statusMu.Unlock()
	state.mu.Lock()
	// Match the existing state.mu -> session.mu order used by queue draining.
	// Generation zero is the legacy escape hatch used by direct event tests.
	if session != nil && gen != 0 {
		session.mu.Lock()
		defer session.mu.Unlock()
		if session.lockGen != gen {
			state.mu.Unlock()
			return
		}
	}
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
