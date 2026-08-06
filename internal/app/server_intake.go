package app

import (
	"capelin-go/internal/server"
	"net/http"
	"strings"
)

const serverModeSystemPrompt = server.ServerModeSystemPrompt

type serverRequestError struct {
	code    int
	message string
}

func (e *serverRequestError) Error() string { return e.message }

func (a *app) prepareServerRequest(r *http.Request, allowed map[string]bool, pathPrefix string) (*server.ExecutionRequest, error) {
	request, err := server.NormalizeRequest(r, server.IntakeConfig{Model: a.cfg.model, Reasoning: a.cfg.reasoning, AllowedTools: allowed}, pathPrefix)
	if err != nil {
		if requestErr, ok := err.(*server.RequestError); ok {
			return nil, &serverRequestError{code: requestErr.Code, message: requestErr.Message}
		}
		return nil, err
	}
	return request, nil
}

func resolveServerEndpoint(path, queryEndpoint string) (string, error) {
	endpoint, err := server.ResolveEndpoint(path, queryEndpoint)
	if err != nil {
		if requestErr, ok := err.(*server.RequestError); ok {
			return "", &serverRequestError{code: requestErr.Code, message: requestErr.Message}
		}
		return "", err
	}
	return endpoint, nil
}

func isServerEndpoint(endpoint string) bool { return server.IsEndpoint(endpoint) }

func writeServerRequestError(w http.ResponseWriter, err error) bool {
	requestErr, ok := err.(*serverRequestError)
	if !ok {
		return false
	}
	serverWriteError(w, requestErr.code, requestErr.message)
	return true
}

func (a *app) newServerExecutionApp(execution *server.ExecutionRequest) (*app, *agentRuntime) {
	serverCfg := a.cfg
	// A server execution is always remote and must not inherit local lifecycle
	// side effects from the composing application.
	serverCfg.idleHookCommand = ""
	serverCfg.idleHookArgs = nil
	serverCfg.allowedTools = cloneAllowedTools(execution.AllowedTools)
	httpClient := serverHTTPClient
	if a.client != nil && a.client.http != nil {
		httpClient = a.client.http
	}
	serverApp := &app{
		cfg:       serverCfg,
		client:    &client{endpoint: strings.TrimRight(execution.RemoteBase, "/"), token: execution.RemoteToken, model: execution.Model, reasoning: execution.Reasoning, debug: a.cfg.debug, http: httpClient},
		fetchHTTP: httpClient,
		skills:    a.skills,
		toolset:   buildAgentTools(execution.AllowedTools),
		dataStore: a.dataStore,
	}
	subagentCfg := a.cfg.subagents
	subagentCfg.Model = execution.Model
	subagentCfg.ReasoningEffort = execution.Reasoning
	serverApp.subagents = newSubagentManager(subagentCfg, serverApp.runSubagentSession)
	runtime := serverApp.rootRuntime()
	runtime.model = execution.Model
	runtime.reasoning = execution.Reasoning
	return serverApp, runtime
}
