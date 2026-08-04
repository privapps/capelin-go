package agent

import (
	"context"
	"errors"
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
