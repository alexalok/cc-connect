package codex

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

func TestAgent_NativeSkillPromptUsesCanonicalName(t *testing.T) {
	formatter, ok := any(&Agent{}).(interface {
		FormatSkillInvocation(*core.Skill, []string) string
	})
	if !ok {
		t.Fatal("Codex must format native skill invocations")
	}
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"pr-loop", nil, "$pr-loop"},
		{"pr-loop", []string{"codex", "against", "our", "fork"}, "$pr-loop codex against our fork"},
		{"plugin:review-code", []string{"review"}, "$plugin:review-code review"},
	} {
		skill := &core.Skill{Name: tc.name, DisplayName: "Friendly display name", Prompt: "FULL SKILL BODY"}
		if got := formatter.FormatSkillInvocation(skill, tc.args); got != tc.want {
			t.Errorf("native prompt = %q, want %q", got, tc.want)
		}
	}
}

type skillRPCRequest struct {
	ID     int64          `json:"id"`
	Method string         `json:"method"`
	Params map[string]any `json:"params"`
}

type skillRPCWriter struct{ requests chan skillRPCRequest }

func (w *skillRPCWriter) Write(data []byte) (int, error) {
	var request skillRPCRequest
	if err := json.Unmarshal(data, &request); err != nil {
		return 0, err
	}
	w.requests <- request
	return len(data), nil
}
func (*skillRPCWriter) Close() error { return nil }

func TestAppServerSession_NativeSkillInputPreservesAttachmentsAndReceipt(t *testing.T) {
	for _, accepted := range []bool{true, false} {
		name := "accepted"
		if !accepted {
			name = "rejected"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			writer := &skillRPCWriter{requests: make(chan skillRPCRequest, 1)}
			s := &appServerSession{ctx: ctx, stdin: writer, workDir: t.TempDir(), promptPreamble: "Keep project context"}
			s.threadID.Store("existing-thread")
			s.alive.Store(true)
			sender, ok := any(s).(interface {
				SendSkill(string, string, []core.ImageAttachment, []core.FileAttachment, *core.Skill, func()) error
			})
			if !ok {
				t.Fatal("app-server must send native skill input")
			}
			skill := &core.Skill{Name: "plugin:review-code", DisplayName: "Friendly display name", Source: filepath.Join(t.TempDir(), "skills with spaces", "review-code"), Prompt: "FULL SKILL BODY MUST NOT BE PASTED"}
			images := []core.ImageAttachment{{MimeType: "image/png", Data: []byte("image data")}}
			files := []core.FileAttachment{{FileName: "review.txt", Data: []byte("file data")}}
			var receipts atomic.Int32
			done := make(chan error, 1)
			go func() {
				done <- sender.SendSkill("$plugin:review-code codex", "message-42", images, files, skill, func() { receipts.Add(1) })
			}()
			var request skillRPCRequest
			select {
			case request = <-writer.requests:
			case <-time.After(time.Second):
				t.Fatal("missing turn/start request")
			}
			if request.Method != "turn/start" || request.Params["threadId"] != "existing-thread" {
				t.Fatalf("request = %+v", request)
			}
			if request.Params["turnTrigger"] != mirrorTurnOrigin || !strings.Contains(request.Params["clientUserMessageId"].(string), "message-42") {
				t.Fatalf("missing bridge origin: %v", request.Params)
			}
			input := request.Params["input"].([]any)
			if len(input) != 3 {
				t.Fatalf("inputs = %v; want text, image, skill", input)
			}
			seen := map[string]bool{}
			for _, raw := range input {
				item := raw.(map[string]any)
				kind := item["type"].(string)
				seen[kind] = true
				switch kind {
				case "text":
					text := item["text"].(string)
					filePath := filepath.Join(s.workDir, ".cc-connect", "attachments", "message-42", "review.txt")
					for _, want := range []string{"Keep project context", "$plugin:review-code codex", filePath} {
						if !strings.Contains(text, want) {
							t.Errorf("prompt %q missing %q", text, want)
						}
					}
					if strings.Contains(text, skill.Prompt) || strings.Contains(text, "Skill Instructions:") {
						t.Fatalf("expanded skill leaked into prompt: %q", text)
					}
					if data, err := os.ReadFile(filePath); err != nil || string(data) != "file data" {
						t.Fatalf("staged file = %q, %v", data, err)
					}
				case "localImage":
					if data, err := os.ReadFile(item["path"].(string)); err != nil || string(data) != "image data" {
						t.Fatalf("staged image = %q, %v", data, err)
					}
				case "skill":
					if item["name"] != skill.Name || item["path"] != filepath.Join(skill.Source, "SKILL.md") {
						t.Fatalf("wrong skill selected: %v", item)
					}
				}
			}
			if !seen["text"] || !seen["localImage"] || !seen["skill"] {
				t.Fatalf("missing input types: %v", seen)
			}
			if receipts.Load() != 0 {
				t.Fatal("receipt fired before turn acknowledgment")
			}
			response := rpcResponseEnvelope{ID: request.ID, Result: json.RawMessage(`{"turn":{"id":"turn-1"}}`)}
			if !accepted {
				response.Error = &rpcError{Code: -1, Message: "skill unavailable"}
			}
			s.handleResponse(response)
			select {
			case err := <-done:
				if (err == nil) != accepted {
					t.Fatalf("SendSkill error = %v, accepted = %v", err, accepted)
				}
			case <-time.After(time.Second):
				t.Fatal("skill send did not finish")
			}
			wantReceipts := int32(0)
			if accepted {
				wantReceipts = 1
			}
			if receipts.Load() != wantReceipts {
				t.Fatalf("receipts = %d, want %d", receipts.Load(), wantReceipts)
			}

			go func() { done <- s.Send("ordinary follow-up", "message-43", nil, nil) }()
			select {
			case request = <-writer.requests:
			case <-time.After(time.Second):
				t.Fatal("missing follow-up request")
			}
			input = request.Params["input"].([]any)
			if len(input) != 1 || input[0].(map[string]any)["text"] != "ordinary follow-up" {
				t.Fatalf("skill selection leaked into next turn: %v", input)
			}
			s.handleResponse(rpcResponseEnvelope{ID: request.ID, Result: json.RawMessage(`{"turn":{"id":"turn-2"}}`)})
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		})
	}
}
