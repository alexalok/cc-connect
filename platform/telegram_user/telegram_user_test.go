package telegram_user

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/chenhg5/cc-connect/core"
	"github.com/gotd/td/session"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
)

func testOptions(t *testing.T) map[string]any {
	t.Helper()
	return map[string]any{
		"api_id": int64(12345), "api_hash": strings.Repeat("a", 32),
		"session_file": filepath.Join(t.TempDir(), "account.session.json"), "allow_from": "42,43",
	}
}

func testPlatform(t *testing.T) (*Platform, chan *core.Message) {
	t.Helper()
	raw, err := New(testOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	p := raw.(*Platform)
	p.limiter = core.NewRateLimiter(p.rateLimit.MaxMessages, p.rateLimit.Window)
	t.Cleanup(func() { _ = p.Stop() })
	p.self = &tg.User{ID: 100, Username: "assistant"}
	got := make(chan *core.Message, 100)
	p.handler = func(_ core.Platform, msg *core.Message) { got <- msg }
	return p, got
}

func testMessage(id int) *tg.Message {
	return &tg.Message{ID: id, Date: int(time.Now().Unix()), FromID: &tg.PeerUser{UserID: 42}, PeerID: &tg.PeerUser{UserID: 42}, Message: "hello"}
}

func testEntities() tg.Entities {
	return tg.Entities{
		Users:    map[int64]*tg.User{42: {ID: 42, Username: "alex", AccessHash: 420}},
		Chats:    map[int64]*tg.Chat{7: {ID: 7, Title: "Team"}},
		Channels: map[int64]*tg.Channel{9: {ID: 9, Title: "Forum", Forum: true, AccessHash: 900}},
	}
}

func receive(t *testing.T, ch <-chan *core.Message) *core.Message {
	t.Helper()
	select {
	case msg := <-ch:
		return msg
	case <-time.After(time.Second):
		t.Fatal("message was not delivered")
		return nil
	}
}

func noMessage(t *testing.T, ch <-chan *core.Message) {
	t.Helper()
	select {
	case msg := <-ch:
		t.Fatalf("unexpected message: %+v", msg)
	case <-time.After(10 * time.Millisecond):
	}
}

func TestNewValidatesCredentialsAndAccess(t *testing.T) {
	for _, tc := range []struct {
		key   string
		value any
	}{
		{"api_id", nil}, {"api_id", -1}, {"api_id", int64(1 << 40)}, {"api_id", 1.2},
		{"api_hash", "secret"}, {"session_file", ""}, {"session_file", "relative.json"},
		{"allow_from", ""}, {"allow_from", "name"}, {"allow_from", "0"}, {"allow_from", "-42"},
		{"allow_from", "42,"}, {"allow_from", "42,*"}, {"allow_chats", "not-a-chat"},
		{"proxy", "http://user:secret@localhost:1234"}, {"proxy", "socks5://localhost"},
		{"rate_limit_messages", 0}, {"rate_limit_messages", -1}, {"rate_limit_messages", 1.5},
		{"rate_limit_window_seconds", "bad"}, {"rate_limit_window_seconds", 0},
	} {
		t.Run(fmt.Sprintf("%s=%v", tc.key, tc.value), func(t *testing.T) {
			opts := testOptions(t)
			opts[tc.key] = tc.value
			_, err := New(opts)
			if err == nil {
				t.Fatal("invalid option accepted")
			}
			if strings.Contains(err.Error(), "secret") {
				t.Fatal("credentials leaked in error")
			}
		})
	}
	opts := testOptions(t)
	opts["allow_from"], opts["allow_chats"] = "*", "42,-7,-1000000000009"
	opts["proxy"] = "socks5://user:password@localhost:1080"
	if _, err := core.CreatePlatform("telegram_user", opts); err != nil {
		t.Fatalf("registered platform: %v", err)
	}
}

func TestIncomingMessagesFilterAndRoute(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*Platform, *tg.Message)
		want   bool
	}{
		{"private", func(*Platform, *tg.Message) {}, true},
		{"private without from ID", func(_ *Platform, m *tg.Message) { m.FromID = nil }, true},
		{"outgoing", func(_ *Platform, m *tg.Message) { m.Out = true }, false},
		{"self even without outgoing flag", func(_ *Platform, m *tg.Message) { m.FromID = &tg.PeerUser{UserID: 100} }, false},
		{"unlisted sender", func(_ *Platform, m *tg.Message) { m.FromID = &tg.PeerUser{UserID: 99} }, false},
		{"Telegram login codes", func(p *Platform, m *tg.Message) { p.allowFrom = "*"; m.FromID = &tg.PeerUser{UserID: 777000} }, false},
		{"restricted chat", func(p *Platform, _ *tg.Message) { p.allowChats = "55" }, false},
		{"old", func(_ *Platform, m *tg.Message) { m.Date = int(core.StartTime.Add(-time.Minute).Unix()) }, false},
		{"empty", func(_ *Platform, m *tg.Message) { m.Message = " " }, false},
		{"broadcast", func(_ *Platform, m *tg.Message) { m.Post = true }, false},
		{"anonymous", func(_ *Platform, m *tg.Message) {
			m.FromID = &tg.PeerChannel{ChannelID: 9}
			m.PeerID = &tg.PeerChannel{ChannelID: 9}
		}, false},
		{"group needs mention", func(_ *Platform, m *tg.Message) { m.PeerID = &tg.PeerChat{ChatID: 7} }, false},
		{"group reply all", func(p *Platform, m *tg.Message) { p.groupReplyAll = true; m.PeerID = &tg.PeerChat{ChatID: 7} }, true},
		{"plain text mention is not an entity", func(_ *Platform, m *tg.Message) { m.PeerID = &tg.PeerChat{ChatID: 7}; m.Message = "email @assistant" }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, got := testPlatform(t)
			m := testMessage(1)
			tc.change(p, m)
			p.handleMessage(context.Background(), testEntities(), m)
			if !tc.want {
				noMessage(t, got)
				return
			}
			msg := receive(t, got)
			if msg.UserID != "42" || msg.Content != "hello" || msg.UserName != "alex" || msg.Platform != "telegram_user" {
				t.Fatalf("wrong message: %+v", msg)
			}
			p.handleMessage(context.Background(), testEntities(), m)
			noMessage(t, got)
		})
	}
}

func TestForumMentionRoutesReplyAndSession(t *testing.T) {
	p, got := testPlatform(t)
	m := testMessage(55)
	m.PeerID = &tg.PeerChannel{ChannelID: 9}
	m.Message = "😀 @AsSiStAnT /help"
	m.Entities = []tg.MessageEntityClass{&tg.MessageEntityMention{Offset: 3, Length: 10}}
	m.ReplyTo = &tg.MessageReplyHeader{ForumTopic: true, ReplyToMsgID: 50, ReplyToTopID: 12}
	p.handleMessage(context.Background(), testEntities(), m)
	msg := receive(t, got)
	if msg.Content != "😀  /help" || msg.SessionKey != "telegram_user:-1000000000009:12:42" || msg.ChannelKey != "-1000000000009:12" {
		t.Fatalf("wrong forum message: %+v", msg)
	}
	if msg.ReplyCtx != (replyContext{chatID: -1000000000009, threadID: 12, messageID: 55}) {
		t.Fatalf("wrong reply context: %+v", msg.ReplyCtx)
	}
	p.shareSessionInChannel = true
	m.ID++
	p.handleMessage(context.Background(), testEntities(), m)
	if got := receive(t, got).SessionKey; got != "telegram_user:-1000000000009:12:shared" {
		t.Fatal(got)
	}
	// An ordinary group reply must not create a topic session.
	m.ID++
	m.PeerID = &tg.PeerChat{ChatID: 7}
	m.ReplyTo = &tg.MessageReplyHeader{ReplyToMsgID: 50}
	p.handleMessage(context.Background(), testEntities(), m)
	if got := receive(t, got).SessionKey; got != "telegram_user:-7:shared" {
		t.Fatal(got)
	}
}

func TestMentionByIDAndInvalidOffsets(t *testing.T) {
	self := &tg.User{ID: 100}
	text, ok := stripMention("Name hello", []tg.MessageEntityClass{&tg.MessageEntityMentionName{UserID: 100, Offset: 0, Length: 4}}, self)
	if !ok || text != "hello" {
		t.Fatalf("%q, %v", text, ok)
	}
	for _, offset := range []int{-1, 100, int(^uint(0) >> 1)} {
		_, ok := stripMention("x", []tg.MessageEntityClass{&tg.MessageEntityMentionName{UserID: 100, Offset: offset, Length: 2}}, self)
		if ok {
			t.Fatal("invalid mention accepted")
		}
	}
}

func TestRateLimitPerSenderAcrossChatsAndTimeout(t *testing.T) {
	p, got := testPlatform(t)
	p.limiter.Stop()
	p.limiter = core.NewRateLimiter(10, 500*time.Millisecond)
	p.groupReplyAll = true
	for i := 1; i <= 10; i++ {
		m := testMessage(i)
		if i%2 == 0 {
			m.PeerID = &tg.PeerChat{ChatID: 7}
		}
		p.handleMessage(context.Background(), testEntities(), m)
		receive(t, got)
	}
	time.Sleep(300 * time.Millisecond)
	p.handleMessage(context.Background(), testEntities(), testMessage(11))
	noMessage(t, got)
	other := testMessage(12)
	other.FromID = &tg.PeerUser{UserID: 43}
	p.handleMessage(context.Background(), testEntities(), other)
	if receive(t, got).UserID != "43" {
		t.Fatal("another sender was rate limited")
	}
	// Rejected traffic did not postpone the timeout, and was not queued.
	time.Sleep(250 * time.Millisecond)
	p.handleMessage(context.Background(), testEntities(), testMessage(13))
	if receive(t, got).MessageID != "13" {
		t.Fatal("sender did not resume at timeout")
	}
	noMessage(t, got)
}

func TestSenderRateLimitConcurrentMessages(t *testing.T) {
	p, _ := testPlatform(t)
	var accepted atomic.Int32
	var wg sync.WaitGroup
	for range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if p.allowSender("42") {
				accepted.Add(1)
			}
		}()
	}
	wg.Wait()
	if accepted.Load() != 10 {
		t.Fatalf("concurrent messages accepted: %d", accepted.Load())
	}
}

func TestDuplicatesAndUnauthorizedMessagesDoNotConsumeQuota(t *testing.T) {
	p, got := testPlatform(t)
	for i := 1; i <= 10; i++ {
		m := testMessage(i)
		m.FromID = &tg.PeerUser{UserID: 99}
		p.handleMessage(context.Background(), testEntities(), m)
		m.FromID = &tg.PeerUser{UserID: 42}
		p.handleMessage(context.Background(), testEntities(), m)
		receive(t, got)
		p.handleMessage(context.Background(), testEntities(), m)
	}
	noMessage(t, got)
}

func TestSessionStoragePersistsPrivately(t *testing.T) {
	s := &sessionStorage{path: filepath.Join(t.TempDir(), "nested", "account.session.json")}
	ctx := context.Background()
	if _, err := s.LoadSession(ctx); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("missing session: %v", err)
	}
	for _, data := range []string{"first", "replacement"} {
		if err := s.StoreSession(ctx, []byte(data)); err != nil {
			t.Fatal(err)
		}
		loaded, err := (&sessionStorage{path: s.path}).LoadSession(ctx)
		if err != nil || string(loaded) != data {
			t.Fatalf("reload: %q, %v", loaded, err)
		}
		info, _ := os.Stat(s.path)
		if runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
			t.Fatalf("session permissions: %v", info.Mode())
		}
		if err := os.Chmod(s.path, 0644); err != nil {
			t.Fatal(err)
		}
	}
	files, _ := os.ReadDir(filepath.Dir(s.path))
	if len(files) != 1 {
		t.Fatal("temporary session files left behind")
	}
}

type fakeAPI struct {
	sent           []*tg.MessagesSendMessageRequest
	edits          []*tg.MessagesEditMessageRequest
	err            error
	dialogs        tg.MessagesDialogsClass
	dialogCalls    int
	deleted        []int
	deletedChannel int64
}

func (a *fakeAPI) MessagesSendMessage(_ context.Context, req *tg.MessagesSendMessageRequest) (tg.UpdatesClass, error) {
	a.sent = append(a.sent, req)
	return &tg.UpdateShortSentMessage{ID: len(a.sent) + 100}, a.err
}
func (a *fakeAPI) MessagesEditMessage(_ context.Context, req *tg.MessagesEditMessageRequest) (tg.UpdatesClass, error) {
	a.edits = append(a.edits, req)
	return &tg.Updates{}, a.err
}
func (a *fakeAPI) MessagesSetTyping(context.Context, *tg.MessagesSetTypingRequest) (bool, error) {
	return true, a.err
}
func (a *fakeAPI) MessagesGetDialogs(context.Context, *tg.MessagesGetDialogsRequest) (tg.MessagesDialogsClass, error) {
	a.dialogCalls++
	return a.dialogs, a.err
}
func (a *fakeAPI) MessagesDeleteMessages(_ context.Context, req *tg.MessagesDeleteMessagesRequest) (*tg.MessagesAffectedMessages, error) {
	a.deleted = req.ID
	return &tg.MessagesAffectedMessages{}, a.err
}
func (a *fakeAPI) ChannelsDeleteMessages(_ context.Context, req *tg.ChannelsDeleteMessagesRequest) (*tg.MessagesAffectedMessages, error) {
	a.deleted, a.deletedChannel = req.ID, req.Channel.(*tg.InputChannel).ChannelID
	return &tg.MessagesAffectedMessages{}, a.err
}

func TestSendReplyPreviewAndUnicodeChunking(t *testing.T) {
	p, _ := testPlatform(t)
	a := &fakeAPI{}
	p.api = a
	p.rememberEntities(testEntities())
	rc := replyContext{chatID: -1000000000009, threadID: 12, messageID: 55}
	text := strings.Repeat("😀", 5000) + "tail"
	if err := p.Reply(context.Background(), rc, text); err != nil {
		t.Fatal(err)
	}
	var joined strings.Builder
	for _, req := range a.sent {
		joined.WriteString(req.Message)
		if !utf8.ValidString(req.Message) || len(utf16.Encode([]rune(req.Message))) > maxMessageUnits {
			t.Fatal("invalid text chunk")
		}
		reply := req.ReplyTo.(*tg.InputReplyToMessage)
		if reply.ReplyToMsgID != 55 || reply.TopMsgID != 12 || req.Peer.(*tg.InputPeerChannel).AccessHash != 900 {
			t.Fatalf("wrong reply target: peer=%+v reply=%+v", req.Peer, req.ReplyTo)
		}
	}
	if joined.String() != text {
		t.Fatal("reply was truncated")
	}
	if err := p.Send(context.Background(), rc, "scheduled"); err != nil {
		t.Fatal(err)
	}
	if a.sent[len(a.sent)-1].ReplyTo.(*tg.InputReplyToMessage).ReplyToMsgID != 12 {
		t.Fatal("proactive message lost its topic")
	}
	handle, err := p.SendPreviewStart(context.Background(), rc, "thinking")
	if err != nil {
		t.Fatal(err)
	}
	if err := p.UpdateMessage(context.Background(), handle, "answer"); err != nil || a.edits[0].ID != handle.(replyContext).messageID {
		t.Fatalf("edit preview: %v", err)
	}
	a.err = &tgerr.Error{Code: 400, Type: "MESSAGE_NOT_MODIFIED"}
	if err := p.UpdateMessage(context.Background(), handle, "answer"); err != nil {
		t.Fatal(err)
	}
	if err := p.UpdateMessage(context.Background(), handle, text); err == nil {
		t.Fatal("oversized preview silently accepted")
	}
	a.err = nil
	if err := p.DeletePreviewMessage(context.Background(), handle); err != nil || a.deletedChannel != 9 || a.deleted[0] != handle.(replyContext).messageID {
		t.Fatalf("preview cleanup: %v", err)
	}
	if err := p.DeletePreviewMessage(context.Background(), replyContext{chatID: 42, messageID: 123}); err != nil || a.deleted[0] != 123 {
		t.Fatalf("private preview cleanup: %v", err)
	}
	a.err = errors.New("network failed")
	if err := p.Reply(context.Background(), rc, "failed"); !errors.Is(err, a.err) {
		t.Fatal("send error was swallowed")
	}
}

func TestReconstructReplyContextResolvesAccessHashAfterRestart(t *testing.T) {
	p, _ := testPlatform(t)
	a := &fakeAPI{dialogs: &tg.MessagesDialogs{
		Dialogs:  []tg.DialogClass{&tg.Dialog{Peer: &tg.PeerUser{UserID: 42}, TopMessage: 10}},
		Messages: []tg.MessageClass{&tg.Message{ID: 10, PeerID: &tg.PeerUser{UserID: 42}, Date: int(time.Now().Unix())}},
		Users:    []tg.UserClass{&tg.User{ID: 42, AccessHash: 420}},
	}}
	p.api = a
	rc, err := p.ReconstructReplyCtx("telegram_user:42:42")
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Send(context.Background(), rc, "scheduled"); err != nil {
		t.Fatal(err)
	}
	if a.dialogCalls != 1 || a.sent[0].Peer.(*tg.InputPeerUser).AccessHash != 420 {
		t.Fatal("access hash was not recovered")
	}
	for _, key := range []string{"telegram:42:42", "telegram_user:0:42", "telegram_user:42", "telegram_user:-9:no:42", "telegram_user:42:bad"} {
		if _, err := p.ReconstructReplyCtx(key); err == nil {
			t.Fatalf("invalid key accepted: %s", key)
		}
	}
	if key, ok := p.ChannelKeyFromSessionKey("telegram_user:-1000000000009:12:42"); !ok || key != "-1000000000009:12" {
		t.Fatalf("channel key: %s, %v", key, ok)
	}
}

func TestLifecycleStartStopAndMissingSession(t *testing.T) {
	p, _ := testPlatform(t)
	if err := p.Start(func(core.Platform, *core.Message) {}); err == nil {
		t.Fatal("startup without login succeeded")
	}
	if err := os.WriteFile(p.credentials.sessionFile, []byte("test"), 0600); err != nil {
		t.Fatal(err)
	}
	p.run = func(ctx context.Context, ready func(messageAPI, *tg.User)) error {
		ready(&fakeAPI{}, &tg.User{ID: 100})
		<-ctx.Done()
		return ctx.Err()
	}
	for range 2 {
		if err := p.Start(func(core.Platform, *core.Message) {}); err != nil {
			t.Fatal(err)
		}
		if err := p.Start(func(core.Platform, *core.Message) {}); err == nil {
			t.Fatal("double start succeeded")
		}
		if err := p.Stop(); err != nil {
			t.Fatal(err)
		}
		if _, err := p.getAPI(); err == nil {
			t.Fatal("client still available after stop")
		}
	}
	p.run = func(context.Context, func(messageAPI, *tg.User)) error { return errors.New("unauthorized") }
	if err := p.Start(func(core.Platform, *core.Message) {}); err == nil {
		t.Fatal("failed connection reported ready")
	}
	if err := p.Stop(); err != nil {
		t.Fatal(err)
	}
}
