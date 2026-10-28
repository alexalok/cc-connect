package core

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
)

type originalAudioSTT struct {
	audio      []byte
	format     string
	language   string
	transcript string
}

func (*originalAudioSTT) SupportsAudioFormat(format string) bool { return format == "ogg" }
func (s *originalAudioSTT) Transcribe(_ context.Context, audio []byte, format, lang string) (string, error) {
	s.audio, s.format, s.language = audio, format, lang
	return s.transcript, nil
}

func TestTranscribeAudio_PreservesProviderSupportedOriginalFormat(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // An intermediate conversion would fail here.
	stt := &originalAudioSTT{transcript: "complete transcript"}
	audio := &AudioAttachment{Data: []byte("original ogg data"), Format: "OGG"}
	text, err := TranscribeAudio(context.Background(), stt, audio, "ru")
	if err != nil || text != stt.transcript {
		t.Fatalf("TranscribeAudio = %q, %v", text, err)
	}
	if !bytes.Equal(stt.audio, audio.Data) || stt.format != "ogg" || stt.language != "ru" {
		t.Fatalf("provider did not receive original audio: %+v", stt)
	}
	if _, err := TranscribeAudio(context.Background(), stt, &AudioAttachment{Data: audio.Data, Format: "amr"}, ""); err == nil {
		t.Fatal("legacy conversion must remain enabled for a format the provider does not accept")
	}
	if _, err := TranscribeAudio(context.Background(), NewOpenAIWhisper("key", "", ""), &AudioAttachment{Data: audio.Data, Format: "ogg"}, ""); err == nil {
		t.Fatal("providers without format support must retain legacy conversion")
	}
}

func TestNewGeminiSTT_DefaultModel(t *testing.T) {
	g := NewGeminiSTT("test-key", "")
	if g.Model != "gemini-flash-latest" {
		t.Errorf("expected default model gemini-flash-latest, got %s", g.Model)
	}
	if g.BaseURL != "https://generativelanguage.googleapis.com/v1beta" {
		t.Errorf("unexpected base URL: %s", g.BaseURL)
	}
}

func TestNewGeminiSTT_CustomModel(t *testing.T) {
	g := NewGeminiSTT("test-key", "gemini-2.5-flash")
	if g.Model != "gemini-2.5-flash" {
		t.Errorf("expected gemini-2.5-flash, got %s", g.Model)
	}
}

func TestGeminiSTT_Transcribe_Success(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("expected application/json, got %s", ct)
		}
		if r.Header.Get("x-goog-api-key") != "test-key" {
			t.Errorf("expected x-goog-api-key 'test-key', got %q", r.Header.Get("x-goog-api-key"))
		}
		if r.URL.Query().Get("key") != "" {
			t.Errorf("expected API key not in query string")
		}
		if !strings.Contains(r.URL.Path, "test-model") {
			t.Errorf("expected model in path, got %s", r.URL.Path)
		}

		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		contents, ok := body["contents"].([]any)
		if !ok || len(contents) == 0 {
			t.Fatal("missing contents in request")
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"candidates": []map[string]any{
				{
					"content": map[string]any{
						"parts": []map[string]any{
							{"text": "你好世界"},
						},
					},
				},
			},
		})
	}))
	defer server.Close()

	g := &GeminiSTT{
		APIKey:  "test-key",
		Model:   "test-model",
		BaseURL: server.URL,
		Client:  server.Client(),
	}

	text, err := g.Transcribe(context.Background(), []byte("fake-audio"), "mp3", "zh")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if text != "你好世界" {
		t.Errorf("expected '你好世界', got %q", text)
	}
}

func TestGeminiSTT_Transcribe_WithLanguage(t *testing.T) {
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"candidates": []map[string]any{
				{"content": map[string]any{"parts": []map[string]any{{"text": "hello"}}}},
			},
		})
	}))
	defer server.Close()

	g := &GeminiSTT{APIKey: "k", Model: "m", BaseURL: server.URL, Client: server.Client()}
	_, err := g.Transcribe(context.Background(), []byte("audio"), "mp3", "en")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify the prompt includes language
	contents := gotBody["contents"].([]any)
	parts := contents[0].(map[string]any)["parts"].([]any)
	textPart := parts[1].(map[string]any)["text"].(string)
	if !strings.Contains(textPart, "en") {
		t.Errorf("expected language 'en' in prompt, got %q", textPart)
	}
}

func TestGeminiSTT_Transcribe_APIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"error":{"message":"API key invalid","code":403}}`))
	}))
	defer server.Close()

	g := &GeminiSTT{APIKey: "bad", Model: "m", BaseURL: server.URL, Client: server.Client()}
	_, err := g.Transcribe(context.Background(), []byte("audio"), "mp3", "")
	if err == nil {
		t.Fatal("expected error for 403 response")
	}
	if !strings.Contains(err.Error(), "403") {
		t.Errorf("expected 403 in error, got: %v", err)
	}
}

func TestGeminiSTT_Transcribe_EmptyResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"candidates": []map[string]any{}})
	}))
	defer server.Close()

	g := &GeminiSTT{APIKey: "k", Model: "m", BaseURL: server.URL, Client: server.Client()}
	_, err := g.Transcribe(context.Background(), []byte("audio"), "mp3", "")
	if err == nil {
		t.Fatal("expected error for empty candidates")
	}
	if !strings.Contains(err.Error(), "empty response") {
		t.Errorf("expected 'empty response' in error, got: %v", err)
	}
}

func TestGeminiSTT_Transcribe_InvalidJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`not json`))
	}))
	defer server.Close()

	g := &GeminiSTT{APIKey: "k", Model: "m", BaseURL: server.URL, Client: server.Client()}
	_, err := g.Transcribe(context.Background(), []byte("audio"), "mp3", "")
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
	if !strings.Contains(err.Error(), "parse response") {
		t.Errorf("expected 'parse response' in error, got: %v", err)
	}
}

// TestConvertAudioToMP3_HonorsContextCancellation asserts that a cancelled
// context aborts the ffmpeg subprocess rather than running it to completion.
// Skips when ffmpeg is not installed (the conversion helper returns the
// "not found" error before the context is ever consulted).
func TestConvertAudioToMP3_HonorsContextCancellation(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not in PATH")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := ConvertAudioToMP3(ctx, []byte{0, 1, 2, 3}, "mp3")
	if err == nil {
		t.Fatal("expected error after context cancellation, got nil")
	}
}
