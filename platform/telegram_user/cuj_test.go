package telegram_user

import (
	"context"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
	"github.com/gotd/td/tg"
)

// This variant of CUJ-D6 lives at the platform boundary so core never imports
// an adapter. Only the Telegram RPC and agent process are mocked; the real
// Engine and SessionManager handle messages, history, and replies.
func TestCUJ_D6_UserAccountSilentlyLimitsSenderAndRecovers(t *testing.T) {
	p, _ := testPlatform(t)
	p.limiter.Stop()
	p.limiter = core.NewRateLimiter(2, time.Second)
	p.groupReplyAll = true
	api := &journeyAPI{visible: make(chan string, 32)}
	p.api = api
	engine := core.NewEngine("user-account", journeyAgent{}, []core.Platform{p}, filepath.Join(t.TempDir(), "sessions.json"), core.LangEnglish)
	engine.SetReplyFooterEnabled(false)
	p.handler = engine.ReceiveMessage
	t.Cleanup(func() { _ = engine.Stop() })

	send := func(id int, user int64, group bool, content string) {
		t.Helper()
		m := testMessage(id)
		m.FromID, m.PeerID, m.Message = &tg.PeerUser{UserID: user}, &tg.PeerUser{UserID: user}, content
		if group {
			m.PeerID = &tg.PeerChat{ChatID: 7}
		}
		e := testEntities()
		e.Users[43] = &tg.User{ID: 43, AccessHash: 430}
		p.handleMessage(context.Background(), e, m)
	}
	waitFor := func(want string) {
		t.Helper()
		timer := time.NewTimer(2 * time.Second)
		defer timer.Stop()
		for {
			select {
			case text := <-api.visible:
				if strings.Contains(text, want) {
					return
				}
			case <-timer.C:
				t.Fatalf("user did not see %q", want)
			}
		}
	}

	// 1. An allowed person receives an answer from the account.
	send(1, 42, false, "first greeting")
	waitFor("answer: first greeting")
	// 2. Text commands work without a bot menu or callback keyboard.
	send(2, 42, false, "/history")
	waitFor("first greeting")
	// 3. A third message from the same person is silently dropped even in
	// another chat. No warning is sent that could keep another bot replying.
	send(3, 42, true, "over the limit")
	select {
	case text := <-api.visible:
		t.Fatalf("rate-limited sender received a reply: %q", text)
	case <-time.After(30 * time.Millisecond):
	}
	// 4. Another allowed person can still use the account.
	send(4, 43, false, "another person")
	waitFor("answer: another person")
	// 5. After the window, new input works; dropped input is never replayed.
	time.Sleep(1100 * time.Millisecond)
	send(5, 42, true, "after timeout")
	waitFor("answer: after timeout")
}

type journeyAPI struct {
	fakeAPI
	visible chan string
	ids     atomic.Int32
}

func (a *journeyAPI) MessagesSendMessage(_ context.Context, req *tg.MessagesSendMessageRequest) (tg.UpdatesClass, error) {
	a.visible <- req.Message
	return &tg.UpdateShortSentMessage{ID: int(a.ids.Add(1))}, nil
}

func (a *journeyAPI) MessagesEditMessage(_ context.Context, req *tg.MessagesEditMessageRequest) (tg.UpdatesClass, error) {
	a.visible <- req.Message
	return &tg.Updates{}, nil
}

type journeyAgent struct{}

func (journeyAgent) Name() string                                                  { return "journey" }
func (journeyAgent) Stop() error                                                   { return nil }
func (journeyAgent) ListSessions(context.Context) ([]core.AgentSessionInfo, error) { return nil, nil }
func (journeyAgent) StartSession(context.Context, string) (core.AgentSession, error) {
	return &journeySession{events: make(chan core.Event, 4)}, nil
}

type journeySession struct {
	events chan core.Event // kept open until engine cancellation ends its reader
	closed atomic.Bool
}

func (s *journeySession) Send(prompt, _ string, _ []core.ImageAttachment, _ []core.FileAttachment) error {
	s.events <- core.Event{Type: core.EventResult, Content: "answer: " + prompt, Done: true}
	return nil
}
func (*journeySession) RespondPermission(string, core.PermissionResult) error { return nil }
func (s *journeySession) Events() <-chan core.Event                           { return s.events }
func (*journeySession) CurrentSessionID() string                              { return "journey-session" }
func (s *journeySession) Alive() bool                                         { return !s.closed.Load() }
func (s *journeySession) Close() error                                        { s.closed.Store(true); return nil }
