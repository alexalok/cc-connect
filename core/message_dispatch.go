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

// deferBusyMessage serializes steering and subsequent user input outside the
// platform callback. This includes /queue so a late steering fallback cannot
// be overtaken by newer queued input. Control commands bypass this dispatcher.
func (e *Engine) deferBusyMessage(p Platform, msg *Message, sessions *SessionManager, agent Agent, key, workspace string) bool {
	session := sessions.FindByID(sessions.ActiveSessionID(msg.SessionKey))
	if session == nil {
		return false
	}
	e.interactiveMu.Lock()
	state := e.interactiveStates[key]
	e.interactiveMu.Unlock()
	if state == nil {
		return false
	}
	state.mu.Lock()
	as := state.agentSession
	_, supported := as.(MessageSteerer)
	if as == nil || state.stopped || (state.pendingSteering == 0 && (msg.forceQueue || !supported || !as.Alive() || !session.Busy())) {
		state.mu.Unlock()
		return false
	}
	if state.pendingSteering >= e.maxQueuedMessages {
		depth := state.pendingSteering
		state.mu.Unlock()
		e.reply(p, msg.ReplyCtx, e.i18n.Tf(MsgQueueFull, depth))
		return true
	}
	previous := state.steeringTail
	done := make(chan struct{})
	state.steeringTail = done
	state.pendingSteering++
	if state.deferredMessages == nil {
		state.deferredMessages = make(map[*Message]bool)
	}
	state.deferredMessages[msg] = false
	messageID := state.currentMessageID
	state.mu.Unlock()
	stoppedSignal := state.stopSignal()
	go func() {
		defer func() {
			state.mu.Lock()
			delete(state.deferredMessages, msg)
			state.pendingSteering--
			if state.steeringTail == done {
				state.steeringTail = nil
			}
			state.mu.Unlock()
			close(done)
			if !session.Busy() {
				e.scheduleAgentSessionIdleClose(key, state)
			}
		}()
		if previous != nil {
			select {
			case <-previous:
			case <-e.ctx.Done():
				return
			case <-stoppedSignal:
				e.reply(p, msg.ReplyCtx, e.i18n.T(MsgMessageSteerFailed))
				return
			}
		}
		e.interactiveMu.Lock()
		current := e.interactiveStates[key] == state
		state.mu.Lock()
		stopped := state.stopped
		recalled := state.deferredMessages[msg]
		sameTurn := state.currentMessageID == messageID
		state.mu.Unlock()
		e.interactiveMu.Unlock()
		if e.ctx.Err() != nil || recalled {
			return
		}
		if !current || stopped || !as.Alive() || sessions.ActiveSessionID(msg.SessionKey) != session.ID || session.IsArchived() || session.isArchiving() {
			e.reply(p, msg.ReplyCtx, e.i18n.T(MsgMessageSteerFailed))
			return
		}
		if !sameTurn {
			msg.forceQueue = true
		}
		e.dispatchUserMessageNow(p, msg, sessions, agent, key, workspace, true)
	}()
	return true
}

// claimUserTurn registers recall ownership before the foreground goroutine
// starts, including the first message whose agent startup is still pending.
func (e *Engine) claimUserTurn(key string, msg *Message) bool {
	e.interactiveMu.Lock()
	defer e.interactiveMu.Unlock()
	state := e.interactiveStates[key]
	if state == nil {
		return false
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if recalled, deferred := state.deferredMessages[msg]; deferred {
		delete(state.deferredMessages, msg)
		if recalled {
			return false
		}
	}
	state.currentMessageID = msg.MessageID
	return true
}

// steerBusyMessage returns false only when no input was delivered, allowing
// the regular queue path. It runs in the ordered background dispatcher.
func (e *Engine) steerBusyMessage(p Platform, msg *Message, session *Session, sessions *SessionManager, key string) bool {
	e.interactiveMu.Lock()
	state := e.interactiveStates[key]
	e.interactiveMu.Unlock()
	if state == nil {
		return false
	}
	state.mu.Lock()
	if state.deferredMessages[msg] {
		state.mu.Unlock()
		return true
	}
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
