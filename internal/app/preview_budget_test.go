package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	configpkg "capelin-go/internal/config"
	"capelin-go/internal/output"
)

// TestAgentQuestionPreviewMaxIsEffectiveEndToEnd pins acceptance criterion 10:
// AGENT_QUESTION_PREVIEW_MAX is resolved by internal/config, mapped onto the
// application config struct by loadConfig, and applied to internal/output by
// the composition root (newApp -> output.SetPreviewMax). Removing the
// `agentQuestionPreviewMax: parsed.AgentQuestionPreviewMax` mapping in
// loadConfig makes the field zero, which restores the built-in 160 budget and
// fails this test.
func TestAgentQuestionPreviewMaxIsEffectiveEndToEnd(t *testing.T) {
	// This test mutates the process-wide preview budget, so it must not run in
	// parallel and must restore whatever value was in effect on entry rather
	// than assuming the built-in default.
	prev := output.PreviewMax()
	t.Cleanup(func() { output.SetPreviewMax(prev) })
	output.SetPreviewMax(0)

	isolateConfigFile(t)
	t.Setenv("IDLE_HOOK_COMMAND", "")
	t.Setenv("IDLE_HOOK_ARGS", "")
	t.Setenv("AGENT_QUESTION_PREVIEW_MAX", "40")

	cfg, err := loadConfig([]string{"task"})
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.agentQuestionPreviewMax != 40 {
		t.Fatalf("agentQuestionPreviewMax = %d, want 40 (config mapping missing?)", cfg.agentQuestionPreviewMax)
	}

	cfg.workspaceRoot = t.TempDir()
	if _, err := newApp(cfg); err != nil {
		t.Fatalf("newApp: %v", err)
	}

	long := strings.Repeat("alpha ", 200)
	rendered := output.Preview(long, false)
	if len([]rune(rendered)) > 40 {
		t.Fatalf("configured cap not applied: %d runes rendered (want <= 40): %q", len([]rune(rendered)), rendered)
	}
	if !strings.Contains(rendered, "… (+") || !strings.HasSuffix(rendered, "more chars)") {
		t.Fatalf("preview lost its truncation suffix: %q", rendered)
	}

	// The rendered agent-question detail suffix built by the interactive status
	// view honours the same configured budget.
	suffix := agentDetailSuffix(long, 0)
	if !strings.Contains(suffix, "question: ") {
		t.Fatalf("agent detail suffix missing question field: %q", suffix)
	}
	question := strings.TrimPrefix(strings.TrimSpace(suffix), "question: ")
	if len([]rune(question)) > 40 {
		t.Fatalf("::agents question preview exceeded the configured cap (%d runes): %q", len([]rune(question)), question)
	}
}

// TestAgentQuestionPreviewMaxDefaultsWhenUnset proves the composition root
// leaves the built-in 160-rune budget in place when the override is absent.
func TestAgentQuestionPreviewMaxDefaultsWhenUnset(t *testing.T) {
	// Shared-global mutation: no t.Parallel, and restore the entry value.
	prev := output.PreviewMax()
	t.Cleanup(func() { output.SetPreviewMax(prev) })
	output.SetPreviewMax(10) // poisoned global; the composition root must reset it

	isolateConfigFile(t)
	t.Setenv("IDLE_HOOK_COMMAND", "")
	t.Setenv("IDLE_HOOK_ARGS", "")
	t.Setenv("AGENT_QUESTION_PREVIEW_MAX", "")

	cfg, err := loadConfig([]string{"task"})
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.agentQuestionPreviewMax != configpkg.DefaultAgentQuestionPreviewMax {
		t.Fatalf("agentQuestionPreviewMax = %d, want %d", cfg.agentQuestionPreviewMax, configpkg.DefaultAgentQuestionPreviewMax)
	}

	workspace := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workspace, "sub"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	cfg.workspaceRoot = workspace
	if _, err := newApp(cfg); err != nil {
		t.Fatalf("newApp: %v", err)
	}

	rendered := output.Preview(strings.Repeat("b", 500), false)
	if got := len([]rune(rendered)); got > configpkg.DefaultAgentQuestionPreviewMax || got < 100 {
		t.Fatalf("default budget not restored: %d runes: %q", got, rendered)
	}
}
