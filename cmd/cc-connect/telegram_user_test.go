//go:build !no_telegram_user

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chenhg5/cc-connect/config"
	"github.com/chenhg5/cc-connect/core"
)

func TestTelegramUserLoginSelectsProject(t *testing.T) {
	i18n := core.NewI18n(core.LangEnglish)
	cfg := &config.Config{Projects: []config.ProjectConfig{
		{Name: "one", Platforms: []config.PlatformConfig{{Type: "telegram_user", Options: map[string]any{"api_id": 1}}}},
		{Name: "two", Platforms: []config.PlatformConfig{{Type: "telegram_user", Options: map[string]any{"api_id": 2}}}},
	}}
	if _, err := telegramUserOptions(cfg, "", i18n); err == nil {
		t.Fatal("ambiguous account selection succeeded")
	}
	if _, err := telegramUserOptions(cfg, "missing", i18n); err == nil {
		t.Fatal("missing project accepted")
	}
	opts, err := telegramUserOptions(cfg, "two", i18n)
	if err != nil || opts["api_id"] != 2 {
		t.Fatalf("wrong account selected: %v, %v", opts, err)
	}
	cfg.Projects = cfg.Projects[:1]
	if _, err := telegramUserOptions(cfg, "", i18n); err != nil {
		t.Fatal(err)
	}
	if topLevelCommandHandlers["telegram-user"] == nil {
		t.Fatal("login command not registered")
	}
}

func TestTelegramUserLogin_LocalizesProjectSelectionError(t *testing.T) {
	for _, language := range []struct{ code, diagnostic string }{
		{"en", "expected exactly one"},
		{"zh", "需要且只能配置一个"},
		{"zh-TW", "需要且只能設定一個"},
		{"ja", "ちょうど1つ"},
		{"es", "exactamente una"},
	} {
		t.Run(language.code, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			content := fmt.Sprintf(`language = %q
[[projects]]
name = "one"
[projects.agent]
type = "codex"
[[projects.platforms]]
type = "telegram_user"
[[projects]]
name = "two"
[projects.agent]
type = "codex"
[[projects.platforms]]
type = "telegram_user"
`, language.code)
			if err := os.WriteFile(path, []byte(content), 0600); err != nil {
				t.Fatal(err)
			}
			for _, selection := range []struct{ project, count string }{{"", "2"}, {"missing", "0"}} {
				args := []string{"login", "--config", path}
				if selection.project != "" {
					args = append(args, "--project", selection.project)
				}
				err := telegramUserLogin(args)
				if err == nil || !strings.Contains(err.Error(), language.diagnostic) ||
					!strings.Contains(err.Error(), selection.count) || !strings.Contains(err.Error(), "--project") {
					t.Fatalf("login selection diagnostic for project %q: %v", selection.project, err)
				}
			}
		})
	}
}
