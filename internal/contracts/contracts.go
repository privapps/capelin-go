// Package contracts contains protocol-neutral values shared by the runtime.
// Provider wire DTOs belong to provider adapters and are intentionally not here.
package contracts

import "encoding/json"

// CapelinUserAgent identifies HTTP requests owned by the Capelin runtime. It is
// intentionally not applied to transparent/raw proxy forwarding.
const CapelinUserAgent = "Capelin-Go"

// Message represents an LLM conversation message.
type Message struct {
	Role             string     `json:"role"`
	Content          string     `json:"content,omitempty"`
	ReasoningContent *string    `json:"reasoning,omitempty"`
	ToolCallID       string     `json:"tool_call_id,omitempty"`
	Name             string     `json:"name,omitempty"`
	ToolCalls        []ToolCall `json:"tool_calls,omitempty"`
}

// UnmarshalJSON accepts both the normalized/session spelling and the native
// Chat Completions spelling. Native content wins when a provider sends both.
// MarshalJSON intentionally keeps the existing `reasoning` representation so
// public compatibility output and old session snapshots remain stable.
func (m *Message) UnmarshalJSON(data []byte) error {
	var wire struct {
		Role             string     `json:"role"`
		Content          string     `json:"content"`
		Reasoning        *string    `json:"reasoning"`
		ReasoningContent *string    `json:"reasoning_content"`
		ToolCallID       string     `json:"tool_call_id"`
		Name             string     `json:"name"`
		ToolCalls        []ToolCall `json:"tool_calls"`
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
	m.ToolCallID = wire.ToolCallID
	m.Name = wire.Name
	m.ToolCalls = wire.ToolCalls
	return nil
}

type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function FunctionCall `json:"function"`
}

type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type Request struct {
	Model           string    `json:"model"`
	Messages        []Message `json:"messages"`
	Tools           []Tool    `json:"tools,omitempty"`
	ToolChoice      string    `json:"tool_choice,omitempty"`
	ReasoningEffort string    `json:"reasoning_effort,omitempty"`
}

type Tool struct {
	Type     string   `json:"type"`
	Function ToolSpec `json:"function"`
}

type ToolSpec struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
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
	Role             string     `json:"role"`
	Content          *string    `json:"content"`
	ReasoningContent *string    `json:"reasoning,omitempty"`
	ToolCalls        []ToolCall `json:"tool_calls,omitempty"`
}

// UnmarshalJSON accepts native reasoning_content as well as the legacy
// reasoning alias. The normalized in-memory representation remains the same.
func (m *CompletionMessage) UnmarshalJSON(data []byte) error {
	var wire struct {
		Role             string     `json:"role"`
		Content          *string    `json:"content"`
		Reasoning        *string    `json:"reasoning"`
		ReasoningContent *string    `json:"reasoning_content"`
		ToolCalls        []ToolCall `json:"tool_calls"`
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

// ContinuationState is an opaque provider-owned snapshot. The turn engine and
// application only carry and persist it; adapters validate and interpret Data.
// Provider and Version let an adapter reject stale or mismatched state and
// rebuild safely from normalized messages.
type ContinuationState struct {
	Provider string          `json:"provider"`
	Version  int             `json:"version"`
	Data     json.RawMessage `json:"data,omitempty"`
}

// ProviderState is a compatibility alias for callers that use the more
// descriptive provider-owned name.
type ProviderState = ContinuationState

type SubagentNode struct {
	ID       string
	Name     string
	Question string
	ParentID string
	Status   string
	Role     string
	Depth    int
}

// OutputSink receives structured runtime events. Implementations must be safe
// for concurrent calls because tools may run in parallel.
type OutputSink interface {
	WriteContent(agentID, content string)
	WriteToolCall(agentID, toolName, args string)
	WriteToolResult(agentID, toolName string, isError bool, detail string)
	WriteSystem(agentID, msg string)
}
