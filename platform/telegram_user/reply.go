package telegram_user

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/gotd/td/telegram/query/dialogs"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
)

const maxMessageUnits = 4096

func (p *Platform) resolvePeer(ctx context.Context, api messageAPI, id int64) (tg.InputPeerClass, error) {
	p.mu.RLock()
	known := p.peers[id].input
	p.mu.RUnlock()
	if known != nil {
		return known, nil
	}
	if id < 0 && id > -channelIDOffset {
		return &tg.InputPeerChat{ChatID: -id}, nil
	}
	// Access hashes are account-specific and are not part of session keys.
	// Resolve them from dialogs after a restart, including archived dialogs,
	// without treating historical messages as new agent input.
	iter := dialogs.NewIterator(dialogs.QueryFunc(func(ctx context.Context, r dialogs.Request) (tg.MessagesDialogsClass, error) {
		return api.MessagesGetDialogs(ctx, &tg.MessagesGetDialogsRequest{
			OffsetDate: r.OffsetDate, OffsetID: r.OffsetID, OffsetPeer: r.OffsetPeer, Limit: r.Limit,
		})
	}), 100)
	for iter.Next(ctx) {
		e := iter.Value().Entities
		p.rememberEntities(tg.Entities{Users: e.Users(), Chats: e.Chats(), Channels: e.Channels()})
		p.mu.RLock()
		known = p.peers[id].input
		p.mu.RUnlock()
		if known != nil {
			return known, nil
		}
	}
	if err := iter.Err(); err != nil {
		return nil, fmt.Errorf("telegram_user: resolve peer: %w", err)
	}
	return nil, fmt.Errorf("telegram_user: chat %d is not in this account's dialogs", id)
}

func (p *Platform) Reply(ctx context.Context, raw any, content string) error {
	rc, ok := raw.(replyContext)
	if !ok {
		return fmt.Errorf("telegram_user: invalid reply context %T", raw)
	}
	for _, chunk := range splitText(content, maxMessageUnits) {
		if _, err := p.sendText(ctx, rc, chunk); err != nil {
			return err
		}
	}
	return nil
}

func (p *Platform) Send(ctx context.Context, raw any, content string) error {
	rc, ok := raw.(replyContext)
	if !ok {
		return fmt.Errorf("telegram_user: invalid reply context %T", raw)
	}
	rc.messageID = 0
	return p.Reply(ctx, rc, content)
}

func (p *Platform) sendText(ctx context.Context, rc replyContext, content string) (int, error) {
	api, err := p.getAPI()
	if err != nil {
		return 0, err
	}
	peer, err := p.resolvePeer(ctx, api, rc.chatID)
	if err != nil {
		return 0, err
	}
	var random [8]byte
	if _, err := rand.Read(random[:]); err != nil {
		return 0, fmt.Errorf("telegram_user: generate message ID: %w", err)
	}
	req := &tg.MessagesSendMessageRequest{
		Peer: peer, Message: content, RandomID: int64(binary.LittleEndian.Uint64(random[:])), NoWebpage: true,
	}
	if rc.messageID > 0 || rc.threadID > 0 {
		reply := &tg.InputReplyToMessage{ReplyToMsgID: rc.messageID}
		if reply.ReplyToMsgID == 0 {
			reply.ReplyToMsgID = rc.threadID
		}
		if rc.threadID > 0 && rc.threadID != reply.ReplyToMsgID && rc.threadID != 1 {
			reply.SetTopMsgID(rc.threadID)
		}
		req.SetReplyTo(reply)
	}
	result, err := api.MessagesSendMessage(ctx, req)
	if err != nil {
		return 0, fmt.Errorf("telegram_user: send message: %w", err)
	}
	return sentMessageID(result, req.RandomID), nil
}

func sentMessageID(result tg.UpdatesClass, randomID int64) int {
	var updates []tg.UpdateClass
	switch u := result.(type) {
	case *tg.UpdateShortSentMessage:
		return u.ID
	case *tg.Updates:
		updates = u.Updates
	case *tg.UpdatesCombined:
		updates = u.Updates
	}
	for _, update := range updates {
		if u, ok := update.(*tg.UpdateMessageID); ok && u.RandomID == randomID {
			return u.ID
		}
	}
	return 0
}

// splitText preserves the complete text, splitting on UTF-16 boundaries without
// splitting surrogate pairs. Telegram's limit is not a byte or rune count.
func splitText(text string, limit int) []string {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	var chunks []string
	start, units := 0, 0
	for pos, r := range text {
		size := 1
		if r > 0xffff {
			size = 2
		}
		if units+size > limit {
			chunks = append(chunks, text[start:pos])
			start, units = pos, 0
		}
		units += size
	}
	return append(chunks, text[start:])
}

func (p *Platform) SendPreviewStart(ctx context.Context, raw any, content string) (any, error) {
	rc, ok := raw.(replyContext)
	if !ok {
		return nil, fmt.Errorf("telegram_user: invalid reply context %T", raw)
	}
	if len(utf16.Encode([]rune(content))) > maxMessageUnits {
		return nil, fmt.Errorf("telegram_user: preview exceeds message limit")
	}
	id, err := p.sendText(ctx, rc, content)
	if err != nil {
		return nil, err
	}
	if id == 0 {
		return nil, fmt.Errorf("telegram_user: send preview returned no message ID")
	}
	rc.messageID = id
	return rc, nil
}

func (p *Platform) UpdateMessage(ctx context.Context, raw any, content string) error {
	rc, ok := raw.(replyContext)
	if !ok || rc.messageID <= 0 {
		return fmt.Errorf("telegram_user: invalid preview handle")
	}
	if len(utf16.Encode([]rune(content))) > maxMessageUnits {
		// Returning an error lets the engine send a complete, chunked final
		// reply. Truncating here would silently lose the end of the answer.
		return fmt.Errorf("telegram_user: preview exceeds message limit")
	}
	api, err := p.getAPI()
	if err != nil {
		return err
	}
	peer, err := p.resolvePeer(ctx, api, rc.chatID)
	if err != nil {
		return err
	}
	_, err = api.MessagesEditMessage(ctx, &tg.MessagesEditMessageRequest{
		Peer: peer, ID: rc.messageID, Message: content, NoWebpage: true,
	})
	if tgerr.Is(err, "MESSAGE_NOT_MODIFIED") {
		return nil
	}
	if err != nil {
		return fmt.Errorf("telegram_user: edit message: %w", err)
	}
	return nil
}

func (p *Platform) ProgressStyle() string     { return "compact" }
func (p *Platform) KeepPreviewOnFinish() bool { return true }

func (p *Platform) DeletePreviewMessage(ctx context.Context, raw any) error {
	rc, ok := raw.(replyContext)
	if !ok || rc.messageID <= 0 {
		return fmt.Errorf("telegram_user: invalid preview handle")
	}
	api, err := p.getAPI()
	if err != nil {
		return err
	}
	peer, err := p.resolvePeer(ctx, api, rc.chatID)
	if err != nil {
		return err
	}
	if channel, ok := peer.(*tg.InputPeerChannel); ok {
		_, err = api.ChannelsDeleteMessages(ctx, &tg.ChannelsDeleteMessagesRequest{
			Channel: &tg.InputChannel{ChannelID: channel.ChannelID, AccessHash: channel.AccessHash}, ID: []int{rc.messageID},
		})
	} else {
		_, err = api.MessagesDeleteMessages(ctx, &tg.MessagesDeleteMessagesRequest{Revoke: true, ID: []int{rc.messageID}})
	}
	if err != nil {
		return fmt.Errorf("telegram_user: delete preview: %w", err)
	}
	return nil
}

func (p *Platform) StartTyping(ctx context.Context, raw any) func() {
	rc, ok := raw.(replyContext)
	if !ok {
		return func() {}
	}
	ctx, cancel := context.WithCancel(ctx)
	go func() {
		ticker := time.NewTicker(4 * time.Second)
		defer ticker.Stop()
		for {
			if ctx.Err() != nil {
				return
			}
			api, err := p.getAPI()
			if err != nil {
				return
			}
			peer, err := p.resolvePeer(ctx, api, rc.chatID)
			if err != nil {
				slog.Warn("telegram_user: resolve typing target", "error", err)
				return
			}
			req := &tg.MessagesSetTypingRequest{Peer: peer, Action: &tg.SendMessageTypingAction{}}
			if rc.threadID > 0 {
				req.SetTopMsgID(rc.threadID)
			}
			if _, err := api.MessagesSetTyping(ctx, req); err != nil {
				if ctx.Err() == nil {
					slog.Warn("telegram_user: typing failed", "error", err)
				}
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return cancel
}
