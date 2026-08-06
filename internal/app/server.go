package app

import (
	"capelin-go/internal/contracts"
	"capelin-go/internal/server"
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	asyncResultTTL           = server.AsyncResultTTL
	maxServerRequestBodySize = server.MaxRequestBodySize
	maxDataKeyLen            = server.MaxDataKeyLen
	maxDataValueSize         = server.MaxDataValueSize
	defaultDataTTLMinutes    = server.DefaultDataTTLMinutes
	maxDataTTLMinutes        = server.MaxDataTTLMinutes
)

// Kept as a narrow compatibility test seam. Production admission is owned by
// internal/server; compatibility handlers copy this channel into that module
// before serving a request.
var asyncSem = make(chan struct{}, server.AsyncMaxConcurrent)

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

func startServer(cfg config) error {
	activeServerPolicy = &cfg.securityPolicy
	proxyHTTPClient = cfg.securityPolicy.secureHTTPClient()
	proxyHTTPClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	serverAllowedTools := map[string]bool{
		toolWebSearch: true, toolFetchPage: true,
		toolCreateSubagent: true, toolRunSubagent: true, toolAwaitSubagent: true,
		toolListSubagents: true, toolReadSubagent: true, toolCancelSubagent: true,
	}
	serverCfg := cfg
	// Server requests are remote work: never carry a local idle hook into the
	// server composition, even when a caller constructs config directly instead
	// of using config.Load's server-mode exclusion.
	serverCfg.idleHookCommand = ""
	serverCfg.idleHookArgs = nil
	serverCfg.allowedTools = cloneAllowedTools(serverAllowedTools)
	a := &app{
		cfg:       serverCfg,
		client:    &client{endpoint: strings.TrimRight(cfg.endpoint, "/"), token: cfg.token, model: cfg.model, reasoning: cfg.reasoning, debug: cfg.debug, http: cfg.securityPolicy.secureHTTPClient()},
		toolset:   buildAgentTools(serverAllowedTools),
		dataStore: newDataStore(),
	}
	a.subagents = newSubagentManager(cfg.subagents, a.runSubagentSession)

	addr := ":" + strconv.Itoa(cfg.serverPort)
	srv := &http.Server{
		Addr: addr, Handler: newServerHandler(a, serverAllowedTools),
		ReadTimeout: 30 * time.Second, ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout: requestTimeout, IdleTimeout: 120 * time.Second, MaxHeaderBytes: 1 << 20,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	fmt.Fprintf(os.Stderr, "[capelin-go] Server listening on %s\n", addr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("server error: %w", err)
	}
	return nil
}

type serverExecutionRequest struct {
	remoteBase         string
	remoteToken        string
	model              string
	reasoning          string
	messages           []contracts.Message
	question           string
	serverAllowedTools map[string]bool
}

type serverExecutionResult struct {
	content   string
	reasoning string
}

type serverExecutor interface {
	Execute(context.Context, *serverExecutionRequest) (serverExecutionResult, error)
}

type serverExecutorFunc func(context.Context, *serverExecutionRequest) (serverExecutionResult, error)

func (f serverExecutorFunc) Execute(ctx context.Context, request *serverExecutionRequest) (serverExecutionResult, error) {
	return f(ctx, request)
}

type serverExecutorAdapter struct{ executor serverExecutor }

func (a serverExecutorAdapter) Execute(ctx context.Context, request *server.ExecutionRequest) (server.ExecutionResult, error) {
	if a.executor == nil {
		return server.ExecutionResult{}, context.Canceled
	}
	result, err := a.executor.Execute(ctx, appServerRequest(request))
	return server.ExecutionResult{Content: result.content, Reasoning: result.reasoning}, err
}

type applicationServerExecutor struct{ app *app }

func (e applicationServerExecutor) Execute(ctx context.Context, request *server.ExecutionRequest) (server.ExecutionResult, error) {
	if e.app == nil {
		return server.ExecutionResult{}, context.Canceled
	}
	serverApp, runtime := e.app.newServerExecutionApp(appServerRequest(request))
	_, content, reasoning, err := serverApp.runTurnLoop(ctx, request.Messages, request.Question, runtime, serverApp.toolset, false)
	if err != nil {
		return server.ExecutionResult{}, err
	}
	return server.ExecutionResult{Content: content, Reasoning: reasoning}, nil
}

func (a *app) serverExecutor() serverExecutor {
	return applicationServerExecutorForApp{app: a}
}

type applicationServerExecutorForApp struct{ app *app }

func (e applicationServerExecutorForApp) Execute(ctx context.Context, request *serverExecutionRequest) (serverExecutionResult, error) {
	result, err := applicationServerExecutor{app: e.app}.Execute(ctx, toServerRequest(request))
	return serverExecutionResult{content: result.Content, reasoning: result.Reasoning}, err
}

func toServerRequest(request *serverExecutionRequest) *server.ExecutionRequest {
	if request == nil {
		return nil
	}
	return &server.ExecutionRequest{
		RemoteBase: request.remoteBase, RemoteToken: request.remoteToken, Model: request.model,
		Reasoning: request.reasoning, Messages: request.messages, Question: request.question,
		AllowedTools: cloneAllowedTools(request.serverAllowedTools),
	}
}

func appServerRequest(request *server.ExecutionRequest) *serverExecutionRequest {
	if request == nil {
		return nil
	}
	return &serverExecutionRequest{
		remoteBase: request.RemoteBase, remoteToken: request.RemoteToken, model: request.Model,
		reasoning: request.Reasoning, messages: request.Messages, question: request.Question,
		serverAllowedTools: cloneAllowedTools(request.AllowedTools),
	}
}

func serverHandlerConfig(a *app, allowed map[string]bool, executor server.Executor) server.HandlerConfig {
	config := server.HandlerConfig{
		Model: a.cfg.model, Reasoning: a.cfg.reasoning, AllowedTools: cloneAllowedTools(allowed),
		AsyncTimeout: a.cfg.asyncTimeout, Store: a.dataStore.serverStore(), Executor: executor,
	}
	if a.cfg.securityEnabled {
		config.AuthorizeOrigin = a.cfg.securityPolicy.authorizeOrigin
		config.LogRejectedOrigin = a.cfg.securityPolicy.logRejectedOrigin
		config.AuthorizeTarget = func(raw string) error {
			_, err := a.cfg.securityPolicy.authorizeTarget(raw)
			return err
		}
		config.LogRejectedTarget = a.cfg.securityPolicy.logRejectedTarget
		config.ProxyHandler = http.HandlerFunc(proxyHandler)
	}
	return config
}

func newServerHandler(a *app, allowed map[string]bool) http.Handler {
	return newServerHandlerWithExecutor(a, allowed, a.serverExecutor())
}

func newServerHandlerWithExecutor(a *app, allowed map[string]bool, executor serverExecutor) http.Handler {
	server.SetAsyncSemaphore(asyncSem)
	return server.NewHandler(serverHandlerConfig(a, allowed, serverExecutorAdapter{executor: executor}))
}

func (a *app) handleChatCompletion(w http.ResponseWriter, r *http.Request, allowed map[string]bool) {
	newServerHandlerWithExecutor(a, allowed, a.serverExecutor()).ServeHTTP(w, r)
}

func (a *app) handleAsyncChatCompletion(w http.ResponseWriter, r *http.Request, allowed map[string]bool) {
	server.SetAsyncSemaphore(asyncSem)
	config := serverHandlerConfig(a, allowed, serverExecutorAdapter{executor: a.serverExecutor()})
	if a.asyncRunner != nil {
		config.AsyncOverride = func(id string, request *server.ExecutionRequest) {
			a.asyncRunner(id, appServerRequest(request))
		}
	}
	server.NewHandler(config).ServeHTTP(w, r)
}

func withCORS(next http.Handler) http.Handler                    { return server.WithCORS(next) }
func writeError(w http.ResponseWriter, code int, message string) { serverWriteError(w, code, message) }

func serverWriteError(w http.ResponseWriter, code int, message string) {
	server.WriteError(w, code, message)
}

func buildChatCompletionJSON(model, content, reasoning string) string {
	return server.BuildChatCompletionJSON(model, content, reasoning)
}

func writeChatCompletionResponse(w http.ResponseWriter, model, content, reasoning string) {
	server.WriteChatCompletionResponse(w, model, content, reasoning)
}

func generateUUID() string { return server.GenerateUUID() }

func (a *app) runAsyncTask(uuid, remoteBase, remoteToken, model, reasoning string, messages []contracts.Message, question string, allowed map[string]bool) {
	a.runAsyncExecution(uuid, &serverExecutionRequest{remoteBase: remoteBase, remoteToken: remoteToken, model: model, reasoning: reasoning, messages: messages, question: question, serverAllowedTools: cloneAllowedTools(allowed)})
}

func (a *app) runAsyncExecution(uuid string, execution *serverExecutionRequest) {
	delivery := server.NewDelivery(serverHandlerConfig(a, execution.serverAllowedTools, serverExecutorAdapter{executor: a.serverExecutor()}))
	delivery.RunAsync(uuid, toServerRequest(execution))
}

func (a *app) storeAsyncResult(uuid, data string) {
	server.NewDelivery(server.HandlerConfig{Store: a.dataStore.serverStore()}).StoreAsyncResult(uuid, data)
}

const asyncResultStorageError = `{"error":{"message":"async result exceeded storage limits","type":"async_error"}}`
