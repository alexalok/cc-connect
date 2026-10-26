package codex

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func writeDiscoveryRollout(t *testing.T, home, id, cwd, originator string, source any, created time.Time) string {
	t.Helper()
	path := writeAllSessionsRollout(t, home, id, cwd, source)
	data, err := json.Marshal(map[string]any{"type": "session_meta", "timestamp": time.Now(), "payload": map[string]any{
		"id": id, "cwd": cwd, "originator": originator, "source": source, "timestamp": created,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestDiscoverSessions_NewTopLevelChatsOnlyAndReadOnly(t *testing.T) {
	home, cwd := t.TempDir(), t.TempDir()
	since := time.Now().Add(-time.Minute)
	recent, old := since.Add(time.Second), since.Add(-time.Hour)
	writeDiscoveryRollout(t, home, "tui", cwd, "codex_cli_rs", "cli", recent)
	writeDiscoveryRollout(t, home, "desktop", cwd, "codex-tui", "vscode", recent)
	writeDiscoveryRollout(t, home, "old-touched-today", cwd, "codex_cli_rs", "cli", old)
	writeDiscoveryRollout(t, home, "missing-created-at", cwd, "codex_cli_rs", "cli", time.Time{})
	writeDiscoveryRollout(t, home, "bridge", cwd, "cc-connect-codex-agent", "vscode", recent)
	execPath := writeDiscoveryRollout(t, home, "exec", cwd, "codex_exec", "exec", recent)
	subagent := writeDiscoveryRollout(t, home, "child", cwd, "codex-tui", map[string]any{"subagent": map[string]any{}}, recent)
	parent, err := os.ReadFile(filepath.Join(filepath.Dir(subagent), "rollout-desktop.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(subagent, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(parent); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	archived := writeDiscoveryRollout(t, home, "archived", cwd, "codex_cli_rs", "cli", recent)
	if err := os.MkdirAll(filepath.Join(home, "archived_sessions"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(archived, filepath.Join(home, "archived_sessions", "chat.jsonl")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "session_index.jsonl"), []byte("{\"id\":\"desktop\",\"thread_name\":\"Desktop title\"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(execPath)
	if err != nil {
		t.Fatal(err)
	}
	a := &Agent{codexHome: home, workDir: cwd, listAllSessions: true}
	got, err := a.DiscoverSessions(context.Background(), since)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, info := range got {
		ids = append(ids, info.ID)
	}
	if !reflect.DeepEqual(ids, []string{"desktop", "tui"}) {
		t.Fatalf("discovered %v", ids)
	}
	if got[0].Summary != "Desktop title" {
		t.Fatalf("title = %q", got[0].Summary)
	}
	after, err := os.ReadFile(execPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("discovery rewrote an exec transcript")
	}
}

func TestDiscoverSessions_RespectsDirectoryAndEffectiveProfile(t *testing.T) {
	home, otherHome, cwd, otherCwd := t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir()
	since := time.Now().Add(-time.Minute)
	writeDiscoveryRollout(t, home, "local", cwd, "codex_cli_rs", "cli", time.Now())
	writeDiscoveryRollout(t, home, "other-directory", otherCwd, "codex-tui", "vscode", time.Now())
	writeDiscoveryRollout(t, otherHome, "other-profile", cwd, "codex_cli_rs", "cli", time.Now())
	t.Setenv("CODEX_HOME", otherHome)
	a := &Agent{workDir: cwd, configEnv: []string{"CODEX_HOME=" + home}}
	got, err := a.DiscoverSessions(context.Background(), since)
	if err != nil || len(got) != 1 || got[0].ID != "local" {
		t.Fatalf("scoped discovery = %+v, %v", got, err)
	}
	a.listAllSessions = true
	got, err = a.DiscoverSessions(context.Background(), since)
	if err != nil || len(got) != 2 {
		t.Fatalf("all directories = %+v, %v", got, err)
	}
	a.codexHome = otherHome
	got, err = a.DiscoverSessions(context.Background(), since)
	if err != nil || len(got) != 1 || got[0].ID != "other-profile" {
		t.Fatalf("explicit profile = %+v, %v", got, err)
	}
}

func TestDiscoverSessions_PartialMetadataRetriedAndFirstPromptTitle(t *testing.T) {
	home, cwd := t.TempDir(), t.TempDir()
	since := time.Now().Add(-time.Minute)
	path := writeDiscoveryRollout(t, home, "chat", cwd, "codex_cli_rs", "cli", time.Now())
	complete, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, complete[:len(complete)/2], 0o600); err != nil {
		t.Fatal(err)
	}
	a := &Agent{codexHome: home, workDir: cwd}
	got, err := a.DiscoverSessions(context.Background(), since)
	if err != nil || len(got) != 0 {
		t.Fatalf("partial discovery = %+v, %v", got, err)
	}
	for _, text := range []string{"# AGENTS.md instructions for /project", "Fix\n the build", "Later message"} {
		item, err := json.Marshal(map[string]any{"type": "response_item", "payload": map[string]any{
			"role": "user", "content": []map[string]string{{"type": "input_text", "text": text}},
		}})
		if err != nil {
			t.Fatal(err)
		}
		complete = append(complete, append(item, '\n')...)
	}
	if err := os.WriteFile(path, complete, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err = a.DiscoverSessions(context.Background(), since)
	if err != nil || len(got) != 1 || got[0].Summary != "Fix the build" {
		t.Fatalf("completed discovery = %+v, %v", got, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := a.DiscoverSessions(ctx, since); err == nil {
		t.Fatal("ignored cancellation")
	}
}

func TestSessionHistory_UsesDiscoveryProfileWithDirectoryScope(t *testing.T) {
	home, otherHome, cwd := t.TempDir(), t.TempDir(), t.TempDir()
	for _, profile := range []struct{ home, prompt string }{{home, "Selected profile history"}, {otherHome, "Wrong profile history"}} {
		path := writeDiscoveryRollout(t, profile.home, "same-id", cwd, "codex_cli_rs", "cli", time.Now())
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		item, err := json.Marshal(map[string]any{"type": "response_item", "payload": map[string]any{
			"role": "user", "content": []map[string]string{{"type": "input_text", "text": profile.prompt}},
		}})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, append(data, append(item, '\n')...), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("CODEX_HOME", otherHome)
	a := &Agent{workDir: cwd, configEnv: []string{"CODEX_HOME=" + home}}
	entries, err := a.GetSessionHistory(context.Background(), "same-id", 10)
	if err != nil || len(entries) != 1 || entries[0].Content != "Selected profile history" {
		t.Fatalf("history disagrees with discovery profile: %+v, %v", entries, err)
	}
}

func TestDiscoverSessions_ResumedSegmentDoesNotImportOldChat(t *testing.T) {
	home, cwd := t.TempDir(), t.TempDir()
	since := time.Now().Add(-time.Minute)
	path := writeDiscoveryRollout(t, home, "old-chat", t.TempDir(), "codex_cli_rs", "cli", since.Add(-time.Hour))
	if err := os.Rename(path, path+".old.jsonl"); err != nil {
		t.Fatal(err)
	}
	writeDiscoveryRollout(t, home, "old-chat", cwd, "codex_cli_rs", "cli", since.Add(time.Second))
	a := &Agent{codexHome: home, workDir: cwd}
	got, err := a.DiscoverSessions(context.Background(), since)
	if err != nil || len(got) != 0 {
		t.Fatalf("imported resumed old chat: %+v, %v", got, err)
	}
	// Multiple segments of a new chat still yield just one topic candidate.
	path = writeDiscoveryRollout(t, home, "new-chat", cwd, "codex_cli_rs", "cli", since.Add(time.Second))
	if err := os.Rename(path, path+".first.jsonl"); err != nil {
		t.Fatal(err)
	}
	writeDiscoveryRollout(t, home, "new-chat", cwd, "codex_cli_rs", "cli", since.Add(2*time.Second))
	got, err = a.DiscoverSessions(context.Background(), since)
	if err != nil || len(got) != 1 || got[0].ID != "new-chat" {
		t.Fatalf("split discovery = %+v, %v", got, err)
	}
}
