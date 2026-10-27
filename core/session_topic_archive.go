package core

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"
)

func (w *sessionTopicSync) removedSessionTopic(id string) bool {
	for _, removed := range w.state.DeletedTopics {
		if removed == id {
			return true
		}
	}
	return false
}

func (w *sessionTopicSync) removeArchivedTopics(ctx context.Context) error {
	source, ok := w.source.(SessionArchiveSource)
	if !ok {
		return nil
	}
	deleter, ok := w.target.(SessionTopicDeleter)
	if !ok {
		return nil
	}
	candidates := w.archiveTopicCandidates()
	ids := make([]string, 0, len(candidates))
	for _, id := range candidates {
		ids = append(ids, id)
	}
	archived, err := source.ArchivedSessionIDs(ctx, ids)
	if err != nil {
		return fmt.Errorf("session topic sync: read archives: %w", err)
	}
	w.archived = make(map[string]bool, len(archived))
	for _, id := range archived {
		w.archived[id] = true
	}
	keys := make([]string, 0, len(candidates))
	for key, id := range candidates {
		if w.archived[id] && w.state.DeletedTopics[key] == "" {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	var failures []error
	for _, key := range keys {
		if err := ctx.Err(); err != nil {
			return errors.Join(append(failures, err)...)
		}
		id := candidates[key]
		deleted, err := w.engine.sessions.deleteIdleArchivedTopic(key, id, func() (bool, error) {
			// Selection is locked during this call. Bound the request and let
			// the next poll retry longer outages or rate limits.
			deleteCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			return deleter.DeleteSessionTopic(deleteCtx, key)
		})
		if err != nil {
			failures = append(failures, fmt.Errorf("session topic sync: remove archived topic %s: %w", key, err))
			continue
		}
		if !deleted {
			continue
		}
		if w.state.DeletedTopics == nil {
			w.state.DeletedTopics = make(map[string]string)
		}
		w.state.DeletedTopics[key] = id
		// Keep history and the native archive intact, but stop any idle bridge
		// reader that could otherwise publish into the now-removed topic.
		w.stopArchivedTopicReader(key, id)
		if err := w.save(); err != nil {
			return errors.Join(append(failures, err)...)
		}
	}
	return errors.Join(failures...)
}

func (w *sessionTopicSync) archiveTopicCandidates() map[string]string {
	candidates := make(map[string]string)
	for id, key := range w.state.Topics {
		candidates[key] = id
	}
	sm := w.engine.sessions
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	// Current selections override imported links. This includes topics first
	// created in the messaging client, and deliberate /switch selections.
	for key, localID := range sm.activeSession {
		delete(candidates, key)
		if session := sm.sessions[localID]; session != nil {
			id := session.GetAgentSessionID()
			if id != "" && id != ContinueSession {
				candidates[key] = id
			}
		}
	}
	for key := range candidates {
		if w.state.DeletedTopics[key] != "" {
			delete(candidates, key)
		}
	}
	return candidates
}

// Hold the selection and turn locks until the API finishes: /new, /switch and
// a newly accepted turn must not repurpose a topic between the check and delete.
// Busy topics are retried on the next poll after their current turn settles.
func (sm *SessionManager) deleteIdleArchivedTopic(key, id string, remove func() (bool, error)) (bool, error) {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	if session := sm.sessions[sm.activeSession[key]]; session != nil {
		session.mu.Lock()
		defer session.mu.Unlock()
		if session.AgentSessionID != id || session.busy {
			return false, nil
		}
	}
	return remove()
}

func (w *sessionTopicSync) stopArchivedTopicReader(key, id string) {
	e := w.engine
	e.interactiveMu.Lock()
	state := e.interactiveStates[key]
	if state != nil {
		state.mu.Lock()
		matches := state.busySession != nil && state.busySession.GetAgentSessionID() == id
		state.mu.Unlock()
		if !matches {
			state = nil
		}
	}
	e.interactiveMu.Unlock()
	if state != nil {
		e.cleanupInteractiveState(key, state)
	}
}
