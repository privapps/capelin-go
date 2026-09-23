package app

import (
	"bytes"
	configpkg "capelin-go/internal/config"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
)

func TestOneShotReliableToolInvocationRecoveryAndFanout(t *testing.T) {
	profile := configpkg.RuntimeProfile{
		MaxIterations: 20,
		Subagents: configpkg.SubagentConfig{
			MaxDepth:          1,
			MaxChildren:       3,
			MaxParallel:       3,
			DefaultTimeoutSec: 2,
			MaxTimeoutSec:     2,
			MaxToolIterations: 4,
			MaxResultChars:    8000,
			MaxAggregateCount: 3,
			MaxAggregateChars: 12000,
		},
		ToolMaxParallel:    3,
		ToolTimeoutSec:     2,
		ToolRetryOnTimeout: false,
	}

	correctedCommand := fmt.Sprintf(
		`{"command":%s,"args":["-test.run","^$"]}`,
		mustJSON(os.Args[0]),
	)
	testApp := newInteractiveTurnTestAppWithResponses(t,
		// The first model turn deliberately exercises both contract failures.
		chatTurnResponse("", "", []map[string]any{
			goalToolCall("bad-capability", toolCreateSubagent, `{"name":"rejected","question":"should not start","allowed_tools":["go"]}`),
			goalToolCall("bad-command", toolExecuteProgram, `{"command":"echo hello | cat"}`),
		}),
		// Each following corrected request is explicit and auditable rather than
		// being synthesized by the application.
		chatTurnResponse("", "", []map[string]any{
			goalToolCall("retry-capability", toolCreateSubagent, `{"name":"worker-a","question":"worker-a","allowed_tools":["read_file"],"execution_mode":"parallel"}`),
		}),
		chatTurnResponse("", "", []map[string]any{
			goalToolCall("retry-command", toolExecuteProgram, correctedCommand),
		}),
		chatTurnResponse("", "", []map[string]any{
			goalToolCall("create-worker-b", toolCreateSubagent, `{"name":"worker-b","question":"worker-b","allowed_tools":["read_file"],"execution_mode":"parallel"}`),
		}),
		chatTurnResponse("", "", []map[string]any{
			goalToolCall("create-worker-c", toolCreateSubagent, `{"name":"worker-c","question":"worker-c","allowed_tools":["read_file"],"execution_mode":"parallel"}`),
		}),
		chatTurnResponse("", "", []map[string]any{
			goalToolCall("run-worker-a", toolRunSubagent, `{"id":"subagent-1","wait":false,"execution_mode":"parallel"}`),
		}),
		chatTurnResponse("", "", []map[string]any{
			goalToolCall("run-worker-b", toolRunSubagent, `{"id":"subagent-2","wait":false,"execution_mode":"parallel"}`),
		}),
		chatTurnResponse("", "", []map[string]any{
			goalToolCall("run-worker-c", toolRunSubagent, `{"id":"subagent-3","wait":false,"execution_mode":"parallel"}`),
		}),
		chatTurnResponse("", "", []map[string]any{
			goalToolCall("await-worker-a", toolAwaitSubagent, `{"id":"subagent-1","timeout_seconds":2}`),
		}),
		chatTurnResponse("", "", []map[string]any{
			goalToolCall("await-worker-b", toolAwaitSubagent, `{"id":"subagent-2","timeout_seconds":2}`),
		}),
		chatTurnResponse("", "", []map[string]any{
			goalToolCall("await-worker-c", toolAwaitSubagent, `{"id":"subagent-3","timeout_seconds":2}`),
		}),
		chatTurnResponse("", "", []map[string]any{
			goalToolCall("aggregate", toolReadSubagent, `{"ids":["subagent-1","subagent-2","subagent-3"]}`),
		}),
		chatTurnResponse("aggregated all three worker results", "", nil),
	)
	testApp.app.cfg.allowedTools = map[string]bool{
		toolCreateSubagent: true,
		toolRunSubagent:    true,
		toolAwaitSubagent:  true,
		toolReadSubagent:   true,
		toolReadFile:       true,
		toolExecuteProgram: true,
	}
	testApp.app.cfg.ordinaryProfile = profile
	testApp.app.cfg.profilesResolved = true
	testApp.app.toolset = buildAgentTools(testApp.app.cfg.allowedTools)
	testApp.app.subagents = newSubagentManager(toSubagentConfig(profile.Subagents),
		func(ctx context.Context, _ *agentRuntime, session *subagentSession) (string, bool, error) {
			select {
			case <-ctx.Done():
				return "", false, ctx.Err()
			default:
			}
			return "worker result: " + session.Name, false, nil
		})

	var idleLog bytes.Buffer
	testApp.app.idleHooks = newIdleHookRunner("notification", nil, testApp.workspaceRoot, false,
		func(context.Context, string, string, bool, []string) (string, error) {
			return `{"exit_code":9,"failed":true,"stderr":"notification unavailable"}`, nil
		}, &idleLog)

	type observedToolResult struct {
		name    string
		isError bool
		output  string
	}
	var mu sync.Mutex
	var toolResults []observedToolResult
	var systemEvents []string
	var content []string
	sink := testApp.app.sink.(*spySink)
	sink.onToolResult = func(name string, isError bool, output string) {
		mu.Lock()
		defer mu.Unlock()
		toolResults = append(toolResults, observedToolResult{name: name, isError: isError, output: output})
	}
	sink.onSystem = func(message string) {
		mu.Lock()
		defer mu.Unlock()
		systemEvents = append(systemEvents, message)
	}
	sink.onContent = func(message string) {
		mu.Lock()
		defer mu.Unlock()
		content = append(content, message)
	}

	if err := testApp.app.runQuestion(context.Background(), "recover and aggregate the worker results"); err != nil {
		t.Fatalf("one-shot recovery flow failed: %v", err)
	}

	mu.Lock()
	results := append([]observedToolResult(nil), toolResults...)
	events := append([]string(nil), systemEvents...)
	answers := append([]string(nil), content...)
	mu.Unlock()

	if got := len(testApp.userPrompts()); got != 13 {
		t.Fatalf("provider requests = %d, want one request per explicit recovery/fan-out step plus final answer (13)", got)
	}
	if len(answers) == 0 || answers[len(answers)-1] != "aggregated all three worker results" {
		t.Fatalf("final one-shot answer = %#v", answers)
	}

	findResult := func(name string, wantError bool) (string, bool) {
		for _, result := range results {
			if result.name == name && result.isError == wantError {
				return result.output, true
			}
		}
		return "", false
	}
	capabilityError, ok := findResult(toolCreateSubagent, true)
	if !ok || !strings.Contains(capabilityError, `unknown tool "go"`) ||
		!strings.Contains(capabilityError, "registered capability names") {
		t.Fatalf("invalid capability admission diagnostic = %q", capabilityError)
	}
	commandError, ok := findResult(toolExecuteProgram, true)
	if !ok || !strings.Contains(commandError, "executable alone in command") ||
		!strings.Contains(commandError, "each argument separately in args") ||
		!strings.Contains(commandError, "never invokes or parses a shell") {
		t.Fatalf("malformed direct command diagnostic = %q", commandError)
	}
	if _, ok := findResult(toolExecuteProgram, false); !ok {
		t.Fatal("corrected direct command did not complete successfully")
	}

	if !containsEvent(events, "phase=capability_admission") ||
		!containsEvent(events, "scope=allowed[await_subagent,create_subagent,execute_program,read_file,read_subagent,run_subagent]") ||
		!containsEvent(events, "requested[go]") {
		t.Fatalf("capability retry audit missing permission scope: %v", events)
	}
	if !containsEvent(events, "phase=command_execution") ||
		!containsEvent(events, "direct execution does not interpret shell syntax") {
		t.Fatalf("command retry audit missing recovery guidance: %v", events)
	}

	var aggregate subagentAggregateEnvelope
	for _, result := range results {
		if result.name != toolReadSubagent || result.isError {
			continue
		}
		if err := json.Unmarshal([]byte(result.output), &aggregate); err != nil {
			t.Fatalf("decode aggregate result: %v", err)
		}
		break
	}
	if aggregate.Kind != "aggregate" || aggregate.Count != 3 || aggregate.Completed != 3 ||
		aggregate.Failed != 0 || aggregate.Running != 0 || aggregate.QueuedOrPending != 0 {
		t.Fatalf("three-worker aggregate = %+v", aggregate)
	}
	if !strings.Contains(aggregate.CombinedOutput, "worker result: worker-a") ||
		!strings.Contains(aggregate.CombinedOutput, "worker result: worker-b") ||
		!strings.Contains(aggregate.CombinedOutput, "worker result: worker-c") {
		t.Fatalf("aggregate omitted a worker result: %q", aggregate.CombinedOutput)
	}
	for _, item := range aggregate.Items {
		if !sameStrings(item.AllowedTools, []string{toolReadFile}) {
			t.Fatalf("worker %q changed corrected permission scope: %v", item.Name, item.AllowedTools)
		}
	}

	if !strings.Contains(idleLog.String(), "[capelin-go] idle hook failed") ||
		!strings.Contains(idleLog.String(), "notification unavailable") {
		t.Fatalf("idle-hook failure was not classified separately: %q", idleLog.String())
	}
}
