package config

import (
	"strings"
	"testing"
)

func TestLoad_ResumeFallbackToNew(t *testing.T) {
	for _, value := range []string{"", "true", "false"} {
		t.Run("value="+value, func(t *testing.T) {
			setting := ""
			if value != "" {
				setting = "resume_fallback_to_new = " + value
			}
			fixture := strings.Replace(projectWithResetOnIdleFixture, "reset_on_idle_mins = 60", setting, 1)
			cfg, err := Load(writeConfigFixture(t, fixture))
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			got := cfg.Projects[0].ResumeFallbackToNew
			if value == "" {
				if got != nil {
					t.Fatalf("omitted option = %v, want nil for backwards-compatible default", *got)
				}
			} else if got == nil || *got != (value == "true") {
				t.Fatalf("resume_fallback_to_new = %v, want %s", got, value)
			}
		})
	}
}
