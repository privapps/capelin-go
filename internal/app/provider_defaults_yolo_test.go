package app

import "testing"

func TestLoadConfigYoloPresetReachesApplicationRuntime(t *testing.T) {
	isolateConfigFile(t)
	for _, key := range []string{
		"ENDPOINT", "MODEL", "TOKEN", "REASONING_EFFORT",
		"MAX_ITERATIONS", "MAX_GOAL_ITERATIONS",
		"SUBAGENT_MAX_DEPTH", "SUBAGENT_MAX_PARALLEL", "SUBAGENT_TIMEOUT_SECONDS",
		"SUBAGENT_MAX_AGGREGATE_CHARS", "SUBAGENT_MAX_ITERATIONS",
		"TOOL_MAX_PARALLEL", "TOOL_TIMEOUT_SECONDS",
	} {
		t.Setenv(key, "")
	}

	cfg, err := loadConfig([]string{"--yolo", "task"})
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.endpoint != "https://opencode.ai/zen/v1/chat/completions" || cfg.model != "deepseek-v4-flash-free" || cfg.token != "public" || cfg.reasoning != "high" {
		t.Fatalf("unexpected provider defaults: %+v", cfg)
	}
	if cfg.maxIterations != 256 || cfg.maxGoalIterations != 200 || cfg.toolMaxParallel != 16 || cfg.toolTimeoutSec != 300 {
		t.Fatalf("unexpected application limits: %+v", cfg)
	}
	if got := cfg.subagents; got.MaxDepth != 2 || got.MaxParallel != 8 || got.DefaultTimeoutSec != 600 || got.MaxToolIterations != 100 || got.MaxAggregateChars != 48000 {
		t.Fatalf("unexpected application subagent limits: %+v", got)
	}

	a, err := newApp(cfg)
	if err != nil {
		t.Fatalf("newApp: %v", err)
	}
	if runtime := a.rootRuntime(); runtime.maxToolIterations != 256 {
		t.Fatalf("root runtime lost YOLO iteration limit: %d", runtime.maxToolIterations)
	}
	if a.subagents.cfg.MaxDepth != 2 || a.subagents.cfg.MaxParallel != 8 || a.subagents.cfg.MaxToolIterations != 100 || a.subagents.cfg.MaxAggregateChars != 48000 {
		t.Fatalf("subagent manager lost YOLO limits: %+v", a.subagents.cfg)
	}
}
