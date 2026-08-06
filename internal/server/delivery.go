package server

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sync"
	"time"
)

const (
	AsyncResultTTL      = time.Hour
	AsyncMaxConcurrent  = 16
	defaultAsyncTimeout = 15 * time.Minute
)

var asyncSem = make(chan struct{}, AsyncMaxConcurrent)

// SetAsyncSemaphore replaces the admission semaphore. It exists to let
// composed tests deterministically exercise capacity behavior.
func SetAsyncSemaphore(sem chan struct{}) {
	if sem != nil {
		asyncSemaphoreMu.Lock()
		asyncSem = sem
		asyncSemaphoreMu.Unlock()
	}
}

var asyncSemaphoreMu sync.RWMutex

func currentAsyncSemaphore() chan struct{} {
	asyncSemaphoreMu.RLock()
	defer asyncSemaphoreMu.RUnlock()
	return asyncSem
}

// ExecutionResult is the protocol-neutral output rendered by the server.
type ExecutionResult struct {
	Content   string
	Reasoning string
}

type Executor interface {
	Execute(context.Context, *ExecutionRequest) (ExecutionResult, error)
}

type ExecutorFunc func(context.Context, *ExecutionRequest) (ExecutionResult, error)

func (f ExecutorFunc) Execute(ctx context.Context, request *ExecutionRequest) (ExecutionResult, error) {
	return f(ctx, request)
}

// HandlerConfig wires delivery to an application executor and data store.
// The server package never constructs a provider, agent, or tool dispatcher.
type HandlerConfig struct {
	Model             string
	Reasoning         string
	AllowedTools      map[string]bool
	AsyncTimeout      time.Duration
	Store             *DataStore
	Executor          Executor
	AsyncOverride     func(string, *ExecutionRequest)
	AuthorizeOrigin   func(string) (string, bool)
	LogRejectedOrigin func(string)
	AuthorizeTarget   func(string) error
	LogRejectedTarget func(string)
	Proxy             *ProxyConfig
}

// Delivery is the synchronous/asynchronous HTTP delivery adapter.
type Delivery struct {
	config       HandlerConfig
	store        *DataStore
	allowedTools map[string]bool
	executor     Executor
}

func NewDelivery(config HandlerConfig) *Delivery {
	store := config.Store
	if store == nil {
		store = NewDataStore()
	}
	return &Delivery{
		config: config, store: store, allowedTools: cloneAllowedTools(config.AllowedTools), executor: config.Executor,
	}
}

// NewHandler assembles health, sync, async, data, and CORS routes.
func NewHandler(config HandlerConfig) http.Handler {
	delivery := NewDelivery(config)
	mux := http.NewServeMux()
	if config.Proxy != nil {
		mux.Handle("/-/", NewProxyHandler(*config.Proxy))
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		delivery.HandleSync(w, r)
	})
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintln(w, `{"status":"ok"}`)
	})
	mux.Handle("/data", NewDataHandler(delivery.store))
	mux.HandleFunc("/async/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		delivery.HandleAsync(w, r)
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if config.AuthorizeOrigin != nil && origin != "" {
			canonical, ok := config.AuthorizeOrigin(origin)
			w.Header().Add("Vary", "Origin")
			if !ok {
				if config.LogRejectedOrigin != nil {
					config.LogRejectedOrigin(origin)
				}
				writeError(w, http.StatusForbidden, "origin not allowed")
				return
			}
			w.Header().Set("Access-Control-Allow-Origin", canonical)
		} else if config.AuthorizeOrigin == nil {
			w.Header().Set("Access-Control-Allow-Origin", "*")
		}
		w.Header().Set("Access-Control-Allow-Methods", "GET, PUT, POST, PATCH, DELETE, HEAD, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		w.Header().Set("Access-Control-Max-Age", "600")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func WithCORS(next http.Handler) http.Handler {
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

func (d *Delivery) intake(r *http.Request, pathPrefix string) (*ExecutionRequest, error) {
	return NormalizeRequest(r, IntakeConfig{
		Model: d.config.Model, Reasoning: d.config.Reasoning, AllowedTools: d.allowedTools,
		AuthorizeTarget: d.config.AuthorizeTarget, LogRejectedTarget: d.config.LogRejectedTarget,
	}, pathPrefix)
}

func (d *Delivery) HandleSync(w http.ResponseWriter, r *http.Request) {
	deferredPanic := true
	defer func() {
		if recovered := recover(); recovered != nil && deferredPanic {
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("internal panic: %v", recovered))
		}
	}()
	execution, err := d.intake(r, "")
	if err != nil {
		WriteRequestError(w, err)
		return
	}
	if d.executor == nil {
		writeError(w, http.StatusInternalServerError, "server executor is nil")
		return
	}
	result, err := d.executor.Execute(r.Context(), execution)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	deferredPanic = false
	WriteChatCompletionResponse(w, execution.Model, result.Content, result.Reasoning)
}

func WriteChatCompletionResponse(w http.ResponseWriter, model, content, reasoning string) {
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprint(w, BuildChatCompletionJSON(model, content, reasoning))
}

func BuildChatCompletionJSON(model, content, reasoning string) string {
	message := map[string]string{"role": "assistant", "content": content}
	if reasoning != "" {
		message["reasoning"] = reasoning
	}
	response := map[string]any{
		"id": fmt.Sprintf("capelin-%d", time.Now().UnixNano()), "object": "chat.completion", "created": time.Now().Unix(), "model": model,
		"choices": []map[string]any{{"index": 0, "message": message, "finish_reason": "stop"}},
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		return `{"error":{"message":"internal error","type":"async_error"}}`
	}
	return string(encoded)
}

func WriteError(w http.ResponseWriter, code int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": message, "type": "server_error", "code": code}})
}

func writeError(w http.ResponseWriter, code int, message string) { WriteError(w, code, message) }

func GenerateUUID() string {
	var buffer [16]byte
	_, _ = rand.Read(buffer[:])
	buffer[6] = (buffer[6] & 0x0f) | 0x40
	buffer[8] = (buffer[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", buffer[0:4], buffer[4:6], buffer[6:8], buffer[8:10], buffer[10:16])
}

func (d *Delivery) HandleAsync(w http.ResponseWriter, r *http.Request) {
	sem := currentAsyncSemaphore()
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
	execution, err := d.intake(r, "/async")
	if err != nil {
		WriteRequestError(w, err)
		return
	}
	id := GenerateUUID()
	admitted = false
	go func() {
		defer func() { <-sem }()
		defer func() {
			if recovered := recover(); recovered != nil {
				errResponse, _ := json.Marshal(map[string]any{"error": map[string]any{"message": fmt.Sprintf("internal panic: %v", recovered), "type": "async_error"}})
				d.StoreAsyncResult(id, string(errResponse))
			}
		}()
		if d.config.AsyncOverride != nil {
			d.config.AsyncOverride(id, execution)
			return
		}
		d.RunAsync(id, execution)
	}()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]string{"id": id})
}

func (d *Delivery) RunAsync(id string, execution *ExecutionRequest) {
	timeout := defaultAsyncTimeout
	if d.config.AsyncTimeout > 0 {
		timeout = d.config.AsyncTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if d.executor == nil {
		d.StoreAsyncResult(id, `{"error":{"message":"server executor is nil","type":"async_error"}}`)
		return
	}
	result, err := d.executor.Execute(ctx, execution)
	if err != nil {
		errorResponse, marshalErr := json.Marshal(map[string]any{"error": map[string]any{"message": err.Error(), "type": "async_error"}})
		if marshalErr != nil {
			d.StoreAsyncResult(id, `{"error":{"message":"internal error","type":"async_error"}}`)
		} else {
			d.StoreAsyncResult(id, string(errorResponse))
		}
		return
	}
	d.StoreAsyncResult(id, BuildChatCompletionJSON(execution.Model, result.Content, result.Reasoning))
}

func (d *Delivery) StoreAsyncResult(id, value string) {
	if d.store.Put(id, value, AsyncResultTTL) {
		return
	}
	const fallback = `{"error":{"message":"async result exceeded storage limits","type":"async_error"}}`
	if d.store.Put(id, fallback, AsyncResultTTL) {
		return
	}
	fmt.Fprintf(os.Stderr, "failed to store async result %q\n", id)
}
