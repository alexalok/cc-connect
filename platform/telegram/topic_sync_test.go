package telegram

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/chenhg5/cc-connect/core"
	tgbot "github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

type topicSyncBot struct {
	stubTelegramBot
	created       []*tgbot.CreateForumTopicParams
	err           error
	chat          *models.ChatFullInfo
	chatErr       error
	member        *models.ChatMember
	memberErr     error
	memberQueries []*tgbot.GetChatMemberParams
}

func (b *topicSyncBot) GetChat(_ context.Context, params *tgbot.GetChatParams) (*models.ChatFullInfo, error) {
	if b.chat != nil || b.chatErr != nil {
		return b.chat, b.chatErr
	}
	return &models.ChatFullInfo{ID: params.ChatID.(int64), Type: models.ChatTypePrivate}, nil
}

func (b *topicSyncBot) GetChatMember(_ context.Context, params *tgbot.GetChatMemberParams) (*models.ChatMember, error) {
	b.memberQueries = append(b.memberQueries, params)
	return b.member, b.memberErr
}

func (b *topicSyncBot) CreateForumTopic(_ context.Context, params *tgbot.CreateForumTopicParams) (*models.ForumTopic, error) {
	b.created = append(b.created, params)
	return &models.ForumTopic{MessageThreadID: 42}, b.err
}

func TestTopicSyncOptionsAcceptPrivateOrGroupDestination(t *testing.T) {
	for _, tc := range []struct {
		name      string
		chat      any
		interval  any
		allow     string
		wantError bool
	}{
		{"disabled", nil, nil, "", false},
		{"string ID", "123", int64(5), "123", false},
		{"integer ID", int64(123), nil, "123,456", false},
		{"wildcard", "123", nil, "*", false},
		{"supergroup", "-100123", nil, "123,456", false},
		{"integer supergroup", int64(-100123), nil, "123", false},
		{"group wildcard", "-100123", nil, "*", false},
		{"group default allow list", "-100123", nil, "", false},
		{"zero", 0, nil, "*", true},
		{"empty", "", nil, "*", true},
		{"invalid", "owner", nil, "*", true},
		{"not allowed", "123", nil, "456", true},
		{"default allow list", "123", nil, "", false},
		{"interval zero", "123", 0, "*", true},
		{"interval fractional", "123", 1.5, "*", true},
		{"interval overflow", "123", int64(9223372036854775807), "*", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := map[string]any{"token": "test", "allow_from": tc.allow}
			if tc.chat != nil {
				opts["topic_sync_chat_id"] = tc.chat
			}
			if tc.interval != nil {
				opts["topic_sync_interval_seconds"] = tc.interval
			}
			platform, err := New(opts)
			if (err != nil) != tc.wantError {
				t.Fatalf("New() error = %v", err)
			}
			if err != nil {
				return
			}
			target, interval := platform.(core.SessionTopicCreator).SessionTopicSyncConfig()
			if tc.chat == nil {
				if target != "" {
					t.Fatalf("sync enabled without explicit destination: %s", target)
				}
			} else if target != fmt.Sprint(tc.chat) || interval <= 0 {
				t.Fatalf("config = %s, %v", target, interval)
			}
		})
	}
}

func TestCreateSessionTopic_ReplyUsesSameSessionKey(t *testing.T) {
	for _, shared := range []bool{false, true} {
		t.Run(fmt.Sprint(shared), func(t *testing.T) {
			bot := &topicSyncBot{}
			p := &Platform{bot: bot, topicSyncChatID: 123, shareSessionInChannel: shared, selfUser: &models.User{HasTopicsEnabled: true}}
			title := "  " + strings.Repeat("界", 140) + "\n title "
			key, err := p.CreateSessionTopic(context.Background(), title)
			if err != nil {
				t.Fatal(err)
			}
			if len(bot.created) != 1 || bot.created[0].ChatID != int64(123) || utf8.RuneCountInString(bot.created[0].Name) != 128 {
				t.Fatalf("unexpected topic creation: %+v", bot.created)
			}
			var received *core.Message
			p.handler = func(_ core.Platform, msg *core.Message) { received = msg }
			p.handleMessage(context.Background(), &models.Message{ID: 10, Date: int(time.Now().Unix()),
				Chat: models.Chat{ID: 123, Type: models.ChatTypePrivate}, From: &models.User{ID: 123}, MessageThreadID: 42, Text: "continue"})
			if received == nil || received.SessionKey != key {
				t.Fatalf("topic key %q does not match inbound message: %+v", key, received)
			}
			ctx, err := p.ReconstructReplyCtx(key)
			if err != nil {
				t.Fatal(err)
			}
			rc := ctx.(replyContext)
			if rc.chatID != 123 || rc.threadID != 42 {
				t.Fatalf("reply context = %+v", rc)
			}
		})
	}
}

func TestCreateSessionTopic_DisabledTopicsAndRedactedFailures(t *testing.T) {
	bot := &topicSyncBot{err: errors.New("request botsecret-token failed")}
	p := &Platform{bot: bot, token: "secret-token", topicSyncChatID: 123}
	if _, err := p.CreateSessionTopic(context.Background(), "chat"); err == nil || len(bot.created) != 0 {
		t.Fatal("created a topic without topic mode enabled")
	}
	p.selfUser = &models.User{HasTopicsEnabled: true}
	if _, err := p.CreateSessionTopic(context.Background(), "chat"); err == nil || strings.Contains(err.Error(), p.token) {
		t.Fatalf("unredacted error: %v", err)
	}
}

func TestCreateSessionTopic_ProductionWrappersForwardAndRetry(t *testing.T) {
	slept := recordSleeps(t)
	creates := 0
	client := pollingTestClient(func(req *http.Request, params url.Values) (*http.Response, error) {
		switch {
		case strings.HasSuffix(req.URL.Path, "/getMe"):
			return pollingTestResponse(&models.User{ID: 1, IsBot: true, HasTopicsEnabled: true})
		case strings.HasSuffix(req.URL.Path, "/getChat"):
			return pollingTestResponse(&models.ChatFullInfo{ID: 123, Type: models.ChatTypePrivate})
		case strings.HasSuffix(req.URL.Path, "/createForumTopic"):
			creates++
			if params.Get("chat_id") != "123" || params.Get("name") != "External chat" {
				t.Errorf("topic creation parameters: %v", params)
			}
			if creates == 1 {
				return &http.Response{StatusCode: 429, Body: io.NopCloser(strings.NewReader(`{"ok":false,"error_code":429,"description":"Too Many Requests","parameters":{"retry_after":1}}`))}, nil
			}
			return pollingTestResponse(&models.ForumTopic{MessageThreadID: 42})
		default:
			return nil, fmt.Errorf("unexpected method: %s", req.URL.Path)
		}
	})
	bot, me, _, err := defaultNewBot("test-token", func(context.Context, *models.Update) {}, client)
	if err != nil {
		t.Fatal(err)
	}
	p := &Platform{bot: bot, selfUser: me, topicSyncChatID: 123}
	key, err := p.CreateSessionTopic(context.Background(), "External chat")
	if err != nil {
		t.Fatal(err)
	}
	if key != "telegram:123:42:123" || creates != 2 || len(*slept) != 1 || (*slept)[0] != time.Second {
		t.Fatalf("key=%q creates=%d sleeps=%v", key, creates, *slept)
	}
}

func TestCreateSessionTopic_SupergroupMessagesCallbacksAndMirrorsShareTopic(t *testing.T) {
	for _, shared := range []bool{false, true} {
		t.Run(fmt.Sprint(shared), func(t *testing.T) {
			bot := &topicSyncBot{
				chat:   &models.ChatFullInfo{ID: -100123, Type: models.ChatTypeSupergroup, IsForum: true},
				member: &models.ChatMember{Type: models.ChatMemberTypeAdministrator, Administrator: &models.ChatMemberAdministrator{CanManageTopics: true}},
			}
			p := &Platform{bot: bot, topicSyncChatID: -100123, allowFrom: "123,456", groupReplyAll: true,
				shareSessionInChannel: shared, selfUser: &models.User{ID: 99, Username: "bridge"}}
			key, err := p.CreateSessionTopic(context.Background(), "External chat")
			if err != nil {
				t.Fatal(err)
			}
			if key != "telegram:-100123:42:0" || len(bot.created) != 1 || bot.created[0].ChatID != int64(-100123) {
				t.Fatalf("wrong supergroup topic: key=%s creates=%+v", key, bot.created)
			}
			if len(bot.memberQueries) != 1 || bot.memberQueries[0].ChatID != int64(-100123) || bot.memberQueries[0].UserID != 99 {
				t.Fatalf("did not inspect the bot's group rights: %+v", bot.memberQueries)
			}
			var received []*core.Message
			p.handler = func(_ core.Platform, msg *core.Message) { received = append(received, msg) }
			for _, user := range []int64{123, 456, 789} {
				p.handleMessage(context.Background(), &models.Message{ID: 10, Date: int(time.Now().Unix()),
					Chat: models.Chat{ID: -100123, Type: models.ChatTypeSupergroup, IsForum: true},
					From: &models.User{ID: user}, MessageThreadID: 42, Text: "continue"})
				// Telegram can omit IsForum on a callback's embedded chat.
				for _, data := range []string{"cmd:/current", "perm:allow"} {
					p.handleCallbackQuery(context.Background(), &models.CallbackQuery{ID: "callback", From: models.User{ID: user}, Data: data,
						Message: models.MaybeInaccessibleMessage{Message: &models.Message{ID: 11,
							Chat: models.Chat{ID: -100123, Type: models.ChatTypeSupergroup}, MessageThreadID: 42}}})
				}
			}
			if len(received) != 6 {
				t.Fatalf("lost allowed input or accepted unauthorized input: %+v", received)
			}
			for _, msg := range received {
				if msg.SessionKey != key || msg.ReplyCtx.(replyContext).threadID != 42 {
					t.Fatalf("reply or button left the linked topic: %+v", msg)
				}
			}
			// Reconstructed contexts are used by mirroring after a restart.
			restarted := &Platform{topicSyncChatID: -100123, shareSessionInChannel: shared}
			rctx, err := restarted.ReconstructReplyCtx(key)
			if err != nil || rctx.(replyContext).chatID != -100123 || rctx.(replyContext).threadID != 42 {
				t.Fatalf("mirror context = %+v, %v", rctx, err)
			}
			if p.buildSessionKey(-100123, 43, 123) == key {
				t.Fatal("different topics share a session")
			}
		})
	}
}

func TestTopicSyncGroupSharingDoesNotChangeOtherChats(t *testing.T) {
	p := &Platform{topicSyncChatID: -100123}
	for _, tc := range []struct {
		chat   int64
		thread int
		want   string
	}{
		{-100123, 42, "telegram:-100123:42:0"},
		{-100123, 0, "telegram:-100123"},
		{-100456, 42, "telegram:-100456:42:123"},
		{-100456, 0, "telegram:-100456:123"},
		{123, 42, "telegram:123:42:123"},
		{123, 0, "telegram:123:123"},
	} {
		key := p.buildSessionKey(tc.chat, tc.thread, 123)
		if key != tc.want {
			t.Errorf("key = %s, want %s", key, tc.want)
		}
		rctx, err := p.ReconstructReplyCtx(key)
		if err != nil || rctx.(replyContext).chatID != tc.chat || rctx.(replyContext).threadID != tc.thread {
			t.Errorf("reply context for %s = %+v, %v", key, rctx, err)
		}
	}
}

func TestCreateSessionTopic_RejectsUnsupportedDestinationsAndPermissions(t *testing.T) {
	for _, tc := range []struct {
		name               string
		chat               *models.ChatFullInfo
		member             *models.ChatMember
		chatErr, memberErr error
		want               string
	}{
		{name: "lookup error", chatErr: errors.New("botsecret-token unreachable"), want: "inspect topic sync destination"},
		{name: "wrong chat", chat: &models.ChatFullInfo{ID: -100456}, want: "no matching chat"},
		{name: "basic group", chat: &models.ChatFullInfo{ID: -100123, Type: models.ChatTypeGroup}, want: "private chat or a supergroup"},
		{name: "channel", chat: &models.ChatFullInfo{ID: -100123, Type: models.ChatTypeChannel}, want: "private chat or a supergroup"},
		{name: "topics disabled", chat: &models.ChatFullInfo{ID: -100123, Type: models.ChatTypeSupergroup}, want: "enable Topics"},
		{name: "rights lookup error", memberErr: errors.New("botsecret-token unreachable"), want: "inspect topic sync permissions"},
		{name: "no member", want: "Manage Topics"},
		{name: "ordinary member", member: &models.ChatMember{Type: models.ChatMemberTypeMember}, want: "Manage Topics"},
		{name: "missing admin data", member: &models.ChatMember{Type: models.ChatMemberTypeAdministrator}, want: "Manage Topics"},
		{name: "admin without topic rights", member: &models.ChatMember{Type: models.ChatMemberTypeAdministrator, Administrator: &models.ChatMemberAdministrator{}}, want: "Manage Topics"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.chat == nil && tc.chatErr == nil {
				tc.chat = &models.ChatFullInfo{ID: -100123, Type: models.ChatTypeSupergroup, IsForum: true}
			}
			bot := &topicSyncBot{chat: tc.chat, member: tc.member, chatErr: tc.chatErr, memberErr: tc.memberErr}
			p := &Platform{bot: bot, token: "secret-token", topicSyncChatID: -100123, selfUser: &models.User{ID: 99}}
			_, err := p.CreateSessionTopic(context.Background(), "External chat")
			if err == nil || !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), p.token) || len(bot.created) != 0 {
				t.Fatalf("error = %v, created = %d", err, len(bot.created))
			}
		})
	}
}

func TestCreateSessionTopic_SupergroupProductionWrappersForwardAndRetry(t *testing.T) {
	slept := recordSleeps(t)
	calls := map[string]int{}
	client := pollingTestClient(func(req *http.Request, params url.Values) (*http.Response, error) {
		method := req.URL.Path[strings.LastIndex(req.URL.Path, "/")+1:]
		calls[method]++
		if method != "getMe" && params.Get("chat_id") != "-100123" {
			t.Errorf("%s destination = %s", method, params.Get("chat_id"))
		}
		if (method == "getChat" || method == "getChatMember" || method == "createForumTopic") && calls[method] == 1 {
			return &http.Response{StatusCode: 429, Body: io.NopCloser(strings.NewReader(`{"ok":false,"error_code":429,"description":"Too Many Requests","parameters":{"retry_after":1}}`))}, nil
		}
		switch method {
		case "getMe":
			return pollingTestResponse(&models.User{ID: 99, IsBot: true}) // DM topics are disabled.
		case "getChat":
			return pollingTestResponse(&models.ChatFullInfo{ID: -100123, Type: models.ChatTypeSupergroup, IsForum: true})
		case "getChatMember":
			if params.Get("user_id") != "99" {
				t.Errorf("permission check user = %s", params.Get("user_id"))
			}
			return pollingTestResponse(&models.ChatMember{Type: models.ChatMemberTypeAdministrator, Administrator: &models.ChatMemberAdministrator{CanManageTopics: true}})
		case "createForumTopic":
			return pollingTestResponse(&models.ForumTopic{MessageThreadID: 42})
		case "sendMessage":
			if params.Get("message_thread_id") != "42" {
				t.Errorf("mirrored message escaped the topic: %v", params)
			}
			return pollingTestResponse(&models.Message{ID: 123})
		default:
			return nil, fmt.Errorf("unexpected method: %s", method)
		}
	})
	bot, me, _, err := defaultNewBot("test-token", func(context.Context, *models.Update) {}, client)
	if err != nil {
		t.Fatal(err)
	}
	p := &Platform{bot: bot, selfUser: me, topicSyncChatID: -100123}
	key, err := p.CreateSessionTopic(context.Background(), "External chat")
	if err != nil {
		t.Fatal(err)
	}
	if key != "telegram:-100123:42:0" || calls["getChat"] != 2 || calls["getChatMember"] != 2 || calls["createForumTopic"] != 2 || len(*slept) != 3 {
		t.Fatalf("key=%q calls=%v sleeps=%v", key, calls, *slept)
	}
	rctx, err := p.ReconstructReplyCtx(key)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Send(context.Background(), rctx, "Mirrored conversation"); err != nil {
		t.Fatal(err)
	}
	if calls["sendMessage"] != 1 {
		t.Fatalf("mirrored message not sent: %v", calls)
	}
}

func TestReconstructReplyCtx_SyncedSupergroupSurvivesDestinationChanges(t *testing.T) {
	for _, originallyShared := range []bool{false, true} {
		original := &Platform{topicSyncChatID: -100123, shareSessionInChannel: originallyShared}
		key := original.buildSessionKey(-100123, 42, 123)
		for _, destination := range []int64{0, 123, -100456} {
			for _, shared := range []bool{false, true} {
				restarted := &Platform{topicSyncChatID: destination, shareSessionInChannel: shared}
				ctx, err := restarted.ReconstructReplyCtx(key)
				if err != nil {
					t.Fatal(err)
				}
				rc := ctx.(replyContext)
				if rc.chatID != -100123 || rc.threadID != 42 {
					t.Fatalf("saved destination %s changed after config update: %+v", key, rc)
				}
			}
		}
	}
	// Enabling sync must not reinterpret an older per-user General-topic key.
	p := &Platform{topicSyncChatID: -100123}
	ctx, err := p.ReconstructReplyCtx("telegram:-100123:123")
	if err != nil || ctx.(replyContext).threadID != 0 {
		t.Fatalf("old General-topic destination was reinterpreted: %+v, %v", ctx, err)
	}
}
