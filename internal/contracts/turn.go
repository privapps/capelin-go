package contracts

import "context"

// TurnState is provider-owned continuation state. The common engine only asks
// for the normalized conversation; adapters may keep protocol-specific items
// privately behind this interface.
type TurnState interface {
	Messages() []Message
}

// ToolResult is a normalized tool continuation value shared by the engine and
// provider adapters. It contains no dispatcher or policy implementation.
type ToolResult struct {
	Call    ToolCall
	Output  string
	IsError bool
	Retried bool
}

// Completion is the normalized view of one provider response. Concrete
// adapters may attach private wire data to their implementation for the next
// continuation request.
type Completion interface {
	Content() string
	ReasoningContent() string
	ToolCalls() []ToolCall
}

// Provider is the protocol-neutral adapter seam consumed by the turn engine.
type Provider interface {
	Initialize([]Message, string) TurnState
	Complete(context.Context, TurnState, []Tool, string, string) (Completion, error)
	ApplyResponse(TurnState, Completion)
	ApplyToolResults(TurnState, []ToolResult)
	AppendUserPrompt(TurnState, string)
	AppendFinalPrompt(TurnState)
}

// ContinuationProvider is an optional extension implemented by providers that
// need to preserve wire-native state between otherwise independent turns.
// Providers that do not implement it continue to work through Provider alone.
type ContinuationProvider interface {
	Provider
	InitializeWithContinuation([]Message, string, *ContinuationState) TurnState
	ContinuationState(TurnState) *ContinuationState
}

// StatefulProvider is kept as an expressive alias for the optional extension.
type StatefulProvider = ContinuationProvider
