package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"capelin-go/internal/policy"
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
		"SUBAGENT_MODEL", "SUBAGENT_REASONING_EFFORT", "IDLE_HOOK_COMMAND", "IDLE_HOOK_ARGS",
	}, numericConfigKeys...) {
		t.Setenv(key, "")
	}
	return path
}

func writeConfig(t *testing.T, path string, values map[string]string) {
	t.Helper()
	lines := make([]string, 0, len(values))
	for _, key := range []string{"ENDPOINT", "MODEL", "TOKEN", "REASONING_EFFORT", "IDLE_HOOK_COMMAND", "IDLE_HOOK_ARGS"} {
		if value, ok := values[key]; ok {
			lines = append(lines, key+" = "+value)
		}
	}
	for _, key := range numericConfigKeys {
		if value, ok := values[key]; ok {
			lines = append(lines, key+" = "+value)
		}
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
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
	assertOrdinaryDefaults(t, cfg.OrdinaryProfile())

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
		"SUBAGENT_MAX_DEPTH = 1",
		"SUBAGENT_MAX_PARALLEL = 4",
		"SUBAGENT_MAX_ITERATIONS = 20",
		"TOOL_MAX_PARALLEL = 8",
		"TOOL_TIMEOUT_SECONDS = 60",
		"IDLE_HOOK_COMMAND =",
		"IDLE_HOOK_ARGS = []",
	} {
		if !strings.Contains(contents, want) {
			t.Fatalf("generated config missing %q:\n%s", want, contents)
		}
	}
}

func TestLoadExistingConfigRetainsValuesAndAddsOnlyMissingSupportedKeys(t *testing.T) {
	path := isolateLoad(t)
	writeConfig(t, path, map[string]string{
		"ENDPOINT":         "https://custom.example/v1/chat/completions",
		"MODEL":            "custom-model",
		"TOKEN":            "custom-token",
		"REASONING_EFFORT": "low",
		"MAX_ITERATIONS":   "77",
	})

	if _, err := Load([]string{"task"}); err != nil {
		t.Fatalf("Load: %v", err)
	}
	saved, err := readConfigFile(path)
	if err != nil {
		t.Fatalf("read migrated config: %v", err)
	}
	for key, want := range map[string]string{
		"ENDPOINT":         "https://custom.example/v1/chat/completions",
		"MODEL":            "custom-model",
		"TOKEN":            "custom-token",
		"REASONING_EFFORT": "low",
		"MAX_ITERATIONS":   "77",
	} {
		if saved[key] != want {
			t.Fatalf("existing %s was changed: got %q want %q", key, saved[key], want)
		}
	}
	for _, key := range numericConfigKeys {
		if _, ok := saved[key]; !ok {
			t.Fatalf("migration did not append missing supported key %s", key)
		}
	}
}

func TestLoadExistingConfigMissingEndpointAddsDefaultAndPreservesValues(t *testing.T) {
	path := isolateLoad(t)
	writeConfig(t, path, map[string]string{
		"MODEL":            "custom-model",
		"TOKEN":            "custom-token",
		"REASONING_EFFORT": "low",
		"MAX_ITERATIONS":   "77",
	})

	cfg, err := Load([]string{"task"})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Endpoint != defaultEndpoint {
		t.Fatalf("missing ENDPOINT did not receive default: got %q want %q", cfg.Endpoint, defaultEndpoint)
	}
	if cfg.Model != "custom-model" || cfg.Token != "custom-token" || cfg.Reasoning != "low" || cfg.MaxIterations != 77 {
		t.Fatalf("existing values were not preserved: model=%q token=%q reasoning=%q maxIterations=%d", cfg.Model, cfg.Token, cfg.Reasoning, cfg.MaxIterations)
	}

	saved, err := readConfigFile(path)
	if err != nil {
		t.Fatalf("read migrated config: %v", err)
	}
	if saved["ENDPOINT"] != defaultEndpoint {
		t.Fatalf("migration did not add ENDPOINT: got %q want %q", saved["ENDPOINT"], defaultEndpoint)
	}
	for key, want := range map[string]string{
		"MODEL":            "custom-model",
		"TOKEN":            "custom-token",
		"REASONING_EFFORT": "low",
		"MAX_ITERATIONS":   "77",
	} {
		if saved[key] != want {
			t.Fatalf("migration changed existing %s: got %q want %q", key, saved[key], want)
		}
	}
}

func TestLoadYoloKeepsOrdinaryProfileAndOnlyChangesPermissions(t *testing.T) {
	path := isolateLoad(t)
	ordinary, err := Load([]string{"task"})
	if err != nil {
		t.Fatalf("ordinary Load: %v", err)
	}
	savedBefore, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read generated config: %v", err)
	}

	yolo, err := Load([]string{"--yolo", "task"})
	if err != nil {
		t.Fatalf("YOLO Load: %v", err)
	}
	if !yolo.Yolo {
		t.Fatal("YOLO flag did not remain enabled")
	}
	if got, want := yolo.OrdinaryProfile(), ordinary.OrdinaryProfile(); got != want {
		t.Fatalf("YOLO changed ordinary profile: got=%+v want=%+v", got, want)
	}
	if len(yolo.AllowedTools) <= len(ordinary.AllowedTools) {
		t.Fatalf("YOLO did not expand permissions: ordinary=%d yolo=%d", len(ordinary.AllowedTools), len(yolo.AllowedTools))
	}
	savedAfter, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config after YOLO: %v", err)
	}
	if string(savedAfter) != string(savedBefore) {
		t.Fatal("YOLO changed persisted ordinary configuration")
	}
}

func TestGoalProfileUsesGoalDefaultsWithoutChangingOrdinaryConfig(t *testing.T) {
	path := isolateLoad(t)
	cfg, err := Load([]string{"--yolo", "task"})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	savedBefore, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read generated config: %v", err)
	}
	ordinaryBefore := cfg.OrdinaryProfile()
	goal := cfg.GoalProfile()

	assertOrdinaryDefaults(t, ordinaryBefore)
	if goal.MaxIterations != 256 || goal.MaxGoalIterations != 64 {
		t.Fatalf("unexpected goal iterations: root=%d outer=%d", goal.MaxIterations, goal.MaxGoalIterations)
	}
	if got := goal.Subagents; got.MaxDepth != 2 || got.MaxChildren != defaultSubagentMaxChildren || got.MaxParallel != 8 || got.DefaultTimeoutSec != 600 || got.MaxToolIterations != 100 || got.MaxResultChars != defaultSubagentResultChars || got.MaxAggregateCount != defaultSubagentAggregateCount || got.MaxAggregateChars != 48000 || got.MaxTimeoutSec != defaultSubagentMaxTimeoutSec {
		t.Fatalf("unexpected goal subagent profile: %+v", got)
	}
	if goal.ToolMaxParallel != 16 || goal.ToolTimeoutSec != 300 || !goal.ToolRetryOnTimeout {
		t.Fatalf("unexpected goal tool profile: %+v", goal)
	}
	if got := cfg.OrdinaryProfile(); got != ordinaryBefore {
		t.Fatalf("goal resolution mutated ordinary profile: before=%+v after=%+v", ordinaryBefore, got)
	}
	savedAfter, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config after goal resolution: %v", err)
	}
	if string(savedAfter) != string(savedBefore) {
		t.Fatal("goal resolution changed persisted configuration")
	}
	if strings.Contains(string(savedAfter), "GOAL_PROFILE") {
		t.Fatal("goal resolution persisted a goal-specific key")
	}
}

func TestGoalProfilePrecedenceAcrossSources(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		env      map[string]string
		saved    map[string]string
		wantOrd  RuntimeProfile
		wantGoal RuntimeProfile
	}{
		{
			name:     "CLI overrides environment and saved values",
			args:     []string{"--max-iterations", "31", "--max-goal-iterations", "32", "--subagent-max-depth", "33", "--subagent-max-parallel", "34", "--subagent-max-iterations", "35", "--subagent-max-aggregate-chars", "36", "--tool-max-parallel", "37", "--tool-timeout-seconds", "38", "task"},
			env:      map[string]string{"MAX_ITERATIONS": "41", "MAX_GOAL_ITERATIONS": "42", "SUBAGENT_MAX_DEPTH": "43", "SUBAGENT_MAX_PARALLEL": "44", "SUBAGENT_MAX_ITERATIONS": "45", "SUBAGENT_MAX_AGGREGATE_CHARS": "46", "TOOL_MAX_PARALLEL": "47", "TOOL_TIMEOUT_SECONDS": "48"},
			saved:    map[string]string{"MAX_ITERATIONS": "51", "MAX_GOAL_ITERATIONS": "52", "SUBAGENT_MAX_DEPTH": "53", "SUBAGENT_MAX_PARALLEL": "54", "SUBAGENT_MAX_ITERATIONS": "55", "SUBAGENT_MAX_AGGREGATE_CHARS": "56", "TOOL_MAX_PARALLEL": "57", "TOOL_TIMEOUT_SECONDS": "58"},
			wantOrd:  RuntimeProfile{MaxIterations: 31, MaxGoalIterations: 32, Subagents: SubagentConfig{MaxDepth: 33, MaxChildren: 8, MaxParallel: 34, DefaultTimeoutSec: 600, MaxTimeoutSec: 1800, MaxToolIterations: 35, MaxResultChars: 8000, MaxAggregateCount: 12, MaxAggregateChars: 36}, ToolMaxParallel: 37, ToolTimeoutSec: 38, ToolRetryOnTimeout: true},
			wantGoal: RuntimeProfile{MaxIterations: 31, MaxGoalIterations: 32, Subagents: SubagentConfig{MaxDepth: 33, MaxChildren: 8, MaxParallel: 34, DefaultTimeoutSec: 600, MaxTimeoutSec: 1800, MaxToolIterations: 35, MaxResultChars: 8000, MaxAggregateCount: 12, MaxAggregateChars: 36}, ToolMaxParallel: 37, ToolTimeoutSec: 38, ToolRetryOnTimeout: true},
		},
		{
			name:     "environment overrides saved values for both profiles",
			env:      map[string]string{"MAX_ITERATIONS": "41", "MAX_GOAL_ITERATIONS": "42", "SUBAGENT_MAX_DEPTH": "43", "SUBAGENT_MAX_PARALLEL": "44", "SUBAGENT_MAX_ITERATIONS": "45", "SUBAGENT_MAX_AGGREGATE_CHARS": "46", "TOOL_MAX_PARALLEL": "47", "TOOL_TIMEOUT_SECONDS": "48"},
			saved:    map[string]string{"MAX_ITERATIONS": "51", "MAX_GOAL_ITERATIONS": "52", "SUBAGENT_MAX_DEPTH": "53", "SUBAGENT_MAX_PARALLEL": "54", "SUBAGENT_MAX_ITERATIONS": "55", "SUBAGENT_MAX_AGGREGATE_CHARS": "56", "TOOL_MAX_PARALLEL": "57", "TOOL_TIMEOUT_SECONDS": "58"},
			wantOrd:  RuntimeProfile{MaxIterations: 41, MaxGoalIterations: 42, Subagents: SubagentConfig{MaxDepth: 43, MaxChildren: 8, MaxParallel: 44, DefaultTimeoutSec: 600, MaxTimeoutSec: 1800, MaxToolIterations: 45, MaxResultChars: 8000, MaxAggregateCount: 12, MaxAggregateChars: 46}, ToolMaxParallel: 47, ToolTimeoutSec: 48, ToolRetryOnTimeout: true},
			wantGoal: RuntimeProfile{MaxIterations: 41, MaxGoalIterations: 42, Subagents: SubagentConfig{MaxDepth: 43, MaxChildren: 8, MaxParallel: 44, DefaultTimeoutSec: 600, MaxTimeoutSec: 1800, MaxToolIterations: 45, MaxResultChars: 8000, MaxAggregateCount: 12, MaxAggregateChars: 46}, ToolMaxParallel: 47, ToolTimeoutSec: 48, ToolRetryOnTimeout: true},
		},
		{
			name:     "custom saved values override goal fallbacks",
			saved:    map[string]string{"MAX_ITERATIONS": "51", "MAX_GOAL_ITERATIONS": "52", "SUBAGENT_MAX_DEPTH": "53", "SUBAGENT_MAX_PARALLEL": "54", "SUBAGENT_MAX_ITERATIONS": "55", "SUBAGENT_MAX_AGGREGATE_CHARS": "56", "TOOL_MAX_PARALLEL": "57", "TOOL_TIMEOUT_SECONDS": "58"},
			wantOrd:  RuntimeProfile{MaxIterations: 51, MaxGoalIterations: 52, Subagents: SubagentConfig{MaxDepth: 53, MaxChildren: 8, MaxParallel: 54, DefaultTimeoutSec: 600, MaxTimeoutSec: 1800, MaxToolIterations: 55, MaxResultChars: 8000, MaxAggregateCount: 12, MaxAggregateChars: 56}, ToolMaxParallel: 57, ToolTimeoutSec: 58, ToolRetryOnTimeout: true},
			wantGoal: RuntimeProfile{MaxIterations: 51, MaxGoalIterations: 52, Subagents: SubagentConfig{MaxDepth: 53, MaxChildren: 8, MaxParallel: 54, DefaultTimeoutSec: 600, MaxTimeoutSec: 1800, MaxToolIterations: 55, MaxResultChars: 8000, MaxAggregateCount: 12, MaxAggregateChars: 56}, ToolMaxParallel: 57, ToolTimeoutSec: 58, ToolRetryOnTimeout: true},
		},
		{
			name:     "saved ordinary defaults use goal fallbacks",
			saved:    map[string]string{"MAX_ITERATIONS": "40", "MAX_GOAL_ITERATIONS": "20", "SUBAGENT_MAX_DEPTH": "1", "SUBAGENT_MAX_PARALLEL": "4", "SUBAGENT_MAX_ITERATIONS": "20", "SUBAGENT_MAX_AGGREGATE_CHARS": "12000", "TOOL_MAX_PARALLEL": "8", "TOOL_TIMEOUT_SECONDS": "60"},
			wantOrd:  RuntimeProfile{MaxIterations: 40, MaxGoalIterations: 20, Subagents: SubagentConfig{MaxDepth: 1, MaxChildren: 8, MaxParallel: 4, DefaultTimeoutSec: 600, MaxTimeoutSec: 1800, MaxToolIterations: 20, MaxResultChars: 8000, MaxAggregateCount: 12, MaxAggregateChars: 12000}, ToolMaxParallel: 8, ToolTimeoutSec: 60, ToolRetryOnTimeout: true},
			wantGoal: RuntimeProfile{MaxIterations: 256, MaxGoalIterations: 64, Subagents: SubagentConfig{MaxDepth: 2, MaxChildren: 8, MaxParallel: 8, DefaultTimeoutSec: 600, MaxTimeoutSec: 1800, MaxToolIterations: 100, MaxResultChars: 8000, MaxAggregateCount: 12, MaxAggregateChars: 48000}, ToolMaxParallel: 16, ToolTimeoutSec: 300, ToolRetryOnTimeout: true},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := isolateLoad(t)
			for key, value := range tc.env {
				t.Setenv(key, value)
			}
			values := map[string]string{"ENDPOINT": defaultEndpoint}
			for key, value := range tc.saved {
				values[key] = value
			}
			if len(tc.saved) > 0 {
				writeConfig(t, path, values)
			}
			cfg, err := Load(tc.args)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			tc.wantOrd.Subagents.Model = defaultModel
			tc.wantOrd.Subagents.ReasoningEffort = defaultReasoning
			tc.wantGoal.Subagents.Model = defaultModel
			tc.wantGoal.Subagents.ReasoningEffort = defaultReasoning
			if got := cfg.OrdinaryProfile(); got != tc.wantOrd {
				t.Fatalf("ordinary profile: got=%+v want=%+v", got, tc.wantOrd)
			}
			if got := cfg.GoalProfile(); got != tc.wantGoal {
				t.Fatalf("goal profile: got=%+v want=%+v", got, tc.wantGoal)
			}
		})
	}
}

func TestExplicitDefaultCLIAndEnvironmentValuesRemainExplicitForGoals(t *testing.T) {
	t.Run("CLI", func(t *testing.T) {
		isolateLoad(t)
		cfg, err := Load([]string{"--max-iterations", "40", "--tool-timeout-seconds", "60", "task"})
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		goal := cfg.GoalProfile()
		if goal.MaxIterations != 40 || goal.ToolTimeoutSec != 60 {
			t.Fatalf("explicit CLI defaults received goal fallbacks: %+v", goal)
		}
	})
	t.Run("environment", func(t *testing.T) {
		isolateLoad(t)
		t.Setenv("MAX_ITERATIONS", "40")
		t.Setenv("TOOL_TIMEOUT_SECONDS", "60")
		cfg, err := Load([]string{"task"})
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		goal := cfg.GoalProfile()
		if goal.MaxIterations != 40 || goal.ToolTimeoutSec != 60 {
			t.Fatalf("explicit environment defaults received goal fallbacks: %+v", goal)
		}
	})
}

func TestLoadRejectsInvalidNonPositiveNumericValuesFromEverySource(t *testing.T) {
	flags := map[string]string{
		"MAX_ITERATIONS":               "--max-iterations",
		"MAX_GOAL_ITERATIONS":          "--max-goal-iterations",
		"SUBAGENT_MAX_DEPTH":           "--subagent-max-depth",
		"SUBAGENT_MAX_CHILDREN":        "--subagent-max-children",
		"SUBAGENT_MAX_PARALLEL":        "--subagent-max-parallel",
		"SUBAGENT_TIMEOUT_SECONDS":     "--subagent-timeout-seconds",
		"SUBAGENT_MAX_RESULT_CHARS":    "--subagent-max-result-chars",
		"SUBAGENT_MAX_AGGREGATE_CHARS": "--subagent-max-aggregate-chars",
		"SUBAGENT_MAX_ITERATIONS":      "--subagent-max-iterations",
		"TOOL_MAX_PARALLEL":            "--tool-max-parallel",
		"TOOL_TIMEOUT_SECONDS":         "--tool-timeout-seconds",
	}
	for key, flag := range flags {
		t.Run("CLI/"+key, func(t *testing.T) {
			isolateLoad(t)
			if _, err := Load([]string{flag, "0", "task"}); err == nil {
				t.Fatalf("Load accepted invalid CLI value for %s", key)
			}
		})
		t.Run("environment/"+key, func(t *testing.T) {
			isolateLoad(t)
			t.Setenv(key, "0")
			if _, err := Load([]string{"task"}); err == nil {
				t.Fatalf("Load accepted invalid environment value for %s", key)
			}
		})
		t.Run("saved/"+key, func(t *testing.T) {
			path := isolateLoad(t)
			writeConfig(t, path, map[string]string{"ENDPOINT": defaultEndpoint, key: "0"})
			if _, err := Load([]string{"task"}); err == nil {
				t.Fatalf("Load accepted invalid saved value for %s", key)
			}
		})
	}
}

func assertOrdinaryDefaults(t *testing.T, profile RuntimeProfile) {
	t.Helper()
	if profile.MaxIterations != 40 || profile.MaxGoalIterations != 20 || profile.ToolMaxParallel != 8 || profile.ToolTimeoutSec != 60 || !profile.ToolRetryOnTimeout {
		t.Fatalf("unexpected ordinary profile: %+v", profile)
	}
	if got := profile.Subagents; got.MaxDepth != 1 || got.MaxChildren != 8 || got.MaxParallel != 4 || got.DefaultTimeoutSec != 600 || got.MaxTimeoutSec != 1800 || got.MaxToolIterations != 20 || got.MaxResultChars != 8000 || got.MaxAggregateCount != 12 || got.MaxAggregateChars != 12000 {
		t.Fatalf("unexpected ordinary subagent profile: %+v", got)
	}
}

func TestIdleHookConfigurationPrecedenceAndValidation(t *testing.T) {
	t.Run("environment overrides saved values", func(t *testing.T) {
		path := isolateLoad(t)
		writeConfig(t, path, map[string]string{
			"IDLE_HOOK_COMMAND": "saved-hook",
			"IDLE_HOOK_ARGS":    `["saved"]`,
		})
		t.Setenv("IDLE_HOOK_COMMAND", "env-hook")
		t.Setenv("IDLE_HOOK_ARGS", `["env", "argument"]`)
		cfg, err := Load([]string{"--allow-tool", policy.ExecuteProgram, "task"})
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.IdleHookCommand != "env-hook" || !reflect.DeepEqual(cfg.IdleHookArgs, []string{"env", "argument"}) {
			t.Fatalf("environment hook values did not override saved values: command=%q args=%#v", cfg.IdleHookCommand, cfg.IdleHookArgs)
		}
	})

	t.Run("malformed arguments are rejected for configured command", func(t *testing.T) {
		path := isolateLoad(t)
		writeConfig(t, path, map[string]string{
			"IDLE_HOOK_COMMAND": "hook",
			"IDLE_HOOK_ARGS":    `{"not":"an array"}`,
		})
		_, err := Load([]string{"--allow-tool", policy.ExecuteProgram, "task"})
		if err == nil || !strings.Contains(err.Error(), "IDLE_HOOK_ARGS") {
			t.Fatalf("malformed hook arguments were not rejected clearly: %v", err)
		}
	})

	t.Run("null array elements are rejected", func(t *testing.T) {
		path := isolateLoad(t)
		writeConfig(t, path, map[string]string{
			"IDLE_HOOK_COMMAND": "hook",
			"IDLE_HOOK_ARGS":    `["valid", null]`,
		})
		_, err := Load([]string{"--allow-tool", policy.ExecuteProgram, "task"})
		if err == nil || !strings.Contains(err.Error(), "IDLE_HOOK_ARGS") {
			t.Fatalf("null hook argument was not rejected clearly: %v", err)
		}
	})

	t.Run("blank command disables hook", func(t *testing.T) {
		path := isolateLoad(t)
		writeConfig(t, path, map[string]string{
			"IDLE_HOOK_COMMAND": "   ",
			"IDLE_HOOK_ARGS":    `{"not":"an array"}`,
		})
		cfg, err := Load([]string{"task"})
		if err != nil {
			t.Fatalf("blank hook command should disable hook: %v", err)
		}
		if cfg.IdleHookCommand != "" || len(cfg.IdleHookArgs) != 0 {
			t.Fatalf("blank hook was not disabled: command=%q args=%#v", cfg.IdleHookCommand, cfg.IdleHookArgs)
		}
	})

	t.Run("permission is required in local mode", func(t *testing.T) {
		path := isolateLoad(t)
		writeConfig(t, path, map[string]string{"IDLE_HOOK_COMMAND": "hook"})
		_, err := Load([]string{"task"})
		if err == nil || !strings.Contains(err.Error(), "execute_program") || !strings.Contains(err.Error(), "--allow-tool") {
			t.Fatalf("missing actionable hook permission error: %v", err)
		}
	})
}

func TestIdleHookSettingsAreMigratedWithoutChangingExistingValues(t *testing.T) {
	path := isolateLoad(t)
	writeConfig(t, path, map[string]string{"MODEL": "preserved", "IDLE_HOOK_COMMAND": "saved-hook", "IDLE_HOOK_ARGS": `["one"]`})
	if _, err := Load([]string{"--allow-tool", policy.ExecuteProgram, "task"}); err != nil {
		t.Fatalf("Load: %v", err)
	}
	saved, err := readConfigFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if saved["MODEL"] != "preserved" || saved["IDLE_HOOK_COMMAND"] != "saved-hook" || saved["IDLE_HOOK_ARGS"] != `["one"]` {
		t.Fatalf("migration changed existing values: %#v", saved)
	}
	if _, ok := saved["IDLE_HOOK_COMMAND"]; !ok {
		t.Fatal("migration omitted IDLE_HOOK_COMMAND")
	}
	if _, ok := saved["IDLE_HOOK_ARGS"]; !ok {
		t.Fatal("migration omitted IDLE_HOOK_ARGS")
	}
}

func TestServerModeIgnoresLocalIdleHookConfiguration(t *testing.T) {
	path := isolateLoad(t)
	writeConfig(t, path, map[string]string{
		"ENDPOINT":          defaultEndpoint,
		"IDLE_HOOK_COMMAND": "server-must-ignore",
		"IDLE_HOOK_ARGS":    `{"not":"an array"}`,
	})
	t.Setenv("IDLE_HOOK_COMMAND", "environment-hook")
	t.Setenv("IDLE_HOOK_ARGS", `not-json`)

	cfg, err := Load([]string{"--server-port", "8899"})
	if err != nil {
		t.Fatalf("server Load rejected local-only hook settings: %v", err)
	}
	if cfg.IdleHookCommand != "" || len(cfg.IdleHookArgs) != 0 {
		t.Fatalf("server config retained local hook: command=%q args=%#v", cfg.IdleHookCommand, cfg.IdleHookArgs)
	}
	if cfg.ServerPort != 8899 || !cfg.ServerSecurityEnabled {
		t.Fatalf("server mode contract changed: port=%d security=%v", cfg.ServerPort, cfg.ServerSecurityEnabled)
	}
}
