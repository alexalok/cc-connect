package codex

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

func validateAppServerURL(url string) error {
	if strings.EqualFold(url, "stdio://") || strings.EqualFold(url, "stdio") || url == "" {
		return nil
	}
	if strings.HasPrefix(url, "unix://") && filepath.IsAbs(strings.TrimPrefix(url, "unix://")) {
		return nil
	}
	return fmt.Errorf("codex app_server_url must be stdio:// or unix:///absolute/socket/path")
}

func (s *appServerSession) isShared() bool { return strings.HasPrefix(s.url, "unix://") }

// A shared session implements cancellation separately from Close: the latter
// detaches only this subscriber and must never terminate a shared daemon.
type sharedAppServerSession struct{ *appServerSession }

func (s *sharedAppServerSession) CancelTurn() error {
	s.sharedMu.Lock()
	submitting := s.sharedSubmitting
	s.stateMu.Lock()
	turnID := s.currentTurn
	s.stateMu.Unlock()
	s.sharedMu.Unlock()
	if submitting {
		return fmt.Errorf("codex shared turn acknowledgement is pending; cannot target an interrupt yet")
	}
	if turnID == "" {
		return nil
	}
	return s.request("turn/interrupt", map[string]any{"threadId": s.CurrentSessionID(), "turnId": turnID}, nil)
}

var _ core.AgentSessionCanceller = (*sharedAppServerSession)(nil)

func (s *appServerSession) clientMessageID(messageID string) string {
	if messageID == "" {
		messageID = fmt.Sprint(time.Now().UnixNano())
	}
	// This is attribution, not a promise that the server deduplicates requests.
	return fmt.Sprintf("%s:%s:%s", mirrorTurnOrigin, messageID, s.CurrentSessionID())
}

func (s *appServerSession) sharedSubmissionError(method string, err error) error {
	var serverError *rpcError
	if errors.As(err, &serverError) {
		return fmt.Errorf("codex app-server %s: %w", method, err)
	}
	s.abortTransport()
	return fmt.Errorf("codex shared server %s acknowledgement lost; the message may have been accepted. Check the saved conversation before retrying: %w", method, err)
}

// The reader must keep receiving RPC responses while turn/start is pending.
// Queue early events until its response tells us which turn we submitted to.
// Other clients' turns remain the responsibility of the history mirror.
type sharedMessage struct {
	method  string
	params  json.RawMessage
	request map[string]json.RawMessage
}

func (s *appServerSession) beginSharedTurn() error {
	s.sharedMu.Lock()
	defer s.sharedMu.Unlock()
	if s.sharedSubmitting || s.sharedTurn != "" {
		return fmt.Errorf("codex shared session already has an active submission")
	}
	s.sharedSubmitting = true
	return nil
}

func (s *appServerSession) finishSharedTurn(turnID string) {
	s.sharedMu.Lock()
	defer s.sharedMu.Unlock()
	s.sharedSubmitting = false
	s.sharedTurn = turnID
	if turnID != "" {
		s.stateMu.Lock()
		s.currentTurn = turnID
		s.pendingMsgs = nil
		s.stateMu.Unlock()
	}
	queued := s.sharedQueue
	s.sharedQueue = nil
	s.sharedQueueBytes = 0
	for _, message := range queued {
		s.dispatchSharedMessage(message)
	}
}

func (s *appServerSession) routeSharedMessage(message sharedMessage) {
	s.sharedMu.Lock()
	defer s.sharedMu.Unlock()
	raw := message.params
	if message.request != nil {
		raw = message.request["params"]
	}
	if s.isOtherSharedThread(raw) {
		return
	}
	if s.sharedSubmitting {
		// Bound memory independently of the socket's individual-frame limit.
		size := len(raw)
		if len(s.sharedQueue) >= 512 || s.sharedQueueBytes+size > 8*1024*1024 {
			s.abortTransport()
			return
		}
		s.sharedQueue = append(s.sharedQueue, message)
		s.sharedQueueBytes += size
		return
	}
	s.dispatchSharedMessage(message)
}

// Caller holds sharedMu, including while replaying buffered messages, so a
// later completion cannot overtake an earlier item or approval request.
func (s *appServerSession) dispatchSharedMessage(message sharedMessage) {
	raw := message.params
	if message.request != nil {
		raw = message.request["params"]
	}
	var scope struct {
		ThreadID string `json:"threadId"`
		TurnID   string `json:"turnId"`
		Turn     struct {
			ID string `json:"id"`
		} `json:"turn"`
	}
	if json.Unmarshal(raw, &scope) != nil {
		return
	}
	if scope.ThreadID != "" && scope.ThreadID != s.CurrentSessionID() {
		return
	}
	if message.request == nil {
		switch message.method {
		case "account/rateLimits/updated", "thread/tokenUsage/updated", "serverRequest/resolved":
			s.handleNotificationDirect(message.method, raw)
			return
		}
	}
	turnID := scope.TurnID
	if turnID == "" {
		turnID = scope.Turn.ID
	}
	// Unscoped server requests must not be answered on behalf of another client.
	if scope.ThreadID != s.CurrentSessionID() || turnID == "" || turnID != s.sharedTurn {
		return
	}
	if message.request != nil {
		var method string
		_ = json.Unmarshal(message.request["method"], &method)
		switch method {
		case "item/commandExecution/requestApproval", "item/fileChange/requestApproval", "item/permissions/requestApproval", "item/tool/requestUserInput":
			s.handleServerRequestDirect(message.request)
		}
		// Dynamic tools belong to the client that registered them. This
		// subscriber does not register tools and must not race that client.
		return
	}
	s.handleNotificationDirect(message.method, raw)
	if message.method == "turn/completed" {
		s.sharedTurn = ""
	}
}
