package server

import (
	"capelin-go/internal/contracts"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"
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

func TestHandlerAcceptsResponsesRequestAndRendersCompatibilityEnvelope(t *testing.T) {
	store := NewDataStore()
	var seen *ExecutionRequest
	handler := NewHandler(HandlerConfig{
		Model: "default-model", Reasoning: "medium", AllowedTools: map[string]bool{"web_search": true}, Store: store,
		Executor: ExecutorFunc(func(_ context.Context, request *ExecutionRequest) (ExecutionResult, error) {
			copy := *request
			copy.Messages = append([]contracts.Message(nil), request.Messages...)
			seen = &copy
			return ExecutionResult{Content: "answer", Reasoning: "checked the request"}, nil
		}),
	})
	body := `{"model":"request-model","instructions":"Answer in one sentence.","input":"What is Capelin?","reasoning":{"effort":"high"},"stream":false}`
	req := httptest.NewRequest(http.MethodPost, "/?endpoint=https://remote.example/v1/responses", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer token")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}
	if seen == nil || seen.Format != ResponsesFormat || seen.Model != "request-model" || seen.Reasoning != "high" || seen.Question != "What is Capelin?" {
		t.Fatalf("normalized request = %#v", seen)
	}
	if seen.Instructions == nil || *seen.Instructions != "Answer in one sentence." {
		t.Fatalf("instructions = %#v", seen.Instructions)
	}
	if len(seen.Messages) != 1 || seen.Messages[0].Role != "system" || !strings.Contains(seen.Messages[0].Content, "Answer in one sentence.") || !strings.Contains(seen.Messages[0].Content, "Only web_search and fetch_page") {
		t.Fatalf("normalized system guidance = %#v", seen.Messages)
	}

	var decoded map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["object"] != "response" || decoded["status"] != "completed" || decoded["model"] != "request-model" || decoded["instructions"] != "Answer in one sentence." || decoded["output_text"] != "answer" {
		t.Fatalf("response envelope = %#v", decoded)
	}
	if decoded["id"] == "" || decoded["created_at"].(float64) <= 0 {
		t.Fatalf("response identity/timestamp missing = %#v", decoded)
	}
	output := decoded["output"].([]any)
	if len(output) != 2 || output[0].(map[string]any)["type"] != "reasoning" || output[1].(map[string]any)["type"] != "message" {
		t.Fatalf("output ordering = %#v", output)
	}
	message := output[1].(map[string]any)
	content := message["content"].([]any)[0].(map[string]any)
	if content["type"] != "output_text" || content["text"] != "answer" || content["annotations"].([]any) == nil {
		t.Fatalf("message content = %#v", message)
	}
}

func TestHandlerKeepsResponsesFormatAcrossAsyncPolling(t *testing.T) {
	store := NewDataStore()
	handler := NewHandler(HandlerConfig{
		Model: "default-model", Store: store,
		Executor: ExecutorFunc(func(_ context.Context, request *ExecutionRequest) (ExecutionResult, error) {
			if request.Format != ResponsesFormat {
				return ExecutionResult{}, errors.New("request format was not preserved")
			}
			return ExecutionResult{Content: "async answer"}, nil
		}),
	})
	req := httptest.NewRequest(http.MethodPost, "/async/?endpoint=https://remote.example/v1/responses", strings.NewReader(`{"instructions":"Be brief","input":"hello"}`))
	req.Header.Set("Authorization", "Bearer token")
	accepted := httptest.NewRecorder()
	handler.ServeHTTP(accepted, req)
	if accepted.Code != http.StatusAccepted {
		t.Fatalf("accepted status = %d: %s", accepted.Code, accepted.Body.String())
	}
	var id map[string]string
	if err := json.Unmarshal(accepted.Body.Bytes(), &id); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		poll := httptest.NewRecorder()
		handler.ServeHTTP(poll, httptest.NewRequest(http.MethodGet, "/data?key="+url.QueryEscape(id["id"]), nil))
		if poll.Code == http.StatusOK {
			var decoded map[string]any
			if err := json.Unmarshal(poll.Body.Bytes(), &decoded); err != nil {
				t.Fatal(err)
			}
			if decoded["object"] != "response" || decoded["output_text"] != "async answer" || decoded["instructions"] != "Be brief" {
				t.Fatalf("async response = %#v", decoded)
			}
			return
		}
		if poll.Code != http.StatusNotFound {
			t.Fatalf("poll status = %d: %s", poll.Code, poll.Body.String())
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("timed out polling async response")
}

func TestHandlerRejectsUnsupportedResponsesCombinations(t *testing.T) {
	handler := NewHandler(HandlerConfig{Executor: ExecutorFunc(func(context.Context, *ExecutionRequest) (ExecutionResult, error) {
		return ExecutionResult{Content: "must not run"}, nil
	})})
	for _, test := range []struct {
		name string
		body string
		want string
	}{
		{name: "mixed messages and input", body: `{"messages":[{"role":"user","content":"chat"}],"input":"responses"}`, want: "messages cannot be combined"},
		{name: "missing input", body: `{"instructions":"only instructions"}`, want: "input is required"},
		{name: "empty input", body: `{"input":"   "}`, want: "input is required"},
		{name: "non-string input", body: `{"input":[{"role":"user","content":"not supported"}]}`, want: "input is required"},
		{name: "stateful responses", body: `{"input":"hello","previous_response_id":"resp_123"}`, want: "previous_response_id is not supported"},
		{name: "streaming", body: `{"input":"hello","stream":true}`, want: "streaming is not supported"},
	} {
		t.Run(test.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/?endpoint=https://remote.example/v1", strings.NewReader(test.body))
			req.Header.Set("Authorization", "Bearer token")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, req)
			if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), test.want) {
				t.Fatalf("response = %d: %s", response.Code, response.Body.String())
			}
		})
	}
}
