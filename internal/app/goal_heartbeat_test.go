package app

import (
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
	if !strings.Contains(initial, "total elapsed") || !strings.Contains(initial, "current turn elapsed") {
		t.Fatalf("immediate goal status omitted elapsed-time fields: %q", initial)
	}
	subsequent := waitForGoalHeartbeatEvent(t, events, "iteration 1/1 working")
	if !strings.Contains(subsequent, "current turn elapsed") {
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
	heartbeat := newGoalHeartbeat(func(message string) { messages = append(messages, message) }, 64, time.Hour, time.Hour)
	heartbeat.startedAt = time.Unix(100, 0)
	heartbeat.now = func() time.Time { return time.Unix(103, 0) }
	heartbeat.beginIteration(2)
	heartbeat.stop()
	heartbeat.stop()
	heartbeat.beginIteration(3)

	if len(messages) != 1 || messages[0] != "[goal] iteration 2/64 working; total elapsed 3s; current turn elapsed 0s" {
		t.Fatalf("unexpected deterministic heartbeat message: %#v", messages)
	}
	if heartbeat.started {
		t.Fatal("heartbeat unexpectedly restarted after stop")
	}
}
