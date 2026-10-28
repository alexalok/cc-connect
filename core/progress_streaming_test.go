package core

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// This platform boundary records the complete message a user sees, with
// progress separated from the answer just as a disclosure renderer requires.
type progressStreamPlatform struct {
	stubPlatformEngine
	finalErr error
}

type progressStreamCard struct {
	p        *progressStreamPlatform
	progress []ProgressCardEntry
	index    int
}

func (p *progressStreamPlatform) CreateStreamingCard(context.Context, any) (StreamingCard, error) {
	return &progressStreamCard{p: p, index: -1}, nil
}
func (c *progressStreamCard) Failed() bool { return false }
func (c *progressStreamCard) AppendProgress(ctx context.Context, entry ProgressCardEntry) error {
	c.progress = append(c.progress, entry)
	return c.Update(ctx, "")
}
func (c *progressStreamCard) Update(_ context.Context, answer string) error {
	var parts []string
	for _, entry := range c.progress {
		parts = append(parts, entry.Text+entry.Tool)
	}
	text := "<progress>" + strings.Join(parts, "\n") + "</progress>" + answer
	c.p.mu.Lock()
	defer c.p.mu.Unlock()
	if c.index == -1 {
		c.index = len(c.p.sent)
		c.p.sent = append(c.p.sent, text)
	} else {
		c.p.sent[c.index] = text
	}
	return nil
}
func (c *progressStreamCard) Finalize(ctx context.Context, answer string) error {
	if c.p.finalErr != nil {
		return c.p.finalErr
	}
	return c.Update(ctx, answer)
}

func TestProgressStreamingCardPublicSummariesAndFinalAnswer(t *testing.T) {
	for _, ending := range []string{"success", "error", "uncertain", "quiet"} {
		t.Run(ending, func(t *testing.T) {
			p := &progressStreamPlatform{stubPlatformEngine: stubPlatformEngine{n: "test"}}
			if ending == "uncertain" {
				p.finalErr = ErrDeliveryUncertain
			}
			e := NewEngine("test", &stubAgent{}, []Platform{p}, "", LangEnglish)
			if ending == "quiet" {
				e.SetDisplayConfig(DisplayCfg{Mode: "quiet", CardMode: "legacy"})
			}
			key := "test:progress"
			session := e.sessions.GetOrCreateActive(key)
			as := newControllableSession("progress-test")
			state := &interactiveState{agentSession: as, platform: p, replyCtx: "ctx"}
			e.interactiveStates[key] = state
			long := "BEGIN " + strings.Repeat("public summary ", 400) + " END"
			as.events <- Event{Type: EventThinking, Content: long, PublicSummary: true}
			as.events <- Event{Type: EventThinking, Content: "private reasoning"}
			as.events <- Event{Type: EventToolUse, ToolName: "Read", ToolInput: "secret arguments"}
			as.events <- Event{Type: EventToolResult, ToolName: "Read", ToolResult: "secret output"}
			as.events <- Event{Type: EventText, Content: "Visible answer"}
			if ending == "error" {
				as.events <- Event{Type: EventError, Error: errors.New("task failed")}
			} else {
				as.events <- Event{Type: EventResult, Content: "Visible answer", Done: true}
			}
			e.processInteractiveEvents(state, session, e.sessions, key, "message", time.Now(), nil, nil, state.replyCtx, 0)
			sent := p.getSent()
			if len(sent) != 1 {
				t.Fatalf("expected one combined message; got %#v", sent)
			}
			if strings.Contains(sent[0], "private reasoning") || strings.Contains(sent[0], "secret") {
				t.Fatal("nonpublic reasoning or raw tool data leaked")
			}
			if ending != "quiet" && !strings.Contains(sent[0], long) {
				t.Fatal("summary was truncated")
			}
			if ending == "quiet" && strings.Contains(sent[0], "public summary") {
				t.Fatal("quiet mode showed progress")
			}
			if ending == "success" && !strings.HasSuffix(sent[0], "</progress>Visible answer") {
				t.Fatal("answer included progress formatting")
			}
			if ending == "error" && !strings.Contains(sent[0], "</progress>❌") && !strings.Contains(strings.Split(sent[0], "</progress>")[1], "task failed") {
				t.Fatal("error was not finalized in the same message")
			}
		})
	}
}

func TestProgressStreamingCardPreservesReplyFooter(t *testing.T) {
	for _, mode := range []string{"standard", "cached", "disabled", "silent", "fallback"} {
		t.Run(mode, func(t *testing.T) {
			workDir := t.TempDir()
			agent := &stubReplyFooterAgent{stubModelModeAgent: stubModelModeAgent{model: "test-model"}, workDir: workDir}
			p := &progressStreamPlatform{stubPlatformEngine: stubPlatformEngine{n: "test"}}
			if mode == "fallback" {
				p.finalErr = errors.New("definite rejection")
			}
			e := NewEngine("test", agent, []Platform{p}, "", LangEnglish)
			e.SetReplyFooterEnabled(mode != "disabled")
			e.SetShowContextIndicator(true)
			e.SetShowWorkdirIndicator(true)
			key := "test:progress-footer"
			session := e.sessions.GetOrCreateActive(key)
			as := newControllableSession("progress-footer")
			if mode == "cached" {
				as.contextUsage = &ContextUsage{InputTokens: 100, CachedInputTokens: 1000, OutputTokens: 20, UsedTokens: 1100, ContextWindow: 10000}
			}
			state := &interactiveState{agentSession: as, platform: p, replyCtx: "ctx", agent: agent, workspaceDir: workDir}
			e.interactiveStates[key] = state
			as.events <- Event{Type: EventThinking, Content: "Public summary", PublicSummary: true}
			answer := "Visible answer"
			if mode == "silent" {
				answer = "NO_REPLY"
			}
			as.events <- Event{Type: EventResult, Content: answer, InputTokens: 28000, Done: true}
			e.processInteractiveEvents(state, session, e.sessions, key, "message", time.Now(), nil, nil, state.replyCtx, 0)
			sent := p.getSent()
			wantMessages := 1
			if mode == "fallback" {
				wantMessages = 2
			}
			if len(sent) != wantMessages {
				t.Fatalf("sent = %#v", sent)
			}
			final := sent[len(sent)-1]
			if mode == "disabled" || mode == "silent" {
				if strings.Contains(final, "test-model") || strings.Contains(final, "NO_REPLY") {
					t.Fatalf("unexpected footer/marker: %q", final)
				}
				return
			}
			for _, want := range []string{"Visible answer\n\n*", "test-model", compactReplyFooterPath(workDir)} {
				if !strings.Contains(final, want) {
					t.Fatalf("final reply lost %q: %q", want, final)
				}
			}
			if mode != "fallback" && !strings.Contains(final, "</progress>Visible answer") {
				t.Fatalf("footer/answer escaped progress boundary: %q", final)
			}
			if strings.Count(final, "test-model") != 1 {
				t.Fatalf("duplicate footer: %q", final)
			}
		})
	}
}
