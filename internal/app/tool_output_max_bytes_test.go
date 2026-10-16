package app

import (
	configpkg "capelin-go/internal/config"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestReadSubagentToolResultReachesNextProviderRequestWithinConfiguredByteCap(t *testing.T) {
	const acceptanceMarker = "ACCEPTANCE_MATRIX_COMPLETE_7f29c1"
	reviewerReport := strings.Repeat("review evidence\n", 2200) + acceptanceMarker
	for _, protocol := range []struct {
		name      string
		responses bool
	}{
		{name: "Chat Completions"},
		{name: "Responses", responses: true},
	} {
		for _, limit := range []struct {
			name       string
			maxBytes   int
			wantMarker bool
		}{
			{name: "raised cap", maxBytes: 64000, wantMarker: true},
			{name: "default cap", maxBytes: 16000},
		} {
			t.Run(protocol.name+"/"+limit.name, func(t *testing.T) {
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

					calls := [][]map[string]any{
						{goalToolCall("create-reviewer", toolCreateSubagent, `{"name":"reviewer","question":"review the acceptance criteria","allowed_tools":["read_file"]}`)},
						{goalToolCall("run-reviewer", toolRunSubagent, `{"id":"subagent-1","wait":true}`)},
						{goalToolCall("read-reviewer", toolReadSubagent, `{"id":"subagent-1"}`)},
					}
					var body string
					if index <= len(calls) {
						body = protocolToolResponse(protocol.responses, "", calls[index-1])
					} else {
						body = protocolFinalResponse(protocol.responses, "review complete")
					}
					w.Header().Set("Content-Type", "application/json")
					_, _ = fmt.Fprint(w, body)
				}))
				defer server.Close()

				endpoint := server.URL + "/chat/completions"
				if protocol.responses {
					endpoint = server.URL + "/responses"
				}
				profile := configpkg.RuntimeProfile{
					MaxIterations:     8,
					MaxGoalIterations: 8,
					Subagents: configpkg.SubagentConfig{
						MaxDepth: 1, MaxChildren: 2, MaxParallel: 2,
						DefaultTimeoutSec: 10, MaxTimeoutSec: 10, MaxToolIterations: 4,
						MaxResultChars: 48000, MaxAggregateCount: 2, MaxAggregateChars: 48000,
					},
					ToolMaxParallel: 1, ToolTimeoutSec: 5,
				}
				allowed := map[string]bool{
					toolCreateSubagent: true,
					toolRunSubagent:    true,
					toolReadSubagent:   true,
					toolReadFile:       true,
				}
				a := &app{
					cfg: config{
						workspaceRoot: t.TempDir(), allowedTools: allowed,
						ordinaryProfile: profile, profilesResolved: true,
					},
					client: &client{
						endpoint: endpoint, model: "test-model", http: server.Client(),
						toolOutputMaxBytes: limit.maxBytes,
					},
					sink: &turnEventSink{},
				}
				a.toolset = buildAgentTools(allowed)
				a.subagents = newSubagentManager(toSubagentConfig(profile.Subagents), func(context.Context, *agentRuntime, *subagentSession) (string, bool, error) {
					return reviewerReport, false, nil
				})

				_, answer, _, err := a.runTurnLoop(context.Background(), nil, "read the reviewer report", a.rootRuntime(), a.toolset, false)
				if err != nil {
					t.Fatalf("parent turn failed: %v", err)
				}
				if answer != "review complete" {
					t.Fatalf("parent answer = %q, want %q", answer, "review complete")
				}

				mu.Lock()
				captured := append([]map[string]any(nil), requests...)
				mu.Unlock()
				if len(captured) != 4 {
					t.Fatalf("provider requests = %d, want 4", len(captured))
				}
				toolOutput, ok := toolResultFromRequest(captured[3], protocol.responses, "read-reviewer")
				if !ok {
					t.Fatalf("request after read_subagent did not contain its tool result: %#v", captured[3])
				}
				if len(toolOutput) <= 16000 {
					t.Fatalf("read_subagent result was only %d bytes, want a result above the built-in 16000-byte cap", len(toolOutput))
				}
				if got := strings.Contains(toolOutput, acceptanceMarker); got != limit.wantMarker {
					t.Fatalf("acceptance marker present = %t, want %t", got, limit.wantMarker)
				}
				if limit.wantMarker {
					if strings.Contains(toolOutput, "[truncated:") {
						t.Fatalf("raised-cap result was unexpectedly truncated: %q", toolOutput[len(toolOutput)-80:])
					}
				} else {
					markerAt := strings.Index(toolOutput, "[truncated:")
					if markerAt != 16000 || !strings.Contains(toolOutput[markerAt:], " bytes]") {
						t.Fatalf("default result does not show a 16000-byte truncation marker: %q", toolOutput[len(toolOutput)-80:])
					}
				}
				if !strings.Contains(toolOutput, strings.ReplaceAll(reviewerReport[:128], "\n", `\n`)) {
					t.Fatal("provider request omitted the beginning of the reviewer report")
				}
			})
		}
	}
}

func toolResultFromRequest(request map[string]any, responses bool, callID string) (string, bool) {
	if responses {
		input, _ := request["input"].([]any)
		for _, raw := range input {
			item, _ := raw.(map[string]any)
			if item["type"] == "function_call_output" && item["call_id"] == callID {
				output, ok := item["output"].(string)
				return output, ok
			}
		}
		return "", false
	}
	messages, _ := request["messages"].([]any)
	for _, raw := range messages {
		message, _ := raw.(map[string]any)
		if message["role"] == "tool" && message["tool_call_id"] == callID {
			content, ok := message["content"].(string)
			return content, ok
		}
	}
	return "", false
}
