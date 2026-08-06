package app

import (
	configpkg "capelin-go/internal/config"
	"capelin-go/internal/contracts"
	toolpkg "capelin-go/internal/tools"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestApplicationToolBatchAdmitsAndRunsTenSubagentsWithinBounds(t *testing.T) {
	profile := configpkg.RuntimeProfile{
		MaxIterations: 40,
		Subagents: configpkg.SubagentConfig{
			MaxDepth:          1,
			MaxChildren:       2,
			MaxParallel:       8,
			DefaultTimeoutSec: 2,
			MaxTimeoutSec:     2,
			MaxToolIterations: 20,
			MaxResultChars:    8000,
			MaxAggregateCount: 12,
			MaxAggregateChars: 12000,
		},
		ToolMaxParallel:    10,
		ToolTimeoutSec:     2,
		ToolRetryOnTimeout: true,
	}
	var running atomic.Int64
	var peak atomic.Int64
	var started atomic.Int64
	updatePeak := func(value int64) {
		for {
			current := peak.Load()
			if value <= current || peak.CompareAndSwap(current, value) {
				return
			}
		}
	}

	a := &app{
		cfg: config{
			workspaceRoot:    t.TempDir(),
			allowedTools:     map[string]bool{toolCreateSubagent: true, toolRunSubagent: true, toolAwaitSubagent: true},
			ordinaryProfile:  profile,
			profilesResolved: true,
		},
	}
	a.subagents = newSubagentManager(toSubagentConfig(profile.Subagents), func(ctx context.Context, _ *agentRuntime, session *subagentSession) (string, error) {
		started.Add(1)
		current := running.Add(1)
		updatePeak(current)
		defer running.Add(-1)
		timer := time.NewTimer(20 * time.Millisecond)
		defer timer.Stop()
		select {
		case <-timer.C:
			return "completed:" + session.ID, nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	})
	root := &agentRuntime{
		sessionID:         rootAgentID,
		role:              agentRoleCoordinator,
		allowedTools:      cloneAllowedTools(a.cfg.allowedTools),
		maxToolIterations: profile.MaxIterations,
		executionProfile:  profile,
	}
	capability := newAppToolCapability(nil, a, root)

	creates := make([]contracts.ToolCall, 0, 10)
	for i := 0; i < 10; i++ {
		creates = append(creates, contracts.ToolCall{
			ID: fmt.Sprintf("create-%d", i),
			Function: contracts.FunctionCall{
				Name:      toolCreateSubagent,
				Arguments: fmt.Sprintf(`{"question":"child-%d","execution_mode":"parallel"}`, i),
			},
		})
	}
	createStart := time.Now()
	createResults := capability.Run(context.Background(), creates)
	if elapsed := time.Since(createStart); elapsed >= time.Second {
		t.Fatalf("create batch took %s; admission waited synchronously", elapsed)
	}
	if started.Load() != 0 {
		t.Fatalf("creation started %d children; create/run phases were not preserved", started.Load())
	}

	ids := make([]string, 0, len(createResults))
	for i, result := range createResults {
		if result.Call.ID != creates[i].ID {
			t.Fatalf("create result %d call id = %q, want %q", i, result.Call.ID, creates[i].ID)
		}
		if result.IsError {
			t.Fatalf("create result %d failed: %s", i, result.Output)
		}
		var snapshot struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		}
		if err := json.Unmarshal([]byte(result.Output), &snapshot); err != nil {
			t.Fatalf("decode create result %d: %v", i, err)
		}
		if snapshot.ID == "" || snapshot.Status != string(subagentStatusPending) {
			t.Fatalf("create result %d = %#v; want a pending handle", i, snapshot)
		}
		ids = append(ids, snapshot.ID)
	}

	runs := make([]contracts.ToolCall, 0, len(ids))
	for i, id := range ids {
		runs = append(runs, contracts.ToolCall{
			ID: fmt.Sprintf("run-%d", i),
			Function: contracts.FunctionCall{
				Name:      toolRunSubagent,
				Arguments: fmt.Sprintf(`{"id":%q,"wait":false,"execution_mode":"parallel"}`, id),
			},
		})
	}
	for i, result := range capability.Run(context.Background(), runs) {
		if result.Call.ID != runs[i].ID {
			t.Fatalf("run result %d call id = %q, want %q", i, result.Call.ID, runs[i].ID)
		}
		if result.IsError {
			t.Fatalf("run result %d failed: %s", i, result.Output)
		}
	}

	awaits := make([]contracts.ToolCall, 0, len(ids))
	for i, id := range ids {
		awaits = append(awaits, contracts.ToolCall{
			ID: fmt.Sprintf("await-%d", i),
			Function: contracts.FunctionCall{
				Name:      toolAwaitSubagent,
				Arguments: fmt.Sprintf(`{"id":%q,"timeout_seconds":2}`, id),
			},
		})
	}
	for i, result := range capability.Run(context.Background(), awaits) {
		if result.Call.ID != awaits[i].ID {
			t.Fatalf("await result %d call id = %q, want %q", i, result.Call.ID, awaits[i].ID)
		}
		if result.IsError {
			t.Fatalf("await result %d failed: %s", i, result.Output)
		}
		var snapshot struct {
			Status string `json:"status"`
		}
		if err := json.Unmarshal([]byte(result.Output), &snapshot); err != nil {
			t.Fatalf("decode await result %d: %v", i, err)
		}
		if snapshot.Status != string(subagentStatusCompleted) {
			t.Fatalf("await result %d status = %q, want completed", i, snapshot.Status)
		}
	}
	if got := peak.Load(); got > int64(profile.Subagents.MaxChildren) || got > int64(profile.Subagents.MaxParallel) {
		t.Fatalf("peak child concurrency = %d, want <= children %d and workers %d", got, profile.Subagents.MaxChildren, profile.Subagents.MaxParallel)
	}
	if started.Load() != int64(len(ids)) {
		t.Fatalf("started children = %d, want %d", started.Load(), len(ids))
	}
}

func batchSchedulingProfile() configpkg.RuntimeProfile {
	return configpkg.RuntimeProfile{
		MaxIterations: 20,
		Subagents: configpkg.SubagentConfig{
			MaxDepth:          1,
			MaxChildren:       1,
			MaxParallel:       1,
			DefaultTimeoutSec: 1,
			MaxTimeoutSec:     1,
			MaxToolIterations: 10,
			MaxResultChars:    8000,
			MaxAggregateCount: 12,
			MaxAggregateChars: 12000,
		},
		ToolMaxParallel:    8,
		ToolTimeoutSec:     3,
		ToolRetryOnTimeout: false,
	}
}

func newBatchSchedulingApp(t *testing.T, profile configpkg.RuntimeProfile, runner subagentRunner) (*app, *agentRuntime, appToolCapability) {
	t.Helper()
	a := &app{cfg: config{
		workspaceRoot: t.TempDir(),
		allowedTools: map[string]bool{
			toolCreateSubagent: true,
			toolRunSubagent:    true,
			toolAwaitSubagent:  true,
			toolListSubagents:  true,
			toolReadSubagent:   true,
			toolCancelSubagent: true,
		},
		ordinaryProfile:  profile,
		profilesResolved: true,
	}}
	a.subagents = newSubagentManager(toSubagentConfig(profile.Subagents), runner)
	root := &agentRuntime{
		sessionID:         rootAgentID,
		role:              agentRoleCoordinator,
		allowedTools:      cloneAllowedTools(a.cfg.allowedTools),
		maxToolIterations: profile.MaxIterations,
		executionProfile:  profile,
	}
	return a, root, newAppToolCapability(nil, a, root)
}

func batchToolCall(id, name, arguments string) contracts.ToolCall {
	return contracts.ToolCall{ID: id, Function: contracts.FunctionCall{Name: name, Arguments: arguments}}
}

func batchEnvelope(t *testing.T, result contracts.ToolResult) subagentEnvelope {
	t.Helper()
	if result.IsError {
		t.Fatalf("tool %s failed: %s", result.Call.ID, result.Output)
	}
	var envelope subagentEnvelope
	if err := json.Unmarshal([]byte(result.Output), &envelope); err != nil {
		t.Fatalf("decode %s result: %v", result.Call.ID, err)
	}
	return envelope
}

func waitForBatchStatuses(t *testing.T, a *app, want map[string]subagentStatus) map[string]contracts.SubagentNode {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		got := make(map[string]contracts.SubagentNode)
		for _, node := range a.subagents.ListAll() {
			got[node.ID] = node
		}
		ready := len(got) >= len(want)
		for id, status := range want {
			if node, ok := got[id]; !ok || node.Status != string(status) {
				ready = false
				break
			}
		}
		if ready {
			return got
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("subagent statuses did not reach %#v: %#v", want, a.subagents.ListAll())
	return nil
}

func TestApplicationBatchTerminalOutcomesReleaseCapacityForRetry(t *testing.T) {
	for _, test := range []struct {
		name       string
		wantStatus subagentStatus
		finish     func(context.CancelFunc, chan struct{}, *appToolCapability, context.Context, string)
	}{
		{name: "completed", wantStatus: subagentStatusCompleted, finish: func(_ context.CancelFunc, release chan struct{}, _ *appToolCapability, _ context.Context, _ string) {
			close(release)
		}},
		{name: "failed", wantStatus: subagentStatusFailed, finish: func(_ context.CancelFunc, release chan struct{}, _ *appToolCapability, _ context.Context, _ string) {
			close(release)
		}},
		{name: "cancelled", wantStatus: subagentStatusCancelled, finish: func(_ context.CancelFunc, _ chan struct{}, capability *appToolCapability, ctx context.Context, id string) {
			result := capability.Run(ctx, []contracts.ToolCall{batchToolCall("cancel-first", toolCancelSubagent, fmt.Sprintf(`{"id":%q}`, id))})[0]
			if result.IsError {
				panic(result.Output)
			}
		}},
		{name: "timed-out", wantStatus: subagentStatusTimedOut, finish: func(_ context.CancelFunc, _ chan struct{}, _ *appToolCapability, _ context.Context, _ string) {}},
	} {
		t.Run(test.name, func(t *testing.T) {
			profile := batchSchedulingProfile()
			profile.Subagents.MaxTimeoutSec = 2
			release := make(chan struct{})
			started := make(chan string, 1)
			a, _, capability := newBatchSchedulingApp(t, profile, func(ctx context.Context, _ *agentRuntime, session *subagentSession) (string, error) {
				if session.Question == "first" {
					started <- session.ID
					switch test.name {
					case "completed":
						<-release
						return "first complete", nil
					case "failed":
						<-release
						return "", errors.New("controlled child failure")
					default:
						<-ctx.Done()
						return "", ctx.Err()
					}
				}
				return "retryable child complete", nil
			})
			ctx := context.Background()
			first := batchEnvelope(t, capability.Run(ctx, []contracts.ToolCall{
				batchToolCall("create-first", toolCreateSubagent, `{"question":"first","execution_mode":"parallel"}`),
			})[0])
			second := batchEnvelope(t, capability.Run(ctx, []contracts.ToolCall{
				batchToolCall("create-second", toolCreateSubagent, `{"question":"second","timeout_seconds":2,"execution_mode":"parallel"}`),
			})[0])
			if result := capability.Run(ctx, []contracts.ToolCall{batchToolCall("run-first", toolRunSubagent, fmt.Sprintf(`{"id":%q,"wait":false}`, first.ID))})[0]; result.IsError {
				t.Fatalf("first run result failed: %s", result.Output)
			}
			<-started
			if result := capability.Run(ctx, []contracts.ToolCall{batchToolCall("run-second", toolRunSubagent, fmt.Sprintf(`{"id":%q,"wait":false}`, second.ID))})[0]; result.IsError {
				t.Fatalf("second run result failed: %s", result.Output)
			}

			capacity := capability.Run(ctx, []contracts.ToolCall{batchToolCall("create-overflow", toolCreateSubagent, `{"question":"overflow"}`)})[0]
			if !capacity.IsError || !strings.Contains(capacity.Output, "admission capacity exceeded") {
				t.Fatalf("overflow result = %#v; want actionable capacity error", capacity)
			}
			test.finish(nil, release, &capability, ctx, first.ID)

			statuses := waitForBatchStatuses(t, a, map[string]subagentStatus{
				first.ID:  test.wantStatus,
				second.ID: subagentStatusCompleted,
			})
			if statuses[first.ID].Status != string(test.wantStatus) {
				t.Fatalf("first status = %q, want %q", statuses[first.ID].Status, test.wantStatus)
			}

			retry := capability.Run(ctx, []contracts.ToolCall{batchToolCall("create-retry", toolCreateSubagent, `{"question":"retry"}`)})[0]
			retryEnvelope := batchEnvelope(t, retry)
			if result := capability.Run(ctx, []contracts.ToolCall{batchToolCall("run-retry", toolRunSubagent, fmt.Sprintf(`{"id":%q,"wait":true}`, retryEnvelope.ID))})[0]; result.IsError {
				t.Fatalf("retry run failed after %s release: %s", test.name, result.Output)
			}
		})
	}
}

func TestApplicationBatchCancellationFinalizesQueuedWorkAndReleasesCapacity(t *testing.T) {
	profile := batchSchedulingProfile()
	firstStarted := make(chan struct{})
	secondStarted := atomic.Int64{}
	a, _, capability := newBatchSchedulingApp(t, profile, func(ctx context.Context, _ *agentRuntime, session *subagentSession) (string, error) {
		if session.Question == "first" {
			close(firstStarted)
			<-ctx.Done()
			return "", ctx.Err()
		}
		if session.Question == "second" {
			secondStarted.Add(1)
		}
		return "unexpected execution", nil
	})
	parentCtx, cancelParent := context.WithCancel(context.Background())
	first := batchEnvelope(t, capability.Run(parentCtx, []contracts.ToolCall{
		batchToolCall("create-first", toolCreateSubagent, `{"question":"first"}`),
	})[0])
	second := batchEnvelope(t, capability.Run(parentCtx, []contracts.ToolCall{
		batchToolCall("create-second", toolCreateSubagent, `{"question":"second"}`),
	})[0])
	if result := capability.Run(parentCtx, []contracts.ToolCall{batchToolCall("run-first", toolRunSubagent, fmt.Sprintf(`{"id":%q,"wait":false}`, first.ID))})[0]; result.IsError {
		t.Fatalf("first run result failed: %s", result.Output)
	}
	<-firstStarted
	if result := capability.Run(parentCtx, []contracts.ToolCall{batchToolCall("run-second", toolRunSubagent, fmt.Sprintf(`{"id":%q,"wait":false}`, second.ID))})[0]; result.IsError {
		t.Fatalf("second run result failed: %s", result.Output)
	}
	cancelParent()
	waitForBatchStatuses(t, a, map[string]subagentStatus{
		first.ID:  subagentStatusCancelled,
		second.ID: subagentStatusCancelled,
	})
	if got := secondStarted.Load(); got != 0 {
		t.Fatalf("queued child started after parent cancellation: %d starts", got)
	}

	retry := capability.Run(context.Background(), []contracts.ToolCall{batchToolCall("create-retry", toolCreateSubagent, `{"question":"retry"}`)})[0]
	retryEnvelope := batchEnvelope(t, retry)
	result := capability.Run(context.Background(), []contracts.ToolCall{batchToolCall("run-retry", toolRunSubagent, fmt.Sprintf(`{"id":%q,"wait":true}`, retryEnvelope.ID))})[0]
	if result.IsError {
		t.Fatalf("capacity was not released after parent cancellation: %s", result.Output)
	}
}

func TestApplicationCancelledCreateDoesNotAdmitAnOrphan(t *testing.T) {
	a, _, capability := newBatchSchedulingApp(t, batchSchedulingProfile(), func(context.Context, *agentRuntime, *subagentSession) (string, error) {
		return "unexpected execution", nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result := capability.Run(ctx, []contracts.ToolCall{
		batchToolCall("create-after-cancel", toolCreateSubagent, `{"question":"orphan"}`),
	})[0]
	if !result.IsError || !strings.Contains(result.Output, "context canceled") {
		t.Fatalf("cancelled create result = %#v; want context cancellation error", result)
	}
	if got := a.subagents.ListAll(); len(got) != 0 {
		t.Fatalf("cancelled create admitted orphan sessions: %#v", got)
	}
}

func TestApplicationQueuedChildDeadlineFinalizesBeforeExecution(t *testing.T) {
	profile := batchSchedulingProfile()
	profile.Subagents.MaxTimeoutSec = 3
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	secondStarted := atomic.Int64{}
	a, _, capability := newBatchSchedulingApp(t, profile, func(ctx context.Context, _ *agentRuntime, session *subagentSession) (string, error) {
		if session.Question == "first" {
			close(firstStarted)
			select {
			case <-releaseFirst:
				return "first complete", nil
			case <-ctx.Done():
				return "", ctx.Err()
			}
		}
		if session.Question == "second" {
			secondStarted.Add(1)
		}
		return "unexpected execution", nil
	})

	first := batchEnvelope(t, capability.Run(context.Background(), []contracts.ToolCall{
		batchToolCall("create-first-deadline", toolCreateSubagent, `{"question":"first","timeout_seconds":3}`),
	})[0])
	second := batchEnvelope(t, capability.Run(context.Background(), []contracts.ToolCall{
		batchToolCall("create-second-deadline", toolCreateSubagent, `{"question":"second","timeout_seconds":1}`),
	})[0])
	if result := capability.Run(context.Background(), []contracts.ToolCall{batchToolCall("run-first-deadline", toolRunSubagent, fmt.Sprintf(`{"id":%q,"wait":false}`, first.ID))})[0]; result.IsError {
		t.Fatalf("run first failed: %s", result.Output)
	}
	<-firstStarted
	if result := capability.Run(context.Background(), []contracts.ToolCall{batchToolCall("run-second-deadline", toolRunSubagent, fmt.Sprintf(`{"id":%q,"wait":false}`, second.ID))})[0]; result.IsError {
		t.Fatalf("run second failed: %s", result.Output)
	}
	waitForBatchStatuses(t, a, map[string]subagentStatus{second.ID: subagentStatusTimedOut})
	if got := secondStarted.Load(); got != 0 {
		t.Fatalf("deadline-expired queued child started %d time(s)", got)
	}

	close(releaseFirst)
	waitForBatchStatuses(t, a, map[string]subagentStatus{first.ID: subagentStatusCompleted})
	retry := batchEnvelope(t, capability.Run(context.Background(), []contracts.ToolCall{
		batchToolCall("create-after-queued-timeout", toolCreateSubagent, `{"question":"retry"}`),
	})[0])
	result := capability.Run(context.Background(), []contracts.ToolCall{batchToolCall("run-after-queued-timeout", toolRunSubagent, fmt.Sprintf(`{"id":%q,"wait":true}`, retry.ID))})[0]
	if result.IsError {
		t.Fatalf("retry after queued timeout failed: %s", result.Output)
	}
}

func TestApplicationBatchListingAndAggregateExposeSchedulerStates(t *testing.T) {
	profile := batchSchedulingProfile()
	profile.Subagents.MaxChildren = 2
	profile.Subagents.MaxParallel = 1
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	a, _, capability := newBatchSchedulingApp(t, profile, func(_ context.Context, _ *agentRuntime, session *subagentSession) (string, error) {
		if session.Question == "running" {
			close(firstStarted)
			<-releaseFirst
		}
		return "done", nil
	})
	create := func(id, question string) subagentEnvelope {
		return batchEnvelope(t, capability.Run(context.Background(), []contracts.ToolCall{
			batchToolCall(id, toolCreateSubagent, fmt.Sprintf(`{"question":%q,"execution_mode":"parallel"}`, question)),
		})[0])
	}
	first := create("create-first", "running")
	second := create("create-second", "queued")
	third := create("create-third", "queued")
	ids := []string{first.ID, second.ID, third.ID}

	initial := capability.Run(context.Background(), []contracts.ToolCall{batchToolCall("list-initial", toolListSubagents, `{}`)})[0]
	if initial.IsError {
		t.Fatalf("initial list failed: %s", initial.Output)
	}
	var initialList []subagentEnvelope
	if err := json.Unmarshal([]byte(initial.Output), &initialList); err != nil {
		t.Fatalf("decode initial list: %v", err)
	}
	for _, item := range initialList {
		if item.Status != string(subagentStatusPending) {
			t.Fatalf("initial list item %s status = %q, want pending", item.ID, item.Status)
		}
	}

	if result := capability.Run(context.Background(), []contracts.ToolCall{batchToolCall("run-first", toolRunSubagent, fmt.Sprintf(`{"id":%q,"wait":false}`, first.ID))})[0]; result.IsError {
		t.Fatalf("run first failed: %s", result.Output)
	}
	<-firstStarted
	if result := capability.Run(context.Background(), []contracts.ToolCall{batchToolCall("run-second", toolRunSubagent, fmt.Sprintf(`{"id":%q,"wait":false}`, second.ID))})[0]; result.IsError {
		t.Fatalf("run second failed: %s", result.Output)
	}
	if result := capability.Run(context.Background(), []contracts.ToolCall{batchToolCall("run-third", toolRunSubagent, fmt.Sprintf(`{"id":%q,"wait":false}`, third.ID))})[0]; result.IsError {
		t.Fatalf("run third failed: %s", result.Output)
	}
	waitForBatchStatuses(t, a, map[string]subagentStatus{
		first.ID:  subagentStatusRunning,
		second.ID: subagentStatusQueued,
		third.ID:  subagentStatusQueued,
	})

	aggregateResult := capability.Run(context.Background(), []contracts.ToolCall{batchToolCall("read-in-flight", toolReadSubagent, fmt.Sprintf(`{"ids":[%q,%q,%q]}`, ids[0], ids[1], ids[2]))})[0]
	if aggregateResult.IsError {
		t.Fatalf("in-flight aggregate failed: %s", aggregateResult.Output)
	}
	var inFlight subagentAggregateEnvelope
	if err := json.Unmarshal([]byte(aggregateResult.Output), &inFlight); err != nil {
		t.Fatalf("decode in-flight aggregate: %v", err)
	}
	if inFlight.Pending != 0 || inFlight.Queued != 2 || inFlight.Running != 1 || inFlight.QueuedOrPending != 2 {
		t.Fatalf("in-flight aggregate state = %+v; want running=1 queued=2 pending=0", inFlight)
	}

	close(releaseFirst)
	awaits := make([]contracts.ToolCall, 0, len(ids))
	for i, id := range ids {
		awaits = append(awaits, batchToolCall(fmt.Sprintf("await-%d", i), toolAwaitSubagent, fmt.Sprintf(`{"id":%q}`, id)))
	}
	for _, result := range capability.Run(context.Background(), awaits) {
		if result.IsError {
			t.Fatalf("await failed: %s", result.Output)
		}
	}
	finalResult := capability.Run(context.Background(), []contracts.ToolCall{batchToolCall("read-final", toolReadSubagent, fmt.Sprintf(`{"ids":[%q,%q,%q]}`, ids[0], ids[1], ids[2]))})[0]
	if finalResult.IsError {
		t.Fatalf("final aggregate failed: %s", finalResult.Output)
	}
	var final subagentAggregateEnvelope
	if err := json.Unmarshal([]byte(finalResult.Output), &final); err != nil {
		t.Fatalf("decode final aggregate: %v", err)
	}
	if final.Completed != 3 || final.Pending != 0 || final.Queued != 0 || final.Running != 0 {
		t.Fatalf("final aggregate state = %+v; want all completed", final)
	}
}

func TestSubagentToolContractDescribesNonBlockingBoundedAdmission(t *testing.T) {
	catalog := toolpkg.Build(map[string]bool{
		toolpkg.CreateSubagent: true,
		toolpkg.RunSubagent:    true,
	})
	if len(catalog) != 2 {
		t.Fatalf("subagent catalog length = %d, want 2", len(catalog))
	}
	for _, tool := range catalog {
		switch tool.Function.Name {
		case toolpkg.CreateSubagent:
			if !strings.Contains(tool.Function.Description, "non-blocking") || !strings.Contains(tool.Function.Description, "capacity error") {
				t.Fatalf("create_subagent description does not describe bounded admission: %q", tool.Function.Description)
			}
			properties, ok := tool.Function.Parameters["properties"].(map[string]any)
			if !ok {
				t.Fatalf("create_subagent properties type = %T", tool.Function.Parameters["properties"])
			}
			overflow, ok := properties["overflow_mode"].(map[string]any)
			if !ok || !strings.Contains(fmt.Sprint(overflow["description"]), "without blocking") {
				t.Fatalf("overflow_mode description does not describe batch-safe admission: %#v", properties["overflow_mode"])
			}
		case toolpkg.RunSubagent:
			if !strings.Contains(tool.Function.Description, "separate phases") || !strings.Contains(tool.Function.Description, "Retry") {
				t.Fatalf("run_subagent description does not describe scheduling recovery: %q", tool.Function.Description)
			}
		default:
			t.Fatalf("unexpected tool %q", tool.Function.Name)
		}
	}
}
