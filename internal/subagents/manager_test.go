package subagents

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestManagerRunsChildThroughNarrowRunnerSeam(t *testing.T) {
	manager := New(Config{MaxDepth: 1, MaxChildren: 2, MaxParallel: 1, DefaultTimeoutSec: 2, MaxTimeoutSec: 2}, func(_ context.Context, runtime *Runtime, session *Session) (string, error) {
		if runtime.Depth != 1 || session.ParentID != "root" {
			t.Fatalf("runtime/session = %#v/%#v", runtime, session)
		}
		return "completed output", nil
	})
	root := &Runtime{SessionID: "root", Role: "coordinator", AllowedTools: map[string]bool{"web_search": true}}
	created, err := manager.Create(context.Background(), root, CreateArgs{Question: "inspect", ExecutionMode: "sequential"})
	if err != nil {
		t.Fatal(err)
	}
	completed, err := manager.Run(context.Background(), root, RunArgs{ID: created.ID, Wait: true})
	if err != nil {
		t.Fatal(err)
	}
	if string(completed.Status) != "completed" || completed.Output != "completed output" {
		t.Fatalf("completed session = %#v", completed)
	}
	visible, err := manager.List(root, ListArgs{})
	if err != nil || len(visible) != 1 {
		t.Fatalf("visible sessions = %#v, err=%v", visible, err)
	}
}

func TestManagerPropagatesParentProfileIntoChildAndNestedLimits(t *testing.T) {
	goalLimits := Config{
		MaxDepth:          2,
		MaxChildren:       8,
		MaxParallel:       8,
		DefaultTimeoutSec: 600,
		MaxTimeoutSec:     1800,
		MaxToolIterations: 100,
		MaxResultChars:    8000,
		MaxAggregateCount: 12,
		MaxAggregateChars: 48000,
		Model:             "goal-model",
		ReasoningEffort:   "high",
	}
	goalProfile := RuntimeProfile{
		MaxIterations:      256,
		MaxGoalIterations:  64,
		Subagents:          goalLimits,
		ToolMaxParallel:    16,
		ToolTimeoutSec:     300,
		ToolRetryOnTimeout: true,
	}

	var observed *Runtime
	manager := New(DefaultConfig(), func(_ context.Context, runtime *Runtime, _ *Session) (string, error) {
		observed = runtime
		return "done", nil
	})
	root := &Runtime{
		SessionID:        "root",
		Role:             "coordinator",
		AllowedTools:     map[string]bool{"read_file": true},
		ExecutionProfile: goalProfile,
	}
	child, err := manager.Create(context.Background(), root, CreateArgs{Question: "inspect"})
	if err != nil {
		t.Fatal(err)
	}
	if child.Depth != 1 || child.Timeout != 600*time.Second {
		t.Fatalf("child lifecycle limits = depth %d, timeout %s", child.Depth, child.Timeout)
	}
	if _, err := manager.Run(context.Background(), root, RunArgs{ID: child.ID, Wait: true}); err != nil {
		t.Fatal(err)
	}
	if observed == nil {
		t.Fatal("runner did not observe a child runtime")
	}
	if observed.MaxToolIterations != 100 || observed.ExecutionProfile.MaxIterations != 100 {
		t.Fatalf("child iteration profile = max=%d execution=%d, want 100", observed.MaxToolIterations, observed.ExecutionProfile.MaxIterations)
	}
	if got := observed.ExecutionProfile.Subagents; got != goalLimits {
		t.Fatalf("child subagent profile = %+v, want %+v", got, goalLimits)
	}
	if got := observed.ExecutionProfile.ToolMaxParallel; got != 16 {
		t.Fatalf("child tool parallelism = %d, want 16", got)
	}
	if got := observed.AllowedTools; len(got) != 1 || !got["read_file"] {
		t.Fatalf("child allowed tools changed: %#v", got)
	}

	nested, err := manager.Create(context.Background(), observed, CreateArgs{Question: "nested"})
	if err != nil {
		t.Fatal(err)
	}
	if nested.Depth != 2 {
		t.Fatalf("nested depth = %d, want 2", nested.Depth)
	}
	if _, err := manager.Create(context.Background(), nestedRuntime(observed, nested), CreateArgs{Question: "too deep"}); err == nil {
		t.Fatal("expected a third level to exceed the goal depth")
	}
}

func nestedRuntime(parent *Runtime, session *Session) *Runtime {
	return &Runtime{
		SessionID:         session.ID,
		Depth:             session.Depth,
		Role:              string(session.Role),
		AllowedTools:      session.AllowedTools,
		MaxToolIterations: parent.MaxToolIterations,
		ExecutionProfile:  parent.ExecutionProfile,
	}
}

func TestManagerUsesProfileParallelismInsteadOfOrdinaryManagerCapacity(t *testing.T) {
	limits := DefaultConfig()
	limits.MaxDepth = 1
	limits.MaxParallel = 8
	goalProfile := RuntimeProfile{Subagents: limits}
	started := make(chan struct{}, 5)
	release := make(chan struct{})
	manager := New(DefaultConfig(), func(ctx context.Context, _ *Runtime, _ *Session) (string, error) {
		started <- struct{}{}
		select {
		case <-release:
			return "done", nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	})
	root := &Runtime{
		SessionID:        "root",
		Role:             "coordinator",
		AllowedTools:     map[string]bool{"read_file": true},
		ExecutionProfile: goalProfile,
	}
	var ids []string
	for range 5 {
		child, err := manager.Create(context.Background(), root, CreateArgs{Question: "parallel"})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, child.ID)
	}
	for _, id := range ids {
		if _, err := manager.Run(context.Background(), root, RunArgs{ID: id, Wait: false}); err != nil {
			t.Fatal(err)
		}
	}
	for range 5 {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("profile parallelism did not start two children")
		}
	}
	close(release)
	for _, id := range ids {
		if _, err := manager.Await(context.Background(), root, AwaitArgs{ID: id}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestManagerUsesProfileAggregateBudget(t *testing.T) {
	limits := DefaultConfig()
	limits.MaxDepth = 1
	limits.MaxChildren = 8
	limits.MaxAggregateChars = 48000
	profile := RuntimeProfile{Subagents: limits}
	manager := New(DefaultConfig(), func(_ context.Context, _ *Runtime, _ *Session) (string, error) {
		return strings.Repeat("x", limits.MaxResultChars), nil
	})
	root := &Runtime{
		SessionID:        "root",
		Role:             "coordinator",
		AllowedTools:     map[string]bool{"read_file": true},
		ExecutionProfile: profile,
	}
	ids := make([]string, 0, limits.MaxChildren)
	for range limits.MaxChildren {
		child, err := manager.Create(context.Background(), root, CreateArgs{Question: "aggregate", ExecutionMode: "sequential"})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, child.ID)
		if _, err := manager.Run(context.Background(), root, RunArgs{ID: child.ID, Wait: true}); err != nil {
			t.Fatal(err)
		}
	}

	value, err := manager.Read(root, ReadArgs{IDs: ids})
	if err != nil {
		t.Fatal(err)
	}
	aggregate, ok := value.(AggregateEnvelope)
	if !ok {
		t.Fatalf("aggregate result type = %T", value)
	}
	if aggregate.Count != limits.MaxChildren || !aggregate.CombinedTrimmed {
		t.Fatalf("aggregate = count %d, trimmed %v; want count %d and a 48000-character cap", aggregate.Count, aggregate.CombinedTrimmed, limits.MaxChildren)
	}
	wantLength := limits.MaxAggregateChars + len("\n\n[... truncated ...]")
	if len(aggregate.CombinedOutput) != wantLength {
		t.Fatalf("aggregate output length = %d, want %d for the selected goal budget", len(aggregate.CombinedOutput), wantLength)
	}
}
