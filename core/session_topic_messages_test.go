package core

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type topicMessageAgent struct {
	topicDiscoveryAgent
	messages      []SessionMessage
	validationErr error
}

func TestSessionTopicSync_UpgradesTopicOnlyJournalWithoutResettingCutoff(t *testing.T) {
	a := &topicDiscoveryAgent{infos: []AgentSessionInfo{{ID: "native", Summary: "Existing empty topic"}}}
	p := &topicSyncPlatform{target: "owner"}
	path := filepath.Join(t.TempDir(), "sessions.json")
	_, original := newTopicSyncTest(t, a, p, path)
	if err := original.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	cutoff := original.state.Since
	p.clearSent()
	upgraded := &topicMessageAgent{topicDiscoveryAgent: *a, messages: []SessionMessage{{ID: "old-message", Role: "assistant", Content: "Catch up the already linked conversation"}}}
	_, worker := newTopicSyncTest(t, upgraded, p, path)
	if err := worker.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(p.keys) != 1 || !worker.state.Since.Equal(cutoff) {
		t.Fatal("upgrade reset cutoff or created a duplicate topic")
	}
	if got := p.getSent(); len(got) != 1 || !strings.Contains(got[0], "Catch up") {
		t.Fatalf("upgrade did not fill the existing topic: %v", got)
	}
}

func TestSessionTopicSync_DeliveryFailureResumesAtFirstUnsentChunk(t *testing.T) {
	a := &topicMessageAgent{
		topicDiscoveryAgent: topicDiscoveryAgent{infos: []AgentSessionInfo{{ID: "native"}}},
		messages:            []SessionMessage{{ID: "long", Role: "assistant", Content: strings.Repeat("A long visible reply. ", 650)}},
	}
	p := &topicMessagePlatform{topicSyncPlatform: topicSyncPlatform{target: "owner"}, failAt: 2}
	path := filepath.Join(t.TempDir(), "sessions.json")
	_, worker := newTopicSyncTest(t, a, p, path)
	if err := worker.poll(context.Background()); err == nil {
		t.Fatal("delivery failure was ignored")
	}
	_, restarted := newTopicSyncTest(t, a, p, path)
	if err := restarted.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	var actual []string
	for _, sent := range p.getSent() {
		if !strings.HasPrefix(sent, "topic:") {
			actual = append(actual, sent)
		}
	}
	want := SplitMessageCodeFenceAware(restarted.formatMirroredMessage(a.messages[0]), maxPlatformMessageLen)
	if strings.Join(actual, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("retried chunks differ: got %d, want %d", len(actual), len(want))
	}
	for _, key := range p.destinations {
		if key != p.keys[0] {
			t.Fatalf("message went to wrong topic: %s", key)
		}
	}
}

func TestSessionTopicSync_DoesNotMirrorAfterNewSwitchOrDelete(t *testing.T) {
	for _, action := range []string{"new", "switch", "delete"} {
		t.Run(action, func(t *testing.T) {
			a := &topicMessageAgent{topicDiscoveryAgent: topicDiscoveryAgent{infos: []AgentSessionInfo{{ID: "native"}}}}
			p := &topicSyncPlatform{target: "owner"}
			e, worker := newTopicSyncTest(t, a, p, filepath.Join(t.TempDir(), "sessions.json"))
			if err := worker.poll(context.Background()); err != nil {
				t.Fatal(err)
			}
			key := p.keys[0]
			switch action {
			case "new":
				e.sessions.NewSession(key, "New work")
			case "switch":
				e.sessions.SwitchToAgentSession(key, "other", a.Name(), "Other")
			case "delete":
				e.sessions.DeleteByAgentSessionID("native")
			}
			p.clearSent()
			a.messages = []SessionMessage{{ID: "later", Role: "assistant", Content: "Should stay in the original chat"}}
			if err := worker.poll(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(p.getSent()) != 0 {
				t.Fatalf("mirrored into a retired topic binding: %v", p.getSent())
			}
		})
	}
}

func TestSessionTopicSync_RejectsInvalidDeliveryProgress(t *testing.T) {
	a := &topicMessageAgent{topicDiscoveryAgent: topicDiscoveryAgent{infos: []AgentSessionInfo{{ID: "native"}}}}
	p := &topicSyncPlatform{target: "owner"}
	e, worker := newTopicSyncTest(t, a, p, filepath.Join(t.TempDir(), "sessions.json"))
	worker.state.Delivered = map[string]map[string]int{"missing-topic": {"message": -1}}
	data, err := json.Marshal(worker.state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(worker.path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := e.prepareSessionTopicSync(); err == nil {
		t.Fatal("invalid delivery progress was silently reset")
	}
}

func (a *topicMessageAgent) ValidateSessionMessageSync() error { return a.validationErr }
func (a *topicMessageAgent) ReadSessionMessages(context.Context, string) ([]SessionMessage, error) {
	return a.messages, nil
}

func (p *topicSyncPlatform) ReconstructReplyCtx(key string) (any, error) { return key, nil }

type topicMessagePlatform struct {
	topicSyncPlatform
	attempts     int
	failAt       int
	destinations []string
}

func (p *topicMessagePlatform) Send(ctx context.Context, reply any, text string) error {
	p.attempts++
	if p.attempts == p.failAt {
		return errors.New("temporary delivery failure")
	}
	p.destinations = append(p.destinations, reply.(string))
	return p.stubPlatformEngine.Send(ctx, reply, text)
}

func TestSessionTopicSync_MirrorsMessagesIntoPreviouslyEmptyTopic(t *testing.T) {
	a := &topicMessageAgent{
		topicDiscoveryAgent: topicDiscoveryAgent{infos: []AgentSessionInfo{{ID: "native", Summary: "Desktop chat"}}},
		messages:            []SessionMessage{{ID: "u1", Role: "user", Content: "Original desktop question"}, {ID: "a1", Role: "assistant", Content: "Original desktop answer"}},
	}
	p := &topicSyncPlatform{target: "owner"}
	path := filepath.Join(t.TempDir(), "sessions.json")
	_, w := newTopicSyncTest(t, a, p, path)
	if err := w.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	visible := strings.Join(p.getSent(), "\n")
	if !strings.Contains(visible, "Original desktop question") || !strings.Contains(visible, "Original desktop answer") {
		t.Fatalf("topic must contain the conversation without /history: %s", visible)
	}
	p.clearSent()
	_, restarted := newTopicSyncTest(t, a, p, path)
	if err := restarted.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(p.getSent()) != 0 {
		t.Fatalf("restart repeated messages: %v", p.getSent())
	}
	a.messages = append(a.messages, SessionMessage{ID: "a2", Role: "assistant", Content: "New desktop reply"})
	if err := restarted.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := p.getSent(); len(got) != 1 || !strings.Contains(got[0], "New desktop reply") {
		t.Fatalf("new desktop message was not mirrored exactly once: %v", got)
	}
}
