package server

import (
	"capelin-go/internal/contracts"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

const MaxRequestBodySize = 10 * 1024 * 1024

const ServerModeSystemPrompt = `You are an execution-focused AI assistant with web search capabilities.

Primary objective:
- Complete the task fully in a single response whenever possible.
- Use web_search to find relevant information.
- Use fetch_page to read specific URLs when you have a target.
- Minimize unnecessary back-and-forth.

Behavior rules:
- Prefer action over clarification.
- If information is missing but non-critical, choose sensible defaults and state them briefly.
- Provide complete, directly usable outputs.
- Structure responses clearly.
- Be concise but thorough.

Tool efficiency rules:
- Limit web_search calls to at most 5 per task; prefer broad, precise queries over many narrow ones.
- Do not retry the same search intent with only minor query variations.
- If the first search yields insufficient results, widen the query instead of repeating it.
- Prefer fetch_page on a known URL over a new web_search when you already have a relevant link.

Output policy:
- Return final answers, not partial work.
- Avoid hedging language.
- Avoid excessive disclaimers.
- Use markdown formatting for readability.`

type requestPayload struct {
	Model     string              `json:"model"`
	Messages  []contracts.Message `json:"messages"`
	Stream    bool                `json:"stream"`
	Reasoning struct {
		Effort string `json:"effort"`
	} `json:"reasoning,omitempty"`
}

// RequestError is an HTTP-compatible normalization failure.
type RequestError struct {
	Code    int
	Message string
}

func (e *RequestError) Error() string { return e.Message }

// ExecutionRequest is the normalized, delivery-neutral input consumed by an
// application executor. It contains no HTTP writer, provider, or tool object.
type ExecutionRequest struct {
	RemoteBase   string
	RemoteToken  string
	Model        string
	Reasoning    string
	Messages     []contracts.Message
	Question     string
	AllowedTools map[string]bool
}

// IntakeConfig supplies only defaults and the tool policy for normalization.
type IntakeConfig struct {
	Model             string
	Reasoning         string
	AllowedTools      map[string]bool
	AuthorizeTarget   func(string) error
	LogRejectedTarget func(string)
}

// NormalizeRequest applies the same authorization, body, endpoint, default,
// validation, and server-tool policy to synchronous and asynchronous paths.
func NormalizeRequest(r *http.Request, cfg IntakeConfig, pathPrefix string) (*ExecutionRequest, error) {
	if r.Method != http.MethodPost {
		return nil, &RequestError{Code: http.StatusMethodNotAllowed, Message: "method not allowed"}
	}
	remoteBase, err := ResolveEndpoint(strings.TrimPrefix(r.URL.Path, pathPrefix), r.URL.Query().Get("endpoint"))
	if err != nil {
		return nil, err
	}
	if cfg.AuthorizeTarget != nil {
		if err := cfg.AuthorizeTarget(remoteBase); err != nil {
			if cfg.LogRejectedTarget != nil {
				cfg.LogRejectedTarget(remoteBase)
			}
			return nil, &RequestError{Code: http.StatusForbidden, Message: "target not allowed"}
		}
	}
	authorization := r.Header.Get("Authorization")
	if !strings.HasPrefix(authorization, "Bearer ") {
		return nil, &RequestError{Code: http.StatusBadRequest, Message: "Authorization: Bearer <token> header is required"}
	}
	remoteToken := strings.TrimSpace(strings.TrimPrefix(authorization, "Bearer "))
	if remoteToken == "" {
		return nil, &RequestError{Code: http.StatusBadRequest, Message: "Authorization: Bearer <token> header is required"}
	}
	body, readErr := io.ReadAll(io.LimitReader(r.Body, MaxRequestBodySize+1))
	if readErr != nil {
		return nil, &RequestError{Code: http.StatusBadRequest, Message: "failed to read request body"}
	}
	if len(body) > MaxRequestBodySize {
		return nil, &RequestError{Code: http.StatusBadRequest, Message: "request body exceeds maximum size of 10MB"}
	}
	var payload requestPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, &RequestError{Code: http.StatusBadRequest, Message: "invalid JSON: " + err.Error()}
	}
	if len(payload.Messages) == 0 {
		return nil, &RequestError{Code: http.StatusBadRequest, Message: "messages array is required and must not be empty"}
	}
	if payload.Stream {
		return nil, &RequestError{Code: http.StatusBadRequest, Message: "streaming is not supported; set stream to false"}
	}
	model := strings.TrimSpace(payload.Model)
	if model == "" {
		model = cfg.Model
	}
	reasoning := cfg.Reasoning
	if payload.Reasoning.Effort != "" {
		reasoning = payload.Reasoning.Effort
	}
	messages := make([]contracts.Message, len(payload.Messages))
	copy(messages, payload.Messages)
	if messages[0].Role == "system" {
		messages[0].Content += "\n\nOnly web_search and fetch_page tools are available. No file, code execution, or skill tools."
	} else {
		messages = append([]contracts.Message{{Role: "system", Content: ServerModeSystemPrompt}}, messages...)
	}
	question := ""
	if messages[len(messages)-1].Role == "user" {
		question = messages[len(messages)-1].Content
		messages = messages[:len(messages)-1]
	}
	return &ExecutionRequest{
		RemoteBase: remoteBase, RemoteToken: remoteToken, Model: model, Reasoning: reasoning,
		Messages: messages, Question: question, AllowedTools: cloneAllowedTools(cfg.AllowedTools),
	}, nil
}

// ResolveEndpoint accepts the historical encoded-path, hex-path, and query
// forms used by the OpenAI-compatible server.
func ResolveEndpoint(path, queryEndpoint string) (string, error) {
	path = strings.TrimPrefix(path, "/")
	if IsEndpoint(path) {
		return path, nil
	}
	if strings.HasPrefix(path, "~") {
		decoded, err := hex.DecodeString(strings.TrimPrefix(path, "~"))
		if err == nil && IsEndpoint(string(decoded)) {
			return string(decoded), nil
		}
	}
	if endpoint := strings.TrimSpace(queryEndpoint); endpoint != "" && IsEndpoint(endpoint) {
		return endpoint, nil
	}
	return "", &RequestError{
		Code:    http.StatusBadRequest,
		Message: "endpoint required: use path /https%3A%2F%2Fexample.com/v1/chat/completions (or http), /~<hex-encoded-URL>, or ?endpoint=https://example.com/v1/chat/completions",
	}
}

func IsEndpoint(endpoint string) bool {
	return strings.HasPrefix(endpoint, "http://") || strings.HasPrefix(endpoint, "https://")
}

func WriteRequestError(w http.ResponseWriter, err error) bool {
	requestErr, ok := err.(*RequestError)
	if !ok {
		return false
	}
	writeError(w, requestErr.Code, requestErr.Message)
	return true
}

func cloneAllowedTools(in map[string]bool) map[string]bool {
	out := make(map[string]bool, len(in))
	for name, enabled := range in {
		if enabled {
			out[name] = true
		}
	}
	return out
}
