package agent

import (
	"context"
	"errors"
	"fmt"
	"time"

	"capelin-go/internal/contracts"

	"golang.org/x/sync/errgroup"
)

// ToolRunnerConfig contains the execution policy shared by tool-capable
// applications. The dispatcher and hooks remain outside the agent package so
// concrete tools and application policy are not coupled to the turn engine.
type ToolRunnerConfig struct {
	// MaxParallel limits the number of calls in one batch. Negative values
	// retain errgroup's unlimited behavior; zero prevents calls from starting,
	// matching the existing application seam.
	MaxParallel int
	// Timeout is the default deadline for each individual call. A zero value
	// preserves the existing immediate-deadline behavior; callers that want no
	// deadline should provide a suitably configured dispatcher instead.
	Timeout time.Duration
	// RetryOnTimeout retries one failed call when its error is or wraps
	// context.DeadlineExceeded.
	RetryOnTimeout bool
}

// ToolRunnerHooks are application-owned extensions to the common execution
// policy. ResolveTimeout may select a per-call deadline (for example, from
// validated tool arguments). HandleError converts a final dispatch error into
// the application-facing tool result and is also the place to record runtime
// errors or apply other application-specific policy.
type ToolRunnerHooks struct {
	ResolveTimeout func(call contracts.ToolCall, defaultTimeout time.Duration) time.Duration
	// HandleResult lets the application preserve a successful dispatch's
	// structured output while adding application-owned result classification.
	// The agent package does not interpret the returned result.
	HandleResult func(call contracts.ToolCall, output string) contracts.ToolResult
	HandleError  func(call contracts.ToolCall, err error) contracts.ToolResult
}

// ToolDispatcher is the concrete application dispatch seam used by the
// configured runner.
type ToolDispatcher func(context.Context, contracts.ToolCall) (string, error)

type configuredToolRunner struct {
	config     ToolRunnerConfig
	dispatcher ToolDispatcher
	hooks      ToolRunnerHooks
}

// NewToolRunner applies the common batch execution policy to an application
// dispatcher. Results always have the same order as calls, regardless of
// completion order. Retry notifications are represented by ToolResult.Retried;
// the engine owns their output event so observable event ordering is unchanged.
func NewToolRunner(config ToolRunnerConfig, dispatcher ToolDispatcher, hooks ToolRunnerHooks) ToolRunner {
	return configuredToolRunner{config: config, dispatcher: dispatcher, hooks: hooks}
}

func (r configuredToolRunner) Run(ctx context.Context, calls []contracts.ToolCall) []contracts.ToolResult {
	results := make([]contracts.ToolResult, len(calls))
	group, groupCtx := errgroup.WithContext(ctx)
	group.SetLimit(r.config.MaxParallel)

	for index, call := range calls {
		index, call := index, call
		group.Go(func() error {
			timeout := r.config.Timeout
			if r.hooks.ResolveTimeout != nil {
				timeout = r.hooks.ResolveTimeout(call, timeout)
			}
			retried := false
			for attempt := 0; attempt <= 1; attempt++ {
				toolCtx, cancel := context.WithTimeout(groupCtx, timeout)
				out, err := r.dispatch(toolCtx, call)
				cancel()
				if err != nil {
					if attempt == 0 && r.config.RetryOnTimeout && errors.Is(err, context.DeadlineExceeded) {
						retried = true
						continue
					}
					result := r.errorResult(call, err)
					result.Retried = retried
					results[index] = result
					return nil
				}
				result := contracts.ToolResult{Call: call, Output: out, Retried: retried}
				if r.hooks.HandleResult != nil {
					result = r.hooks.HandleResult(call, out)
					if result.Call.ID == "" && result.Call.Function.Name == "" {
						result.Call = call
					}
					result.Retried = result.Retried || retried
				}
				results[index] = result
				return nil
			}
			return nil
		})
	}
	_ = group.Wait()
	return results
}

func (r configuredToolRunner) dispatch(ctx context.Context, call contracts.ToolCall) (string, error) {
	if r.dispatcher == nil {
		return "", errors.New("tool dispatcher is nil")
	}
	return r.dispatcher(ctx, call)
}

func (r configuredToolRunner) errorResult(call contracts.ToolCall, err error) contracts.ToolResult {
	if r.hooks.HandleError != nil {
		result := r.hooks.HandleError(call, err)
		if result.Call.ID == "" && result.Call.Function.Name == "" {
			result.Call = call
		}
		return result
	}
	return contracts.ToolResult{Call: call, Output: fmt.Sprintf("Tool error: %v", err), IsError: true}
}
