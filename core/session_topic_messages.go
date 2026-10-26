package core

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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
		}
	}
	return errors.Join(failures...)
}

func (w *sessionTopicSync) topicStillSelected(key, id string) bool {
	if w.engine.sessions.agentSessionDeleted(id) {
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
		content := w.formatMirroredMessage(message)
		if content == "" {
			continue
		}
		chunks := SplitMessageCodeFenceAware(content, maxPlatformMessageLen)
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
	}
	return nil
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
