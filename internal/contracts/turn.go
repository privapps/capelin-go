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
	// DisplayOutput is an optional concise, human-facing rendering of Output.
	// Output remains the authoritative structured value sent to the model; this
	// field is consumed only by interactive output sinks.
	DisplayOutput string `json:"-"`
	// Recovery describes a bounded, explicit recovery choice for a
	// recoverable tool result. It is separate from Retried because corrected
	// capability or command retries are chosen by the model/operator rather
	// than performed by the runner.
	Recovery *ToolRecovery
}

// ToolRecovery is the protocol-neutral audit record for one recoverable tool
// failure. A result offers at most one corrected retry; it never changes the
// worker's permission scope on its own.
type ToolRecovery struct {
	Kind                     string               `json:"kind"`
	Phase                    string               `json:"phase"`
	Attempt                  int                  `json:"attempt"`
	MaxRetries               int                  `json:"max_retries"`
	Retryable                bool                 `json:"retryable"`
	RequiresExplicitDecision bool                 `json:"requires_explicit_decision"`
	Guidance                 string               `json:"guidance"`
	PermissionScope          *ToolPermissionScope `json:"permission_scope,omitempty"`
}

// ToolPermissionScope makes the policy boundary visible in recovery output.
// RequestedTools is the attempted child scope; AllowedTools is the scope
// available to the current runtime; EffectiveTools is populated only when a
// request was admitted.
type ToolPermissionScope struct {
	AllowedTools   []string `json:"allowed_tools"`
	RequestedTools []string `json:"requested_tools,omitempty"`
	EffectiveTools []string `json:"effective_tools,omitempty"`
	RestrictOnly   bool     `json:"restrict_only"`
}

const (
	RecoveryKindCorrectedRetry       = "corrected_retry"
	RecoveryPhaseCapabilityAdmission = "capability_admission"
	RecoveryPhaseCommandExecution    = "command_execution"
	RecoveryPhaseToolInvocation      = "tool_invocation"
)

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
