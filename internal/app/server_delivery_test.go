package app

import (
	"capelin-go/internal/types"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
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
