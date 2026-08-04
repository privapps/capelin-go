package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// TestRunTurnLoopComposesProviderEngineAndApplicationTools exercises the
// production composition seam rather than a package-local fake adapter. The
// application supplies the tool catalog and runner, providers owns wire
// translation, and agent owns continuation policy.
func TestRunTurnLoopComposesProviderEngineAndApplicationTools(t *testing.T) {
	workspace := t.TempDir()
	marker := filepath.Join(workspace, "seam-marker.txt")
	if err := os.WriteFile(marker, []byte("composed seam output"), 0o600); err != nil {
		t.Fatal(err)
	}

	call := map[string]any{
		"id": "read-call", "type": "function",
		"function": map[string]any{
			"name":      toolReadFile,
			"arguments": fmt.Sprintf(`{"path":%q}`, filepath.Base(marker)),
		},
	}
	var mu sync.Mutex
	var requests []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		mu.Lock()
		requests = append(requests, payload)
		index := len(requests)
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		if index == 1 {
			_, _ = fmt.Fprint(w, chatTurnResponse("", "", []map[string]any{call}))
			return
		}
		_, _ = fmt.Fprint(w, chatTurnResponse("composed answer", "", nil))
	}))
	defer server.Close()

	a := &app{
		cfg: config{
			workspaceRoot:   workspace,
			maxIterations:   3,
			toolMaxParallel: 1,
			toolTimeoutSec:  5,
			allowedTools:    map[string]bool{toolReadFile: true},
		},
		client: &client{endpoint: server.URL + "/chat/completions", model: "test-model", http: server.Client()},
		sink:   &turnEventSink{},
	}
	a.toolset = buildAgentTools(a.cfg.allowedTools)

	_, answer, _, err := a.runTurnLoop(context.Background(), nil, "inspect the workspace", a.rootRuntime(), a.toolset, true)
	if err != nil {
		t.Fatal(err)
	}
	if answer != "composed answer" {
		t.Fatalf("answer=%q", answer)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(requests) != 2 {
		t.Fatalf("provider requests=%d, want 2", len(requests))
	}
	if !requestIncludesTool(requests[0], toolReadFile) {
		t.Fatalf("initial request did not receive the application catalog: %#v", requests[0])
	}
	if !requestIncludesToolResult(requests[1], "composed seam output") {
		t.Fatalf("continuation request did not receive the application tool result: %#v", requests[1])
	}
}

func requestIncludesTool(request map[string]any, name string) bool {
	tools, _ := request["tools"].([]any)
	for _, raw := range tools {
		tool, _ := raw.(map[string]any)
		function, _ := tool["function"].(map[string]any)
		if function["name"] == name {
			return true
		}
	}
	return false
}

func requestIncludesToolResult(request map[string]any, want string) bool {
	messages, _ := request["messages"].([]any)
	for _, raw := range messages {
		message, _ := raw.(map[string]any)
		if message["role"] == "tool" && strings.Contains(fmt.Sprint(message["content"]), want) {
			return true
		}
	}
	return false
}
