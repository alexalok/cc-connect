package telegram

import (
	"context"
	"log/slog"
	"time"

	"github.com/chenhg5/cc-connect/core"
	tgbot "github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

// Telegram clears chat actions after five seconds, or when a message arrives.
// Leave time for network latency and refresh after progress messages as well.
const typingInterval = 3 * time.Second
const typingRequestTimeout = 3 * time.Second

type typingSession struct {
	target  replyContext
	refresh chan struct{}
	cancel  context.CancelFunc
	done    chan struct{} // closed by the typing loop after all requests have ended
}

// StartTyping keeps the indicator active until stopped, including across
// temporary disconnects. The returned stop function is safe to call repeatedly.
func (p *Platform) StartTyping(ctx context.Context, rctx any) (stop func()) {
	rc, ok := rctx.(replyContext)
	if !ok || ctx.Err() != nil {
		return func() {}
	}
	ctx, cancel := context.WithCancel(ctx)
	s := &typingSession{target: rc, refresh: make(chan struct{}, 1), cancel: cancel, done: make(chan struct{})}

	p.mu.Lock()
	if p.stopping {
		p.mu.Unlock()
		cancel()
		return func() {}
	}
	if p.typing == nil {
		p.typing = make(map[*typingSession]struct{})
	}
	p.typing[s] = struct{}{}
	newTicker := p.newTypingTicker
	p.mu.Unlock()
	if newTicker == nil {
		newTicker = func(d time.Duration) typingTicker { return &stdlibTypingTicker{Ticker: time.NewTicker(d)} }
	}

	p.sendTyping(ctx, rc)
	go func() {
		defer close(s.done)
		defer cancel()
		defer func() {
			p.mu.Lock()
			delete(p.typing, s)
			p.mu.Unlock()
		}()
		ticker := newTicker(typingInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C():
			case <-s.refresh:
			}
			if ctx.Err() != nil {
				return
			}
			p.sendTyping(ctx, rc)
		}
	}()
	return func() {
		cancel()
		<-s.done
	}
}

func (p *Platform) sendTyping(ctx context.Context, rc replyContext) {
	if ctx.Err() != nil {
		return
	}
	bot, err := p.connectedBot("typing")
	if err != nil {
		// Polling reconnects independently; the next heartbeat will retry.
		slog.Debug("telegram: typing waiting for reconnect", "error", err)
		return
	}
	requestCtx, cancel := context.WithTimeout(ctx, typingRequestTimeout)
	defer cancel()
	_, err = bot.SendChatAction(requestCtx, &tgbot.SendChatActionParams{
		ChatID: rc.chatID, MessageThreadID: rc.threadID, Action: models.ChatActionTyping,
	})
	if err != nil && ctx.Err() == nil {
		slog.Debug("telegram: typing send failed", "error", core.RedactToken(err.Error(), p.token))
	}
}

// refreshTyping wakes only active turns in the message's chat and topic. It
// never blocks message delivery on a chat action, and coalesces pending wakes.
func (p *Platform) refreshTyping(rc replyContext) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	for s := range p.typing {
		if s.target.chatID == rc.chatID && s.target.threadID == rc.threadID {
			select {
			case s.refresh <- struct{}{}:
			default:
			}
		}
	}
}
