package core

import "context"

// SessionStatusSource supplies the latest native turn status without resuming it.
// Empty status means unknown or owned by the bridge's live event reader.
type SessionStatusSource interface {
	ReadSessionStatus(context.Context, string) (ConversationStatus, error)
}

func (w *sessionTopicSync) mirrorTopicStatus(ctx context.Context, key, id string, reply any) error {
	source, ok := w.source.(SessionStatusSource)
	if !ok {
		return nil
	}
	setter, ok := w.platform.(ConversationStatusSetter)
	if !ok {
		return nil
	}
	if enabled, ok := w.platform.(interface{ ConversationStatusEnabled() bool }); ok && !enabled.ConversationStatusEnabled() {
		return nil
	}
	// A bridge turn can begin and finish during the native read. Capture the
	// selected Session and generation before I/O, then validate both afterward.
	sm := w.engine.sessions
	sm.mu.RLock()
	selected := sm.sessions[sm.activeSession[key]]
	if selected == nil || sm.deletedAgentSessionIDs[id] {
		sm.mu.RUnlock()
		return nil
	}
	selected.mu.Lock()
	gen := selected.lockGen
	eligible := selected.AgentSessionID == id && !selected.busy
	selected.mu.Unlock()
	sm.mu.RUnlock()
	if !eligible {
		return nil
	}

	status, err := source.ReadSessionStatus(ctx, id)
	if err != nil {
		return err
	}
	if status == "" {
		return nil
	}
	// Keep validation and enqueue atomic with selection changes and TryLock.
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	session := sm.sessions[sm.activeSession[key]]
	if session != selected || sm.deletedAgentSessionIDs[id] {
		return nil
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.AgentSessionID != id || session.busy || session.lockGen != gen {
		return nil
	}
	setter.SetConversationStatus(ctx, reply, status)
	return nil
}
