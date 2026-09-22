package subagents

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestManagerRunsChildThroughNarrowRunnerSeam(t *testing.T) {
	manager := New(Config{MaxDepth: 1, MaxChildren: 2, MaxParallel: 1, DefaultTimeoutSec: 2, MaxTimeoutSec: 2}, func(_ context.Context, runtime *Runtime, session *Session) (string, bool, error) {
		if runtime.Depth != 1 || session.ParentID != "root" {
			t.Fatalf("runtime/session = %#v/%#v", runtime, session)
		}
		return "completed output", false, nil
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
	manager := New(DefaultConfig(), func(_ context.Context, runtime *Runtime, _ *Session) (string, bool, error) {
		observed = runtime
		return "done", false, nil
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
	manager := New(DefaultConfig(), func(ctx context.Context, _ *Runtime, _ *Session) (string, bool, error) {
		started <- struct{}{}
		select {
		case <-release:
			return "done", false, nil
		case <-ctx.Done():
			return "", false, ctx.Err()
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
	manager := New(DefaultConfig(), func(_ context.Context, _ *Runtime, _ *Session) (string, bool, error) {
		return strings.Repeat("x", limits.MaxResultChars), false, nil
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

// TestManagerScopeIsolatesVisibilityAndControl pins the scope-filtering rules
// that the application seam cannot observe directly: a root in one agent scope
// can neither see nor control a worker created in another scope, while its own
// scope keeps the established root-sees-all behavior.
func TestManagerScopeIsolatesVisibilityAndControl(t *testing.T) {
	manager := New(Config{MaxDepth: 1, MaxChildren: 4, MaxParallel: 2, DefaultTimeoutSec: 5, MaxTimeoutSec: 5}, func(context.Context, *Runtime, *Session) (string, bool, error) {
		return "done", false, nil
	})
	first := &Runtime{SessionID: "root", ScopeID: "scope-a", Role: "coordinator", AllowedTools: map[string]bool{"read_file": true}}
	second := &Runtime{SessionID: "root", ScopeID: "scope-b", Role: "coordinator", AllowedTools: map[string]bool{"read_file": true}}

	alpha, err := manager.Create(context.Background(), first, CreateArgs{Question: "alpha"})
	if err != nil {
		t.Fatal(err)
	}
	if alpha.ScopeID != "scope-a" {
		t.Fatalf("worker scope = %q, want scope-a", alpha.ScopeID)
	}
	beta, err := manager.Create(context.Background(), second, CreateArgs{Question: "beta"})
	if err != nil {
		t.Fatal(err)
	}

	// Each scope lists only its own worker.
	for _, tc := range []struct {
		runtime *Runtime
		want    string
	}{{first, alpha.ID}, {second, beta.ID}} {
		items, err := manager.List(tc.runtime, ListArgs{IncludeDescendants: true})
		if err != nil {
			t.Fatal(err)
		}
		if len(items) != 1 || items[0].ID != tc.want {
			t.Fatalf("scope %q listed %#v, want only %s", tc.runtime.ScopeID, items, tc.want)
		}
	}

	// Cross-scope read, run, await, and cancel are all rejected.
	for name, call := range map[string]func() error{
		"read": func() error { _, err := manager.Read(second, ReadArgs{ID: alpha.ID}); return err },
		"run":  func() error { _, err := manager.Run(context.Background(), second, RunArgs{ID: alpha.ID}); return err },
		"await": func() error {
			_, err := manager.Await(context.Background(), second, AwaitArgs{ID: alpha.ID})
			return err
		},
		"cancel": func() error { _, err := manager.Cancel(second, CancelArgs{ID: alpha.ID}); return err },
	} {
		err := call()
		if err == nil {
			t.Fatalf("cross-scope %s unexpectedly succeeded", name)
		}
		if !strings.Contains(err.Error(), "not visible") {
			t.Fatalf("cross-scope %s error = %v, want a not-visible control error", name, err)
		}
	}

	// The foreign worker was left untouched and its own scope still controls it.
	snapshot, err := manager.Read(first, ReadArgs{ID: alpha.ID})
	if err != nil {
		t.Fatal(err)
	}
	if envelope, ok := snapshot.(Envelope); !ok || envelope.Status != string(subagentStatusPending) {
		t.Fatalf("foreign-scope operations changed the worker: %#v", snapshot)
	}

	// ListScope and ListAll agree with the scope boundary.
	if nodes := manager.ListScope("scope-a"); len(nodes) != 1 || nodes[0].ID != alpha.ID {
		t.Fatalf("ListScope(scope-a) = %#v, want only %s", nodes, alpha.ID)
	}
	if nodes := manager.ListAll(); len(nodes) != 2 {
		t.Fatalf("ListAll = %#v, want both workers", nodes)
	}
}

// TestManagerCloseScopeCancelsOnlyItsOwnWork verifies closing a scope cancels
// its outstanding workers, leaves other scopes untouched, and reports zero for
// an empty or unknown scope so a switch cannot raise a spurious error.
func TestManagerCloseScopeCancelsOnlyItsOwnWork(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	started := make(chan string, 4)
	manager := New(Config{MaxDepth: 1, MaxChildren: 4, MaxParallel: 4, DefaultTimeoutSec: 30, MaxTimeoutSec: 30}, func(ctx context.Context, _ *Runtime, session *Session) (string, bool, error) {
		started <- session.ID
		select {
		case <-release:
			return "done", false, nil
		case <-ctx.Done():
			return "", false, ctx.Err()
		}
	})
	doomed := &Runtime{SessionID: "root", ScopeID: "scope-old", Role: "coordinator", AllowedTools: map[string]bool{"read_file": true}}
	survivor := &Runtime{SessionID: "root", ScopeID: "scope-new", Role: "coordinator", AllowedTools: map[string]bool{"read_file": true}}

	pending, err := manager.Create(context.Background(), doomed, CreateArgs{Question: "pending"})
	if err != nil {
		t.Fatal(err)
	}
	running, err := manager.Create(context.Background(), doomed, CreateArgs{Question: "running"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Run(context.Background(), doomed, RunArgs{ID: running.ID}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("running worker never started")
	}
	keep, err := manager.Create(context.Background(), survivor, CreateArgs{Question: "keep"})
	if err != nil {
		t.Fatal(err)
	}

	if cancelled := manager.CloseScope("scope-old"); cancelled != 2 {
		t.Fatalf("CloseScope cancelled %d workers, want 2", cancelled)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		pendingSnap, err := manager.Read(doomed, ReadArgs{ID: pending.ID})
		if err != nil {
			t.Fatal(err)
		}
		runningSnap, err := manager.Read(doomed, ReadArgs{ID: running.ID})
		if err != nil {
			t.Fatal(err)
		}
		if pendingSnap.(Envelope).Status == string(subagentStatusCancelled) &&
			runningSnap.(Envelope).Status == string(subagentStatusCancelled) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("closed scope did not cancel its work: pending=%s running=%s",
				pendingSnap.(Envelope).Status, runningSnap.(Envelope).Status)
		}
		time.Sleep(10 * time.Millisecond)
	}

	// The other live scope is untouched.
	keepSnap, err := manager.Read(survivor, ReadArgs{ID: keep.ID})
	if err != nil {
		t.Fatal(err)
	}
	if status := keepSnap.(Envelope).Status; status != string(subagentStatusPending) {
		t.Fatalf("closing one scope changed another scope's worker to %q", status)
	}

	// Closing an empty, unknown, or already-closed scope is a quiet no-op.
	if got := manager.CloseScope(""); got != 0 {
		t.Fatalf("CloseScope(\"\") = %d, want 0", got)
	}
	if got := manager.CloseScope("scope-unknown"); got != 0 {
		t.Fatalf("CloseScope(unknown) = %d, want 0", got)
	}
	if got := manager.CloseScope("scope-old"); got != 0 {
		t.Fatalf("re-closing a drained scope = %d, want 0", got)
	}
}

// TestManagerUnscopedRuntimesRetainLegacyBehavior pins compatibility for
// one-shot and server executions, which carry no agent scope.
func TestManagerUnscopedRuntimesRetainLegacyBehavior(t *testing.T) {
	manager := New(Config{MaxDepth: 1, MaxChildren: 2, MaxParallel: 1, DefaultTimeoutSec: 5, MaxTimeoutSec: 5}, func(context.Context, *Runtime, *Session) (string, bool, error) {
		return "done", false, nil
	})
	root := &Runtime{SessionID: "root", Role: "coordinator", AllowedTools: map[string]bool{"read_file": true}}
	child, err := manager.Create(context.Background(), root, CreateArgs{Question: "inspect", ExecutionMode: "sequential"})
	if err != nil {
		t.Fatal(err)
	}
	if child.ScopeID != "" {
		t.Fatalf("unscoped worker carried scope %q", child.ScopeID)
	}
	if _, err := manager.Run(context.Background(), root, RunArgs{ID: child.ID, Wait: true}); err != nil {
		t.Fatal(err)
	}
	items, err := manager.List(root, ListArgs{})
	if err != nil || len(items) != 1 {
		t.Fatalf("unscoped list = %#v (err=%v)", items, err)
	}
	if nodes := manager.ListScope(""); len(nodes) != 1 || nodes[0].ID != child.ID {
		t.Fatalf("ListScope(\"\") = %#v, want the legacy unscoped child", nodes)
	}
}
