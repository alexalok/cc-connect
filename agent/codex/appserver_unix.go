package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"path/filepath"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

// unixSocketStream adapts WebSocket messages to the existing JSON-lines reader.
// Closing a bridge session closes only its connection, never the shared server.
type unixSocketStream struct{ conn *websocket.Conn }

func (s *unixSocketStream) Write(p []byte) (int, error) {
	if err := s.conn.WriteMessage(websocket.TextMessage, p); err != nil {
		return 0, err
	}
	return len(p), nil
}
func (s *unixSocketStream) Close() error { return s.conn.Close() }

func (s *appServerSession) connectUnixSocket(path string) error {
	if !filepath.IsAbs(path) {
		return fmt.Errorf("codex app-server Unix socket path must be absolute")
	}
	dialer := websocket.Dialer{
		HandshakeTimeout: 10 * time.Second,
		NetDialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", path)
		},
	}
	conn, _, err := dialer.DialContext(s.ctx, "ws://localhost/", nil)
	if err != nil {
		return fmt.Errorf("codex app-server connect Unix socket: %w", err)
	}
	conn.SetReadLimit(16 * 1024 * 1024)
	s.procMu.Lock()
	s.stdin = &unixSocketStream{conn: conn}
	s.procMu.Unlock()
	reader, writer := io.Pipe()
	stop := context.AfterFunc(s.ctx, func() { _ = conn.Close(); _ = reader.Close() })
	s.wg.Add(2)
	go func() {
		defer reader.Close()
		s.readLoop(reader)
	}()
	go func() {
		defer s.wg.Done()
		defer stop()
		defer conn.Close()
		defer writer.Close()
		for {
			_, data, err := conn.ReadMessage()
			if err != nil {
				_ = writer.CloseWithError(err)
				return
			}
			if _, err := writer.Write(append(data, '\n')); err != nil {
				return
			}
		}
	}()
	slog.Info("codex app-server connected", "transport", "unix", "path", path)
	return nil
}

// Only shared transports need filtering; dedicated stdio sessions also carry
// intentional asynchronous subagent deliveries from other thread IDs.
func (s *appServerSession) isOtherSharedThread(raw json.RawMessage) bool {
	if !strings.HasPrefix(s.url, "unix://") {
		return false
	}
	var scope struct {
		ThreadID      string `json:"threadId"`
		ThreadIDSnake string `json:"thread_id"`
	}
	if json.Unmarshal(raw, &scope) != nil {
		return false
	}
	id := scope.ThreadID
	if id == "" {
		id = scope.ThreadIDSnake
	}
	return id != "" && id != s.CurrentSessionID()
}
