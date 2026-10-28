package codex

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/chenhg5/cc-connect/core"
)

var _ core.SessionArchiver = (*Agent)(nil)
var _ core.ProviderSessionArchiver = (*Agent)(nil)

// ArchiveSession uses Codex's native archive operation, including its rollout
// and state database updates. This management connection never resumes a
// thread or starts a turn, and works with both exec and app-server backends.
func (a *Agent) ArchiveSession(ctx context.Context, sessionID string) error {
	return a.ArchiveSessionWithProvider(ctx, sessionID, "")
}

// ArchiveSessionWithProvider uses the conversation's saved profile without
// mutating the default provider shared by other topics.
func (a *Agent) ArchiveSessionWithProvider(ctx context.Context, sessionID, providerName string) error {
	if strings.TrimSpace(sessionID) == "" || sessionID == core.ContinueSession {
		return fmt.Errorf("codex: archive: missing session ID")
	}
	ctx, cancel := context.WithTimeout(ctx, appServerRequestTimeout)
	defer cancel()
	a.mu.RLock()
	codexHome, err := a.sessionCodexHomeForProviderLocked(providerName)
	if err != nil {
		a.mu.RUnlock()
		return fmt.Errorf("codex: archive: resolve profile: %w", err)
	}
	extraEnv := a.mergedSessionEnvLocked()
	if providerName != "" {
		for _, provider := range a.providers {
			if provider.Name == providerName {
				extraEnv = append([]string(nil), a.configEnv...)
				extraEnv = append(extraEnv, codexProviderEnv(provider)...)
				extraEnv = append(extraEnv, a.sessionEnv...)
				break
			}
		}
	}
	s := &appServerSession{
		cliBin:           a.cmd,
		cliExtraArgs:     append([]string(nil), a.cliExtraArgs...),
		url:              "stdio://",
		workDir:          a.workDir,
		codexHome:        codexHome,
		extraEnv:         extraEnv,
		ctx:              ctx,
		cancel:           cancel,
		events:           make(chan core.Event, 128),
		pending:          make(map[int64]chan rpcResponseEnvelope),
		pendingApprovals: make(map[string]chan core.PermissionResult),
	}
	a.mu.RUnlock()
	s.alive.Store(true)
	if err := s.connect(); err != nil {
		return fmt.Errorf("codex: archive: %w", err)
	}
	defer func() {
		if err := s.Close(); err != nil {
			slog.Warn("codex: archive connection close failed", "error", err)
		}
	}()
	if err := s.initialize(); err != nil {
		return fmt.Errorf("codex: archive: %w", err)
	}
	if err := s.request("thread/archive", map[string]any{"threadId": sessionID}, nil); err != nil {
		return fmt.Errorf("codex: archive session: %w", err)
	}
	return nil
}
