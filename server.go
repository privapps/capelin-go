package main

import (
	"capelin-go/internal/types"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const asyncResultTTL = 1 * time.Hour
const asyncMaxConcurrent = 16

var asyncSem = make(chan struct{}, asyncMaxConcurrent)

// serverHTTPClient is reused across all server-mode requests to preserve
// TCP connections and HTTP/2 streams.
var serverHTTPClient = &http.Client{
	Timeout: requestTimeout,
	Transport: &http.Transport{
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   10,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	},
}

const serverModeSystemPrompt = `You are an execution-focused AI assistant with web search capabilities.

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

func startServer(cfg config) error {
	// In server mode, web_search, fetch_page, and subagent tools are available.
	serverAllowedTools := map[string]bool{
		toolWebSearch:      true,
		toolFetchPage:      true,
		toolCreateSubagent: true,
		toolRunSubagent:    true,
		toolAwaitSubagent:  true,
		toolListSubagents:  true,
		toolReadSubagent:   true,
		toolCancelSubagent: true,
	}

	a := &app{
		cfg: cfg,
		client: &client{
			baseURL:   strings.TrimRight(cfg.baseURL, "/"),
			token:     cfg.token,
			model:     cfg.model,
			reasoning: cfg.reasoning,
			debug:     cfg.debug,
			http:      serverHTTPClient,
		},
		skills:    nil,
		toolset:   buildAgentTools(serverAllowedTools),
		dataStore: newDataStore(),
	}
	a.subagents = newSubagentManager(cfg.subagents, a.runSubagentSession)

	mux := http.NewServeMux()
	// Catch-all pattern: any POST to /<remoteURL> is handled
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		a.handleChatCompletion(w, r, serverAllowedTools)
	})
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, `{"status":"ok"}`)
	})
	mux.HandleFunc("/data", a.dataHandler)
	// Async variant: returns UUID immediately, result stored in /data.
	mux.HandleFunc("/async/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		a.handleAsyncChatCompletion(w, r, serverAllowedTools)
	})

	// CORS middleware for browser-based clients (e.g. data.html).
	corsHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, PUT, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		mux.ServeHTTP(w, r)
	})

	addr := ":" + strconv.Itoa(cfg.serverPort)
	srv := &http.Server{
		Addr:              addr,
		Handler:           corsHandler,
		ReadTimeout:       30 * time.Second,
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      requestTimeout, // generous: LLM turn-loop can take minutes
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20, // 1MB
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		srv.Shutdown(shutdownCtx)
	}()

	fmt.Fprintf(os.Stderr, "[capelin-go] Server listening on %s\n", addr)
	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		return fmt.Errorf("server error: %w", err)
	}
	return nil
}

// serverRequest wraps the incoming OpenAI-format request with extracted header info.
type serverRequest struct {
	Model    string          `json:"model"`
	Messages []types.Message `json:"messages"`
	Stream   bool            `json:"stream"`
	Reasoning struct {
		Effort string `json:"effort"`
	} `json:"reasoning,omitempty"`
}

func (a *app) handleChatCompletion(w http.ResponseWriter, r *http.Request, serverAllowedTools map[string]bool) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	// Extract remote URL: try path first (URL-encoded), then query parameter.
	// Path example: /https%3A%2F%2Fexample.com/v1/chat/completions
	// Query example: ?base-url=https://example.com/v1
	remoteBase := ""

	// Try path: URL-encoded endpoint (includes /chat/completions)
	// Example: /https%3A%2F%2Fopencode.ai/zen/v1/chat/completions
	path := r.URL.Path
	if path != "/" && path != "" {
		decoded := r.URL.Path // Go automatically decodes %XX in Path
		decoded = strings.TrimPrefix(decoded, "/")
		if strings.HasPrefix(decoded, "http://") || strings.HasPrefix(decoded, "https://") {
			remoteBase = decoded
		}
	}

	// Try hex-encoded path: /~<hex-encoded URL>
	// Example: /~68747470733a2f2f6f70656e636f64652e61692f7a656e2f76312f636861742f636f6d706c6574696f6e73
	if remoteBase == "" && strings.HasPrefix(path, "/~") {
		hexStr := strings.TrimPrefix(path, "/~")
		if decoded, err := hex.DecodeString(hexStr); err == nil {
			url := string(decoded)
			if strings.HasPrefix(url, "http://") || strings.HasPrefix(url, "https://") {
				remoteBase = url
			}
		}
	}

	// Fall back to query parameter: ?endpoint=https://example.com/v1/chat/completions
	if remoteBase == "" {
		remoteBase = strings.TrimSpace(r.URL.Query().Get("endpoint"))
	}

	if remoteBase == "" {
		writeError(w, http.StatusBadRequest, "endpoint required: use path /https%3A%2F%2Fexample.com/v1/chat/completions (or http), /~<hex-encoded-URL>, or ?endpoint=https://example.com/v1/chat/completions")
		return
	}

	// Extract bearer token from Authorization header
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		writeError(w, http.StatusBadRequest, "Authorization: Bearer <token> header is required")
		return
	}
	remoteToken := strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
	if remoteToken == "" {
		writeError(w, http.StatusBadRequest, "Authorization: Bearer <token> header is required")
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 10*1024*1024))
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read request body")
		return
	}

	var req serverRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}

	if len(req.Messages) == 0 {
		writeError(w, http.StatusBadRequest, "messages array is required and must not be empty")
		return
	}

	if req.Stream {
		writeError(w, http.StatusBadRequest, "streaming is not supported; set stream to false")
		return
	}

	model := strings.TrimSpace(req.Model)
	if model == "" {
		model = a.cfg.model
	}

	reasoning := a.cfg.reasoning
	if req.Reasoning.Effort != "" {
		reasoning = req.Reasoning.Effort
	}

	// Build a client targeting the remote LLM.
	// Strip /chat/completions from endpoint since complete() re-appends it.
	baseURL := strings.TrimSuffix(remoteBase, "/chat/completions")
	remoteClient := &client{
		baseURL:   strings.TrimRight(baseURL, "/"),
		token:     remoteToken,
		model:     model,
		reasoning: reasoning,
		debug:     a.cfg.debug,
		http:      serverHTTPClient,
	}

	// Build server-mode app with the remote client.
	serverApp := &app{
		cfg:     a.cfg,
		client:  remoteClient,
		skills:  a.skills,
		toolset: buildAgentTools(serverAllowedTools),
	}

	// Create subagent manager with model from request (overrides default config model).
	subagentCfg := a.cfg.subagents
	subagentCfg.Model = model
	subagentCfg.ReasoningEffort = reasoning
	serverApp.subagents = newSubagentManager(subagentCfg, serverApp.runSubagentSession)

	// Create runtime with the model from the request (overrides default config model).
	runtime := serverApp.rootRuntime()
	runtime.model = model
	runtime.reasoning = reasoning

	// Build system prompt: use first message if it's a system message, otherwise use server default.
	messages := make([]types.Message, len(req.Messages))
	copy(messages, req.Messages)
	if len(messages) > 0 && messages[0].Role == "system" {
		// Keep the user's system prompt but append tool restriction note.
		messages[0].Content = messages[0].Content + "\n\nOnly web_search and fetch_page tools are available. No file, code execution, or skill tools."
	} else {
		messages = append([]types.Message{{Role: "system", Content: serverModeSystemPrompt}}, messages...)
	}

	// Run the turn loop. Pop the last user message and pass it as the question
	// to avoid appending an empty user message.
	question := ""
	if len(messages) > 0 && messages[len(messages)-1].Role == "user" {
		question = messages[len(messages)-1].Content
		messages = messages[:len(messages)-1]
	}
	_, result, reasoning, err := serverApp.runTurnLoop(r.Context(), messages, question, runtime, serverApp.toolset, false)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeChatCompletionResponse(w, model, result, reasoning)
}

func writeChatCompletionResponse(w http.ResponseWriter, model, content, reasoning string) {
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprint(w, buildChatCompletionJSON(model, content, reasoning))
}

func buildChatCompletionJSON(model, content, reasoning string) string {
	message := map[string]string{
		"role":    "assistant",
		"content": content,
	}
	if reasoning != "" {
		message["reasoning"] = reasoning
	}

	resp := map[string]any{
		"id":      fmt.Sprintf("capelin-%d", time.Now().UnixNano()),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]any{
			{
				"index":         0,
				"message":       message,
				"finish_reason": "stop",
			},
		},
	}

	b, err := json.Marshal(resp)
	if err != nil {
		return fmt.Sprintf(`{"error":{"message":"internal error","type":"async_error"}}`)
	}
	return string(b)
}

func writeError(w http.ResponseWriter, code int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    "server_error",
			"code":    code,
		},
	})
}

func generateUUID() string {
	var buf [16]byte
	rand.Read(buf[:])
	buf[6] = (buf[6] & 0x0f) | 0x40 // version 4
	buf[8] = (buf[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		buf[0:4], buf[4:6], buf[6:8], buf[8:10], buf[10:16])
}

func (a *app) handleAsyncChatCompletion(w http.ResponseWriter, r *http.Request, serverAllowedTools map[string]bool) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	// Strip /async prefix to get the same path shape as handleChatCompletion.
	asyncPath := strings.TrimPrefix(r.URL.Path, "/async")
	if asyncPath == "" {
		asyncPath = "/"
	}

	// Extract remote URL from the path (after stripping /async).
	remoteBase := ""

	// Try path: URL-encoded endpoint
	path := asyncPath
	if path != "/" && path != "" {
		decoded := strings.TrimPrefix(path, "/")
		if strings.HasPrefix(decoded, "http://") || strings.HasPrefix(decoded, "https://") {
			remoteBase = decoded
		}
	}

	// Try hex-encoded path: /~<hex-encoded URL>
	if remoteBase == "" && strings.HasPrefix(path, "/~") {
		hexStr := strings.TrimPrefix(path, "/~")
		if decoded, err := hex.DecodeString(hexStr); err == nil {
			url := string(decoded)
			if strings.HasPrefix(url, "http://") || strings.HasPrefix(url, "https://") {
				remoteBase = url
			}
		}
	}

	// Fall back to query parameter
	if remoteBase == "" {
		remoteBase = strings.TrimSpace(r.URL.Query().Get("endpoint"))
	}

	if remoteBase == "" {
		writeError(w, http.StatusBadRequest, "endpoint required: use /async/https%3A%2F%2Fexample.com/v1/chat/completions, /async/~<hex-encoded-URL>, or /async/?endpoint=...")
		return
	}

	// Extract bearer token
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		writeError(w, http.StatusBadRequest, "Authorization: Bearer <token> header is required")
		return
	}
	remoteToken := strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
	if remoteToken == "" {
		writeError(w, http.StatusBadRequest, "Authorization: Bearer <token> header is required")
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 10*1024*1024))
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read request body")
		return
	}

	var req serverRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}

	if len(req.Messages) == 0 {
		writeError(w, http.StatusBadRequest, "messages array is required and must not be empty")
		return
	}

	if req.Stream {
		writeError(w, http.StatusBadRequest, "streaming is not supported; set stream to false")
		return
	}

	// Generate UUID and return immediately.
	uuid := generateUUID()

	// Prepare everything needed for background execution.
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
	if len(messages) > 0 && messages[0].Role == "system" {
		messages[0].Content = messages[0].Content + "\n\nOnly web_search and fetch_page tools are available. No file, code execution, or skill tools."
	} else {
		messages = append([]types.Message{{Role: "system", Content: serverModeSystemPrompt}}, messages...)
	}
	question := ""
	if len(messages) > 0 && messages[len(messages)-1].Role == "user" {
		question = messages[len(messages)-1].Content
		messages = messages[:len(messages)-1]
	}

	go func() {
		asyncSem <- struct{}{}        // acquire slot
		defer func() { <-asyncSem }() // release slot

		defer func() {
			if r := recover(); r != nil {
				errResp, _ := json.Marshal(map[string]any{
					"error": map[string]any{
						"message": fmt.Sprintf("internal panic: %v", r),
						"type":    "async_error",
					},
				})
				a.dataStore.Put(uuid, string(errResp), asyncResultTTL)
			}
		}()

		a.runAsyncTask(uuid, remoteBase, remoteToken, model, reasoning, messages, question, serverAllowedTools)
	}()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(map[string]string{"id": uuid})
}

func (a *app) runAsyncTask(uuid, remoteBase, remoteToken, model, reasoning string, messages []types.Message, question string, serverAllowedTools map[string]bool) {
	baseURL := strings.TrimSuffix(remoteBase, "/chat/completions")
	remoteClient := &client{
		baseURL:   strings.TrimRight(baseURL, "/"),
		token:     remoteToken,
		model:     model,
		reasoning: reasoning,
		debug:     a.cfg.debug,
		http:      serverHTTPClient,
	}

	serverApp := &app{
		cfg:     a.cfg,
		client:  remoteClient,
		skills:  a.skills,
		toolset: buildAgentTools(serverAllowedTools),
	}
	subagentCfg := a.cfg.subagents
	subagentCfg.Model = model
	subagentCfg.ReasoningEffort = reasoning
	serverApp.subagents = newSubagentManager(subagentCfg, serverApp.runSubagentSession)

	runtime := serverApp.rootRuntime()
	runtime.model = model
	runtime.reasoning = reasoning

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	_, result, reasoningResult, err := serverApp.runTurnLoop(ctx, messages, question, runtime, serverApp.toolset, false)

	var data string
	if err != nil {
		errResp, marshalErr := json.Marshal(map[string]any{
			"error": map[string]any{
				"message": err.Error(),
				"type":    "async_error",
			},
		})
		if marshalErr != nil {
			data = fmt.Sprintf(`{"error":{"message":"internal error","type":"async_error"}}`)
		} else {
			data = string(errResp)
		}
	} else {
		data = buildChatCompletionJSON(model, result, reasoningResult)
	}

	a.dataStore.Put(uuid, data, asyncResultTTL)
}
