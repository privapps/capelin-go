package app

import (
	"capelin-go/internal/contracts"
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

// Tests in this file exercise the feature's primary application-level seam:
// commands are submitted through the interactive dispatcher with a real
// interactive session, a real subagent manager, and a captured output sink.
// Direct manager assertions are limited to scope-filtering rules that cannot
// be observed through the application seam.

// newAgentScopeTestApp builds an interactive test app whose root runtime may
// create and control subagents, wired to a runner supplied by the caller.
func newAgentScopeTestApp(t *testing.T, runner subagentRunner) *interactiveTurnTestApp {
	t.Helper()
	testApp := newInteractiveTurnTestApp(t)
	testApp.app.cfg.allowedTools = map[string]bool{
		toolCreateSubagent: true,
		toolRunSubagent:    true,
		toolAwaitSubagent:  true,
		toolReadSubagent:   true,
		toolListSubagents:  true,
		toolCancelSubagent: true,
	}
	testApp.app.toolset = buildAgentTools(testApp.app.cfg.allowedTools)
	cfg := defaultSubagentRuntimeConfig()
	cfg.MaxDepth = 3
	testApp.app.subagents = newSubagentManager(cfg, runner)
	return testApp
}

func completingSubagentRunner(context.Context, *agentRuntime, *subagentSession) (string, bool, error) {
	return "child result", false, nil
}

// agentsView submits ::agents through the interactive dispatcher and returns
// the single rendered status line.
func agentsView(t *testing.T, testApp *interactiveTurnTestApp, session *interactiveSession) string {
	t.Helper()
	var systems []string
	previous := testApp.app.sink.(*spySink).onSystem
	testApp.app.sink.(*spySink).onSystem = func(message string) { systems = append(systems, message) }
	defer func() { testApp.app.sink.(*spySink).onSystem = previous }()
	if stopped := testApp.app.handleInteractiveInput(context.Background(), session, "::agents"); stopped {
		t.Fatal("::agents unexpectedly stopped the session")
	}
	if len(systems) != 1 {
		t.Fatalf("::agents emitted %d system messages, want 1: %#v", len(systems), systems)
	}
	return systems[0]
}

// scopeTestApp builds an interactive test app whose subagent runner blocks
// until released, so pending, queued, and running workers can all be observed.
type scopeTestApp struct {
	*interactiveTurnTestApp
	started     chan string
	release     chan struct{}
	releaseOnce sync.Once
	mu          sync.Mutex
	ran         []string
}

func newScopeTestApp(t *testing.T) *scopeTestApp {
	t.Helper()
	scoped := &scopeTestApp{
		started: make(chan string, 16),
		release: make(chan struct{}),
	}
	scoped.interactiveTurnTestApp = newAgentScopeTestApp(t, func(ctx context.Context, _ *agentRuntime, session *subagentSession) (string, bool, error) {
		scoped.mu.Lock()
		scoped.ran = append(scoped.ran, session.ID)
		scoped.mu.Unlock()
		scoped.started <- session.ID
		select {
		case <-scoped.release:
			return "child result", false, nil
		case <-ctx.Done():
			return "", false, ctx.Err()
		}
	})
	t.Cleanup(scoped.releaseWorkers)
	return scoped
}

func (s *scopeTestApp) releaseWorkers() {
	s.releaseOnce.Do(func() { close(s.release) })
}

// TestInteractiveAgentScopeResetsOnSessionNew is the primary regression test
// for the reported defect: a worker created in the first conversation must not
// appear in ::agents after /session-new, and a worker created afterwards must
// appear normally.
func TestInteractiveAgentScopeResetsOnSessionNew(t *testing.T) {
	testApp := newAgentScopeTestApp(t, completingSubagentRunner)
	session, err := testApp.app.newInteractiveSession([]contracts.Message{{Role: "system", Content: "test"}})
	if err != nil {
		t.Fatal(err)
	}

	oldRoot := testApp.app.rootRuntime()
	completed, err := testApp.app.subagents.create(context.Background(), oldRoot, createSubagentArgs{Name: "worker-old", Question: "old completed work"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := testApp.app.subagents.run(context.Background(), oldRoot, runSubagentArgs{ID: completed.ID, Wait: true}); err != nil {
		t.Fatal(err)
	}
	pending, err := testApp.app.subagents.create(context.Background(), oldRoot, createSubagentArgs{Question: "old pending work"})
	if err != nil {
		t.Fatal(err)
	}

	before := agentsView(t, testApp, session)
	if !strings.Contains(before, completed.ID) || !strings.Contains(before, pending.ID) {
		t.Fatalf("first-scope workers were not rendered before the switch: %q", before)
	}

	if stopped := testApp.app.handleInteractiveInput(context.Background(), session, "/session-new"); stopped {
		t.Fatal("/session-new unexpectedly stopped the session")
	}

	after := agentsView(t, testApp, session)
	wantEmpty := "[::agents]\n- root (coordinator, idle)\naggregate: total=0; active=0; pending=0 queued=0 running=0; completed=0 failed=0 cancelled=0 timed_out=0"
	if after != wantEmpty {
		t.Fatalf("::agents after /session-new = %q, want the empty root view %q", after, wantEmpty)
	}
	if strings.Contains(after, completed.ID) || strings.Contains(after, pending.ID) {
		t.Fatalf("::agents leaked previous-scope workers after /session-new: %q", after)
	}

	// The new conversation retains full agent orchestration functionality.
	newRoot := testApp.app.rootRuntime()
	fresh, err := testApp.app.subagents.create(context.Background(), newRoot, createSubagentArgs{Name: "worker-new", Question: "new work"})
	if err != nil {
		t.Fatal(err)
	}
	rendered := agentsView(t, testApp, session)
	if !strings.Contains(rendered, fresh.ID+" (worker, pending, name: worker-new)") {
		t.Fatalf("new-scope worker was not rendered: %q", rendered)
	}
	if strings.Contains(rendered, completed.ID) || strings.Contains(rendered, pending.ID) {
		t.Fatalf("new scope rendered a worker from the retired scope: %q", rendered)
	}
	if !strings.Contains(rendered, "aggregate: total=1; active=1; pending=1 queued=0 running=0; completed=0 failed=0 cancelled=0 timed_out=0") {
		t.Fatalf("new-scope aggregate counted foreign workers: %q", rendered)
	}
	if !strings.Contains(rendered, "root (coordinator, idle) descendants: 1") {
		t.Fatalf("new-scope root descendant count included foreign workers: %q", rendered)
	}
}

// TestInteractiveAgentScopeResetsOnSessionResume proves a resumed conversation
// starts from an empty live agent scope: agent trees are runtime state and are
// never restored from the durable conversation snapshot.
func TestInteractiveAgentScopeResetsOnSessionResume(t *testing.T) {
	testApp := newAgentScopeTestApp(t, completingSubagentRunner)
	first, err := testApp.app.newInteractiveSession([]contracts.Message{{Role: "system", Content: "test"}, {Role: "user", Content: "first"}})
	if err != nil {
		t.Fatal(err)
	}
	firstID := first.id

	worker, err := testApp.app.subagents.create(context.Background(), testApp.app.rootRuntime(), createSubagentArgs{Question: "work in the first scope"})
	if err != nil {
		t.Fatal(err)
	}
	if view := agentsView(t, testApp, first); !strings.Contains(view, worker.ID) {
		t.Fatalf("first-scope worker missing before the switch: %q", view)
	}

	// Switch away and back again: the durable conversation returns, the
	// ephemeral agent tree does not.
	if stopped := testApp.app.handleInteractiveInput(context.Background(), first, "/session-new"); stopped {
		t.Fatal("/session-new unexpectedly stopped the session")
	}
	if stopped := testApp.app.handleInteractiveInput(context.Background(), first, "/session-resume "+firstID); stopped {
		t.Fatal("/session-resume unexpectedly stopped the session")
	}
	if first.id != firstID {
		t.Fatalf("resume restored session %q, want %q", first.id, firstID)
	}
	if len(first.messages) != 2 {
		t.Fatalf("resume did not restore the durable conversation: %#v", first.messages)
	}

	view := agentsView(t, testApp, first)
	if strings.Contains(view, worker.ID) {
		t.Fatalf("resume restored an ephemeral worker from the snapshot: %q", view)
	}
	if !strings.Contains(view, "aggregate: total=0;") {
		t.Fatalf("resumed conversation did not start from an empty scope: %q", view)
	}
}

// TestInteractiveAgentScopeIsolatesNestedHierarchyAndCounts checks that a new
// scope's nested hierarchy, descendant counts, and aggregates never include
// workers from another scope.
func TestInteractiveAgentScopeIsolatesNestedHierarchyAndCounts(t *testing.T) {
	testApp := newAgentScopeTestApp(t, completingSubagentRunner)
	session, err := testApp.app.newInteractiveSession([]contracts.Message{{Role: "system", Content: "test"}})
	if err != nil {
		t.Fatal(err)
	}

	oldRoot := testApp.app.rootRuntime()
	oldParent, err := testApp.app.subagents.create(context.Background(), oldRoot, createSubagentArgs{Question: "old parent"})
	if err != nil {
		t.Fatal(err)
	}
	oldChildRuntime := &agentRuntime{
		sessionID:         oldParent.ID,
		scopeID:           oldRoot.scopeID,
		depth:             oldParent.Depth,
		allowedTools:      map[string]bool{toolCreateSubagent: true},
		maxToolIterations: 5,
	}
	oldChild, err := testApp.app.subagents.create(context.Background(), oldChildRuntime, createSubagentArgs{Question: "old nested"})
	if err != nil {
		t.Fatal(err)
	}

	if stopped := testApp.app.handleInteractiveInput(context.Background(), session, "/session-new"); stopped {
		t.Fatal("/session-new unexpectedly stopped the session")
	}

	newRoot := testApp.app.rootRuntime()
	newParent, err := testApp.app.subagents.create(context.Background(), newRoot, createSubagentArgs{Question: "new parent"})
	if err != nil {
		t.Fatal(err)
	}
	newChildRuntime := &agentRuntime{
		sessionID:         newParent.ID,
		scopeID:           newRoot.scopeID,
		depth:             newParent.Depth,
		allowedTools:      map[string]bool{toolCreateSubagent: true},
		maxToolIterations: 5,
	}
	newChild, err := testApp.app.subagents.create(context.Background(), newChildRuntime, createSubagentArgs{Question: "new nested"})
	if err != nil {
		t.Fatal(err)
	}

	view := agentsView(t, testApp, session)
	for _, absent := range []string{oldParent.ID, oldChild.ID} {
		if strings.Contains(view, absent) {
			t.Fatalf("::agents rendered foreign-scope worker %s: %q", absent, view)
		}
	}
	if !strings.Contains(view, newParent.ID+" (worker, pending) question: new parent; descendants: 1") {
		t.Fatalf("new-scope parent descendant count is wrong: %q", view)
	}
	if !strings.Contains(view, newChild.ID+" (worker, pending) question: new nested") {
		t.Fatalf("new-scope nested worker missing: %q", view)
	}
	if !strings.Contains(view, "root (coordinator, idle) descendants: 2") {
		t.Fatalf("new-scope root descendant count included foreign workers: %q", view)
	}
	if !strings.Contains(view, "aggregate: total=2; active=2; pending=2 queued=0 running=0; completed=0 failed=0 cancelled=0 timed_out=0") {
		t.Fatalf("new-scope aggregate counted foreign workers: %q", view)
	}
}

// TestInteractiveAgentScopeRejectsForeignWorkerControl proves scope isolation
// also covers the model-facing control plane: after a switch, the active root
// can neither observe nor control a worker belonging to the retired scope, and
// the foreign worker is left unchanged.
func TestInteractiveAgentScopeRejectsForeignWorkerControl(t *testing.T) {
	testApp := newAgentScopeTestApp(t, completingSubagentRunner)
	session, err := testApp.app.newInteractiveSession([]contracts.Message{{Role: "system", Content: "test"}})
	if err != nil {
		t.Fatal(err)
	}
	oldRoot := testApp.app.rootRuntime()
	foreign, err := testApp.app.subagents.create(context.Background(), oldRoot, createSubagentArgs{Question: "foreign work"})
	if err != nil {
		t.Fatal(err)
	}

	if stopped := testApp.app.handleInteractiveInput(context.Background(), session, "/session-new"); stopped {
		t.Fatal("/session-new unexpectedly stopped the session")
	}
	newRoot := testApp.app.rootRuntime()

	if items, err := testApp.app.subagents.list(newRoot, listSubagentsArgs{IncludeDescendants: true}); err != nil || len(items) != 0 {
		t.Fatalf("list from the new scope returned %#v (err=%v), want no foreign workers", items, err)
	}
	if _, err := testApp.app.subagents.read(newRoot, readSubagentArgs{ID: foreign.ID}); err == nil {
		t.Fatal("read of a foreign-scope worker unexpectedly succeeded")
	} else if !strings.Contains(err.Error(), "not visible") {
		t.Fatalf("read error = %v, want a not-visible control error", err)
	}
	if _, err := testApp.app.subagents.run(context.Background(), newRoot, runSubagentArgs{ID: foreign.ID}); err == nil {
		t.Fatal("run of a foreign-scope worker unexpectedly succeeded")
	} else if !strings.Contains(err.Error(), "not visible") {
		t.Fatalf("run error = %v, want a not-visible control error", err)
	}
	awaitCtx, cancelAwait := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelAwait()
	if _, err := testApp.app.subagents.await(awaitCtx, newRoot, awaitSubagentArgs{ID: foreign.ID}); err == nil {
		t.Fatal("await of a foreign-scope worker unexpectedly succeeded")
	} else if !strings.Contains(err.Error(), "not visible") {
		t.Fatalf("await error = %v, want a not-visible control error", err)
	}
	if _, err := testApp.app.subagents.cancel(newRoot, cancelSubagentArgs{ID: foreign.ID}); err == nil {
		t.Fatal("cancel of a foreign-scope worker unexpectedly succeeded")
	} else if !strings.Contains(err.Error(), "not visible") {
		t.Fatalf("cancel error = %v, want a not-visible control error", err)
	}

	// The foreign worker itself was not modified by the rejected operations:
	// it was cancelled by the switch, not by the new root's control attempts.
	if _, err := testApp.app.subagents.read(oldRoot, readSubagentArgs{ID: foreign.ID}); err != nil {
		t.Fatalf("the retired scope's own root lost access to its worker: %v", err)
	}
}

// TestInteractiveSessionSwitchCancelsOldScopeWorkers covers ticket 02: a
// successful switch requests cancellation of pending, queued, and running
// old-scope workers.
func TestInteractiveSessionSwitchCancelsOldScopeWorkers(t *testing.T) {
	scoped := newScopeTestApp(t)
	session, err := scoped.app.newInteractiveSession([]contracts.Message{{Role: "system", Content: "test"}})
	if err != nil {
		t.Fatal(err)
	}
	oldRoot := scoped.app.rootRuntime()

	pendingWorker, err := scoped.app.subagents.create(context.Background(), oldRoot, createSubagentArgs{Question: "pending work"})
	if err != nil {
		t.Fatal(err)
	}
	runningWorker, err := scoped.app.subagents.create(context.Background(), oldRoot, createSubagentArgs{Question: "running work"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := scoped.app.subagents.run(context.Background(), oldRoot, runSubagentArgs{ID: runningWorker.ID}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-scoped.started:
	case <-time.After(2 * time.Second):
		t.Fatal("the running worker never reached the runner")
	}

	if stopped := scoped.app.handleInteractiveInput(context.Background(), session, "/session-new"); stopped {
		t.Fatal("/session-new unexpectedly stopped the session")
	}

	// Cancellation is requested for both the not-yet-started and the running
	// worker. Observe the outcome through the retired scope's own root, which
	// is the only agent still permitted to read them.
	deadline := time.Now().Add(3 * time.Second)
	for {
		pendingSnap, err := scoped.app.subagents.read(oldRoot, readSubagentArgs{ID: pendingWorker.ID})
		if err != nil {
			t.Fatal(err)
		}
		runningSnap, err := scoped.app.subagents.read(oldRoot, readSubagentArgs{ID: runningWorker.ID})
		if err != nil {
			t.Fatal(err)
		}
		pendingStatus := pendingSnap.(subagentEnvelope).Status
		runningStatus := runningSnap.(subagentEnvelope).Status
		if pendingStatus == string(subagentStatusCancelled) && runningStatus == string(subagentStatusCancelled) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("switch did not cancel old-scope workers: pending=%s running=%s", pendingStatus, runningStatus)
		}
		time.Sleep(10 * time.Millisecond)
	}
	scoped.releaseWorkers()
}

// TestInteractiveSessionSwitchIsolatesOldScopeOutput proves an old-scope
// worker that finishes after the switch cannot emit into the new conversation
// and cannot be admitted through the new root.
func TestInteractiveSessionSwitchIsolatesOldScopeOutput(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})
	var testApp *interactiveTurnTestApp
	// Delegate to the real emitting runner so scope-based output suppression
	// is exercised rather than bypassed by a test double.
	testApp = newAgentScopeTestApp(t, func(ctx context.Context, runtime *agentRuntime, session *subagentSession) (string, bool, error) {
		close(started)
		<-release
		return testApp.app.runSubagentSession(ctx, runtime, session)
	})
	interactiveSession, err := testApp.app.newInteractiveSession([]contracts.Message{{Role: "system", Content: "test"}})
	if err != nil {
		t.Fatal(err)
	}
	oldRoot := testApp.app.rootRuntime()
	oldRoot.emitOutput = true
	worker, err := testApp.app.subagents.create(context.Background(), oldRoot, createSubagentArgs{Question: "slow old work"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := testApp.app.subagents.run(context.Background(), oldRoot, runSubagentArgs{ID: worker.ID}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("worker never started")
	}

	if stopped := testApp.app.handleInteractiveInput(context.Background(), interactiveSession, "/session-new"); stopped {
		t.Fatal("/session-new unexpectedly stopped the session")
	}

	// Everything the old worker emits from here on must be suppressed.
	var afterSwitch []string
	var mu sync.Mutex
	testApp.app.sink.(*spySink).onSystem = func(message string) {
		mu.Lock()
		afterSwitch = append(afterSwitch, message)
		mu.Unlock()
	}
	close(release)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		snap, err := testApp.app.subagents.read(oldRoot, readSubagentArgs{ID: worker.ID})
		if err != nil {
			t.Fatal(err)
		}
		if status := snap.(subagentEnvelope).Status; status != string(subagentStatusRunning) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	leaked := append([]string(nil), afterSwitch...)
	mu.Unlock()
	for _, message := range leaked {
		if strings.Contains(message, worker.ID) {
			t.Fatalf("old-scope worker output was attributed to the new conversation: %q", message)
		}
	}

	// The old worker can also never be admitted through the new root.
	newRoot := testApp.app.rootRuntime()
	if _, err := testApp.app.subagents.run(context.Background(), newRoot, runSubagentArgs{ID: worker.ID}); err == nil {
		t.Fatal("the new root admitted an old-scope worker")
	}
	view := agentsView(t, testApp, interactiveSession)
	if strings.Contains(view, worker.ID) {
		t.Fatalf("::agents rendered old-scope work in the new conversation: %q", view)
	}
}

// TestInteractiveFailedSwitchLeavesScopeAndWorkersUnchanged covers the
// recovery requirement: a failed /session-resume must not retire the scope,
// hide workers, or revoke worker control.
func TestInteractiveFailedSwitchLeavesScopeAndWorkersUnchanged(t *testing.T) {
	testApp := newAgentScopeTestApp(t, completingSubagentRunner)
	session, err := testApp.app.newInteractiveSession([]contracts.Message{{Role: "system", Content: "test"}, {Role: "user", Content: "current"}})
	if err != nil {
		t.Fatal(err)
	}

	// A worker from an earlier, already-retired scope establishes the negative
	// half of the assertion: a failed switch must preserve the boundary in
	// both directions, not merely avoid hiding the current scope's work.
	retiredWorker, err := testApp.app.subagents.create(context.Background(), testApp.app.rootRuntime(), createSubagentArgs{Question: "retired work"})
	if err != nil {
		t.Fatal(err)
	}
	if stopped := testApp.app.handleInteractiveInput(context.Background(), session, "/session-new"); stopped {
		t.Fatal("/session-new unexpectedly stopped the session")
	}

	root := testApp.app.rootRuntime()
	scopeBefore := root.scopeID
	worker, err := testApp.app.subagents.create(context.Background(), root, createSubagentArgs{Question: "current work"})
	if err != nil {
		t.Fatal(err)
	}
	originalID := session.id

	if err := testApp.app.switchToSavedSession(session, "not-a-session-path"); err == nil {
		t.Fatal("invalid resume selector unexpectedly succeeded")
	}

	if session.id != originalID || len(session.messages) != 1 {
		t.Fatalf("failed switch replaced the conversation: id=%s messages=%#v", session.id, session.messages)
	}
	if got := testApp.app.currentAgentScope(); got != scopeBefore {
		t.Fatalf("failed switch rotated the agent scope: %q -> %q", scopeBefore, got)
	}
	view := agentsView(t, testApp, session)
	if !strings.Contains(view, worker.ID) {
		t.Fatalf("failed switch hid a visible worker: %q", view)
	}
	// The retired scope must still be excluded; a failed switch is not an
	// opportunity for old-scope work to reappear.
	if strings.Contains(view, retiredWorker.ID) {
		t.Fatalf("failed switch exposed a retired-scope worker: %q", view)
	}
	if _, err := testApp.app.subagents.read(testApp.app.rootRuntime(), readSubagentArgs{ID: retiredWorker.ID}); err == nil {
		t.Fatal("failed switch granted control over a retired-scope worker")
	}
	if _, err := testApp.app.subagents.read(testApp.app.rootRuntime(), readSubagentArgs{ID: worker.ID}); err != nil {
		t.Fatalf("failed switch revoked worker control: %v", err)
	}
	snap, err := testApp.app.subagents.read(root, readSubagentArgs{ID: worker.ID})
	if err != nil {
		t.Fatal(err)
	}
	if status := snap.(subagentEnvelope).Status; status != string(subagentStatusPending) {
		t.Fatalf("failed switch changed worker status to %q", status)
	}
}

// TestInteractiveSessionSwitchWithoutWorkersSucceedsQuietly verifies a switch
// from a conversation with no outstanding workers produces no spurious
// cancellation error and still yields an empty new scope.
func TestInteractiveSessionSwitchWithoutWorkersSucceedsQuietly(t *testing.T) {
	testApp := newAgentScopeTestApp(t, completingSubagentRunner)
	session, err := testApp.app.newInteractiveSession([]contracts.Message{{Role: "system", Content: "test"}})
	if err != nil {
		t.Fatal(err)
	}
	scopeBefore := testApp.app.currentAgentScope()

	var systems []string
	testApp.app.sink.(*spySink).onSystem = func(message string) { systems = append(systems, message) }
	if stopped := testApp.app.handleInteractiveInput(context.Background(), session, "/session-new"); stopped {
		t.Fatal("/session-new unexpectedly stopped the session")
	}
	for _, message := range systems {
		if strings.Contains(message, "cancelled") || strings.Contains(message, "failed") {
			t.Fatalf("empty-scope switch reported spurious cancellation: %q", message)
		}
	}
	if got := testApp.app.currentAgentScope(); got == scopeBefore {
		t.Fatal("switch did not install a fresh agent scope")
	}
	view := agentsView(t, testApp, session)
	if !strings.Contains(view, "aggregate: total=0;") {
		t.Fatalf("switch did not produce an empty new scope: %q", view)
	}
}

// TestOneShotAndServerRuntimesKeepUnscopedSubagentBehavior pins the
// compatibility requirement that non-interactive execution modes are
// unaffected: they share the legacy empty scope and see their own children.
func TestOneShotAndServerRuntimesKeepUnscopedSubagentBehavior(t *testing.T) {
	manager := newSubagentManager(defaultSubagentRuntimeConfig(), func(context.Context, *agentRuntime, *subagentSession) (string, bool, error) {
		return "child result", false, nil
	})
	// A runtime with no scope is the one-shot/server identity.
	unscoped := &agentRuntime{sessionID: rootAgentID, depth: 0, allowedTools: map[string]bool{toolCreateSubagent: true}}
	child, err := manager.create(context.Background(), unscoped, createSubagentArgs{Question: "one-shot work"})
	if err != nil {
		t.Fatal(err)
	}
	if child.ScopeID != "" {
		t.Fatalf("unscoped runtime produced a scoped worker: %q", child.ScopeID)
	}
	items, err := manager.list(unscoped, listSubagentsArgs{})
	if err != nil || len(items) != 1 || items[0].ID != child.ID {
		t.Fatalf("unscoped list = %#v (err=%v), want the single one-shot child", items, err)
	}
	if _, err := manager.run(context.Background(), unscoped, runSubagentArgs{ID: child.ID, Wait: true}); err != nil {
		t.Fatalf("unscoped run failed: %v", err)
	}
	if nodes := manager.ListAll(); len(nodes) != 1 {
		t.Fatalf("ListAll = %#v, want the single one-shot child", nodes)
	}
}
