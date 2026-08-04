package app

import (
	"capelin-go/internal/types"
	"context"
	"encoding/json"
)

// The adapters below are retained only for package-local compatibility tests
// and legacy helpers. Production turns use internal/agent.Engine through
// app.runTurnLoop and do not use this state or adapter seam.
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
