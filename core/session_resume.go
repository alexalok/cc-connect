package core

// SetResumeFallbackToNew controls whether a failed or unsafe resume may replace
// the saved agent session. The default is true. Setting false leaves the binding
// intact for retry or an explicit /new, without starting a replacement session.
func (e *Engine) SetResumeFallbackToNew(enabled bool) {
	e.disableResumeFallback.Store(!enabled)
}

// failedInteractiveState retains the queue and records a user-facing startup
// failure. The caller must hold interactiveMu.
func (e *Engine) failedInteractiveState(key string, p Platform, replyCtx any, agent Agent, session *Session, message MsgKey) *interactiveState {
	state := &interactiveState{
		platform:         p,
		replyCtx:         replyCtx,
		agent:            agent,
		busySession:      session,
		eventsNeedResync: true,
		startFailureMsg:  message,
	}
	adoptPendingFromPlaceholder(e.interactiveStates[key], state)
	e.interactiveStates[key] = state
	return state
}

func (e *Engine) rejectQueuedMessagesAfterResumeFailure(state *interactiveState) {
	state.mu.Lock()
	remaining := state.pendingMessages
	state.pendingMessages = nil
	message := state.startFailureMsg
	state.mu.Unlock()
	for _, queued := range remaining {
		e.reply(queued.platform, queued.replyCtx, e.i18n.T(message))
	}
}
