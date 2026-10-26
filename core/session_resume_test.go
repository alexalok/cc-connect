package core

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestResumeFallbackPolicy_PreservesBindingWhenDisabled(t *testing.T) {
	for _, policy := range []string{"default", "enabled", "disabled"} {
		t.Run(policy, func(t *testing.T) {
			var calls []string
			agent := &controllableAgent{startSessionFn: func(_ context.Context, id string) (AgentSession, error) {
				calls = append(calls, id)
				if id != "" {
					return nil, errors.New("temporary resume failure")
				}
				return newControllableSession("fresh-id"), nil
			}}
			p := &stubPlatformEngine{n: "test"}
			store := filepath.Join(t.TempDir(), "sessions.json")
			e := NewEngine("test", agent, []Platform{p}, store, LangEnglish)
			t.Cleanup(e.cancel)
			if policy != "default" {
				e.SetResumeFallbackToNew(policy == "enabled")
			}
			const key = "test:user"
			session := e.sessions.GetOrCreateActive(key)
			session.SetAgentSessionID("saved-id", agent.Name())
			e.sessions.Save()

			state := e.getOrCreateInteractiveStateWith(key, p, "ctx", session, e.sessions, nil, "", 0)
			wantID, wantCalls := "fresh-id", []string{"saved-id", ""}
			if policy == "disabled" {
				wantID, wantCalls = "saved-id", []string{"saved-id"}
				if state.agentSession != nil || state.startFailureMsg != MsgSessionResumePreserved {
					t.Fatalf("failed resume must return a recoverable error without an agent: %+v", state)
				}
			} else if state.agentSession == nil {
				t.Fatal("default/enabled fallback did not start a fresh session")
			}
			if !reflect.DeepEqual(calls, wantCalls) {
				t.Fatalf("StartSession calls = %q, want %q", calls, wantCalls)
			}
			if got := session.GetAgentSessionID(); got != wantID {
				t.Fatalf("binding = %q, want %q", got, wantID)
			}
			if got := NewSessionManager(store).GetOrCreateActive(key).GetAgentSessionID(); got != wantID {
				t.Fatalf("persisted binding = %q, want %q", got, wantID)
			}
		})
	}
}

type resumePolicyValidatingAgent struct {
	controllableAgent
	valid bool
}

func (a *resumePolicyValidatingAgent) ValidateSessionID(context.Context, string) bool { return a.valid }

func TestResumeFallbackPolicy_ValidationFailurePreservesBinding(t *testing.T) {
	var calls []string
	agent := &resumePolicyValidatingAgent{}
	agent.startSessionFn = func(_ context.Context, id string) (AgentSession, error) {
		calls = append(calls, id)
		return newControllableSession(id), nil
	}
	p := &stubPlatformEngine{n: "test"}
	e := NewEngine("test", agent, []Platform{p}, "", LangEnglish)
	t.Cleanup(e.cancel)
	e.SetResumeFallbackToNew(false)
	const key = "test:user"
	session := e.sessions.GetOrCreateActive(key)
	session.SetAgentSessionID("saved-id", agent.Name())
	state := e.getOrCreateInteractiveStateWith(key, p, "ctx", session, e.sessions, nil, "", 0)
	if state.agentSession != nil || len(calls) != 0 || session.GetAgentSessionID() != "saved-id" {
		t.Fatalf("invalid saved session must remain bound and never start: calls=%q, id=%q", calls, session.GetAgentSessionID())
	}
	// Once the original session is available, a retry resumes that exact ID.
	agent.valid = true
	state = e.getOrCreateInteractiveStateWith(key, p, "ctx", session, e.sessions, nil, "", 0)
	if state.agentSession == nil || !reflect.DeepEqual(calls, []string{"saved-id"}) {
		t.Fatalf("retry did not resume original binding: calls=%q", calls)
	}
}

func TestResumeFallbackPolicy_UnsafeResumeRemainsBlockedAcrossRetries(t *testing.T) {
	var calls []string
	agent := &controllableAgent{startSessionFn: func(_ context.Context, id string) (AgentSession, error) {
		calls = append(calls, id)
		return newControllableSession("fresh-id"), nil
	}}
	p := &stubPlatformEngine{n: "test"}
	e := NewEngine("test", agent, []Platform{p}, "", LangEnglish)
	t.Cleanup(e.cancel)
	e.SetResumeFallbackToNew(false)
	const key = "test:user"
	session := e.sessions.GetOrCreateActive(key)
	session.SetAgentSessionID("possibly-live-id", agent.Name())
	e.markUnsafeResume(key)
	for i := 0; i < 2; i++ {
		state := e.getOrCreateInteractiveStateWith(key, p, "ctx", session, e.sessions, nil, "", 0)
		if state.agentSession != nil || state.startFailureMsg != MsgSessionResumeBlocked || len(calls) != 0 {
			t.Fatalf("unsafe resume was not blocked on attempt %d: calls=%q", i+1, calls)
		}
		if got := session.GetAgentSessionID(); got != "possibly-live-id" {
			t.Fatalf("binding = %q, want original unsafe binding", got)
		}
	}

	// An explicit new conversation may start and its later resume must not
	// inherit the guard for the abandoned conversation.
	fresh := e.sessions.NewSession(key, "fresh")
	state := e.getOrCreateInteractiveStateWith(key, p, "ctx", fresh, e.sessions, nil, "", 0)
	if state.agentSession == nil || !reflect.DeepEqual(calls, []string{""}) {
		t.Fatalf("explicit new conversation failed: calls=%q", calls)
	}
	e.cleanupInteractiveState(key)
	state = e.getOrCreateInteractiveStateWith(key, p, "ctx", fresh, e.sessions, nil, "", 0)
	if state.agentSession == nil || !reflect.DeepEqual(calls, []string{"", "fresh-id"}) {
		t.Fatalf("new conversation inherited the old unsafe flag: calls=%q", calls)
	}

	// The native original remains selectable through /switch even when /new
	// cleared its old cc-connect binding. Selecting it must not bypass safety.
	e.cleanupInteractiveState(key)
	previous := e.sessions.SwitchToAgentSession(key, "possibly-live-id", agent.Name(), "previous")
	state = e.getOrCreateInteractiveStateWith(key, p, "ctx", previous, e.sessions, nil, "", 0)
	if state.agentSession != nil || state.startFailureMsg != MsgSessionResumeBlocked || len(calls) != 2 {
		t.Fatalf("switching back bypassed unsafe native-ID guard: calls=%q", calls)
	}

	// A safe existing conversation is independent of the blocked ID.
	e.cleanupInteractiveState(key)
	safe := e.sessions.SwitchToAgentSession(key, "safe-id", agent.Name(), "safe")
	state = e.getOrCreateInteractiveStateWith(key, p, "ctx", safe, e.sessions, nil, "", 0)
	if state.agentSession == nil || !reflect.DeepEqual(calls, []string{"", "fresh-id", "safe-id"}) {
		t.Fatalf("safe existing conversation was blocked by another ID: calls=%q", calls)
	}
}

type closeClearsNativeIDSession struct {
	*controllableAgentSession
}

func (s *closeClearsNativeIDSession) Close() error {
	err := s.controllableAgentSession.Close()
	s.sessionID = ""
	return err
}

func TestResumeFallbackPolicy_UnsafeTeardownCapturedBeforeDirectNew(t *testing.T) {
	for _, fallback := range []bool{false, true} {
		t.Run(fmt.Sprintf("fallback=%v", fallback), func(t *testing.T) {
			var calls []string
			agent := &controllableAgent{startSessionFn: func(_ context.Context, id string) (AgentSession, error) {
				calls = append(calls, id)
				return newControllableSession("fresh-id"), nil
			}}
			p := &stubPlatformEngine{n: "test"}
			e := NewEngine("test", agent, []Platform{p}, "", LangEnglish)
			t.Cleanup(e.cancel)
			e.SetResumeFallbackToNew(fallback)
			const key = "test:user"
			old := &closeClearsNativeIDSession{newControllableSession("possibly-live-id")}
			old.closeErr = errors.New("process termination unconfirmed")
			e.sessions.GetOrCreateActive(key).SetAgentSessionID("possibly-live-id", agent.Name())
			e.interactiveStates[key] = &interactiveState{agentSession: old, agent: agent, platform: p, replyCtx: "ctx"}

			// /new itself fails to terminate the old process; no strict retry
			// happened first to convert a channel marker into a native guard.
			e.cmdNew(p, &Message{SessionKey: key, ReplyCtx: "ctx"}, []string{"replacement"})
			fresh := e.sessions.GetOrCreateActive(key)
			e.getOrCreateInteractiveStateWith(key, p, "ctx", fresh, e.sessions, nil, "", 0)
			e.cleanupInteractiveState(key)
			previous := e.sessions.SwitchToAgentSession(key, "possibly-live-id", agent.Name(), "previous")
			state := e.getOrCreateInteractiveStateWith(key, p, "ctx", previous, e.sessions, nil, "", 0)
			wantCalls := []string{""}
			if fallback {
				wantCalls = append(wantCalls, "")
				if state.agentSession == nil {
					t.Fatal("legacy mode should start a fresh session after unsafe resume")
				}
			} else if state.agentSession != nil || state.startFailureMsg != MsgSessionResumeBlocked {
				t.Fatal("strict mode should block the old native conversation after /new")
			}
			if !reflect.DeepEqual(calls, wantCalls) {
				t.Fatalf("unsafe native session was resumed: calls=%q, want %q", calls, wantCalls)
			}
		})
	}
}

func TestHandleRelay_UnsafeNativeSessionRespectsResumePolicy(t *testing.T) {
	for _, fallback := range []bool{false, true} {
		t.Run(fmt.Sprintf("fallback=%v", fallback), func(t *testing.T) {
			var calls []string
			agent := &controllableAgent{startSessionFn: func(_ context.Context, id string) (AgentSession, error) {
				calls = append(calls, id)
				return &resumePolicyReplySession{cujAgentSession: newCUJAgentSession(), id: "fresh-id"}, nil
			}}
			e := NewEngine("test", agent, nil, "", LangEnglish)
			t.Cleanup(e.cancel)
			e.SetResumeFallbackToNew(fallback)
			e.markUnsafeAgentSession(agent, "possibly-live-id")
			session := e.sessions.GetOrCreateActive("relay:source:test:chat")
			session.SetAgentSessionID("possibly-live-id", agent.Name())
			_, err := e.HandleRelay(context.Background(), "source", "test:chat:user", "hello")
			if fallback {
				if err != nil || !reflect.DeepEqual(calls, []string{""}) {
					t.Fatalf("legacy relay did not start fresh: calls=%q, error=%v", calls, err)
				}
			} else if err == nil || len(calls) != 0 || session.GetAgentSessionID() != "possibly-live-id" {
				t.Fatalf("strict relay did not preserve unsafe binding: calls=%q, error=%v", calls, err)
			}
		})
	}
}

func TestResumeFallbackPolicy_FailureNotifiesQueuedMessages(t *testing.T) {
	agent := &controllableAgent{startSessionFn: func(context.Context, string) (AgentSession, error) {
		return nil, errors.New("private provider detail")
	}}
	p := &stubPlatformEngine{n: "test"}
	e := NewEngine("test", agent, []Platform{p}, "", LangEnglish)
	t.Cleanup(e.cancel)
	e.SetResumeFallbackToNew(false)
	const key = "test:user"
	session := e.sessions.GetOrCreateActive(key)
	session.SetAgentSessionID("saved-id", agent.Name())
	e.interactiveStates[key] = &interactiveState{pendingMessages: []queuedMessage{{platform: p, replyCtx: "queued", content: "queued"}}}
	e.processInteractiveMessage(p, &Message{SessionKey: key, Platform: "test", Content: "first", ReplyCtx: "first"}, session, 0)
	state := e.interactiveStates[key]
	if len(state.pendingMessages) != 0 || len(p.getSent()) != 2 {
		t.Fatalf("queued input was not rejected after resume failed: pending=%d, replies=%q", len(state.pendingMessages), p.getSent())
	}
	if !e.queueMessageForBusySession(p, &Message{SessionKey: key, Content: "late", ReplyCtx: "late"}, key) {
		t.Fatal("late queued message was not handled")
	}
	if len(state.pendingMessages) != 0 {
		t.Fatal("message was queued after resume had already failed")
	}
	for _, reply := range p.getSent() {
		if !strings.Contains(reply, "saved session was preserved") || strings.Contains(reply, "private provider detail") {
			t.Fatalf("unexpected resume failure reply: %q", reply)
		}
	}
}

func TestHandleRelay_ResumeFallbackDisabledPreservesBinding(t *testing.T) {
	var calls []string
	failed := false
	agent := &controllableAgent{startSessionFn: func(_ context.Context, id string) (AgentSession, error) {
		calls = append(calls, id)
		if !failed {
			failed = true
			return nil, errors.New("temporary resume failure")
		}
		return &resumePolicyReplySession{cujAgentSession: newCUJAgentSession(), id: id}, nil
	}}
	store := filepath.Join(t.TempDir(), "sessions.json")
	e := NewEngine("test", agent, nil, store, LangEnglish)
	t.Cleanup(e.cancel)
	e.SetResumeFallbackToNew(false)
	const key = "relay:source:test:chat"
	session := e.sessions.GetOrCreateActive(key)
	session.SetAgentSessionID("saved-id", agent.Name())
	e.sessions.Save()
	if _, err := e.HandleRelay(context.Background(), "source", "test:chat:user", "hello"); err == nil {
		t.Fatal("expected resume failure")
	}
	if !reflect.DeepEqual(calls, []string{"saved-id"}) {
		t.Fatalf("unexpected fresh start after failed relay resume: %q", calls)
	}
	if got := NewSessionManager(store).GetOrCreateActive(key).GetAgentSessionID(); got != "saved-id" {
		t.Fatalf("persisted binding = %q, want saved-id", got)
	}
	if _, err := e.HandleRelay(context.Background(), "source", "test:chat:user", "retry"); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if !reflect.DeepEqual(calls, []string{"saved-id", "saved-id"}) {
		t.Fatalf("relay retry did not use original binding: %q", calls)
	}
}

type resumePolicyReplySession struct {
	*cujAgentSession
	id string
}

func (s *resumePolicyReplySession) CurrentSessionID() string { return s.id }
