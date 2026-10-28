package telegram_user

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"

	"github.com/gotd/td/session"
)

// sessionStorage replaces the file atomically, with owner-only permissions even
// when replacing a previously world-readable file. A session authorizes access
// to the entire Telegram account, so its contents must never appear in logs.
type sessionStorage struct {
	path string
	mu   sync.Mutex
}

func (s *sessionStorage) LoadSession(ctx context.Context) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(s.path)
	if os.IsNotExist(err) {
		return nil, session.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("telegram_user: read session: %w", err)
	}
	return data, nil
}

func (s *sessionStorage) StoreSession(ctx context.Context, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("telegram_user: create session directory: %w", err)
	}
	f, err := os.CreateTemp(dir, ".telegram-user-session-*")
	if err != nil {
		return fmt.Errorf("telegram_user: create session file: %w", err)
	}
	defer func() {
		if err := os.Remove(f.Name()); err != nil && !os.IsNotExist(err) {
			slog.Warn("telegram_user: remove temporary session file", "error", err)
		}
	}()
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return fmt.Errorf("telegram_user: write session: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("telegram_user: sync session: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("telegram_user: close session: %w", err)
	}
	if err := os.Rename(f.Name(), s.path); err != nil {
		return fmt.Errorf("telegram_user: replace session: %w", err)
	}
	return nil
}
