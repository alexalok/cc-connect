// Package realtime transcribes recordings through an OpenAI-compatible Realtime
// endpoint, including gateways that authenticate with a ChatGPT subscription.
package realtime

import (
	"context"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	"github.com/chenhg5/cc-connect/core"
)

const (
	defaultModel = "gpt-transcribe"
	sampleRate   = 24000
	chunkBytes   = sampleRate // half a second of mono PCM16
)

// Transcriber implements core.SpeechToText. Each call uses its own connection
// and commits the entire recording as one item, without voice activity detection.
type Transcriber struct {
	apiKey   string
	endpoint string
	model    string
	dialer   *websocket.Dialer
	decode   func(context.Context, []byte) ([]byte, error)
}

var _ core.SpeechToText = (*Transcriber)(nil)
var _ core.AudioFormatSupporter = (*Transcriber)(nil)

// New uses the same credentials and base URL settings as the OpenAI speech
// provider. A gateway credential can be used instead of an OpenAI API key.
func New(apiKey, baseURL, model string) (*Transcriber, error) {
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return nil, fmt.Errorf("speech realtime: api_key is required")
	}
	endpoint, err := websocketEndpoint(baseURL)
	if err != nil {
		return nil, err
	}
	if model = strings.TrimSpace(model); model == "" {
		model = defaultModel
	}
	dialer := *websocket.DefaultDialer
	dialer.HandshakeTimeout = 20 * time.Second
	return &Transcriber{apiKey: apiKey, endpoint: endpoint, model: model, dialer: &dialer, decode: decodeAudio}, nil
}

func websocketEndpoint(baseURL string) (string, error) {
	if strings.TrimSpace(baseURL) == "" {
		baseURL = "https://api.openai.com/v1"
	}
	u, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || u.Host == "" || u.User != nil || u.Fragment != "" {
		return "", fmt.Errorf("speech realtime: invalid base_url")
	}
	switch u.Scheme {
	case "https", "wss":
		u.Scheme = "wss"
	case "http", "ws":
		ip := net.ParseIP(u.Hostname())
		if !strings.EqualFold(u.Hostname(), "localhost") && (ip == nil || !ip.IsLoopback()) {
			return "", fmt.Errorf("speech realtime: base_url must use HTTPS or WSS except on loopback")
		}
		u.Scheme = "ws"
	default:
		return "", fmt.Errorf("speech realtime: unsupported base_url scheme")
	}
	u.Path = strings.TrimRight(u.Path, "/")
	if u.Path == "" {
		u.Path = "/v1"
	}
	if !strings.HasSuffix(u.Path, "/realtime") {
		u.Path += "/realtime"
	}
	q := u.Query()
	q.Set("intent", "transcription")
	q.Del("model") // Transcription sessions select their model in session.update.
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// SupportsAudioFormat avoids an intermediate lossy MP3 conversion in core.
func (*Transcriber) SupportsAudioFormat(format string) bool {
	switch strings.ToLower(format) {
	case "mp3", "mpeg", "mpga", "mp4", "m4a", "aac", "wav", "webm", "ogg", "oga", "opus", "flac", "amr":
		return true
	default:
		return false
	}
}

func (t *Transcriber) Transcribe(ctx context.Context, audio []byte, _ string, lang string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	pcm, err := t.decode(ctx, audio)
	if err != nil {
		return "", err
	}
	if len(pcm) < sampleRate*2/10 {
		return "", fmt.Errorf("speech realtime: recording must contain at least 100 ms of audio")
	}
	conn, resp, err := t.dialer.DialContext(ctx, t.endpoint, http.Header{
		"Authorization": {"Bearer " + t.apiKey},
	})
	if err != nil {
		if resp != nil {
			_ = resp.Body.Close()
			return "", fmt.Errorf("speech realtime: connection rejected (HTTP %d)", resp.StatusCode)
		}
		return "", t.wrapError(ctx, "connect", err)
	}
	defer func() { _ = conn.Close() }()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	deadline, _ := ctx.Deadline()
	if err := conn.SetReadDeadline(deadline); err != nil {
		return "", t.wrapError(ctx, "set read deadline", err)
	}
	if err := conn.SetWriteDeadline(deadline); err != nil {
		return "", t.wrapError(ctx, "set write deadline", err)
	}
	conn.SetReadLimit(1 << 20)
	if err := conn.WriteJSON(t.sessionUpdate(lang)); err != nil {
		return "", t.wrapError(ctx, "configure session", err)
	}
	if _, err := t.readUntil(ctx, conn, "session.updated", ""); err != nil {
		return "", err
	}
	if err := appendRecording(conn, pcm); err != nil {
		return "", t.wrapError(ctx, "send audio", err)
	}
	committed, err := t.readUntil(ctx, conn, "input_audio_buffer.committed", "")
	if err != nil {
		return "", err
	}
	if committed.ItemID == "" {
		return "", fmt.Errorf("speech realtime: commit returned no item ID")
	}
	completed, err := t.readUntil(ctx, conn, "conversation.item.input_audio_transcription.completed", committed.ItemID)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(completed.Transcript), nil
}

func (t *Transcriber) sessionUpdate(lang string) map[string]any {
	transcription := map[string]any{"model": t.model}
	if lang = strings.TrimSpace(lang); lang != "" {
		if t.model == "gpt-transcribe" || t.model == "gpt-live-transcribe" {
			transcription["languages"] = []string{lang}
		} else {
			transcription["language"] = lang
		}
	}
	return map[string]any{
		"type": "session.update",
		"session": map[string]any{
			"type": "transcription",
			"audio": map[string]any{"input": map[string]any{
				"format":         map[string]any{"type": "audio/pcm", "rate": sampleRate},
				"transcription":  transcription,
				"turn_detection": nil,
			}},
		},
	}
}

func appendRecording(conn *websocket.Conn, pcm []byte) error {
	for offset := 0; offset < len(pcm); offset += chunkBytes {
		chunk := pcm[offset:min(offset+chunkBytes, len(pcm))]
		if err := conn.WriteJSON(map[string]string{
			"type": "input_audio_buffer.append", "audio": base64.StdEncoding.EncodeToString(chunk),
		}); err != nil {
			return err
		}
	}
	return conn.WriteJSON(map[string]string{"type": "input_audio_buffer.commit"})
}

type event struct {
	Type       string `json:"type"`
	ItemID     string `json:"item_id"`
	Transcript string `json:"transcript"`
	Error      struct {
		Message string `json:"message"`
	} `json:"error"`
}

func (t *Transcriber) readUntil(ctx context.Context, conn *websocket.Conn, kind, itemID string) (event, error) {
	for {
		var ev event
		if err := conn.ReadJSON(&ev); err != nil {
			return event{}, t.wrapError(ctx, "read "+kind, err)
		}
		if ev.Type == "error" || ev.Type == "conversation.item.input_audio_transcription.failed" {
			return event{}, fmt.Errorf("speech realtime: %s", core.RedactToken(ev.Error.Message, t.apiKey))
		}
		if ev.Type == kind && (itemID == "" || ev.ItemID == itemID) {
			return ev, nil
		}
	}
}

func (t *Transcriber) wrapError(ctx context.Context, action string, err error) error {
	if ctx.Err() != nil {
		return fmt.Errorf("speech realtime: %s: %w", action, ctx.Err())
	}
	return fmt.Errorf("speech realtime: %s: %s", action, core.RedactToken(err.Error(), t.apiKey))
}
