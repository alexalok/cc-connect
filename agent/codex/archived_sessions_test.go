package codex

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func archiveDiscoveryRollout(t *testing.T, home, id, origin string) string {
	t.Helper()
	path := writeDiscoveryRollout(t, home, id, t.TempDir(), origin, "vscode", time.Now().Add(-24*time.Hour))
	dir := filepath.Join(home, "archived_sessions")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	archived := filepath.Join(dir, filepath.Base(path))
	if err := os.Rename(path, archived); err != nil {
		t.Fatal(err)
	}
	return archived
}

func TestArchivedSessionIDs_ExternalArchiveRequiresPositiveMetadata(t *testing.T) {
	home := t.TempDir()
	desktop := archiveDiscoveryRollout(t, home, "desktop", "codex-tui")
	before, err := os.ReadFile(desktop)
	if err != nil {
		t.Fatal(err)
	}
	archiveDiscoveryRollout(t, home, "bot", "cc-connect-codex-agent")
	archiveDiscoveryRollout(t, home, "unrelated", "codex-tui")
	archiveDiscoveryRollout(t, home, "resumed", "codex-tui")
	writeDiscoveryRollout(t, home, "resumed", t.TempDir(), "codex-tui", "vscode", time.Now())
	writeDiscoveryRollout(t, home, "active", t.TempDir(), "codex-tui", "vscode", time.Now())
	// A copied parent record later in a child's archive is not an archive of
	// the parent. Neither filenames nor missing live files prove an archive.
	child := archiveDiscoveryRollout(t, home, "child", "codex-tui")
	data, err := os.ReadFile(child)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(child, append(data, []byte(`{"type":"session_meta","payload":{"id":"missing"}}`)...), 0o600); err != nil {
		t.Fatal(err)
	}
	a := &Agent{codexHome: home}
	got, err := a.ArchivedSessionIDs(context.Background(), []string{"desktop", "bot", "active", "resumed", "missing"})
	if err != nil || !reflect.DeepEqual(got, []string{"bot", "desktop"}) {
		t.Fatalf("archived = %v, %v", got, err)
	}
	after, err := os.ReadFile(desktop)
	if err != nil || string(before) != string(after) {
		t.Fatal("archive scan changed native history")
	}
}

func TestArchivedSessionIDs_UsesEffectiveProfileAndHandlesUnarchive(t *testing.T) {
	home, wrongHome := t.TempDir(), t.TempDir()
	path := archiveDiscoveryRollout(t, home, "chat", "codex-tui")
	archiveDiscoveryRollout(t, wrongHome, "wrong", "codex-tui")
	t.Setenv("CODEX_HOME", wrongHome)
	a := &Agent{workDir: t.TempDir(), configEnv: []string{"CODEX_HOME=" + home}}
	got, err := a.ArchivedSessionIDs(context.Background(), []string{"chat", "wrong"})
	if err != nil || !reflect.DeepEqual(got, []string{"chat"}) {
		t.Fatalf("profile archives = %v, %v", got, err)
	}
	if err := os.Rename(path, filepath.Join(home, "sessions", "restored.jsonl")); err != nil {
		t.Fatal(err)
	}
	got, err = a.ArchivedSessionIDs(context.Background(), []string{"chat"})
	if err != nil || len(got) != 0 {
		t.Fatalf("unarchived = %v, %v", got, err)
	}
	a.codexHome = wrongHome
	got, err = a.ArchivedSessionIDs(context.Background(), []string{"wrong"})
	if err != nil || !reflect.DeepEqual(got, []string{"wrong"}) {
		t.Fatalf("explicit profile archives = %v, %v", got, err)
	}
}

func TestArchivedSessionIDs_IncompleteScanNeverReturnsArchive(t *testing.T) {
	home := t.TempDir()
	archiveDiscoveryRollout(t, home, "chat", "codex-tui")
	path := filepath.Join(home, "sessions", "partial.jsonl")
	if err := os.WriteFile(path, []byte(`{"type":`), 0o600); err != nil {
		t.Fatal(err)
	}
	a := &Agent{codexHome: home}
	if got, err := a.ArchivedSessionIDs(context.Background(), []string{"chat"}); err == nil || len(got) != 0 {
		t.Fatalf("partial scan = %v, %v", got, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := a.ArchivedSessionIDs(ctx, []string{"chat"}); err == nil {
		t.Fatal("ignored cancellation")
	}
	a.codexHome = t.TempDir()
	if got, err := a.ArchivedSessionIDs(context.Background(), []string{"missing"}); err != nil || len(got) != 0 {
		t.Fatalf("empty profile = %v, %v", got, err)
	}
}
