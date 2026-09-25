package agent

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"capelin-go/internal/contracts"
)

func TestConfiguredToolRunnerLimitsParallelismAndPreservesOrder(t *testing.T) {
	calls := []contracts.ToolCall{
		{ID: "one", Function: contracts.FunctionCall{Name: "one"}},
		{ID: "two", Function: contracts.FunctionCall{Name: "two"}},
		{ID: "three", Function: contracts.FunctionCall{Name: "three"}},
	}
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	runner := NewToolRunner(ToolRunnerConfig{MaxParallel: 2, Timeout: time.Second}, func(_ context.Context, call contracts.ToolCall) (string, error) {
		started <- struct{}{}
		<-release
		return call.ID, nil
	}, ToolRunnerHooks{})

	done := make(chan []contracts.ToolResult, 1)
	go func() { done <- runner.Run(context.Background(), calls) }()
	for range 2 {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for parallel tool calls")
		}
	}
	select {
	case <-started:
		t.Fatal("tool runner exceeded configured parallelism")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)

	select {
	case results := <-done:
		if len(results) != len(calls) {
			t.Fatalf("result count=%d, want %d", len(results), len(calls))
		}
		for i, result := range results {
			if result.Call.ID != calls[i].ID || result.Output != calls[i].ID {
				t.Fatalf("result[%d]=%#v, want call/output %q", i, result, calls[i].ID)
			}
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for tool runner")
	}
}

func TestConfiguredToolRunnerRetriesTimeoutOnce(t *testing.T) {
	var attempts atomic.Int32
	call := contracts.ToolCall{ID: "retry", Function: contracts.FunctionCall{Name: "retry"}}
	runner := NewToolRunner(ToolRunnerConfig{
		Timeout:        10 * time.Millisecond,
		RetryOnTimeout: true,
		MaxParallel:    1,
	}, func(ctx context.Context, _ contracts.ToolCall) (string, error) {
		if attempts.Add(1) == 1 {
			<-ctx.Done()
			return "", ctx.Err()
		}
		return "recovered", nil
	}, ToolRunnerHooks{})

	results := runner.Run(context.Background(), []contracts.ToolCall{call})
	if attempts.Load() != 2 {
		t.Fatalf("attempts=%d, want 2", attempts.Load())
	}
	if len(results) != 1 || results[0].Output != "recovered" || !results[0].Retried || results[0].IsError {
		t.Fatalf("results=%#v", results)
	}
}

func TestConfiguredToolRunnerUsesApplicationErrorHook(t *testing.T) {
	call := contracts.ToolCall{ID: "error", Function: contracts.FunctionCall{Name: "error"}}
	wantErr := errors.New("dispatch failed")
	var handled atomic.Int32
	runner := NewToolRunner(ToolRunnerConfig{Timeout: time.Second, MaxParallel: 1}, func(context.Context, contracts.ToolCall) (string, error) {
		return "", wantErr
	}, ToolRunnerHooks{
		HandleError: func(call contracts.ToolCall, err error) contracts.ToolResult {
			handled.Add(1)
			return contracts.ToolResult{Call: call, Output: err.Error(), IsError: true}
		},
	})

	results := runner.Run(context.Background(), []contracts.ToolCall{call})
	if handled.Load() != 1 || len(results) != 1 || results[0].Output != wantErr.Error() || !results[0].IsError {
		t.Fatalf("handled=%d results=%#v", handled.Load(), results)
	}
}

func TestConfiguredToolRunnerSerializesClassifiedCalls(t *testing.T) {
	calls := []contracts.ToolCall{
		{ID: "update", Function: contracts.FunctionCall{Name: "update_todos"}},
		{ID: "work", Function: contracts.FunctionCall{Name: "read_file"}},
		{ID: "work-2", Function: contracts.FunctionCall{Name: "list_files"}},
		{ID: "complete", Function: contracts.FunctionCall{Name: "complete_goal"}},
	}
	var mu sync.Mutex
	var controlOrder []string
	var running atomic.Int32
	var peak atomic.Int32
	runner := NewToolRunner(ToolRunnerConfig{MaxParallel: 3, Timeout: time.Second}, func(_ context.Context, call contracts.ToolCall) (string, error) {
		if call.Function.Name == "read_file" || call.Function.Name == "list_files" {
			current := running.Add(1)
			for {
				previous := peak.Load()
				if current <= previous || peak.CompareAndSwap(previous, current) {
					break
				}
			}
			time.Sleep(20 * time.Millisecond)
			running.Add(-1)
		} else {
			mu.Lock()
			controlOrder = append(controlOrder, call.Function.Name)
			mu.Unlock()
		}
		return call.ID, nil
	}, ToolRunnerHooks{
		Serialize: func(call contracts.ToolCall) bool {
			return call.Function.Name == "update_todos" || call.Function.Name == "complete_goal"
		},
	})

	results := runner.Run(context.Background(), calls)
	if got, want := controlOrder, []string{"update_todos", "complete_goal"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("serialized control order=%v, want %v", got, want)
	}
	if peak.Load() != 2 {
		t.Fatalf("non-control peak=%d, want 2", peak.Load())
	}
	for i, result := range results {
		if result.Call.ID != calls[i].ID || result.Output != calls[i].ID {
			t.Fatalf("result[%d]=%#v, want call/output %q", i, result, calls[i].ID)
		}
	}
}

func TestConfiguredToolRunnerSerializesMatchingKeysOnly(t *testing.T) {
	calls := []contracts.ToolCall{
		{ID: "a1", Function: contracts.FunctionCall{Name: "edit_file", Arguments: `{"path":"a.txt"}`}},
		{ID: "a2", Function: contracts.FunctionCall{Name: "edit_file", Arguments: `{"path":"a.txt"}`}},
		{ID: "b1", Function: contracts.FunctionCall{Name: "edit_file", Arguments: `{"path":"b.txt"}`}},
	}
	started := make(chan string, len(calls))
	release := make(chan struct{})
	runner := NewToolRunner(ToolRunnerConfig{MaxParallel: 3, Timeout: time.Second}, func(_ context.Context, call contracts.ToolCall) (string, error) {
		started <- call.ID
		if call.ID == "a1" || call.ID == "b1" {
			<-release
		}
		return call.ID, nil
	}, ToolRunnerHooks{
		SerializeKey: func(call contracts.ToolCall) string {
			return call.Function.Arguments
		},
	})

	done := make(chan []contracts.ToolResult, 1)
	go func() { done <- runner.Run(context.Background(), calls) }()
	for range 2 {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for independent keyed calls")
		}
	}
	select {
	case got := <-started:
		t.Fatalf("same-key call started before its predecessor completed: %q", got)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)

	select {
	case results := <-done:
		for i, result := range results {
			if result.Call.ID != calls[i].ID || result.Output != calls[i].ID {
				t.Fatalf("result[%d]=%#v, want call/output %q", i, result, calls[i].ID)
			}
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for keyed runner")
	}
}
