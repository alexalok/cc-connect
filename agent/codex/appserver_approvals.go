package codex

import (
	"encoding/json"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

func (s *appServerSession) registerApproval(id string) (chan core.PermissionResult, chan struct{}) {
	s.approvalsMu.Lock()
	defer s.approvalsMu.Unlock()
	if s.pendingApprovals == nil {
		s.pendingApprovals = make(map[string]chan core.PermissionResult)
	}
	if s.approvalDone == nil {
		s.approvalDone = make(map[string]chan struct{})
	}
	s.resolveApprovalLocked(id)
	ch, done := make(chan core.PermissionResult, 1), make(chan struct{})
	s.pendingApprovals[id], s.approvalDone[id] = ch, done
	return ch, done
}

// approvalDone is closed only under approvalsMu. It also releases an engine
// that is waiting for a Telegram answer after another client already answered.
func (s *appServerSession) resolveApprovalLocked(id string) {
	delete(s.pendingApprovals, id)
	if done := s.approvalDone[id]; done != nil {
		close(done)
		delete(s.approvalDone, id)
	}
}

func (s *appServerSession) resolveServerRequest(raw json.RawMessage) {
	var params struct {
		RequestID json.RawMessage `json:"requestId"`
	}
	if json.Unmarshal(raw, &params) != nil || len(params.RequestID) == 0 {
		return
	}
	s.approvalsMu.Lock()
	s.resolveApprovalLocked(string(params.RequestID))
	s.approvalsMu.Unlock()
}

func (s *appServerSession) awaitApproval(id string, ch chan core.PermissionResult, done chan struct{}) (core.PermissionResult, bool) {
	timer := time.NewTimer(5 * time.Minute)
	defer timer.Stop()
	result := core.PermissionResult{Behavior: "deny"}
	reply := true
	select {
	case result = <-ch:
	case <-timer.C:
	case <-done:
		reply = false
	case <-s.contextDone():
		reply = false
	}
	s.approvalsMu.Lock()
	defer s.approvalsMu.Unlock()
	if s.approvalDone[id] != done {
		return result, false
	}
	s.resolveApprovalLocked(id)
	return result, reply
}
