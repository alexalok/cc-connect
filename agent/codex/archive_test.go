package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

// The helper accepts only management RPCs; resuming/creating a thread or
// starting a turn fails the protocol check instead of invoking a real model.
func TestArchiveRPCProcess(t *testing.T) {
	if os.Getenv("CC_ARCHIVE_HELPER") != "1" {
		return
	}
	if os.Args[len(os.Args)-1] != "app-server" {
		os.Exit(2)
	}
	cwd, _ := os.Getwd()
	if cwd != os.Getenv("CC_ARCHIVE_WORKDIR") || os.Getenv("CODEX_HOME") != os.Getenv("CC_ARCHIVE_HOME") || os.Getenv("CC_ARCHIVE_OVERRIDE") != "session" {
		os.Exit(3)
	}
	scanner := bufio.NewScanner(os.Stdin)
	for _, method := range []string{"initialize", "initialized", "thread/archive"} {
		if !scanner.Scan() {
			os.Exit(4)
		}
		var req struct {
			ID     int            `json:"id"`
			Method string         `json:"method"`
			Params map[string]any `json:"params"`
		}
		if json.Unmarshal(scanner.Bytes(), &req) != nil || req.Method != method {
			os.Exit(5)
		}
		if method == "initialized" {
			continue
		}
		if method == "thread/archive" {
			if req.Params["threadId"] != "thread-to-archive" {
				os.Exit(6)
			}
			if os.Getenv("CC_ARCHIVE_HANG") == "1" {
				scanner.Scan()
				os.Exit(0)
			}
			if os.Getenv("CC_ARCHIVE_ERROR") == "1" {
				fmt.Printf("{\"id\":%d,\"error\":{\"code\":-1,\"message\":\"archive denied\"}}\n", req.ID)
				continue
			}
		}
		fmt.Printf("{\"id\":%d,\"result\":{}}\n", req.ID)
	}
	scanner.Scan()
	os.Exit(0)
}

func TestArchiveSession_NativeRPCWithConfiguredCLIAndHome(t *testing.T) {
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	workDir, archiveHome := t.TempDir(), t.TempDir()
	for _, mode := range []string{"success", "error", "canceled"} {
		t.Run(mode, func(t *testing.T) {
			a := &Agent{
				cmd: bin, cliExtraArgs: []string{"-test.run=^TestArchiveRPCProcess$", "--"},
				workDir: workDir, codexHome: archiveHome,
				// An unrelated listener must not redirect the archive RPC away from stdio.
				appServerURL: "ws://127.0.0.1:1",
				configEnv:    []string{"CC_ARCHIVE_HELPER=1", "CC_ARCHIVE_WORKDIR=" + workDir, "CC_ARCHIVE_HOME=" + archiveHome, "CC_ARCHIVE_OVERRIDE=config"},
				sessionEnv:   []string{"CC_ARCHIVE_OVERRIDE=session"},
			}
			deadline := 5 * time.Second
			if mode == "error" {
				a.configEnv = append(a.configEnv, "CC_ARCHIVE_ERROR=1")
			}
			if mode == "canceled" {
				a.configEnv = append(a.configEnv, "CC_ARCHIVE_HANG=1")
				deadline = 250 * time.Millisecond
			}
			ctx, cancel := context.WithTimeout(context.Background(), deadline)
			defer cancel()
			start := time.Now()
			err := a.ArchiveSession(ctx, "thread-to-archive")
			if mode == "success" && err != nil {
				t.Fatal(err)
			}
			if mode == "error" && (err == nil || !strings.Contains(err.Error(), "archive denied")) {
				t.Fatalf("error = %v", err)
			}
			if mode == "canceled" && (err == nil || time.Since(start) > 3*time.Second) {
				t.Fatalf("cancellation took %v, error = %v", time.Since(start), err)
			}
		})
	}
}

func TestArchiveSession_RejectsMissingID(t *testing.T) {
	a := &Agent{cmd: "must-not-be-started"}
	for _, id := range []string{"", "  ", core.ContinueSession} {
		if err := a.ArchiveSession(context.Background(), id); err == nil || !strings.Contains(err.Error(), "missing session ID") {
			t.Fatalf("id %q: %v", id, err)
		}
	}
}

func TestArchiveSession_RelativeHomeMatchesSessionProfile(t *testing.T) {
	root, workDir := t.TempDir(), t.TempDir()
	t.Chdir(root)
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, source := range []string{"explicit", "process", "project", "session"} {
		t.Run(source, func(t *testing.T) {
			t.Setenv("CODEX_HOME", "wrong-profile")
			a := &Agent{
				cmd: bin, cliExtraArgs: []string{"-test.run=^TestArchiveRPCProcess$", "--"},
				workDir: workDir, listAllSessions: true,
				configEnv: []string{"CC_ARCHIVE_HELPER=1", "CC_ARCHIVE_WORKDIR=" + workDir,
					"CC_ARCHIVE_HOME=" + filepath.Join(root, "profile"), "CC_ARCHIVE_OVERRIDE=session"},
			}
			switch source {
			case "explicit":
				a.codexHome = "profile"
			case "process":
				t.Setenv("CODEX_HOME", "profile")
			case "project":
				a.configEnv = append(a.configEnv, "CODEX_HOME=profile")
			case "session":
				a.configEnv = append(a.configEnv, "CODEX_HOME=wrong-project-profile")
				a.sessionEnv = []string{"CODEX_HOME=profile"}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := a.ArchiveSession(ctx, "thread-to-archive"); err != nil {
				t.Fatalf("archive must use the profile resolved from the bridge directory: %v", err)
			}
		})
	}
}

func TestArchiveSession_UsesSavedProviderWithoutChangingDefault(t *testing.T) {
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	workDir, defaultHome, savedHome := t.TempDir(), t.TempDir(), t.TempDir()
	a := &Agent{cmd: bin, cliExtraArgs: []string{"-test.run=^TestArchiveRPCProcess$", "--"}, workDir: workDir,
		configEnv: []string{"CC_ARCHIVE_HELPER=1", "CC_ARCHIVE_WORKDIR=" + workDir, "CC_ARCHIVE_HOME=" + savedHome},
		providers: []core.ProviderConfig{
			{Name: "default", Env: map[string]string{"CODEX_HOME": defaultHome, "CC_ARCHIVE_OVERRIDE": "wrong"}},
			{Name: "saved", Env: map[string]string{"CODEX_HOME": savedHome, "CC_ARCHIVE_OVERRIDE": "session"}},
		}, activeIdx: 0}
	archiver, ok := any(a).(interface {
		ArchiveSessionWithProvider(context.Context, string, string) error
	})
	if !ok {
		t.Fatal("archiving cannot select the saved provider")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := archiver.ArchiveSessionWithProvider(ctx, "thread-to-archive", "saved"); err != nil {
		t.Fatal(err)
	}
	if a.GetActiveProvider().Name != "default" {
		t.Fatal("archiving changed another topic's default provider")
	}
	if err := archiver.ArchiveSessionWithProvider(ctx, "thread-to-archive", "removed"); err == nil || !strings.Contains(err.Error(), "not configured") {
		t.Fatalf("missing provider must fail without default fallback: %v", err)
	}
}
