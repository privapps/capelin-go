package providers

import (
	"bufio"
	"bytes"
	"context"
	cryptorand "crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"capelin-go/internal/contracts"
)

const (
	openCodeZenUserAgent = "opencode/1.18.32 ai-sdk/provider-utils/4.0.23 runtime/bun/1.3.14"
	openCodeZenClient    = "cli"
	openCodeZenProject   = "global"
	zenIDRandomChars     = 14
	zenIDCounterMask     = 1<<12 - 1
)

const zenIDAlphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

const zenIDTimeMask uint64 = 1<<48 - 1

var zenIDClock = struct {
	sync.Mutex
	lastMillis  uint64
	counter     uint16
	initialized bool
}{}

// OpenCodeZen adapts the OpenAI-compatible Zen endpoint. It embeds the
// protocol-neutral Chat Completions state behavior while replacing only the
// request/response transport with Zen's required streamed protocol.
type OpenCodeZen struct {
	*ChatCompletions
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
	sessionID, requestID, err := newZenIDs()
	if err != nil {
		return nil, fmt.Errorf("create OpenCode Zen request IDs: %w", err)
	}

	token := strings.TrimSpace(p.cfg.Token)
	anonymous := token == "" || strings.EqualFold(token, "public")
	wireTools, responseTools, err := zenTools(tools, anonymous)
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

func newZenIDs() (string, string, error) {
	timeField := nextZenIDTime()
	sessionTail, err := newZenIDRandomTail()
	if err != nil {
		return "", "", err
	}
	requestTail, err := newZenIDRandomTail()
	if err != nil {
		return "", "", err
	}
	return fmt.Sprintf("ses_%012x%s", zenIDTimeMask-timeField, sessionTail),
		fmt.Sprintf("msg_%012x%s", timeField, requestTail), nil
}

func nextZenIDTime() uint64 {
	// OpenCode stores Unix milliseconds in the high 36 bits and a monotonically
	// increasing sequence counter in the low 12 bits.
	currentMillis := uint64(time.Now().UnixMilli())
	zenIDClock.Lock()
	defer zenIDClock.Unlock()
	if !zenIDClock.initialized || currentMillis > zenIDClock.lastMillis {
		zenIDClock.lastMillis = currentMillis
		zenIDClock.counter = 1
		zenIDClock.initialized = true
	} else {
		zenIDClock.counter++
		if zenIDClock.counter > zenIDCounterMask {
			zenIDClock.lastMillis++
			zenIDClock.counter = 0
		}
	}
	return ((zenIDClock.lastMillis << 12) + uint64(zenIDClock.counter)) & zenIDTimeMask
}

func newZenIDRandomTail() (string, error) {
	result := make([]byte, zenIDRandomChars)
	for index := range result {
		var randomByte [1]byte
		for {
			if _, err := io.ReadFull(cryptorand.Reader, randomByte[:]); err != nil {
				return "", err
			}
			// 248 is the largest multiple of 62 below 256, avoiding modulo bias.
			if randomByte[0] < 248 {
				result[index] = zenIDAlphabet[int(randomByte[0])%len(zenIDAlphabet)]
				break
			}
		}
	}
	return string(result), nil
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

	message := CompletionMessage{Role: role}
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
		if name == "" {
			return nil, errors.New("Zen SSE stream called the inert read compatibility tool; no read capability was authorized")
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

func zenTools(tools []contracts.Tool, anonymous bool) ([]contracts.Tool, map[string]string, error) {
	result := make([]contracts.Tool, 0, len(tools)+1)
	responseTools := make(map[string]string, len(tools)+1)
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
	if anonymous {
		if _, hasAuthorizedRead := responseTools["read"]; !hasAuthorizedRead {
			result = append(result, contracts.Tool{
				Type: "function",
				Function: contracts.ToolSpec{
					Name:        "read",
					Description: "Compatibility placeholder. This tool is unavailable.",
					Parameters: map[string]any{
						"type":                 "object",
						"properties":           map[string]any{},
						"additionalProperties": false,
					},
				},
			})
			// An empty mapping marks the advertised compatibility tool as inert.
			responseTools["read"] = ""
		}
	}
	return result, responseTools, nil
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
		// Zen's wire vocabulary calls this tool "bash"; its description and
		// arguments still preserve Capelin's direct executable contract.
		return "bash"
	default:
		return name
	}
}

var _ contracts.Provider = (*OpenCodeZen)(nil)
