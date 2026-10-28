package core

import (
	"context"
	"path/filepath"
	"testing"
)

type nativeStatusAgent struct {
	topicMessageAgent
	status ConversationStatus
	read   func()
}

func (a *nativeStatusAgent) ReadSessionStatus(context.Context, string) (ConversationStatus, error) {
	if a.read != nil {
		a.read()
	}
	return a.status, nil
}

type nativeStatusPlatform struct {
	topicSyncPlatform
	status       ConversationStatus
	statusTarget any
}

func (p *nativeStatusPlatform) SetConversationStatus(_ context.Context, target any, status ConversationStatus) {
	p.status = status
	p.statusTarget = target
}

func TestSessionTopicStatus_SyncsOnlySelectedIdleNativeSession(t *testing.T) {
	for _, action := range []string{"idle", "busy", "switch", "delete", "archiving", "archived"} {
		t.Run(action, func(t *testing.T) {
			a := &nativeStatusAgent{topicMessageAgent: topicMessageAgent{topicDiscoveryAgent: topicDiscoveryAgent{infos: []AgentSessionInfo{{ID: "native", Summary: "Native"}}}}, status: ConversationQuestion}
			p := &nativeStatusPlatform{topicSyncPlatform: topicSyncPlatform{target: "owner"}}
			e, w := newTopicSyncTest(t, a, p, filepath.Join(t.TempDir(), "sessions.json"))
			if err := w.poll(context.Background()); err != nil {
				t.Fatal(err)
			}
			if p.status != ConversationQuestion || p.statusTarget != p.keys[0] {
				t.Fatal("native question not shown in matching topic")
			}
			p.status = ""
			a.read = func() {
				switch action {
				case "busy":
					e.sessions.GetOrCreateActive(p.keys[0]).TryLock()
				case "switch":
					e.sessions.NewSession(p.keys[0], "")
				case "archiving", "archived":
					session := e.sessions.GetOrCreateActive(p.keys[0])
					session.mu.Lock()
					session.archiving = action == "archiving"
					session.Archived = action == "archived"
					session.mu.Unlock()
				case "delete":
					e.sessions.mu.Lock()
					e.sessions.deletedAgentSessionIDs = map[string]bool{"native": true}
					e.sessions.mu.Unlock()
				}
			}
			if err := w.poll(context.Background()); err != nil {
				t.Fatal(err)
			}
			if action == "idle" {
				if p.status != ConversationQuestion {
					t.Fatal("idle native status missing")
				}
			} else if p.status != "" {
				t.Fatalf("stale native status escaped %s guard", action)
			}
		})
	}
}

func TestSessionTopicStatus_PreservesQuestionAfterInterveningBridgeTurn(t *testing.T) {
	for _, mode := range []string{"same session", "replacement session"} {
		t.Run(mode, func(t *testing.T) {
			a := &nativeStatusAgent{topicMessageAgent: topicMessageAgent{topicDiscoveryAgent: topicDiscoveryAgent{infos: []AgentSessionInfo{{ID: "native", Summary: "Native"}}}}, status: ConversationFinished}
			p := &nativeStatusPlatform{topicSyncPlatform: topicSyncPlatform{target: "owner"}}
			e, w := newTopicSyncTest(t, a, p, filepath.Join(t.TempDir(), "sessions.json"))
			ctx := context.Background()
			if err := w.poll(ctx); err != nil {
				t.Fatal(err)
			}
			key := p.keys[0]
			selected := e.sessions.GetOrCreateActive(key)
			// The replacement below deliberately reaches the same generation:
			// checking the session pointer is necessary as well as its counter.
			gen, _ := selected.TryLock()
			selected.Unlock(gen)
			a.read = func() {
				session := selected
				if mode == "replacement session" {
					session = e.sessions.NewSession(key, "")
					session.SetAgentSessionID("native", a.Name())
				}
				turnGen, locked := session.TryLock()
				if !locked {
					t.Fatal("intervening turn could not start")
				}
				state := &interactiveState{platform: p, replyCtx: key, pendingUserInput: &pendingUserInput{}}
				e.updateConversationStatusForTurn(state, session, key, turnGen, ConversationFinished)
				session.Unlock(turnGen)
			}
			if err := w.poll(ctx); err != nil {
				t.Fatal(err)
			}
			if p.status != ConversationQuestion {
				t.Fatalf("native snapshot overwrote the newer bridge question: %s", p.status)
			}
			// Later transcript reads identify the bridge-owned turn and emit no
			// status, so they cannot repair an incorrect earlier publication.
			a.read = nil
			a.status = ""
			if err := w.poll(ctx); err != nil {
				t.Fatal(err)
			}
			if p.status != ConversationQuestion {
				t.Fatal("pending bridge question disappeared on the next poll")
			}
		})
	}
}
