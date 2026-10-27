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
	status, err := source.ReadSessionStatus(ctx, id)
	if err != nil {
		return err
	}
	if status == "" {
		return nil
	}
	// Check selection and publish under the same locks used by /new, /switch,
	// deletion and TryLock. An old snapshot cannot overwrite a new bot turn.
	sm := w.engine.sessions
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	session := sm.sessions[sm.activeSession[key]]
	if session == nil || sm.deletedAgentSessionIDs[id] {
		return nil
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.AgentSessionID != id || session.busy {
		return nil
	}
	setter.SetConversationStatus(ctx, reply, status)
	return nil
}
