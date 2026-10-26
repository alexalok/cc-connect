package telegram

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

func TestDeleteTopic_DeletesOnlyCurrentForumOrPrivateTopic(t *testing.T) {
	for _, rc := range []replyContext{{chatID: -100123, threadID: 42}, {chatID: 123, threadID: 1}} {
		t.Run(fmt.Sprint(rc), func(t *testing.T) {
			var calls atomic.Int32
			p := newTelegramTestPlatform(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if !strings.HasSuffix(r.URL.Path, "/deleteForumTopic") {
					t.Errorf("unexpected API: %s", r.URL.Path)
				}
				if err := r.ParseMultipartForm(1024); err != nil {
					t.Error(err)
				}
				if r.FormValue("chat_id") != strconv.FormatInt(rc.chatID, 10) || r.FormValue("message_thread_id") != strconv.Itoa(rc.threadID) {
					t.Errorf("wrong topic: %v", r.Form)
				}
				fmt.Fprint(w, `{"ok":true,"result":true}`)
			})
			deleted, err := p.DeleteTopic(context.Background(), rc)
			if err != nil || !deleted || calls.Load() != 1 {
				t.Fatalf("deleted=%v, calls=%d, error=%v", deleted, calls.Load(), err)
			}
		})
	}
}

func TestDeleteTopic_NoTopicOrGeneralDoesNotCallAPI(t *testing.T) {
	p := &Platform{}
	for _, rc := range []replyContext{{chatID: 123}, {chatID: -100123}, {chatID: -100123, threadID: 1}} {
		deleted, err := p.DeleteTopic(context.Background(), rc)
		if err != nil || deleted {
			t.Fatalf("context=%+v: deleted=%v, error=%v", rc, deleted, err)
		}
	}
	if _, err := p.DeleteTopic(context.Background(), "invalid"); err == nil {
		t.Fatal("expected invalid context error")
	}
}

func TestDeleteTopic_FailureIsReportedAndTokenRedacted(t *testing.T) {
	for _, response := range []string{
		`{"ok":false,"error_code":403,"description":"denied TEST_TOKEN"}`,
		`{"ok":true,"result":false}`,
	} {
		p := newTelegramTestPlatform(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, response) })
		p.token = "TEST_TOKEN"
		deleted, err := p.DeleteTopic(context.Background(), replyContext{chatID: 123, threadID: 42})
		if deleted || err == nil || strings.Contains(err.Error(), "TEST_TOKEN") {
			t.Fatalf("deleted=%v, error=%v", deleted, err)
		}
	}
}
