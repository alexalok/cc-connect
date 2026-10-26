package telegram

import (
	"context"
	"errors"
	"fmt"
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
	created []*tgbot.CreateForumTopicParams
	err     error
}

func (b *topicSyncBot) CreateForumTopic(_ context.Context, params *tgbot.CreateForumTopicParams) (*models.ForumTopic, error) {
	b.created = append(b.created, params)
	return &models.ForumTopic{MessageThreadID: 42}, b.err
}

func TestTopicSyncOptionsRequireExplicitAllowedPrivateChat(t *testing.T) {
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
		{"group", "-123", nil, "*", true},
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
			} else if target != "123" || interval <= 0 {
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
