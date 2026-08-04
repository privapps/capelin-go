package app

import (
	"bytes"
	"capelin-go/internal/types"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
)

type retryableHTTPError struct {
	StatusCode int
	msg        string
}

func (e *retryableHTTPError) Error() string { return e.msg }

type retryableTransportError struct{ err error }

func (e *retryableTransportError) Error() string { return e.err.Error() }
func (e *retryableTransportError) Unwrap() error { return e.err }

type completionMessage struct {
	message     types.CompletionMessage
	outputItems []json.RawMessage
}

func (m *completionMessage) Content() string {
	if m.message.Content == nil {
		return ""
	}
	return *m.message.Content
}
func (m *completionMessage) ReasoningContent() string {
	if m.message.ReasoningContent == nil {
		return ""
	}
	return *m.message.ReasoningContent
}
func (m *completionMessage) ToolCalls() []types.ToolCall { return m.message.ToolCalls }
func (m *completionMessage) asMessage() types.Message {
	message := types.Message{Role: m.message.Role, ToolCalls: m.message.ToolCalls}
	if m.message.Content != nil {
		message.Content = *m.message.Content
	}
	return message
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

func (c *client) isResponsesEndpoint() bool {
	parsed, err := url.Parse(c.endpoint)
	if err != nil {
		return false
	}
	path := strings.TrimRight(parsed.Path, "/")
	return path == "/responses" || strings.HasSuffix(path, "/responses")
}

func messagesToResponsesInput(messages []types.Message) []json.RawMessage {
	input := make([]json.RawMessage, 0, len(messages))
	for _, message := range messages {
		if message.Role == "assistant" && len(message.ToolCalls) > 0 {
			if message.Content != "" {
				input = append(input, marshalResponsesItem(map[string]any{
					"role":    "assistant",
					"content": message.Content,
				}))
			}
			for _, call := range message.ToolCalls {
				input = append(input, marshalResponsesItem(map[string]any{
					"type":      "function_call",
					"call_id":   call.ID,
					"name":      call.Function.Name,
					"arguments": call.Function.Arguments,
				}))
			}
			continue
		}
		if message.Role == "tool" {
			input = append(input, marshalResponsesItem(map[string]any{
				"type":    "function_call_output",
				"call_id": message.ToolCallID,
				"output":  message.Content,
			}))
			continue
		}
		input = append(input, marshalResponsesItem(map[string]any{
			"role":    message.Role,
			"content": message.Content,
		}))
	}
	return input
}

func marshalResponsesItem(value any) json.RawMessage {
	raw, _ := json.Marshal(value)
	return raw
}

func responsesTools(tools []types.Tool) []responsesTool {
	result := make([]responsesTool, 0, len(tools))
	for _, tool := range tools {
		result = append(result, responsesTool{
			Type:        "function",
			Name:        tool.Function.Name,
			Description: tool.Function.Description,
			Parameters:  tool.Function.Parameters,
		})
	}
	return result
}

func (c *client) completeResponses(ctx context.Context, input []json.RawMessage, tools []types.Tool, model, reasoning string) (*completionMessage, error) {
	if model == "" {
		model = c.model
	}
	reqBody := responsesRequest{
		Model: model,
		Input: input,
		Tools: responsesTools(tools),
	}
	if strings.TrimSpace(reasoning) != "" {
		reqBody.Reasoning = &responsesReasoning{Effort: reasoning}
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	if c.debug {
		fmt.Fprintf(os.Stderr, "[capelin-go] >>> POST %s\n", c.endpoint)
		fmt.Fprintf(os.Stderr, "\n%s\n\n", string(body))
	}

	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &retryableTransportError{err: err}
	}
	rawBody, err := io.ReadAll(io.LimitReader(resp.Body, 10*1024*1024))
	resp.Body.Close()
	if err != nil {
		return nil, &retryableTransportError{err: err}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg := fmt.Sprintf("model request failed: %s: %s", resp.Status, strings.TrimSpace(string(rawBody)))
		if isRetryableStatus(resp.StatusCode) {
			return nil, &retryableHTTPError{StatusCode: resp.StatusCode, msg: msg}
		}
		return nil, errors.New(msg)
	}
	if c.debug {
		fmt.Fprintf(os.Stderr, "[capelin-go] <<< RESPONSE %s\n\n%s\n\n", resp.Status, string(rawBody))
	}
	return parseResponsesResponse(rawBody)
}

func parseResponsesResponse(raw []byte) (*completionMessage, error) {
	var response struct {
		Output     []json.RawMessage `json:"output"`
		OutputText *string           `json:"output_text"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		return nil, err
	}
	if len(response.Output) == 0 && (response.OutputText == nil || strings.TrimSpace(*response.OutputText) == "") {
		return nil, errors.New("model returned no output")
	}

	result := &completionMessage{}
	result.outputItems = append(result.outputItems, response.Output...)
	for _, item := range response.Output {
		var parsed struct {
			Type       string            `json:"type"`
			Name       string            `json:"name"`
			CallID     string            `json:"call_id"`
			Arguments  string            `json:"arguments"`
			Role       string            `json:"role"`
			OutputText string            `json:"output_text"`
			Content    []json.RawMessage `json:"content"`
			Summary    []json.RawMessage `json:"summary"`
		}
		if err := json.Unmarshal(item, &parsed); err != nil {
			return nil, fmt.Errorf("invalid responses output item: %w", err)
		}
		switch parsed.Type {
		case "message":
			result.message.Role = parsed.Role
			if parsed.OutputText != "" {
				result.message.Content = stringPointer(result.message.Content, parsed.OutputText)
			}
			for _, part := range parsed.Content {
				text, partType := responsesTextPart(part)
				if text == "" {
					continue
				}
				if partType == "output_text" || partType == "text" || partType == "" {
					result.message.Content = stringPointer(result.message.Content, text)
				}
			}
		case "reasoning":
			for _, summary := range parsed.Summary {
				text, _ := responsesTextPart(summary)
				if text != "" {
					result.message.ReasoningContent = stringPointer(result.message.ReasoningContent, text)
				}
			}
		case "function_call":
			if parsed.CallID == "" || parsed.Name == "" || parsed.Arguments == "" {
				return nil, errors.New("model returned malformed function_call output")
			}
			result.message.ToolCalls = append(result.message.ToolCalls, types.ToolCall{
				ID:   parsed.CallID,
				Type: "function",
				Function: types.FunctionCall{
					Name:      parsed.Name,
					Arguments: parsed.Arguments,
				},
			})
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

func responsesTextPart(raw json.RawMessage) (string, string) {
	var object struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &object); err == nil && object.Text != "" {
		return object.Text, object.Type
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text, ""
	}
	return "", ""
}

func stringPointer(existing *string, text string) *string {
	if existing == nil {
		return &text
	}
	combined := *existing + text
	return &combined
}

func isRetryableStatus(code int) bool { return code == 429 || code >= 500 }

func isRetryableError(err error) bool {
	if err == nil || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return false
	}
	var httpErr *retryableHTTPError
	if errors.As(err, &httpErr) {
		return true
	}
	var transportErr *retryableTransportError
	return errors.As(err, &transportErr)
}

func (c *client) complete(ctx context.Context, messages []types.Message, tools []types.Tool, model, reasoning string) (*completionMessage, error) {
	if c.isResponsesEndpoint() {
		return c.completeResponses(ctx, messagesToResponsesInput(messages), tools, model, reasoning)
	}
	if model == "" {
		model = c.model
	}
	request := types.Request{Model: model, Messages: messages, Tools: tools, ReasoningEffort: reasoning}
	if len(tools) > 0 {
		request.ToolChoice = "auto"
	}
	body, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &retryableTransportError{err: err}
	}
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 10*1024*1024))
	resp.Body.Close()
	if readErr != nil {
		return nil, &retryableTransportError{err: readErr}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		message := fmt.Sprintf("model request failed: %s: %s", resp.Status, strings.TrimSpace(string(raw)))
		if isRetryableStatus(resp.StatusCode) {
			return nil, &retryableHTTPError{StatusCode: resp.StatusCode, msg: message}
		}
		return nil, errors.New(message)
	}
	var decoded types.Response
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, err
	}
	choices := decoded.Choices
	if len(choices) == 0 && decoded.Data != nil {
		choices = decoded.Data.Choices
	}
	if len(choices) == 0 {
		return nil, errors.New("model returned no choices")
	}
	return &completionMessage{message: choices[0].Message}, nil
}

func (a *app) runResponsesTurnLoop(ctx context.Context, messages []types.Message, question string, runtime *agentRuntime, toolset []types.Tool, emitOutput bool) ([]types.Message, string, string, error) {
	// Keep the legacy entry point on the canonical protocol-neutral engine.
	return a.runTurnLoop(ctx, messages, question, runtime, toolset, emitOutput)
}
