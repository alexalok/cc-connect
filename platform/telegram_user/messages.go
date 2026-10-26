package telegram_user

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/chenhg5/cc-connect/core"
	"github.com/gotd/td/tg"
)

const channelIDOffset int64 = 1000000000000

type peerInfo struct {
	input     tg.InputPeerClass
	name      string
	broadcast bool
}

type replyContext struct {
	chatID    int64
	threadID  int
	messageID int
}

func chatID(peer tg.PeerClass) int64 {
	switch p := peer.(type) {
	case *tg.PeerUser:
		return p.UserID
	case *tg.PeerChat:
		return -p.ChatID
	case *tg.PeerChannel:
		return -channelIDOffset - p.ChannelID
	default:
		return 0
	}
}

func (p *Platform) rememberEntities(e tg.Entities) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for id, u := range e.Users {
		if !u.Min && u.AccessHash != 0 {
			name := strings.TrimSpace(u.FirstName + " " + u.LastName)
			if u.Username != "" {
				name = u.Username
			}
			p.peers[id] = peerInfo{input: &tg.InputPeerUser{UserID: id, AccessHash: u.AccessHash}, name: name}
		}
	}
	for id, c := range e.Chats {
		p.peers[-id] = peerInfo{input: c.AsInputPeer(), name: c.Title}
	}
	for id, c := range e.Channels {
		if !c.Min && c.AccessHash != 0 {
			p.peers[-channelIDOffset-id] = peerInfo{input: &tg.InputPeerChannel{ChannelID: id, AccessHash: c.AccessHash}, name: c.Title, broadcast: c.Broadcast}
		}
	}
}

func (p *Platform) handleMessage(ctx context.Context, e tg.Entities, incoming tg.MessageClass) {
	msg, ok := incoming.(*tg.Message)
	if !ok || msg.Out || msg.Post || strings.TrimSpace(msg.Message) == "" || core.IsOldMessage(time.Unix(int64(msg.Date), 0)) {
		return
	}
	p.mu.RLock()
	self, handler := p.self, p.handler
	p.mu.RUnlock()
	if self == nil || handler == nil || ctx.Err() != nil {
		return
	}
	sender, ok := msg.FromID.(*tg.PeerUser)
	if !ok {
		// Private messages may omit from_id; their peer is the sender.
		if peer, private := msg.PeerID.(*tg.PeerUser); private && msg.FromID == nil {
			sender = peer
		} else {
			return // Anonymous/channel senders are not user identities.
		}
	}
	uid := strconv.FormatInt(sender.UserID, 10)
	id := chatID(msg.PeerID)
	if id == 0 || sender.UserID == self.ID || sender.UserID == 777000 ||
		!core.AllowList(p.allowFrom, uid) || !core.AllowList(p.allowChats, strconv.FormatInt(id, 10)) {
		return
	}
	p.rememberEntities(e)
	p.mu.RLock()
	peer, user := p.peers[id], p.peers[sender.UserID]
	p.mu.RUnlock()
	if peer.broadcast {
		return
	}
	content, mentioned := stripMention(msg.Message, msg.Entities, self)
	if id < 0 && !p.groupReplyAll && !mentioned {
		return
	}
	if strings.TrimSpace(content) == "" || p.dedup.IsDuplicate(fmt.Sprintf("%d:%d", id, msg.ID)) {
		return
	}
	if !p.allowSender(uid) {
		return
	}
	threadID := 0
	if reply, ok := msg.ReplyTo.(*tg.MessageReplyHeader); ok && reply.ForumTopic {
		threadID = reply.ReplyToTopID
		if threadID == 0 {
			threadID = reply.ReplyToMsgID
		}
	}
	channelKey := strconv.FormatInt(id, 10)
	if threadID != 0 {
		channelKey += ":" + strconv.Itoa(threadID)
	}
	sessionKey := p.Name() + ":" + channelKey
	if !p.shareSessionInChannel || id > 0 {
		sessionKey += ":" + uid
	} else {
		sessionKey += ":shared"
	}
	if user.name == "" {
		user.name = uid
	}
	message := &core.Message{
		SessionKey: sessionKey, Platform: p.Name(), MessageID: strconv.Itoa(msg.ID),
		ChannelID: strconv.FormatInt(id, 10), ChannelKey: channelKey, UserID: uid,
		UserName: user.name, ChatName: peer.name, Content: content,
		ReplyCtx:          replyContext{chatID: id, threadID: threadID, messageID: msg.ID},
		UserMessageTimeMs: int64(msg.Date) * 1000,
	}
	go handler(p, message)
}

// Telegram entity offsets are UTF-16 code units, not UTF-8 bytes or Go runes.
func stripMention(text string, entities []tg.MessageEntityClass, self *tg.User) (string, bool) {
	units := utf16.Encode([]rune(text))
	remove := make([]bool, len(units))
	mentioned := false
	for _, entity := range entities {
		offset, length, match := 0, 0, false
		switch e := entity.(type) {
		case *tg.MessageEntityMention:
			offset, length = e.Offset, e.Length
			if offset >= 0 && length > 0 && offset <= len(units)-length {
				match = self.Username != "" && strings.EqualFold(string(utf16.Decode(units[offset:offset+length])), "@"+self.Username)
			}
		case *tg.MessageEntityMentionName:
			offset, length, match = e.Offset, e.Length, e.UserID == self.ID
		}
		if !match || offset < 0 || length <= 0 || offset > len(units)-length {
			continue
		}
		mentioned = true
		for i := offset; i < offset+length; i++ {
			remove[i] = true
		}
	}
	filtered := make([]uint16, 0, len(units))
	for i, unit := range units {
		if !remove[i] {
			filtered = append(filtered, unit)
		}
	}
	return strings.TrimSpace(string(utf16.Decode(filtered))), mentioned
}

func (p *Platform) ReconstructReplyCtx(key string) (any, error) {
	parts := strings.Split(key, ":")
	if (len(parts) != 3 && len(parts) != 4) || parts[0] != p.Name() {
		return nil, fmt.Errorf("telegram_user: invalid session key")
	}
	id, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || id == 0 {
		return nil, fmt.Errorf("telegram_user: invalid chat ID in session key")
	}
	if parts[len(parts)-1] != "shared" {
		uid, err := strconv.ParseInt(parts[len(parts)-1], 10, 64)
		if err != nil || uid <= 0 {
			return nil, fmt.Errorf("telegram_user: invalid user ID in session key")
		}
	}
	rc := replyContext{chatID: id}
	if len(parts) == 4 {
		rc.threadID, err = strconv.Atoi(parts[2])
		if err != nil || rc.threadID <= 0 {
			return nil, fmt.Errorf("telegram_user: invalid topic ID in session key")
		}
	}
	return rc, nil
}

func (p *Platform) ChannelKeyFromSessionKey(key string) (string, bool) {
	raw, err := p.ReconstructReplyCtx(key)
	if err != nil {
		return "", false
	}
	rc := raw.(replyContext)
	key = strconv.FormatInt(rc.chatID, 10)
	if rc.threadID > 0 {
		key += ":" + strconv.Itoa(rc.threadID)
	}
	return key, true
}
