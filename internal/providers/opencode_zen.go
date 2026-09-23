package providers

import (
	"bufio"
	"bytes"
	"context"
	cryptorand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"

	"capelin-go/internal/contracts"
)

const (
	openCodeZenUserAgent = "opencode/1.18.32 ai-sdk/provider-utils/4.0.23 runtime/bun/1.3.14"
	openCodeZenClient    = "cli"
	openCodeZenProject   = "global"
	zenIDRandomBytes     = 6
	zenIDRandomChars     = 14
)

const zenIDAlphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

// OpenCodeZen adapts the OpenAI-compatible Zen endpoint. It embeds the
// protocol-neutral Chat Completions state behavior while replacing only the
// request/response transport with Zen's required streamed protocol.
type OpenCodeZen struct {
	*ChatCompletions

	sessionMu sync.Mutex
	sessionID string
}

func NewOpenCodeZen(cfg Config) *OpenCodeZen {
	return &OpenCodeZen{ChatCompletions: NewChatCompletions(cfg)}
}

type zenRequest struct {
	Model      string           `json:"model"`
	Messages   []chatMessage    `json:"messages"`
	Tools      []contracts.Tool `json:"tools,omitempty"`
	ToolChoice string           `json:"tool_choice,omitempty"`
	Reasoning  string           `json:"reasoning_effort,omitempty"`
	Stream     bool             `json:"stream"`
}

type zenStreamChunk struct {
	Choices []struct {
		Delta struct {
			Role             string  `json:"role"`
			Content          *string `json:"content"`
			Reasoning        *string `json:"reasoning"`
			ReasoningContent *string `json:"reasoning_content"`
			ToolCalls        []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
}

func (p *OpenCodeZen) Complete(ctx context.Context, state contracts.TurnState, tools []contracts.Tool, model, reasoning string) (contracts.Completion, error) {
	if model == "" {
		model = p.cfg.Model
	}
	sessionID, err := p.getSessionID()
	if err != nil {
		return nil, fmt.Errorf("create OpenCode Zen session ID: %w", err)
	}
	requestID, err := newZenID("msg")
	if err != nil {
		return nil, fmt.Errorf("create OpenCode Zen request ID: %w", err)
	}

	wireTools, responseTools, err := zenTools(tools, strings.EqualFold(strings.TrimSpace(p.cfg.Token), "public"))
	if err != nil {
		return nil, err
	}
	req := zenRequest{
		Model:     model,
		Messages:  zenMessages(state.Messages(), reasoning),
		Tools:     wireTools,
		Reasoning: zenReasoningEffort(reasoning),
		Stream:    true,
	}
	if len(wireTools) > 0 {
		req.ToolChoice = "auto"
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	return p.completeStream(ctx, body, sessionID, requestID, responseTools)
}

func (p *OpenCodeZen) getSessionID() (string, error) {
	p.sessionMu.Lock()
	defer p.sessionMu.Unlock()
	if p.sessionID == "" {
		var err error
		p.sessionID, err = newZenID("ses")
		if err != nil {
			return "", err
		}
	}
	return p.sessionID, nil
}

func newZenID(prefix string) (string, error) {
	hexPart := make([]byte, zenIDRandomBytes)
	if _, err := io.ReadFull(cryptorand.Reader, hexPart); err != nil {
		return "", err
	}
	base62Part := make([]byte, zenIDRandomChars)
	randomBytes := make([]byte, zenIDRandomChars)
	if _, err := io.ReadFull(cryptorand.Reader, randomBytes); err != nil {
		return "", err
	}
	for index, value := range randomBytes {
		base62Part[index] = zenIDAlphabet[int(value)%len(zenIDAlphabet)]
	}
	return prefix + "_" + hex.EncodeToString(hexPart) + string(base62Part), nil
}

func (p *OpenCodeZen) completeStream(ctx context.Context, body []byte, sessionID, requestID string, responseTools map[string]string) (contracts.Completion, error) {
	client := p.cfg.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.cfg.Endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "*/*")
	req.Header.Set("User-Agent", openCodeZenUserAgent)
	req.Header.Set("x-opencode-client", openCodeZenClient)
	req.Header.Set("x-opencode-project", openCodeZenProject)
	req.Header.Set("x-opencode-session", sessionID)
	req.Header.Set("x-opencode-request", requestID)
	if p.cfg.Token != "" {
		req.Header.Set("Authorization", "Bearer "+p.cfg.Token)
	}
	if p.cfg.Debug {
		fmt.Fprintf(os.Stderr, "[capelin-go] >>> POST %s\n\n%s\n\n", p.cfg.Endpoint, body)
	}

	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &retryableTransportError{err: err}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 10*1024*1024))
		resp.Body.Close()
		if readErr != nil {
			return nil, &retryableTransportError{err: readErr}
		}
		return nil, modelHTTPError(resp.StatusCode, resp.Status, raw)
	}
	defer resp.Body.Close()

	completion, err := parseZenSSE(ctx, resp.Body, responseTools)
	if err != nil {
		return nil, err
	}
	if p.cfg.Debug {
		fmt.Fprintf(os.Stderr, "[capelin-go] <<< RESPONSE %s (buffered Zen SSE)\n\ncontent=%q\nreasoning=%q\ntool_calls=%v\n\n", resp.Status, completion.Content(), completion.ReasoningContent(), completion.ToolCalls())
	}
	return completion, nil
}

func parseZenSSE(ctx context.Context, body io.Reader, responseTools map[string]string) (contracts.Completion, error) {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 64*1024), 10*1024*1024)

	var content strings.Builder
	var reasoning strings.Builder
	var role string
	var toolCalls []zenToolCall
	var sawEvent bool
	var completed bool
	var dataLines []string
	contentSeen := false
	reasoningSeen := false

	processEvent := func() error {
		if len(dataLines) == 0 {
			return nil
		}
		data := strings.Join(dataLines, "\n")
		dataLines = nil
		if strings.TrimSpace(data) == "[DONE]" {
			completed = true
			return nil
		}

		var chunk zenStreamChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			return fmt.Errorf("invalid SSE event: %w", err)
		}
		sawEvent = true
		if len(chunk.Choices) == 0 {
			return nil
		}
		choice := chunk.Choices[0]
		if choice.Delta.Role != "" {
			role = choice.Delta.Role
		}
		if choice.Delta.Content != nil {
			content.WriteString(*choice.Delta.Content)
			contentSeen = true
		}
		if choice.Delta.ReasoningContent != nil {
			reasoning.WriteString(*choice.Delta.ReasoningContent)
			reasoningSeen = true
		} else if choice.Delta.Reasoning != nil {
			reasoning.WriteString(*choice.Delta.Reasoning)
			reasoningSeen = true
		}
		for _, delta := range choice.Delta.ToolCalls {
			if delta.Index < 0 {
				return fmt.Errorf("Zen SSE stream contained invalid negative tool-call index %d", delta.Index)
			}
			for len(toolCalls) <= delta.Index {
				toolCalls = append(toolCalls, zenToolCall{})
			}
			call := &toolCalls[delta.Index]
			if delta.ID != "" {
				call.ID = delta.ID
			}
			if delta.Type != "" {
				call.Type = delta.Type
			}
			if delta.Function.Name != "" {
				call.Name = delta.Function.Name
			}
			call.Arguments.WriteString(delta.Function.Arguments)
		}
		if choice.FinishReason != nil && strings.TrimSpace(*choice.FinishReason) != "" {
			completed = true
		}
		return nil
	}

	for scanner.Scan() {
		line := strings.TrimSuffix(scanner.Text(), "\r")
		if line == "" {
			if err := processEvent(); err != nil {
				return nil, err
			}
			continue
		}
		if strings.HasPrefix(line, "data:") {
			value := strings.TrimPrefix(line, "data:")
			if strings.HasPrefix(value, " ") {
				value = value[1:]
			}
			dataLines = append(dataLines, value)
			continue
		}
		if strings.HasPrefix(line, ":") || strings.HasPrefix(line, "event:") || strings.HasPrefix(line, "id:") || strings.HasPrefix(line, "retry:") {
			continue
		}
		return nil, fmt.Errorf("invalid SSE line: %q", line)
	}
	if err := scanner.Err(); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("read Zen SSE stream: %w", err)
	}
	if err := processEvent(); err != nil {
		return nil, err
	}
	if !sawEvent {
		return nil, errors.New("Zen SSE stream contained no events")
	}
	if !completed {
		return nil, errors.New("Zen SSE stream ended before completion")
	}

	message := contracts.CompletionMessage{Role: role}
	if message.Role == "" {
		message.Role = "assistant"
	}
	if contentSeen {
		value := content.String()
		message.Content = &value
	}
	if reasoningSeen {
		value := reasoning.String()
		message.ReasoningContent = &value
	}
	for _, call := range toolCalls {
		if call.ID == "" || call.Name == "" {
			return nil, errors.New("Zen SSE stream contained an incomplete tool call")
		}
		arguments := strings.TrimSpace(call.Arguments.String())
		if arguments == "" {
			arguments = "{}"
		}
		if !json.Valid([]byte(arguments)) {
			return nil, errors.New("Zen SSE stream contained malformed tool-call arguments")
		}
		name, ok := responseTools[call.Name]
		if !ok {
			return nil, fmt.Errorf("Zen SSE stream returned unsupported tool %q", call.Name)
		}
		typeName := call.Type
		if typeName == "" {
			typeName = "function"
		}
		message.ToolCalls = append(message.ToolCalls, contracts.ToolCall{
			ID: call.ID, Type: typeName,
			Function: contracts.FunctionCall{Name: name, Arguments: arguments},
		})
	}
	return completionMessage{message: message}, nil
}

type zenToolCall struct {
	ID        string
	Type      string
	Name      string
	Arguments strings.Builder
}

func zenMessages(messages []contracts.Message, reasoning string) []chatMessage {
	result := chatMessages(messages, reasoning)
	for i := range result {
		for j := range result[i].ToolCalls {
			result[i].ToolCalls[j].Function.Name = zenToolName(result[i].ToolCalls[j].Function.Name)
		}
	}
	return result
}

func zenTools(tools []contracts.Tool, requireFreeTierTools bool) ([]contracts.Tool, map[string]string, error) {
	result := make([]contracts.Tool, 0, len(tools)+2)
	responseTools := make(map[string]string, len(tools)+2)
	appendTool := func(tool contracts.Tool) error {
		canonical := strings.TrimSpace(tool.Function.Name)
		if canonical == "" {
			return errors.New("Zen tool mapping requires a tool name")
		}
		wireName := zenToolName(canonical)
		if existing, ok := responseTools[wireName]; ok && existing != canonical {
			return fmt.Errorf("Zen tool name %q maps to both %q and %q", wireName, existing, canonical)
		}
		if _, ok := responseTools[wireName]; ok {
			return nil
		}
		result = append(result, tool)
		result[len(result)-1].Function.Name = wireName
		responseTools[wireName] = canonical
		return nil
	}
	for _, tool := range tools {
		if err := appendTool(tool); err != nil {
			return nil, nil, err
		}
	}
	if requireFreeTierTools {
		for _, tool := range zenFreeTierTools() {
			if _, ok := responseTools[zenToolName(tool.Function.Name)]; ok {
				continue
			}
			if err := appendTool(tool); err != nil {
				return nil, nil, err
			}
		}
	}
	return result, responseTools, nil
}

func zenFreeTierTools() []contracts.Tool {
	return []contracts.Tool{
		{
			Type: "function",
			Function: contracts.ToolSpec{
				Name:        "read_file",
				Description: "Read a file from the workspace.",
				Parameters:  map[string]any{"type": "object"},
			},
		},
		{
			Type: "function",
			Function: contracts.ToolSpec{
				Name:        "execute_program",
				Description: "Execute a local program.",
				Parameters:  map[string]any{"type": "object"},
			},
		},
	}
}

func zenReasoningEffort(reasoning string) string {
	if strings.EqualFold(strings.TrimSpace(reasoning), "max") {
		return "high"
	}
	return reasoning
}

func zenToolName(name string) string {
	switch name {
	case "read_file":
		return "read"
	case "write_file":
		return "write"
	case "edit_file":
		return "edit"
	case "execute_program":
		return "bash"
	default:
		return name
	}
}

var _ contracts.Provider = (*OpenCodeZen)(nil)
