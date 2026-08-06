package app

import (
	"capelin-go/internal/contracts"
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

func newBlockedGoalHeartbeatApp(t *testing.T) (*app, *interactiveSession, <-chan struct{}, chan struct{}, <-chan string) {
	t.Helper()
	started := make(chan struct{})
	release := make(chan struct{})
	events := make(chan string, 32)
	var startedOnce sync.Once
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		startedOnce.Do(func() { close(started) })
		select {
		case <-release:
			return &http.Response{
				StatusCode: http.StatusOK,
				Status:     "200 OK",
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(chatTurnResponse("done", "", nil))),
				Request:    r,
			}, nil
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
	})
	a := &app{
		cfg: config{
			model:             "test-model",
			maxIterations:     1,
			maxGoalIterations: 1,
			workspaceRoot:     t.TempDir(),
			toolMaxParallel:   1,
			toolTimeoutSec:    1,
			yolo:              true,
		},
		client: &client{
			endpoint: "http://goal.test/chat/completions",
			model:    "test-model",
			http:     &http.Client{Transport: transport},
		},
		sink: &spySink{onSystem: func(message string) { events <- message }},
	}
	session, err := a.newInteractiveSession(nil)
	if err != nil {
		t.Fatal(err)
	}
	return a, session, started, release, events
}

func waitForGoalHeartbeatEvent(t *testing.T, events <-chan string, want string) string {
	t.Helper()
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	for {
		select {
		case event := <-events:
			if strings.Contains(event, want) {
				return event
			}
		case <-timer.C:
			t.Fatalf("timed out waiting for goal event containing %q", want)
		}
	}
}

func TestInteractiveGoalHeartbeatReportsBlockedTurnAndStopsBeforeTerminalStatus(t *testing.T) {
	a, session, started, release, events := newBlockedGoalHeartbeatApp(t)
	a.goalHeartbeatInitialDelay = 10 * time.Millisecond
	a.goalHeartbeatCadence = 10 * time.Millisecond

	done := make(chan bool, 1)
	go func() { done <- a.runGoal(context.Background(), session, "blocked objective") }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("goal provider request did not start")
	}

	initial := waitForGoalHeartbeatEvent(t, events, "iteration 1/1 working")
	if !strings.Contains(initial, "total elapsed") || !strings.Contains(initial, "current turn elapsed") ||
		!strings.Contains(initial, "subagents 0 active; todos 0/0 completed; current: none") {
		t.Fatalf("immediate goal status omitted elapsed-time fields: %q", initial)
	}
	subsequent := waitForGoalHeartbeatEvent(t, events, "iteration 1/1 working")
	if !strings.Contains(subsequent, "current turn elapsed") ||
		!strings.Contains(subsequent, "subagents 0 active; todos 0/0 completed; current: none") {
		t.Fatalf("subsequent heartbeat omitted current-turn elapsed time: %q", subsequent)
	}

	close(release)
	select {
	case stopped := <-done:
		if stopped {
			t.Fatal("completed blocked goal unexpectedly stopped the session")
		}
	case <-time.After(time.Second):
		t.Fatal("blocked goal did not finish after provider release")
	}
	terminal := waitForGoalHeartbeatEvent(t, events, "[goal] incomplete")
	if strings.Contains(terminal, "working") {
		t.Fatalf("terminal goal status was not terminal: %q", terminal)
	}
	select {
	case event := <-events:
		t.Fatalf("heartbeat emitted after runGoal returned: %q", event)
	case <-time.After(30 * time.Millisecond):
	}
}

func TestInteractiveGoalHeartbeatReportsOrderedCurrentChecklistAndActiveSubagents(t *testing.T) {
	a, session, started, release, events := newBlockedGoalHeartbeatApp(t)
	a.goalHeartbeatInitialDelay = 10 * time.Millisecond
	a.goalHeartbeatCadence = 10 * time.Millisecond
	session.activeGoal = &goalState{Objective: "resume objective", Generation: 1}
	session.todos = []todoItem{
		{ID: "first", Content: " first  item ", Status: todoStatusInProgress},
		{ID: "second", Content: "completed item", Status: todoStatusCompleted},
		{ID: "third", Content: "third\nitem", Status: todoStatusInProgress},
	}
	syncRuntimeTodos(session)
	a.cfg.allowedTools = map[string]bool{toolListFiles: true}
	a.subagents = newSubagentManager(defaultSubagentRuntimeConfig(), nil)
	if _, err := a.subagents.create(context.Background(), a.rootRuntime(), createSubagentArgs{Question: "pending child"}); err != nil {
		t.Fatalf("create pending subagent: %v", err)
	}

	done := make(chan bool, 1)
	go func() { done <- a.runGoal(context.Background(), session, "") }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("goal provider request did not start")
	}

	heartbeat := waitForGoalHeartbeatEvent(t, events, "iteration 1/1 working")
	wantProgress := "subagents 1 active; todos 1/3 completed; current: first item, third item"
	if !strings.Contains(heartbeat, wantProgress) {
		t.Fatalf("heartbeat omitted ordered runtime progress: got %q, want substring %q", heartbeat, wantProgress)
	}

	close(release)
	select {
	case stopped := <-done:
		if stopped {
			t.Fatal("completed blocked goal unexpectedly stopped the session")
		}
	case <-time.After(time.Second):
		t.Fatal("blocked goal did not finish after provider release")
	}
	_ = waitForGoalHeartbeatEvent(t, events, "[goal] incomplete")
	select {
	case event := <-events:
		t.Fatalf("heartbeat emitted after runGoal returned: %q", event)
	case <-time.After(30 * time.Millisecond):
	}
}

func TestInteractiveGoalHeartbeatStopsBeforeCancellationStatus(t *testing.T) {
	a, session, started, release, events := newBlockedGoalHeartbeatApp(t)
	a.goalHeartbeatInitialDelay = 10 * time.Millisecond
	a.goalHeartbeatCadence = 10 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan bool, 1)
	go func() { done <- a.runGoal(ctx, session, "cancelled objective") }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("goal provider request did not start")
	}
	_ = waitForGoalHeartbeatEvent(t, events, "iteration 1/1 working")
	cancel()
	select {
	case stopped := <-done:
		if !stopped {
			t.Fatal("cancelled goal did not stop the session")
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled goal did not return")
	}
	close(release)
	terminal := waitForGoalHeartbeatEvent(t, events, "[goal] incomplete")
	if !strings.Contains(terminal, "cancelled") || strings.Contains(terminal, "working") {
		t.Fatalf("unexpected cancellation status: %q", terminal)
	}
	select {
	case event := <-events:
		t.Fatalf("heartbeat emitted after cancellation returned: %q", event)
	case <-time.After(30 * time.Millisecond):
	}
}

func TestGoalHeartbeatFormattingHandlesElapsedTimeAndStopIdempotently(t *testing.T) {
	var messages []string
	heartbeat := newGoalHeartbeat(func(message string) { messages = append(messages, message) }, 64, time.Hour, time.Hour, nil, nil)
	heartbeat.startedAt = time.Unix(100, 0)
	heartbeat.now = func() time.Time { return time.Unix(103, 0) }
	heartbeat.beginIteration(2)
	heartbeat.stop()
	heartbeat.stop()
	heartbeat.beginIteration(3)

	if len(messages) != 1 || messages[0] != "[goal] iteration 2/64 working; total elapsed 03s; current turn elapsed 00s; subagents 0 active; todos 0/0 completed; current: none" {
		t.Fatalf("unexpected deterministic heartbeat message: %#v", messages)
	}
	if heartbeat.started {
		t.Fatal("heartbeat unexpectedly restarted after stop")
	}
}

func TestGoalHeartbeatFormatsProgressFromOneOrderedSnapshot(t *testing.T) {
	var messages []string
	todosSnapshots := 0
	subagentSnapshots := 0
	heartbeat := newGoalHeartbeat(
		func(message string) { messages = append(messages, message) },
		64,
		time.Hour,
		time.Hour,
		func() []todoItem {
			todosSnapshots++
			return []todoItem{
				{ID: "first", Content: "pending", Status: todoStatusPending},
				{ID: "second", Content: " second\nitem ", Status: todoStatusInProgress},
				{ID: "third", Content: "completed", Status: todoStatusCompleted},
				{ID: "fourth", Content: "fourth", Status: todoStatusInProgress},
			}
		},
		func() []contracts.SubagentNode {
			subagentSnapshots++
			return []contracts.SubagentNode{
				{ID: "pending", Status: string(subagentStatusPending)},
				{ID: "queued", Status: string(subagentStatusQueued)},
				{ID: "running", Status: string(subagentStatusRunning)},
				{ID: "completed", Status: string(subagentStatusCompleted)},
				{ID: "failed", Status: string(subagentStatusFailed)},
				{ID: "cancelled", Status: string(subagentStatusCancelled)},
				{ID: "timed-out", Status: string(subagentStatusTimedOut)},
			}
		},
	)
	heartbeat.startedAt = time.Unix(0, 0)
	heartbeat.now = func() time.Time { return time.Unix(3723, 0) }
	heartbeat.beginIteration(2)
	heartbeat.stop()

	want := "[goal] iteration 2/64 working; total elapsed 1h2m03s; current turn elapsed 00s; subagents 3 active; todos 1/4 completed; current: second item, fourth"
	if len(messages) != 1 || messages[0] != want {
		t.Fatalf("unexpected progress heartbeat: got %#v, want %q", messages, want)
	}
	if todosSnapshots != 1 || subagentSnapshots != 1 {
		t.Fatalf("heartbeat used unexpected snapshots: todos=%d subagents=%d", todosSnapshots, subagentSnapshots)
	}
}

func TestGoalHeartbeatFormatsEmptyChecklistAndDurations(t *testing.T) {
	for _, test := range []struct {
		name  string
		value time.Duration
		want  string
	}{
		{name: "zero", value: 0, want: "00s"},
		{name: "seconds", value: 3 * time.Second, want: "03s"},
		{name: "minutes", value: 62 * time.Second, want: "1m02s"},
		{name: "hours", value: time.Hour + 2*time.Minute + 3*time.Second, want: "1h2m03s"},
		{name: "rounding", value: 59*time.Second + 500*time.Millisecond, want: "1m00s"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := formatGoalHeartbeatDuration(test.value); got != test.want {
				t.Fatalf("formatGoalHeartbeatDuration(%s) = %q, want %q", test.value, got, test.want)
			}
		})
	}

	var messages []string
	heartbeat := newGoalHeartbeat(func(message string) { messages = append(messages, message) }, 1, time.Hour, time.Hour, func() []todoItem {
		return []todoItem{}
	}, func() []contracts.SubagentNode {
		return nil
	})
	heartbeat.startedAt = time.Unix(0, 0)
	heartbeat.now = func() time.Time { return time.Unix(0, 0) }
	heartbeat.beginIteration(1)
	heartbeat.stop()
	if len(messages) != 1 || !strings.HasSuffix(messages[0], "; subagents 0 active; todos 0/0 completed; current: none") {
		t.Fatalf("unexpected empty-checklist heartbeat: %#v", messages)
	}
}
