//go:build !no_telegram_user

package main

import (
	"testing"

	"github.com/chenhg5/cc-connect/config"
)

func TestTelegramUserLoginSelectsProject(t *testing.T) {
	cfg := &config.Config{Projects: []config.ProjectConfig{
		{Name: "one", Platforms: []config.PlatformConfig{{Type: "telegram_user", Options: map[string]any{"api_id": 1}}}},
		{Name: "two", Platforms: []config.PlatformConfig{{Type: "telegram_user", Options: map[string]any{"api_id": 2}}}},
	}}
	if _, err := telegramUserOptions(cfg, ""); err == nil {
		t.Fatal("ambiguous account selection succeeded")
	}
	if _, err := telegramUserOptions(cfg, "missing"); err == nil {
		t.Fatal("missing project accepted")
	}
	opts, err := telegramUserOptions(cfg, "two")
	if err != nil || opts["api_id"] != 2 {
		t.Fatalf("wrong account selected: %v, %v", opts, err)
	}
	cfg.Projects = cfg.Projects[:1]
	if _, err := telegramUserOptions(cfg, ""); err != nil {
		t.Fatal(err)
	}
	if topLevelCommandHandlers["telegram-user"] == nil {
		t.Fatal("login command not registered")
	}
}
