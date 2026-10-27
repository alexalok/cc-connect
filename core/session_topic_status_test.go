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
	for _, action := range []string{"idle", "busy", "switch", "delete"} {
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
