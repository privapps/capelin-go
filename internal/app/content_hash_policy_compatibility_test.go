package app

import (
	"capelin-go/internal/contracts"
	"capelin-go/internal/server"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEditFileWithoutHashReturnsMigrationRetryError(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "notes.txt")
	initial := []byte("before")
	if err := os.WriteFile(path, initial, 0o600); err != nil {
		t.Fatal(err)
	}
	a := &app{cfg: config{
		workspaceRoot: root,
		allowedTools:  map[string]bool{toolEditFile: true},
	}}
	arguments, err := json.Marshal(map[string]string{
		"path": "notes.txt", "old_str": "before", "new_str": "after",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = a.runTool(context.Background(), contracts.ToolCall{
		Type:     "function",
		Function: contracts.FunctionCall{Name: toolEditFile, Arguments: string(arguments)},
	})
	if err == nil || !strings.Contains(err.Error(), "content_hash is required") || !strings.Contains(err.Error(), "reread") {
		t.Fatalf("missing-hash edit error = %v, want migration/retry guidance", err)
	}
	got, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(got) != string(initial) {
		t.Fatalf("missing-hash edit changed bytes to %q, want %q", got, initial)
	}
}

func TestServerExecutionPolicyExcludesLocalFileCapabilities(t *testing.T) {
	allowed := map[string]bool{
		toolWebSearch:      true,
		toolFetchPage:      true,
		toolCreateSubagent: true,
		toolRunSubagent:    true,
		toolAwaitSubagent:  true,
		toolListSubagents:  true,
		toolReadSubagent:   true,
		toolCancelSubagent: true,
	}
	a := &app{cfg: config{model: "default", workspaceRoot: t.TempDir(), allowedTools: map[string]bool{toolEditFile: true}}}
	serverApp, runtime := a.newServerExecutionApp(&server.ExecutionRequest{
		RemoteBase:   "https://remote.example/v1/chat/completions",
		RemoteToken:  "token",
		Model:        "model",
		AllowedTools: allowed,
	})

	for _, name := range []string{
		toolListFiles, toolReadFile, toolWriteFile, toolEditFile, toolAppendFile,
		toolExecuteProgram, toolExecuteSkill, toolListSkills, toolReadSkill,
	} {
		if runtime.allowedTools[name] {
			t.Fatalf("server runtime exposed local capability %q", name)
		}
	}
	for _, tool := range serverApp.toolset {
		for _, name := range []string{toolListFiles, toolReadFile, toolWriteFile, toolEditFile, toolAppendFile, toolExecuteProgram, toolExecuteSkill, toolListSkills, toolReadSkill} {
			if tool.Function.Name == name {
				t.Fatalf("server provider catalog exposed local capability %q", name)
			}
		}
	}
}
