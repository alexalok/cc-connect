package core

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type cujMessageDiscoveryAgent struct {
	cujTopicDiscoveryAgent
	messages []SessionMessage
}

func (a *cujMessageDiscoveryAgent) ValidateSessionMessageSync() error { return nil }
func (a *cujMessageDiscoveryAgent) ReadSessionMessages(context.Context, string) ([]SessionMessage, error) {
	return a.messages, nil
}

// This subcase belongs to B13: a real engine and persisted session store bridge
// a desktop conversation into a topic, then accept input from both clients.
func testCUJExternalConversationMirroring(t *testing.T) {
	testCUJConversationMirroring(t, "owner", "", []string{"owner"})
}

func testCUJConversationMirroring(t *testing.T, destination, keyPrefix string, users []string) {
	a := &cujMessageDiscoveryAgent{
		cujTopicDiscoveryAgent: cujTopicDiscoveryAgent{cujDirectoryAgent: cujDirectoryAgent{
			defaultWorkDir: "/default", workDirs: map[string]string{"desktop-chat": "/desktop/project"},
			sessions: []AgentSessionInfo{{ID: "desktop-chat", Summary: "Desktop conversation"}},
		}},
		messages: []SessionMessage{{ID: "u1", Role: "user", Content: "Original desktop question"}, {ID: "a1", Role: "assistant", Content: "Original desktop answer"}},
	}
	p := &topicSyncPlatform{stubPlatformEngine: stubPlatformEngine{n: "test"}, target: destination, keyPrefix: keyPrefix}
	path := filepath.Join(t.TempDir(), "sessions.json")
	e, worker := newTopicSyncTest(t, a, p, path)
	poll := func(w *sessionTopicSync) {
		t.Helper()
		if err := w.poll(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	poll(worker)
	visible := strings.Join(p.getSent(), "\n")
	if !strings.Contains(visible, "Original desktop question") || !strings.Contains(visible, "Original desktop answer") {
		t.Fatalf("new topic is empty: %s", visible)
	}
	key := p.keys[0]
	env := &cujEnv{t: t, engine: e, plat: &p.stubPlatformEngine}
	actions := 0
	send := func(engine *Engine, text string) {
		p.clearSent()
		user := users[actions%len(users)]
		actions++
		engine.ReceiveMessage(p, &Message{SessionKey: key, Platform: "test", UserID: user, Content: text, ReplyCtx: key})
	}
	// Action 1: inspect the conversation selected by the imported topic.
	send(e, "/current")
	if !env.sentContains("desktop-chat") {
		t.Fatalf("wrong topic binding: %v", p.getSent())
	}
	// Desktop activity remains visible without issuing a bot command.
	p.clearSent()
	a.messages = append(a.messages, SessionMessage{ID: "a2", Role: "assistant", Content: "An ongoing desktop reply"})
	poll(worker)
	if got := p.getSent(); len(got) != 1 || !strings.Contains(got[0], "An ongoing desktop reply") {
		t.Fatalf("new desktop reply not mirrored once: %v", got)
	}
	// Action 2: continue through Telegram using the same native conversation.
	send(e, "Continue from the bot")
	env.waitFor("bot continuation", 3*time.Second, func() bool {
		return env.sentContains("session=desktop-chat cwd=/desktop/project") && !env.activeSession(key).Busy()
	})
	if err := e.Stop(); err != nil {
		t.Fatal(err)
	}
	p.clearSent()
	e2, restarted := newTopicSyncTest(t, a, p, path)
	poll(restarted)
	if len(p.getSent()) != 0 {
		t.Fatalf("restart replayed delivered conversation: %v", p.getSent())
	}
	// Action 3: the topic still selects the native chat after a bridge restart.
	send(e2, "/current")
	if !env.sentContains("desktop-chat") {
		t.Fatalf("native chat selection missing: %v", p.getSent())
	}
	p.clearSent()
	a.messages = append(a.messages, SessionMessage{ID: "u2", Role: "user", Content: "Desktop follow-up after restart"})
	poll(restarted)
	if got := p.getSent(); len(got) != 1 || !strings.Contains(got[0], "Desktop follow-up after restart") {
		t.Fatalf("restart lost ongoing sync: %v", got)
	}
	// Action 4: intentionally reusing this topic must stop the old chat feed.
	send(e2, "/new")
	if !env.sentContains(e2.i18n.T(MsgNewSessionCreated)) {
		t.Fatalf("new session not created: %v", p.getSent())
	}
	p.clearSent()
	a.messages = append(a.messages, SessionMessage{ID: "a3", Role: "assistant", Content: "Old chat must not enter new work"})
	poll(restarted)
	if len(p.getSent()) != 0 {
		t.Fatalf("old conversation leaked into /new: %v", p.getSent())
	}
}
