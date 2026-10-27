package telegram

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/go-telegram/bot/models"
)

func TestDeleteSessionTopic_ExternalArchiveUsesProductionWrappers(t *testing.T) {
	for _, shared := range []bool{false, true} {
		t.Run(fmt.Sprint(shared), func(t *testing.T) {
			slept := recordSleeps(t)
			calls := 0
			client := pollingTestClient(func(req *http.Request, params url.Values) (*http.Response, error) {
				if strings.HasSuffix(req.URL.Path, "/getMe") {
					return pollingTestResponse(&models.User{ID: 1, IsBot: true, HasTopicsEnabled: true})
				}
				if !strings.HasSuffix(req.URL.Path, "/deleteForumTopic") {
					return nil, fmt.Errorf("unexpected method: %s", req.URL.Path)
				}
				calls++
				if params.Get("chat_id") != "123" || params.Get("message_thread_id") != "42" {
					t.Errorf("wrong topic: %v", params)
				}
				if calls == 1 {
					return &http.Response{StatusCode: 429, Body: io.NopCloser(strings.NewReader(`{"ok":false,"error_code":429,"description":"Too Many Requests","parameters":{"retry_after":1}}`))}, nil
				}
				return pollingTestResponse(true)
			})
			bot, _, _, err := defaultNewBot("test-token", func(context.Context, *models.Update) {}, client)
			if err != nil {
				t.Fatal(err)
			}
			p := &Platform{bot: bot, topicSyncChatID: 123, shareSessionInChannel: shared}
			deleted, err := p.DeleteSessionTopic(context.Background(), p.buildSessionKey(123, 42, 123))
			if err != nil || !deleted || calls != 2 || len(*slept) != 1 || (*slept)[0] != time.Second {
				t.Fatalf("deleted=%v calls=%d sleeps=%v err=%v", deleted, calls, *slept, err)
			}
		})
	}
}

func TestDeleteSessionTopic_OnlyConfiguredDestinationTopics(t *testing.T) {
	for _, shared := range []bool{false, true} {
		p := &Platform{topicSyncChatID: 123, shareSessionInChannel: shared}
		for _, key := range []string{p.buildSessionKey(123, 0, 123), p.buildSessionKey(456, 42, 456), p.buildSessionKey(-100123, 42, 123), "other:123:42"} {
			if deleted, err := p.DeleteSessionTopic(context.Background(), key); err != nil || deleted {
				t.Fatalf("out of scope %s: deleted=%v err=%v", key, deleted, err)
			}
		}
		p.topicSyncChatID = 0
		if deleted, err := p.DeleteSessionTopic(context.Background(), p.buildSessionKey(123, 42, 123)); err != nil || deleted {
			t.Fatalf("disabled: deleted=%v err=%v", deleted, err)
		}
	}
}

func TestDeleteSessionTopic_AlreadyDeletedIsIdempotentAndErrorsAreRedacted(t *testing.T) {
	for _, tc := range []struct {
		response string
		deleted  bool
	}{
		{`{"ok":false,"error_code":400,"description":"Bad Request: TOPIC_ID_INVALID"}`, true},
		{`{"ok":false,"error_code":400,"description":"Bad Request: chat not found TEST_TOKEN"}`, false},
		{`{"ok":false,"error_code":403,"description":"denied TEST_TOKEN"}`, false},
		{`{"ok":true,"result":false}`, false},
	} {
		p := newTelegramTestPlatform(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, tc.response) })
		p.topicSyncChatID, p.token = 123, "TEST_TOKEN"
		deleted, err := p.DeleteSessionTopic(context.Background(), "telegram:123:42:123")
		if deleted != tc.deleted || (err == nil) != tc.deleted {
			t.Fatalf("response=%s deleted=%v err=%v", tc.response, deleted, err)
		}
		if err != nil && strings.Contains(err.Error(), "TEST_TOKEN") {
			t.Fatalf("leaked token: %v", err)
		}
	}
}
