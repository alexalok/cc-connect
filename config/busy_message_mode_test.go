package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadBusyMessageMode(t *testing.T) {
	for _, mode := range []string{"", "queue", "steer", " STEER ", "interrupt", "true"} {
		t.Run(mode, func(t *testing.T) {
			setting := ""
			if mode != "" {
				setting = fmt.Sprintf("busy_message_mode = %q\n", mode)
			}
			path := filepath.Join(t.TempDir(), "config.toml")
			body := "[[projects]]\nname = \"demo\"\n" + setting + "[projects.agent]\ntype = \"codex\"\n[[projects.platforms]]\ntype = \"telegram\"\n"
			if err := os.WriteFile(path, []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			cfg, err := Load(path)
			invalid := mode == "interrupt" || mode == "true"
			if invalid {
				if err == nil || !strings.Contains(err.Error(), "busy_message_mode") {
					t.Fatalf("invalid mode accepted: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := cfg.Projects[0].BusyMessageMode; got != mode {
				t.Fatalf("loaded mode = %q, want %q", got, mode)
			}
		})
	}
}
