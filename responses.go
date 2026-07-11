package main

import (
	"bytes"
	"capelin-go/internal/types"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"
)

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

	var lastErr error
	for attempt := 0; attempt < completeMaxAttempts; attempt++ {
		if attempt > 0 {
			delay := completeRetryBase * time.Duration(1<<(attempt-1))
			delay += time.Duration(rand.Int63n(int64(delay) / 2))
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(delay):
			}
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
			lastErr = err
			continue
		}
		rawBody, err := io.ReadAll(io.LimitReader(resp.Body, 10*1024*1024))
		resp.Body.Close()
		if err != nil {
			lastErr = err
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			msg := fmt.Sprintf("model request failed: %s: %s", resp.Status, strings.TrimSpace(string(rawBody)))
			if isRetryableStatus(resp.StatusCode) {
				lastErr = &retryableHTTPError{StatusCode: resp.StatusCode, msg: msg}
				continue
			}
			return nil, errors.New(msg)
		}
		if c.debug {
			fmt.Fprintf(os.Stderr, "[capelin-go] <<< RESPONSE %s\n\n%s\n\n", resp.Status, string(rawBody))
		}
		return parseResponsesResponse(rawBody)
	}
	return nil, lastErr
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

func (a *app) runResponsesTurnLoop(ctx context.Context, messages []types.Message, question string, runtime *agentRuntime, toolset []types.Tool, emitOutput bool) ([]types.Message, string, string, error) {
	input := messagesToResponsesInput(messages)
	input = append(input, marshalResponsesItem(map[string]any{"role": "user", "content": question}))

	maxIterations := defaultMaxIterations
	if runtime != nil && runtime.maxToolIterations > 0 {
		maxIterations = runtime.maxToolIterations
	}
	runtimeModel, runtimeReasoning := a.client.model, a.client.reasoning
	if runtime != nil && runtime.model != "" {
		runtimeModel, runtimeReasoning = runtime.model, runtime.reasoning
	}
	lastContent := ""
	var reasoningBuf strings.Builder
	agentID := rootAgentID
	if runtime != nil && strings.TrimSpace(runtime.sessionID) != "" {
		agentID = runtime.sessionID
	}
	sink := a.sink
	if sink == nil {
		sink = &stdioSink{}
	}

	for iter := 0; iter < maxIterations; iter++ {
		if iter == maxIterations-3 && maxIterations > 3 {
			input = append(input, marshalResponsesItem(map[string]any{
				"role":    "user",
				"content": fmt.Sprintf("[SYSTEM] You have %d iterations remaining. Wrap up and produce a final answer now.", maxIterations-iter),
			}))
		}

		var resp *completionMessage
		var lastErr error
		for attempt := 0; attempt < 3; attempt++ {
			if attempt > 0 {
				delay := 2 * time.Second * time.Duration(1<<(attempt-1))
				delay += time.Duration(rand.Int63n(int64(delay) / 2))
				if emitOutput {
					sink.WriteSystem(agentID, fmt.Sprintf("[tool] model request failed (429/5xx), retrying in %v…", delay))
				}
				select {
				case <-ctx.Done():
					return messages, "", "", ctx.Err()
				case <-time.After(delay):
				}
			}
			resp, lastErr = a.client.completeResponses(ctx, input, toolset, runtimeModel, runtimeReasoning)
			if lastErr == nil {
				break
			}
			if !isRetryableError(lastErr) {
				return messages, "", "", lastErr
			}
		}
		if lastErr != nil {
			return messages, "", "", lastErr
		}

		input = append(input, resp.outputItems...)
		if content := strings.TrimSpace(resp.Content()); content != "" {
			lastContent = content
			if emitOutput {
				sink.WriteContent(agentID, content)
			}
		}
		if reasoning := strings.TrimSpace(resp.ReasoningContent()); reasoning != "" {
			if reasoningBuf.Len() > 0 {
				reasoningBuf.WriteString("\n\n")
			}
			fmt.Fprintf(&reasoningBuf, "[Turn %d]\nThinking: %s", iter+1, reasoning)
		}
		messages = append(messages, resp.asMessage())
		toolCalls := resp.ToolCalls()
		if len(toolCalls) == 0 {
			if emitOutput {
				sink.WriteSystem(agentID, "")
			}
			return messages, lastContent, reasoningBuf.String(), nil
		}

		type toolResult struct {
			call    types.ToolCall
			out     string
			isError bool
		}
		results := make([]toolResult, len(toolCalls))
		g, gctx := errgroup.WithContext(ctx)
		g.SetLimit(a.cfg.toolMaxParallel)
		for i, call := range toolCalls {
			i, call := i, call
			g.Go(func() error {
				if emitOutput {
					sink.WriteToolCall(agentID, call.Function.Name, call.Function.Arguments)
				}
				timeoutSec := a.cfg.toolTimeoutSec
				if toolTimeout := parseToolTimeout(call); toolTimeout > 0 {
					timeoutSec = toolTimeout
				}
				for attempt := 0; attempt <= 1; attempt++ {
					toolCtx, cancel := context.WithTimeout(gctx, time.Duration(timeoutSec)*time.Second)
					out, err := a.runToolForRuntime(toolCtx, runtime, call)
					cancel()
					if err != nil {
						if attempt == 0 && a.cfg.toolRetryOnTimeout && errors.Is(err, context.DeadlineExceeded) {
							if emitOutput {
								sink.WriteSystem(agentID, fmt.Sprintf("[tool] %s timed out, retrying…", call.Function.Name))
							}
							continue
						}
						results[i] = toolResult{call: call, out: fmt.Sprintf("Tool error: %v", err), isError: true}
						return nil
					}
					results[i] = toolResult{call: call, out: out}
					return nil
				}
				return nil
			})
		}
		g.Wait()
		for _, result := range results {
			if emitOutput {
				sink.WriteToolResult(agentID, result.call.Function.Name, result.isError, result.out)
			}
			input = append(input, marshalResponsesItem(map[string]any{
				"type":    "function_call_output",
				"call_id": result.call.ID,
				"output":  result.out,
			}))
			messages = append(messages, types.Message{
				Role:       "tool",
				ToolCallID: result.call.ID,
				Content:    result.out,
			})
		}
		if reasoningBuf.Len() > 0 {
			reasoningBuf.WriteString("\n\n")
		}
		fmt.Fprintf(&reasoningBuf, "[Turn %d]\nTool calls:\n", iter+1)
		for _, result := range results {
			args := truncateStr(result.call.Function.Arguments, 200)
			fmt.Fprintf(&reasoningBuf, "  %s(%s)\n", result.call.Function.Name, args)
			if summary := extractToolSummary(result.call.Function.Name, result.out, result.isError); summary != "" {
				fmt.Fprintf(&reasoningBuf, "  > %s\n", summary)
			}
		}
	}

	if !a.cfg.finalOnly {
		fmt.Fprintf(os.Stderr, "[capelin-go] Maximum tool iterations (%d) reached; requesting final answer.\n", maxIterations)
	}
	input = append(input, marshalResponsesItem(map[string]any{
		"role":    "user",
		"content": "[SYSTEM] Maximum tool iterations reached. Based on everything you have gathered so far, provide your best final answer now. Do not request any more tools.",
	}))
	resp, err := a.client.completeResponses(ctx, input, nil, runtimeModel, runtimeReasoning)
	if err != nil {
		if lastContent != "" {
			return messages, lastContent, reasoningBuf.String(), nil
		}
		return messages, "", "", fmt.Errorf("exceeded maximum tool iterations (%d) and final-answer call failed: %w", maxIterations, err)
	}
	if content := strings.TrimSpace(resp.Content()); content != "" {
		if emitOutput {
			sink.WriteContent(agentID, content)
			sink.WriteSystem(agentID, "")
		}
		messages = append(messages, resp.asMessage())
		return messages, content, reasoningBuf.String(), nil
	}
	return messages, lastContent, reasoningBuf.String(), nil
}
