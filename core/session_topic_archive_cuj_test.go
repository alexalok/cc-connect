package core

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type cujArchiveDiscoveryAgent struct {
	cujTopicDiscoveryAgent
	archived []string
}

func (a *cujArchiveDiscoveryAgent) ArchivedSessionIDs(context.Context, []string) ([]string, error) {
	return a.archived, nil
}

func testCUJExternalTopicArchive(t *testing.T) {
	for _, offline := range []bool{false, true} {
		t.Run(fmt.Sprint("offline=", offline), func(t *testing.T) { testCUJExternalTopicArchiveWithDowntime(t, offline) })
	}
}

func testCUJExternalTopicArchiveWithDowntime(t *testing.T, offline bool) {
	a := &cujArchiveDiscoveryAgent{cujTopicDiscoveryAgent: cujTopicDiscoveryAgent{cujDirectoryAgent: cujDirectoryAgent{
		defaultWorkDir: "/default", workDirs: map[string]string{"desktop-chat": "/desktop/project"},
		sessions: []AgentSessionInfo{{ID: "desktop-chat", Summary: "Desktop conversation"}},
	}}}
	p := &topicArchivePlatform{topicSyncPlatform: topicSyncPlatform{stubPlatformEngine: stubPlatformEngine{n: "test"}, target: "owner"}}
	path := filepath.Join(t.TempDir(), "sessions.json")
	e, worker := newTopicSyncTest(t, a, p, path)
	if err := worker.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	key := p.keys[0]
	env := &cujEnv{t: t, engine: e, plat: &p.stubPlatformEngine}
	send := func(content string) {
		p.clearSent()
		e.ReceiveMessage(p, &Message{SessionKey: key, Platform: "test", UserID: "owner", Content: content, ReplyCtx: key})
	}
	// The user inspects and continues the same chat from its Telegram topic.
	send("/current")
	if !env.sentContains("desktop-chat") {
		t.Fatalf("wrong current chat: %v", p.getSent())
	}
	send("/history")
	if !env.sentContains("Original desktop question") {
		t.Fatalf("missing history: %v", p.getSent())
	}
	send("Continue this work")
	env.waitFor("continuation", 3*time.Second, func() bool {
		return env.sentContains("session=desktop-chat cwd=/desktop/project") && !env.activeSession(key).Busy()
	})
	// The user archives it in the desktop app, with or without bridge downtime.
	if offline {
		if err := e.Stop(); err != nil {
			t.Fatal(err)
		}
	}
	a.sessions, a.archived = nil, []string{"desktop-chat"}
	p.clearSent()
	restarted := worker
	if offline {
		_, restarted = newTopicSyncTest(t, a, p, path)
	}
	if err := restarted.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := p.getSent(); len(got) != 1 || !strings.Contains(got[0], "deleted: "+key) {
		t.Fatalf("archived topic remained visible: %v", got)
	}
	p.clearSent()
	_, again := newTopicSyncTest(t, a, p, path)
	if err := again.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := p.getSent(); len(got) != 0 {
		t.Fatalf("cleanup repeated after restart: %v", got)
	}
}
