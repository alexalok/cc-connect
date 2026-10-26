package core

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type topicDiscoveryAgent struct {
	stubAgent
	infos   []AgentSessionInfo
	err     error
	entered chan struct{}
}

func (a *topicDiscoveryAgent) DiscoverSessions(ctx context.Context, _ time.Time) ([]AgentSessionInfo, error) {
	if a.entered != nil {
		close(a.entered)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return a.infos, a.err
}

type topicSyncPlatform struct {
	stubPlatformEngine
	target string
	err    error
	keys   []string
}

func (p *topicSyncPlatform) SessionTopicSyncConfig() (string, time.Duration) {
	return p.target, time.Hour
}
func (p *topicSyncPlatform) CreateSessionTopic(_ context.Context, title string) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.err != nil {
		return "", p.err
	}
	key := fmt.Sprintf("test:owner:topic%d", len(p.keys)+1)
	p.keys = append(p.keys, key)
	p.sent = append(p.sent, "topic: "+title)
	return key, nil
}

func newTopicSyncTest(t *testing.T, a Agent, p *topicSyncPlatform, path string) (*Engine, *sessionTopicSync) {
	t.Helper()
	e := NewEngine("sync", a, []Platform{p}, path, LangEnglish)
	t.Cleanup(func() {
		if err := e.Stop(); err != nil {
			t.Error(err)
		}
	})
	workers, err := e.prepareSessionTopicSync()
	if err != nil {
		t.Fatal(err)
	}
	if len(workers) != 1 {
		t.Fatalf("workers = %d", len(workers))
	}
	return e, workers[0]
}

func TestSessionTopicSync_PersistsCutoffAndLinksAcrossRestarts(t *testing.T) {
	a := &topicDiscoveryAgent{infos: []AgentSessionInfo{{ID: "native", Summary: "Native chat"}}}
	p := &topicSyncPlatform{target: "owner"}
	path := filepath.Join(t.TempDir(), "sessions.json")
	e, w := newTopicSyncTest(t, a, p, path)
	cutoff := w.state.Since
	if err := w.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(p.keys) != 1 {
		t.Fatalf("created %d topics", len(p.keys))
	}
	key := p.keys[0]
	if got := e.sessions.GetOrCreateActive(key).GetAgentSessionID(); got != "native" {
		t.Fatalf("bound %q", got)
	}
	if err := w.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(p.keys) != 1 {
		t.Fatal("repeated poll created duplicate topic")
	}
	// Simulate a crash after journaling the topic but before saving sessions.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	e2, w2 := newTopicSyncTest(t, a, p, path)
	if !w2.state.Since.Equal(cutoff) {
		t.Fatal("restart reset the new-chat cutoff")
	}
	if got := e2.sessions.GetOrCreateActive(key).GetAgentSessionID(); got != "native" {
		t.Fatalf("restored %q", got)
	}
	if err := w2.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(p.keys) != 1 {
		t.Fatal("restart created duplicate topic")
	}
	// New sessions discovered after a restart (including downtime) are linked.
	a.infos = append(a.infos, AgentSessionInfo{ID: "offline", Summary: "Offline chat"})
	if err := w2.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(p.keys) != 2 {
		t.Fatal("missed chat created during downtime")
	}
}

func TestSessionTopicSync_DoesNotEchoBotSessionsOrUndoUserSwitch(t *testing.T) {
	a := &topicDiscoveryAgent{infos: []AgentSessionInfo{{ID: "bot"}, {ID: "past"}, {ID: "native"}}}
	p := &topicSyncPlatform{target: "owner"}
	path := filepath.Join(t.TempDir(), "sessions.json")
	e, w := newTopicSyncTest(t, a, p, path)
	s := e.sessions.SwitchToAgentSession("test:existing", "past", a.Name(), "")
	s.SetAgentInfo("bot", a.Name(), "")
	e.sessions.legacyData = true
	if err := w.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(p.keys) != 1 {
		t.Fatalf("echoed bot sessions: %v", p.keys)
	}
	key := p.keys[0]
	e.sessions.SwitchToAgentSession(key, "user-selected", a.Name(), "Selected")
	e2, _ := newTopicSyncTest(t, a, p, path)
	if got := e2.sessions.GetOrCreateActive(key).GetAgentSessionID(); got != "user-selected" {
		t.Fatalf("overrode selection with %q", got)
	}
}

func TestSessionTopicSync_RetriesFailuresAndRejectsCorruptState(t *testing.T) {
	a := &topicDiscoveryAgent{infos: []AgentSessionInfo{{ID: "native"}}, err: errors.New("offline")}
	p := &topicSyncPlatform{target: "owner", err: errors.New("unavailable")}
	e, w := newTopicSyncTest(t, a, p, filepath.Join(t.TempDir(), "sessions.json"))
	if err := w.poll(context.Background()); err == nil {
		t.Fatal("ignored discovery error")
	}
	a.err = nil
	if err := w.poll(context.Background()); err == nil {
		t.Fatal("ignored creation error")
	}
	p.err = nil
	if err := w.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(p.keys) != 1 {
		t.Fatal("failed to retry creation")
	}
	if err := os.WriteFile(w.path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := e.prepareSessionTopicSync(); err == nil {
		t.Fatal("corrupt state silently reset sync")
	}
}

func TestSessionTopicSync_RequiresSupportedAgentAndPersistentStorage(t *testing.T) {
	for _, tc := range []struct {
		name         string
		agent        Agent
		target, path string
		wantError    bool
	}{
		{"disabled", &stubAgent{}, "", "", false},
		{"unsupported", &stubAgent{}, "owner", filepath.Join(t.TempDir(), "s.json"), true},
		{"no storage", &topicDiscoveryAgent{}, "owner", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &topicSyncPlatform{target: tc.target}
			e := NewEngine("test", tc.agent, []Platform{p}, tc.path, LangEnglish)
			defer e.Stop()
			_, err := e.prepareSessionTopicSync()
			if (err != nil) != tc.wantError {
				t.Fatalf("prepare error = %v", err)
			}
		})
	}
}

func TestSessionTopicSync_DoesNotCreateTopicsWithoutDurableState(t *testing.T) {
	a := &topicDiscoveryAgent{infos: []AgentSessionInfo{{ID: "native"}}}
	p := &topicSyncPlatform{target: "owner"}
	_, w := newTopicSyncTest(t, a, p, filepath.Join(t.TempDir(), "s.json"))
	// A directory cannot be atomically replaced with the state file. This
	// simulates an unwritable journal without relying on process privileges.
	w.path = t.TempDir()
	if err := w.poll(context.Background()); err == nil {
		t.Fatal("ignored journal write failure")
	}
	if len(p.keys) != 0 {
		t.Fatal("created a remote topic without writable state")
	}
}

func TestSessionTopicSync_StopCancelsAndJoinsDiscovery(t *testing.T) {
	a := &topicDiscoveryAgent{entered: make(chan struct{})}
	p := &topicSyncPlatform{target: "owner"}
	e := NewEngine("test", a, []Platform{p}, filepath.Join(t.TempDir(), "s.json"), LangEnglish)
	if err := e.Start(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-a.entered:
	case <-time.After(time.Second):
		t.Fatal("discovery did not start")
	}
	done := make(chan error, 1)
	go func() { done <- e.Stop() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Stop did not cancel discovery")
	}
}
