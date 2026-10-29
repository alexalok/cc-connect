package telegram

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/chenhg5/cc-connect/core"
	tgbot "github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

type generalTopicBot struct {
	topicSyncBot
	copies    []*tgbot.CopyMessageParams
	sends     []*tgbot.SendMessageParams
	copyErr   error
	emptyCopy bool
	receipts  chan *tgbot.SetMessageReactionParams
}

func (b *generalTopicBot) CreateForumTopic(ctx context.Context, params *tgbot.CreateForumTopicParams) (*models.ForumTopic, error) {
	_, err := b.topicSyncBot.CreateForumTopic(ctx, params)
	return &models.ForumTopic{MessageThreadID: 41 + len(b.created)}, err
}

func (b *generalTopicBot) CopyMessage(_ context.Context, params *tgbot.CopyMessageParams) (*models.MessageID, error) {
	b.copies = append(b.copies, params)
	if b.emptyCopy {
		return nil, b.copyErr
	}
	return &models.MessageID{ID: 200 + len(b.copies)}, b.copyErr
}

func (b *generalTopicBot) SendMessage(_ context.Context, params *tgbot.SendMessageParams) (*models.Message, error) {
	b.sends = append(b.sends, params)
	return &models.Message{ID: 300 + len(b.sends)}, nil
}

func newGeneralTopicPlatform(t *testing.T) (*Platform, *generalTopicBot) {
	t.Helper()
	platform, err := New(map[string]any{
		"token": "test-token", "allow_from": "7,8", "group_reply_all": true,
		"topic_sync_chat_id": "-100123", "general_topic_auto_create": true,
		"read_receipts": false,
	})
	if err != nil {
		t.Fatal(err)
	}
	p := platform.(*Platform)
	b := &generalTopicBot{topicSyncBot: topicSyncBot{
		stubTelegramBot: *newStubTelegramBot(),
		chat:            &models.ChatFullInfo{ID: -100123, Type: models.ChatTypeSupergroup, IsForum: true},
		member:          &models.ChatMember{Type: models.ChatMemberTypeAdministrator, Administrator: &models.ChatMemberAdministrator{CanManageTopics: true}},
	}}
	p.bot, p.selfUser = b, &models.User{ID: 99, Username: "bridge"}
	return p, b
}

func generalMessage(id, threadID int) *models.Message {
	return &models.Message{ID: id, Date: int(time.Now().Unix()), Text: "Help with this task",
		Chat: models.Chat{ID: -100123, Type: models.ChatTypeSupergroup, IsForum: true},
		From: &models.User{ID: 7, Username: "alice"}, MessageThreadID: threadID}
}

func TestGeneralTopicAutoCreate_GeneralMessageStartsNewTopic(t *testing.T) {
	for _, threadID := range []int{0, 1} {
		t.Run(fmt.Sprint(threadID), func(t *testing.T) {
			p, b := newGeneralTopicPlatform(t)
			var received []*core.Message
			p.handler = func(platform core.Platform, msg *core.Message) {
				received = append(received, msg)
				if err := platform.Reply(context.Background(), msg.ReplyCtx, "Answer in the new topic"); err != nil {
					t.Error(err)
				}
			}
			opener := generalMessage(10, threadID)
			p.handleMessage(context.Background(), opener)
			if len(b.created) != 1 || len(b.copies) != 1 || len(received) != 1 {
				t.Fatalf("creates=%d copies=%d turns=%d; General must open a topic before dispatch", len(b.created), len(b.copies), len(received))
			}
			got := received[0]
			if got.SessionKey != "telegram:-100123:42:0" || got.ChannelKey != "-100123:42" || got.UserID != "7" || got.Content != opener.Text || got.MessageID != "10" {
				t.Fatalf("lost opener identity or topic routing: %+v", got)
			}
			if b.created[0].Name != opener.Text || b.copies[0].MessageID != 10 || b.copies[0].FromChatID != int64(-100123) || b.copies[0].ChatID != int64(-100123) || b.copies[0].MessageThreadID != 42 {
				t.Fatalf("incorrect topic or copied opener: %+v %+v", b.created, b.copies)
			}
			if len(b.sends) != 1 || b.sends[0].MessageThreadID != 42 || b.sends[0].ReplyParameters.MessageID != 201 {
				t.Fatalf("reply escaped new topic or quoted General's message: %+v", b.sends)
			}
			if opener.MessageThreadID != threadID || opener.ID != 10 {
				t.Fatal("mutated original update")
			}
		})
	}
}

func TestGeneralTopicOptions(t *testing.T) {
	for _, tc := range []struct {
		name    string
		enabled any
		chat    any
		want    bool
		wantErr bool
	}{
		{"default", nil, nil, false, false},
		{"disabled", false, nil, false, false},
		{"enabled", true, "-100123", true, false},
		{"missing destination", true, nil, false, true},
		{"private destination", true, "7", false, true},
		{"string flag", "true", "-100123", false, true},
		{"numeric flag", 1, "-100123", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := map[string]any{"token": "test-token", "allow_from": "7"}
			if tc.enabled != nil {
				opts["general_topic_auto_create"] = tc.enabled
			}
			if tc.chat != nil {
				opts["topic_sync_chat_id"] = tc.chat
			}
			got, err := New(opts)
			if (err != nil) != tc.wantErr {
				t.Fatalf("error=%v, wantError=%v", err, tc.wantErr)
			}
			if err == nil && got.(*Platform).autoGeneralTopics != tc.want {
				t.Fatal("incorrect default or flag")
			}
		})
	}
}

func TestGeneralTopicAutoCreate_RespectsExistingRoutingAndAccess(t *testing.T) {
	for _, tc := range []struct {
		name         string
		change       func(*Platform, *models.Message)
		wantDispatch bool
	}{
		{"disabled", func(p *Platform, m *models.Message) { p.autoGeneralTopics = false }, true},
		{"other group", func(p *Platform, m *models.Message) { m.Chat.ID = -100456 }, true},
		{"private chat", func(p *Platform, m *models.Message) { m.Chat.Type = models.ChatTypePrivate; m.Chat.ID = 7 }, true},
		{"non-forum", func(p *Platform, m *models.Message) { m.Chat.IsForum = false }, true},
		{"existing topic", func(p *Platform, m *models.Message) { m.MessageThreadID = 55 }, true},
		{"command", func(p *Platform, m *models.Message) { m.Text = "/help" }, true},
		{"mentioned command", func(p *Platform, m *models.Message) {
			m.Text = "/help@bridge"
			m.Entities = []models.MessageEntity{{Type: models.MessageEntityTypeBotCommand, Offset: 0, Length: 12}}
		}, true},
		{"unauthorized", func(p *Platform, m *models.Message) { m.From.ID = 12345 }, false},
		{"bot", func(p *Platform, m *models.Message) { m.From.IsBot = true }, false},
		{"not directed", func(p *Platform, m *models.Message) { p.groupReplyAll = false }, false},
		{"old", func(p *Platform, m *models.Message) { m.Date = int(core.StartTime.Add(-time.Hour).Unix()) }, false},
		{"service", func(p *Platform, m *models.Message) {
			m.Text = ""
			m.ForumTopicCreated = &models.ForumTopicCreated{Name: "topic"}
		}, false},
		{"unsupported content", func(p *Platform, m *models.Message) { m.Text = ""; m.Sticker = &models.Sticker{} }, false},
		{"no handler", func(p *Platform, m *models.Message) { p.handler = nil }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, b := newGeneralTopicPlatform(t)
			calls := 0
			p.handler = func(_ core.Platform, m *core.Message) { calls++ }
			m := generalMessage(10, 0)
			tc.change(p, m)
			p.handleMessage(context.Background(), m)
			if len(b.created) != 0 || len(b.copies) != 0 || (calls == 1) != tc.wantDispatch {
				t.Fatalf("creates=%d copies=%d turns=%d", len(b.created), len(b.copies), calls)
			}
		})
	}
}

func TestGeneralTopicAutoCreate_MediaOpenersKeepAttachments(t *testing.T) {
	for _, tc := range []struct {
		name  string
		set   func(*models.Message)
		check func(*core.Message) bool
	}{
		{"photo", func(m *models.Message) { m.Photo = []models.PhotoSize{{FileID: "photo"}} }, func(m *core.Message) bool { return len(m.Images) == 1 }},
		{"document", func(m *models.Message) { m.Document = &models.Document{FileID: "doc", FileName: "notes.txt"} }, func(m *core.Message) bool { return len(m.Files) == 1 }},
		{"voice", func(m *models.Message) { m.Voice = &models.Voice{FileID: "voice", MimeType: "audio/ogg"} }, func(m *core.Message) bool { return m.Audio != nil }},
		{"audio", func(m *models.Message) { m.Audio = &models.Audio{FileID: "audio", MimeType: "audio/mpeg"} }, func(m *core.Message) bool { return m.Audio != nil }},
		{"location", func(m *models.Message) { m.Location = &models.Location{Latitude: 12, Longitude: 34} }, func(m *core.Message) bool { return m.Location != nil && strings.Contains(m.ExtraContent, "12") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, b := newGeneralTopicPlatform(t)
			p.httpClient = &http.Client{Transport: topicFileTransport{}}
			var received *core.Message
			p.handler = func(_ core.Platform, m *core.Message) { received = m }
			m := generalMessage(10, 0)
			m.Text = ""
			tc.set(m)
			p.handleMessage(context.Background(), m)
			if received == nil || !tc.check(received) || received.ReplyCtx.(replyContext).threadID != 42 || len(b.copies) != 1 {
				t.Fatalf("lost media or topic: %+v copies=%v", received, b.copies)
			}
		})
	}
}

func TestGeneralTopicAutoCreate_SeparateConversationsAndTopicFollowups(t *testing.T) {
	p, b := newGeneralTopicPlatform(t)
	var received []*core.Message
	p.handler = func(_ core.Platform, m *core.Message) { received = append(received, m) }
	p.handleMessage(context.Background(), generalMessage(10, 0))
	p.handleMessage(context.Background(), generalMessage(11, 0))
	followup := generalMessage(12, 42)
	followup.From.ID = 8
	p.handleMessage(context.Background(), followup)
	p.handleCallbackQuery(context.Background(), &models.CallbackQuery{ID: "button", From: models.User{ID: 8}, Data: "cmd:/current",
		Message: models.MaybeInaccessibleMessage{Message: &models.Message{ID: 201, Chat: models.Chat{ID: -100123, Type: models.ChatTypeSupergroup}, MessageThreadID: 42}}})
	if len(b.created) != 2 || len(received) != 4 {
		t.Fatalf("topics=%d messages=%d", len(b.created), len(received))
	}
	for i, m := range received {
		want := "telegram:-100123:42:0"
		if i == 1 {
			want = "telegram:-100123:43:0"
		}
		if m.SessionKey != want {
			t.Fatalf("message %d routed to %s, want %s", i, m.SessionKey, want)
		}
	}
	restarted, _ := newGeneralTopicPlatform(t)
	rc, err := restarted.ReconstructReplyCtx(received[0].SessionKey)
	if err != nil || rc.(replyContext).threadID != 42 {
		t.Fatalf("restart lost reply destination: %+v %v", rc, err)
	}
}

func TestGeneralTopicAutoCreate_AlbumsAndDuplicateUpdates(t *testing.T) {
	p, b := newGeneralTopicPlatform(t)
	p.httpClient = &http.Client{Transport: topicFileTransport{}}
	var mu sync.Mutex
	var received []*core.Message
	p.handler = func(_ core.Platform, m *core.Message) { mu.Lock(); defer mu.Unlock(); received = append(received, m) }
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			m := generalMessage(10+i%4, 0)
			m.Text = ""
			m.Photo = []models.PhotoSize{{FileID: "photo"}}
			m.MediaGroupID = "album"
			p.handleMessage(context.Background(), m)
		}(i)
	}
	wg.Wait()
	if len(b.created) != 1 || len(b.copies) != 4 || len(received) != 4 {
		t.Fatalf("album split or duplicate: creates=%d copies=%d turns=%d", len(b.created), len(b.copies), len(received))
	}
	for _, m := range received {
		if m.ReplyCtx.(replyContext).threadID != 42 {
			t.Fatal("album member left topic")
		}
	}
}

func TestGeneralTopicAutoCreate_FailuresNeverStartGeneralTurn(t *testing.T) {
	for _, tc := range []struct {
		name string
		fail func(*Platform, *generalTopicBot)
	}{
		{"create", func(p *Platform, b *generalTopicBot) { b.err = errors.New("bot test-token unavailable") }},
		{"permission", func(p *Platform, b *generalTopicBot) { b.member.Administrator.CanManageTopics = false }},
		{"copy", func(p *Platform, b *generalTopicBot) { b.copyErr = errors.New("bot test-token unavailable") }},
		{"missing copy", func(p *Platform, b *generalTopicBot) { b.emptyCopy = true }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, b := newGeneralTopicPlatform(t)
			p.SetI18n(core.NewI18n(core.LangSpanish))
			p.handler = func(core.Platform, *core.Message) { t.Error("failed opener started agent turn") }
			tc.fail(p, b)
			p.handleMessage(context.Background(), generalMessage(10, 0))
			if len(b.sends) != 1 || b.sends[0].MessageThreadID != 0 || !strings.Contains(b.sends[0].Text, "No pude iniciar") || strings.Contains(b.sends[0].Text, "test-token") {
				t.Fatalf("missing translated failure notice: %+v", b.sends)
			}
		})
	}
	p, b := newGeneralTopicPlatform(t)
	b.getFileErr = errors.New("download failed")
	p.handler = func(core.Platform, *core.Message) { t.Error("failed download reached agent") }
	m := generalMessage(10, 0)
	m.Photo = []models.PhotoSize{{FileID: "photo"}}
	p.handleMessage(context.Background(), m)
	if len(b.created) != 0 {
		t.Fatal("created empty topic before downloading opener")
	}
}

func TestGeneralTopicAutoCreate_CopyRetryReusesCreatedTopic(t *testing.T) {
	p, b := newGeneralTopicPlatform(t)
	calls := 0
	p.handler = func(core.Platform, *core.Message) { calls++ }
	b.copyErr = errors.New("temporary failure")
	p.handleMessage(context.Background(), generalMessage(10, 0))
	b.copyErr = nil
	p.handleMessage(context.Background(), generalMessage(10, 0))
	p.handleMessage(context.Background(), generalMessage(10, 0))
	if len(b.created) != 1 || len(b.copies) != 2 || calls != 1 {
		t.Fatalf("retry duplicated work: creates=%d copies=%d turns=%d", len(b.created), len(b.copies), calls)
	}
}

func TestGeneralTopicAutoCreate_TitleUsesPromptOrLocalizedFallback(t *testing.T) {
	p, b := newGeneralTopicPlatform(t)
	p.groupReplyAll = false
	p.handler = func(core.Platform, *core.Message) {}
	m := generalMessage(10, 0)
	m.Text = "@bridge " + strings.Repeat("日", 140) + "  task"
	m.Entities = []models.MessageEntity{{Type: models.MessageEntityTypeMention, Offset: 0, Length: 7}}
	p.handleMessage(context.Background(), m)
	if len(b.created) != 1 || utf8.RuneCountInString(b.created[0].Name) != 128 || strings.Contains(b.created[0].Name, "@bridge") {
		t.Fatalf("incorrect prompt title: %+v", b.created)
	}
	p.groupReplyAll = true
	p.SetI18n(core.NewI18n(core.LangJapanese))
	m = generalMessage(11, 0)
	m.Text = ""
	m.Location = &models.Location{Latitude: 12, Longitude: 34}
	p.handleMessage(context.Background(), m)
	if len(b.created) != 2 || b.created[1].Name != p.noticeI18n().T(core.MsgUntitled) {
		t.Fatalf("incorrect fallback title: %+v", b.created)
	}
}

func TestGeneralTopicAutoCreate_ProductionWrappersCopyAndRetry(t *testing.T) {
	slept := recordSleeps(t)
	copies, creates := 0, 0
	client := pollingTestClient(func(req *http.Request, params url.Values) (*http.Response, error) {
		switch {
		case strings.HasSuffix(req.URL.Path, "/getMe"):
			return pollingTestResponse(&models.User{ID: 99, IsBot: true, Username: "bridge"})
		case strings.HasSuffix(req.URL.Path, "/getChat"):
			return pollingTestResponse(&models.ChatFullInfo{ID: -100123, Type: models.ChatTypeSupergroup, IsForum: true})
		case strings.HasSuffix(req.URL.Path, "/getChatMember"):
			return pollingTestResponse(&models.ChatMember{Type: models.ChatMemberTypeAdministrator, Administrator: &models.ChatMemberAdministrator{CanManageTopics: true}})
		case strings.HasSuffix(req.URL.Path, "/createForumTopic"):
			creates++
			return pollingTestResponse(&models.ForumTopic{MessageThreadID: 42})
		case strings.HasSuffix(req.URL.Path, "/copyMessage"):
			copies++
			if params.Get("chat_id") != "-100123" || params.Get("from_chat_id") != "-100123" || params.Get("message_thread_id") != "42" || params.Get("message_id") != "10" {
				t.Errorf("copy parameters: %v", params)
			}
			if copies == 1 {
				return &http.Response{StatusCode: 429, Body: io.NopCloser(strings.NewReader(`{"ok":false,"error_code":429,"description":"Too Many Requests","parameters":{"retry_after":1}}`))}, nil
			}
			return pollingTestResponse(&models.MessageID{ID: 201})
		default:
			return nil, fmt.Errorf("unexpected method: %s", req.URL.Path)
		}
	})
	bot, me, _, err := defaultNewBot("test-token", func(context.Context, *models.Update) {}, client)
	if err != nil {
		t.Fatal(err)
	}
	p, _ := newGeneralTopicPlatform(t)
	p.bot, p.selfUser = bot, me
	var received *core.Message
	p.handler = func(_ core.Platform, m *core.Message) { received = m }
	p.handleMessage(context.Background(), generalMessage(10, 0))
	if received == nil || received.ReplyCtx.(replyContext).messageID != 201 || creates != 1 || copies != 2 || len(*slept) != 1 {
		t.Fatalf("wrapper/retry failure: message=%+v creates=%d copies=%d sleeps=%v", received, creates, copies, *slept)
	}
}

func (b *generalTopicBot) SetMessageReaction(_ context.Context, params *tgbot.SetMessageReactionParams) (bool, error) {
	if b.receipts != nil {
		b.receipts <- params
	}
	return true, nil
}

func TestGeneralTopicAutoCreate_ReceiptMarksOriginalAfterDelivery(t *testing.T) {
	p, b := newGeneralTopicPlatform(t)
	p.readReceipts = true
	b.receipts = make(chan *tgbot.SetMessageReactionParams, 2)
	var delivered func()
	p.handler = func(_ core.Platform, m *core.Message) { delivered = m.OnDelivered }
	p.handleMessage(context.Background(), generalMessage(10, 0))
	if delivered == nil {
		t.Fatal("missing delivery receipt")
	}
	select {
	case <-b.receipts:
		t.Fatal("receipt before agent delivery")
	default:
	}
	delivered()
	delivered()
	select {
	case call := <-b.receipts:
		if call.MessageID != 10 || call.ChatID != int64(-100123) {
			t.Fatalf("receipt moved to the bot's copy: %+v", call)
		}
	case <-time.After(time.Second):
		t.Fatal("no delivery receipt")
	}
}
