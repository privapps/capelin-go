// Package types is the compatibility import path for shared runtime contracts.
// New capability modules should import internal/contracts directly.
package types

import (
	"capelin-go/internal/contracts"
	"capelin-go/internal/providers"
)

type Message = contracts.Message
type ToolCall = contracts.ToolCall
type FunctionCall = contracts.FunctionCall
type Tool = contracts.Tool
type ToolSpec = contracts.ToolSpec
type Request = providers.Request
type ResponseDataInner = providers.ResponseDataInner
type Response = providers.Response
type CompletionMessage = providers.CompletionMessage
type SubagentNode = contracts.SubagentNode
type OutputSink = contracts.OutputSink
type ContinuationState = contracts.ContinuationState
type ProviderState = contracts.ProviderState
