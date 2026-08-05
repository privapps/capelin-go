package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var numericConfigKeys = []string{
	"MAX_ITERATIONS",
	"MAX_GOAL_ITERATIONS",
	"SUBAGENT_MAX_DEPTH",
	"SUBAGENT_MAX_CHILDREN",
	"SUBAGENT_MAX_PARALLEL",
	"SUBAGENT_TIMEOUT_SECONDS",
	"SUBAGENT_MAX_RESULT_CHARS",
	"SUBAGENT_MAX_AGGREGATE_CHARS",
	"SUBAGENT_MAX_ITERATIONS",
	"TOOL_MAX_PARALLEL",
	"TOOL_TIMEOUT_SECONDS",
}

func isolateLoad(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.ini")
	t.Setenv("CAPELIN_CONFIG_FILE", path)
	for _, key := range append([]string{
		"ENDPOINT", "MODEL", "TOKEN", "REASONING_EFFORT", "SYSTEM_PROMPT",
		"SUBAGENT_MODEL", "SUBAGENT_REASONING_EFFORT",
	}, numericConfigKeys...) {
		t.Setenv(key, "")
	}
	return path
}

func TestLoadFirstRunUsesProviderDefaultsAndGeneratesOrdinaryConfig(t *testing.T) {
	path := isolateLoad(t)

	cfg, err := Load([]string{"task"})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Endpoint != defaultEndpoint || cfg.Model != defaultModel || cfg.Token != defaultToken || cfg.Reasoning != defaultReasoning {
		t.Fatalf("unexpected provider defaults: endpoint=%q model=%q token=%q reasoning=%q", cfg.Endpoint, cfg.Model, cfg.Token, cfg.Reasoning)
	}
	if cfg.MaxIterations != defaultMaxIterations || cfg.MaxGoalIterations != defaultMaxGoalIterations {
		t.Fatalf("unexpected ordinary iteration defaults: root=%d goal=%d", cfg.MaxIterations, cfg.MaxGoalIterations)
	}
	if got := cfg.Subagents; got.MaxDepth != defaultSubagentMaxDepth || got.MaxChildren != defaultSubagentMaxChildren || got.MaxParallel != defaultSubagentMaxParallel || got.DefaultTimeoutSec != defaultSubagentDefaultTimeoutSec || got.MaxToolIterations != defaultSubagentToolIterations || got.MaxResultChars != defaultSubagentResultChars || got.MaxAggregateCount != defaultSubagentAggregateCount || got.MaxAggregateChars != defaultSubagentAggregateChars || got.MaxTimeoutSec != defaultSubagentMaxTimeoutSec {
		t.Fatalf("unexpected ordinary subagent defaults: %+v", got)
	}
	if cfg.ToolMaxParallel != defaultToolMaxParallel || cfg.ToolTimeoutSec != defaultToolTimeoutSec {
		t.Fatalf("unexpected ordinary tool defaults: parallel=%d timeout=%d", cfg.ToolMaxParallel, cfg.ToolTimeoutSec)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	contents := string(data)
	for _, want := range []string{
		"ENDPOINT = " + defaultEndpoint,
		"MODEL = " + defaultModel,
		"TOKEN = " + defaultToken,
		"REASONING_EFFORT = " + defaultReasoning,
		"MAX_ITERATIONS = 40",
		"MAX_GOAL_ITERATIONS = 20",
	} {
		if !strings.Contains(contents, want) {
			t.Fatalf("generated config missing %q:\n%s", want, contents)
		}
	}
}

func TestLoadYoloUsesBoundedPresetWithoutChangingSavedProviderDefaults(t *testing.T) {
	path := isolateLoad(t)

	cfg, err := Load([]string{"--yolo", "task"})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Endpoint != defaultEndpoint || cfg.Model != defaultModel || cfg.Token != defaultToken || cfg.Reasoning != defaultReasoning {
		t.Fatalf("YOLO changed provider defaults: endpoint=%q model=%q token=%q reasoning=%q", cfg.Endpoint, cfg.Model, cfg.Token, cfg.Reasoning)
	}
	if cfg.MaxIterations != yoloMaxIterations || cfg.MaxGoalIterations != yoloMaxGoalIterations {
		t.Fatalf("unexpected YOLO iteration defaults: root=%d goal=%d", cfg.MaxIterations, cfg.MaxGoalIterations)
	}
	if got := cfg.Subagents; got.MaxDepth != yoloSubagentMaxDepth || got.MaxChildren != defaultSubagentMaxChildren || got.MaxParallel != yoloSubagentMaxParallel || got.DefaultTimeoutSec != yoloSubagentTimeoutSec || got.MaxToolIterations != yoloSubagentToolIterations || got.MaxResultChars != defaultSubagentResultChars || got.MaxAggregateCount != defaultSubagentAggregateCount || got.MaxAggregateChars != yoloSubagentAggregateChars || got.MaxTimeoutSec != defaultSubagentMaxTimeoutSec {
		t.Fatalf("unexpected YOLO subagent defaults: %+v", got)
	}
	if cfg.ToolMaxParallel != yoloToolMaxParallel || cfg.ToolTimeoutSec != yoloToolTimeoutSec {
		t.Fatalf("unexpected YOLO tool defaults: parallel=%d timeout=%d", cfg.ToolMaxParallel, cfg.ToolTimeoutSec)
	}

	saved, err := readConfigFile(path)
	if err != nil {
		t.Fatalf("read generated config: %v", err)
	}
	if saved["MAX_ITERATIONS"] != "40" || saved["MAX_GOAL_ITERATIONS"] != "20" || saved["SUBAGENT_MAX_ITERATIONS"] != "20" || saved["TOOL_MAX_PARALLEL"] != "8" || saved["TOOL_TIMEOUT_SECONDS"] != "60" {
		t.Fatalf("YOLO rewrote ordinary saved limits: %#v", saved)
	}
}

func TestLoadYoloPreservesCustomizedSavedValues(t *testing.T) {
	path := isolateLoad(t)
	contents := strings.Join([]string{
		"ENDPOINT = https://custom.example/v1/chat/completions",
		"MODEL = custom-model",
		"TOKEN = custom-token",
		"REASONING_EFFORT = low",
		"MAX_ITERATIONS = 77",
		"MAX_GOAL_ITERATIONS = 88",
		"SUBAGENT_MAX_DEPTH = 3",
		"SUBAGENT_MAX_PARALLEL = 6",
		"SUBAGENT_TIMEOUT_SECONDS = 123",
		"SUBAGENT_MAX_AGGREGATE_CHARS = 24000",
		"SUBAGENT_MAX_ITERATIONS = 55",
		"TOOL_MAX_PARALLEL = 9",
		"TOOL_TIMEOUT_SECONDS = 90",
		"",
	}, "\n")
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg, err := Load([]string{"--yolo", "task"})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Endpoint != "https://custom.example/v1/chat/completions" || cfg.Model != "custom-model" || cfg.Token != "custom-token" || cfg.Reasoning != "low" {
		t.Fatalf("custom provider settings were not preserved: %+v", cfg)
	}
	if cfg.MaxIterations != 77 || cfg.MaxGoalIterations != 88 || cfg.Subagents.MaxDepth != 3 || cfg.Subagents.MaxParallel != 6 || cfg.Subagents.DefaultTimeoutSec != 123 || cfg.Subagents.MaxAggregateChars != 24000 || cfg.Subagents.MaxToolIterations != 55 || cfg.ToolMaxParallel != 9 || cfg.ToolTimeoutSec != 90 {
		t.Fatalf("custom saved values were not preserved: %+v", cfg)
	}
	saved, err := readConfigFile(path)
	if err != nil {
		t.Fatalf("read migrated config: %v", err)
	}
	if saved["ENDPOINT"] != "https://custom.example/v1/chat/completions" || saved["MODEL"] != "custom-model" || saved["TOKEN"] != "custom-token" || saved["REASONING_EFFORT"] != "low" || saved["MAX_ITERATIONS"] != "77" {
		t.Fatalf("migration rewrote existing values: %#v", saved)
	}
	if _, ok := saved["SUBAGENT_MAX_CHILDREN"]; !ok {
		t.Fatal("migration did not append missing keys")
	}
}

func TestLoadExplicitCLIAndEnvironmentValuesOverrideYoloPreset(t *testing.T) {
	isolateLoad(t)
	t.Setenv("MAX_ITERATIONS", "41")
	t.Setenv("MAX_GOAL_ITERATIONS", "42")
	t.Setenv("SUBAGENT_MAX_DEPTH", "43")
	t.Setenv("SUBAGENT_MAX_PARALLEL", "44")
	t.Setenv("SUBAGENT_TIMEOUT_SECONDS", "45")
	t.Setenv("SUBAGENT_MAX_AGGREGATE_CHARS", "46")
	t.Setenv("SUBAGENT_MAX_ITERATIONS", "47")
	t.Setenv("TOOL_MAX_PARALLEL", "48")
	t.Setenv("TOOL_TIMEOUT_SECONDS", "49")

	cfg, err := Load([]string{"--yolo", "--max-iterations", "51", "--subagent-max-parallel", "52", "--tool-timeout-seconds", "53", "task"})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.MaxIterations != 51 || cfg.MaxGoalIterations != 42 || cfg.Subagents.MaxDepth != 43 || cfg.Subagents.MaxParallel != 52 || cfg.Subagents.DefaultTimeoutSec != 45 || cfg.Subagents.MaxAggregateChars != 46 || cfg.Subagents.MaxToolIterations != 47 || cfg.ToolMaxParallel != 48 || cfg.ToolTimeoutSec != 53 {
		t.Fatalf("explicit values did not win over YOLO preset: %+v", cfg)
	}
}

func TestLoadProviderPrecedenceIsEnvironmentThenFileThenBuiltIn(t *testing.T) {
	path := isolateLoad(t)
	if err := os.WriteFile(path, []byte(strings.Join([]string{
		"ENDPOINT = https://file.example/v1/chat/completions",
		"MODEL = file-model",
		"TOKEN = file-token",
		"REASONING_EFFORT = low",
		"",
	}, "\n")), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg, err := Load([]string{"task"})
	if err != nil {
		t.Fatalf("Load from file: %v", err)
	}
	if cfg.Endpoint != "https://file.example/v1/chat/completions" || cfg.Model != "file-model" || cfg.Token != "file-token" || cfg.Reasoning != "low" {
		t.Fatalf("file provider settings were not applied: %+v", cfg)
	}

	t.Setenv("ENDPOINT", "https://env.example/v1/chat/completions")
	t.Setenv("MODEL", "env-model")
	t.Setenv("TOKEN", "env-token")
	t.Setenv("REASONING_EFFORT", "high")
	cfg, err = Load([]string{"task"})
	if err != nil {
		t.Fatalf("Load from environment: %v", err)
	}
	if cfg.Endpoint != "https://env.example/v1/chat/completions" || cfg.Model != "env-model" || cfg.Token != "env-token" || cfg.Reasoning != "high" {
		t.Fatalf("environment provider settings did not override file: %+v", cfg)
	}
}

func TestLoadYoloUsesOrdinaryGeneratedValuesAsModeFallbacks(t *testing.T) {
	path := isolateLoad(t)
	ordinary := strings.Join([]string{
		"ENDPOINT = " + defaultEndpoint,
		"MODEL = " + defaultModel,
		"TOKEN = " + defaultToken,
		"REASONING_EFFORT = " + defaultReasoning,
		"MAX_ITERATIONS = 40",
		"MAX_GOAL_ITERATIONS = 20",
		"SUBAGENT_MAX_DEPTH = 1",
		"SUBAGENT_MAX_PARALLEL = 4",
		"SUBAGENT_TIMEOUT_SECONDS = 600",
		"SUBAGENT_MAX_AGGREGATE_CHARS = 12000",
		"SUBAGENT_MAX_ITERATIONS = 20",
		"TOOL_MAX_PARALLEL = 8",
		"TOOL_TIMEOUT_SECONDS = 60",
		"",
	}, "\n")
	if err := os.WriteFile(path, []byte(ordinary), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg, err := Load([]string{"--yolo", "task"})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.MaxIterations != yoloMaxIterations || cfg.MaxGoalIterations != yoloMaxGoalIterations || cfg.ToolMaxParallel != yoloToolMaxParallel || cfg.ToolTimeoutSec != yoloToolTimeoutSec {
		t.Fatalf("ordinary saved values did not activate YOLO root/tool fallbacks: %+v", cfg)
	}
	if cfg.Subagents.MaxDepth != yoloSubagentMaxDepth || cfg.Subagents.MaxParallel != yoloSubagentMaxParallel || cfg.Subagents.DefaultTimeoutSec != yoloSubagentTimeoutSec || cfg.Subagents.MaxAggregateChars != yoloSubagentAggregateChars || cfg.Subagents.MaxToolIterations != yoloSubagentToolIterations {
		t.Fatalf("ordinary saved values did not activate YOLO subagent fallbacks: %+v", cfg.Subagents)
	}

	saved, err := readConfigFile(path)
	if err != nil {
		t.Fatalf("read config after YOLO load: %v", err)
	}
	if saved["MAX_ITERATIONS"] != "40" || saved["MAX_GOAL_ITERATIONS"] != "20" || saved["SUBAGENT_MAX_ITERATIONS"] != "20" || saved["TOOL_MAX_PARALLEL"] != "8" || saved["TOOL_TIMEOUT_SECONDS"] != "60" {
		t.Fatalf("YOLO load rewrote generated ordinary settings: %#v", saved)
	}
}

func TestLoadRejectsInvalidExplicitNumericValues(t *testing.T) {
	for _, tc := range []struct {
		name string
		key  string
	}{
		{name: "environment", key: "MAX_ITERATIONS"},
		{name: "saved config", key: "SUBAGENT_MAX_PARALLEL"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := isolateLoad(t)
			if tc.name == "environment" {
				t.Setenv(tc.key, "0")
			} else if err := os.WriteFile(path, []byte("ENDPOINT = "+defaultEndpoint+"\n"+tc.key+" = 0\n"), 0o644); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}
			if _, err := Load([]string{"--yolo", "task"}); err == nil {
				t.Fatalf("Load accepted invalid %s value", tc.name)
			}
		})
	}
}
