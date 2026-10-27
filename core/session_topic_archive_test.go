package core

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

type topicArchiveAgent struct {
	topicDiscoveryAgent
	archived   []string
	archiveErr error
	beforeRead func()
}

func (a *topicArchiveAgent) ArchivedSessionIDs(context.Context, []string) ([]string, error) {
	if a.beforeRead != nil {
		a.beforeRead()
	}
	return a.archived, a.archiveErr
}

type topicArchivePlatform struct {
	topicSyncPlatform
	deleted     []string
	deleteErr   error
	afterDelete func()
}

func (p *topicArchivePlatform) DeleteSessionTopic(_ context.Context, key string) (bool, error) {
	if p.deleteErr != nil {
		return false, p.deleteErr
	}
	p.deleted = append(p.deleted, key)
	p.mu.Lock()
	p.sent = append(p.sent, "deleted: "+key)
	p.mu.Unlock()
	if p.afterDelete != nil {
		p.afterDelete()
	}
	return true, nil
}

func TestSessionTopicSync_ArchiveRechecksSelectionAfterDiscovery(t *testing.T) {
	a := &topicArchiveAgent{topicDiscoveryAgent: topicDiscoveryAgent{infos: []AgentSessionInfo{{ID: "desktop"}}}}
	p := &topicArchivePlatform{topicSyncPlatform: topicSyncPlatform{target: "owner"}}
	e, w := newTopicSyncTest(t, a, p, filepath.Join(t.TempDir(), "sessions.json"))
	if err := w.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	key := p.keys[0]
	a.archived = []string{"desktop"}
	a.beforeRead = func() { e.sessions.SwitchToAgentSession(key, "replacement", a.Name(), "replacement") }
	if err := w.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(p.deleted) != 0 {
		t.Fatalf("deleted switched topic: %v", p.deleted)
	}
}

func TestSessionTopicSync_ArchiveSaveFailureRetainsSuccessfulDeletion(t *testing.T) {
	a := &topicArchiveAgent{topicDiscoveryAgent: topicDiscoveryAgent{infos: []AgentSessionInfo{{ID: "desktop"}}}}
	p := &topicArchivePlatform{topicSyncPlatform: topicSyncPlatform{target: "owner"}}
	e, w := newTopicSyncTest(t, a, p, filepath.Join(t.TempDir(), "sessions.json"))
	if err := w.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	key, journal := p.keys[0], w.path
	// The topic was reused before the newly selected conversation was archived.
	// Its original journal link must also stay retired after snapshot recovery.
	e.sessions.SwitchToAgentSession(key, "replacement", a.Name(), "replacement")
	a.archived = []string{"replacement"}
	p.afterDelete = func() { w.path = t.TempDir() }
	if err := w.poll(context.Background()); !errors.Is(err, errSessionTopicSyncPersistence) {
		t.Fatalf("save failure = %v", err)
	}
	w.path, p.afterDelete = journal, nil
	if err := w.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(p.deleted, []string{key}) {
		t.Fatalf("repeated successful delete: %v", p.deleted)
	}
	// Even a missing regular session snapshot must not restore the old link.
	if err := os.Remove(e.sessions.StorePath()); err != nil {
		t.Fatal(err)
	}
	e2, again := newTopicSyncTest(t, a, p, e.sessions.StorePath())
	if e2.sessions.ActiveSessionID(key) != "" {
		t.Fatal("rebound removed topic")
	}
	if err := again.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(p.keys) != 1 || len(p.deleted) != 1 {
		t.Fatalf("recreated or deleted again: %v %v", p.keys, p.deleted)
	}
}

func TestSessionTopicSync_ArchiveRetriesBusyTopicAfterTurnFinishes(t *testing.T) {
	a := &topicArchiveAgent{archived: []string{"bot"}}
	p := &topicArchivePlatform{topicSyncPlatform: topicSyncPlatform{target: "owner"}}
	e, w := newTopicSyncTest(t, a, p, filepath.Join(t.TempDir(), "sessions.json"))
	s := e.sessions.SwitchToAgentSession("test:owner:bot-topic", "bot", a.Name(), "")
	gen, _ := s.TryLock()
	if err := w.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(p.deleted) != 0 {
		t.Fatal("deleted busy topic")
	}
	s.Unlock(gen)
	if err := w.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(p.deleted) != 1 {
		t.Fatalf("did not retry idle topic: %v", p.deleted)
	}
}

func TestSessionTopicSync_ExternalArchiveRemovesTopicAcrossRestart(t *testing.T) {
	a := &topicArchiveAgent{topicDiscoveryAgent: topicDiscoveryAgent{infos: []AgentSessionInfo{{ID: "desktop"}}}}
	p := &topicArchivePlatform{topicSyncPlatform: topicSyncPlatform{target: "owner"}}
	path := filepath.Join(t.TempDir(), "sessions.json")
	e, w := newTopicSyncTest(t, a, p, path)
	if err := w.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	key := p.keys[0]
	// Archiving elsewhere while the bridge is offline must still remove its topic.
	if err := e.Stop(); err != nil {
		t.Fatal(err)
	}
	a.infos, a.archived = nil, []string{"desktop"}
	_, restarted := newTopicSyncTest(t, a, p, path)
	if err := restarted.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(p.deleted, []string{key}) {
		t.Fatalf("deleted topics = %v, want %s", p.deleted, key)
	}
	// Successful cleanup is durable and stale discovery cannot recreate the topic.
	a.infos = []AgentSessionInfo{{ID: "desktop"}}
	_, again := newTopicSyncTest(t, a, p, path)
	if err := again.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(p.deleted) != 1 || len(p.keys) != 1 {
		t.Fatalf("repeated cleanup: deletes=%v creates=%v", p.deleted, p.keys)
	}
}

func TestSessionTopicSync_ExternalArchiveRetriesDeletionAndIncludesBotTopics(t *testing.T) {
	a := &topicArchiveAgent{archived: []string{"bot-session"}}
	p := &topicArchivePlatform{topicSyncPlatform: topicSyncPlatform{target: "owner"}, deleteErr: errors.New("offline")}
	path := filepath.Join(t.TempDir(), "sessions.json")
	e, w := newTopicSyncTest(t, a, p, path)
	key := "test:owner:bot-topic"
	e.sessions.SwitchToAgentSession(key, "bot-session", a.Name(), "Bot conversation")
	if err := w.poll(context.Background()); err == nil {
		t.Fatal("ignored delete failure")
	}
	p.deleteErr = nil
	_, restarted := newTopicSyncTest(t, a, p, path)
	if err := restarted.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(p.deleted, []string{key}) {
		t.Fatalf("deleted topics = %v", p.deleted)
	}
}

func TestSessionTopicSync_ExternalArchiveProtectsChangedAndBusyTopics(t *testing.T) {
	for _, action := range []string{"new", "switch", "busy", "missing", "source error", "journal error"} {
		t.Run(action, func(t *testing.T) {
			a := &topicArchiveAgent{topicDiscoveryAgent: topicDiscoveryAgent{infos: []AgentSessionInfo{{ID: "desktop"}}}}
			p := &topicArchivePlatform{topicSyncPlatform: topicSyncPlatform{target: "owner"}}
			e, w := newTopicSyncTest(t, a, p, filepath.Join(t.TempDir(), "sessions.json"))
			if err := w.poll(context.Background()); err != nil {
				t.Fatal(err)
			}
			key := p.keys[0]
			a.infos, a.archived = nil, []string{"desktop"}
			switch action {
			case "new":
				e.sessions.NewSession(key, "new")
			case "switch":
				e.sessions.SwitchToAgentSession(key, "other", a.Name(), "other")
			case "busy":
				e.sessions.GetOrCreateActive(key).TryLock()
			case "missing":
				a.archived = nil
			case "source error":
				a.archiveErr = errors.New("unavailable")
			case "journal error":
				w.path = t.TempDir()
			}
			err := w.poll(context.Background())
			if (action == "source error" || action == "journal error") && err == nil {
				t.Fatal("ignored failure")
			}
			if len(p.deleted) != 0 {
				t.Fatalf("deleted protected topic: %v", p.deleted)
			}
		})
	}
}
