package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestLoadConfigDefaults(t *testing.T) {
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
	t.Setenv("BASE_URL", "http://localhost:8235/v1")
	_, err := loadConfig([]string{"--allow-tool", "rm_rf", "task"})
	if err == nil {
		t.Fatal("expected error for unknown tool")
	}
}

func TestLoadConfigRejectInteractiveFlag(t *testing.T) {
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
