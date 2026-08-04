// Package types is the compatibility import path for shared runtime contracts.
// New capability modules should import internal/contracts directly.
package types

import "capelin-go/internal/contracts"

type Message = contracts.Message
type ToolCall = contracts.ToolCall
type FunctionCall = contracts.FunctionCall
type Request = contracts.Request
type Tool = contracts.Tool
type ToolSpec = contracts.ToolSpec
type ResponseDataInner = contracts.ResponseDataInner
type Response = contracts.Response
type CompletionMessage = contracts.CompletionMessage
type SubagentNode = contracts.SubagentNode
type OutputSink = contracts.OutputSink
type ContinuationState = contracts.ContinuationState
type ProviderState = contracts.ProviderState
