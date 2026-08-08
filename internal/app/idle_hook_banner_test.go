package app

import (
	"bytes"
	"context"
	"io"
	"os"
	"strings"
	"testing"
)

// TestIdleHookBannerReportsCommandAndSource proves the startup banner names the
// command and its resolved configuration source exactly once.
func TestIdleHookBannerReportsCommandAndSource(t *testing.T) {
	t.Setenv("IDLE_HOOK_COMMAND", "env-hook")
	oldStderr := os.Stderr
	r, w, _ := os.Pipe()
	os.Stderr = w

	cfg, err := loadConfig([]string{"task"})
	if err != nil {
		os.Stderr = oldStderr
		_ = w.Close()
		t.Fatalf("loadConfig: %v", err)
	}
	if _, err := newApp(cfg); err != nil {
		os.Stderr = oldStderr
		_ = w.Close()
		t.Fatalf("newApp: %v", err)
	}
	_ = w.Close()
	os.Stderr = oldStderr

	var buf bytes.Buffer
	_, _ = io.Copy(&buf, r)
	got := buf.String()
	if !strings.Contains(got, "idle hook enabled") || !strings.Contains(got, "env-hook") || !strings.Contains(got, "env") {
		t.Fatalf("startup banner missing command/source: %q", got)
	}
	if strings.Count(got, "idle hook enabled") != 1 {
		t.Fatalf("startup banner printed multiple times: %q", got)
	}
	_ = context.Background()
}
