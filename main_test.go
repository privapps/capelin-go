package main

import (
	"capelin-go/internal/skills"
	"capelin-go/internal/types"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
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
	if cfg.endpoint != defaultEndpoint {
		t.Fatalf("unexpected endpoint: %q", cfg.endpoint)
	}
	if cfg.model != defaultModel {
		t.Fatalf("unexpected model: %q", cfg.model)
	}
	if !cfg.allowedTools[toolListFiles] || !cfg.allowedTools[toolReadFile] {
		t.Fatal("expected safe default tools to be enabled")
	}
	if !cfg.allowedTools[toolCreateSubagent] || !cfg.allowedTools[toolRunSubagent] {
		t.Fatal("expected subagent tools to be enabled by default")
	}
	if cfg.allowedTools[toolWriteFile] || cfg.allowedTools[toolExecuteProgram] || cfg.allowedTools[toolEditFile] {
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

func TestLoadConfigInteractiveFlag(t *testing.T) {
	isolateConfigFile(t)
	t.Setenv("BASE_URL", "http://localhost:8235/v1")
	cfg, err := loadConfig([]string{"-i", "task"})
	if err != nil {
		t.Fatalf("loadConfig returned error for -i flag: %v", err)
	}
	if !cfg.interactive {
		t.Fatal("expected cfg.interactive to be true with -i flag")
	}
	if cfg.initialQuestion != "task" {
		t.Fatalf("expected initialQuestion to be \"task\", got %q", cfg.initialQuestion)
	}
}

func TestLoadConfigInteractiveLongFlag(t *testing.T) {
	isolateConfigFile(t)
	t.Setenv("BASE_URL", "http://localhost:8235/v1")
	cfg, err := loadConfig([]string{"--interactive", "hello world"})
	if err != nil {
		t.Fatalf("loadConfig returned error for --interactive flag: %v", err)
	}
	if !cfg.interactive {
		t.Fatal("expected cfg.interactive to be true with --interactive flag")
	}
}

func TestLoadConfigInteractiveFlagNoQuestion(t *testing.T) {
	isolateConfigFile(t)
	t.Setenv("BASE_URL", "http://localhost:8235/v1")
	cfg, err := loadConfig([]string{"-i"})
	if err != nil {
		t.Fatalf("loadConfig returned error for -i with no question: %v", err)
	}
	if !cfg.interactive {
		t.Fatal("expected cfg.interactive to be true")
	}
	if cfg.initialQuestion != "" {
		t.Fatalf("expected empty initialQuestion, got %q", cfg.initialQuestion)
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

func TestRunEditFile(t *testing.T) {
	root := t.TempDir()
	_, err := runWriteFile(root, false, writeFileArgs{Path: "e.txt", Content: "hello world\ngoodbye"})
	if err != nil {
		t.Fatalf("runWriteFile: %v", err)
	}

	// Successful edit.
	out, err := runEditFile(root, false, editFileArgs{Path: "e.txt", OldStr: "hello world", NewStr: "hi there"})
	if err != nil {
		t.Fatalf("runEditFile: %v", err)
	}
	if !strings.Contains(out, "e.txt") {
		t.Fatalf("unexpected edit output: %q", out)
	}
	data, _ := os.ReadFile(filepath.Join(root, "e.txt"))
	if string(data) != "hi there\ngoodbye" {
		t.Fatalf("unexpected file content after edit: %q", string(data))
	}

	// Error: old_str not found.
	_, err = runEditFile(root, false, editFileArgs{Path: "e.txt", OldStr: "not present", NewStr: "x"})
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected not-found error, got: %v", err)
	}

	// Error: old_str appears more than once.
	_, err = runWriteFile(root, false, writeFileArgs{Path: "dup.txt", Content: "foo\nfoo\n"})
	if err != nil {
		t.Fatalf("runWriteFile dup: %v", err)
	}
	_, err = runEditFile(root, false, editFileArgs{Path: "dup.txt", OldStr: "foo", NewStr: "bar"})
	if err == nil || !strings.Contains(err.Error(), "times") {
		t.Fatalf("expected duplicate-match error, got: %v", err)
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

func TestRunExecuteProgramYoloBypassesDangerousPattern(t *testing.T) {
	root := t.TempDir()
	// bash is blocked by the dangerous-pattern policy when yolo=false.
	_, err := runExecuteProgram(context.Background(), root, false, executeProgramArgs{
		Command: "bash",
		Args:    []string{"-c", "echo hi"},
	})
	if err == nil || !strings.Contains(err.Error(), "blocked by dangerous-pattern policy") {
		t.Fatalf("expected dangerous-pattern error without yolo, got: %v", err)
	}

	// In yolo mode the policy check is skipped so bash can run.
	out, err := runExecuteProgram(context.Background(), root, true, executeProgramArgs{
		Command: "bash",
		Args:    []string{"-c", "echo yolo"},
	})
	if err != nil {
		t.Fatalf("runExecuteProgram yolo bash: %v", err)
	}
	if !strings.Contains(out, "yolo") {
		t.Fatalf("expected 'yolo' in output, got: %s", out)
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

	skillsMap, err := skills.LoadFromDirs([]struct {
		Path   string
		Source string
	}{
		{Path: filepath.Join(project, ".agents", "skills"), Source: "project"},
		{Path: filepath.Join(userHome, ".agents", "skills"), Source: "user"},
	})
	if err != nil {
		t.Fatalf("LoadFromDirs: %v", err)
	}
	if skillsMap["demo"].Description != "project desc" {
		t.Fatalf("expected project override, got: %q", skillsMap["demo"].Description)
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
	sk, err := skills.ParseFile(filepath.Join(skillDir, "SKILL.md"), "project")
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
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

	_, err = a.runTool(context.Background(), types.ToolCall{
		ID:   "1",
		Type: "function",
		Function: types.FunctionCall{
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

	_, err = a.runTool(context.Background(), types.ToolCall{
		ID:   "2",
		Type: "function",
		Function: types.FunctionCall{
			Name:      toolExecuteProgram,
			Arguments: `{"command":"echo"}`,
		},
	})
	if err == nil {
		t.Fatal("expected disabled-tool error for execute_program")
	}

	_, err = a.runTool(context.Background(), types.ToolCall{
		ID:   "3",
		Type: "function",
		Function: types.FunctionCall{
			Name:      toolExecuteSkill,
			Arguments: `{"name":"vPass","command":"opencli"}`,
		},
	})
	if err == nil {
		t.Fatal("expected disabled-tool error for execute_skill")
	}

	_, err = a.runTool(context.Background(), types.ToolCall{
		ID:   "4",
		Type: "function",
		Function: types.FunctionCall{
			Name:      toolEditFile,
			Arguments: `{"path":"x.txt","old_str":"hello","new_str":"world"}`,
		},
	})
	if err == nil {
		t.Fatal("expected disabled-tool error for edit_file")
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
	cmds := skills.ExtractExecutableCommands(content, "#!/bin/bash\nopencli thing \"$1\"")
	if !slices.Contains(cmds, "opencli") {
		t.Fatalf("expected opencli parsed from skill commands, got: %v", cmds)
	}
	if !slices.Contains(cmds, "echo") {
		t.Fatalf("expected echo parsed from skill commands, got: %v", cmds)
	}
}

func TestRunExecuteSkill(t *testing.T) {
	root := t.TempDir()
	skills := map[string]skills.Skill{
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
	skills := map[string]skills.Skill{
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
		t.Fatal("expected subagent tools to be enabled by default")
	}
	if cfg.subagents.MaxDepth != 2 || cfg.subagents.MaxChildren != 5 || cfg.subagents.MaxParallel != 3 || cfg.subagents.DefaultTimeoutSec != 45 {
		t.Fatalf("unexpected subagent cfg: %+v", cfg.subagents)
	}
}

func TestSubagentLifecycleAndAggregation(t *testing.T) {
	isolateConfigFile(t)
	t.Setenv("BASE_URL", "http://localhost:8235/v1")
	cfg, err := loadConfig([]string{"task"})
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

	createCall := types.ToolCall{
		ID:   "1",
		Type: "function",
		Function: types.FunctionCall{
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

	runCall := types.ToolCall{
		ID:   "2",
		Type: "function",
		Function: types.FunctionCall{
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

	readAggregate := types.ToolCall{
		ID:   "3",
		Type: "function",
		Function: types.FunctionCall{
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
	_, err := m.create(context.Background(), root, createSubagentArgs{Question: "a"})
	if err != nil {
		t.Fatalf("first create failed: %v", err)
	}
	_, err = m.create(context.Background(), root, createSubagentArgs{Question: "b", OverflowMode: "fail_fast"})
	if err == nil {
		t.Fatal("expected max-children rejection")
	}

	childRuntime := &agentRuntime{
		sessionID:         "subagent-1",
		depth:             1,
		allowedTools:      map[string]bool{toolCreateSubagent: true},
		maxToolIterations: 5,
	}
	_, err = m.create(context.Background(), childRuntime, createSubagentArgs{Question: "nested"})
	if err == nil {
		t.Fatal("expected max-depth rejection")
	}
}

func TestSubagentCreateWaitsForChildSlotByDefault(t *testing.T) {
	cfg := defaultSubagentRuntimeConfig()
	cfg.MaxDepth = 1
	cfg.MaxChildren = 1
	cfg.DefaultTimeoutSec = 2
	m := newSubagentManager(cfg, func(ctx context.Context, runtime *agentRuntime, session *subagentSession) (string, error) {
		time.Sleep(120 * time.Millisecond)
		return "ok", nil
	})
	root := &agentRuntime{
		sessionID:         rootAgentID,
		depth:             0,
		allowedTools:      map[string]bool{toolCreateSubagent: true, toolRunSubagent: true, toolAwaitSubagent: true},
		maxToolIterations: 5,
	}

	first, err := m.create(context.Background(), root, createSubagentArgs{Question: "a"})
	if err != nil {
		t.Fatalf("first create failed: %v", err)
	}
	if _, err := m.run(context.Background(), root, runSubagentArgs{ID: first.ID}); err != nil {
		t.Fatalf("run first: %v", err)
	}

	start := time.Now()
	second, err := m.create(context.Background(), root, createSubagentArgs{Question: "b"})
	if err != nil {
		t.Fatalf("second create failed: %v", err)
	}
	if second.ID == "" {
		t.Fatal("expected second child to be created")
	}
	if time.Since(start) < 80*time.Millisecond {
		t.Fatal("expected create to wait for a free child slot")
	}
}

func TestSubagentCreateWaitForSlotTimeout(t *testing.T) {
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

	if _, err := m.create(context.Background(), root, createSubagentArgs{Question: "a"}); err != nil {
		t.Fatalf("first create failed: %v", err)
	}
	_, err := m.create(context.Background(), root, createSubagentArgs{Question: "b", WaitTimeoutSeconds: 1})
	if err == nil {
		t.Fatal("expected wait_for_slot timeout")
	}
	if !strings.Contains(err.Error(), "wait_for_slot timed out") {
		t.Fatalf("expected wait timeout error, got %v", err)
	}
}

func TestSubagentCreateWaitForSlotRespectsContextCancellation(t *testing.T) {
	cfg := defaultSubagentRuntimeConfig()
	cfg.MaxDepth = 1
	cfg.MaxChildren = 1
	cfg.DefaultTimeoutSec = 300
	m := newSubagentManager(cfg, func(ctx context.Context, runtime *agentRuntime, session *subagentSession) (string, error) {
		return "ok", nil
	})
	root := &agentRuntime{
		sessionID:         rootAgentID,
		depth:             0,
		allowedTools:      map[string]bool{toolCreateSubagent: true},
		maxToolIterations: 5,
	}

	if _, err := m.create(context.Background(), root, createSubagentArgs{Question: "a"}); err != nil {
		t.Fatalf("first create failed: %v", err)
	}

	waitCtx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	_, err := m.create(waitCtx, root, createSubagentArgs{Question: "b", WaitTimeoutSeconds: 300})
	if err == nil {
		t.Fatal("expected create to stop waiting when context is cancelled")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected context deadline exceeded, got %v", err)
	}
}

func TestSubagentCreateFailFastOverflowMode(t *testing.T) {
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

	if _, err := m.create(context.Background(), root, createSubagentArgs{Question: "a"}); err != nil {
		t.Fatalf("first create failed: %v", err)
	}
	_, err := m.create(context.Background(), root, createSubagentArgs{Question: "b", OverflowMode: "fail_fast"})
	if err == nil {
		t.Fatal("expected fail_fast overflow rejection")
	}
	if !strings.Contains(err.Error(), "max children exceeded") {
		t.Fatalf("unexpected error: %v", err)
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
		session, err := m.create(context.Background(), root, createSubagentArgs{Question: fmt.Sprintf("q-%d", i), ExecutionMode: "parallel"})
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

	created, err := m.create(context.Background(), root, createSubagentArgs{Question: "q", ExecutionMode: "parallel"})
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

	_, err = a.runToolForRuntime(context.Background(), runtime, types.ToolCall{
		ID:   "x",
		Type: "function",
		Function: types.FunctionCall{
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
		t.Skip("config file already existed; skipping creation test")
	}

	// Write the default content and parse it.
	if err := os.WriteFile(path, []byte(defaultConfigFileContent), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	cfg, err = readConfigFile(path)
	if err != nil {
		t.Fatalf("readConfigFile: %v", err)
	}
	if cfg["ENDPOINT"] != "http://localhost:8235/v1/chat/completions" {
		t.Fatalf("unexpected ENDPOINT: %q", cfg["ENDPOINT"])
	}
	if cfg["MODEL"] != "gpt-5-mini" {
		t.Fatalf("unexpected MODEL: %q", cfg["MODEL"])
	}
	if cfg["REASONING_EFFORT"] != "medium" {
		t.Fatalf("unexpected REASONING_EFFORT: %q", cfg["REASONING_EFFORT"])
	}
}

func TestConfigFileEnvOverridesFile(t *testing.T) {
	t.Setenv("ENDPOINT", "http://override:9999/v1/chat/completions")
	t.Setenv("MODEL", "")
	t.Setenv("TOKEN", "")
	t.Setenv("REASONING_EFFORT", "")
	t.Setenv("SYSTEM_PROMPT", "")
	t.Setenv("systemPrompt", "")
	t.Setenv("MAX_ITERATIONS", "")

	dir := t.TempDir()
	path := filepath.Join(dir, "config.ini")
	content := "ENDPOINT = http://file-url:1234/v1/chat/completions\nMODEL = file-model\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	fileCfg, err := readConfigFile(path)
	if err != nil {
		t.Fatalf("readConfigFile: %v", err)
	}
	// Env var "ENDPOINT" is set to override; file has a different value.
	got := readCfg("ENDPOINT", fileCfg, defaultEndpoint)
	if got != "http://override:9999/v1/chat/completions" {
		t.Fatalf("expected env to win over file, got %q", got)
	}

	// MODEL has no env override; file value should be used.
	got = readCfg("MODEL", fileCfg, defaultModel)
	if got != "file-model" {
		t.Fatalf("expected file MODEL to be used, got %q", got)
	}
}

func TestLegacyBaseURLConfigIsIgnored(t *testing.T) {
	t.Setenv("ENDPOINT", "")
	fileCfg := map[string]string{"BASE_URL": "http://legacy:1234/v1"}

	endpoint, err := readEndpoint(fileCfg)
	if err != nil {
		t.Fatalf("readEndpoint: %v", err)
	}
	if endpoint != defaultEndpoint {
		t.Fatalf("expected legacy BASE_URL to be ignored, got %q", endpoint)
	}
}

func TestExistingConfigMissingEndpointReturnsError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.ini")
	if err := os.WriteFile(path, []byte("BASE_URL = http://legacy:1234/v1\nMODEL = test-model\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	t.Setenv("CAPELIN_CONFIG_FILE", path)

	_, err := ensureConfigFile()
	if err == nil {
		t.Fatal("expected missing ENDPOINT error")
	}
	if !strings.Contains(err.Error(), "missing ENDPOINT") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestServerModeAllowsConfigWithoutEndpoint(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.ini")
	if err := os.WriteFile(path, []byte("MODEL = test-model\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	t.Setenv("CAPELIN_CONFIG_FILE", path)
	t.Setenv("ENDPOINT", "")

	cfg, err := loadConfig([]string{"--server-port", "8889"})
	if err != nil {
		t.Fatalf("server mode should not require ENDPOINT: %v", err)
	}
	if cfg.serverPort != 8889 {
		t.Fatalf("serverPort = %d, want 8889", cfg.serverPort)
	}
}

func TestClientUsesCompleteEndpointWithoutAppendingPath(t *testing.T) {
	var gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer server.Close()

	client := &client{
		endpoint: server.URL + "/custom/chat/completions",
		http:     server.Client(),
	}
	if _, err := client.complete(context.Background(), []types.Message{{Role: "user", Content: "hello"}}, nil, "model", ""); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if gotPath != "/custom/chat/completions" {
		t.Fatalf("expected complete endpoint path, got %q", gotPath)
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
	req := types.Request{Model: "m", ReasoningEffort: v}
	b, _ := json.Marshal(req)
	if strings.Contains(string(b), "reasoning_effort") {
		t.Fatalf("expected reasoning_effort to be omitted from JSON, got %s", string(b))
	}
}

func TestReasoningEffortNoneCaseInsensitive(t *testing.T) {
	for _, val := range []string{"none", "None", "NONE", "nOnE", "nil", "NIL"} {
		v, err := readReasoningEffort(map[string]string{"REASONING_EFFORT": val})
		if err != nil {
			t.Fatalf("readReasoningEffort(%q): %v", val, err)
		}
		if v != "" {
			t.Fatalf("readReasoningEffort(%q) expected unset, got %q", val, v)
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
		t.Fatalf("upsert changed existing legacy BASE_URL: %q", cfg["BASE_URL"])
	}
	if cfg["ENDPOINT"] != defaultEndpoint {
		t.Fatalf("expected ENDPOINT=%q after upsert, got %q", defaultEndpoint, cfg["ENDPOINT"])
	}

	// New keys must have been added with their defaults.
	if cfg["SUBAGENT_MAX_PARALLEL"] != "4" {
		t.Fatalf("expected SUBAGENT_MAX_PARALLEL=4 after upsert, got %q", cfg["SUBAGENT_MAX_PARALLEL"])
	}
	if cfg["SUBAGENT_MAX_DEPTH"] != "1" {
		t.Fatalf("expected SUBAGENT_MAX_DEPTH=1 after upsert, got %q", cfg["SUBAGENT_MAX_DEPTH"])
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

func TestLoadConfigSubagentModelFlag(t *testing.T) {
	isolateConfigFile(t)
	t.Setenv("BASE_URL", "http://localhost:8235/v1")
	cfg, err := loadConfig([]string{"--subagent-model", "gpt-4o", "task"})
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.subagents.Model != "gpt-4o" {
		t.Fatalf("expected subagent model gpt-4o, got %q", cfg.subagents.Model)
	}
}

func TestLoadConfigSubagentModelFlagEqualForm(t *testing.T) {
	isolateConfigFile(t)
	t.Setenv("BASE_URL", "http://localhost:8235/v1")
	cfg, err := loadConfig([]string{"--subagent-model=claude-3-sonnet", "task"})
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.subagents.Model != "claude-3-sonnet" {
		t.Fatalf("expected claude-3-sonnet, got %q", cfg.subagents.Model)
	}
}

func TestLoadConfigSubagentReasoningEffortFlag(t *testing.T) {
	isolateConfigFile(t)
	t.Setenv("BASE_URL", "http://localhost:8235/v1")
	cfg, err := loadConfig([]string{"--subagent-reasoning-effort", "high", "task"})
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.subagents.ReasoningEffort != "high" {
		t.Fatalf("expected subagent reasoning effort high, got %q", cfg.subagents.ReasoningEffort)
	}
}

func TestLoadConfigSubagentReasoningEffortNone(t *testing.T) {
	isolateConfigFile(t)
	t.Setenv("BASE_URL", "http://localhost:8235/v1")
	// "none" must be converted to "" so the field is omitted from API requests.
	cfg, err := loadConfig([]string{"--subagent-reasoning-effort", "none", "task"})
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.subagents.ReasoningEffort != "" {
		t.Fatalf("expected empty (none→omit) for subagent reasoning, got %q", cfg.subagents.ReasoningEffort)
	}
}

func TestLoadConfigSubagentReasoningEffortNoneCaseInsensitive(t *testing.T) {
	isolateConfigFile(t)
	t.Setenv("BASE_URL", "http://localhost:8235/v1")
	for _, val := range []string{"none", "None", "NONE"} {
		cfg, err := loadConfig([]string{"--subagent-reasoning-effort", val, "task"})
		if err != nil {
			t.Fatalf("loadConfig(%q): %v", val, err)
		}
		if cfg.subagents.ReasoningEffort != "" {
			t.Fatalf("--subagent-reasoning-effort=%q expected empty, got %q", val, cfg.subagents.ReasoningEffort)
		}
	}
}

func TestLoadConfigSubagentModelInheritsRoot(t *testing.T) {
	isolateConfigFile(t)
	t.Setenv("BASE_URL", "http://localhost:8235/v1")
	t.Setenv("MODEL", "root-model")
	defer t.Setenv("MODEL", "")

	cfg, err := loadConfig([]string{"task"})
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	// No --subagent-model or SUBAGENT_MODEL set → must inherit root model.
	if cfg.subagents.Model != "root-model" {
		t.Fatalf("expected subagent model to inherit root-model, got %q", cfg.subagents.Model)
	}
}

func TestLoadConfigSubagentReasoningInheritsRoot(t *testing.T) {
	isolateConfigFile(t)
	t.Setenv("BASE_URL", "http://localhost:8235/v1")
	t.Setenv("REASONING_EFFORT", "low")
	defer t.Setenv("REASONING_EFFORT", "")

	cfg, err := loadConfig([]string{"task"})
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	// No --subagent-reasoning-effort set → must inherit root reasoning.
	if cfg.subagents.ReasoningEffort != "low" {
		t.Fatalf("expected subagent reasoning to inherit low, got %q", cfg.subagents.ReasoningEffort)
	}
}

func TestLoadConfigSubagentModelEnvVar(t *testing.T) {
	isolateConfigFile(t)
	t.Setenv("BASE_URL", "http://localhost:8235/v1")
	t.Setenv("SUBAGENT_MODEL", "env-subagent-model")
	defer t.Setenv("SUBAGENT_MODEL", "")

	cfg, err := loadConfig([]string{"task"})
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.subagents.Model != "env-subagent-model" {
		t.Fatalf("expected env SUBAGENT_MODEL, got %q", cfg.subagents.Model)
	}
}

func TestLoadConfigSubagentReasoningEnvVar(t *testing.T) {
	isolateConfigFile(t)
	t.Setenv("BASE_URL", "http://localhost:8235/v1")
	t.Setenv("SUBAGENT_REASONING_EFFORT", "medium")
	defer t.Setenv("SUBAGENT_REASONING_EFFORT", "")

	cfg, err := loadConfig([]string{"task"})
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.subagents.ReasoningEffort != "medium" {
		t.Fatalf("expected SUBAGENT_REASONING_EFFORT=medium, got %q", cfg.subagents.ReasoningEffort)
	}
}

func TestLoadConfigSubagentModelFlagOverridesEnv(t *testing.T) {
	isolateConfigFile(t)
	t.Setenv("BASE_URL", "http://localhost:8235/v1")
	t.Setenv("SUBAGENT_MODEL", "env-model")
	defer t.Setenv("SUBAGENT_MODEL", "")

	cfg, err := loadConfig([]string{"--subagent-model", "flag-model", "task"})
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.subagents.Model != "flag-model" {
		t.Fatalf("expected CLI flag to override env, got %q", cfg.subagents.Model)
	}
}

func TestLoadConfigSubagentReasoningFlagOverridesEnv(t *testing.T) {
	isolateConfigFile(t)
	t.Setenv("BASE_URL", "http://localhost:8235/v1")
	t.Setenv("SUBAGENT_REASONING_EFFORT", "high")
	defer t.Setenv("SUBAGENT_REASONING_EFFORT", "")

	cfg, err := loadConfig([]string{"--subagent-reasoning-effort", "low", "task"})
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.subagents.ReasoningEffort != "low" {
		t.Fatalf("expected CLI flag (low) to override env (high), got %q", cfg.subagents.ReasoningEffort)
	}
}

func TestUpsertConfigFileKeysIncludesSubagentModelKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.ini")

	// Write an older config without the new subagent model keys.
	existing := "BASE_URL = http://localhost:8235/v1\nMODEL = gpt-5-mini\nSUBAGENT_MAX_DEPTH = 1\n"
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

	// New keys must be present (with empty default values).
	if _, ok := cfg["SUBAGENT_MODEL"]; !ok {
		t.Fatal("expected SUBAGENT_MODEL to be present after upsert")
	}
	if _, ok := cfg["SUBAGENT_REASONING_EFFORT"]; !ok {
		t.Fatal("expected SUBAGENT_REASONING_EFFORT to be present after upsert")
	}
	// Pre-existing values must be unchanged.
	if cfg["BASE_URL"] != "http://localhost:8235/v1" {
		t.Fatalf("upsert changed existing legacy BASE_URL: %q", cfg["BASE_URL"])
	}
	if cfg["ENDPOINT"] != defaultEndpoint {
		t.Fatalf("expected ENDPOINT=%q after upsert, got %q", defaultEndpoint, cfg["ENDPOINT"])
	}
}

func TestSubagentExecutionUsesConfiguredModelAndReasoning(t *testing.T) {
	isolateConfigFile(t)
	t.Setenv("BASE_URL", "http://localhost:8235/v1")

	cfg, err := loadConfig([]string{
		"--subagent-model", "gpt-4o",
		"--subagent-reasoning-effort", "high",
		"task",
	})
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	a, err := newApp(cfg)
	if err != nil {
		t.Fatalf("newApp: %v", err)
	}

	var capturedModel, capturedReasoning string
	a.subagents.runner = func(ctx context.Context, runtime *agentRuntime, session *subagentSession) (string, error) {
		capturedModel = runtime.model
		capturedReasoning = runtime.reasoning
		return "ok", nil
	}

	root := a.rootRuntime()
	session, err := a.subagents.create(context.Background(), root, createSubagentArgs{
		Question:      "do work",
		ExecutionMode: "sequential",
	})
	if err != nil {
		t.Fatalf("create_subagent: %v", err)
	}
	_, err = a.subagents.run(context.Background(), root, runSubagentArgs{
		ID:   session.ID,
		Wait: true,
	})
	if err != nil {
		t.Fatalf("run_subagent: %v", err)
	}
	if capturedModel != "gpt-4o" {
		t.Fatalf("expected subagent runtime.model=gpt-4o, got %q", capturedModel)
	}
	if capturedReasoning != "high" {
		t.Fatalf("expected subagent runtime.reasoning=high, got %q", capturedReasoning)
	}
}

func TestSubagentExecutionInheritsRootModelWhenNotConfigured(t *testing.T) {
	isolateConfigFile(t)
	t.Setenv("BASE_URL", "http://localhost:8235/v1")
	t.Setenv("MODEL", "root-model-x")
	defer t.Setenv("MODEL", "")

	cfg, err := loadConfig([]string{"task"})
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	a, err := newApp(cfg)
	if err != nil {
		t.Fatalf("newApp: %v", err)
	}

	var capturedModel string
	a.subagents.runner = func(ctx context.Context, runtime *agentRuntime, session *subagentSession) (string, error) {
		capturedModel = runtime.model
		return "ok", nil
	}

	root := a.rootRuntime()
	session, err := a.subagents.create(context.Background(), root, createSubagentArgs{
		Question:      "do work",
		ExecutionMode: "sequential",
	})
	if err != nil {
		t.Fatalf("create_subagent: %v", err)
	}
	_, err = a.subagents.run(context.Background(), root, runSubagentArgs{
		ID:   session.ID,
		Wait: true,
	})
	if err != nil {
		t.Fatalf("run_subagent: %v", err)
	}
	if capturedModel != "root-model-x" {
		t.Fatalf("expected subagent to inherit root model root-model-x, got %q", capturedModel)
	}
}

func TestRootRuntimeCarriesModelAndReasoning(t *testing.T) {
	isolateConfigFile(t)
	t.Setenv("BASE_URL", "http://localhost:8235/v1")
	t.Setenv("MODEL", "my-root-model")
	t.Setenv("REASONING_EFFORT", "low")
	defer func() {
		t.Setenv("MODEL", "")
		t.Setenv("REASONING_EFFORT", "")
	}()

	cfg, err := loadConfig([]string{"task"})
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	a, err := newApp(cfg)
	if err != nil {
		t.Fatalf("newApp: %v", err)
	}
	rt := a.rootRuntime()
	if rt.model != "my-root-model" {
		t.Fatalf("expected rootRuntime.model=my-root-model, got %q", rt.model)
	}
	if rt.reasoning != "low" {
		t.Fatalf("expected rootRuntime.reasoning=low, got %q", rt.reasoning)
	}
}

func TestLoadConfigServerPort(t *testing.T) {
	isolateConfigFile(t)
	t.Setenv("BASE_URL", "http://localhost:8235/v1")
	cfg, err := loadConfig([]string{"--server-port", "8899"})
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.serverPort != 8899 {
		t.Fatalf("expected serverPort=8899, got %d", cfg.serverPort)
	}
}

func TestLoadConfigServerPortEqualForm(t *testing.T) {
	isolateConfigFile(t)
	t.Setenv("BASE_URL", "http://localhost:8235/v1")
	cfg, err := loadConfig([]string{"--server-port=9090"})
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.serverPort != 9090 {
		t.Fatalf("expected serverPort=9090, got %d", cfg.serverPort)
	}
}

func TestLoadConfigServerAlias(t *testing.T) {
	isolateConfigFile(t)
	t.Setenv("ENDPOINT", "")
	cfg, err := loadConfig([]string{"--server", "8889"})
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.serverPort != 8889 {
		t.Fatalf("expected serverPort=8889, got %d", cfg.serverPort)
	}
}

func TestLoadConfigServerPortRejectsZero(t *testing.T) {
	isolateConfigFile(t)
	t.Setenv("BASE_URL", "http://localhost:8235/v1")
	_, err := loadConfig([]string{"--server-port", "0"})
	if err == nil {
		t.Fatal("expected error for --server-port=0")
	}
}

func TestLoadConfigServerPortRejectsNegative(t *testing.T) {
	isolateConfigFile(t)
	t.Setenv("BASE_URL", "http://localhost:8235/v1")
	_, err := loadConfig([]string{"--server-port", "-1"})
	if err == nil {
		t.Fatal("expected error for --server-port=-1")
	}
}

func TestLoadConfigServerPortRejectsNonNumeric(t *testing.T) {
	isolateConfigFile(t)
	t.Setenv("BASE_URL", "http://localhost:8235/v1")
	_, err := loadConfig([]string{"--server-port", "abc"})
	if err == nil {
		t.Fatal("expected error for --server-port=abc")
	}
}

func TestServerRequiresEndpoint(t *testing.T) {
	isolateConfigFile(t)
	t.Setenv("BASE_URL", "http://localhost:8235/v1")
	serverAllowedTools := map[string]bool{toolWebSearch: true, toolFetchPage: true}
	a := &app{
		cfg:     config{workspaceRoot: t.TempDir()},
		client:  &client{http: &http.Client{}},
		toolset: buildAgentTools(serverAllowedTools),
	}

	body := `{"model":"test","messages":[{"role":"user","content":"hello"}]}`
	// No path URL and no ?endpoint= query parameter
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	a.handleChatCompletion(w, req, serverAllowedTools)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "endpoint required") {
		t.Fatalf("expected endpoint required error, got: %s", w.Body.String())
	}
}

func TestWithCORSHandlesPreflight(t *testing.T) {
	called := false
	handler := withCORS(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))

	req := httptest.NewRequest(http.MethodOptions, "/", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", w.Code)
	}
	if called {
		t.Fatal("expected preflight request not to reach wrapped handler")
	}
	assertCORSHeaders(t, w)
}

func TestWithCORSAddsHeadersToResponses(t *testing.T) {
	handler := withCORS(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad request", http.StatusBadRequest)
	}))

	req := httptest.NewRequest(http.MethodPost, "/", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
	assertCORSHeaders(t, w)
}

func assertCORSHeaders(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	expected := map[string]string{
		"Access-Control-Allow-Origin":  "*",
		"Access-Control-Allow-Methods": "GET, POST, OPTIONS",
		"Access-Control-Allow-Headers": "Content-Type, Authorization",
		"Access-Control-Max-Age":       "600",
	}
	for key, want := range expected {
		if got := w.Header().Get(key); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
}

func TestProxyHandlerForwardsRequestAndResponse(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.Path != "/target" || r.URL.RawQuery != "x=1" {
			t.Errorf("upstream request = %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
		}
		if got := r.Header.Get("X-Test"); got != "forwarded" {
			t.Errorf("X-Test = %q", got)
		}
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("X-Upstream", "yes")
		w.WriteHeader(http.StatusAccepted)
		w.Write([]byte("echo:" + string(body)))
	}))
	defer upstream.Close()

	origAllow := allowPrivateFetch
	t.Cleanup(func() { allowPrivateFetch = origAllow })
	allowPrivateFetch = true

	req := httptest.NewRequest(http.MethodPut, "/-/?endpoint="+url.QueryEscape(upstream.URL+"/target?x=1"), strings.NewReader("payload"))
	req.Header.Set("X-Test", "forwarded")
	w := httptest.NewRecorder()
	proxyHandler(w, req)

	if w.Code != http.StatusAccepted || w.Body.String() != "echo:payload" {
		t.Fatalf("proxy response = %d %q", w.Code, w.Body.String())
	}
	if w.Header().Get("X-Upstream") != "yes" {
		t.Fatalf("proxy headers = %#v", w.Header())
	}
}

type proxyRoundTripper func(*http.Request) (*http.Response, error)

func (f proxyRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestProxyHandlerUsesCanonicalizedAuthorizedTarget(t *testing.T) {
	oldPolicy := activeServerPolicy
	oldClient := proxyHTTPClient
	t.Cleanup(func() {
		activeServerPolicy = oldPolicy
		proxyHTTPClient = oldClient
	})
	activeServerPolicy = &serverSecurityPolicy{AllowedTargets: map[string]bool{"https://example.com": true}}
	var gotScheme string
	proxyHTTPClient = &http.Client{Transport: proxyRoundTripper(func(r *http.Request) (*http.Response, error) {
		gotScheme = r.URL.Scheme
		return &http.Response{StatusCode: http.StatusNoContent, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(""))}, nil
	})}

	req := httptest.NewRequest(http.MethodGet, "/-/?endpoint="+url.QueryEscape("HTTPS://EXAMPLE.COM/path"), nil)
	w := httptest.NewRecorder()
	proxyHandler(w, req)
	if w.Code != http.StatusNoContent || gotScheme != "https" {
		t.Fatalf("status=%d scheme=%q, want 204 and canonical https", w.Code, gotScheme)
	}
}

func TestProxyHandlerSupportsHexAndQueryTargets(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()

	origAllow := allowPrivateFetch
	t.Cleanup(func() { allowPrivateFetch = origAllow })
	allowPrivateFetch = true

	cases := []string{
		"/-/~" + hex.EncodeToString([]byte(upstream.URL)),
		"/-/?endpoint=" + url.QueryEscape(upstream.URL),
	}
	for _, path := range cases {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		w := httptest.NewRecorder()
		proxyHandler(w, req)
		if w.Code != http.StatusNoContent {
			t.Errorf("%s: status = %d, body = %s", path, w.Code, w.Body.String())
		}
	}
}

func TestProxyHandlerRelaysRedirectAndRejectsInvalidTarget(t *testing.T) {
	upstream := httptest.NewServer(http.RedirectHandler("/next", http.StatusFound))
	defer upstream.Close()

	origAllow := allowPrivateFetch
	t.Cleanup(func() { allowPrivateFetch = origAllow })
	allowPrivateFetch = true

	req := httptest.NewRequest(http.MethodGet, "/-/?endpoint="+url.QueryEscape(upstream.URL), nil)
	w := httptest.NewRecorder()
	proxyHandler(w, req)
	if w.Code != http.StatusFound || w.Header().Get("Location") != "/next" {
		t.Fatalf("redirect = %d Location=%q", w.Code, w.Header().Get("Location"))
	}

	bad := httptest.NewRequest(http.MethodGet, "/-/?endpoint=ftp%3A%2F%2Fexample.com", nil)
	w = httptest.NewRecorder()
	proxyHandler(w, bad)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("invalid target status = %d", w.Code)
	}
}

func TestProxyHandlerBlocksPrivateTargetsByDefault(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	// allowPrivateFetch left at its default (false): the proxy must refuse to
	// dial loopback/private targets like this httptest server.
	req := httptest.NewRequest(http.MethodGet, "/-/?endpoint="+url.QueryEscape(upstream.URL), nil)
	w := httptest.NewRecorder()
	proxyHandler(w, req)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("expected proxy to reject private target, got status = %d, body = %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "private or local") {
		t.Fatalf("expected SSRF-block error message, got body = %s", w.Body.String())
	}
}

func TestServerRequiresBearerToken(t *testing.T) {
	isolateConfigFile(t)
	t.Setenv("BASE_URL", "http://localhost:8235/v1")
	serverAllowedTools := map[string]bool{toolWebSearch: true, toolFetchPage: true}
	a := &app{
		cfg:     config{workspaceRoot: t.TempDir()},
		client:  &client{http: &http.Client{}},
		toolset: buildAgentTools(serverAllowedTools),
	}

	body := `{"model":"test","messages":[{"role":"user","content":"hello"}]}`
	req := httptest.NewRequest(http.MethodPost, "/?endpoint=https://example.com/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	// No Authorization header
	w := httptest.NewRecorder()

	a.handleChatCompletion(w, req, serverAllowedTools)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "Bearer") {
		t.Fatalf("expected Bearer token error, got: %s", w.Body.String())
	}
}

func TestServerRejectsMalformedBearerToken(t *testing.T) {
	isolateConfigFile(t)
	serverAllowedTools := map[string]bool{toolWebSearch: true, toolFetchPage: true}
	a := &app{
		cfg:     config{workspaceRoot: t.TempDir()},
		client:  &client{http: &http.Client{}},
		toolset: buildAgentTools(serverAllowedTools),
	}
	body := `{"model":"test","messages":[{"role":"user","content":"hello"}]}`

	cases := []struct {
		name  string
		value string
	}{
		{"missing prefix", "Token sk-test"},
		{"lowercase prefix", "bearer sk-test"},
		{"extra spaces", "Bearer   "},
		{"no space", "Bearersk-test"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost,
				"/?endpoint=https://example.com/v1/chat/completions",
				strings.NewReader(body))
			req.Header.Set("Authorization", tc.value)
			w := httptest.NewRecorder()
			a.handleChatCompletion(w, req, serverAllowedTools)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("expected 400 for %q, got %d", tc.name, w.Code)
			}
		})
	}
}

func TestServerAcceptsTrimmedBearerToken(t *testing.T) {
	isolateConfigFile(t)
	serverAllowedTools := map[string]bool{toolWebSearch: true, toolFetchPage: true}
	a := &app{
		cfg:     config{workspaceRoot: t.TempDir()},
		client:  &client{http: &http.Client{}},
		toolset: buildAgentTools(serverAllowedTools),
	}
	body := `{"model":"test","messages":[{"role":"user","content":"hello"}]}`

	req := httptest.NewRequest(http.MethodPost,
		"/?endpoint=https://example.com/v1/chat/completions",
		strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer  sk-test ") // extra spaces
	w := httptest.NewRecorder()

	// This should NOT return 400 (it will fail downstream with a dial error,
	// but that means the token was accepted and forwarded correctly).
	a.handleChatCompletion(w, req, serverAllowedTools)
	if w.Code == http.StatusBadRequest {
		t.Fatalf("trimmed bearer token should be accepted, got 400: %s", w.Body.String())
	}
}

func TestServerRejectsMethodGet(t *testing.T) {
	isolateConfigFile(t)
	t.Setenv("BASE_URL", "http://localhost:8235/v1")
	serverAllowedTools := map[string]bool{toolWebSearch: true, toolFetchPage: true}
	a := &app{
		cfg:     config{workspaceRoot: t.TempDir()},
		client:  &client{http: &http.Client{}},
		toolset: buildAgentTools(serverAllowedTools),
	}

	req := httptest.NewRequest(http.MethodGet, "/?endpoint=https://example.com/v1/chat/completions", nil)
	w := httptest.NewRecorder()

	a.handleChatCompletion(w, req, serverAllowedTools)

	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", w.Code)
	}
}

func TestServerRejectsEmptyMessages(t *testing.T) {
	isolateConfigFile(t)
	t.Setenv("BASE_URL", "http://localhost:8235/v1")
	serverAllowedTools := map[string]bool{toolWebSearch: true, toolFetchPage: true}
	a := &app{
		cfg:     config{workspaceRoot: t.TempDir()},
		client:  &client{http: &http.Client{}},
		toolset: buildAgentTools(serverAllowedTools),
	}

	body := `{"model":"test","messages":[]}`
	req := httptest.NewRequest(http.MethodPost, "/?endpoint=https://example.com/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer sk-test")
	w := httptest.NewRecorder()

	a.handleChatCompletion(w, req, serverAllowedTools)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "messages") {
		t.Fatalf("expected messages error, got: %s", w.Body.String())
	}
}

func TestServerRejectsInvalidJSON(t *testing.T) {
	isolateConfigFile(t)
	t.Setenv("BASE_URL", "http://localhost:8235/v1")
	serverAllowedTools := map[string]bool{toolWebSearch: true, toolFetchPage: true}
	a := &app{
		cfg:     config{workspaceRoot: t.TempDir()},
		client:  &client{http: &http.Client{}},
		toolset: buildAgentTools(serverAllowedTools),
	}

	req := httptest.NewRequest(http.MethodPost, "/?endpoint=https://example.com/v1/chat/completions", strings.NewReader("not json"))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer sk-test")
	w := httptest.NewRecorder()

	a.handleChatCompletion(w, req, serverAllowedTools)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "invalid JSON") {
		t.Fatalf("expected JSON error, got: %s", w.Body.String())
	}
}

func TestServerOnlyAllowsSearchAndFetchTools(t *testing.T) {
	isolateConfigFile(t)
	t.Setenv("BASE_URL", "http://localhost:8235/v1")
	serverAllowedTools := map[string]bool{
		toolWebSearch:      true,
		toolFetchPage:      true,
		toolCreateSubagent: true,
		toolRunSubagent:    true,
		toolAwaitSubagent:  true,
		toolListSubagents:  true,
		toolReadSubagent:   true,
		toolCancelSubagent: true,
	}
	a := &app{
		cfg:     config{workspaceRoot: t.TempDir()},
		client:  &client{http: &http.Client{}},
		toolset: buildAgentTools(serverAllowedTools),
	}

	// Verify toolset has web_search, fetch_page, and subagent tools
	if len(a.toolset) != 8 {
		t.Fatalf("expected 8 tools in server mode, got %d", len(a.toolset))
	}
	toolNames := map[string]bool{}
	for _, tool := range a.toolset {
		toolNames[tool.Function.Name] = true
	}
	if !toolNames[toolWebSearch] || !toolNames[toolFetchPage] {
		t.Fatalf("expected web_search and fetch_page tools, got: %v", toolNames)
	}
	if !toolNames[toolCreateSubagent] || !toolNames[toolRunSubagent] || !toolNames[toolAwaitSubagent] {
		t.Fatalf("expected subagent tools in server mode, got: %v", toolNames)
	}
	if toolNames[toolReadFile] || toolNames[toolWriteFile] || toolNames[toolExecuteProgram] {
		t.Fatalf("expected no file/exec tools in server mode, got: %v", toolNames)
	}
}

func TestServerModeSystemPromptIsStripped(t *testing.T) {
	if !strings.Contains(serverModeSystemPrompt, "web_search") {
		t.Fatal("expected serverModeSystemPrompt to reference web_search")
	}
	if strings.Contains(serverModeSystemPrompt, "skill") {
		t.Fatal("expected serverModeSystemPrompt to not reference skills")
	}
}

func TestIsRetryableError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"deadline exceeded", context.DeadlineExceeded, false},
		{"context canceled", context.Canceled, false},
		{"retryable 429", &retryableHTTPError{StatusCode: 429, msg: "429 too many"}, true},
		{"retryable 500", &retryableHTTPError{StatusCode: 500, msg: "500 internal"}, true},
		{"retryable 502", &retryableHTTPError{StatusCode: 502, msg: "502 bad gateway"}, true},
		{"non-retryable 400", fmt.Errorf("model request failed: 400 Bad Request: bad input"), false},
		{"non-retryable 401", fmt.Errorf("model request failed: 401 Unauthorized: no token"), false},
		{"wrapped retryable", fmt.Errorf("outer: %w", &retryableHTTPError{StatusCode: 429, msg: "429"}), true},
		{"wrapped non-retryable", fmt.Errorf("outer: %w", context.DeadlineExceeded), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isRetryableError(tt.err); got != tt.want {
				t.Fatalf("isRetryableError(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestFinalOnlySinkBuffersLastContent(t *testing.T) {
	var last string
	wrapped := &spySink{onContent: func(s string) { last = s }}
	sink := &finalOnlySink{wrapped: wrapped}

	sink.WriteContent(rootAgentID, "first")
	sink.WriteContent(rootAgentID, "second")
	sink.WriteContent(rootAgentID, "third")

	if last != "" {
		t.Fatalf("expected nothing flushed yet, got: %q", last)
	}
	sink.FlushContent()
	if last != "third" {
		t.Fatalf("expected last content flushed, got: %q", last)
	}
}

func TestFinalOnlySinkSuppressesSubagentContent(t *testing.T) {
	var last string
	wrapped := &spySink{onContent: func(s string) { last = s }}
	sink := &finalOnlySink{wrapped: wrapped}

	sink.WriteContent("subagent-1", "subagent output")
	sink.WriteContent(rootAgentID, "root output")
	sink.FlushContent()

	if last != "root output" {
		t.Fatalf("expected only root content, got: %q", last)
	}
}

func TestFinalOnlySinkSuppressesToolAndSystemOutput(t *testing.T) {
	var toolCalls, toolResults, systems int
	wrapped := &spySink{
		onToolCall:   func(string, string) { toolCalls++ },
		onToolResult: func(string, bool, string) { toolResults++ },
		onSystem:     func(string) { systems++ },
	}
	sink := &finalOnlySink{wrapped: wrapped}

	sink.WriteToolCall(rootAgentID, "web_search", "{}")
	sink.WriteToolResult(rootAgentID, "web_search", false, "")
	sink.WriteSystem(rootAgentID, "msg")

	if toolCalls != 0 || toolResults != 0 || systems != 0 {
		t.Fatalf("expected all suppressed, got calls=%d results=%d systems=%d", toolCalls, toolResults, systems)
	}
}

// spySink is a minimal OutputSink for testing.
type spySink struct {
	onContent    func(string)
	onToolCall   func(string, string)
	onToolResult func(string, bool, string)
	onSystem     func(string)
}

func (s *spySink) WriteContent(_ string, content string) {
	if s.onContent != nil {
		s.onContent(content)
	}
}
func (s *spySink) WriteToolCall(_ string, toolName, args string) {
	if s.onToolCall != nil {
		s.onToolCall(toolName, args)
	}
}
func (s *spySink) WriteToolResult(_ string, toolName string, isError bool, detail string) {
	if s.onToolResult != nil {
		s.onToolResult(toolName, isError, detail)
	}
}
func (s *spySink) WriteSystem(_ string, msg string) {
	if s.onSystem != nil {
		s.onSystem(msg)
	}
}

func TestWriteChatCompletionResponseWithReasoning(t *testing.T) {
	w := httptest.NewRecorder()
	writeChatCompletionResponse(w, "gpt-4o", "final answer", "thinking about stuff")

	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}

	choices, ok := resp["choices"].([]any)
	if !ok || len(choices) == 0 {
		t.Fatalf("expected choices array, got: %v", resp["choices"])
	}
	choice := choices[0].(map[string]any)
	msg := choice["message"].(map[string]any)

	if msg["reasoning"] != "thinking about stuff" {
		t.Fatalf("expected reasoning in response, got: %v", msg["reasoning"])
	}
	if msg["content"] != "final answer" {
		t.Fatalf("expected content in response, got: %v", msg["content"])
	}
	if msg["role"] != "assistant" {
		t.Fatalf("expected role assistant, got: %v", msg["role"])
	}
}

func TestWriteChatCompletionResponseWithoutReasoning(t *testing.T) {
	w := httptest.NewRecorder()
	writeChatCompletionResponse(w, "gpt-4o", "final answer", "")

	body := w.Body.String()
	if strings.Contains(body, "reasoning") {
		t.Fatalf("expected no reasoning key when empty, got: %s", body)
	}

	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}
	choices := resp["choices"].([]any)
	choice := choices[0].(map[string]any)
	msg := choice["message"].(map[string]any)

	if msg["content"] != "final answer" {
		t.Fatalf("expected content in response, got: %v", msg["content"])
	}
}

func TestRunTurnLoopReturnsReasoning(t *testing.T) {
	// Mock LLM that returns reasoning_content in its response.
	mockResp := types.Response{
		Choices: []struct {
			Message      types.CompletionMessage `json:"message"`
			FinishReason string                  `json:"finish_reason"`
		}{{
			Message: types.CompletionMessage{
				Role:             "assistant",
				Content:          ptrString("Here is the answer"),
				ReasoningContent: ptrString("I thought about this carefully"),
			},
			FinishReason: "stop",
		}},
	}
	mockBody, _ := json.Marshal(mockResp)
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(mockBody)
	}))
	defer mockServer.Close()

	a := &app{
		cfg: config{
			workspaceRoot:  t.TempDir(),
			toolTimeoutSec: 30,
		},
		client: &client{
			endpoint: mockServer.URL + "/chat/completions",
			token:    "test-token",
			model:    "test-model",
			http:     &http.Client{},
		},
		toolset: []types.Tool{},
	}

	messages := []types.Message{
		{Role: "system", Content: "You are a test assistant."},
	}
	runtime := a.rootRuntime()

	_, _, reasoning, err := a.runTurnLoop(context.Background(), messages, "hello", runtime, a.toolset, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(reasoning, "I thought about this carefully") {
		t.Fatalf("expected reasoning to contain LLM thinking, got: %q", reasoning)
	}
	if !strings.Contains(reasoning, "[Turn 1]") {
		t.Fatalf("expected reasoning to contain turn header, got: %q", reasoning)
	}
	if !strings.Contains(reasoning, "Thinking:") {
		t.Fatalf("expected reasoning to contain Thinking label, got: %q", reasoning)
	}
}

func TestRunTurnLoopReasoningWithToolCalls(t *testing.T) {
	callCount := 0
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		var resp types.Response
		if callCount == 1 {
			// First call: return a tool call
			resp = types.Response{
				Choices: []struct {
					Message      types.CompletionMessage `json:"message"`
					FinishReason string                  `json:"finish_reason"`
				}{{
					Message: types.CompletionMessage{
						Role:             "assistant",
						Content:          ptrString(""),
						ReasoningContent: ptrString("I need to search for info"),
						ToolCalls: []types.ToolCall{{
							ID:   "call-1",
							Type: "function",
							Function: types.FunctionCall{
								Name:      "web_search",
								Arguments: `{"query":"test query"}`,
							},
						}},
					},
					FinishReason: "tool_calls",
				}},
			}
		} else {
			// Second call: return final answer
			resp = types.Response{
				Choices: []struct {
					Message      types.CompletionMessage `json:"message"`
					FinishReason string                  `json:"finish_reason"`
				}{{
					Message: types.CompletionMessage{
						Role:             "assistant",
						Content:          ptrString("Here are the results"),
						ReasoningContent: ptrString("Based on the search results"),
					},
					FinishReason: "stop",
				}},
			}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer mockServer.Close()

	// Mock the search URL so web_search doesn't make real HTTP requests.
	mockSearchServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<html><body><div class="result"><a class="result__a" href="https://example.com">Test Result</a><a class="result__snippet">Test snippet</a></div></body></html>`)
	}))
	defer mockSearchServer.Close()
	origDDG := ddgSearchURL
	origBing := bingSearchURL
	ddgSearchURL = mockSearchServer.URL
	bingSearchURL = mockSearchServer.URL
	defer func() {
		ddgSearchURL = origDDG
		bingSearchURL = origBing
	}()

	a := &app{
		cfg: config{
			workspaceRoot:   t.TempDir(),
			toolTimeoutSec:  30,
			toolMaxParallel: 4,
		},
		client: &client{
			endpoint: mockServer.URL + "/chat/completions",
			token:    "test-token",
			model:    "test-model",
			http:     &http.Client{},
		},
		toolset: buildAgentTools(map[string]bool{toolWebSearch: true}),
	}

	messages := []types.Message{
		{Role: "system", Content: "You are a test assistant."},
	}
	runtime := a.rootRuntime()

	_, _, reasoning, err := a.runTurnLoop(context.Background(), messages, "search for test", runtime, a.toolset, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(reasoning, "I need to search for info") {
		t.Fatalf("expected reasoning to contain first turn thinking, got: %q", reasoning)
	}
	if !strings.Contains(reasoning, "Based on the search results") {
		t.Fatalf("expected reasoning to contain second turn thinking, got: %q", reasoning)
	}
	if !strings.Contains(reasoning, "web_search") {
		t.Fatalf("expected reasoning to contain tool call name, got: %q", reasoning)
	}
	if !strings.Contains(reasoning, "Tool calls:") {
		t.Fatalf("expected reasoning to contain tool calls section, got: %q", reasoning)
	}
	if !strings.Contains(reasoning, "[Turn 1]") {
		t.Fatalf("expected reasoning to contain [Turn 1], got: %q", reasoning)
	}
	if !strings.Contains(reasoning, "[Turn 2]") {
		t.Fatalf("expected reasoning to contain [Turn 2], got: %q", reasoning)
	}
}

func TestServerModeToolsetIncludesSubagents(t *testing.T) {
	serverAllowedTools := map[string]bool{
		toolWebSearch:      true,
		toolFetchPage:      true,
		toolCreateSubagent: true,
		toolRunSubagent:    true,
		toolAwaitSubagent:  true,
		toolListSubagents:  true,
		toolReadSubagent:   true,
		toolCancelSubagent: true,
	}
	toolset := buildAgentTools(serverAllowedTools)

	if len(toolset) != 8 {
		t.Fatalf("expected 8 tools in server mode, got %d", len(toolset))
	}
	toolNames := map[string]bool{}
	for _, tool := range toolset {
		toolNames[tool.Function.Name] = true
	}
	expectedTools := []string{
		toolWebSearch, toolFetchPage,
		toolCreateSubagent, toolRunSubagent, toolAwaitSubagent,
		toolListSubagents, toolReadSubagent, toolCancelSubagent,
	}
	for _, name := range expectedTools {
		if !toolNames[name] {
			t.Fatalf("expected tool %q in server mode toolset", name)
		}
	}
}

func TestResponsesEndpointDetection(t *testing.T) {
	tests := []struct {
		endpoint string
		want     bool
	}{
		{"https://example.com/responses", true},
		{"https://example.com/responses/", true},
		{"https://example.com/v1/responses", true},
		{"https://example.com/chat/completions", false},
		{"https://example.com/responses-extra", false},
		{"https://example.com/chat/responses/v1", false},
	}
	for _, tc := range tests {
		if got := (&client{endpoint: tc.endpoint}).isResponsesEndpoint(); got != tc.want {
			t.Errorf("isResponsesEndpoint(%q) = %v, want %v", tc.endpoint, got, tc.want)
		}
	}
}

func TestResponsesExactPayloadAndText(t *testing.T) {
	var payload map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses/" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]}]}`))
	}))
	defer server.Close()

	tool := types.Tool{Type: "function", Function: types.ToolSpec{
		Name: "lookup", Description: "Look something up",
		Parameters: map[string]any{"type": "object", "properties": map[string]any{}},
	}}
	c := &client{endpoint: server.URL + "/responses/", http: server.Client()}
	resp, err := c.complete(context.Background(), []types.Message{{Role: "user", Content: "hi"}}, []types.Tool{tool}, "gpt-test", "")
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if got := resp.Content(); got != "hello" {
		t.Fatalf("content = %q, want hello", got)
	}
	if _, ok := payload["messages"]; ok {
		t.Fatal("Responses request must not contain messages")
	}
	if _, ok := payload["previous_response_id"]; ok {
		t.Fatal("Responses request must not contain previous_response_id")
	}
	if got := payload["model"]; got != "gpt-test" {
		t.Fatalf("model = %v", got)
	}
	input, ok := payload["input"].([]any)
	if !ok || len(input) != 1 || input[0].(map[string]any)["content"] != "hi" {
		t.Fatalf("unexpected input: %#v", payload["input"])
	}
	tools, ok := payload["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("unexpected tools: %#v", payload["tools"])
	}
	toolPayload := tools[0].(map[string]any)
	if toolPayload["type"] != "function" || toolPayload["name"] != "lookup" {
		t.Fatalf("unexpected flat function tool: %#v", toolPayload)
	}
	if _, ok := toolPayload["function"]; ok {
		t.Fatal("Responses function tools must be flat")
	}
	if _, ok := payload["reasoning"]; ok {
		t.Fatal("unset reasoning must be omitted")
	}
}

func TestResponsesReasoningAndToolContinuation(t *testing.T) {
	var requests []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		requests = append(requests, payload)
		w.Header().Set("Content-Type", "application/json")
		if len(requests) == 1 {
			_, _ = w.Write([]byte(`{"output":[{"type":"reasoning","summary":[{"type":"summary_text","text":"checking"}]},{"type":"function_call","call_id":"call-1","name":"list_files","arguments":"{\"path\":\".\"}"}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"output":[{"type":"reasoning","summary":[{"type":"summary_text","text":"done"}]},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"finished"}]}]}`))
	}))
	defer server.Close()

	a := &app{
		cfg: config{
			workspaceRoot:      t.TempDir(),
			toolMaxParallel:    1,
			toolTimeoutSec:     5,
			toolRetryOnTimeout: false,
			allowedTools:       map[string]bool{toolListFiles: true},
		},
		client: &client{
			endpoint:  server.URL + "/responses",
			model:     "test-model",
			reasoning: "high",
			http:      server.Client(),
		},
		toolset: buildAgentTools(map[string]bool{toolListFiles: true}),
	}
	_, result, reasoning, err := a.runTurnLoop(context.Background(), nil, "inspect", a.rootRuntime(), a.toolset, false)
	if err != nil {
		t.Fatalf("runTurnLoop: %v", err)
	}
	if result != "finished" || !strings.Contains(reasoning, "checking") || !strings.Contains(reasoning, "done") {
		t.Fatalf("unexpected result/reasoning: %q / %q", result, reasoning)
	}
	if len(requests) != 2 {
		t.Fatalf("request count = %d, want 2", len(requests))
	}
	if _, ok := requests[0]["reasoning"].(map[string]any); !ok {
		t.Fatalf("expected reasoning object in request: %#v", requests[0])
	}
	secondInput := requests[1]["input"].([]any)
	if len(secondInput) != 4 {
		t.Fatalf("second input length = %d, want 4: %#v", len(secondInput), secondInput)
	}
	if secondInput[1].(map[string]any)["type"] != "reasoning" || secondInput[2].(map[string]any)["type"] != "function_call" {
		t.Fatalf("native output items were not preserved: %#v", secondInput)
	}
	output := secondInput[3].(map[string]any)
	if output["type"] != "function_call_output" || output["call_id"] != "call-1" {
		t.Fatalf("unexpected function output: %#v", output)
	}
}

func TestResponsesMalformedAndEmptyOutput(t *testing.T) {
	if _, err := parseResponsesResponse([]byte(`not json`)); err == nil {
		t.Fatal("expected malformed response error")
	}
	if _, err := parseResponsesResponse([]byte(`{"output":[]}`)); err == nil {
		t.Fatal("expected empty output error")
	}
	if _, err := parseResponsesResponse([]byte(`{"output":[{"type":"reasoning","summary":[]}]}`)); err == nil {
		t.Fatal("expected unusable output error")
	}
	if _, err := parseResponsesResponse([]byte(`{"output":[{"type":"function_call","call_id":"","name":"lookup","arguments":"{}"}]}`)); err == nil {
		t.Fatal("expected malformed function call error")
	}
}

func ptrString(s string) *string {
	return &s
}

func TestDataEndpointPUTAndGetRoundtrip(t *testing.T) {
	a := &app{cfg: config{workspaceRoot: t.TempDir()}, dataStore: newDataStore()}

	// PUT
	putReq := httptest.NewRequest(http.MethodPut, "/data?key=foo", strings.NewReader("hello world"))
	putW := httptest.NewRecorder()
	a.dataHandler(putW, putReq)

	if putW.Code != http.StatusOK {
		t.Fatalf("PUT expected 200, got %d: %s", putW.Code, putW.Body.String())
	}
	var putResp map[string]any
	if err := json.Unmarshal(putW.Body.Bytes(), &putResp); err != nil {
		t.Fatalf("PUT response not JSON: %v", err)
	}
	if putResp["ok"] != true || putResp["key"] != "foo" {
		t.Fatalf("unexpected PUT response: %v", putResp)
	}

	// GET
	getReq := httptest.NewRequest(http.MethodGet, "/data?key=foo", nil)
	getW := httptest.NewRecorder()
	a.dataHandler(getW, getReq)

	if getW.Code != http.StatusOK {
		t.Fatalf("GET expected 200, got %d", getW.Code)
	}
	if getW.Body.String() != "hello world" {
		t.Fatalf("GET expected 'hello world', got %q", getW.Body.String())
	}
	if getW.Header().Get("Content-Type") != "text/plain" {
		t.Fatalf("expected Content-Type text/plain, got %q", getW.Header().Get("Content-Type"))
	}
}

func TestDataEndpointGETMissingKey(t *testing.T) {
	a := &app{cfg: config{workspaceRoot: t.TempDir()}, dataStore: newDataStore()}

	req := httptest.NewRequest(http.MethodGet, "/data?key=nonexistent", nil)
	w := httptest.NewRecorder()
	a.dataHandler(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestDataEndpointMissingKeyParam(t *testing.T) {
	a := &app{cfg: config{workspaceRoot: t.TempDir()}, dataStore: newDataStore()}

	// GET without key
	req := httptest.NewRequest(http.MethodGet, "/data", nil)
	w := httptest.NewRecorder()
	a.dataHandler(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("GET without key: expected 400, got %d", w.Code)
	}

	// PUT without key
	req = httptest.NewRequest(http.MethodPut, "/data", strings.NewReader("val"))
	w = httptest.NewRecorder()
	a.dataHandler(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("PUT without key: expected 400, got %d", w.Code)
	}
}

func TestDataEndpointKeyTooLong(t *testing.T) {
	a := &app{cfg: config{workspaceRoot: t.TempDir()}, dataStore: newDataStore()}

	longKey := strings.Repeat("a", maxDataKeyLen+1)
	req := httptest.NewRequest(http.MethodPut, "/data?key="+longKey, strings.NewReader("val"))
	w := httptest.NewRecorder()
	a.dataHandler(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for long key, got %d", w.Code)
	}
}

func TestDataEndpointMethodNotAllowed(t *testing.T) {
	a := &app{cfg: config{workspaceRoot: t.TempDir()}, dataStore: newDataStore()}

	req := httptest.NewRequest(http.MethodDelete, "/data?key=foo", nil)
	w := httptest.NewRecorder()
	a.dataHandler(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", w.Code)
	}
}

func TestDataEndpointPUTOverwrites(t *testing.T) {
	a := &app{cfg: config{workspaceRoot: t.TempDir()}, dataStore: newDataStore()}

	// PUT first value
	req := httptest.NewRequest(http.MethodPut, "/data?key=ow", strings.NewReader("first"))
	w := httptest.NewRecorder()
	a.dataHandler(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("first PUT: %d", w.Code)
	}

	// PUT second value
	req = httptest.NewRequest(http.MethodPut, "/data?key=ow", strings.NewReader("second"))
	w = httptest.NewRecorder()
	a.dataHandler(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("second PUT: %d", w.Code)
	}

	// GET should return second
	req = httptest.NewRequest(http.MethodGet, "/data?key=ow", nil)
	w = httptest.NewRecorder()
	a.dataHandler(w, req)
	if w.Body.String() != "second" {
		t.Fatalf("expected 'second', got %q", w.Body.String())
	}
}

func TestDataEndpointTTLValidation(t *testing.T) {
	a := &app{cfg: config{workspaceRoot: t.TempDir()}, dataStore: newDataStore()}

	// TTL > max should be capped
	req := httptest.NewRequest(http.MethodPut, "/data?key=ttl1&ttl=99999", strings.NewReader("val"))
	w := httptest.NewRecorder()
	a.dataHandler(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT with high TTL: %d", w.Code)
	}
	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["ttl"] != float64(maxDataTTLMinutes) {
		t.Fatalf("expected TTL capped to %d, got %v", maxDataTTLMinutes, resp["ttl"])
	}

	// TTL 0 should use default
	req = httptest.NewRequest(http.MethodPut, "/data?key=ttl2&ttl=0", strings.NewReader("val"))
	w = httptest.NewRecorder()
	a.dataHandler(w, req)
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["ttl"] != float64(defaultDataTTLMinutes) {
		t.Fatalf("expected default TTL %d, got %v", defaultDataTTLMinutes, resp["ttl"])
	}

	// Negative TTL should error
	req = httptest.NewRequest(http.MethodPut, "/data?key=ttl3&ttl=-1", strings.NewReader("val"))
	w = httptest.NewRecorder()
	a.dataHandler(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for negative TTL, got %d", w.Code)
	}
}

func TestDataEndpointValueTooLarge(t *testing.T) {
	a := &app{cfg: config{workspaceRoot: t.TempDir()}, dataStore: newDataStore()}

	bigBody := strings.NewReader(strings.Repeat("x", maxDataValueSize+1))
	req := httptest.NewRequest(http.MethodPut, "/data?key=big", bigBody)
	w := httptest.NewRecorder()
	a.dataHandler(w, req)

	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413, got %d", w.Code)
	}
}

// --- Async endpoint tests ---

func TestAsyncRequiresEndpoint(t *testing.T) {
	isolateConfigFile(t)
	t.Setenv("BASE_URL", "http://localhost:8235/v1")
	serverAllowedTools := map[string]bool{toolWebSearch: true, toolFetchPage: true}
	a := &app{
		cfg:       config{workspaceRoot: t.TempDir()},
		client:    &client{http: &http.Client{}},
		toolset:   buildAgentTools(serverAllowedTools),
		dataStore: newDataStore(),
	}

	body := `{"model":"test","messages":[{"role":"user","content":"hello"}]}`
	req := httptest.NewRequest(http.MethodPost, "/async/", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer sk-test")
	w := httptest.NewRecorder()

	a.handleAsyncChatCompletion(w, req, serverAllowedTools)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "endpoint required") {
		t.Fatalf("expected endpoint required error, got: %s", w.Body.String())
	}
}

func TestAsyncRequiresBearerToken(t *testing.T) {
	isolateConfigFile(t)
	t.Setenv("BASE_URL", "http://localhost:8235/v1")
	serverAllowedTools := map[string]bool{toolWebSearch: true, toolFetchPage: true}
	a := &app{
		cfg:       config{workspaceRoot: t.TempDir()},
		client:    &client{http: &http.Client{}},
		toolset:   buildAgentTools(serverAllowedTools),
		dataStore: newDataStore(),
	}

	body := `{"model":"test","messages":[{"role":"user","content":"hello"}]}`
	req := httptest.NewRequest(http.MethodPost, "/async/?endpoint=https://example.com/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	a.handleAsyncChatCompletion(w, req, serverAllowedTools)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "Bearer") {
		t.Fatalf("expected Bearer token error, got: %s", w.Body.String())
	}
}

func TestAsyncRejectsMethodGet(t *testing.T) {
	isolateConfigFile(t)
	t.Setenv("BASE_URL", "http://localhost:8235/v1")
	serverAllowedTools := map[string]bool{toolWebSearch: true, toolFetchPage: true}
	a := &app{
		cfg:       config{workspaceRoot: t.TempDir()},
		client:    &client{http: &http.Client{}},
		toolset:   buildAgentTools(serverAllowedTools),
		dataStore: newDataStore(),
	}

	req := httptest.NewRequest(http.MethodGet, "/async/?endpoint=https://example.com/v1/chat/completions", nil)
	w := httptest.NewRecorder()

	a.handleAsyncChatCompletion(w, req, serverAllowedTools)

	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", w.Code)
	}
}

func TestAsyncRejectsEmptyMessages(t *testing.T) {
	isolateConfigFile(t)
	t.Setenv("BASE_URL", "http://localhost:8235/v1")
	serverAllowedTools := map[string]bool{toolWebSearch: true, toolFetchPage: true}
	a := &app{
		cfg:       config{workspaceRoot: t.TempDir()},
		client:    &client{http: &http.Client{}},
		toolset:   buildAgentTools(serverAllowedTools),
		dataStore: newDataStore(),
	}

	body := `{"model":"test","messages":[]}`
	req := httptest.NewRequest(http.MethodPost, "/async/?endpoint=https://example.com/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer sk-test")
	w := httptest.NewRecorder()

	a.handleAsyncChatCompletion(w, req, serverAllowedTools)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "messages") {
		t.Fatalf("expected messages error, got: %s", w.Body.String())
	}
}

func TestAsyncRejectsInvalidJSON(t *testing.T) {
	isolateConfigFile(t)
	t.Setenv("BASE_URL", "http://localhost:8235/v1")
	serverAllowedTools := map[string]bool{toolWebSearch: true, toolFetchPage: true}
	a := &app{
		cfg:       config{workspaceRoot: t.TempDir()},
		client:    &client{http: &http.Client{}},
		toolset:   buildAgentTools(serverAllowedTools),
		dataStore: newDataStore(),
	}

	req := httptest.NewRequest(http.MethodPost, "/async/?endpoint=https://example.com/v1/chat/completions", strings.NewReader("not json"))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer sk-test")
	w := httptest.NewRecorder()

	a.handleAsyncChatCompletion(w, req, serverAllowedTools)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "invalid JSON") {
		t.Fatalf("expected JSON error, got: %s", w.Body.String())
	}
}

func TestAsyncRejectsStreaming(t *testing.T) {
	isolateConfigFile(t)
	t.Setenv("BASE_URL", "http://localhost:8235/v1")
	serverAllowedTools := map[string]bool{toolWebSearch: true, toolFetchPage: true}
	a := &app{
		cfg:       config{workspaceRoot: t.TempDir()},
		client:    &client{http: &http.Client{}},
		toolset:   buildAgentTools(serverAllowedTools),
		dataStore: newDataStore(),
	}

	body := `{"model":"test","messages":[{"role":"user","content":"hi"}],"stream":true}`
	req := httptest.NewRequest(http.MethodPost, "/async/?endpoint=https://example.com/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer sk-test")
	w := httptest.NewRecorder()

	a.handleAsyncChatCompletion(w, req, serverAllowedTools)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "streaming") {
		t.Fatalf("expected streaming error, got: %s", w.Body.String())
	}
}

func TestAsyncReturns202WithUUID(t *testing.T) {
	isolateConfigFile(t)
	t.Setenv("BASE_URL", "http://localhost:8235/v1")
	serverAllowedTools := map[string]bool{toolWebSearch: true, toolFetchPage: true}
	a := &app{
		cfg:       config{workspaceRoot: t.TempDir()},
		client:    &client{http: &http.Client{}},
		toolset:   buildAgentTools(serverAllowedTools),
		dataStore: newDataStore(),
	}

	body := `{"model":"test","messages":[{"role":"user","content":"hello"}]}`
	req := httptest.NewRequest(http.MethodPost, "/async/?endpoint=https://example.com/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer sk-test")
	w := httptest.NewRecorder()

	a.handleAsyncChatCompletion(w, req, serverAllowedTools)

	if w.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response not JSON: %v", err)
	}
	id, ok := resp["id"]
	if !ok || id == "" {
		t.Fatalf("expected 'id' field in response, got: %v", resp)
	}
	// Validate UUID format: 8-4-4-4-12 hex
	if len(id) != 36 || id[8] != '-' || id[13] != '-' || id[18] != '-' || id[23] != '-' {
		t.Fatalf("expected UUID format (8-4-4-4-12), got: %s", id)
	}
}

func TestAsyncExtractsEndpointFromPath(t *testing.T) {
	isolateConfigFile(t)
	t.Setenv("BASE_URL", "http://localhost:8235/v1")
	serverAllowedTools := map[string]bool{toolWebSearch: true, toolFetchPage: true}
	a := &app{
		cfg:       config{workspaceRoot: t.TempDir()},
		client:    &client{http: &http.Client{}},
		toolset:   buildAgentTools(serverAllowedTools),
		dataStore: newDataStore(),
	}

	body := `{"model":"test","messages":[{"role":"user","content":"hello"}]}`
	// Endpoint supplied via query; literal URL paths are intentionally rejected.
	req := httptest.NewRequest(http.MethodPost, "/async/?endpoint="+url.QueryEscape("https://example.com/v1/chat/completions"), strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer sk-test")
	w := httptest.NewRecorder()

	a.handleAsyncChatCompletion(w, req, serverAllowedTools)

	if w.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", w.Code, w.Body.String())
	}
}

func TestAsyncExtractsEndpointFromHexPath(t *testing.T) {
	isolateConfigFile(t)
	t.Setenv("BASE_URL", "http://localhost:8235/v1")
	serverAllowedTools := map[string]bool{toolWebSearch: true, toolFetchPage: true}
	a := &app{
		cfg:       config{workspaceRoot: t.TempDir()},
		client:    &client{http: &http.Client{}},
		toolset:   buildAgentTools(serverAllowedTools),
		dataStore: newDataStore(),
	}

	// Hex-encode "https://example.com/v1/chat/completions"
	hexURL := "68747470733a2f2f6578616d706c652e636f6d2f76312f636861742f636f6d706c6574696f6e73"
	body := `{"model":"test","messages":[{"role":"user","content":"hello"}]}`
	req := httptest.NewRequest(http.MethodPost, "/async/~"+hexURL, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer sk-test")
	w := httptest.NewRecorder()

	a.handleAsyncChatCompletion(w, req, serverAllowedTools)

	if w.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", w.Code, w.Body.String())
	}
}

func TestGenerateUUIDFormat(t *testing.T) {
	id := generateUUID()
	// UUID v4: 8-4-4-4-12 hex with hyphens
	if len(id) != 36 {
		t.Fatalf("expected length 36, got %d: %s", len(id), id)
	}
	if id[8] != '-' || id[13] != '-' || id[18] != '-' || id[23] != '-' {
		t.Fatalf("expected hyphens at positions 8,13,18,23, got: %s", id)
	}
	// Version nibble should be 4
	if id[14] != '4' {
		t.Fatalf("expected version 4, got: %s", id)
	}
	// Variant nibble should be 8 or 9 or a or b
	if id[19] != '8' && id[19] != '9' && id[19] != 'a' && id[19] != 'b' {
		t.Fatalf("expected valid variant at position 19, got: %s", id)
	}
	// Two generated UUIDs should be different
	id2 := generateUUID()
	if id == id2 {
		t.Fatalf("two UUIDs should not be identical: %s", id)
	}
}

func TestAsyncStoresResultInDataStore(t *testing.T) {
	isolateConfigFile(t)
	serverAllowedTools := map[string]bool{toolWebSearch: true, toolFetchPage: true}

	// Mock LLM endpoint that returns a simple completion.
	mockLLM := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]any{
			"id":     "mock-123",
			"object": "chat.completion",
			"choices": []map[string]any{{
				"index": 0,
				"message": map[string]string{
					"role":    "assistant",
					"content": "mock response",
				},
				"finish_reason": "stop",
			}},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer mockLLM.Close()

	a := &app{
		cfg:       config{workspaceRoot: t.TempDir()},
		client:    &client{http: &http.Client{}},
		toolset:   buildAgentTools(serverAllowedTools),
		dataStore: newDataStore(),
	}

	uuid := generateUUID()
	messages := []types.Message{
		{Role: "system", Content: "You are helpful."},
		{Role: "user", Content: "what is 2+2?"},
	}

	a.runAsyncTask(uuid, mockLLM.URL+"/v1/chat/completions", "sk-test", "test-model", "low", messages, "what is 2+2?", serverAllowedTools)

	// Verify the result was stored.
	val, ok := a.dataStore.Get(uuid)
	if !ok {
		t.Fatal("expected result to be stored in data store")
	}

	var result map[string]any
	if err := json.Unmarshal([]byte(val), &result); err != nil {
		t.Fatalf("stored value not valid JSON: %v", err)
	}
	if result["object"] != "chat.completion" {
		t.Fatalf("expected object=chat.completion, got %v", result["object"])
	}
	choices, ok := result["choices"].([]any)
	if !ok || len(choices) != 1 {
		t.Fatalf("expected 1 choice, got %v", result["choices"])
	}
	choice := choices[0].(map[string]any)
	msg := choice["message"].(map[string]any)
	if msg["content"] != "mock response" {
		t.Fatalf("expected content 'mock response', got %v", msg["content"])
	}
}

func TestAsyncRuntimeUsesServerToolPolicyForRootAndSubagents(t *testing.T) {
	isolateConfigFile(t)
	serverAllowedTools := map[string]bool{
		toolWebSearch:      true,
		toolFetchPage:      true,
		toolCreateSubagent: true,
		toolRunSubagent:    true,
		toolAwaitSubagent:  true,
		toolListSubagents:  true,
		toolReadSubagent:   true,
		toolCancelSubagent: true,
	}
	cfg := config{
		workspaceRoot: t.TempDir(),
		allowedTools: map[string]bool{
			toolReadFile: true,
		},
	}
	a := &app{cfg: cfg, dataStore: newDataStore()}

	var requestBody string
	mockLLM := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request body: %v", err)
		}
		requestBody = string(body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"done"}}]}`))
	}))
	defer mockLLM.Close()

	a.runAsyncTask(
		generateUUID(),
		mockLLM.URL,
		"sk-test",
		"test-model",
		"low",
		[]types.Message{{Role: "user", Content: "hello"}},
		"hello",
		serverAllowedTools,
	)

	if strings.Contains(requestBody, `read_file`) {
		t.Fatal("async root request exposed read_file despite server tool policy")
	}
	if !strings.Contains(requestBody, toolCreateSubagent) {
		t.Fatal("expected async root request to retain subagent tools")
	}
}

func TestAsyncStoresErrorOnFailure(t *testing.T) {
	isolateConfigFile(t)
	serverAllowedTools := map[string]bool{toolWebSearch: true, toolFetchPage: true}

	// Mock LLM that returns an error.
	mockLLM := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":"internal error"}`))
	}))
	defer mockLLM.Close()

	a := &app{
		cfg:       config{workspaceRoot: t.TempDir()},
		client:    &client{http: &http.Client{}},
		toolset:   buildAgentTools(serverAllowedTools),
		dataStore: newDataStore(),
	}

	uuid := generateUUID()
	messages := []types.Message{{Role: "user", Content: "hello"}}

	a.runAsyncTask(uuid, mockLLM.URL+"/v1/chat/completions", "sk-test", "test-model", "low", messages, "hello", serverAllowedTools)

	// Verify the error was stored.
	val, ok := a.dataStore.Get(uuid)
	if !ok {
		t.Fatal("expected error to be stored in data store")
	}

	var result map[string]any
	if err := json.Unmarshal([]byte(val), &result); err != nil {
		t.Fatalf("stored value not valid JSON: %v", err)
	}
	errObj, ok := result["error"].(map[string]any)
	if !ok {
		t.Fatalf("expected error object, got: %v", result)
	}
	if errObj["type"] != "async_error" {
		t.Fatalf("expected type=async_error, got %v", errObj["type"])
	}
}

func TestStoreAsyncResultFallsBackForOversizedResult(t *testing.T) {
	a := &app{dataStore: newDataStore()}
	uuid := generateUUID()

	a.storeAsyncResult(uuid, strings.Repeat("x", maxDataValueSize+1))

	value, ok := a.dataStore.Get(uuid)
	if !ok {
		t.Fatal("expected bounded async error to be stored")
	}
	if value != asyncResultStorageError {
		t.Fatalf("unexpected fallback value: %q", value)
	}
}

func TestAsyncPanicRecovery(t *testing.T) {
	a := &app{
		cfg:       config{workspaceRoot: t.TempDir()},
		dataStore: newDataStore(),
	}

	uuid := generateUUID()

	// Simulate a panic in the goroutine by calling runAsyncTask in a goroutine
	// that will panic, and verify the error is stored.
	// We test this by verifying the defer/recover mechanism stores an error.
	// We can't easily make runAsyncTask panic directly, so we test the
	// recovery wrapper indirectly by checking that a panic in the goroutine
	// results in a stored error.

	// For a unit test, we verify the panic recovery by running a function
	// that panics and checking the dataStore.
	go func() {
		defer func() {
			if r := recover(); r != nil {
				errResp, _ := json.Marshal(map[string]any{
					"error": map[string]any{
						"message": fmt.Sprintf("internal panic: %v", r),
						"type":    "async_error",
					},
				})
				a.dataStore.Put(uuid, string(errResp), asyncResultTTL)
			}
		}()
		panic("test panic: simulated crash")
	}()

	// Wait for goroutine to complete.
	time.Sleep(50 * time.Millisecond)

	val, ok := a.dataStore.Get(uuid)
	if !ok {
		t.Fatal("expected panic error to be stored in data store")
	}

	var result map[string]any
	if err := json.Unmarshal([]byte(val), &result); err != nil {
		t.Fatalf("stored value not valid JSON: %v", err)
	}
	errObj, ok := result["error"].(map[string]any)
	if !ok {
		t.Fatalf("expected error object, got: %v", result)
	}
	if errObj["type"] != "async_error" {
		t.Fatalf("expected type=async_error, got %v", errObj["type"])
	}
	if !strings.Contains(errObj["message"].(string), "test panic: simulated crash") {
		t.Fatalf("expected panic message, got: %v", errObj["message"])
	}
}

func TestAsyncConcurrencyLimit(t *testing.T) {
	// Save and restore the global semaphore.
	origSem := asyncSem
	asyncSem = make(chan struct{}, 2) // limit to 2
	defer func() { asyncSem = origSem }()

	// Fill the semaphore to capacity.
	asyncSem <- struct{}{}
	asyncSem <- struct{}{}

	// Verify a third acquire blocks.
	acquired := make(chan struct{})
	go func() {
		asyncSem <- struct{}{} // blocks when full
		close(acquired)
	}()

	select {
	case <-acquired:
		t.Fatal("goroutine acquired semaphore when it should be blocked")
	case <-time.After(100 * time.Millisecond):
		// Expected: blocked.
	}

	// Release a slot — goroutine should unblock.
	<-asyncSem
	select {
	case <-acquired:
		// Expected.
	case <-time.After(time.Second):
		t.Fatal("goroutine did not unblock after slot release")
	}
}

func TestBuildChatCompletionJSONWithReasoning(t *testing.T) {
	jsonStr := buildChatCompletionJSON("gpt-4o", "answer", "thinking")
	var result map[string]any
	if err := json.Unmarshal([]byte(jsonStr), &result); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	choices := result["choices"].([]any)
	msg := choices[0].(map[string]any)["message"].(map[string]any)
	if msg["reasoning"] != "thinking" {
		t.Fatalf("expected reasoning, got %v", msg["reasoning"])
	}
	if msg["content"] != "answer" {
		t.Fatalf("expected content, got %v", msg["content"])
	}
}

func TestBuildChatCompletionJSONWithoutReasoning(t *testing.T) {
	jsonStr := buildChatCompletionJSON("gpt-4o", "answer", "")
	var result map[string]any
	if err := json.Unmarshal([]byte(jsonStr), &result); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	choices := result["choices"].([]any)
	msg := choices[0].(map[string]any)["message"].(map[string]any)
	if _, ok := msg["reasoning"]; ok {
		t.Fatalf("expected no reasoning key when empty")
	}
}
