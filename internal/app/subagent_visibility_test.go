package app

import (
	"capelin-go/internal/contracts"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// --- recordingSink ---

// recordingSink captures all OutputSink events with agent attribution for
// test assertions. It is safe for concurrent calls.
type recordingSink struct {
	mu     sync.Mutex
	events []recordedEvent
}

type recordedEvent struct {
	AgentID  string
	Type     string // "content", "tool_call", "tool_result", "system"
	Content  string
	ToolName string
	IsError  bool
}

func (s *recordingSink) WriteContent(agentID, content string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, recordedEvent{AgentID: agentID, Type: "content", Content: content})
}

func (s *recordingSink) WriteToolCall(agentID, toolName, args string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, recordedEvent{AgentID: agentID, Type: "tool_call", ToolName: toolName, Content: args})
}

func (s *recordingSink) WriteToolResult(agentID, toolName string, isError bool, detail string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, recordedEvent{AgentID: agentID, Type: "tool_result", ToolName: toolName, IsError: isError, Content: detail})
}

func (s *recordingSink) WriteSystem(agentID, msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, recordedEvent{AgentID: agentID, Type: "system", Content: msg})
}

func (s *recordingSink) snapshot() []recordedEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]recordedEvent, len(s.events))
	copy(out, s.events)
	return out
}

func (s *recordingSink) len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.events)
}

// --- test fixture ---

// mockCompletionHandler returns an http.HandlerFunc that serves a canned
// chat-completions response with the given content string.
func mockCompletionHandler(content string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]any{
			"choices": []map[string]any{{
				"message": map[string]string{
					"role":    "assistant",
					"content": content,
				},
				"finish_reason": "stop",
			}},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}
}

// newSubagentVisibilityApp creates a minimal app wired to a mock LLM provider
// and the supplied sink, suitable for exercising runSubagentSession end-to-end.
func newSubagentVisibilityApp(t *testing.T, sink contracts.OutputSink) *app {
	t.Helper()
	mockServer := httptest.NewServer(mockCompletionHandler("child completion output"))
	t.Cleanup(mockServer.Close)

	a := &app{
		cfg: config{
			workspaceRoot: t.TempDir(),
			allowedTools:  map[string]bool{toolListFiles: true, toolReadFile: true},
		},
		client: &client{
			endpoint: mockServer.URL,
			model:    "test-model",
			http:     &http.Client{},
		},
		sink: sink,
	}
	a.subagents = newSubagentManager(defaultSubagentRuntimeConfig(), a.runSubagentSession)
	return a
}

// --- Emitting-path tests ---

// TestSubagentVisibilityEmittingPath verifies that when emitOutput=true:
//   - Content events reach the sink attributed to the child's agent ID.
//   - Exactly one "running" system line appears before execution.
//   - Exactly one terminal system line appears after execution.
func TestSubagentVisibilityEmittingPath(t *testing.T) {
	sink := &recordingSink{}
	a := newSubagentVisibilityApp(t, sink)

	parent := a.rootRuntime()
	parent.emitOutput = true

	// Create + run a subagent synchronously (wait=true).
	session, err := a.subagents.create(context.Background(), parent, createSubagentArgs{
		Question:      "inspect the repo for issues",
		ExecutionMode: "sequential",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	result, err := a.subagents.run(context.Background(), parent, runSubagentArgs{
		ID:   session.ID,
		Wait: true,
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if result.Status != subagentStatusCompleted {
		t.Fatalf("expected completed, got %s", result.Status)
	}

	events := sink.snapshot()

	// Collect running and terminal lines for this child.
	var runningLines, terminalLines []recordedEvent
	for _, e := range events {
		if e.Type != "system" || e.AgentID != session.ID {
			continue
		}
		switch {
		case strings.HasPrefix(e.Content, "[subagent "+session.ID+"] running:"):
			runningLines = append(runningLines, e)
		case strings.HasPrefix(e.Content, "[subagent "+session.ID+"] "):
			// Terminal lines contain "completed", "failed", "cancelled", or "timed out".
			terminalLines = append(terminalLines, e)
		}
	}

	if len(runningLines) != 1 {
		t.Fatalf("expected exactly 1 running line, got %d: %v", len(runningLines), runningLines)
	}
	if !strings.Contains(runningLines[0].Content, "inspect the repo for issues") {
		t.Fatalf("running line missing question: %q", runningLines[0].Content)
	}

	if len(terminalLines) != 1 {
		t.Fatalf("expected exactly 1 terminal line, got %d: %v", len(terminalLines), terminalLines)
	}
	if !strings.Contains(terminalLines[0].Content, "completed in") {
		t.Fatalf("terminal line should report completion: %q", terminalLines[0].Content)
	}

	// Content events must be attributed to the child's agent ID.
	contentEvents := filterEvents(events, "content", session.ID)
	if len(contentEvents) == 0 {
		t.Fatal("expected at least one content event attributed to child ID")
	}
	for _, e := range contentEvents {
		if e.AgentID != session.ID {
			t.Fatalf("content event attributed to %q, want %q", e.AgentID, session.ID)
		}
	}
}

// TestSubagentVisibilitySuppressedPath verifies that when emitOutput=false:
//   - The recording sink receives no content events, no tool events, and no status lines.
//   - The child's returned output is still non-empty.
func TestSubagentVisibilitySuppressedPath(t *testing.T) {
	sink := &recordingSink{}
	a := newSubagentVisibilityApp(t, sink)

	parent := a.rootRuntime()
	parent.emitOutput = false

	session, err := a.subagents.create(context.Background(), parent, createSubagentArgs{
		Question:      "do some background work",
		ExecutionMode: "sequential",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	result, err := a.subagents.run(context.Background(), parent, runSubagentArgs{
		ID:   session.ID,
		Wait: true,
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if result.Status != subagentStatusCompleted {
		t.Fatalf("expected completed, got %s", result.Status)
	}

	// The sink must be completely empty — no content, tool, or status events.
	if n := sink.len(); n != 0 {
		t.Fatalf("suppressed path produced %d sink events: %v", n, sink.snapshot())
	}

	// The child's output must still be present in the session.
	if strings.TrimSpace(result.Output) == "" {
		t.Fatal("child output should be non-empty even when emit is suppressed")
	}
}

// --- Unit tests for helpers ---

func TestSubagentStatusQuestionCollapsesWhitespaceAndTruncates(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantSub string // substring that must appear in the result
		maxLen  int
	}{
		{
			name:    "normal question",
			input:   "inspect the repo",
			wantSub: "inspect the repo",
			maxLen:  120,
		},
		{
			name:    "collapses whitespace",
			input:   "  hello   world  \n\n  foo  ",
			wantSub: "hello world foo",
			maxLen:  120,
		},
		{
			name:    "truncates long question",
			input:   strings.Repeat("a", 200),
			wantSub: "… (+",
			maxLen:  200,
		},
		{
			name:    "unicode not split",
			input:   strings.Repeat("é", 200),
			wantSub: "… (+",
			maxLen:  200,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := subagentStatusQuestion(tc.input)
			if !strings.Contains(got, tc.wantSub) {
				t.Fatalf("result %q missing expected substring %q", got, tc.wantSub)
			}
			if len([]rune(got)) > tc.maxLen {
				t.Fatalf("result length %d exceeds max %d", len([]rune(got)), tc.maxLen)
			}
		})
	}
}

func TestSubagentOutcomeMapping(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{name: "nil error → completed", err: nil, want: "completed"},
		{name: "context.Canceled → cancelled", err: context.Canceled, want: "cancelled"},
		{name: "context.DeadlineExceeded → timed out", err: context.DeadlineExceeded, want: "timed out"},
		{name: "other error → failed", err: errors.New("something broke"), want: "failed"},
		{name: "wrapped cancel → cancelled", err: fmt.Errorf("outer: %w", context.Canceled), want: "cancelled"},
		{name: "wrapped deadline → timed out", err: fmt.Errorf("outer: %w", context.DeadlineExceeded), want: "timed out"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := subagentOutcome(tc.err)
			if got != tc.want {
				t.Fatalf("subagentOutcome(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}

// filterEvents returns events matching the given type and agent ID.
func filterEvents(events []recordedEvent, eventType, agentID string) []recordedEvent {
	var out []recordedEvent
	for _, e := range events {
		if e.Type == eventType && e.AgentID == agentID {
			out = append(out, e)
		}
	}
	return out
}
