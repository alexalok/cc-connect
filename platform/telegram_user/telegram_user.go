// Package telegram_user connects a regular Telegram account through MTProto.
package telegram_user

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/chenhg5/cc-connect/core"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/updates"
	"github.com/gotd/td/telegram/updates/hook"
	"github.com/gotd/td/tg"
)

func init() { core.RegisterPlatform("telegram_user", New) }

type messageAPI interface {
	MessagesSendMessage(context.Context, *tg.MessagesSendMessageRequest) (tg.UpdatesClass, error)
	MessagesEditMessage(context.Context, *tg.MessagesEditMessageRequest) (tg.UpdatesClass, error)
	MessagesSetTyping(context.Context, *tg.MessagesSetTypingRequest) (bool, error)
	MessagesGetDialogs(context.Context, *tg.MessagesGetDialogsRequest) (tg.MessagesDialogsClass, error)
	MessagesDeleteMessages(context.Context, *tg.MessagesDeleteMessagesRequest) (*tg.MessagesAffectedMessages, error)
	ChannelsDeleteMessages(context.Context, *tg.ChannelsDeleteMessagesRequest) (*tg.MessagesAffectedMessages, error)
}

type connectionRunner func(context.Context, func(messageAPI, *tg.User)) error

type Platform struct {
	credentials           credentials
	allowFrom             string
	allowChats            string
	groupReplyAll         bool
	shareSessionInChannel bool
	rateLimit             core.RateLimitCfg

	mu      sync.RWMutex
	api     messageAPI
	self    *tg.User
	handler core.MessageHandler
	cancel  context.CancelFunc
	done    chan struct{} // closed only by the connection goroutine
	run     connectionRunner
	peers   map[int64]peerInfo // Bot API-style chat IDs, including -100... channels
	dedup   core.MessageDedup
	limiter *core.RateLimiter
}

func New(opts map[string]any) (core.Platform, error) {
	c, err := parseCredentials(opts)
	if err != nil {
		return nil, err
	}
	allowFrom, _ := opts["allow_from"].(string)
	allowFrom = strings.TrimSpace(allowFrom)
	if err := validateIDList("allow_from", allowFrom, true, true); err != nil {
		return nil, err
	}
	allowChats, _ := opts["allow_chats"].(string)
	allowChats = strings.TrimSpace(allowChats)
	if err := validateIDList("allow_chats", allowChats, false, false); err != nil {
		return nil, err
	}
	rateLimit, err := parseRateLimit(opts)
	if err != nil {
		return nil, err
	}
	p := &Platform{credentials: c, allowFrom: allowFrom, allowChats: allowChats, peers: make(map[int64]peerInfo), rateLimit: rateLimit}
	p.groupReplyAll, _ = opts["group_reply_all"].(bool)
	p.shareSessionInChannel, _ = opts["share_session_in_channel"].(bool)
	p.run = p.runClient
	return p, nil
}

func (p *Platform) Name() string { return "telegram_user" }

func (p *Platform) Start(handler core.MessageHandler) error {
	if handler == nil {
		return fmt.Errorf("telegram_user: message handler is required")
	}
	if _, err := os.Stat(p.credentials.sessionFile); err != nil {
		return fmt.Errorf("telegram_user: read session (run cc-connect telegram-user login first): %w", err)
	}
	p.mu.Lock()
	if p.cancel != nil {
		p.mu.Unlock()
		return fmt.Errorf("telegram_user: already started")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	p.stopLimiterLocked()
	p.limiter = core.NewRateLimiter(p.rateLimit.MaxMessages, p.rateLimit.Window)
	p.cancel, p.done, p.handler = cancel, done, handler
	p.mu.Unlock()

	ready := make(chan error, 1)
	go p.serve(ctx, done, ready)
	timer := time.NewTimer(45 * time.Second)
	defer timer.Stop()
	select {
	case err := <-ready:
		if err != nil {
			cancel()
		}
		return err
	case <-timer.C:
		cancel()
		return fmt.Errorf("telegram_user: connection timed out")
	case <-ctx.Done():
		return fmt.Errorf("telegram_user: start: %w", ctx.Err())
	}
}

func (p *Platform) serve(ctx context.Context, done chan struct{}, ready chan<- error) {
	defer close(done)
	defer func() {
		p.mu.Lock()
		p.stopLimiterLocked()
		p.api, p.cancel, p.self, p.handler = nil, nil, nil, nil
		p.mu.Unlock()
	}()
	err := p.run(ctx, func(api messageAPI, self *tg.User) {
		p.mu.Lock()
		if ctx.Err() != nil {
			p.mu.Unlock()
			return
		}
		p.api, p.self = api, self
		p.mu.Unlock()
		slog.Info("telegram_user: connected", "user_id", self.ID)
		ready <- nil
	})
	if err == nil {
		err = fmt.Errorf("telegram_user: connection closed")
	}
	if ctx.Err() == nil {
		slog.Error("telegram_user: connection stopped", "error", core.RedactToken(err.Error(), p.credentials.appHash))
	}
	select {
	case ready <- fmt.Errorf("telegram_user: connect: %w", err):
	default:
	}
}

func (p *Platform) runClient(ctx context.Context, ready func(messageAPI, *tg.User)) error {
	dispatcher := tg.NewUpdateDispatcher()
	dispatcher.OnNewMessage(func(ctx context.Context, e tg.Entities, update *tg.UpdateNewMessage) error {
		p.handleMessage(ctx, e, update.Message)
		return nil
	})
	dispatcher.OnNewChannelMessage(func(ctx context.Context, e tg.Entities, update *tg.UpdateNewChannelMessage) error {
		p.handleMessage(ctx, e, update.Message)
		return nil
	})
	// The manager expands short updates and recovers sequence gaps after a
	// reconnect. A raw dispatcher alone would miss ordinary private messages.
	manager := updates.New(updates.Config{Handler: dispatcher})
	opts := p.credentials.clientOptions()
	opts.UpdateHandler = manager
	// Our own sends/edits/deletes advance Telegram's update sequence too.
	opts.Middlewares = []telegram.Middleware{hook.UpdateHook(manager.Handle), hook.AffectedHook(manager)}
	client := telegram.NewClient(p.credentials.appID, p.credentials.appHash, opts)
	return client.Run(ctx, func(ctx context.Context) error {
		status, err := client.Auth().Status(ctx)
		if err != nil {
			return fmt.Errorf("check authorization: %w", err)
		}
		if !status.Authorized {
			return fmt.Errorf("session is not authorized; run cc-connect telegram-user login")
		}
		if status.User.Bot {
			return fmt.Errorf("session belongs to a bot; use a regular user account")
		}
		return manager.Run(ctx, client.API(), status.User.ID, updates.AuthOptions{
			OnStart: func(context.Context) { ready(client.API(), status.User) },
		})
	})
}

func (p *Platform) Stop() error {
	p.mu.Lock()
	p.stopLimiterLocked()
	cancel, done := p.cancel, p.done
	p.mu.Unlock()
	if cancel == nil {
		return nil
	}
	cancel()
	select {
	case <-done:
		return nil
	case <-time.After(10 * time.Second):
		return fmt.Errorf("telegram_user: timed out stopping connection")
	}
}

func (p *Platform) getAPI() (messageAPI, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.api == nil {
		return nil, fmt.Errorf("telegram_user: not connected")
	}
	return p.api, nil
}
