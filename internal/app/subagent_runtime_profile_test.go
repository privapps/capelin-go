package app

import (
	configpkg "capelin-go/internal/config"
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func subagentTestProfiles() (configpkg.RuntimeProfile, configpkg.RuntimeProfile) {
	ordinary := configpkg.RuntimeProfile{
		MaxIterations:     40,
		MaxGoalIterations: 20,
		Subagents: configpkg.SubagentConfig{
			MaxDepth:          1,
			MaxChildren:       8,
			MaxParallel:       4,
			DefaultTimeoutSec: 600,
			MaxTimeoutSec:     1800,
			MaxToolIterations: 20,
			MaxResultChars:    8000,
			MaxAggregateCount: 12,
			MaxAggregateChars: 12000,
		},
		ToolMaxParallel:    8,
		ToolTimeoutSec:     60,
		ToolRetryOnTimeout: true,
	}
	goal := ordinary
	goal.MaxIterations = 256
	goal.MaxGoalIterations = 64
	goal.Subagents.MaxDepth = 2
	goal.Subagents.MaxParallel = 8
	goal.Subagents.MaxToolIterations = 100
	goal.Subagents.MaxAggregateChars = 48000
	goal.ToolMaxParallel = 16
	goal.ToolTimeoutSec = 300
	return ordinary, goal
}

func runApplicationSubagentWithProfile(t *testing.T, profile configpkg.RuntimeProfile) (int, *agentRuntime, *subagentSession) {
	t.Helper()
	var requestCount atomic.Int32
	call := []map[string]any{{
		"id":   "list-files",
		"type": "function",
		"function": map[string]any{
			"name":      toolListFiles,
			"arguments": `{"path":"."}`,
		},
	}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		request := int(requestCount.Add(1))
		response := chatTurnResponse("subagent complete", "", nil)
		if request <= profile.Subagents.MaxToolIterations {
			response = chatTurnResponse("", "", call)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(response))
	}))
	t.Cleanup(server.Close)

	a := &app{
		cfg: config{
			model:            "test-model",
			workspaceRoot:    t.TempDir(),
			allowedTools:     map[string]bool{toolListFiles: true},
			yolo:             true,
			ordinaryProfile:  profile,
			profilesResolved: true,
		},
		client: &client{endpoint: server.URL, model: "test-model", http: server.Client()},
		sink:   &spySink{},
	}
	var observed *agentRuntime
	a.subagents = newSubagentManager(toSubagentConfig(profile.Subagents), func(ctx context.Context, runtime *agentRuntime, session *subagentSession) (string, error) {
		observed = runtime
		return a.runSubagentSession(ctx, runtime, session)
	})

	root := &agentRuntime{
		sessionID:         rootAgentID,
		role:              agentRoleCoordinator,
		allowedTools:      cloneAllowedTools(a.cfg.allowedTools),
		maxToolIterations: profile.MaxIterations,
		executionProfile:  profile,
		model:             a.cfg.model,
	}
	created, err := a.subagents.create(context.Background(), root, createSubagentArgs{Question: "inspect", ExecutionMode: "sequential"})
	if err != nil {
		t.Fatal(err)
	}
	completed, err := a.subagents.run(context.Background(), root, runSubagentArgs{ID: created.ID, Wait: true})
	if err != nil {
		t.Fatal(err)
	}
	if completed.Status != subagentStatusCompleted {
		t.Fatalf("subagent status = %s, error=%q", completed.Status, completed.Error)
	}
	return int(requestCount.Load()), observed, completed
}

func TestApplicationSubagentLoopUsesGoalAndOrdinaryIterationProfiles(t *testing.T) {
	ordinary, goal := subagentTestProfiles()
	tests := []struct {
		name           string
		profile        configpkg.RuntimeProfile
		wantRequests   int
		wantParallel   int
		wantDepth      int
		wantIterations int
		wantAggregate  int
	}{
		{name: "ordinary yolo", profile: ordinary, wantRequests: 21, wantParallel: 4, wantDepth: 1, wantIterations: 20, wantAggregate: 12000},
		{name: "goal", profile: goal, wantRequests: 101, wantParallel: 8, wantDepth: 2, wantIterations: 100, wantAggregate: 48000},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			requests, runtime, session := runApplicationSubagentWithProfile(t, test.profile)
			if requests != test.wantRequests {
				t.Fatalf("provider requests = %d, want %d (the final request is after the tool-iteration cap)", requests, test.wantRequests)
			}
			if runtime == nil {
				t.Fatal("subagent runner did not observe a runtime")
			}
			if runtime.maxToolIterations != test.wantIterations || runtime.executionProfile.MaxIterations != test.wantIterations {
				t.Fatalf("subagent iteration limits = max %d/profile %d, want %d", runtime.maxToolIterations, runtime.executionProfile.MaxIterations, test.wantIterations)
			}
			limits := runtime.executionProfile.Subagents
			if limits.MaxDepth != test.wantDepth || limits.MaxParallel != test.wantParallel || limits.MaxToolIterations != test.wantIterations || limits.MaxAggregateChars != test.wantAggregate {
				t.Fatalf("subagent limits = %+v", limits)
			}
			if limits.DefaultTimeoutSec != 600 || limits.MaxChildren != 8 || limits.MaxResultChars != 8000 || limits.MaxAggregateCount != 12 || limits.MaxTimeoutSec != 1800 {
				t.Fatalf("unchanged subagent limits were not retained: %+v", limits)
			}
			if !runtime.executionProfile.ToolRetryOnTimeout {
				t.Fatal("subagent timeout retry behavior was not inherited")
			}
			if session.Timeout.Seconds() != 600 {
				t.Fatalf("default child timeout = %s, want 600s", session.Timeout)
			}
			if got := runtime.allowedTools; len(got) != 1 || !got[toolListFiles] {
				t.Fatalf("subagent tool policy changed: %#v", got)
			}
		})
	}
}
