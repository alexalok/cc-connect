package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

func TestStageImages_PreservesCaptionlessPrompt(t *testing.T) {
	for _, backend := range []string{"exec", "app_server"} {
		t.Run(backend, func(t *testing.T) {
			dir := t.TempDir()
			stage := (&codexSession{workDir: dir}).stageImages
			if backend == "app_server" {
				stage = (&appServerSession{workDir: dir}).stageImages
			}
			for _, caption := range []string{"", " \n", "What changed?"} {
				data := []byte("photo bytes")
				prompt, paths, err := stage(caption, []core.ImageAttachment{{MimeType: "image/jpeg", Data: data}})
				if err != nil {
					t.Fatal(err)
				}
				if prompt != caption {
					t.Errorf("prompt = %q, want original caption %q", prompt, caption)
				}
				if len(paths) != 1 {
					t.Fatalf("image paths = %v", paths)
				}
				got, err := os.ReadFile(paths[0])
				if err != nil || !bytes.Equal(got, data) {
					t.Fatalf("image content = %q, error = %v", got, err)
				}
			}
		})
	}
}

func TestAppServerSend_CaptionlessImagesHaveNoText(t *testing.T) {
	for _, caption := range []string{"", " \n", "Explain this photo"} {
		t.Run(caption, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			stdin := &lockedWriteCloser{}
			s := &appServerSession{ctx: ctx, stdin: stdin, workDir: t.TempDir()}
			s.alive.Store(true)
			s.threadID.Store("thread-1")
			done := make(chan error, 1)
			go func() {
				done <- s.Send(caption, "photo-1", []core.ImageAttachment{{MimeType: "image/jpeg", Data: []byte("photo")}}, nil)
			}()
			line := waitForWrittenJSONLine(t, stdin)
			var request struct {
				ID     int64  `json:"id"`
				Method string `json:"method"`
				Params struct {
					Input []struct {
						Type string `json:"type"`
						Text string `json:"text"`
						Path string `json:"path"`
					} `json:"input"`
				} `json:"params"`
			}
			if err := json.Unmarshal([]byte(line), &request); err != nil {
				t.Fatal(err)
			}
			s.pendingMu.Lock()
			ch := s.pending[request.ID]
			delete(s.pending, request.ID)
			s.pendingMu.Unlock()
			ch <- rpcResponseEnvelope{Result: json.RawMessage(`{"turn":{"id":"turn-1"}}`)}
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("Send did not complete")
			}
			if request.Method != "turn/start" {
				t.Fatalf("method = %q", request.Method)
			}
			input := request.Params.Input
			if caption == "Explain this photo" {
				if len(input) != 2 || input[0].Type != "text" || input[0].Text != caption {
					t.Fatalf("caption not preserved: %+v", input)
				}
				input = input[1:]
			}
			if len(input) != 1 || input[0].Type != "localImage" || input[0].Path == "" {
				t.Fatalf("want only photo, got %+v", input)
			}
		})
	}
}

// Exercise Send through a real subprocess, including its argv and stdin.
func TestExecSend_CaptionlessImagesDoNotRequireStdin(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, thread := range []string{"", "existing-thread"} {
		for _, caption := range []string{"", " \n", "Explain\nthis photo"} {
			t.Run(thread+"/"+caption, func(t *testing.T) {
				cs, err := newCodexSession(context.Background(), executable,
					[]string{"-test.run=^TestExecImagePromptHelper$", "--"}, t.TempDir(), "gpt-test", "", "low", thread, "",
					[]string{"CC_IMAGE_PROMPT_HELPER=1", "CC_IMAGE_EXPECTED_CAPTION=" + caption}, "", "", "")
				if err != nil {
					t.Fatal(err)
				}
				defer cs.Close()
				if err := cs.Send(caption, "image", []core.ImageAttachment{{MimeType: "image/png", Data: []byte("photo")}}, nil); err != nil {
					t.Fatal(err)
				}
				deadline := time.After(5 * time.Second)
				for {
					select {
					case event := <-cs.Events():
						if event.Type == core.EventError {
							t.Fatalf("exec rejected image-only input: %v", event.Error)
						}
						if event.Type == core.EventResult && event.Done {
							return
						}
					case <-deadline:
						t.Fatal("exec did not complete")
					}
				}
			})
		}
	}
}

func TestExecImagePromptHelper(t *testing.T) {
	if os.Getenv("CC_IMAGE_PROMPT_HELPER") != "1" {
		return
	}
	args := os.Args
	for len(args) > 0 && args[0] != "--" {
		args = args[1:]
	}
	if len(args) < 2 {
		os.Exit(2)
	}
	args = args[1:]
	stdin, err := io.ReadAll(os.Stdin)
	if err != nil {
		os.Exit(2)
	}
	prompt := args[len(args)-1]
	if prompt == "-" {
		prompt = string(stdin)
		// Codex rejects an empty required stdin prompt before processing images.
		if strings.TrimSpace(prompt) == "" {
			os.Stderr.WriteString("No prompt provided via stdin.")
			os.Exit(1)
		}
	}
	expected := os.Getenv("CC_IMAGE_EXPECTED_CAPTION")
	if strings.TrimSpace(expected) == "" {
		expected = ""
	}
	if prompt != expected {
		os.Stderr.WriteString("caption changed")
		os.Exit(1)
	}
	path := valueAfter(args, "--image")
	if data, err := os.ReadFile(path); err != nil || string(data) != "photo" {
		os.Stderr.WriteString("missing image")
		os.Exit(1)
	}
	os.Stdout.WriteString("{\"type\":\"turn.completed\"}\n")
	os.Exit(0)
}
