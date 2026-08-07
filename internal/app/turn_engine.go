package app

import (
	"capelin-go/internal/agent"
	"capelin-go/internal/contracts"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

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
	catalog []contracts.Tool
	runner  appToolRunner
}

func (c appToolCapability) Tools() []contracts.Tool {
	return append([]contracts.Tool(nil), c.catalog...)
}

func (c appToolCapability) Run(ctx context.Context, calls []contracts.ToolCall) []agent.ToolResult {
	return c.runner.Run(ctx, calls)
}

func newAppToolCapability(toolset []contracts.Tool, a *app, runtime *agentRuntime) appToolCapability {
	if runtime == nil {
		runtime = a.rootRuntime()
	}
	profile := a.runtimeProfileFor(runtime)
	return appToolCapability{
		catalog: append([]contracts.Tool(nil), toolset...),
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
					HandleResult: func(call contracts.ToolCall, output string) contracts.ToolResult {
						result := contracts.ToolResult{Call: call, Output: output}
						if commandFailed(call, output) {
							runtime.recordRecoverableToolError(fmt.Errorf("%s reported a command failure", call.Function.Name))
							result.IsError = true
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
						return contracts.ToolResult{
							Call:    call,
							Output:  fmt.Sprintf("Tool error: %v", err),
							IsError: true,
						}
					},
				},
			)},
	}
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
