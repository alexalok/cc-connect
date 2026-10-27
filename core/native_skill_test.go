package core

import (
	"context"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type nativeSkillAgent struct {
	resultAgent
	skill *Skill
}

func (a *nativeSkillAgent) ListSkills(context.Context) ([]*Skill, error) {
	return []*Skill{a.skill}, nil
}

func (*nativeSkillAgent) FormatSkillInvocation(skill *Skill, args []string) string {
	return strings.TrimSpace("invoke:" + skill.Name + " " + strings.Join(args, " "))
}

type nativeSkillSession struct {
	stubAgentSession
	events chan Event
}

func (s *nativeSkillSession) Events() <-chan Event { return s.events }
func (s *nativeSkillSession) Send(prompt, _ string, _ []ImageAttachment, _ []FileAttachment) error {
	s.events <- Event{Type: EventResult, Content: "plain: " + prompt, Done: true}
	return nil
}
func (s *nativeSkillSession) SendSkill(prompt, messageID string, images []ImageAttachment, files []FileAttachment, skill *Skill, onDelivered func()) error {
	if onDelivered != nil {
		onDelivered()
		onDelivered() // The bridge must keep delivery callbacks idempotent.
	}
	s.events <- Event{Type: EventResult, Content: "native: " + prompt + " from " + skill.Source, Done: true}
	return nil
}

func newNativeSkillEngine(t *testing.T, p Platform) (*Engine, *nativeSkillAgent) {
	t.Helper()
	s := &nativeSkillSession{events: make(chan Event, 4)}
	a := &nativeSkillAgent{
		resultAgent: resultAgent{session: s},
		skill: &Skill{Name: "review-code", DisplayName: "Friendly display name", Source: filepath.Join(t.TempDir(), "review-code"),
			Description: "Review code", Prompt: "FULL SKILL BODY MUST NOT BE PASTED"},
	}
	e := NewEngine("test", a, []Platform{p}, filepath.Join(t.TempDir(), "sessions.json"), LangEnglish)
	t.Cleanup(func() { _ = e.Stop() })
	return e, a
}

func TestSkillCommand_NativeInvocationPreservesSelectionAndReceipt(t *testing.T) {
	p := &stubPlatformEngine{n: "telegram"}
	e, a := newNativeSkillEngine(t, p)
	msg := skillMessage(p.Name(), "a", "/REVIEW_CODE reviewer extra")
	var receipts atomic.Int32
	msg.OnDelivered = func() { receipts.Add(1) }
	e.ReceiveMessage(p, msg)
	env := &cujEnv{t: t, engine: e, plat: p}
	env.waitFor("skill reply", 3*time.Second, func() bool { return len(p.getSent()) > 0 })
	want := "native: invoke:review-code reviewer extra from " + a.skill.Source
	if got := strings.Join(p.getSent(), "\n"); !strings.Contains(got, want) || strings.Contains(got, a.skill.Prompt) {
		t.Fatalf("visible reply = %q, want %q without expanded instructions", got, want)
	}
	if receipts.Load() != 1 {
		t.Fatalf("delivery receipts = %d, want 1", receipts.Load())
	}
}

func TestSkillCommand_NativeTextWithoutStructuredTransport(t *testing.T) {
	p := &stubPlatformEngine{n: "test"}
	e, a := newNativeSkillEngine(t, p)
	s := newResultAgentSession("native text accepted")
	a.session = s // A text-only transport does not implement SendSkill.
	msg := skillMessage(p.Name(), "a", "/review_code")
	e.ReceiveMessage(p, msg)
	env := &cujEnv{t: t, engine: e, plat: p}
	env.waitFor("text-only skill reply", 3*time.Second, func() bool {
		return strings.Contains(strings.Join(p.getSent(), "\n"), "native text accepted") && !e.sessions.GetOrCreateActive(msg.SessionKey).Busy()
	})
	if len(s.sentPrompts) != 1 || s.sentPrompts[0] != "invoke:review-code" {
		t.Fatalf("text-only native prompts = %v", s.sentPrompts)
	}
}

func TestScheduledSkill_NativeInvocation(t *testing.T) {
	for _, kind := range []string{"cron", "timer"} {
		t.Run(kind, func(t *testing.T) {
			p := &stubCronReplyTargetPlatform{stubPlatformEngine: stubPlatformEngine{n: "test"}}
			e, a := newNativeSkillEngine(t, p)
			const prompt = "/review_code reviewer"
			var err error
			if kind == "cron" {
				err = e.ExecuteCronJob(&CronJob{ID: "skill", SessionKey: "test:channel:user", Prompt: prompt})
			} else {
				err = e.ExecuteTimerJob(&TimerJob{ID: "skill", SessionKey: "test:channel:user", Prompt: prompt})
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Join(p.getSent(), "\n"); !strings.Contains(got, "native: invoke:review-code reviewer from "+a.skill.Source) || strings.Contains(got, a.skill.Prompt) {
				t.Fatalf("scheduled skill reply = %q", got)
			}
		})
	}
}
