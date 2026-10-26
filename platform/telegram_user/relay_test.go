package telegram_user

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/chenhg5/cc-connect/core"
	"github.com/gotd/td/tg"
)

func TestRelayVisibility_PreservesChatAndTopic(t *testing.T) {
	for _, tc := range []struct {
		name, key string
		chat      int64
		topic     int
	}{
		{"private", "telegram_user:42:42", 42, 0},
		{"basic group", "telegram_user:-7:42", -7, 0},
		{"supergroup", "telegram_user:-1000000000009:shared", -1000000000009, 0},
		{"forum topic", "telegram_user:-1000000000009:12:42", -1000000000009, 12},
		{"shared topic", "telegram_user:-1000000000009:12:shared", -1000000000009, 12},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rm := core.NewRelayManager("")
			apis := map[string]*fakeAPI{}
			for _, name := range []string{"source", "target"} {
				p, _ := testPlatform(t)
				api := &fakeAPI{}
				p.api = api
				p.rememberEntities(testEntities())
				engine := core.NewEngine(name, journeyAgent{}, []core.Platform{p}, "", core.LangEnglish)
				t.Cleanup(func() { _ = engine.Stop() })
				rm.RegisterEngine(name, engine)
				apis[name] = api
			}
			rm.Bind("telegram_user", strconv.FormatInt(tc.chat, 10), map[string]string{"source": "Source", "target": "Target"})
			response, err := rm.Send(context.Background(), core.RelayRequest{
				From: "source", To: "target", SessionKey: tc.key, Message: "relay question",
			})
			if err != nil || response.Response != "answer: relay question" {
				t.Fatalf("relay response: %+v, %v", response, err)
			}
			for name, api := range apis {
				if len(api.sent) != 1 || !strings.Contains(api.sent[0].Message, "relay question") {
					t.Fatalf("%s visibility echo missing: %+v", name, api.sent)
				}
				req := api.sent[0]
				var gotChat int64
				switch peer := req.Peer.(type) {
				case *tg.InputPeerUser:
					gotChat = peer.UserID
				case *tg.InputPeerChat:
					gotChat = -peer.ChatID
				case *tg.InputPeerChannel:
					gotChat = -channelIDOffset - peer.ChannelID
				}
				if gotChat != tc.chat {
					t.Fatalf("%s echo sent to chat %d, want %d", name, gotChat, tc.chat)
				}
				if tc.topic == 0 {
					if req.ReplyTo != nil {
						t.Fatalf("%s echo unexpectedly threaded: %+v", name, req.ReplyTo)
					}
				} else if reply, ok := req.ReplyTo.(*tg.InputReplyToMessage); !ok || reply.ReplyToMsgID != tc.topic {
					t.Fatalf("%s echo lost its forum topic: %+v", name, req.ReplyTo)
				}
			}
		})
	}
}
