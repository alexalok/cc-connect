package core

import (
	"log/slog"
	"strings"
	"unicode"
)

// SetBusyMessageMode selects how normal messages are delivered during a turn.
// Configuration validation rejects unknown values; the zero value keeps queueing.
func (e *Engine) SetBusyMessageMode(mode string) {
	e.busyMessageMode = strings.ToLower(strings.TrimSpace(mode))
}

func isQueueCommand(content string) bool {
	command, _, _ := strings.Cut(strings.TrimSpace(content), " ")
	if end := strings.IndexFunc(command, unicode.IsSpace); end >= 0 {
		command = command[:end]
	}
	return strings.HasPrefix(command, "/") && matchPrefix(strings.ToLower(command[1:]), builtinCommands) == "queue"
}

// prepareQueuedMessage returns false for a valid payload so handleMessage can
// dispatch it once, bypassing command/permission interpretation of the payload.
// Keep the original quoting, line breaks, attachments, and platform reply context.
func (e *Engine) prepareQueuedMessage(p Platform, msg *Message, raw string) bool {
	payload := ""
	if end := strings.IndexFunc(raw, unicode.IsSpace); end >= 0 {
		payload = strings.TrimSpace(raw[end:])
	}
	if payload == "" && len(msg.Images) == 0 && len(msg.Files) == 0 && msg.Location == nil {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgQueueEmpty))
		return true
	}
	if e.matchBannedWord(payload) != "" {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgBannedWordBlocked))
		return true
	}
	msg.Content = payload
	if msg.ExtraContent != "" {
		msg.Content = msg.ExtraContent + "\n" + payload
	}
	msg.forceQueue = true
	return false
}

// steerBusyMessage returns true when delivery was attempted or the message was
// handled. Startup, unsupported backends, and completed turns use the queue path.
func (e *Engine) steerBusyMessage(p Platform, msg *Message, session *Session, sessions *SessionManager, key string) bool {
	e.interactiveMu.Lock()
	state := e.interactiveStates[key]
	e.interactiveMu.Unlock()
	if state == nil {
		return false
	}
	state.mu.Lock()
	as := state.agentSession
	stopped := state.stopped
	state.mu.Unlock()
	steerer, ok := as.(MessageSteerer)
	if !ok || stopped || !as.Alive() {
		return false
	}
	prompt := e.buildSenderPrompt(msg.Content, msg.UserID, msg.UserName, msg.Platform, msg.SessionKey, msg.ChannelKey)
	accepted, err := steerer.SteerMessage(prompt, msg.MessageID, msg.Images, msg.Files)
	if err != nil {
		slog.Error("steer: delivery failed", "session", msg.SessionKey, "error", err)
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgMessageSteerFailed))
		return true
	}
	if !accepted {
		return false
	}
	if msg.OnDelivered != nil {
		delivered := msg.OnDelivered
		msg.OnDelivered = nil
		delivered()
	}
	state.mu.Lock()
	if msg.UserMessageTimeMs > state.lastSteeredUserMessageTimeMs {
		state.lastSteeredUserMessageTimeMs = msg.UserMessageTimeMs
	}
	state.mu.Unlock()
	session.TouchUserActivity()
	session.AddHistory("user", msg.Content)
	sessions.Save()
	runMessageAccepted(msg)
	e.reply(p, msg.ReplyCtx, e.i18n.T(MsgMessageSteered))
	return true
}
