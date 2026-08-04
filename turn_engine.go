package main

import (
	"capelin-go/internal/types"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"
)

// turnState is the protocol-neutral conversation state. Adapters may retain
// protocol-specific continuation items privately in input.
type turnState struct {
	messages []types.Message
	input    []json.RawMessage
}

type turnAdapter interface {
	initialize(messages []types.Message, question string) *turnState
	complete(ctx context.Context, state *turnState, tools []types.Tool, model, reasoning string) (*completionMessage, error)
	applyResponse(state *turnState, response *completionMessage)
	applyToolResults(state *turnState, results []turnToolResult)
	appendUserPrompt(state *turnState, content string)
	appendFinalPrompt(state *turnState)
}

type chatTurnAdapter struct{ client *client }

func (a chatTurnAdapter) initialize(messages []types.Message, question string) *turnState {
	state := &turnState{messages: append([]types.Message(nil), messages...)}
	state.messages = append(state.messages, types.Message{Role: "user", Content: question})
	return state
}

func (a chatTurnAdapter) complete(ctx context.Context, state *turnState, tools []types.Tool, model, reasoning string) (*completionMessage, error) {
	return a.client.complete(ctx, state.messages, tools, model, reasoning)
}

func (a chatTurnAdapter) applyResponse(state *turnState, response *completionMessage) {
	state.messages = append(state.messages, response.asMessage())
}

func (a chatTurnAdapter) applyToolResults(state *turnState, results []turnToolResult) {
	for _, result := range results {
		state.messages = append(state.messages, types.Message{Role: "tool", ToolCallID: result.call.ID, Content: result.out})
	}
}

func (a chatTurnAdapter) appendUserPrompt(state *turnState, content string) {
	state.messages = append(state.messages, types.Message{Role: "user", Content: content})
}

func (a chatTurnAdapter) appendFinalPrompt(state *turnState) {
	a.appendUserPrompt(state, "[SYSTEM] Maximum tool iterations reached. Based on everything you have gathered so far, provide your best final answer now. Do not request any more tools.")
}

type responsesTurnAdapter struct{ client *client }

func (a responsesTurnAdapter) initialize(messages []types.Message, question string) *turnState {
	input := messagesToResponsesInput(messages)
	input = append(input, marshalResponsesItem(map[string]any{"role": "user", "content": question}))
	conversation := append([]types.Message(nil), messages...)
	conversation = append(conversation, types.Message{Role: "user", Content: question})
	return &turnState{messages: conversation, input: input}
}

func (a responsesTurnAdapter) complete(ctx context.Context, state *turnState, tools []types.Tool, model, reasoning string) (*completionMessage, error) {
	return a.client.completeResponses(ctx, state.input, tools, model, reasoning)
}

func (a responsesTurnAdapter) applyResponse(state *turnState, response *completionMessage) {
	state.input = append(state.input, response.outputItems...)
	state.messages = append(state.messages, response.asMessage())
}

func (a responsesTurnAdapter) applyToolResults(state *turnState, results []turnToolResult) {
	for _, result := range results {
		state.input = append(state.input, marshalResponsesItem(map[string]any{
			"type": "function_call_output", "call_id": result.call.ID, "output": result.out,
		}))
		state.messages = append(state.messages, types.Message{Role: "tool", ToolCallID: result.call.ID, Content: result.out})
	}
}

func (a responsesTurnAdapter) appendUserPrompt(state *turnState, content string) {
	state.input = append(state.input, marshalResponsesItem(map[string]any{
		"role": "user", "content": content,
	}))
	state.messages = append(state.messages, types.Message{Role: "user", Content: content})
}

func (a responsesTurnAdapter) appendFinalPrompt(state *turnState) {
	a.appendUserPrompt(state, "[SYSTEM] Maximum tool iterations reached. Based on everything you have gathered so far, provide your best final answer now. Do not request any more tools.")
}

type turnToolResult struct {
	call    types.ToolCall
	out     string
	isError bool
}

func (a *app) runTurnLoopWithAdapter(ctx context.Context, messages []types.Message, question string, runtime *agentRuntime, toolset []types.Tool, emitOutput bool, adapter turnAdapter) ([]types.Message, string, string, error) {
	state := adapter.initialize(messages, question)
	maxIterations := defaultMaxIterations
	if runtime != nil && runtime.maxToolIterations > 0 {
		maxIterations = runtime.maxToolIterations
	}
	runtimeModel, runtimeReasoning := a.client.model, a.client.reasoning
	if runtime != nil && runtime.model != "" {
		runtimeModel, runtimeReasoning = runtime.model, runtime.reasoning
	}
	lastContent := ""
	var reasoningBuf strings.Builder
	agentID := rootAgentID
	if runtime != nil && strings.TrimSpace(runtime.sessionID) != "" {
		agentID = runtime.sessionID
	}
	sink := a.sink
	if sink == nil {
		sink = &stdioSink{}
	}

	for iter := 0; iter < maxIterations; iter++ {
		if iter == maxIterations-3 && maxIterations > 3 {
			adapter.appendUserPrompt(state, fmt.Sprintf("[SYSTEM] You have %d iterations remaining. Wrap up and produce a final answer now.", maxIterations-iter))
		}

		resp, err := a.completeTurnWithRetry(ctx, state, toolset, runtimeModel, runtimeReasoning, agentID, emitOutput, sink, adapter)
		if err != nil {
			return state.messages, "", "", err
		}
		if content := strings.TrimSpace(resp.Content()); content != "" {
			lastContent = content
			if emitOutput {
				sink.WriteContent(agentID, content)
			}
		}
		if reasoning := strings.TrimSpace(resp.ReasoningContent()); reasoning != "" {
			if reasoningBuf.Len() > 0 {
				reasoningBuf.WriteString("\n\n")
			}
			fmt.Fprintf(&reasoningBuf, "[Turn %d]\nThinking: %s", iter+1, reasoning)
		}
		adapter.applyResponse(state, resp)
		toolCalls := resp.ToolCalls()
		if len(toolCalls) == 0 {
			if emitOutput {
				sink.WriteSystem(agentID, "")
			}
			return state.messages, lastContent, reasoningBuf.String(), nil
		}

		results := runTurnTools(ctx, a, runtime, toolCalls, agentID, emitOutput, sink)
		for _, result := range results {
			if emitOutput {
				sink.WriteToolResult(agentID, result.call.Function.Name, result.isError, result.out)
			}
		}
		adapter.applyToolResults(state, results)
		if reasoningBuf.Len() > 0 {
			reasoningBuf.WriteString("\n\n")
		}
		fmt.Fprintf(&reasoningBuf, "[Turn %d]\nTool calls:\n", iter+1)
		for _, result := range results {
			args := truncateStr(result.call.Function.Arguments, 200)
			fmt.Fprintf(&reasoningBuf, "  %s(%s)\n", result.call.Function.Name, args)
			if summary := extractToolSummary(result.call.Function.Name, result.out, result.isError); summary != "" {
				fmt.Fprintf(&reasoningBuf, "  > %s\n", summary)
			}
		}
	}

	if !a.cfg.finalOnly {
		fmt.Fprintf(os.Stderr, "[capelin-go] Maximum tool iterations (%d) reached; requesting final answer.\n", maxIterations)
	}
	adapter.appendFinalPrompt(state)
	resp, err := a.completeTurnWithRetry(ctx, state, nil, runtimeModel, runtimeReasoning, agentID, emitOutput, sink, adapter)
	if err != nil {
		if lastContent != "" {
			return state.messages, lastContent, reasoningBuf.String(), nil
		}
		return state.messages, "", "", fmt.Errorf("exceeded maximum tool iterations (%d) and final-answer call failed: %w", maxIterations, err)
	}
	if content := strings.TrimSpace(resp.Content()); content != "" {
		if emitOutput {
			sink.WriteContent(agentID, content)
			sink.WriteSystem(agentID, "")
		}
		if reasoning := strings.TrimSpace(resp.ReasoningContent()); reasoning != "" {
			if reasoningBuf.Len() > 0 {
				reasoningBuf.WriteString("\n\n")
			}
			fmt.Fprintf(&reasoningBuf, "[Turn %d]\nThinking: %s", maxIterations+1, reasoning)
		}
		adapter.applyResponse(state, resp)
		return state.messages, content, reasoningBuf.String(), nil
	}
	return state.messages, lastContent, reasoningBuf.String(), nil
}

func (a *app) completeTurnWithRetry(ctx context.Context, state *turnState, tools []types.Tool, model, reasoning, agentID string, emitOutput bool, sink types.OutputSink, adapter turnAdapter) (*completionMessage, error) {
	const maxAttempts = 3
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			delay := 2 * time.Second * time.Duration(1<<(attempt-1))
			delay += time.Duration(rand.Int63n(int64(delay) / 2))
			if emitOutput {
				sink.WriteSystem(agentID, fmt.Sprintf("[tool] model request failed (429/5xx), retrying in %v…", delay))
			}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(delay):
			}
		}
		resp, err := adapter.complete(ctx, state, tools, model, reasoning)
		if err == nil {
			return resp, nil
		}
		if !isRetryableError(err) {
			return nil, err
		}
		if attempt == maxAttempts-1 {
			return nil, err
		}
	}
	return nil, errors.New("turn retry exhausted")
}

func runTurnTools(ctx context.Context, a *app, runtime *agentRuntime, calls []types.ToolCall, agentID string, emitOutput bool, sink types.OutputSink) []turnToolResult {
	results := make([]turnToolResult, len(calls))
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(a.cfg.toolMaxParallel)
	for i, call := range calls {
		i, call := i, call
		g.Go(func() error {
			if emitOutput {
				sink.WriteToolCall(agentID, call.Function.Name, call.Function.Arguments)
			}
			timeoutSec := a.cfg.toolTimeoutSec
			if toolTimeout := parseToolTimeout(call); toolTimeout > 0 {
				timeoutSec = toolTimeout
			}
			for attempt := 0; attempt <= 1; attempt++ {
				toolCtx, cancel := context.WithTimeout(gctx, time.Duration(timeoutSec)*time.Second)
				out, err := a.runToolForRuntime(toolCtx, runtime, call)
				cancel()
				if err != nil {
					if attempt == 0 && a.cfg.toolRetryOnTimeout && errors.Is(err, context.DeadlineExceeded) {
						if emitOutput {
							sink.WriteSystem(agentID, fmt.Sprintf("[tool] %s timed out, retrying…", call.Function.Name))
						}
						continue
					}
					runtime.recordToolError(err)
					results[i] = turnToolResult{call: call, out: fmt.Sprintf("Tool error: %v", err), isError: true}
					return nil
				}
				results[i] = turnToolResult{call: call, out: out}
				return nil
			}
			return nil
		})
	}
	_ = g.Wait()
	return results
}
