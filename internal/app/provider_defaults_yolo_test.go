package app

import "testing"

func TestLoadConfigYoloKeepsOrdinaryLimitsInApplicationRuntime(t *testing.T) {
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
	if cfg.maxIterations != 40 || cfg.maxGoalIterations != 20 || cfg.toolMaxParallel != 8 || cfg.toolTimeoutSec != 60 {
		t.Fatalf("YOLO changed ordinary application limits: %+v", cfg)
	}
	if got := cfg.subagents; got.MaxDepth != 1 || got.MaxParallel != 4 || got.DefaultTimeoutSec != 600 || got.MaxToolIterations != 20 || got.MaxAggregateChars != 12000 {
		t.Fatalf("YOLO changed ordinary application subagent limits: %+v", got)
	}

	a, err := newApp(cfg)
	if err != nil {
		t.Fatalf("newApp: %v", err)
	}
	if runtime := a.rootRuntime(); runtime.maxToolIterations != 40 {
		t.Fatalf("root runtime did not retain ordinary iteration limit: %d", runtime.maxToolIterations)
	}
	if runtime := a.rootRuntime(); runtime.executionProfile.MaxIterations != 40 || runtime.executionProfile.ToolMaxParallel != 8 || runtime.executionProfile.ToolTimeoutSec != 60 {
		t.Fatalf("root runtime did not retain ordinary execution profile: %+v", runtime.executionProfile)
	}
	if cfg.goalProfile.MaxIterations != 256 || cfg.goalProfile.MaxGoalIterations != 64 || cfg.goalProfile.ToolMaxParallel != 16 || cfg.goalProfile.ToolTimeoutSec != 300 {
		t.Fatalf("loaded application did not retain goal execution profile: %+v", cfg.goalProfile)
	}
	if a.subagents.cfg.MaxDepth != 1 || a.subagents.cfg.MaxParallel != 4 || a.subagents.cfg.MaxToolIterations != 20 || a.subagents.cfg.MaxAggregateChars != 12000 {
		t.Fatalf("subagent manager did not retain ordinary limits: %+v", a.subagents.cfg)
	}
}
