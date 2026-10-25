package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/chenhg5/cc-connect/core"
)

type reloadResumePolicyAgent struct {
	stubMainAgent
	calls []string
}

func (a *reloadResumePolicyAgent) StartSession(_ context.Context, id string) (core.AgentSession, error) {
	a.calls = append(a.calls, id)
	if id != "" {
		return nil, errors.New("temporary resume failure")
	}
	return &reloadResumePolicySession{events: make(chan core.Event, 1)}, nil
}

type reloadResumePolicySession struct {
	stubMainAgentSession
	events chan core.Event
}

func (s *reloadResumePolicySession) CurrentSessionID() string  { return "saved-id" }
func (s *reloadResumePolicySession) Events() <-chan core.Event { return s.events }
func (s *reloadResumePolicySession) Send(string, string, []core.ImageAttachment, []core.FileAttachment) error {
	s.events <- core.Event{Type: core.EventResult, Content: "answer", Done: true}
	return nil
}

func TestReloadConfig_ResumeFallbackToNew(t *testing.T) {
	agent := &reloadResumePolicyAgent{}
	e := core.NewEngine("demo", agent, nil, "", core.LangEnglish)
	t.Cleanup(func() { _ = e.Stop() })
	call := func() error {
		_, err := e.HandleRelay(context.Background(), "source", "test:chat:user", "hello")
		return err
	}
	// Bind a conversation through the public relay entrypoint.
	if err := call(); err != nil {
		t.Fatalf("initial conversation: %v", err)
	}
	path := filepath.Join(t.TempDir(), "config.toml")
	for _, value := range []string{"false", "true", "false", ""} {
		t.Run("reload="+value, func(t *testing.T) {
			setting := ""
			if value != "" {
				setting = "resume_fallback_to_new = " + value + "\n"
			}
			body := "[[projects]]\nname = \"demo\"\n" + setting + `
[projects.agent]
type = "stub-main"
[[projects.platforms]]
type = "test"
`
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := reloadConfig(path, "demo", e); err != nil {
				t.Fatalf("reloadConfig: %v", err)
			}
			agent.calls = nil
			err := call()
			wantCalls := []string{"saved-id", ""}
			if value == "false" {
				wantCalls = []string{"saved-id"}
			}
			if (err != nil) != (value == "false") {
				t.Fatalf("relay error = %v, want error=%v", err, value == "false")
			}
			if !reflect.DeepEqual(agent.calls, wantCalls) {
				t.Fatalf("StartSession calls = %q, want %q", agent.calls, wantCalls)
			}
		})
	}
}
