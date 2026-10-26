package realtime

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func testTranscriber(t *testing.T, handler func(*websocket.Conn), pcm []byte) *Transcriber {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/realtime" || r.URL.Query().Get("intent") != "transcription" || r.URL.Query().Has("model") {
			t.Errorf("incorrect transcription URL: %s", r.URL)
		}
		if r.Header.Get("Authorization") != "Bearer gateway-secret" {
			t.Error("missing gateway authorization")
		}
		upgrader := websocket.Upgrader{}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		handler(conn)
	}))
	t.Cleanup(server.Close)
	stt, err := New("gateway-secret", server.URL+"/v1", "")
	if err != nil {
		t.Fatal(err)
	}
	stt.decode = func(context.Context, []byte) ([]byte, error) { return pcm, nil }
	return stt
}

func TestTranscribe_CommitsWholeRecordingAndWaitsForMatchingFinal(t *testing.T) {
	pcm := bytes.Repeat([]byte{1, 2}, chunkBytes+200)
	stt := testTranscriber(t, func(conn *websocket.Conn) {
		var update struct {
			Type    string `json:"type"`
			Session struct {
				Type  string `json:"type"`
				Audio struct {
					Input map[string]json.RawMessage `json:"input"`
				} `json:"audio"`
			} `json:"session"`
		}
		if err := conn.ReadJSON(&update); err != nil {
			t.Error(err)
			return
		}
		input := update.Session.Audio.Input
		if update.Type != "session.update" || update.Session.Type != "transcription" || string(input["turn_detection"]) != "null" {
			t.Errorf("expected transcription-only session with VAD disabled: %+v", update)
		}
		var transcription struct {
			Model     string   `json:"model"`
			Languages []string `json:"languages"`
		}
		if err := json.Unmarshal(input["transcription"], &transcription); err != nil {
			t.Error(err)
		}
		if transcription.Model != "gpt-transcribe" || len(transcription.Languages) != 1 || transcription.Languages[0] != "ru" {
			t.Errorf("transcription configuration: %+v", transcription)
		}
		conn.WriteJSON(map[string]string{"type": "session.created"})
		conn.WriteJSON(map[string]string{"type": "session.updated"})
		var received []byte
		for {
			var msg map[string]string
			if err := conn.ReadJSON(&msg); err != nil {
				t.Error(err)
				return
			}
			if msg["type"] == "input_audio_buffer.commit" {
				break
			}
			if msg["type"] != "input_audio_buffer.append" {
				t.Errorf("unexpected request: %v", msg)
				return
			}
			chunk, err := base64.StdEncoding.DecodeString(msg["audio"])
			if err != nil || len(chunk) > chunkBytes {
				t.Errorf("invalid audio chunk: bytes=%d, err=%v", len(chunk), err)
			}
			received = append(received, chunk...)
		}
		if !bytes.Equal(received, pcm) {
			t.Error("recording truncated or altered before commit")
		}
		conn.WriteJSON(map[string]string{"type": "input_audio_buffer.committed", "item_id": "voice-1"})
		conn.WriteJSON(map[string]string{"type": "conversation.item.input_audio_transcription.delta", "item_id": "voice-1", "delta": "partial"})
		conn.WriteJSON(map[string]string{"type": "conversation.item.input_audio_transcription.completed", "item_id": "other", "transcript": "unrelated"})
		conn.WriteJSON(map[string]string{"type": "conversation.item.input_audio_transcription.completed", "item_id": "voice-1", "transcript": "  Complete recording, including the last sentence.  "})
	}, pcm)
	text, err := stt.Transcribe(context.Background(), []byte("recording"), "ogg", "ru")
	if err != nil || text != "Complete recording, including the last sentence." {
		t.Fatalf("Transcribe = %q, %v", text, err)
	}
}

func TestTranscribe_ProviderFailureNeverReturnsPartialText(t *testing.T) {
	stt := testTranscriber(t, func(conn *websocket.Conn) {
		var msg map[string]any
		if err := conn.ReadJSON(&msg); err != nil {
			t.Error(err)
			return
		}
		conn.WriteJSON(map[string]string{"type": "session.updated"})
		for {
			if err := conn.ReadJSON(&msg); err != nil {
				t.Error(err)
				return
			}
			if msg["type"] == "input_audio_buffer.commit" {
				break
			}
		}
		conn.WriteJSON(map[string]string{"type": "input_audio_buffer.committed", "item_id": "voice-1"})
		conn.WriteJSON(map[string]string{"type": "conversation.item.input_audio_transcription.delta", "item_id": "voice-1", "delta": "Do not return this"})
		conn.WriteJSON(map[string]any{"type": "conversation.item.input_audio_transcription.failed", "error": map[string]string{"message": "invalid credential gateway-secret"}})
	}, make([]byte, 4800))
	text, err := stt.Transcribe(context.Background(), []byte("recording"), "wav", "")
	if text != "" || err == nil || strings.Contains(err.Error(), "gateway-secret") || !strings.Contains(err.Error(), "invalid credential") {
		t.Fatalf("unexpected result: %q, %v", text, err)
	}
}

func TestTranscribe_CancellationClosesBlockedConnection(t *testing.T) {
	ready := make(chan struct{})
	closed := make(chan struct{})
	stt := testTranscriber(t, func(conn *websocket.Conn) {
		defer close(closed)
		var msg map[string]any
		if err := conn.ReadJSON(&msg); err != nil {
			t.Error(err)
			return
		}
		close(ready)
		if err := conn.ReadJSON(&msg); err == nil {
			t.Error("expected connection to close during cancellation")
		}
	}, make([]byte, 4800))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, err := stt.Transcribe(ctx, []byte("recording"), "wav", "")
		result <- err
	}()
	select {
	case <-ready:
	case <-time.After(3 * time.Second):
		t.Fatal("connection never became ready")
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected cancellation, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled transcription did not return")
	}
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("canceled transcription left its socket open")
	}
}

func TestTranscribe_HandshakeFailureDoesNotExposeResponseBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte("gateway-secret"))
	}))
	defer server.Close()
	stt, err := New("gateway-secret", server.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	stt.decode = func(context.Context, []byte) ([]byte, error) { return make([]byte, 4800), nil }
	_, err = stt.Transcribe(context.Background(), []byte("recording"), "wav", "")
	if err == nil || !strings.Contains(err.Error(), "HTTP 401") || strings.Contains(err.Error(), "gateway-secret") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestWebsocketEndpoint_TranscriptionRoute(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{"", "wss://api.openai.com/v1/realtime?intent=transcription"},
		{"http://127.0.0.2:10101/v1", "ws://127.0.0.2:10101/v1/realtime?intent=transcription"},
		{"https://example.com/proxy/v1/realtime?model=wrong&intent=quicksilver", "wss://example.com/proxy/v1/realtime?intent=transcription"},
	} {
		got, err := websocketEndpoint(tc.input)
		if err != nil || got != tc.want {
			t.Errorf("endpoint(%q) = %q, %v; want %q", tc.input, got, err, tc.want)
		}
	}
	for _, invalid := range []string{"http://example.com/v1", "https://user:secret@example.com/v1", "ftp://example.com", "bad-url", "https://example.com/#secret"} {
		if _, err := websocketEndpoint(invalid); err == nil {
			t.Errorf("accepted invalid endpoint %q", invalid)
		}
	}
}

func TestTranscribe_RejectsShortRecordingBeforeConnecting(t *testing.T) {
	stt, err := New("key", "", "")
	if err != nil {
		t.Fatal(err)
	}
	stt.decode = func(context.Context, []byte) ([]byte, error) { return make([]byte, 4798), nil }
	_, err = stt.Transcribe(context.Background(), []byte("recording"), "wav", "")
	if err == nil || !strings.Contains(err.Error(), "100 ms") {
		t.Fatalf("unexpected error: %v", err)
	}
}
