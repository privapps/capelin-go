package main

import (
	"capelin-go/internal/types"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

const maxServerRequestBodySize = 10 * 1024 * 1024

// serverRequest is the common request shape accepted by the server adapters.
type serverRequest struct {
	Model     string          `json:"model"`
	Messages  []types.Message `json:"messages"`
	Stream    bool            `json:"stream"`
	Reasoning struct {
		Effort string `json:"effort"`
	} `json:"reasoning,omitempty"`
}

type serverRequestError struct {
	code    int
	message string
}

func (e *serverRequestError) Error() string { return e.message }

// serverExecutionRequest is the normalized value shared by sync and async adapters.
// Its interface deliberately contains delivery-neutral execution facts only.
type serverExecutionRequest struct {
	remoteBase         string
	remoteToken        string
	model              string
	reasoning          string
	messages           []types.Message
	question           string
	serverAllowedTools map[string]bool
}

func (a *app) prepareServerRequest(r *http.Request, serverAllowedTools map[string]bool, pathPrefix string) (*serverExecutionRequest, error) {
	if r.Method != http.MethodPost {
		return nil, &serverRequestError{code: http.StatusMethodNotAllowed, message: "method not allowed"}
	}

	remoteBase, err := resolveServerEndpoint(strings.TrimPrefix(r.URL.Path, pathPrefix), r.URL.Query().Get("endpoint"))
	if err != nil {
		return nil, err
	}

	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		return nil, &serverRequestError{code: http.StatusBadRequest, message: "Authorization: Bearer <token> header is required"}
	}
	remoteToken := strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
	if remoteToken == "" {
		return nil, &serverRequestError{code: http.StatusBadRequest, message: "Authorization: Bearer <token> header is required"}
	}

	body, readErr := io.ReadAll(io.LimitReader(r.Body, maxServerRequestBodySize+1))
	if readErr != nil {
		return nil, &serverRequestError{code: http.StatusBadRequest, message: "failed to read request body"}
	}
	if len(body) > maxServerRequestBodySize {
		return nil, &serverRequestError{code: http.StatusBadRequest, message: "request body exceeds maximum size of 10MB"}
	}
	var req serverRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, &serverRequestError{code: http.StatusBadRequest, message: "invalid JSON: " + err.Error()}
	}
	if len(req.Messages) == 0 {
		return nil, &serverRequestError{code: http.StatusBadRequest, message: "messages array is required and must not be empty"}
	}
	if req.Stream {
		return nil, &serverRequestError{code: http.StatusBadRequest, message: "streaming is not supported; set stream to false"}
	}

	model := strings.TrimSpace(req.Model)
	if model == "" {
		model = a.cfg.model
	}
	reasoning := a.cfg.reasoning
	if req.Reasoning.Effort != "" {
		reasoning = req.Reasoning.Effort
	}

	messages := make([]types.Message, len(req.Messages))
	copy(messages, req.Messages)
	if messages[0].Role == "system" {
		messages[0].Content += "\n\nOnly web_search and fetch_page tools are available. No file, code execution, or skill tools."
	} else {
		messages = append([]types.Message{{Role: "system", Content: serverModeSystemPrompt}}, messages...)
	}
	question := ""
	if messages[len(messages)-1].Role == "user" {
		question = messages[len(messages)-1].Content
		messages = messages[:len(messages)-1]
	}

	return &serverExecutionRequest{
		remoteBase:         remoteBase,
		remoteToken:        remoteToken,
		model:              model,
		reasoning:          reasoning,
		messages:           messages,
		question:           question,
		serverAllowedTools: cloneAllowedTools(serverAllowedTools),
	}, nil
}

func resolveServerEndpoint(path, queryEndpoint string) (string, error) {
	path = strings.TrimPrefix(path, "/")
	if isServerEndpoint(path) {
		return path, nil
	}
	if strings.HasPrefix(path, "~") {
		hexStr := strings.TrimPrefix(path, "~")
		decoded, err := hex.DecodeString(hexStr)
		if err == nil && isServerEndpoint(string(decoded)) {
			return string(decoded), nil
		}
	}
	if endpoint := strings.TrimSpace(queryEndpoint); endpoint != "" {
		if isServerEndpoint(endpoint) {
			return endpoint, nil
		}
	}
	return "", &serverRequestError{
		code:    http.StatusBadRequest,
		message: "endpoint required: use path /https%3A%2F%2Fexample.com/v1/chat/completions (or http), /~<hex-encoded-URL>, or ?endpoint=https://example.com/v1/chat/completions",
	}
}

func isServerEndpoint(endpoint string) bool {
	return strings.HasPrefix(endpoint, "http://") || strings.HasPrefix(endpoint, "https://")
}

func writeServerRequestError(w http.ResponseWriter, err error) bool {
	requestErr, ok := err.(*serverRequestError)
	if !ok {
		return false
	}
	writeError(w, requestErr.code, requestErr.message)
	return true
}

func (a *app) newServerExecutionApp(execution *serverExecutionRequest) (*app, *agentRuntime) {
	serverCfg := a.cfg
	serverCfg.allowedTools = cloneAllowedTools(execution.serverAllowedTools)
	httpClient := serverHTTPClient
	if a.client != nil && a.client.http != nil {
		httpClient = a.client.http
	}
	serverApp := &app{
		cfg:       serverCfg,
		client:    &client{endpoint: strings.TrimRight(execution.remoteBase, "/"), token: execution.remoteToken, model: execution.model, reasoning: execution.reasoning, debug: a.cfg.debug, http: httpClient},
		skills:    a.skills,
		toolset:   buildAgentTools(execution.serverAllowedTools),
		dataStore: a.dataStore,
	}
	subagentCfg := a.cfg.subagents
	subagentCfg.Model = execution.model
	subagentCfg.ReasoningEffort = execution.reasoning
	serverApp.subagents = newSubagentManager(subagentCfg, serverApp.runSubagentSession)
	runtime := serverApp.rootRuntime()
	runtime.model = execution.model
	runtime.reasoning = execution.reasoning
	return serverApp, runtime
}
