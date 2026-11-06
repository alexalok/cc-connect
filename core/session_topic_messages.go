package core

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"sort"
	"strings"
)

func (w *sessionTopicSync) mirrorMessages(ctx context.Context) error {
	if w.messages == nil {
		return nil
	}
	if w.state.Delivered == nil {
		w.state.Delivered = make(map[string]map[string]int)
	}
	ids := make([]string, 0, len(w.state.Topics))
	for id := range w.state.Topics {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	// Bound a backlog so a poll can yield to shutdown and platform traffic.
	remaining := 100
	var failures []error
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return errors.Join(append(failures, err)...)
		}
		if remaining == 0 {
			break
		}
		if err := w.mirrorTopicMessages(ctx, id, &remaining); err != nil {
			failures = append(failures, fmt.Errorf("session topic sync: mirror %s: %w", id, err))
			if errors.Is(err, errSessionTopicSyncPersistence) {
				return errors.Join(failures...)
			}
		}
	}
	return errors.Join(failures...)
}

func (w *sessionTopicSync) topicStillSelected(key, id string) bool {
	if w.archived[id] || w.state.DeletedTopics[key] != "" || w.engine.sessions.agentSessionDeleted(id) {
		return false
	}
	session := w.engine.sessions.FindByID(w.engine.sessions.ActiveSessionID(key))
	return session != nil && session.GetAgentSessionID() == id && !session.Busy()
}

func (w *sessionTopicSync) mirrorTopicMessages(ctx context.Context, id string, remaining *int) error {
	key := w.state.Topics[id]
	if !w.topicStillSelected(key, id) {
		return nil
	}
	messages, err := w.messages.ReadSessionMessages(ctx, id)
	if err != nil {
		return err
	}
	reply, err := w.replies.ReconstructReplyCtx(key)
	if err != nil {
		return err
	}
	if err := w.mirrorTopicStatus(ctx, key, id, reply); err != nil {
		slog.Warn("session topic status sync failed", "session", id, "error", err)
	}
	delivered := w.state.Delivered[id]
	if delivered == nil {
		delivered = make(map[string]int)
		w.state.Delivered[id] = delivered
	}
	sent := 0
	defer func() {
		if sent > 0 {
			slog.Info("session topic messages mirrored", "project", w.engine.name, "session", id, "chunks", sent)
		}
	}()
	for _, message := range messages {
		if *remaining == 0 || !w.topicStillSelected(key, id) {
			return nil
		}
		if message.ID == "" {
			return fmt.Errorf("message source returned an empty message ID")
		}
		display := message
		progress, tracked := w.state.Attachments[id][message.ID]
		if message.Role == "assistant" && ((tracked && progress != -1) || (w.engine.responseAttachments.Load() && delivered[message.ID] == 0)) {
			display.Content = renderResponseAttachmentLinks(message.Content)
		}
		content := w.formatMirroredMessage(display)
		if content == "" {
			continue
		}
		chunks := SplitMessageCodeFenceAware(content, maxPlatformMessageLen)
		attachmentsEnabled := message.Role == "assistant" && w.engine.responseAttachments.Load() && len(responseLinks(message.Content, "/")) > 0
		if attachmentsEnabled {
			if w.state.Attachments == nil {
				w.state.Attachments = make(map[string]map[string]int)
			}
			if w.state.Attachments[id] == nil {
				w.state.Attachments[id] = make(map[string]int)
			}
			if _, tracked := w.state.Attachments[id][message.ID]; !tracked {
				progress := 0
				if delivered[message.ID] > 0 {
					progress = -1
				}
				w.state.Attachments[id][message.ID] = progress
				// Save before the first text chunk, so a restart between text
				// and files resumes attachments rather than treating them as old.
				if progress == 0 {
					if err := w.save(); err != nil {
						return err
					}
				}
			}
		}
		for i := delivered[message.ID]; i < len(chunks); i++ {
			if *remaining == 0 || !w.topicStillSelected(key, id) {
				return nil
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := w.platform.Send(ctx, reply, chunks[i]); err != nil {
				return err
			}
			sent++
			*remaining -= 1
			delivered[message.ID] = i + 1
			// Keep the in-memory acknowledgment on a persistence failure. The
			// next poll saves it before sending anything else. Telegram has no
			// idempotency key, so a crash between send and save can still replay
			// that one chunk; never advance before a successful send.
			if err := w.save(); err != nil {
				return err
			}
		}
		if attachmentsEnabled && w.state.Attachments[id][message.ID] >= 0 {
			if !w.engine.attachmentSendEnabled.Load() {
				w.state.Attachments[id][message.ID] = -2
				if err := w.save(); err != nil {
					return err
				}
				continue
			}
			if err := w.mirrorResponseAttachments(ctx, id, key, reply, message, remaining); err != nil {
				return err
			}
		}
	}
	return nil
}

func (w *sessionTopicSync) mirrorResponseAttachments(ctx context.Context, id, key string, reply any, message SessionMessage, remaining *int) error {
	workDir := responseWorkDir(nil, w.engine.agent, "")
	if source, ok := w.engine.agent.(SessionWorkDirProvider); ok {
		selected := w.engine.sessions.FindByID(w.engine.sessions.ActiveSessionID(key))
		if selected == nil || selected.GetAgentSessionID() != id {
			return nil
		}
		dir, err := source.GetSessionWorkDir(ctx, id, selected.GetActiveProvider())
		if err != nil {
			return err
		}
		if dir != "" {
			workDir = dir
		}
	}
	attachments := responseLinks(message.Content, workDir)
	for i := w.state.Attachments[id][message.ID]; i < len(attachments); i++ {
		if *remaining == 0 || !w.topicStillSelected(key, id) {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		retry, err := w.engine.sendResponseAttachment(ctx, w.platform, reply, attachments[i])
		if err != nil {
			if retry {
				return err
			}
			slog.Warn("mirrored response attachment unavailable", "path", attachments[i].path, "error", err)
			if err := w.platform.Send(ctx, reply, w.engine.i18n.Tf(MsgResponseAttachmentFailed, filepath.Base(attachments[i].path))); err != nil {
				return err
			}
		}
		*remaining -= 1
		w.state.Attachments[id][message.ID] = i + 1
		if err := w.save(); err != nil {
			return err
		}
	}
	w.state.Attachments[id][message.ID] = -2
	return w.save()
}

func (w *sessionTopicSync) formatMirroredMessage(message SessionMessage) string {
	content := strings.TrimSpace(message.Content)
	if message.Attachments > 0 {
		content = strings.TrimSpace(content + "\n\n" + w.engine.i18n.Tf(MsgSyncedAttachments, message.Attachments))
	}
	if content == "" {
		return ""
	}
	switch message.Role {
	case "user":
		return w.engine.i18n.Tf(MsgSyncedUserMessage, content)
	case "assistant":
		return w.engine.i18n.Tf(MsgSyncedAssistantMessage, content)
	default:
		return ""
	}
}
