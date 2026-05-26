package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// isolateConfigFile points CAPELIN_CONFIG_FILE to a fresh temp path so tests
// are not affected by the developer's real ~/.local/capelin-go/config.ini.
func isolateConfigFile(t *testing.T) {
	t.Helper()
	t.Setenv("CAPELIN_CONFIG_FILE", filepath.Join(t.TempDir(), "config.ini"))
}

func TestLoadConfigDefaults(t *testing.T) {
	isolateConfigFile(t)
	t.Setenv("BASE_URL", "")
	t.Setenv("MODEL", "")
	t.Setenv("TOKEN", "")
	t.Setenv("REASONING_EFFORT", "")
	t.Setenv("SYSTEM_PROMPT", "")
	t.Setenv("systemPrompt", "")

	cfg, err := loadConfig([]string{"hello"})
	if err != nil {
		t.Fatalf("loadConfig returned error: %v", err)
	}
	if cfg.baseURL != defaultBaseURL {
		t.Fatalf("unexpected baseURL: %q", cfg.baseURL)
	}
	if cfg.model != defaultModel {
		t.Fatalf("unexpected model: %q", cfg.model)
	}
	if !cfg.allowedTools[toolListFiles] || !cfg.allowedTools[toolReadFile] {
		t.Fatal("expected safe default tools to be enabled")
	}
	if cfg.allowedTools[toolWriteFile] || cfg.allowedTools[toolExecuteProgram] {
		t.Fatal("expected mutating/exec tools disabled by default")
	}
}

func TestLoadConfigReasoningPassThrough(t *testing.T) {
	isolateConfigFile(t)
	t.Setenv("BASE_URL", "http://localhost:8235/v1")
	t.Setenv("REASONING_EFFORT", "trace-heavy-v2")
	cfg, err := loadConfig([]string{"task"})
	if err != nil {
		t.Fatalf("loadConfig returned error: %v", err)
	}
	if cfg.reasoning != "trace-heavy-v2" {
		t.Fatalf("unexpected reasoning: %q", cfg.reasoning)
	}
}

func TestLoadConfigAllowTool(t *testing.T) {
	isolateConfigFile(t)
	t.Setenv("BASE_URL", "http://localhost:8235/v1")
	cfg, err := loadConfig([]string{"--allow-tool", toolWriteFile, "--allow-tool=execute_program", "--allow-tool=execute_skill", "task"})
	if err != nil {
		t.Fatalf("loadConfig error: %v", err)
	}
	if !cfg.allowedTools[toolWriteFile] || !cfg.allowedTools[toolExecuteProgram] || !cfg.allowedTools[toolExecuteSkill] {
		t.Fatal("expected allow-tool to enable requested tools")
	}
}

func TestLoadConfigRejectUnknownAllowTool(t *testing.T) {
	isolateConfigFile(t)
	t.Setenv("BASE_URL", "http://localhost:8235/v1")
	_, err := loadConfig([]string{"--allow-tool", "rm_rf", "task"})
	if err == nil {
		t.Fatal("expected error for unknown tool")
	}
}

func TestLoadConfigRejectInteractiveFlag(t *testing.T) {
	isolateConfigFile(t)
	t.Setenv("BASE_URL", "http://localhost:8235/v1")
	_, err := loadConfig([]string{"-i", "task"})
	if err == nil {
		t.Fatal("expected unknown flag error for interactive mode")
	}
}

func TestResolveWorkspacePathRejectTraversal(t *testing.T) {
	root := t.TempDir()
	_, err := resolveWorkspacePath(root, "../outside.txt")
	if err == nil {
		t.Fatal("expected traversal rejection")
	}
}

func TestRunWriteFileAndReadFile(t *testing.T) {
	root := t.TempDir()
	_, err := runWriteFile(root, false, writeFileArgs{Path: "a/b.txt", Content: "line1\nline2"})
	if err != nil {
		t.Fatalf("runWriteFile: %v", err)
	}
	out, err := runReadFile(root, false, readFileArgs{Path: "a/b.txt", StartLine: 2, EndLine: 2})
	if err != nil {
		t.Fatalf("runReadFile: %v", err)
	}
	if !strings.Contains(out, "2. line2") {
		t.Fatalf("unexpected read output: %q", out)
	}
}

func TestRunListFiles(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "x.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	out, err := runListFiles(root, false, listFilesArgs{Path: "."})
	if err != nil {
		t.Fatalf("runListFiles: %v", err)
	}
	if !strings.Contains(out, "x.txt") || !strings.Contains(out, "dir/") {
		t.Fatalf("unexpected list output: %q", out)
	}
}

func TestContainsDangerousPattern(t *testing.T) {
	if !containsDangerousPattern("bash", []string{"-lc", "echo hi"}) {
		t.Fatal("expected bash command to be blocked")
	}
	if containsDangerousPattern("go", []string{"test", "./...;rm"}) {
		t.Fatal("expected shell-like punctuation in args to be treated as literal")
	}
	if !containsDangerousPattern("go bad", []string{"test"}) {
		t.Fatal("expected command token with whitespace to be blocked")
	}
	if !containsDangerousPattern("go;rm", []string{"test"}) {
		t.Fatal("expected shell metacharacters in command token to be blocked")
	}
	if containsDangerousPattern("go", []string{"test", "./..."}) {
		t.Fatal("expected safe args")
	}
}

func TestRunExecuteProgram(t *testing.T) {
	root := t.TempDir()
	out, err := runExecuteProgram(context.Background(), root, false, executeProgramArgs{
		Command: "echo",
		Args:    []string{"ok"},
	})
	if err != nil {
		t.Fatalf("runExecuteProgram: %v", err)
	}
	if !strings.Contains(out, "\"exit_code\": 0") || !strings.Contains(out, "ok") {
		t.Fatalf("unexpected execute output: %s", out)
	}
}

func TestLoadSkillsPrecedence(t *testing.T) {
	base := t.TempDir()
	project := filepath.Join(base, "project")
	userHome := filepath.Join(base, "home")
	projectSkills := filepath.Join(project, ".agents", "skills", "demo")
	userSkills := filepath.Join(userHome, ".agents", "skills", "demo")
	if err := os.MkdirAll(projectSkills, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(userSkills, 0o755); err != nil {
		t.Fatal(err)
	}

	projectSkill := "---\nname: demo\ndescription: project desc\n---\n# Demo\nProject"
	userSkill := "---\nname: demo\ndescription: user desc\n---\n# Demo\nUser"
	if err := os.WriteFile(filepath.Join(projectSkills, "SKILL.md"), []byte(projectSkill), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(userSkills, "SKILL.md"), []byte(userSkill), 0o644); err != nil {
		t.Fatal(err)
	}

	skills, err := loadSkillsFromDirs([]struct {
		path   string
		source string
	}{
		{path: filepath.Join(project, ".agents", "skills"), source: "project"},
		{path: filepath.Join(userHome, ".agents", "skills"), source: "user"},
	})
	if err != nil {
		t.Fatalf("loadSkillsFromDirs: %v", err)
	}
	if skills["demo"].Description != "project desc" {
		t.Fatalf("expected project override, got: %q", skills["demo"].Description)
	}
}

func TestValidateFetchURLRejectsLocal(t *testing.T) {
	_, err := validateFetchURL(context.Background(), "http://localhost")
	if err == nil {
		t.Fatal("expected localhost rejection")
	}
}

func TestRunFetchPageMock(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<html><body><h1>Hello</h1><p>world</p></body></html>`))
	}))
	defer ts.Close()

	origAllow := allowPrivateFetch
	origClient := toolHTTPClient
	t.Cleanup(func() {
		allowPrivateFetch = origAllow
		toolHTTPClient = origClient
	})
	allowPrivateFetch = true
	toolHTTPClient = ts.Client()

	out, err := runFetchPage(context.Background(), ts.URL)
	if err != nil {
		t.Fatalf("runFetchPage: %v", err)
	}
	if !strings.Contains(out, "# Hello") {
		t.Fatalf("unexpected output: %q", out)
	}
}

func TestRunAppendFileCreatesParentDirs(t *testing.T) {
	root := t.TempDir()
	out, err := runAppendFile(root, false, appendFileArgs{Path: "newdir/notes.txt", Content: "appended"})
	if err != nil {
		t.Fatalf("runAppendFile: %v", err)
	}
	if !strings.Contains(out, "appended") {
		t.Fatalf("unexpected output: %q", out)
	}
	data, err := os.ReadFile(filepath.Join(root, "newdir", "notes.txt"))
	if err != nil {
		t.Fatalf("reading appended file: %v", err)
	}
	if string(data) != "appended" {
		t.Fatalf("unexpected file content: %q", string(data))
	}
}

func TestParseSkillFileNameFallback(t *testing.T) {
	dir := t.TempDir()
	skillDir := filepath.Join(dir, "my-skill")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	content := "---\ndescription: a skill without a name\n---\n# Body"
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	sk, err := parseSkillFile(filepath.Join(skillDir, "SKILL.md"), "project")
	if err != nil {
		t.Fatalf("parseSkillFile: %v", err)
	}
	if sk.Name != "my-skill" {
		t.Fatalf("expected dir-name fallback, got: %q", sk.Name)
	}
}

func TestDisabledToolReturnsError(t *testing.T) {
	isolateConfigFile(t)
	t.Setenv("BASE_URL", "http://localhost:8235/v1")
	cfg, err := loadConfig([]string{"task"})
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	a := &app{cfg: cfg}

	_, err = a.runTool(context.Background(), apiToolCall{
		ID:   "1",
		Type: "function",
		Function: apiFunctionCall{
			Name:      toolWriteFile,
			Arguments: `{"path":"x.txt","content":"hi"}`,
		},
	})
	if err == nil {
		t.Fatal("expected disabled-tool error for write_file")
	}
	if !strings.Contains(err.Error(), "--allow-tool") {
		t.Fatalf("expected hint in error message, got: %v", err)
	}

	_, err = a.runTool(context.Background(), apiToolCall{
		ID:   "2",
		Type: "function",
		Function: apiFunctionCall{
			Name:      toolExecuteProgram,
			Arguments: `{"command":"echo"}`,
		},
	})
	if err == nil {
		t.Fatal("expected disabled-tool error for execute_program")
	}

	_, err = a.runTool(context.Background(), apiToolCall{
		ID:   "3",
		Type: "function",
		Function: apiFunctionCall{
			Name:      toolExecuteSkill,
			Arguments: `{"name":"vPass","command":"opencli"}`,
		},
	})
	if err == nil {
		t.Fatal("expected disabled-tool error for execute_skill")
	}

	_, err = a.runTool(context.Background(), apiToolCall{
		ID:   "4",
		Type: "function",
		Function: apiFunctionCall{
			Name:      toolCreateSubagent,
			Arguments: `{"question":"hello"}`,
		},
	})
	if err == nil {
		t.Fatal("expected disabled-tool error for create_subagent")
	}
}

func TestResolveWorkspacePathRejectsAbsolute(t *testing.T) {
	root := t.TempDir()
	_, err := resolveWorkspacePath(root, "/etc/passwd")
	if err == nil {
		t.Fatal("expected rejection of absolute path")
	}
}

func TestContainsDangerousPatternRedirection(t *testing.T) {
	if containsDangerousPattern("cat", []string{"secret.txt", ">", "/tmp/out"}) {
		t.Fatal("expected redirection symbols in args to be treated as literal")
	}
	if containsDangerousPattern("cat", []string{"<", "input.txt"}) {
		t.Fatal("expected redirection symbols in args to be treated as literal")
	}
	if !containsDangerousPattern("cat", []string{"file", string([]byte{'a', 0, 'b'})}) {
		t.Fatal("expected NUL byte in args to be blocked")
	}
}

func TestLoadConfigYolo(t *testing.T) {
	isolateConfigFile(t)
	t.Setenv("BASE_URL", "http://localhost:8235/v1")
	cfg, err := loadConfig([]string{"--yolo", "task"})
	if err != nil {
		t.Fatalf("loadConfig --yolo: %v", err)
	}
	if !cfg.yolo {
		t.Fatal("expected yolo=true")
	}
	for name := range optInTools {
		if !cfg.allowedTools[name] {
			t.Fatalf("expected opt-in tool %q enabled in yolo mode", name)
		}
	}
}

func TestYoloAllowsAbsolutePath(t *testing.T) {
	root := t.TempDir()
	// In yolo mode, an absolute path outside workspace root should be accepted.
	outsideDir := t.TempDir()
	target := filepath.Join(outsideDir, "test.txt")
	if err := os.WriteFile(target, []byte("outside"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := runReadFile(root, true, readFileArgs{Path: target})
	if err != nil {
		t.Fatalf("runReadFile yolo absolute: %v", err)
	}
	if !strings.Contains(out, "outside") {
		t.Fatalf("unexpected output: %q", out)
	}
}

func TestNoYoloRejectsAbsolutePath(t *testing.T) {
	root := t.TempDir()
	_, err := runReadFile(root, false, readFileArgs{Path: "/etc/hostname"})
	if err == nil {
		t.Fatal("expected absolute path to be rejected without yolo")
	}
}

func TestResolveWorkspacePathRejectsSymlinkEscape(t *testing.T) {
	outside := t.TempDir()
	root := t.TempDir()
	linkPath := filepath.Join(root, "link")
	if err := os.Symlink(outside, linkPath); err != nil {
		t.Skipf("symlinks not supported on this platform: %v", err)
	}
	_, err := resolveWorkspacePath(root, "link/secret.txt")
	if err == nil {
		t.Fatal("expected symlink escape to be rejected")
	}
}

func TestExtractExecutableCommands(t *testing.T) {
	content := `---
name: demo
description: test
runs: |
  #!/bin/bash
  opencli thing "$1"
---

## Commands
` + "```bash\nopencli secretshare share \"x\"\necho done\n```\n"
	cmds := extractExecutableCommands(content, "#!/bin/bash\nopencli thing \"$1\"")
	if !slices.Contains(cmds, "opencli") {
		t.Fatalf("expected opencli parsed from skill commands, got: %v", cmds)
	}
	if !slices.Contains(cmds, "echo") {
		t.Fatalf("expected echo parsed from skill commands, got: %v", cmds)
	}
}

func TestRunExecuteSkill(t *testing.T) {
	root := t.TempDir()
	skills := map[string]skill{
		"demo": {
			Name:     "demo",
			Commands: []string{"echo"},
		},
	}
	out, err := runExecuteSkill(context.Background(), root, false, skills, executeSkillArgs{
		Name:    "demo",
		Command: "echo",
		Args:    []string{"ok"},
	})
	if err != nil {
		t.Fatalf("runExecuteSkill: %v", err)
	}
	if !strings.Contains(out, "\"exit_code\": 0") || !strings.Contains(out, "ok") {
		t.Fatalf("unexpected execute_skill output: %s", out)
	}
}

func TestRunExecuteSkillRejectsUndeclaredCommand(t *testing.T) {
	root := t.TempDir()
	skills := map[string]skill{
		"demo": {Name: "demo", Commands: []string{"echo"}},
	}
	_, err := runExecuteSkill(context.Background(), root, false, skills, executeSkillArgs{
		Name:    "demo",
		Command: "opencli",
	})
	if err == nil {
		t.Fatal("expected undeclared command to be rejected")
	}
}

func TestLoadConfigSubagentFlags(t *testing.T) {
	isolateConfigFile(t)
	t.Setenv("BASE_URL", "http://localhost:8235/v1")
	cfg, err := loadConfig([]string{
		"--allow-tool", toolCreateSubagent,
		"--allow-tool", toolRunSubagent,
		"--subagent-max-depth", "2",
		"--subagent-max-children", "5",
		"--subagent-max-parallel", "3",
		"--subagent-timeout-seconds", "45",
		"task",
	})
	if err != nil {
		t.Fatalf("loadConfig returned error: %v", err)
	}
	if !cfg.allowedTools[toolCreateSubagent] || !cfg.allowedTools[toolRunSubagent] {
		t.Fatal("expected subagent tools to be enabled")
	}
	if cfg.subagents.MaxDepth != 2 || cfg.subagents.MaxChildren != 5 || cfg.subagents.MaxParallel != 3 || cfg.subagents.DefaultTimeoutSec != 45 {
		t.Fatalf("unexpected subagent cfg: %+v", cfg.subagents)
	}
}

func TestSubagentLifecycleAndAggregation(t *testing.T) {
	isolateConfigFile(t)
	t.Setenv("BASE_URL", "http://localhost:8235/v1")
	cfg, err := loadConfig([]string{
		"--allow-tool", toolCreateSubagent,
		"--allow-tool", toolRunSubagent,
		"--allow-tool", toolAwaitSubagent,
		"--allow-tool", toolListSubagents,
		"--allow-tool", toolReadSubagent,
		"--allow-tool", toolCancelSubagent,
		"task",
	})
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	a, err := newApp(cfg)
	if err != nil {
		t.Fatalf("newApp: %v", err)
	}
	a.subagents.runner = func(ctx context.Context, runtime *agentRuntime, session *subagentSession) (string, error) {
		return "done: " + session.Question, nil
	}

	createCall := apiToolCall{
		ID:   "1",
		Type: "function",
		Function: apiFunctionCall{
			Name:      toolCreateSubagent,
			Arguments: `{"name":"worker-a","question":"inspect repo","execution_mode":"sequential"}`,
		},
	}
	createOut, err := a.runTool(context.Background(), createCall)
	if err != nil {
		t.Fatalf("create_subagent error: %v", err)
	}
	var created subagentEnvelope
	if err := json.Unmarshal([]byte(createOut), &created); err != nil {
		t.Fatalf("decode create output: %v", err)
	}
	if created.Status != string(subagentStatusPending) {
		t.Fatalf("unexpected initial status: %s", created.Status)
	}

	runCall := apiToolCall{
		ID:   "2",
		Type: "function",
		Function: apiFunctionCall{
			Name:      toolRunSubagent,
			Arguments: `{"id":"` + created.ID + `","wait":true}`,
		},
	}
	runOut, err := a.runTool(context.Background(), runCall)
	if err != nil {
		t.Fatalf("run_subagent error: %v", err)
	}
	var ran subagentEnvelope
	if err := json.Unmarshal([]byte(runOut), &ran); err != nil {
		t.Fatalf("decode run output: %v", err)
	}
	if ran.Status != string(subagentStatusCompleted) {
		t.Fatalf("expected completed status, got %s", ran.Status)
	}
	if !strings.Contains(ran.Output, "inspect repo") {
		t.Fatalf("unexpected output: %q", ran.Output)
	}

	readAggregate := apiToolCall{
		ID:   "3",
		Type: "function",
		Function: apiFunctionCall{
			Name:      toolReadSubagent,
			Arguments: `{"ids":["` + created.ID + `"]}`,
		},
	}
	aggregateOut, err := a.runTool(context.Background(), readAggregate)
	if err != nil {
		t.Fatalf("read_subagent aggregate error: %v", err)
	}
	var agg subagentAggregateEnvelope
	if err := json.Unmarshal([]byte(aggregateOut), &agg); err != nil {
		t.Fatalf("decode aggregate output: %v", err)
	}
	if agg.Kind != "aggregate" || agg.Count != 1 || agg.Completed != 1 {
		t.Fatalf("unexpected aggregate envelope: %+v", agg)
	}
}

func TestSubagentPolicyInheritancePreventsEscalation(t *testing.T) {
	parentAllowed := map[string]bool{
		toolListFiles: true,
		toolReadFile:  true,
	}
	_, err := deriveChildAllowedTools(parentAllowed, []string{toolWriteFile}, 1, 2)
	if err == nil {
		t.Fatal("expected escalation to be rejected")
	}
}

func TestSubagentMaxDepthAndChildren(t *testing.T) {
	cfg := defaultSubagentRuntimeConfig()
	cfg.MaxDepth = 1
	cfg.MaxChildren = 1
	m := newSubagentManager(cfg, func(ctx context.Context, runtime *agentRuntime, session *subagentSession) (string, error) {
		return "ok", nil
	})
	root := &agentRuntime{
		sessionID:         rootAgentID,
		depth:             0,
		allowedTools:      map[string]bool{toolCreateSubagent: true},
		maxToolIterations: 5,
	}
	_, err := m.create(root, createSubagentArgs{Question: "a"})
	if err != nil {
		t.Fatalf("first create failed: %v", err)
	}
	_, err = m.create(root, createSubagentArgs{Question: "b"})
	if err == nil {
		t.Fatal("expected max-children rejection")
	}

	childRuntime := &agentRuntime{
		sessionID:         "subagent-1",
		depth:             1,
		allowedTools:      map[string]bool{toolCreateSubagent: true},
		maxToolIterations: 5,
	}
	_, err = m.create(childRuntime, createSubagentArgs{Question: "nested"})
	if err == nil {
		t.Fatal("expected max-depth rejection")
	}
}

func TestSubagentParallelBoundedWorkerPool(t *testing.T) {
	cfg := defaultSubagentRuntimeConfig()
	cfg.MaxParallel = 2
	cfg.MaxDepth = 1
	m := newSubagentManager(cfg, nil)

	var concurrent atomic.Int64
	var peak atomic.Int64
	m.runner = func(ctx context.Context, runtime *agentRuntime, session *subagentSession) (string, error) {
		now := concurrent.Add(1)
		for {
			currentPeak := peak.Load()
			if now <= currentPeak || peak.CompareAndSwap(currentPeak, now) {
				break
			}
		}
		time.Sleep(80 * time.Millisecond)
		concurrent.Add(-1)
		return "ok", nil
	}

	root := &agentRuntime{
		sessionID:         rootAgentID,
		depth:             0,
		allowedTools:      map[string]bool{toolCreateSubagent: true, toolRunSubagent: true, toolAwaitSubagent: true},
		maxToolIterations: 5,
	}
	ids := make([]string, 0, 3)
	for i := 0; i < 3; i++ {
		session, err := m.create(root, createSubagentArgs{Question: fmt.Sprintf("q-%d", i), ExecutionMode: "parallel"})
		if err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
		ids = append(ids, session.ID)
		if _, err := m.run(context.Background(), root, runSubagentArgs{ID: session.ID, ExecutionMode: "parallel"}); err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
	}
	for _, id := range ids {
		if _, err := m.await(context.Background(), root, awaitSubagentArgs{ID: id, TimeoutSeconds: 5}); err != nil {
			t.Fatalf("await %s: %v", id, err)
		}
	}
	if peak.Load() > 2 {
		t.Fatalf("expected peak parallelism <= 2, got %d", peak.Load())
	}
}

func TestRunSubagentPreservesCreatedExecutionMode(t *testing.T) {
	cfg := defaultSubagentRuntimeConfig()
	cfg.MaxDepth = 1
	m := newSubagentManager(cfg, func(ctx context.Context, runtime *agentRuntime, session *subagentSession) (string, error) {
		return "mode=" + session.ExecutionMode, nil
	})
	root := &agentRuntime{
		sessionID: rootAgentID,
		depth:     0,
		allowedTools: map[string]bool{
			toolCreateSubagent: true,
			toolRunSubagent:    true,
			toolAwaitSubagent:  true,
		},
		maxToolIterations: 5,
	}

	created, err := m.create(root, createSubagentArgs{Question: "q", ExecutionMode: "parallel"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	queued, err := m.run(context.Background(), root, runSubagentArgs{ID: created.ID})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if queued.ExecutionMode != "parallel" {
		t.Fatalf("expected queued execution mode to remain parallel, got %q", queued.ExecutionMode)
	}
	done, err := m.await(context.Background(), root, awaitSubagentArgs{ID: created.ID, TimeoutSeconds: 5})
	if err != nil {
		t.Fatalf("await: %v", err)
	}
	if done.Status != subagentStatusCompleted {
		t.Fatalf("expected completed status, got %q", done.Status)
	}
	if !strings.Contains(done.Output, "mode=parallel") {
		t.Fatalf("unexpected output: %q", done.Output)
	}
}

func TestBuildAgentToolsHonorsRestrictedAlwaysTools(t *testing.T) {
	enabled := map[string]bool{
		toolReadFile: true,
	}
	tools := buildAgentTools(enabled)
	if len(tools) != 1 {
		t.Fatalf("expected exactly one tool, got %d", len(tools))
	}
	if tools[0].Function.Name != toolReadFile {
		t.Fatalf("expected only read_file tool, got %q", tools[0].Function.Name)
	}
}

func TestRunToolForRuntimeRejectsRestrictedAlwaysTool(t *testing.T) {
	isolateConfigFile(t)
	t.Setenv("BASE_URL", "http://localhost:8235/v1")
	cfg, err := loadConfig([]string{"task"})
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	a := &app{cfg: cfg}
	runtime := &agentRuntime{
		sessionID: rootAgentID,
		depth:     0,
		allowedTools: map[string]bool{
			toolReadFile: true,
		},
		maxToolIterations: 5,
	}

	_, err = a.runToolForRuntime(context.Background(), runtime, apiToolCall{
		ID:   "x",
		Type: "function",
		Function: apiFunctionCall{
			Name:      toolWebSearch,
			Arguments: `{"query":"test"}`,
		},
	})
	if err == nil {
		t.Fatal("expected restricted always-enabled tool to be blocked by runtime policy")
	}
	if !strings.Contains(err.Error(), "disabled by current policy") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestLoadConfigMaxIterationsFlag(t *testing.T) {
	isolateConfigFile(t)
	t.Setenv("BASE_URL", "http://localhost:8235/v1")
	t.Setenv("MAX_ITERATIONS", "")

	// Default should be 40.
	cfg, err := loadConfig([]string{"task"})
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.maxIterations != defaultMaxIterations {
		t.Fatalf("expected default maxIterations=%d, got %d", defaultMaxIterations, cfg.maxIterations)
	}

	// Flag overrides default.
	cfg, err = loadConfig([]string{"--max-iterations", "100", "task"})
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.maxIterations != 100 {
		t.Fatalf("expected maxIterations=100, got %d", cfg.maxIterations)
	}

	// = form.
	cfg, err = loadConfig([]string{"--max-iterations=75", "task"})
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.maxIterations != 75 {
		t.Fatalf("expected maxIterations=75, got %d", cfg.maxIterations)
	}

	// Reject non-positive value.
	_, err = loadConfig([]string{"--max-iterations", "0", "task"})
	if err == nil {
		t.Fatal("expected error for --max-iterations=0")
	}
}

func TestLoadConfigMaxIterationsEnv(t *testing.T) {
	isolateConfigFile(t)
	t.Setenv("BASE_URL", "http://localhost:8235/v1")
	t.Setenv("MAX_ITERATIONS", "60")

	cfg, err := loadConfig([]string{"task"})
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.maxIterations != 60 {
		t.Fatalf("expected maxIterations=60 from env, got %d", cfg.maxIterations)
	}

	// CLI flag wins over env.
	cfg, err = loadConfig([]string{"--max-iterations", "25", "task"})
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.maxIterations != 25 {
		t.Fatalf("expected CLI flag (25) to override env (60), got %d", cfg.maxIterations)
	}
}

func TestRootRuntimeUsesMaxIterations(t *testing.T) {
	isolateConfigFile(t)
	t.Setenv("BASE_URL", "http://localhost:8235/v1")
	t.Setenv("MAX_ITERATIONS", "")

	cfg, err := loadConfig([]string{"--max-iterations", "99", "task"})
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	a, err := newApp(cfg)
	if err != nil {
		t.Fatalf("newApp: %v", err)
	}
	rt := a.rootRuntime()
	if rt.maxToolIterations != 99 {
		t.Fatalf("expected rootRuntime.maxToolIterations=99, got %d", rt.maxToolIterations)
	}
}

func TestConfigFileCreatedWithDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.ini")

	// File does not exist yet; readConfigFile after ensureConfigFile should return defaults.
	cfg, err := readConfigFile(path)
	if err == nil && len(cfg) > 0 {
		// file existed somehow - ok, skip creation test
	}

	// Write the default content and parse it.
	if err := os.WriteFile(path, []byte(defaultConfigFileContent), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	cfg, err = readConfigFile(path)
	if err != nil {
		t.Fatalf("readConfigFile: %v", err)
	}
	if cfg["BASE_URL"] != "http://localhost:8235/v1" {
		t.Fatalf("unexpected BASE_URL: %q", cfg["BASE_URL"])
	}
	if cfg["MODEL"] != "gpt-5-mini" {
		t.Fatalf("unexpected MODEL: %q", cfg["MODEL"])
	}
	if cfg["REASONING_EFFORT"] != "medium" {
		t.Fatalf("unexpected REASONING_EFFORT: %q", cfg["REASONING_EFFORT"])
	}
}

func TestConfigFileEnvOverridesFile(t *testing.T) {
	t.Setenv("BASE_URL", "http://override:9999/v1")
	t.Setenv("MODEL", "")
	t.Setenv("TOKEN", "")
	t.Setenv("REASONING_EFFORT", "")
	t.Setenv("SYSTEM_PROMPT", "")
	t.Setenv("systemPrompt", "")
	t.Setenv("MAX_ITERATIONS", "")

	dir := t.TempDir()
	path := filepath.Join(dir, "config.ini")
	content := "BASE_URL = http://file-url:1234/v1\nMODEL = file-model\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	fileCfg, err := readConfigFile(path)
	if err != nil {
		t.Fatalf("readConfigFile: %v", err)
	}
	// Env var "BASE_URL" is set to override; file has a different value.
	got := readCfg("BASE_URL", fileCfg, defaultBaseURL)
	if got != "http://override:9999/v1" {
		t.Fatalf("expected env to win over file, got %q", got)
	}
	// MODEL has no env override; file value should be used.
	got = readCfg("MODEL", fileCfg, defaultModel)
	if got != "file-model" {
		t.Fatalf("expected file MODEL to be used, got %q", got)
	}
}

func TestReasoningEffortNoneOmitted(t *testing.T) {
	t.Setenv("REASONING_EFFORT", "none")
	v, err := readReasoningEffort(map[string]string{})
	if err != nil {
		t.Fatalf("readReasoningEffort: %v", err)
	}
	if v != "" {
		t.Fatalf("expected empty string for reasoning_effort=none, got %q", v)
	}

	// Ensure omitempty actually omits the field when empty.
	req := apiRequest{Model: "m", ReasoningEffort: v}
	b, _ := json.Marshal(req)
	if strings.Contains(string(b), "reasoning_effort") {
		t.Fatalf("expected reasoning_effort to be omitted from JSON, got %s", string(b))
	}
}

func TestReasoningEffortNoneCaseInsensitive(t *testing.T) {
	for _, val := range []string{"none", "None", "NONE", "nOnE"} {
		v, err := readReasoningEffort(map[string]string{"REASONING_EFFORT": val})
		if err != nil {
			t.Fatalf("readReasoningEffort(%q): %v", val, err)
		}
		if v != "" {
			t.Fatalf("readReasoningEffort(%q) expected empty, got %q", val, v)
		}
	}
}

func TestLoadConfigSubagentEnvVars(t *testing.T) {
	isolateConfigFile(t)
	t.Setenv("BASE_URL", "http://localhost:8235/v1")
	t.Setenv("SUBAGENT_MAX_DEPTH", "3")
	t.Setenv("SUBAGENT_MAX_CHILDREN", "12")
	t.Setenv("SUBAGENT_MAX_PARALLEL", "6")
	t.Setenv("SUBAGENT_TIMEOUT_SECONDS", "120")
	defer func() {
		t.Setenv("SUBAGENT_MAX_DEPTH", "")
		t.Setenv("SUBAGENT_MAX_CHILDREN", "")
		t.Setenv("SUBAGENT_MAX_PARALLEL", "")
		t.Setenv("SUBAGENT_TIMEOUT_SECONDS", "")
	}()

	cfg, err := loadConfig([]string{"task"})
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.subagents.MaxDepth != 3 {
		t.Fatalf("expected MaxDepth=3 from env, got %d", cfg.subagents.MaxDepth)
	}
	if cfg.subagents.MaxChildren != 12 {
		t.Fatalf("expected MaxChildren=12 from env, got %d", cfg.subagents.MaxChildren)
	}
	if cfg.subagents.MaxParallel != 6 {
		t.Fatalf("expected MaxParallel=6 from env, got %d", cfg.subagents.MaxParallel)
	}
	if cfg.subagents.DefaultTimeoutSec != 120 {
		t.Fatalf("expected DefaultTimeoutSec=120 from env, got %d", cfg.subagents.DefaultTimeoutSec)
	}
}

func TestLoadConfigSubagentFlagOverridesEnv(t *testing.T) {
	isolateConfigFile(t)
	t.Setenv("BASE_URL", "http://localhost:8235/v1")
	t.Setenv("SUBAGENT_MAX_PARALLEL", "6")
	defer t.Setenv("SUBAGENT_MAX_PARALLEL", "")

	cfg, err := loadConfig([]string{"--subagent-max-parallel", "2", "task"})
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.subagents.MaxParallel != 2 {
		t.Fatalf("expected CLI flag (2) to override env (6), got %d", cfg.subagents.MaxParallel)
	}
}

func TestLoadConfigSubagentDefaults(t *testing.T) {
	isolateConfigFile(t)
	t.Setenv("BASE_URL", "http://localhost:8235/v1")
	t.Setenv("SUBAGENT_MAX_DEPTH", "")
	t.Setenv("SUBAGENT_MAX_CHILDREN", "")
	t.Setenv("SUBAGENT_MAX_PARALLEL", "")
	t.Setenv("SUBAGENT_TIMEOUT_SECONDS", "")

	cfg, err := loadConfig([]string{"task"})
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.subagents.MaxDepth != defaultSubagentMaxDepth {
		t.Fatalf("expected default MaxDepth=%d, got %d", defaultSubagentMaxDepth, cfg.subagents.MaxDepth)
	}
	if cfg.subagents.MaxChildren != defaultSubagentMaxChildren {
		t.Fatalf("expected default MaxChildren=%d, got %d", defaultSubagentMaxChildren, cfg.subagents.MaxChildren)
	}
	if cfg.subagents.MaxParallel != defaultSubagentMaxParallel {
		t.Fatalf("expected default MaxParallel=%d, got %d", defaultSubagentMaxParallel, cfg.subagents.MaxParallel)
	}
	if cfg.subagents.DefaultTimeoutSec != defaultSubagentDefaultTimeoutSec {
		t.Fatalf("expected default DefaultTimeoutSec=%d, got %d", defaultSubagentDefaultTimeoutSec, cfg.subagents.DefaultTimeoutSec)
	}
}

func TestUpsertConfigFileKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.ini")

	// Write a minimal existing config (old format, missing subagent keys).
	existing := "BASE_URL = http://localhost:8235/v1\nMODEL = gpt-5-mini\n"
	if err := os.WriteFile(path, []byte(existing), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if err := upsertConfigFileKeys(path); err != nil {
		t.Fatalf("upsertConfigFileKeys: %v", err)
	}

	cfg, err := readConfigFile(path)
	if err != nil {
		t.Fatalf("readConfigFile after upsert: %v", err)
	}

	// Pre-existing values must be unchanged.
	if cfg["BASE_URL"] != "http://localhost:8235/v1" {
		t.Fatalf("upsert changed existing BASE_URL: %q", cfg["BASE_URL"])
	}

	// New keys must have been added with their defaults.
	if cfg["SUBAGENT_MAX_PARALLEL"] != "4" {
		t.Fatalf("expected SUBAGENT_MAX_PARALLEL=4 after upsert, got %q", cfg["SUBAGENT_MAX_PARALLEL"])
	}
	if cfg["SUBAGENT_MAX_DEPTH"] != "1" {
		t.Fatalf("expected SUBAGENT_MAX_DEPTH=1 after upsert, got %q", cfg["SUBAGENT_MAX_DEPTH"])
	}
	if cfg["ON_MAX_ITERATIONS"] != "continue" {
		t.Fatalf("expected ON_MAX_ITERATIONS=continue after upsert, got %q", cfg["ON_MAX_ITERATIONS"])
	}

	// Calling upsert again must be idempotent.
	if err := upsertConfigFileKeys(path); err != nil {
		t.Fatalf("second upsertConfigFileKeys: %v", err)
	}
	cfg2, err := readConfigFile(path)
	if err != nil {
		t.Fatalf("readConfigFile after second upsert: %v", err)
	}
	if cfg2["SUBAGENT_MAX_PARALLEL"] != cfg["SUBAGENT_MAX_PARALLEL"] {
		t.Fatal("upsert is not idempotent")
	}
}
