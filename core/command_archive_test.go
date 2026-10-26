package core

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

type archiveCommandAgent struct {
	Agent
	archive func(string) error
}

func (a *archiveCommandAgent) ArchiveSession(_ context.Context, id string) error {
	return a.archive(id)
}

type archiveTopicPlatform struct {
	stubPlatformEngine
	deleteTopic func(any) (bool, error)
}

func (p *archiveTopicPlatform) DeleteTopic(_ context.Context, replyCtx any) (bool, error) {
	return p.deleteTopic(replyCtx)
}

func TestCmdArchive_ArchivesBeforeDeletingTopicAndPreservesHistory(t *testing.T) {
	var actions []string
	running := newCUJAgentSession()
	a := &archiveCommandAgent{Agent: &stubAgent{}, archive: func(id string) error {
		if running.Alive() {
			t.Error("agent must be stopped before archiving")
		}
		actions = append(actions, "archive:"+id)
		return nil
	}}
	p := &archiveTopicPlatform{stubPlatformEngine: stubPlatformEngine{n: "test"}, deleteTopic: func(ctx any) (bool, error) {
		actions = append(actions, "delete:"+ctx.(string))
		return true, nil
	}}
	storePath := filepath.Join(t.TempDir(), "sessions.json")
	e := NewEngine("test", a, []Platform{p}, storePath, LangEnglish)
	t.Cleanup(func() { _ = e.Stop() })
	const key = "test:topic"
	s := e.sessions.GetOrCreateActive(key)
	s.SetAgentInfo("thread-1", a.Name(), "Keep this conversation")
	s.AddHistory("user", "important history")
	e.interactiveStates[key] = &interactiveState{agentSession: running}
	e.ReceiveMessage(p, &Message{SessionKey: key, Content: "/archive", ReplyCtx: "topic-1"})
	if want := []string{"archive:thread-1", "delete:topic-1"}; !reflect.DeepEqual(actions, want) {
		t.Fatalf("actions = %v, want %v", actions, want)
	}
	if len(p.getSent()) != 0 {
		t.Fatalf("must not reply into a deleted topic: %v", p.getSent())
	}
	reloaded := NewSessionManager(storePath)
	archived := reloaded.FindByID(s.ID)
	if archived == nil || !archived.IsArchived() || archived.GetAgentSessionID() != "thread-1" {
		t.Fatal("native ID and archive state must survive restart")
	}
	if history := archived.GetHistory(0); len(history) != 1 || history[0].Content != "important history" {
		t.Fatalf("archived history = %+v", history)
	}
	if next := reloaded.GetOrCreateActive(key); next.ID == s.ID || next.GetAgentSessionID() != "" {
		t.Fatal("next message must not resume the archived session")
	}
}

func TestCmdArchive_ArchiveFailureKeepsTopicAndSession(t *testing.T) {
	a := &archiveCommandAgent{Agent: &stubAgent{}, archive: func(string) error { return errors.New("archive failed") }}
	p := &archiveTopicPlatform{stubPlatformEngine: stubPlatformEngine{n: "test"}, deleteTopic: func(any) (bool, error) {
		t.Fatal("topic must survive a failed archive")
		return false, nil
	}}
	e := NewEngine("test", a, []Platform{p}, "", LangEnglish)
	t.Cleanup(func() { _ = e.Stop() })
	s := e.sessions.GetOrCreateActive("test:topic")
	s.SetAgentSessionID("thread-1", a.Name())
	s.AddHistory("user", "keep me")
	e.ReceiveMessage(p, &Message{SessionKey: "test:topic", Content: "/archive"})
	if s.IsArchived() || e.sessions.GetOrCreateActive("test:topic") != s || s.HistoryLen() != 1 {
		t.Fatal("failed archive changed the session")
	}
	if got := p.getSent(); len(got) != 1 || !strings.Contains(got[0], "topic has been kept") {
		t.Fatalf("reply = %v", got)
	}
}

func TestCmdArchive_TopicFailureCanBeRetriedAfterRestart(t *testing.T) {
	archives, deletes := 0, 0
	a := &archiveCommandAgent{Agent: &stubAgent{}, archive: func(string) error { archives++; return nil }}
	p := &archiveTopicPlatform{stubPlatformEngine: stubPlatformEngine{n: "test"}, deleteTopic: func(any) (bool, error) {
		deletes++
		if deletes == 1 {
			return false, errors.New("permission denied")
		}
		return true, nil
	}}
	path := filepath.Join(t.TempDir(), "sessions.json")
	e := NewEngine("test", a, []Platform{p}, path, LangEnglish)
	t.Cleanup(func() { _ = e.Stop() })
	s := e.sessions.GetOrCreateActive("test:topic")
	s.SetAgentSessionID("thread-1", a.Name())
	msg := &Message{SessionKey: "test:topic", Content: "/archive"}
	e.ReceiveMessage(p, msg)
	if got := p.getSent(); len(got) != 1 || !strings.Contains(got[0], "Session archived, but") {
		t.Fatalf("partial failure reply = %v", got)
	}
	e.sessions = NewSessionManager(path)
	p.clearSent()
	e.ReceiveMessage(p, msg)
	if archives != 1 || deletes != 2 || len(p.getSent()) != 0 {
		t.Fatalf("retry: archive calls=%d, delete calls=%d, replies=%v", archives, deletes, p.getSent())
	}
}

func TestCmdArchive_UnsupportedEmptyInvalidAndDisabled(t *testing.T) {
	for _, tc := range []struct {
		name, command                  string
		unsupported, started, disabled bool
		want                           MsgKey
	}{
		{name: "unsupported", command: "/archive", unsupported: true, started: true, want: MsgArchiveNotSupported},
		{name: "empty", command: "/archive", want: MsgArchiveNoSession},
		{name: "arguments", command: "/archive 2", started: true, want: MsgArchiveUsage},
		{name: "disabled", command: "/archive", started: true, disabled: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var a Agent = &archiveCommandAgent{Agent: &stubAgent{}, archive: func(string) error {
				t.Fatal("archive must not run")
				return nil
			}}
			if tc.unsupported {
				a = &stubAgent{}
			}
			p := &archiveTopicPlatform{stubPlatformEngine: stubPlatformEngine{n: "test"}, deleteTopic: func(any) (bool, error) {
				t.Fatal("topic must not be deleted")
				return false, nil
			}}
			e := NewEngine("test", a, []Platform{p}, "", LangEnglish)
			t.Cleanup(func() { _ = e.Stop() })
			if tc.started {
				e.sessions.GetOrCreateActive("test:topic").SetAgentSessionID("thread-1", a.Name())
			}
			if tc.disabled {
				e.SetDisabledCommands([]string{"archive"})
			}
			e.ReceiveMessage(p, &Message{SessionKey: "test:topic", Content: tc.command})
			if tc.want != "" {
				if got := p.getSent(); len(got) != 1 || got[0] != e.i18n.T(tc.want) {
					t.Fatalf("reply = %v", got)
				}
			}
		})
	}
}

func TestCmdArchive_WithoutTopicReportsSuccess(t *testing.T) {
	a := &archiveCommandAgent{Agent: &stubAgent{}, archive: func(string) error { return nil }}
	p := &stubPlatformEngine{n: "test"}
	e := NewEngine("test", a, []Platform{p}, "", LangEnglish)
	t.Cleanup(func() { _ = e.Stop() })
	e.sessions.GetOrCreateActive("test:chat").SetAgentSessionID("thread-1", a.Name())
	e.ReceiveMessage(p, &Message{SessionKey: "test:chat", Content: "/archive"})
	if got := p.getSent(); len(got) != 1 || got[0] != e.i18n.T(MsgArchiveSuccess) {
		t.Fatalf("reply = %v", got)
	}
}

func TestCmdArchive_ConcurrentMessagesCannotResumeDuringArchive(t *testing.T) {
	for _, stage := range []string{"archive", "delete topic"} {
		t.Run(stage, func(t *testing.T) {
			entered, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
			block := func() { close(entered); <-release }
			a := &archiveCommandAgent{Agent: &controllableAgent{startSessionFn: func(context.Context, string) (AgentSession, error) {
				t.Error("a new turn must not start while archiving")
				return nil, errors.New("unexpected start")
			}}, archive: func(string) error {
				if stage == "archive" {
					block()
				}
				return nil
			}}
			p := &archiveTopicPlatform{stubPlatformEngine: stubPlatformEngine{n: "test"}, deleteTopic: func(any) (bool, error) {
				if stage == "delete topic" {
					block()
				}
				return true, nil
			}}
			e := NewEngine("test", a, []Platform{p}, "", LangEnglish)
			t.Cleanup(func() { _ = e.Stop() })
			e.sessions.GetOrCreateActive("test:topic").SetAgentSessionID("thread-1", a.Name())
			go func() {
				defer close(finished)
				e.ReceiveMessage(p, &Message{SessionKey: "test:topic", Content: "/archive"})
			}()
			select {
			case <-entered:
			case <-time.After(2 * time.Second):
				t.Fatal("archive did not reach blocking operation")
			}
			e.ReceiveMessage(p, &Message{SessionKey: "test:topic", Content: "another task"})
			e.ReceiveMessage(p, &Message{SessionKey: "test:topic", Content: "/archive"})
			e.ReceiveMessage(p, &Message{SessionKey: "test:topic", Content: "/new"})
			got := p.getSent()
			close(release)
			select {
			case <-finished:
			case <-time.After(2 * time.Second):
				t.Fatal("archive did not finish")
			}
			if len(got) != 3 {
				t.Fatalf("concurrent replies = %v", got)
			}
			for _, reply := range got {
				if reply != e.i18n.T(MsgArchiveInProgress) {
					t.Fatalf("concurrent replies = %v", got)
				}
			}
		})
	}
}

func TestCmdArchive_UnconfirmedStopKeepsTopic(t *testing.T) {
	a := &archiveCommandAgent{Agent: &stubAgent{}, archive: func(string) error {
		t.Fatal("must not archive a session whose process may still be running")
		return nil
	}}
	p := &archiveTopicPlatform{stubPlatformEngine: stubPlatformEngine{n: "test"}, deleteTopic: func(any) (bool, error) {
		t.Fatal("must not delete topic after a failed stop")
		return false, nil
	}}
	e := NewEngine("test", a, []Platform{p}, "", LangEnglish)
	t.Cleanup(func() { _ = e.Stop() })
	e.sessions.GetOrCreateActive("test:topic").SetAgentSessionID("thread-1", a.Name())
	running := newControllableSession("thread-1")
	running.closeErr = errors.New("could not stop process")
	e.interactiveStates["test:topic"] = &interactiveState{agentSession: running, agent: a}
	e.ReceiveMessage(p, &Message{SessionKey: "test:topic", Content: "/archive"})
	if got := p.getSent(); len(got) != 1 || got[0] != e.i18n.T(MsgArchiveStopFailed) {
		t.Fatalf("reply = %v", got)
	}
}
