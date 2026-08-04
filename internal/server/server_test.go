package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestNormalizeRequestKeepsSyncAndAsyncFactsIdentical(t *testing.T) {
	body := `{"model":"request-model","reasoning":{"effort":"low"},"messages":[{"role":"system","content":"be concise"},{"role":"user","content":"hello"}]}`
	allowed := map[string]bool{"web_search": true, "fetch_page": true}
	makeRequest := func(path string) *http.Request {
		r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer token")
		return r
	}
	syncRequest, err := NormalizeRequest(makeRequest("/https%3A%2F%2Fremote.example/v1"), IntakeConfig{Model: "default", Reasoning: "medium", AllowedTools: allowed}, "")
	if err != nil {
		t.Fatal(err)
	}
	asyncRequest, err := NormalizeRequest(makeRequest("/async/https%3A%2F%2Fremote.example/v1"), IntakeConfig{Model: "default", Reasoning: "medium", AllowedTools: allowed}, "/async")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(syncRequest, asyncRequest) {
		t.Fatalf("normalized requests differ: %#v vs %#v", syncRequest, asyncRequest)
	}
	if syncRequest.Model != "request-model" || syncRequest.Reasoning != "low" || syncRequest.Question != "hello" {
		t.Fatalf("request overrides were not preserved: %#v", syncRequest)
	}
}

func TestHandlerUsesExecutorAndPreservesResponseShape(t *testing.T) {
	store := NewDataStore()
	handler := NewHandler(HandlerConfig{
		Model: "default-model", Reasoning: "medium", AllowedTools: map[string]bool{"web_search": true}, Store: store,
		Executor: ExecutorFunc(func(ctx context.Context, request *ExecutionRequest) (ExecutionResult, error) {
			if err := ctx.Err(); err != nil {
				return ExecutionResult{}, err
			}
			if request.Question != "hello" {
				t.Fatalf("question = %q", request.Question)
			}
			return ExecutionResult{Content: "answer", Reasoning: "trace"}, nil
		}),
	})
	req := httptest.NewRequest(http.MethodPost, "/?endpoint=https://remote.example/v1", strings.NewReader(`{"messages":[{"role":"user","content":"hello"}]}`))
	req.Header.Set("Authorization", "Bearer token")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}
	var decoded map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["object"] != "chat.completion" {
		t.Fatalf("response object = %v", decoded["object"])
	}
	message := decoded["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if message["content"] != "answer" || message["reasoning"] != "trace" {
		t.Fatalf("response message = %#v", message)
	}
}
