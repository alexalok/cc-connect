package core

import "log/slog"

// cmdArchive archives the current agent session before removing its topic.
// Keeping the archived session as the active pointer permits an immediate retry
// if topic deletion fails. GetOrCreateActive skips it for subsequent chat turns.
func (e *Engine) cmdArchive(p Platform, msg *Message, args []string) {
	if len(args) != 0 {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgArchiveUsage))
		return
	}
	agent, sessions, interactiveKey, err := e.archiveCommandContext(p, msg)
	if err != nil {
		e.reply(p, msg.ReplyCtx, e.i18n.Tf(MsgWsResolutionError, err))
		return
	}
	archiver, ok := agent.(SessionArchiver)
	if !ok {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgArchiveNotSupported))
		return
	}
	session := sessions.FindByID(sessions.ActiveSessionID(msg.SessionKey))
	if session == nil || session.GetAgentSessionID() == "" || session.GetAgentSessionID() == ContinueSession {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgArchiveNoSession))
		return
	}
	if !session.beginArchive() {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgArchiveInProgress))
		return
	}
	defer session.endArchive()
	if !session.IsArchived() {
		e.cleanupInteractiveState(interactiveKey)
		closeSettled := e.awaitSessionClose(interactiveKey)
		e.closingMu.Lock()
		unsafe := e.unsafeResume[interactiveKey]
		e.closingMu.Unlock()
		if !closeSettled || unsafe || e.isUnsafeAgentSession(agent, session.GetAgentSessionID()) {
			e.reply(p, msg.ReplyCtx, e.i18n.T(MsgArchiveStopFailed))
			return
		}
		session.ForceUnlock()
		var archiveErr error
		if scoped, ok := agent.(ProviderSessionArchiver); ok {
			session.mu.Lock()
			provider := session.ActiveProvider
			session.mu.Unlock()
			archiveErr = scoped.ArchiveSessionWithProvider(e.ctx, session.GetAgentSessionID(), provider)
		} else {
			archiveErr = archiver.ArchiveSession(e.ctx, session.GetAgentSessionID())
		}
		if err := archiveErr; err != nil {
			slog.Warn("archive: agent session archive failed", "error", err)
			e.reply(p, msg.ReplyCtx, e.i18n.Tf(MsgArchiveFailed, err))
			return
		}
		session.SetArchived(true)
		sessions.Save()
	}
	if deleter, ok := p.(TopicDeleter); ok {
		deleted, err := deleter.DeleteTopic(e.ctx, msg.ReplyCtx)
		if err != nil {
			slog.Warn("archive: topic deletion failed", "error", err)
			e.reply(p, msg.ReplyCtx, e.i18n.Tf(MsgArchiveTopicFailed, err))
			return
		}
		if deleted {
			return // The topic and its messages are gone; there is nowhere to reply.
		}
	}
	e.reply(p, msg.ReplyCtx, e.i18n.T(MsgArchiveSuccess))
}

// Send-bound conversations override channel/global context just as they do for
// incoming turns. Resolve before stopping or archiving any selected session.
func (e *Engine) archiveCommandContext(p Platform, msg *Message) (Agent, *SessionManager, string, error) {
	if workDir := e.sendWorkDirForSession(msg.SessionKey); workDir != "" {
		agent, sessions, err := e.getOrCreateWorkspaceAgent(workDir)
		return agent, sessions, workDir + ":" + msg.SessionKey, err
	}
	return e.commandContext(p, msg)
}
