package core

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
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
	for _, command := range []string{"/current", "/status", "/history", "/list"} {
		e.ReceiveMessage(p, &Message{SessionKey: msg.SessionKey, Content: command})
	}
	e.sessions = NewSessionManager(path)
	p.clearSent()
	e.ReceiveMessage(p, msg)
	if archives != 1 || deletes != 2 || len(p.getSent()) != 0 {
		t.Fatalf("retry: archive calls=%d, delete calls=%d, replies=%v", archives, deletes, p.getSent())
	}
}

func TestCmdArchive_AcceptedWorkerCannotStartAfterCleanup(t *testing.T) {
	for _, archiveFails := range []bool{false, true} {
		t.Run(fmt.Sprint("archive failure=", archiveFails), func(t *testing.T) {
			starts := 0
			a := &archiveCommandAgent{Agent: &controllableAgent{startSessionFn: func(context.Context, string) (AgentSession, error) {
				starts++
				return nil, errors.New("unexpected late start")
			}}, archive: func(string) error {
				if archiveFails {
					return errors.New("archive failed")
				}
				return nil
			}}
			p := &archiveTopicPlatform{stubPlatformEngine: stubPlatformEngine{n: "test"}, deleteTopic: func(any) (bool, error) { return true, nil }}
			e := NewEngine("test", a, []Platform{p}, "", LangEnglish)
			t.Cleanup(func() { _ = e.Stop() })
			const key = "test:topic"
			s := e.sessions.GetOrCreateActive(key)
			s.SetAgentSessionID("thread-1", a.Name())
			s.AddHistory("user", "original conversation")
			// Model a message accepted by handleMessage whose worker has not
			// been scheduled yet: its lock and queue placeholder already exist.
			gen, _ := s.TryLock()
			e.ensureInteractiveStateForQueueing(key, p, nil)
			e.ReceiveMessage(p, &Message{SessionKey: key, Content: "/archive"})
			p.clearSent()
			e.processInteractiveMessageWith(p, &Message{SessionKey: key, Content: "late worker"}, s, a, e.sessions, key, "", key, gen)
			if starts != 0 || len(p.getSent()) != 0 || s.HistoryLen() != 1 {
				t.Fatalf("late worker survived archive cleanup: starts=%d replies=%v history=%v", starts, p.getSent(), s.GetHistory(0))
			}
		})
	}
}

func TestCmdArchive_WorkerWaitingForStartupCannotPassArchive(t *testing.T) {
	var starts atomic.Int32
	a := &archiveCommandAgent{Agent: &controllableAgent{startSessionFn: func(context.Context, string) (AgentSession, error) {
		starts.Add(1)
		return nil, errors.New("unexpected late start")
	}}, archive: func(string) error { return nil }}
	p := &archiveTopicPlatform{stubPlatformEngine: stubPlatformEngine{n: "test"}, deleteTopic: func(any) (bool, error) { return true, nil }}
	e := NewEngine("test", a, []Platform{p}, "", LangEnglish)
	t.Cleanup(func() { _ = e.Stop() })
	const key = "test:topic"
	s := e.sessions.GetOrCreateActive(key)
	s.SetAgentSessionID("thread-1", a.Name())
	gen, _ := s.TryLock()
	// Hold startup until the worker has passed its initial cancellation
	// check. Archiving must also be checked under this mutex before spawn.
	e.interactiveMu.Lock()
	locked := true
	defer func() {
		if locked {
			e.interactiveMu.Unlock()
		}
	}()
	workerDone, archiveDone := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(workerDone)
		e.processInteractiveMessageWith(p, &Message{SessionKey: key, Content: "accepted message"}, s, a, e.sessions, key, "", key, gen)
	}()
	deadline := time.Now().Add(2 * time.Second)
	for s.HistoryLen() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if s.HistoryLen() == 0 {
		t.Fatal("worker did not reach startup")
	}
	go func() {
		defer close(archiveDone)
		e.ReceiveMessage(p, &Message{SessionKey: key, Content: "/archive"})
	}()
	for !s.isArchiving() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !s.isArchiving() {
		t.Fatal("archive did not begin")
	}
	e.interactiveMu.Unlock()
	locked = false
	for _, done := range []chan struct{}{workerDone, archiveDone} {
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("worker or archive did not finish")
		}
	}
	if starts.Load() != 0 || len(p.getSent()) != 0 {
		t.Fatalf("worker started during archive: starts=%d replies=%v", starts.Load(), p.getSent())
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

func TestCmdArchive_WaitsForPreviouslyRequestedStop(t *testing.T) {
	closeComplete := make(chan struct{})
	a := &archiveCommandAgent{Agent: &stubAgent{}, archive: func(string) error {
		select {
		case <-closeComplete:
		default:
			t.Error("archive ran while the previous process was still closing")
		}
		return nil
	}}
	p := &archiveTopicPlatform{stubPlatformEngine: stubPlatformEngine{n: "test"}, deleteTopic: func(any) (bool, error) { return true, nil }}
	e := NewEngine("test", a, []Platform{p}, "", LangEnglish)
	t.Cleanup(func() { _ = e.Stop() })
	const key = "test:topic"
	e.sessions.GetOrCreateActive(key).SetAgentSessionID("thread-1", a.Name())
	finishClose := e.beginSessionClose(key)
	done := make(chan struct{})
	go func() {
		defer close(done)
		e.ReceiveMessage(p, &Message{SessionKey: key, Content: "/archive"})
	}()
	select {
	case <-done:
		t.Error("archive finished before the pending stop")
	case <-time.After(50 * time.Millisecond):
	}
	close(closeComplete)
	finishClose()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("archive did not finish after process shutdown")
	}
}

func TestCmdArchive_UsesSendBoundWorkspaceSession(t *testing.T) {
	const key = "test:topic"
	workspace := t.TempDir()
	running := newCUJAgentSession()
	global := &archiveCommandAgent{Agent: &stubAgent{}, archive: func(string) error {
		t.Fatal("must not archive the global conversation")
		return nil
	}}
	var archived string
	bound := &archiveCommandAgent{Agent: &stubAgent{}, archive: func(id string) error {
		if running.Alive() {
			t.Error("workspace agent must stop before archiving")
		}
		archived = id
		return nil
	}}
	deleted := false
	p := &archiveTopicPlatform{stubPlatformEngine: stubPlatformEngine{n: "test"}, deleteTopic: func(any) (bool, error) {
		deleted = true
		return true, nil
	}}
	e := NewEngine("test", global, []Platform{p}, "", LangEnglish)
	t.Cleanup(func() { _ = e.Stop() })
	old := e.sessions.GetOrCreateActive(key)
	old.SetAgentSessionID("old-global", global.Name())
	e.workspacePool = newWorkspacePool(DefaultWorkspaceIdleTimeout)
	ws := e.workspacePool.GetOrCreate(workspace)
	ws.agent, ws.sessions = bound, NewSessionManager("")
	selected := ws.sessions.GetOrCreateActive(key)
	selected.SetAgentSessionID("bound-session", bound.Name())
	e.bindSendWorkDir(key, workspace)
	e.interactiveStates[workspace+":"+key] = &interactiveState{agentSession: running, busySession: selected}
	e.ReceiveMessage(p, &Message{SessionKey: key, Content: "/archive"})
	if archived != "bound-session" || !deleted || !selected.IsArchived() || old.IsArchived() {
		t.Fatalf("archived=%q deleted=%v bound=%v global=%v", archived, deleted, selected.IsArchived(), old.IsArchived())
	}
}

type providerArchiveAgent struct {
	*archiveCommandAgent
	provider string
}

func (a *providerArchiveAgent) ArchiveSessionWithProvider(_ context.Context, id, provider string) error {
	a.provider = provider
	return a.archive(id)
}

func TestCmdArchive_PassesSavedProviderAfterRestart(t *testing.T) {
	a := &providerArchiveAgent{archiveCommandAgent: &archiveCommandAgent{Agent: &stubAgent{}, archive: func(string) error { return nil }}}
	p := &stubPlatformEngine{n: "test"}
	path := filepath.Join(t.TempDir(), "sessions.json")
	e := NewEngine("test", a, []Platform{p}, path, LangEnglish)
	t.Cleanup(func() { _ = e.Stop() })
	session := e.sessions.GetOrCreateActive("test:topic")
	session.SetAgentSessionID("saved-thread", a.Name())
	session.SetActiveProvider("saved-profile")
	e.sessions.Save()
	e.sessions = NewSessionManager(path)
	e.ReceiveMessage(p, &Message{SessionKey: "test:topic", Content: "/archive"})
	if a.provider != "saved-profile" {
		t.Fatalf("archive provider = %q, want saved-profile", a.provider)
	}
}
