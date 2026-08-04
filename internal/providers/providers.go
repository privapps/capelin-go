// Package providers contains model-protocol adapters. Wire DTOs and
// continuation representations intentionally stay private to this package.
package providers

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"

	"capelin-go/internal/contracts"
)

const (
	chatContinuationProvider      = "chat_completions"
	responsesContinuationProvider = "responses"
	continuationVersion           = 1
)

// Config configures a provider adapter without coupling it to application
// configuration or the turn engine.
type Config struct {
	Endpoint string
	Token    string
	Model    string
	Debug    bool
	HTTP     *http.Client
}

func IsResponsesEndpoint(endpoint string) bool {
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return false
	}
	path := strings.TrimRight(parsed.Path, "/")
	return path == "/responses" || strings.HasSuffix(path, "/responses")
}

func New(cfg Config) contracts.Provider {
	if IsResponsesEndpoint(cfg.Endpoint) {
		return NewResponses(cfg)
	}
	return NewChatCompletions(cfg)
}

type retryableHTTPError struct {
	status  int
	message string
}

func (e *retryableHTTPError) Error() string { return e.message }
func (*retryableHTTPError) Retryable() bool { return true }

type retryableTransportError struct{ err error }

func (e *retryableTransportError) Error() string { return e.err.Error() }
func (e *retryableTransportError) Unwrap() error { return e.err }
func (*retryableTransportError) Retryable() bool { return true }

func isRetryableStatus(status int) bool {
	return status == 408 || status == 409 || status == 425 || status == 429 || status >= 500
}

func doJSON(ctx context.Context, cfg Config, body []byte) ([]byte, string, error) {
	client := cfg.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.Endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if cfg.Token != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.Token)
	}
	if cfg.Debug {
		fmt.Fprintf(os.Stderr, "[capelin-go] >>> POST %s\n\n%s\n\n", cfg.Endpoint, body)
	}
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, "", ctx.Err()
		}
		return nil, "", &retryableTransportError{err: err}
	}
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 10*1024*1024))
	resp.Body.Close()
	if readErr != nil {
		return nil, "", &retryableTransportError{err: readErr}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		message := fmt.Sprintf("model request failed: %s: %s", resp.Status, strings.TrimSpace(string(raw)))
		if isRetryableStatus(resp.StatusCode) {
			return nil, "", &retryableHTTPError{status: resp.StatusCode, message: message}
		}
		return nil, "", errors.New(message)
	}
	if cfg.Debug {
		fmt.Fprintf(os.Stderr, "[capelin-go] <<< RESPONSE %s\n\n%s\n\n", resp.Status, raw)
	}
	return raw, resp.Status, nil
}

// ChatCompletions adapts the OpenAI-compatible chat payload.
type ChatCompletions struct{ cfg Config }

func NewChatCompletions(cfg Config) *ChatCompletions { return &ChatCompletions{cfg: cfg} }

type chatState struct{ messages []contracts.Message }

func (s *chatState) Messages() []contracts.Message {
	return append([]contracts.Message(nil), s.messages...)
}
func (p *ChatCompletions) Initialize(messages []contracts.Message, question string) contracts.TurnState {
	return p.InitializeWithContinuation(messages, question, nil)
}
func (*ChatCompletions) InitializeWithContinuation(messages []contracts.Message, question string, state *contracts.ContinuationState) contracts.TurnState {
	// Chat Completions continuation data is reconstructible from normalized
	// messages. Validate the envelope when present so a state from another
	// provider/version cannot accidentally be treated as native state.
	if !validContinuationEnvelope(state, chatContinuationProvider, messages) {
		state = nil
	}
	_ = state
	copied := cloneMessages(messages)
	return &chatState{messages: append(copied, contracts.Message{Role: "user", Content: question})}
}
func (*ChatCompletions) ContinuationState(state contracts.TurnState) *contracts.ContinuationState {
	s, ok := state.(*chatState)
	if !ok {
		return nil
	}
	data, _ := json.Marshal(chatContinuationData{MessagesHash: messagesHash(s.messages)})
	return &contracts.ContinuationState{Provider: chatContinuationProvider, Version: continuationVersion, Data: data}
}
func (p *ChatCompletions) ApplyResponse(state contracts.TurnState, response contracts.Completion) {
	s := state.(*chatState)
	s.messages = append(s.messages, response.(completionMessage).asMessage())
}
func (p *ChatCompletions) ApplyToolResults(state contracts.TurnState, results []contracts.ToolResult) {
	s := state.(*chatState)
	for _, result := range results {
		s.messages = append(s.messages, contracts.Message{Role: "tool", ToolCallID: result.Call.ID, Content: result.Output})
	}
}
func (*ChatCompletions) AppendUserPrompt(state contracts.TurnState, content string) {
	s := state.(*chatState)
	s.messages = append(s.messages, contracts.Message{Role: "user", Content: content})
}
func (*ChatCompletions) AppendFinalPrompt(state contracts.TurnState) {
	(&ChatCompletions{}).AppendUserPrompt(state, "[SYSTEM] Maximum tool iterations reached. Based on everything you have gathered so far, provide your best final answer now. Do not request any more tools.")
}

type chatRequest struct {
	Model      string           `json:"model"`
	Messages   []chatMessage    `json:"messages"`
	Tools      []contracts.Tool `json:"tools,omitempty"`
	ToolChoice string           `json:"tool_choice,omitempty"`
	Reasoning  string           `json:"reasoning_effort,omitempty"`
}

func (p *ChatCompletions) Complete(ctx context.Context, state contracts.TurnState, tools []contracts.Tool, model, reasoning string) (contracts.Completion, error) {
	if model == "" {
		model = p.cfg.Model
	}
	req := chatRequest{Model: model, Messages: chatMessages(state.Messages()), Tools: tools, Reasoning: reasoning}
	if len(tools) > 0 {
		req.ToolChoice = "auto"
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	raw, _, err := doJSON(ctx, p.cfg, body)
	if err != nil {
		return nil, err
	}
	var response struct {
		Choices []struct {
			Message contracts.CompletionMessage `json:"message"`
		} `json:"choices"`
		Data *struct {
			Choices []struct {
				Message contracts.CompletionMessage `json:"message"`
			} `json:"choices"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		return nil, err
	}
	choices := response.Choices
	if len(choices) == 0 && response.Data != nil {
		choices = response.Data.Choices
	}
	if len(choices) == 0 {
		return nil, errors.New("model returned no choices")
	}
	return completionMessage{message: choices[0].Message}, nil
}

// Responses adapts the Responses API, including its provider-specific output
// items used for continuation after a function call.
type Responses struct{ cfg Config }

func NewResponses(cfg Config) *Responses { return &Responses{cfg: cfg} }

type responsesState struct {
	messages []contracts.Message
	input    []json.RawMessage
}

type chatMessage struct {
	Role             string               `json:"role"`
	Content          string               `json:"content,omitempty"`
	ReasoningContent *string              `json:"reasoning_content,omitempty"`
	ToolCallID       string               `json:"tool_call_id,omitempty"`
	Name             string               `json:"name,omitempty"`
	ToolCalls        []contracts.ToolCall `json:"tool_calls,omitempty"`
}

type chatContinuationData struct {
	MessagesHash string `json:"messages_hash"`
}

func chatMessages(messages []contracts.Message) []chatMessage {
	result := make([]chatMessage, 0, len(messages))
	for _, message := range messages {
		wire := chatMessage{
			Role: message.Role, Content: message.Content, ToolCallID: message.ToolCallID,
			Name: message.Name, ToolCalls: append([]contracts.ToolCall(nil), message.ToolCalls...),
		}
		if message.Role == "assistant" && message.ReasoningContent != nil {
			value := *message.ReasoningContent
			wire.ReasoningContent = &value
		}
		result = append(result, wire)
	}
	return result
}

func (s *responsesState) Messages() []contracts.Message {
	return append([]contracts.Message(nil), s.messages...)
}
func (p *Responses) Initialize(messages []contracts.Message, question string) contracts.TurnState {
	return p.InitializeWithContinuation(messages, question, nil)
}
func (*Responses) InitializeWithContinuation(messages []contracts.Message, question string, state *contracts.ContinuationState) contracts.TurnState {
	conversation := append([]contracts.Message(nil), messages...)
	conversation = append(conversation, contracts.Message{Role: "user", Content: question})
	input := messagesToInput(messages)
	if restored := restoreResponsesInput(state, messages); restored != nil {
		input = restored
	}
	input = append(input, marshal(map[string]any{"role": "user", "content": question}))
	return &responsesState{messages: conversation, input: input}
}
func (*Responses) ContinuationState(state contracts.TurnState) *contracts.ContinuationState {
	s, ok := state.(*responsesState)
	if !ok {
		return nil
	}
	data, _ := json.Marshal(responsesContinuationData{
		Input:        cloneRawMessages(s.input),
		MessagesHash: messagesHash(s.messages),
	})
	return &contracts.ContinuationState{Provider: responsesContinuationProvider, Version: continuationVersion, Data: data}
}
func (p *Responses) ApplyResponse(state contracts.TurnState, response contracts.Completion) {
	s := state.(*responsesState)
	c := response.(completionMessage)
	s.input = append(s.input, c.outputItems...)
	s.messages = append(s.messages, c.asMessage())
}
func (p *Responses) ApplyToolResults(state contracts.TurnState, results []contracts.ToolResult) {
	s := state.(*responsesState)
	for _, result := range results {
		s.input = append(s.input, marshal(map[string]any{"type": "function_call_output", "call_id": result.Call.ID, "output": result.Output}))
		s.messages = append(s.messages, contracts.Message{Role: "tool", ToolCallID: result.Call.ID, Content: result.Output})
	}
}
func (*Responses) AppendUserPrompt(state contracts.TurnState, content string) {
	s := state.(*responsesState)
	s.input = append(s.input, marshal(map[string]any{"role": "user", "content": content}))
	s.messages = append(s.messages, contracts.Message{Role: "user", Content: content})
}
func (p *Responses) AppendFinalPrompt(state contracts.TurnState) {
	p.AppendUserPrompt(state, "[SYSTEM] Maximum tool iterations reached. Based on everything you have gathered so far, provide your best final answer now. Do not request any more tools.")
}

type responsesRequest struct {
	Model     string              `json:"model"`
	Input     []json.RawMessage   `json:"input"`
	Tools     []responsesTool     `json:"tools,omitempty"`
	Reasoning *responsesReasoning `json:"reasoning,omitempty"`
}
type responsesReasoning struct {
	Effort string `json:"effort"`
}
type responsesTool struct {
	Type        string         `json:"type"`
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters"`
}

func (p *Responses) Complete(ctx context.Context, state contracts.TurnState, tools []contracts.Tool, model, reasoning string) (contracts.Completion, error) {
	if model == "" {
		model = p.cfg.Model
	}
	req := responsesRequest{Model: model, Input: state.(*responsesState).input, Tools: responseTools(tools)}
	if strings.TrimSpace(reasoning) != "" {
		req.Reasoning = &responsesReasoning{Effort: reasoning}
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	raw, _, err := doJSON(ctx, p.cfg, body)
	if err != nil {
		return nil, err
	}
	return parseResponse(raw)
}

// completionMessage carries raw Responses output items while exposing only the
// normalized view to the engine.
type completionMessage struct {
	message     contracts.CompletionMessage
	outputItems []json.RawMessage
}

func (m completionMessage) Content() string {
	if m.message.Content == nil {
		return ""
	}
	return *m.message.Content
}
func (m completionMessage) ReasoningContent() string {
	if m.message.ReasoningContent == nil {
		return ""
	}
	return *m.message.ReasoningContent
}
func (m completionMessage) ToolCalls() []contracts.ToolCall { return m.message.ToolCalls }
func (m completionMessage) asMessage() contracts.Message {
	msg := contracts.Message{Role: m.message.Role, ToolCalls: m.message.ToolCalls}
	if m.message.Content != nil {
		msg.Content = *m.message.Content
	}
	if m.message.ReasoningContent != nil {
		value := *m.message.ReasoningContent
		msg.ReasoningContent = &value
	}
	return msg
}
func (m completionMessage) asMessagePtr() completionMessage { return m }

func messagesToInput(messages []contracts.Message) []json.RawMessage {
	input := make([]json.RawMessage, 0, len(messages))
	for _, message := range messages {
		if message.Role == "assistant" {
			if message.ReasoningContent != nil && strings.TrimSpace(*message.ReasoningContent) != "" {
				input = append(input, marshal(map[string]any{
					"type":    "reasoning",
					"summary": []any{map[string]any{"type": "summary_text", "text": *message.ReasoningContent}},
				}))
			}
			if message.Content != "" {
				input = append(input, marshal(map[string]any{"role": "assistant", "content": message.Content}))
			}
			for _, call := range message.ToolCalls {
				input = append(input, marshal(map[string]any{"type": "function_call", "call_id": call.ID, "name": call.Function.Name, "arguments": call.Function.Arguments}))
			}
			if len(message.ToolCalls) > 0 || message.Content != "" || message.ReasoningContent != nil {
				continue
			}
		}
		if message.Role == "tool" {
			input = append(input, marshal(map[string]any{"type": "function_call_output", "call_id": message.ToolCallID, "output": message.Content}))
			continue
		}
		input = append(input, marshal(map[string]any{"role": message.Role, "content": message.Content}))
	}
	return input
}
func responseTools(tools []contracts.Tool) []responsesTool {
	result := make([]responsesTool, 0, len(tools))
	for _, tool := range tools {
		result = append(result, responsesTool{Type: "function", Name: tool.Function.Name, Description: tool.Function.Description, Parameters: tool.Function.Parameters})
	}
	return result
}
func marshal(value any) json.RawMessage { raw, _ := json.Marshal(value); return raw }
func parseResponse(raw []byte) (contracts.Completion, error) {
	var response struct {
		Output           []json.RawMessage `json:"output"`
		OutputText       *string           `json:"output_text"`
		Reasoning        *string           `json:"reasoning"`
		ReasoningContent *string           `json:"reasoning_content"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		return nil, err
	}
	if len(response.Output) == 0 && (response.OutputText == nil || strings.TrimSpace(*response.OutputText) == "") {
		return nil, errors.New("model returned no output")
	}
	result := completionMessage{outputItems: append([]json.RawMessage(nil), response.Output...)}
	if response.ReasoningContent != nil {
		result.message.ReasoningContent = response.ReasoningContent
	} else {
		result.message.ReasoningContent = response.Reasoning
	}
	for _, item := range response.Output {
		var parsed struct {
			Type             string            `json:"type"`
			Name             string            `json:"name"`
			CallID           string            `json:"call_id"`
			Arguments        string            `json:"arguments"`
			Role             string            `json:"role"`
			OutputText       string            `json:"output_text"`
			Reasoning        *string           `json:"reasoning"`
			ReasoningContent *string           `json:"reasoning_content"`
			Content          []json.RawMessage `json:"content"`
			Summary          []json.RawMessage `json:"summary"`
		}
		if err := json.Unmarshal(item, &parsed); err != nil {
			return nil, fmt.Errorf("invalid responses output item: %w", err)
		}
		switch parsed.Type {
		case "message":
			result.message.Role = parsed.Role
			if parsed.ReasoningContent != nil {
				result.message.ReasoningContent = parsed.ReasoningContent
			} else if parsed.Reasoning != nil {
				result.message.ReasoningContent = parsed.Reasoning
			}
			if parsed.OutputText != "" {
				result.message.Content = stringPtr(result.message.Content, parsed.OutputText)
			}
			for _, part := range parsed.Content {
				text, kind := textPart(part)
				if text != "" && (kind == "output_text" || kind == "text" || kind == "") {
					result.message.Content = stringPtr(result.message.Content, text)
				}
			}
		case "reasoning":
			for _, summary := range parsed.Summary {
				text, _ := textPart(summary)
				if text != "" {
					result.message.ReasoningContent = stringPtr(result.message.ReasoningContent, text)
				}
			}
			if len(parsed.Summary) == 0 {
				if parsed.ReasoningContent != nil {
					result.message.ReasoningContent = stringPtr(result.message.ReasoningContent, *parsed.ReasoningContent)
				} else if parsed.Reasoning != nil {
					result.message.ReasoningContent = stringPtr(result.message.ReasoningContent, *parsed.Reasoning)
				}
			}
		case "function_call":
			if parsed.CallID == "" || parsed.Name == "" || parsed.Arguments == "" {
				return nil, errors.New("model returned malformed function_call output")
			}
			result.message.ToolCalls = append(result.message.ToolCalls, contracts.ToolCall{ID: parsed.CallID, Type: "function", Function: contracts.FunctionCall{Name: parsed.Name, Arguments: parsed.Arguments}})
		}
	}
	if response.OutputText != nil && strings.TrimSpace(*response.OutputText) != "" && result.message.Content == nil {
		result.message.Content = response.OutputText
	}
	if result.Content() == "" && len(result.ToolCalls()) == 0 {
		return nil, errors.New("model returned empty output")
	}
	if result.message.Role == "" {
		result.message.Role = "assistant"
	}
	return result, nil
}
func textPart(raw json.RawMessage) (string, string) {
	var obj struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &obj) == nil && obj.Text != "" {
		return obj.Text, obj.Type
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text, ""
	}
	return "", ""
}
func stringPtr(existing *string, text string) *string {
	if existing == nil {
		return &text
	}
	combined := *existing + text
	return &combined
}

type responsesContinuationData struct {
	Input        []json.RawMessage `json:"input"`
	MessagesHash string            `json:"messages_hash"`
}

func validContinuationEnvelope(state *contracts.ContinuationState, provider string, messages []contracts.Message) bool {
	if state == nil || state.Provider != provider || state.Version != continuationVersion {
		return false
	}
	var data chatContinuationData
	if err := json.Unmarshal(state.Data, &data); err != nil {
		return false
	}
	return data.MessagesHash == messagesHash(messages)
}

func restoreResponsesInput(state *contracts.ContinuationState, messages []contracts.Message) []json.RawMessage {
	if state == nil || state.Provider != responsesContinuationProvider || state.Version != continuationVersion {
		return nil
	}
	var data responsesContinuationData
	if err := json.Unmarshal(state.Data, &data); err != nil || data.MessagesHash != messagesHash(messages) || len(data.Input) == 0 {
		return nil
	}
	for _, item := range data.Input {
		if len(item) == 0 || !json.Valid(item) {
			return nil
		}
	}
	return cloneRawMessages(data.Input)
}

func messagesHash(messages []contracts.Message) string {
	raw, _ := json.Marshal(messages)
	hash := sha256.Sum256(raw)
	return hex.EncodeToString(hash[:])
}

func cloneMessages(messages []contracts.Message) []contracts.Message {
	if messages == nil {
		return nil
	}
	result := make([]contracts.Message, len(messages))
	copy(result, messages)
	for i := range result {
		result[i].ToolCalls = append([]contracts.ToolCall(nil), messages[i].ToolCalls...)
		if messages[i].ReasoningContent != nil {
			value := *messages[i].ReasoningContent
			result[i].ReasoningContent = &value
		}
	}
	return result
}

func cloneRawMessages(messages []json.RawMessage) []json.RawMessage {
	if messages == nil {
		return nil
	}
	result := make([]json.RawMessage, len(messages))
	for i, message := range messages {
		result[i] = append(json.RawMessage(nil), message...)
	}
	return result
}

var _ contracts.Provider = (*ChatCompletions)(nil)
var _ contracts.Provider = (*Responses)(nil)
