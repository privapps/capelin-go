package providers

import (
	"capelin-go/internal/contracts"
	"encoding/json"
)

// Request and response values are provider-wire compatibility DTOs. They live
// with the adapters so protocol translation does not leak into contracts.
type Request struct {
	Model           string              `json:"model"`
	Messages        []contracts.Message `json:"messages"`
	Tools           []contracts.Tool    `json:"tools,omitempty"`
	ToolChoice      string              `json:"tool_choice,omitempty"`
	ReasoningEffort string              `json:"reasoning_effort,omitempty"`
}

type ResponseDataInner struct {
	Choices []struct {
		Message      CompletionMessage `json:"message"`
		FinishReason string            `json:"finish_reason"`
	} `json:"choices"`
}

type Response struct {
	Data    *ResponseDataInner `json:"data,omitempty"`
	Choices []struct {
		Message      CompletionMessage `json:"message"`
		FinishReason string            `json:"finish_reason"`
	} `json:"choices"`
}

type CompletionMessage struct {
	Role             string               `json:"role"`
	Content          *string              `json:"content"`
	ReasoningContent *string              `json:"reasoning,omitempty"`
	ToolCalls        []contracts.ToolCall `json:"tool_calls,omitempty"`
}

// UnmarshalJSON accepts native reasoning_content as well as the legacy
// reasoning alias. The normalized in-memory representation remains the same.
func (m *CompletionMessage) UnmarshalJSON(data []byte) error {
	var wire struct {
		Role             string               `json:"role"`
		Content          *string              `json:"content"`
		Reasoning        *string              `json:"reasoning"`
		ReasoningContent *string              `json:"reasoning_content"`
		ToolCalls        []contracts.ToolCall `json:"tool_calls"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	m.Role = wire.Role
	m.Content = wire.Content
	m.ReasoningContent = wire.Reasoning
	if wire.ReasoningContent != nil {
		m.ReasoningContent = wire.ReasoningContent
	}
	m.ToolCalls = wire.ToolCalls
	return nil
}
