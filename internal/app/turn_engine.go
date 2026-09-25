package app

import (
	"capelin-go/internal/agent"
	"capelin-go/internal/contracts"
	"capelin-go/internal/tools"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type searchTaskContextKey struct{}
type searchBatchContextKey struct{}

type searchBatchContext struct {
	batch     *tools.SearchBatch
	orderByID map[string]int
}

func withSearchTask(ctx context.Context, task *tools.SearchTask) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if searchTaskFromContext(ctx) != nil {
		return ctx
	}
	if task == nil {
		task = tools.NewSearchTask()
	}
	return context.WithValue(ctx, searchTaskContextKey{}, task)
}

func searchTaskFromContext(ctx context.Context) *tools.SearchTask {
	if ctx == nil {
		return nil
	}
	task, _ := ctx.Value(searchTaskContextKey{}).(*tools.SearchTask)
	return task
}

func withSearchBatch(ctx context.Context, calls []contracts.ToolCall) context.Context {
	searchCount := 0
	for _, call := range calls {
		if call.Function.Name == toolWebSearch && strings.TrimSpace(call.ID) != "" {
			searchCount++
		}
	}
	if searchCount == 0 {
		return ctx
	}
	batch := &searchBatchContext{
		batch:     tools.NewSearchBatch(searchCount),
		orderByID: make(map[string]int, searchCount),
	}
	order := 0
	for _, call := range calls {
		if call.Function.Name != toolWebSearch || strings.TrimSpace(call.ID) == "" {
			continue
		}
		batch.orderByID[call.ID] = order
		order++
	}
	return context.WithValue(ctx, searchBatchContextKey{}, batch)
}

func searchBatchFromContext(ctx context.Context, callID string) (*tools.SearchBatch, int, bool) {
	if ctx == nil {
		return nil, 0, false
	}
	batch, _ := ctx.Value(searchBatchContextKey{}).(*searchBatchContext)
	if batch == nil {
		return nil, 0, false
	}
	order, ok := batch.orderByID[callID]
	if !ok {
		return nil, 0, false
	}
	return batch.batch, order, ok
}

// turnToolResult is retained for the protocol-compatibility adapters. New
// turns use contracts.ToolResult through agent.ToolRunner.
type turnToolResult struct {
	call    contracts.ToolCall
	out     string
	isError bool
	retried bool
}

// fatalToolFailure keeps the application-owned stop classification visible to
// the interactive controller without adding a provider-specific field to the
// normalized tool result sent through the agent engine.
type fatalToolFailure struct{ err error }

func (e fatalToolFailure) Error() string { return e.err.Error() }
func (e fatalToolFailure) Unwrap() error { return e.err }

// appToolRunner is the application adapter around agent's common execution
// policy. It contributes only concrete dispatch and application-specific error
// and timeout hooks.
type appToolRunner struct {
	runner        agent.ToolRunner
	runtime       *agentRuntime
	bindParentCtx func(context.Context)
}

func (r appToolRunner) Run(ctx context.Context, calls []contracts.ToolCall) []agent.ToolResult {
	if r.bindParentCtx != nil {
		r.bindParentCtx(ctx)
	}
	results := r.runner.Run(ctx, calls)
	// A parent cancellation is a runtime stop condition even when the
	// per-call runner already converted the child context error into a tool
	// result. The engine will also observe ctx.Err() before continuing.
	if err := ctx.Err(); err != nil {
		r.runtime.recordFatalError(err)
	}
	return results
}

// appToolCapability is the application-owned adapter at the agent boundary.
// The agent engine receives only its catalog and batch runner; all concrete
// implementations, policy checks, validation, formatting, and subagent
// orchestration stay behind this compatibility wrapper.
type appToolCapability struct {
	catalog    []contracts.Tool
	runner     appToolRunner
	searchTask *tools.SearchTask
}

func (c appToolCapability) Tools() []contracts.Tool {
	return append([]contracts.Tool(nil), c.catalog...)
}

func (c appToolCapability) Run(ctx context.Context, calls []contracts.ToolCall) []agent.ToolResult {
	ctx = withSearchTask(ctx, c.searchTask)
	ctx = withSearchBatch(ctx, calls)
	return c.runner.Run(ctx, calls)
}

func newAppToolCapability(toolset []contracts.Tool, a *app, runtime *agentRuntime) appToolCapability {
	if runtime == nil {
		runtime = a.rootRuntime()
	}
	profile := a.runtimeProfileFor(runtime)
	return appToolCapability{
		catalog:    append([]contracts.Tool(nil), toolset...),
		searchTask: tools.NewSearchTask(),
		runner: appToolRunner{
			runtime: runtime,
			bindParentCtx: func(ctx context.Context) {
				if a.subagents != nil {
					a.subagents.bindParentContext(runtime, ctx)
				}
			},
			runner: agent.NewToolRunner(
				agent.ToolRunnerConfig{
					MaxParallel:    profile.ToolMaxParallel,
					Timeout:        time.Duration(profile.ToolTimeoutSec) * time.Second,
					RetryOnTimeout: profile.ToolRetryOnTimeout,
				},
				func(ctx context.Context, call contracts.ToolCall) (string, error) {
					return a.runToolForRuntime(ctx, runtime, call)
				},
				agent.ToolRunnerHooks{
					ResolveTimeout: func(call contracts.ToolCall, defaultTimeout time.Duration) time.Duration {
						if seconds := parseToolTimeout(call); seconds > 0 {
							return time.Duration(seconds) * time.Second
						}
						return defaultTimeout
					},
					Serialize: func(call contracts.ToolCall) bool {
						return isGoalControlTool(call.Function.Name)
					},
					SerializeKey: sameFileMutationKey,
					HandleResult: func(call contracts.ToolCall, output string) contracts.ToolResult {
						result := contracts.ToolResult{Call: call, Output: output}
						result.DisplayOutput = conciseToolDisplay(call.Function.Name, output)
						if commandFailed(call, output) {
							runtime.recordRecoverableToolError(fmt.Errorf("%s reported a command failure", call.Function.Name))
							result.IsError = true
							result.Recovery = correctedRetryRecovery(runtime, call)
							return result
						}
						// A successful non-control tool result is the liveness
						// signal for the current goal iteration. Checklist
						// management and the completion handshake stay on the
						// control plane: their meaningful state changes are
						// evaluated through the checklist and completion checks.
						// Recording is scoped to an enabled goal runtime so
						// ordinary and subagent turns never carry the flag.
						if runtime.goalIsEnabled() && !isGoalControlTool(call.Function.Name) {
							runtime.recordSuccessfulToolActivity()
						}
						return result
					},
					HandleError: func(call contracts.ToolCall, err error) contracts.ToolResult {
						if fatalToolError(err) {
							runtime.recordFatalError(err)
						} else {
							runtime.recordRecoverableToolError(err)
						}
						result := contracts.ToolResult{
							Call:    call,
							Output:  fmt.Sprintf("Tool error: %v", err),
							IsError: true,
						}
						if !fatalToolError(err) {
							result.Recovery = correctedRetryRecovery(runtime, call)
						}
						return result
					},
				},
			)},
	}
}

// sameFileMutationKey gives the batch runner a stable lane for edit_file
// calls that target the same path. The edit tool performs the authoritative
// workspace/path validation; this key only prevents same-batch calls from
// racing one another while allowing different files to use the configured
// parallelism.
func sameFileMutationKey(call contracts.ToolCall) string {
	if call.Function.Name != toolEditFile {
		return ""
	}
	var args struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
		return ""
	}
	path := strings.TrimSpace(args.Path)
	if path == "" {
		return ""
	}
	return toolEditFile + ":" + filepath.Clean(filepath.FromSlash(path))
}

func correctedRetryRecovery(runtime *agentRuntime, call contracts.ToolCall) *contracts.ToolRecovery {
	recovery := &contracts.ToolRecovery{
		Kind:                     contracts.RecoveryKindCorrectedRetry,
		Phase:                    recoveryPhase(call.Function.Name),
		Attempt:                  1,
		MaxRetries:               1,
		Retryable:                true,
		RequiresExplicitDecision: true,
		Guidance:                 recoveryGuidance(call.Function.Name),
		PermissionScope:          recoveryPermissionScope(runtime, call),
	}
	return recovery
}

func recoveryPhase(toolName string) string {
	switch toolName {
	case toolCreateSubagent:
		return contracts.RecoveryPhaseCapabilityAdmission
	case toolExecuteProgram, toolExecuteSkill:
		return contracts.RecoveryPhaseCommandExecution
	default:
		return contracts.RecoveryPhaseToolInvocation
	}
}

func recoveryGuidance(toolName string) string {
	switch toolName {
	case toolCreateSubagent:
		return "Retry once with a corrected allowed_tools subset from the visible scope; the rejected request created no worker and did not broaden permissions."
	case toolExecuteProgram, toolExecuteSkill:
		return "Retry once with a corrected executable and separate args; direct execution does not interpret shell syntax and does not change permissions."
	default:
		return "Retry once with corrected arguments; the failed call did not change permissions."
	}
}

func recoveryPermissionScope(runtime *agentRuntime, call contracts.ToolCall) *contracts.ToolPermissionScope {
	scope := &contracts.ToolPermissionScope{
		AllowedTools: sortedEnabledTools(nil),
		RestrictOnly: call.Function.Name == toolCreateSubagent,
	}
	if runtime != nil {
		scope.AllowedTools = sortedEnabledTools(runtime.allowedTools)
	}
	if call.Function.Name == toolCreateSubagent {
		var args createSubagentArgs
		if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err == nil {
			scope.RequestedTools = filterNonEmpty(args.AllowedTools)
		}
	}
	return scope
}

func sortedEnabledTools(enabled map[string]bool) []string {
	out := make([]string, 0, len(enabled))
	for name, allowed := range enabled {
		if allowed && strings.TrimSpace(name) != "" {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// commandFailed recognizes the structured result emitted by execute_program
// and execute_skill. Keeping the original JSON as Output gives the provider
// the exit code, stdout, stderr, and timeout fields needed to recover.
func commandFailed(call contracts.ToolCall, output string) bool {
	if call.Function.Name != toolExecuteProgram && call.Function.Name != toolExecuteSkill {
		return false
	}
	var result struct {
		Failed bool `json:"failed"`
	}
	return json.Unmarshal([]byte(output), &result) == nil && result.Failed
}

// isGoalControlTool identifies checklist-management and completion-handshake
// tools. Their meaningful state changes are validated through the
// authoritative checklist comparison and the completion handshake checks
// rather than through the per-iteration activity signal.
func isGoalControlTool(name string) bool {
	return name == toolUpdateTodos || name == toolCompleteGoal
}

func fatalToolError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) {
		return true
	}
	message := strings.ToLower(err.Error())
	for _, marker := range []string{
		"runtime is required",
		"tool dispatcher is nil",
		"subagent capability is unavailable",
		"todo capability is unavailable",
	} {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
}
