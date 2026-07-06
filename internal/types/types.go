package types

// Message represents an LLM API message (user, assistant, system, tool).
type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	Name       string     `json:"name,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
}

// ToolCall represents a tool invocation returned by the LLM.
type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function FunctionCall `json:"function"`
}

// FunctionCall holds the name and arguments of a tool call.
type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// Request is the LLM API completion request body.
type Request struct {
	Model           string    `json:"model"`
	Messages        []Message `json:"messages"`
	Tools           []Tool    `json:"tools,omitempty"`
	ToolChoice      string    `json:"tool_choice,omitempty"`
	ReasoningEffort string    `json:"reasoning_effort,omitempty"`
}

// Tool is the top-level tool descriptor sent in API requests.
type Tool struct {
	Type     string   `json:"type"`
	Function ToolSpec `json:"function"`
}

// ToolSpec describes a single tool's name, description, and parameter schema.
type ToolSpec struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

// ResponseDataInner handles the nested "data" envelope some APIs return.
type ResponseDataInner struct {
	Choices []struct {
		Message      CompletionMessage `json:"message"`
		FinishReason string            `json:"finish_reason"`
	} `json:"choices"`
}

// Response is the LLM API completion response body.
type Response struct {
	Data    *ResponseDataInner `json:"data,omitempty"`
	Choices []struct {
		Message      CompletionMessage `json:"message"`
		FinishReason string            `json:"finish_reason"`
	} `json:"choices"`
}

// CompletionMessage is the assistant message inside an API response.
type CompletionMessage struct {
	Role             string     `json:"role"`
	Content          *string    `json:"content"`
	ReasoningContent *string    `json:"reasoning,omitempty"`
	ToolCalls        []ToolCall `json:"tool_calls,omitempty"`
}

// SubagentNode is a snapshot of one agent node for TUI display.
type SubagentNode struct {
	ID       string
	Name     string
	Question string
	ParentID string
	Status   string
	Depth    int
}

// OutputSink receives structured output events from runTurnLoop.
// All methods must be safe to call from any goroutine.
type OutputSink interface {
	WriteContent(agentID, content string)
	WriteToolCall(agentID, toolName, args string)
	WriteToolResult(agentID, toolName string, isError bool, detail string)
	WriteSystem(agentID, msg string)
}
