package agent

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"strconv"
	"strings"
	"time"

	"capelin-go/internal/contracts"
)

// Provider is the protocol-neutral adapter seam consumed by the turn engine.
type Provider = contracts.Provider

// ToolResult is the normalized result passed back to a provider for
// continuation.
type ToolResult = contracts.ToolResult

// ToolRunner executes a batch of calls. Implementations may execute calls in
// parallel, but must return results in the same order as calls.
type ToolRunner interface {
	Run(context.Context, []contracts.ToolCall) []contracts.ToolResult
}

// ToolCatalog supplies the provider-facing tool definitions for a turn. The
// engine deliberately knows only this narrow catalog seam; tool schemas and
// implementations remain owned by the application capability that provides it.
type ToolCatalog interface {
	Tools() []contracts.Tool
}

// ToolCapability combines the two independent parts of the tool boundary. It
// is intentionally small so the engine can be exercised with controlled
// adapters without importing any concrete tool implementation.
type ToolCapability interface {
	ToolCatalog
	ToolRunner
}

// RunOptions controls one protocol-neutral turn.
type RunOptions struct {
	Messages          []contracts.Message
	Question          string
	Model             string
	Reasoning         string
	MaxToolIterations int
	AgentID           string
	EmitOutput        bool
	FinalOnly         bool
	Sink              contracts.OutputSink
	// Tools is retained for callers that already provide a static tool slice.
	// Prefer ToolCatalog when the catalog is runtime- or policy-dependent.
	Tools          []contracts.Tool
	ToolCatalog    ToolCatalog
	ToolCapability ToolCapability
	ToolRunner     ToolRunner
	ToolSummary    func(toolName, output string, isError bool) string
	// ContinuationState is opaque to the engine. Stateful providers validate it
	// and either restore their native wire state or rebuild from Messages.
	ContinuationState *contracts.ContinuationState
}

// Result contains the conversation and observable answer produced by a turn.
type Result struct {
	Messages          []contracts.Message
	Answer            string
	Reasoning         string
	ContinuationState *contracts.ContinuationState
}

// Engine owns the common turn policy. Providers only translate requests,
// responses, and continuation state.
type Engine struct {
	Provider Provider
}

func (e *Engine) Run(ctx context.Context, options RunOptions) (Result, error) {
	if e == nil || e.Provider == nil {
		return Result{}, context.Canceled
	}
	// Resolve the capability once per turn. Keeping the legacy fields as a
	// fallback makes this extraction source-compatible for existing adapters.
	catalog := options.ToolCatalog
	runner := options.ToolRunner
	if options.ToolCapability != nil {
		if catalog == nil {
			catalog = options.ToolCapability
		}
		if runner == nil {
			runner = options.ToolCapability
		}
	}
	tools := options.Tools
	if catalog != nil {
		tools = catalog.Tools()
	}
	options.Tools = tools
	options.ToolCatalog = catalog
	options.ToolRunner = runner

	state := initializeState(e.Provider, options.Messages, options.Question, options.ContinuationState)
	maxIterations := options.MaxToolIterations
	if maxIterations <= 0 {
		maxIterations = 40
	}
	agentID := options.AgentID
	if agentID == "" {
		agentID = "root"
	}
	sink := options.Sink
	if sink == nil {
		sink = discardSink{}
	}
	lastContent := ""
	reasoning := reasoningAccumulator{}

	for iter := 0; iter < maxIterations; iter++ {
		if err := ctx.Err(); err != nil {
			return Result{Messages: state.Messages(), ContinuationState: exportContinuationState(e.Provider, state)}, err
		}
		if iter == maxIterations-3 && maxIterations > 3 {
			e.Provider.AppendUserPrompt(state, "[SYSTEM] You have "+itoa(maxIterations-iter)+" iterations remaining. Wrap up and produce a final answer now.")
		}
		response, err := e.completeWithRetry(ctx, state, options)
		if err != nil {
			return Result{Messages: state.Messages(), ContinuationState: exportContinuationState(e.Provider, state)}, err
		}
		if content := trim(response.Content()); content != "" {
			lastContent = content
			if options.EmitOutput {
				sink.WriteContent(agentID, content)
			}
		}
		if value := trim(response.ReasoningContent()); value != "" {
			reasoning.Add(iter+1, value)
		}
		e.Provider.ApplyResponse(state, response)
		calls := response.ToolCalls()
		if len(calls) == 0 {
			if options.EmitOutput {
				sink.WriteSystem(agentID, "")
			}
			return Result{Messages: state.Messages(), Answer: lastContent, Reasoning: reasoning.String(), ContinuationState: exportContinuationState(e.Provider, state)}, nil
		}
		if options.ToolRunner == nil {
			return Result{Messages: state.Messages(), Answer: lastContent, Reasoning: reasoning.String(), ContinuationState: exportContinuationState(e.Provider, state)}, context.Canceled
		}
		for _, call := range calls {
			if options.EmitOutput {
				sink.WriteToolCall(agentID, call.Function.Name, call.Function.Arguments)
			}
		}
		results := options.ToolRunner.Run(ctx, calls)
		for _, result := range results {
			if options.EmitOutput {
				if result.Retried {
					sink.WriteSystem(agentID, "[tool] "+result.Call.Function.Name+" timed out, retrying…")
				}
				sink.WriteToolResult(agentID, result.Call.Function.Name, result.IsError, result.Output)
			}
		}
		e.Provider.ApplyToolResults(state, results)
		reasoning.AddToolCalls(iter+1, results, options.ToolSummary)
	}

	if !options.FinalOnly {
		// Keep this diagnostic on stderr in the application sink rather than
		// making the protocol adapters aware of turn limits.
		sink.WriteSystem(agentID, "[capelin-go] Maximum tool iterations ("+itoa(maxIterations)+") reached; requesting final answer.")
	}
	e.Provider.AppendFinalPrompt(state)
	response, err := e.completeWithRetry(ctx, state, RunOptions{
		Model: options.Model, Reasoning: options.Reasoning, Tools: nil,
		ToolRunner: options.ToolRunner, Sink: sink, EmitOutput: options.EmitOutput,
		AgentID: agentID,
	})
	if err != nil {
		if lastContent != "" {
			return Result{Messages: state.Messages(), Answer: lastContent, Reasoning: reasoning.String(), ContinuationState: exportContinuationState(e.Provider, state)}, nil
		}
		return Result{Messages: state.Messages(), ContinuationState: exportContinuationState(e.Provider, state)}, errorf("exceeded maximum tool iterations (%d) and final-answer call failed: %v", maxIterations, err)
	}
	if content := trim(response.Content()); content != "" {
		if options.EmitOutput {
			sink.WriteContent(agentID, content)
			sink.WriteSystem(agentID, "")
		}
		if value := trim(response.ReasoningContent()); value != "" {
			reasoning.Add(maxIterations+1, value)
		}
		e.Provider.ApplyResponse(state, response)
		return Result{Messages: state.Messages(), Answer: content, Reasoning: reasoning.String(), ContinuationState: exportContinuationState(e.Provider, state)}, nil
	}
	return Result{Messages: state.Messages(), Answer: lastContent, Reasoning: reasoning.String(), ContinuationState: exportContinuationState(e.Provider, state)}, nil
}

func (e *Engine) completeWithRetry(ctx context.Context, state contracts.TurnState, options RunOptions) (contracts.Completion, error) {
	const maxAttempts = 3
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			delay := retryDelay(attempt)
			if options.EmitOutput && options.Sink != nil {
				options.Sink.WriteSystem(options.AgentID, "[tool] model request failed (429/5xx), retrying in "+delay.String()+"…")
			}
			timer := time.After(delay)
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-timer:
			}
		}
		response, err := e.Provider.Complete(ctx, state, options.Tools, options.Model, options.Reasoning)
		if err == nil {
			return response, nil
		}
		if !retryable(err) || attempt == maxAttempts-1 {
			return nil, err
		}
	}
	return nil, errorf("turn retry exhausted")
}

func initializeState(provider Provider, messages []contracts.Message, question string, continuation *contracts.ContinuationState) contracts.TurnState {
	if stateful, ok := provider.(contracts.ContinuationProvider); ok {
		return stateful.InitializeWithContinuation(messages, question, continuation)
	}
	return provider.Initialize(messages, question)
}

func exportContinuationState(provider Provider, state contracts.TurnState) *contracts.ContinuationState {
	if stateful, ok := provider.(contracts.ContinuationProvider); ok {
		return stateful.ContinuationState(state)
	}
	return nil
}

func retryDelay(attempt int) time.Duration {
	delay := 2 * time.Second * time.Duration(1<<(attempt-1))
	return delay + time.Duration(rand.Int63n(int64(delay)/2))
}

func retryable(err error) bool {
	type marker interface{ Retryable() bool }
	var value marker
	return errors.As(err, &value) && value.Retryable()
}

func trim(value string) string                { return strings.TrimSpace(value) }
func itoa(value int) string                   { return strconv.Itoa(value) }
func errorf(format string, args ...any) error { return fmt.Errorf(format, args...) }

type reasoningAccumulator struct{ value strings.Builder }

func (r *reasoningAccumulator) Add(turn int, text string) {
	if r.value.Len() > 0 {
		r.value.WriteString("\n\n")
	}
	fmt.Fprintf(&r.value, "[Turn %d]\nThinking: %s", turn, text)
}
func (r *reasoningAccumulator) AddToolCalls(turn int, results []ToolResult, summary func(string, string, bool) string) {
	if r.value.Len() > 0 {
		r.value.WriteString("\n\n")
	}
	fmt.Fprintf(&r.value, "[Turn %d]\nTool calls:\n", turn)
	for _, result := range results {
		fmt.Fprintf(&r.value, "  %s(%s)\n", result.Call.Function.Name, truncate(result.Call.Function.Arguments, 200))
		if summary != nil {
			if text := summary(result.Call.Function.Name, result.Output, result.IsError); text != "" {
				fmt.Fprintf(&r.value, "  > %s\n", text)
			}
		}
	}
}
func (r *reasoningAccumulator) String() string { return r.value.String() }
func truncate(value string, max int) string {
	if len(value) <= max {
		return value
	}
	return value[:max] + "..."
}

type discardSink struct{}

func (discardSink) WriteContent(string, string)                  {}
func (discardSink) WriteToolCall(string, string, string)         {}
func (discardSink) WriteToolResult(string, string, bool, string) {}
func (discardSink) WriteSystem(string, string)                   {}
