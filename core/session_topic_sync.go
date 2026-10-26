package core

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"
)

type sessionTopicSyncState struct {
	Since  time.Time         `json:"since"`
	Topics map[string]string `json:"topics"` // native session ID -> platform session key
	// Successful chunks per message; old topic-only journals start empty and
	// catch up their linked chats without changing the new-chat cutoff.
	Delivered map[string]map[string]int `json:"delivered,omitempty"`
}

// A worker owns its state and runs one poll at a time. The engine cancels and
// joins workers before stopping the platform and agent.
type sessionTopicSync struct {
	engine   *Engine
	source   SessionDiscoverer
	target   SessionTopicCreator
	path     string
	state    sessionTopicSyncState
	interval time.Duration
	messages SessionMessageSource
	platform Platform
	replies  ReplyContextReconstructor
}

func (e *Engine) prepareSessionTopicSync() ([]*sessionTopicSync, error) {
	var workers []*sessionTopicSync
	for _, p := range e.platforms {
		target, ok := p.(SessionTopicCreator)
		if !ok {
			continue
		}
		destination, interval := target.SessionTopicSyncConfig()
		if destination == "" {
			continue
		}
		source, ok := e.agent.(SessionDiscoverer)
		if !ok {
			return nil, fmt.Errorf("session topic sync: agent %s does not support discovery", e.agent.Name())
		}
		if e.multiWorkspace {
			return nil, fmt.Errorf("session topic sync requires a single-workspace project")
		}
		if e.sessions.StorePath() == "" || interval <= 0 {
			return nil, fmt.Errorf("session topic sync requires persistent session storage and a positive poll interval")
		}
		key := sha256.Sum256([]byte(e.agent.Name() + ":" + p.Name() + ":" + destination))
		worker := &sessionTopicSync{
			engine: e, source: source, target: target, interval: interval,
			path: fmt.Sprintf("%s.topic-sync-%x.json", e.sessions.StorePath(), key[:8]),
		}
		if messages, ok := e.agent.(SessionMessageSource); ok {
			if err := messages.ValidateSessionMessageSync(); err != nil {
				return nil, fmt.Errorf("session topic sync: %w", err)
			}
			replies, ok := p.(ReplyContextReconstructor)
			if !ok {
				return nil, fmt.Errorf("session topic sync: platform cannot address saved topics")
			}
			worker.messages, worker.platform, worker.replies = messages, p, replies
		}
		if err := worker.load(); err != nil {
			return nil, err
		}
		// Restore links before accepting messages, including recovery when the
		// topic journal was saved but the regular session snapshot was not.
		for id, sessionKey := range worker.state.Topics {
			worker.bind(sessionKey, id, "")
		}
		workers = append(workers, worker)
	}
	return workers, nil
}

func (w *sessionTopicSync) load() error {
	data, err := os.ReadFile(w.path)
	if os.IsNotExist(err) {
		w.state = sessionTopicSyncState{Since: time.Now().UTC(), Topics: make(map[string]string)}
		return w.save() // Establish the cutoff before any network calls.
	}
	if err != nil {
		return fmt.Errorf("session topic sync: read state: %w", err)
	}
	if err := json.Unmarshal(data, &w.state); err != nil {
		return fmt.Errorf("session topic sync: decode state: %w", err)
	}
	if w.state.Since.IsZero() || w.state.Topics == nil {
		return fmt.Errorf("session topic sync: invalid saved state in %s", w.path)
	}
	for id, key := range w.state.Topics {
		if id == "" || key == "" {
			return fmt.Errorf("session topic sync: invalid saved topic link in %s", w.path)
		}
	}
	for id, messages := range w.state.Delivered {
		if w.state.Topics[id] == "" {
			return fmt.Errorf("session topic sync: delivery progress has no topic")
		}
		for messageID, chunks := range messages {
			if messageID == "" || chunks < 0 {
				return fmt.Errorf("session topic sync: invalid delivery progress")
			}
		}
	}
	return nil
}

func (w *sessionTopicSync) save() error {
	data, err := json.Marshal(w.state)
	if err != nil {
		return fmt.Errorf("session topic sync: encode state: %w", err)
	}
	if err := AtomicWriteFile(w.path, data, 0o600); err != nil {
		return fmt.Errorf("session topic sync: save state: %w", err)
	}
	return nil
}

func (w *sessionTopicSync) bind(key, id, title string) {
	w.engine.sessions.bindSessionTopic(key, id, w.engine.agent.Name(), title)
}

func (sm *SessionManager) bindSessionTopic(key, id, agentName, title string) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	// A user may have deliberately used /new or /switch in this topic since
	// it was imported. Check deletion and existing choices atomically with
	// creation, so a concurrent delete cannot be undone by recovery either.
	if sm.deletedAgentSessionIDs[id] || len(sm.userSessions[key]) != 0 {
		return
	}
	session := sm.createLocked(key, title)
	session.SetAgentInfo(id, agentName, title)
	session.MarkExplicitlyActivated()
	sm.saveLocked()
}

func (w *sessionTopicSync) poll(ctx context.Context) error {
	// Retry any previous persistence failure before creating more topics.
	if err := w.save(); err != nil {
		return err
	}
	sessions, err := w.source.DiscoverSessions(ctx, w.state.Since)
	if err != nil {
		return fmt.Errorf("session topic sync: discover: %w", err)
	}
	for _, info := range sessions {
		if err := ctx.Err(); err != nil {
			return err
		}
		if info.ID == "" || w.state.Topics[info.ID] != "" {
			continue
		}
		// Recheck for every creation: the bot may have started a session while
		// the discovery scan was running. Bot-originated chats already have a
		// destination and must not be echoed back as a second topic.
		if w.engine.sessions.agentSessionDeleted(info.ID) || w.engine.sessions.hasAgentSession(info.ID) {
			continue
		}
		title := strings.TrimSpace(info.Summary)
		if title == "" {
			title = w.engine.i18n.T(MsgUntitled) + " " + info.ID
		}
		if err := w.createAndBind(ctx, info.ID, title); err != nil {
			return err
		}
	}
	return w.mirrorMessages(ctx)
}

func (w *sessionTopicSync) createAndBind(ctx context.Context, id, title string) error {
	// The key does not exist until the API returns, so take the creation gate
	// before publishing the topic. First inbound messages wait at the engine
	// entrypoint until the saved link is usable, including slash commands.
	w.engine.topicSyncBindingMu.Lock()
	defer w.engine.topicSyncBindingMu.Unlock()
	key, err := w.target.CreateSessionTopic(ctx, title)
	if err != nil {
		return fmt.Errorf("session topic sync: create topic: %w", err)
	}
	if key == "" {
		return fmt.Errorf("session topic sync: platform returned an empty session key")
	}
	w.state.Topics[id] = key
	// Keep the returned topic even if persistence fails so an in-process
	// retry cannot create a duplicate. Save its link before the session
	// snapshot, allowing startup to recover a partially saved binding.
	err = w.save()
	w.bind(key, id, title)
	return err
}

// Unlike the /list filter, discovery must also deduplicate IDs retained in a
// legacy session store, even if some older sessions have lost their IDs.
func (sm *SessionManager) hasAgentSession(id string) bool {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	for _, session := range sm.sessions {
		session.mu.Lock()
		known := session.AgentSessionID == id
		for _, past := range session.PastAgentSessionIDs {
			known = known || past == id
		}
		session.mu.Unlock()
		if known {
			return true
		}
	}
	return false
}

func (w *sessionTopicSync) run(ctx context.Context, wg *sync.WaitGroup) {
	defer wg.Done()
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		pollCtx, cancel := context.WithTimeout(ctx, time.Minute)
		err := w.poll(pollCtx)
		cancel()
		if err != nil && ctx.Err() == nil {
			slog.Warn("session topic sync failed", "project", w.engine.name, "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
