package app

import (
	"bytes"
	"capelin-go/internal/server"
	"capelin-go/internal/types"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestServerDeliveryUsesApplicationExecutorForBothModes(t *testing.T) {
	a := &app{
		cfg:       config{model: "default-model", reasoning: "default-reasoning"},
		dataStore: newDataStore(),
	}
	var mu sync.Mutex
	var requests []*serverExecutionRequest
	executor := serverExecutorFunc(func(_ context.Context, request *serverExecutionRequest) (serverExecutionResult, error) {
		mu.Lock()
		copyRequest := *request
		copyRequest.messages = append([]types.Message(nil), request.messages...)
		requests = append(requests, &copyRequest)
		mu.Unlock()
		return serverExecutionResult{content: "from executor"}, nil
	})
	handler := newServerHandlerWithExecutor(a, map[string]bool{toolWebSearch: true}, executor)
	body := `{"model":"","reasoning":{"effort":""},"messages":[{"role":"user","content":"hello"}]}`
	endpoint := url.QueryEscape("https://remote.example/v1/chat/completions")

	syncRequest := httptest.NewRequest(http.MethodPost, "/?endpoint="+endpoint, strings.NewReader(body))
	syncRequest.Header.Set("Authorization", "Bearer token")
	syncResponse := httptest.NewRecorder()
	handler.ServeHTTP(syncResponse, syncRequest)
	if syncResponse.Code != http.StatusOK || !strings.Contains(syncResponse.Body.String(), "from executor") {
		t.Fatalf("sync response = %d: %s", syncResponse.Code, syncResponse.Body.String())
	}

	asyncRequest := httptest.NewRequest(http.MethodPost, "/async/?endpoint="+endpoint, strings.NewReader(body))
	asyncRequest.Header.Set("Authorization", "Bearer token")
	asyncResponse := httptest.NewRecorder()
	handler.ServeHTTP(asyncResponse, asyncRequest)
	if asyncResponse.Code != http.StatusAccepted {
		t.Fatalf("async response = %d: %s", asyncResponse.Code, asyncResponse.Body.String())
	}
	var accepted map[string]string
	if err := json.Unmarshal(asyncResponse.Body.Bytes(), &accepted); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(time.Second)
	found := false
	for time.Now().Before(deadline) {
		poll := httptest.NewRecorder()
		handler.ServeHTTP(poll, httptest.NewRequest(http.MethodGet, "/data?key="+url.QueryEscape(accepted["id"]), nil))
		if poll.Code == http.StatusOK {
			if !strings.Contains(poll.Body.String(), "from executor") {
				t.Fatalf("unexpected async result: %s", poll.Body.String())
			}
			found = true
			break
		}
		if poll.Code != http.StatusNotFound {
			t.Fatalf("poll status = %d: %s", poll.Code, poll.Body.String())
		}
		time.Sleep(time.Millisecond)
	}
	if !found {
		t.Fatal("timed out polling executor result")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(requests) != 2 {
		t.Fatalf("executor calls = %d, want 2", len(requests))
	}
	for _, request := range requests {
		if request.remoteBase != "https://remote.example/v1/chat/completions" || request.model != "default-model" || request.reasoning != "default-reasoning" {
			t.Fatalf("request normalization = %#v", request)
		}
		if len(request.messages) != 1 || request.messages[0].Role != "system" || request.question != "hello" {
			t.Fatalf("normalized messages/question = %#v/%q", request.messages, request.question)
		}
	}
}

func TestServerDeliveryRecoversSynchronousExecutorPanic(t *testing.T) {
	a := &app{cfg: config{}, dataStore: newDataStore()}
	handler := newServerHandlerWithExecutor(a, nil, serverExecutorFunc(func(context.Context, *serverExecutionRequest) (serverExecutionResult, error) {
		panic("delivery panic")
	}))
	req := httptest.NewRequest(http.MethodPost, "/?endpoint=https%3A%2F%2Fremote.example", strings.NewReader(`{"messages":[{"role":"user","content":"hello"}]}`))
	req.Header.Set("Authorization", "Bearer token")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	if response.Code != http.StatusInternalServerError || !strings.Contains(response.Body.String(), "delivery panic") {
		t.Fatalf("panic response = %d: %s", response.Code, response.Body.String())
	}
}

func TestServerDeliveryNeverTriggersConfiguredLocalIdleHook(t *testing.T) {
	var hookCalls atomic.Int32
	hook := newIdleHookRunner("local-hook", []string{"must-not-run"}, t.TempDir(), true,
		func(context.Context, string, string, bool, []string) (string, error) {
			hookCalls.Add(1)
			return `{"exit_code":0}`, nil
		}, &bytes.Buffer{})
	if hook == nil {
		t.Fatal("configured local hook did not create a runner")
	}
	defer hook.drainAndClose()

	a := &app{
		cfg:       config{model: "server-model", reasoning: "server-reasoning", yolo: true, idleHookCommand: "local-hook", idleHookArgs: []string{"must-not-run"}},
		dataStore: newDataStore(),
		idleHooks: hook,
	}
	serverApp, _ := a.newServerExecutionApp(&serverExecutionRequest{
		remoteBase: "https://remote.example/v1/chat/completions", model: "server-model",
		serverAllowedTools: map[string]bool{toolWebSearch: true},
	})
	if serverApp.idleHooks != nil || serverApp.cfg.idleHookCommand != "" || len(serverApp.cfg.idleHookArgs) != 0 {
		t.Fatal("server execution inherited a local idle hook")
	}

	var executionCalls atomic.Int32
	var exposedProgram atomic.Bool
	executor := serverExecutorFunc(func(_ context.Context, request *serverExecutionRequest) (serverExecutionResult, error) {
		if request.serverAllowedTools[toolExecuteProgram] {
			exposedProgram.Store(true)
		}
		switch request.question {
		case "hello":
			if executionCalls.Add(1) == 1 {
				return serverExecutionResult{content: "sync answer"}, nil
			}
			return serverExecutionResult{}, errors.New("sync failure")
		case "async-success":
			return serverExecutionResult{content: "async answer"}, nil
		case "async-timeout":
			return serverExecutionResult{}, context.DeadlineExceeded
		default:
			return serverExecutionResult{}, context.Canceled
		}
	})
	handler := newServerHandlerWithExecutor(a, map[string]bool{toolWebSearch: true}, executor)
	endpoint := url.QueryEscape("https://remote.example/v1/chat/completions")
	post := func(path, question string) *httptest.ResponseRecorder {
		body := `{"messages":[{"role":"user","content":"` + question + `"}]}`
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer token")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		return response
	}

	syncSuccess := post("/?endpoint="+endpoint, "hello")
	if syncSuccess.Code != http.StatusOK || !strings.Contains(syncSuccess.Body.String(), "sync answer") {
		t.Fatalf("sync success response = %d: %s", syncSuccess.Code, syncSuccess.Body.String())
	}
	syncFailure := post("/?endpoint="+endpoint, "hello")
	if syncFailure.Code != http.StatusInternalServerError || !strings.Contains(syncFailure.Body.String(), "sync failure") {
		t.Fatalf("sync failure response = %d: %s", syncFailure.Code, syncFailure.Body.String())
	}

	asyncRequests := make([]struct {
		question string
		id       string
	}, 0, 3)
	for _, question := range []string{"async-success", "async-timeout", "async-cancel"} {
		response := post("/async/?endpoint="+endpoint, question)
		if response.Code != http.StatusAccepted {
			t.Fatalf("async response = %d: %s", response.Code, response.Body.String())
		}
		var accepted map[string]string
		if err := json.Unmarshal(response.Body.Bytes(), &accepted); err != nil {
			t.Fatal(err)
		}
		asyncRequests = append(asyncRequests, struct {
			question string
			id       string
		}{question: question, id: accepted["id"]})
	}
	for _, asyncRequest := range asyncRequests {
		deadline := time.Now().Add(2 * time.Second)
		for {
			poll := httptest.NewRecorder()
			handler.ServeHTTP(poll, httptest.NewRequest(http.MethodGet, "/data?key="+url.QueryEscape(asyncRequest.id), nil))
			if poll.Code == http.StatusOK {
				if asyncRequest.question == "async-success" && !strings.Contains(poll.Body.String(), "async answer") {
					t.Fatalf("unexpected async success result: %s", poll.Body.String())
				}
				if asyncRequest.question != "async-success" && !strings.Contains(poll.Body.String(), "async_error") {
					t.Fatalf("unexpected async terminal result: %s", poll.Body.String())
				}
				break
			}
			if poll.Code != http.StatusNotFound {
				t.Fatalf("async poll status = %d: %s", poll.Code, poll.Body.String())
			}
			if time.Now().After(deadline) {
				t.Fatalf("timed out polling async result %q", asyncRequest.id)
			}
			time.Sleep(time.Millisecond)
		}
	}
	if got := hookCalls.Load(); got != 0 {
		t.Fatalf("server requests triggered local idle hook %d time(s)", got)
	}
	if exposedProgram.Load() {
		t.Fatal("server request exposed execute_program")
	}
}

func TestServerAsyncAdmissionContractRemainsUnchangedWithIdleHook(t *testing.T) {
	var hookCalls atomic.Int32
	hook := newIdleHookRunner("local-hook", nil, t.TempDir(), true,
		func(context.Context, string, string, bool, []string) (string, error) {
			hookCalls.Add(1)
			return `{"exit_code":0}`, nil
		}, &bytes.Buffer{})
	defer hook.drainAndClose()

	a := &app{cfg: config{model: "server-model", idleHookCommand: "local-hook", yolo: true}, dataStore: newDataStore(), idleHooks: hook}
	var executorCalls atomic.Int32
	handler := newServerHandlerWithExecutor(a, map[string]bool{toolWebSearch: true}, serverExecutorFunc(func(context.Context, *serverExecutionRequest) (serverExecutionResult, error) {
		executorCalls.Add(1)
		return serverExecutionResult{content: "must not run"}, nil
	}))
	oldSemaphore := asyncSem
	limitedSemaphore := make(chan struct{}, 1)
	asyncSem = limitedSemaphore
	server.SetAsyncSemaphore(limitedSemaphore)
	defer func() {
		asyncSem = oldSemaphore
		server.SetAsyncSemaphore(oldSemaphore)
	}()
	limitedSemaphore <- struct{}{}

	req := httptest.NewRequest(http.MethodPost, "/async/?endpoint=https%3A%2F%2Fremote.example%2Fv1", strings.NewReader(`{"messages":[{"role":"user","content":"hello"}]}`))
	req.Header.Set("Authorization", "Bearer token")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	if response.Code != http.StatusTooManyRequests || response.Header().Get("Retry-After") != "1" || !strings.Contains(response.Body.String(), "async capacity exhausted") {
		t.Fatalf("admission response = %d headers=%v body=%s", response.Code, response.Header(), response.Body.String())
	}
	if executorCalls.Load() != 0 || hookCalls.Load() != 0 {
		t.Fatalf("rejected async request executed work: executor=%d hook=%d", executorCalls.Load(), hookCalls.Load())
	}
	<-limitedSemaphore
}
