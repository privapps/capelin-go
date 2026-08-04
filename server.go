package main

import (
	"capelin-go/internal/types"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
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

	serverCfg := cfg
	serverCfg.allowedTools = cloneAllowedTools(serverAllowedTools)
	a := &app{
		cfg: serverCfg,
		client: &client{
			endpoint:  strings.TrimRight(cfg.endpoint, "/"),
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

	addr := ":" + strconv.Itoa(cfg.serverPort)
	srv := &http.Server{
		Addr:              addr,
		Handler:           newServerHandler(a, serverAllowedTools),
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

// newServerHandler assembles the production HTTP seam used by both delivery
// adapters. Keeping route selection here lets application-level tests exercise
// endpoint intake, async acceptance, and /data polling without opening a port.
func newServerHandler(a *app, serverAllowedTools map[string]bool) http.Handler {
	mux := http.NewServeMux()
	// Catch-all pattern: any POST to /<remoteURL> is handled.
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
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, PUT, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		w.Header().Set("Access-Control-Max-Age", "600")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (a *app) handleChatCompletion(w http.ResponseWriter, r *http.Request, serverAllowedTools map[string]bool) {
	execution, err := a.prepareServerRequest(r, serverAllowedTools, "")
	if err != nil {
		writeServerRequestError(w, err)
		return
	}
	serverApp, runtime := a.newServerExecutionApp(execution)
	_, result, reasoning, err := serverApp.runTurnLoop(r.Context(), execution.messages, execution.question, runtime, serverApp.toolset, false)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeChatCompletionResponse(w, execution.model, result, reasoning)
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

	// Reserve capacity before reading/parsing the potentially large body. Keep
	// the channel local: tests and future configuration reloads may replace the
	// package-level semaphore, but an admitted task must release the semaphore
	// that admitted it.
	sem := asyncSem
	select {
	case sem <- struct{}{}:
	default:
		w.Header().Set("Retry-After", "1")
		writeError(w, http.StatusTooManyRequests, "async capacity exhausted")
		return
	}
	admitted := true
	defer func() {
		if admitted {
			<-sem
		}
	}()

	execution, err := a.prepareServerRequest(r, serverAllowedTools, "/async")
	if err != nil {
		writeServerRequestError(w, err)
		return
	}

	// Generate UUID and return immediately.
	uuid := generateUUID()
	// Transfer ownership of the admitted slot to the background task before
	// launching it. This prevents a fast task from releasing the slot before
	// the handler's deferred cleanup observes the transfer.
	admitted = false

	go func() {
		defer func() { <-sem }() // release admitted slot

		defer func() {
			if r := recover(); r != nil {
				errResp, _ := json.Marshal(map[string]any{
					"error": map[string]any{
						"message": fmt.Sprintf("internal panic: %v", r),
						"type":    "async_error",
					},
				})
				a.storeAsyncResult(uuid, string(errResp))
			}
		}()

		runAsync := a.runAsyncExecution
		if a.asyncRunner != nil {
			runAsync = a.asyncRunner
		}
		runAsync(uuid, execution)
	}()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(map[string]string{"id": uuid})
}

// runAsyncTask retains the narrow test seam used by existing callers while
// delegating all runtime construction to the shared normalized request path.
func (a *app) runAsyncTask(uuid, remoteBase, remoteToken, model, reasoning string, messages []types.Message, question string, serverAllowedTools map[string]bool) {
	a.runAsyncExecution(uuid, &serverExecutionRequest{
		remoteBase:         remoteBase,
		remoteToken:        remoteToken,
		model:              model,
		reasoning:          reasoning,
		messages:           messages,
		question:           question,
		serverAllowedTools: cloneAllowedTools(serverAllowedTools),
	})
}

func (a *app) runAsyncExecution(uuid string, execution *serverExecutionRequest) {
	serverApp, runtime := a.newServerExecutionApp(execution)

	timeout := 15 * time.Minute
	if a.cfg.asyncTimeout > 0 {
		timeout = a.cfg.asyncTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	_, result, reasoningResult, err := serverApp.runTurnLoop(ctx, execution.messages, execution.question, runtime, serverApp.toolset, false)

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
		data = buildChatCompletionJSON(execution.model, result, reasoningResult)
	}

	a.storeAsyncResult(uuid, data)
}

const asyncResultStorageError = `{"error":{"message":"async result exceeded storage limits","type":"async_error"}}`

func (a *app) storeAsyncResult(uuid, data string) {
	if a.dataStore.Put(uuid, data, asyncResultTTL) {
		return
	}
	if a.dataStore.Put(uuid, asyncResultStorageError, asyncResultTTL) {
		return
	}
	fmt.Fprintf(os.Stderr, "failed to store async result %q\n", uuid)
}
