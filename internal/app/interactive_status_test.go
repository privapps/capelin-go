package app

import (
	"capelin-go/internal/contracts"
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestInteractiveStatusCompleterOffersColonCommands(t *testing.T) {
	completer := interactiveCommandCompleter()
	candidates, offset := completer.Do([]rune("::"), 2)
	if offset != 2 {
		t.Fatalf("unexpected :: completion offset: got %d, want 2", offset)
	}
	got := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		got = append(got, string(candidate))
	}
	// The catalog is sorted, so ::session takes its place between ::agents and
	// ::stats rather than being appended.
	want := []string{"agents ", "session ", "stats ", "todos "}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected :: candidates: got %#v, want %#v", got, want)
	}

	candidates, offset = completer.Do([]rune("::t"), 3)
	if offset != 3 || len(candidates) != 1 || string(candidates[0]) != "odos " {
		t.Fatalf("unique ::todos completion mismatch: candidates=%q offset=%d", candidates, offset)
	}
}

func TestInteractiveStatsEmitsPortableRuntimeFieldsLocally(t *testing.T) {
	testApp := newInteractiveTurnTestApp(t)
	testApp.app.cfg.reasoning = "high"
	var systems []string
	testApp.app.sink.(*spySink).onSystem = func(message string) { systems = append(systems, message) }
	session, err := testApp.app.newInteractiveSession(nil)
	if err != nil {
		t.Fatal(err)
	}
	if stopped := testApp.app.handleInteractiveInput(context.Background(), session, "::stats"); stopped {
		t.Fatal("::stats unexpectedly stopped the session")
	}
	if got := testApp.userPrompts(); len(got) != 0 {
		t.Fatalf("::stats reached the model: %#v", got)
	}
	if len(systems) != 1 {
		t.Fatalf("::stats emitted %d system messages, want 1: %#v", len(systems), systems)
	}
	line := systems[0]
	for _, want := range []string{
		"[::stats]", "cwd:", "model: test-model", "reasoning effort: high",
		"os: " + runtime.GOOS, "arch: " + runtime.GOARCH, "go version:", "cpus:",
		"gomaxprocs:", "goroutines:", "memory alloc:", "memory sys:", " KB",
	} {
		if !strings.Contains(line, want) {
			t.Fatalf("::stats output missing %q: %q", want, line)
		}
	}
}

func TestInteractiveStatsMarksUnsetValuesExplicitly(t *testing.T) {
	application := &app{cfg: config{}, sink: &spySink{}}
	var systems []string
	application.sink.(*spySink).onSystem = func(message string) { systems = append(systems, message) }
	application.handleStatusCommand(nil, "::stats", false, nil)
	if len(systems) != 1 {
		t.Fatalf("::stats emitted %d system messages, want 1: %#v", len(systems), systems)
	}
	for _, want := range []string{"model: unset", "reasoning effort: unset"} {
		if !strings.Contains(systems[0], want) {
			t.Fatalf("::stats did not mark unset value %q: %q", want, systems[0])
		}
	}
}

func TestInteractiveTodosShowsCommittedSnapshotNotInflight(t *testing.T) {
	testApp := newInteractiveTurnTestApp(t)
	var systems []string
	testApp.app.sink.(*spySink).onSystem = func(message string) { systems = append(systems, message) }
	session, err := testApp.app.newInteractiveSession(nil)
	if err != nil {
		t.Fatal(err)
	}
	session.todos = []todoItem{
		{ID: "1", Content: "first item", Status: todoStatusInProgress},
		{ID: "2", Content: "second item", Status: todoStatusCompleted},
	}
	syncRuntimeTodos(session)
	// Simulate a mutation still in flight in the active worker: it exists only
	// on the runtime and must not be visible through the committed snapshot.
	session.runtime.todosMu.Lock()
	session.runtime.todos = []todoItem{{ID: "9", Content: "in flight mutation", Status: todoStatusPending}}
	session.runtime.todosMu.Unlock()

	if stopped := testApp.app.handleInteractiveInput(context.Background(), session, "::todos"); stopped {
		t.Fatal("::todos unexpectedly stopped the session")
	}
	if got := testApp.userPrompts(); len(got) != 0 {
		t.Fatalf("::todos reached the model: %#v", got)
	}
	if len(systems) != 1 {
		t.Fatalf("::todos emitted %d system messages, want 1: %#v", len(systems), systems)
	}
	line := systems[0]
	for _, want := range []string{"[::todos]", "1 [in_progress] first item", "2 [completed] second item"} {
		if !strings.Contains(line, want) {
			t.Fatalf("::todos output missing %q: %q", want, line)
		}
	}
	if strings.Contains(line, "in flight mutation") {
		t.Fatalf("::todos exposed in-flight worker state: %q", line)
	}
}

func TestInteractiveTodosEmptyListHasClearLocalResult(t *testing.T) {
	testApp := newInteractiveTurnTestApp(t)
	var systems []string
	testApp.app.sink.(*spySink).onSystem = func(message string) { systems = append(systems, message) }
	session, err := testApp.app.newInteractiveSession(nil)
	if err != nil {
		t.Fatal(err)
	}
	if stopped := testApp.app.handleInteractiveInput(context.Background(), session, "::todos"); stopped {
		t.Fatal("::todos unexpectedly stopped the session")
	}
	if len(systems) != 1 || !strings.Contains(systems[0], "no committed todos") {
		t.Fatalf("empty ::todos result missing: %#v", systems)
	}
}

func TestInteractiveAgentsShowsRootHierarchyRolesAndAggregates(t *testing.T) {
	testApp := newInteractiveTurnTestApp(t)
	testApp.app.cfg.allowedTools = map[string]bool{toolListFiles: true, toolReadFile: true}
	testApp.app.subagents = newSubagentManager(defaultSubagentRuntimeConfig(), func(context.Context, *agentRuntime, *subagentSession) (string, bool, error) {
		return "child result", false, nil
	})
	root := testApp.app.rootRuntime()
	first, err := testApp.app.subagents.create(context.Background(), root, createSubagentArgs{Name: "worker-a", Question: "inspect one"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := testApp.app.subagents.create(context.Background(), root, createSubagentArgs{Question: "inspect two"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := testApp.app.subagents.run(context.Background(), root, runSubagentArgs{ID: first.ID, Wait: true}); err != nil {
		t.Fatalf("run subagent: %v", err)
	}
	var systems []string
	testApp.app.sink.(*spySink).onSystem = func(message string) { systems = append(systems, message) }
	session, err := testApp.app.newInteractiveSession(nil)
	if err != nil {
		t.Fatal(err)
	}
	if stopped := testApp.app.handleInteractiveInput(context.Background(), session, "::agents"); stopped {
		t.Fatal("::agents unexpectedly stopped the session")
	}
	if got := testApp.userPrompts(); len(got) != 0 {
		t.Fatalf("::agents reached the model: %#v", got)
	}
	if len(systems) != 1 {
		t.Fatalf("::agents emitted %d system messages, want 1: %#v", len(systems), systems)
	}
	line := systems[0]
	for _, want := range []string{
		"[::agents]",
		"root (coordinator, idle)",
		first.ID + " (worker, completed, name: worker-a)",
		second.ID + " (worker, pending)",
		"aggregate: total=2; active=1; pending=1 queued=0 running=0; completed=1 failed=0 cancelled=0 timed_out=0",
	} {
		if !strings.Contains(line, want) {
			t.Fatalf("::agents output missing %q: %q", want, line)
		}
	}
}

func TestStatusCommandsRunDuringBusyTurnWhileOrdinaryInputIsRejected(t *testing.T) {
	testApp := newInteractiveTurnTestApp(t)
	session, err := testApp.app.newInteractiveSession([]contracts.Message{{Role: "system", Content: "test"}})
	if err != nil {
		t.Fatal(err)
	}
	session.todos = []todoItem{{ID: "1", Content: "committed work", Status: todoStatusPending}}
	syncRuntimeTodos(session)
	started := make(chan struct{})
	release := make(chan struct{})
	var controller *interactiveTurnController
	controller = newInteractiveTurnController(nil, nil, nil, func(outcome interactiveTurnOutcome) {
		testApp.app.finishInteractiveTurn(controller, session, outcome)
	}, nil)
	if !testApp.app.startInteractiveTurn(context.Background(), controller, session, func(_ context.Context, _ *interactiveSession) (bool, error) {
		close(started)
		<-release
		return false, nil
	}) {
		t.Fatal("busy turn did not start")
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("busy turn did not reach the blocking worker")
	}
	var systems []string
	testApp.app.sink.(*spySink).onSystem = func(message string) { systems = append(systems, message) }

	for _, input := range []string{"::stats", "::todos", "::agents", "::stats extra", "::bogus"} {
		if stopped := testApp.app.handleInteractiveInputAsync(context.Background(), controller, session, nil, nil, input); stopped {
			t.Fatalf("%q unexpectedly ended the session", input)
		}
	}
	if got := testApp.userPrompts(); len(got) != 0 {
		t.Fatalf("busy status commands reached the model: %#v", got)
	}
	// Ordinary prompts and existing slash commands keep their busy rejection.
	if stopped := testApp.app.handleInteractiveInputAsync(context.Background(), controller, session, nil, nil, "ordinary prompt"); stopped {
		t.Fatal("busy ordinary prompt unexpectedly ended the session")
	}
	if stopped := testApp.app.handleInteractiveInputAsync(context.Background(), controller, session, nil, nil, "/save"); stopped {
		t.Fatal("busy /save unexpectedly ended the session")
	}
	if got := testApp.userPrompts(); len(got) != 0 {
		t.Fatalf("busy ordinary input reached the model: %#v", got)
	}
	if _, err := os.Stat(filepath.Join(testApp.workspaceRoot, interactiveResponseFile)); !os.IsNotExist(err) {
		t.Fatalf("busy /save wrote the response file: %v", err)
	}
	joined := strings.Join(systems, "\n")
	for _, want := range []string{
		"[::stats]", "[::todos]", "committed work", "[::agents]",
		"root (coordinator, active)", "unknown status command", "accepts no arguments",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("busy status output missing %q: %q", want, joined)
		}
	}
	if len(session.messages) != 1 || len(session.todos) != 1 {
		t.Fatalf("status commands mutated session state: messages=%d todos=%#v", len(session.messages), session.todos)
	}

	close(release)
	controller.wait()
	systems = nil
	if stopped := testApp.app.handleInteractiveInputAsync(context.Background(), controller, session, nil, nil, "::agents"); stopped {
		t.Fatal("idle ::agents unexpectedly ended the session")
	}
	if len(systems) != 1 || !strings.Contains(systems[0], "root (coordinator, idle)") {
		t.Fatalf("idle ::agents root status missing: %#v", systems)
	}
	// Recovery to ordinary input after the busy period.
	if stopped := testApp.app.handleInteractiveInputAsync(context.Background(), controller, session, nil, nil, "ordinary prompt"); stopped {
		t.Fatal("ordinary prompt after busy unexpectedly ended the session")
	}
	deadline := time.After(2 * time.Second)
	for len(testApp.userPrompts()) < 1 {
		select {
		case <-deadline:
			t.Fatal("ordinary prompt after busy never reached the model")
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func TestStatusCommandsLeaveSessionAndPersistenceUnchanged(t *testing.T) {
	testApp := newInteractiveTurnTestApp(t)
	var systems []string
	testApp.app.sink.(*spySink).onSystem = func(message string) { systems = append(systems, message) }
	session, err := testApp.app.newInteractiveSession([]contracts.Message{{Role: "system", Content: "test"}})
	if err != nil {
		t.Fatal(err)
	}
	session.todos = []todoItem{{ID: "1", Content: "keep", Status: todoStatusPending}}
	syncRuntimeTodos(session)
	session.lastResponse = "kept response"
	if err := testApp.app.saveInteractiveSession(session); err != nil {
		t.Fatal(err)
	}
	before, err := testApp.app.sessionStore.resolve(session.id)
	if err != nil {
		t.Fatal(err)
	}
	for _, input := range []string{"::stats", "::todos", "::agents", "::bogus", "::stats extra"} {
		if stopped := testApp.app.handleInteractiveInput(context.Background(), session, input); stopped {
			t.Fatalf("%q unexpectedly stopped the session", input)
		}
	}
	if got := testApp.userPrompts(); len(got) != 0 {
		t.Fatalf("status commands reached the model: %#v", got)
	}
	if len(session.messages) != 1 || session.lastResponse != "kept response" || len(session.todos) != 1 {
		t.Fatalf("status commands mutated session state: messages=%d response=%q todos=%#v", len(session.messages), session.lastResponse, session.todos)
	}
	after, err := testApp.app.sessionStore.resolve(session.id)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before.Todos, after.Todos) || !reflect.DeepEqual(before.Messages, after.Messages) || before.LastContent != after.LastContent {
		t.Fatalf("status commands changed persisted session state: before=%#v after=%#v", before, after)
	}
	if len(systems) != 5 {
		t.Fatalf("expected 5 local status outputs, got %d: %#v", len(systems), systems)
	}
}

// TestInteractiveAgentsWithNoSubagentsShowsOnlyRoot covers the gap where the
// manager has never created a child: the root coordinator must still render
// with an aggregate total of zero and no child lines.
func TestInteractiveAgentsWithNoSubagentsShowsOnlyRoot(t *testing.T) {
	testApp := newInteractiveTurnTestApp(t)
	// A manager with no created children.
	testApp.app.subagents = newSubagentManager(defaultSubagentRuntimeConfig(), func(context.Context, *agentRuntime, *subagentSession) (string, bool, error) {
		return "child result", false, nil
	})
	var systems []string
	testApp.app.sink.(*spySink).onSystem = func(message string) { systems = append(systems, message) }
	session, err := testApp.app.newInteractiveSession(nil)
	if err != nil {
		t.Fatal(err)
	}
	if stopped := testApp.app.handleInteractiveInput(context.Background(), session, "::agents"); stopped {
		t.Fatal("::agents unexpectedly stopped the session")
	}
	if got := testApp.userPrompts(); len(got) != 0 {
		t.Fatalf("::agents reached the model: %#v", got)
	}
	if len(systems) != 1 {
		t.Fatalf("::agents emitted %d system messages, want 1: %#v", len(systems), systems)
	}
	line := systems[0]
	for _, want := range []string{
		"[::agents]",
		"root (coordinator, idle)",
		"aggregate: total=0; active=0; pending=0 queued=0 running=0; completed=0 failed=0 cancelled=0 timed_out=0",
	} {
		if !strings.Contains(line, want) {
			t.Fatalf("no-subagent ::agents output missing %q: %q", want, line)
		}
	}
	// Root is the only entry; no child/worker line may appear.
	if strings.Contains(line, "(worker,") {
		t.Fatalf("no-subagent ::agents unexpectedly listed a worker: %q", line)
	}
}

// TestInteractiveStatusCompletionContinuesMidLine proves that the :: namespace
// completion offers the full command catalog even when the token is embedded
// after existing text and the cursor is at the end of the line.
func TestInteractiveStatusCompletionContinuesMidLine(t *testing.T) {
	completer := interactiveCommandCompleter()
	line := []rune("status: ::")
	candidates, offset := completer.Do(line, len(line))
	if offset != 2 {
		// The completer returns pos-tokenStart; the cursor is at the end of the
		// line and the token starts at index 8, so the replaced span is 2.
		t.Fatalf("unexpected continuation offset: got %d, want %d", offset, 2)
	}
	got := runeStrings(candidates)
	want := []string{"agents ", "session ", "stats ", "todos "}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("mid-line :: candidates: got %#v, want %#v", got, want)
	}

	// Partial continuation after existing text.
	line = []rune("before ::st")
	candidates, offset = completer.Do(line, len(line))
	if offset != 4 {
		t.Fatalf("unexpected partial offset: got %d, want %d", offset, 4)
	}
	got = runeStrings(candidates)
	if !reflect.DeepEqual(got, []string{"ats "}) {
		t.Fatalf("mid-line ::st candidates: got %#v, want [ats ]", got)
	}
}

// TestInteractiveColonBranchReplacementSpan proves the :: branch in
// completionCandidates replaces the entire typed :: token (including the
// sentinel) rather than inserting, both at the end of a line and after
// existing trailing text.
func TestInteractiveColonBranchReplacementSpan(t *testing.T) {
	prefix := "::t"
	names := []string{"::todos", "::agents", "::stats"}
	line := []rune("::t")
	candidates, offset := completionCandidates(prefix, names, 0, line, len(line))
	if offset != len(line) {
		t.Fatalf("replacement offset should span the whole token: got %d, want %d", offset, len(line))
	}
	if got := runeStrings(candidates); !reflect.DeepEqual(got, []string{"odos "}) {
		t.Fatalf("end-of-line ::t replacement: got %#v, want [odos ]", got)
	}

	// Continuation with trailing text on the line: the token start is the
	// position of the leading ':' so the whole ::t is replaced.
	full := []rune("run ::t now")
	tokenStart := 4 // index of the first ':'
	candidates, offset = completionCandidates("::t", names, tokenStart, full, len(full)-len(" now"))
	if offset != len(full)-len(" now")-tokenStart {
		t.Fatalf("mid-line replacement offset: got %d, want %d", offset, len(full)-len(" now")-tokenStart)
	}
	if got := runeStrings(candidates); !reflect.DeepEqual(got, []string{"odos"}) {
		t.Fatalf("mid-line ::t replacement: got %#v, want [odos]", got)
	}
}

// --- Ticket 03: readable ::stats memory ---

// TestFormatGroupedUintGroupsDecimalDigits pins the local comma-grouping
// formatter across the boundaries where grouping logic changes, including the
// full uint64 range, so ::stats never depends on a locale package.
func TestFormatGroupedUintGroupsDecimalDigits(t *testing.T) {
	for _, testCase := range []struct {
		value uint64
		want  string
	}{
		{0, "0"},
		{7, "7"},
		{999, "999"},
		{1000, "1,000"},
		{1024, "1,024"},
		{4100, "4,100"},
		{12345, "12,345"},
		{33151, "33,151"},
		{100000, "100,000"},
		{1234567, "1,234,567"},
		{math.MaxUint64, "18,446,744,073,709,551,615"},
	} {
		if got := formatGroupedUint(testCase.value); got != testCase.want {
			t.Fatalf("formatGroupedUint(%d) = %q, want %q", testCase.value, got, testCase.want)
		}
	}
}

// TestInteractiveStatsReportsMemoryInGroupedKilobytes covers the ticket-03
// acceptance criteria through the application seam: both memory counters are
// integer 1024-byte KB with comma grouping, no raw byte rendering remains, and
// every other diagnostic field is still present.
func TestInteractiveStatsReportsMemoryInGroupedKilobytes(t *testing.T) {
	testApp := newInteractiveTurnTestApp(t)
	testApp.app.cfg.reasoning = "high"
	var systems []string
	testApp.app.sink.(*spySink).onSystem = func(message string) { systems = append(systems, message) }
	session, err := testApp.app.newInteractiveSession(nil)
	if err != nil {
		t.Fatal(err)
	}
	if stopped := testApp.app.handleInteractiveInput(context.Background(), session, "::stats"); stopped {
		t.Fatal("::stats unexpectedly stopped the session")
	}
	if got := testApp.userPrompts(); len(got) != 0 {
		t.Fatalf("::stats reached the model: %#v", got)
	}
	if len(systems) != 1 {
		t.Fatalf("::stats emitted %d system messages, want 1: %#v", len(systems), systems)
	}
	line := systems[0]
	allocPattern := regexp.MustCompile(`memory alloc: [0-9]{1,3}(,[0-9]{3})* KB`)
	sysPattern := regexp.MustCompile(`memory sys: [0-9]{1,3}(,[0-9]{3})* KB`)
	if !allocPattern.MatchString(line) {
		t.Fatalf("::stats memory alloc is not grouped KB: %q", line)
	}
	if !sysPattern.MatchString(line) {
		t.Fatalf("::stats memory sys is not grouped KB: %q", line)
	}
	if strings.Contains(line, " bytes") {
		t.Fatalf("::stats still reports raw bytes: %q", line)
	}
	// Every pre-existing diagnostic field must survive the unit change.
	for _, want := range []string{
		"[::stats]", "cwd:", "model: test-model", "reasoning effort: high",
		"os: " + runtime.GOOS, "arch: " + runtime.GOARCH, "go version:", "cpus:",
		"gomaxprocs:", "goroutines:",
	} {
		if !strings.Contains(line, want) {
			t.Fatalf("::stats output missing %q: %q", want, line)
		}
	}
}

// --- Ticket 02: ::agents questions and descendant counts ---

// TestCountAgentDescendantsCountsNestedWork proves the count is recursive
// rather than direct-children only, and that a malformed cyclic hierarchy
// terminates instead of recursing forever.
func TestCountAgentDescendantsCountsNestedWork(t *testing.T) {
	children := map[string][]contracts.SubagentNode{
		rootAgentID:  {{ID: "subagent-1"}, {ID: "subagent-4"}},
		"subagent-1": {{ID: "subagent-2"}},
		"subagent-2": {{ID: "subagent-3"}},
	}
	memo := map[string]int{}
	if got := countAgentDescendants(children, rootAgentID, map[string]bool{}, memo); got != 4 {
		t.Fatalf("root descendants = %d, want 4", got)
	}
	if got := memo["subagent-1"]; got != 2 {
		t.Fatalf("subagent-1 descendants = %d, want 2", got)
	}
	if got := memo["subagent-2"]; got != 1 {
		t.Fatalf("subagent-2 descendants = %d, want 1", got)
	}
	if got := memo["subagent-3"]; got != 0 {
		t.Fatalf("subagent-3 descendants = %d, want 0", got)
	}
	if got := memo["subagent-4"]; got != 0 {
		t.Fatalf("subagent-4 descendants = %d, want 0", got)
	}

	cyclic := map[string][]contracts.SubagentNode{
		rootAgentID:  {{ID: "subagent-1"}},
		"subagent-1": {{ID: "subagent-2"}},
		"subagent-2": {{ID: "subagent-1"}},
	}
	if got := countAgentDescendants(cyclic, rootAgentID, map[string]bool{}, map[string]int{}); got < 1 {
		t.Fatalf("cyclic hierarchy count = %d, want at least 1 without recursing forever", got)
	}
}

// TestInteractiveAgentsShowsQuestionsAndDescendantCounts exercises the
// ticket-02 rendering through the application seam with a real subagent
// manager: questions are normalized to one line, the synthetic root reports
// the total subagent count, and a middle node's count includes grandchildren.
func TestInteractiveAgentsShowsQuestionsAndDescendantCounts(t *testing.T) {
	testApp := newInteractiveTurnTestApp(t)
	cfg := defaultSubagentRuntimeConfig()
	cfg.MaxDepth = 3
	testApp.app.subagents = newSubagentManager(cfg, func(context.Context, *agentRuntime, *subagentSession) (string, bool, error) {
		return "child result", false, nil
	})
	newTestRuntime := func(id string, depth int) *agentRuntime {
		return &agentRuntime{
			sessionID:         id,
			depth:             depth,
			allowedTools:      map[string]bool{toolListFiles: true, toolCreateSubagent: true},
			maxToolIterations: 5,
		}
	}
	root := newTestRuntime(rootAgentID, 0)
	parent, err := testApp.app.subagents.create(context.Background(), root, createSubagentArgs{
		Name:     "worker-a",
		Question: "inspect\n  the   repo\tfor issues",
	})
	if err != nil {
		t.Fatal(err)
	}
	child, err := testApp.app.subagents.create(context.Background(), newTestRuntime(parent.ID, parent.Depth), createSubagentArgs{Question: "nested work"})
	if err != nil {
		t.Fatal(err)
	}
	childRuntime := newTestRuntime(child.ID, child.Depth)
	grandchild, err := testApp.app.subagents.create(context.Background(), childRuntime, createSubagentArgs{Question: "   "})
	if err == nil {
		t.Fatalf("whitespace-only question was accepted: %#v", grandchild)
	}
	grandchild, err = testApp.app.subagents.create(context.Background(), childRuntime, createSubagentArgs{Question: "deep work"})
	if err != nil {
		t.Fatal(err)
	}

	var systems []string
	testApp.app.sink.(*spySink).onSystem = func(message string) { systems = append(systems, message) }
	session, err := testApp.app.newInteractiveSession(nil)
	if err != nil {
		t.Fatal(err)
	}
	if stopped := testApp.app.handleInteractiveInput(context.Background(), session, "::agents"); stopped {
		t.Fatal("::agents unexpectedly stopped the session")
	}
	if got := testApp.userPrompts(); len(got) != 0 {
		t.Fatalf("::agents reached the model: %#v", got)
	}
	if len(systems) != 1 {
		t.Fatalf("::agents emitted %d system messages, want 1: %#v", len(systems), systems)
	}
	line := systems[0]
	for _, want := range []string{
		"[::agents]",
		// The synthetic root reports the total subagent descendant count.
		"root (coordinator, idle) descendants: 3",
		// Existing identity rendering is preserved; question and count follow it.
		parent.ID + " (worker, pending, name: worker-a) question: inspect the repo for issues; descendants: 2",
		// A middle node counts its grandchildren too.
		child.ID + " (worker, pending) question: nested work; descendants: 1",
		// A leaf renders its question and omits the descendant field.
		grandchild.ID + " (worker, pending) question: deep work",
		"aggregate: total=3; active=3; pending=3 queued=0 running=0; completed=0 failed=0 cancelled=0 timed_out=0",
	} {
		if !strings.Contains(line, want) {
			t.Fatalf("::agents output missing %q: %q", want, line)
		}
	}
	if strings.Contains(line, grandchild.ID+" (worker, pending) question: deep work; descendants:") {
		t.Fatalf("leaf agent rendered a descendant field: %q", line)
	}
	// Newlines inside a question must never leak into the rendered tree.
	for _, rendered := range strings.Split(line, "\n") {
		if strings.Contains(rendered, "question: ") && strings.Contains(rendered, "\t") {
			t.Fatalf("question was not whitespace-normalized: %q", rendered)
		}
	}
	// Hierarchy indentation is preserved: the grandchild is nested deepest.
	if !strings.Contains(line, "\n      - "+grandchild.ID) {
		t.Fatalf("::agents lost hierarchy indentation: %q", line)
	}
}

// TestInteractiveAgentsOmitsEmptyQuestionAndZeroDescendants pins the omission
// rules directly on the renderer, including the whitespace-only question case
// that the manager itself refuses to create.
func TestInteractiveAgentsOmitsEmptyQuestionAndZeroDescendants(t *testing.T) {
	if got := agentDetailSuffix("", 0); got != "" {
		t.Fatalf("empty detail suffix = %q, want empty", got)
	}
	if got := agentDetailSuffix("   \n\t ", 0); got != "" {
		t.Fatalf("whitespace-only question suffix = %q, want empty", got)
	}
	if got := agentDetailSuffix("", 2); got != " descendants: 2" {
		t.Fatalf("descendants-only suffix = %q", got)
	}
	if got := agentDetailSuffix(" ask\n  something ", 0); got != " question: ask something" {
		t.Fatalf("question-only suffix = %q", got)
	}
	if got := agentDetailSuffix("ask", 3); got != " question: ask; descendants: 3" {
		t.Fatalf("combined suffix = %q", got)
	}

	// The zero-subagent case must not render a root descendant field.
	testApp := newInteractiveTurnTestApp(t)
	testApp.app.subagents = newSubagentManager(defaultSubagentRuntimeConfig(), func(context.Context, *agentRuntime, *subagentSession) (string, bool, error) {
		return "child result", false, nil
	})
	rendered := testApp.app.formatInteractiveAgents(false, false)
	if !strings.Contains(rendered, "root (coordinator, idle)") {
		t.Fatalf("zero-subagent root line missing: %q", rendered)
	}
	if strings.Contains(rendered, "descendants:") {
		t.Fatalf("zero-subagent output rendered a descendant field: %q", rendered)
	}
	if strings.Contains(rendered, "question:") {
		t.Fatalf("zero-subagent output rendered a question field: %q", rendered)
	}
}

// --- Ticket 04: ::agents rename and deprecated ::agent alias ---

// TestAgentsDeprecatedAgentAliasEmitsNoteAndRenders verifies the retired
// singular spelling behaves like ::agents but first emits exactly one
// deprecation note line, and the same [::agents] view is rendered.
func TestAgentsDeprecatedAgentAliasEmitsNoteAndRenders(t *testing.T) {
	testApp := newInteractiveTurnTestApp(t)
	testApp.app.subagents = newSubagentManager(defaultSubagentRuntimeConfig(), func(context.Context, *agentRuntime, *subagentSession) (string, bool, error) {
		return "child result", false, nil
	})
	session, err := testApp.app.newInteractiveSession(nil)
	if err != nil {
		t.Fatal(err)
	}
	var systems []string
	testApp.app.sink.(*spySink).onSystem = func(message string) { systems = append(systems, message) }
	if stopped := testApp.app.handleInteractiveInput(context.Background(), session, "::agent"); stopped {
		t.Fatal("::agent unexpectedly stopped the session")
	}
	if len(systems) != 2 {
		t.Fatalf("::agent emitted %d system messages, want 2 (note + view): %#v", len(systems), systems)
	}
	if !strings.Contains(systems[0], "::agent is deprecated") {
		t.Fatalf("::agent deprecation note missing or out of order: %#v", systems)
	}
	if !strings.Contains(systems[1], "[::agents]") {
		t.Fatalf("::agent did not render the ::agents view: %#v", systems)
	}
}

// --- Ticket 05: ::agents question flags and preview integration ---

// TestAgentsFullFlagShowsCompleteQuestion verifies -f/--full bypasses
// truncation so the complete question (whitespace-collapsed) is shown.
func TestAgentsFullFlagShowsCompleteQuestion(t *testing.T) {
	testApp := newInteractiveTurnTestApp(t)
	testApp.app.subagents = newSubagentManager(defaultSubagentRuntimeConfig(), func(context.Context, *agentRuntime, *subagentSession) (string, bool, error) {
		return "child result", false, nil
	})
	session, err := testApp.app.newInteractiveSession(nil)
	if err != nil {
		t.Fatal(err)
	}
	longQuestion := strings.Repeat("alpha ", 80) + strings.Repeat("β", 40)
	if _, err := testApp.app.subagents.create(context.Background(), &agentRuntime{sessionID: rootAgentID, depth: 0, allowedTools: map[string]bool{toolCreateSubagent: true}}, createSubagentArgs{Question: longQuestion}); err != nil {
		t.Fatal(err)
	}
	var systems []string
	testApp.app.sink.(*spySink).onSystem = func(message string) { systems = append(systems, message) }
	if stopped := testApp.app.handleInteractiveInput(context.Background(), session, "::agents --full"); stopped {
		t.Fatal("::agents --full unexpectedly stopped the session")
	}
	line := systems[len(systems)-1]
	if strings.Contains(line, "… (") {
		t.Fatalf("::agents --full still truncated the question: %q", line)
	}
	if !strings.Contains(line, strings.Join(strings.Fields(longQuestion), " ")) {
		t.Fatalf("::agents --full omitted the full question: %q", line)
	}
}

// TestAgentsGraphemePreviewSuffix verifies the shared preview helper truncates
// long questions with a grapheme-aware suffix and never splits an emoji cluster.
func TestAgentsGraphemePreviewSuffix(t *testing.T) {
	testApp := newInteractiveTurnTestApp(t)
	testApp.app.subagents = newSubagentManager(defaultSubagentRuntimeConfig(), func(context.Context, *agentRuntime, *subagentSession) (string, bool, error) {
		return "child result", false, nil
	})
	session, err := testApp.app.newInteractiveSession(nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := testApp.app.subagents.create(context.Background(), &agentRuntime{sessionID: rootAgentID, depth: 0, allowedTools: map[string]bool{toolCreateSubagent: true}}, createSubagentArgs{Question: strings.Repeat("🍎", 300)}); err != nil {
		t.Fatal(err)
	}
	var systems []string
	testApp.app.sink.(*spySink).onSystem = func(message string) { systems = append(systems, message) }
	if stopped := testApp.app.handleInteractiveInput(context.Background(), session, "::agents"); stopped {
		t.Fatal("::agents unexpectedly stopped the session")
	}
	line := systems[len(systems)-1]
	if !strings.Contains(line, "… (+") || !strings.Contains(line, "more chars)") {
		t.Fatalf("::agents did not preview the long emoji question with the suffix: %q", line)
	}
	if !utf8.ValidString(line) {
		t.Fatalf("::agents rendered invalid UTF-8: %q", line)
	}
}

// TestAgentsTerminatorAcceptsDashLeadingQuestion verifies the "--" terminator
// and --model/--timeout flags are accepted without error and that ::agents
// still renders its view (status-only interpretation; no spawn).
func TestAgentsTerminatorAcceptsDashLeadingQuestion(t *testing.T) {
	testApp := newInteractiveTurnTestApp(t)
	testApp.app.subagents = newSubagentManager(defaultSubagentRuntimeConfig(), func(context.Context, *agentRuntime, *subagentSession) (string, bool, error) {
		return "child result", false, nil
	})
	session, err := testApp.app.newInteractiveSession(nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, input := range []string{"::agents -- -weird", "::agents --model x --timeout 5"} {
		var systems []string
		testApp.app.sink.(*spySink).onSystem = func(message string) { systems = append(systems, message) }
		if stopped := testApp.app.handleInteractiveInput(context.Background(), session, input); stopped {
			t.Fatalf("%q unexpectedly stopped the session", input)
		}
		if len(systems) == 0 || !strings.Contains(systems[len(systems)-1], "[::agents]") {
			t.Fatalf("%q did not render the ::agents view: %#v", input, systems)
		}
	}
}

// --- Ticket 01: live ::todos during active turns ---

// TestInteractiveTodosShowsLiveWorkerChecklistDuringBusyTurn is the primary
// ticket-01 regression: it blocks a worker after changing its runtime
// checklist, queries ::todos, and verifies the live view, unchanged parent
// persistence, and the committed result after the turn is released.
func TestInteractiveTodosShowsLiveWorkerChecklistDuringBusyTurn(t *testing.T) {
	testApp := newInteractiveTurnTestApp(t)
	session, err := testApp.app.newInteractiveSession([]contracts.Message{{Role: "system", Content: "test"}})
	if err != nil {
		t.Fatal(err)
	}
	session.todos = []todoItem{{ID: "1", Content: "committed work", Status: todoStatusPending}}
	syncRuntimeTodos(session)
	if err := testApp.app.saveInteractiveSession(session); err != nil {
		t.Fatal(err)
	}
	before, err := testApp.app.sessionStore.resolve(session.id)
	if err != nil {
		t.Fatal(err)
	}

	started := make(chan struct{})
	release := make(chan struct{})
	var controller *interactiveTurnController
	controller = newInteractiveTurnController(nil, nil, nil, func(outcome interactiveTurnOutcome) {
		testApp.app.finishInteractiveTurn(controller, session, outcome)
	}, nil)
	if !testApp.app.startInteractiveTurn(context.Background(), controller, session, func(_ context.Context, worker *interactiveSession) (bool, error) {
		worker.runtime.replaceTodos([]todoItem{
			{ID: "1", Content: "committed work", Status: todoStatusCompleted},
			{ID: "2", Content: "live worker item", Status: todoStatusInProgress},
		})
		close(started)
		<-release
		worker.todos = worker.runtime.snapshotTodos()
		return false, nil
	}) {
		t.Fatal("busy turn did not start")
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("busy turn did not reach the blocking worker")
	}

	var systems []string
	testApp.app.sink.(*spySink).onSystem = func(message string) { systems = append(systems, message) }
	if stopped := testApp.app.handleInteractiveInputAsync(context.Background(), controller, session, nil, nil, "::todos"); stopped {
		t.Fatal("busy ::todos unexpectedly ended the session")
	}
	if len(systems) != 1 {
		t.Fatalf("busy ::todos emitted %d system messages, want 1: %#v", len(systems), systems)
	}
	live := systems[0]
	for _, want := range []string{
		"[::todos]", "(live)",
		"- 1 [completed] committed work",
		"- 2 [in_progress] live worker item",
	} {
		if !strings.Contains(live, want) {
			t.Fatalf("live ::todos output missing %q: %q", want, live)
		}
	}
	// Order must follow the worker checklist, not ID order coincidence.
	if strings.Index(live, "live worker item") < strings.Index(live, "committed work") {
		t.Fatalf("live ::todos reordered the worker checklist: %q", live)
	}
	if got := testApp.userPrompts(); len(got) != 0 {
		t.Fatalf("busy ::todos reached the model: %#v", got)
	}
	// The busy query is read-only with respect to the parent session.
	if len(session.messages) != 1 || len(session.todos) != 1 || session.todos[0].Status != todoStatusPending {
		t.Fatalf("busy ::todos mutated the parent session: messages=%d todos=%#v", len(session.messages), session.todos)
	}
	during, err := testApp.app.sessionStore.resolve(session.id)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before.Todos, during.Todos) || !reflect.DeepEqual(before.Messages, during.Messages) {
		t.Fatalf("busy ::todos persisted parent state: before=%#v during=%#v", before, during)
	}
	if _, err := os.Stat(filepath.Join(testApp.workspaceRoot, interactiveResponseFile)); !os.IsNotExist(err) {
		t.Fatalf("busy ::todos wrote the response file: %v", err)
	}

	close(release)
	controller.wait()

	// After a successful turn the worker checklist is committed and the idle
	// view reports it without the live marker.
	systems = nil
	if stopped := testApp.app.handleInteractiveInputAsync(context.Background(), controller, session, nil, nil, "::todos"); stopped {
		t.Fatal("idle ::todos unexpectedly ended the session")
	}
	if len(systems) != 1 {
		t.Fatalf("idle ::todos emitted %d system messages, want 1: %#v", len(systems), systems)
	}
	committed := systems[0]
	if strings.Contains(committed, "(live)") {
		t.Fatalf("idle ::todos still reported a live view: %q", committed)
	}
	for _, want := range []string{
		"- 1 [completed] committed work",
		"- 2 [in_progress] live worker item",
	} {
		if !strings.Contains(committed, want) {
			t.Fatalf("committed ::todos output missing %q: %q", want, committed)
		}
	}
}

// TestInteractiveTodosDiscardsWorkerChecklistOnFailedTurn proves a failed turn
// leaves the prior committed checklist unchanged once the worker is discarded,
// while the live view during the turn still showed the worker's state.
func TestInteractiveTodosDiscardsWorkerChecklistOnFailedTurn(t *testing.T) {
	testApp := newInteractiveTurnTestApp(t)
	session, err := testApp.app.newInteractiveSession([]contracts.Message{{Role: "system", Content: "test"}})
	if err != nil {
		t.Fatal(err)
	}
	session.todos = []todoItem{{ID: "1", Content: "committed work", Status: todoStatusPending}}
	syncRuntimeTodos(session)
	if err := testApp.app.saveInteractiveSession(session); err != nil {
		t.Fatal(err)
	}

	started := make(chan struct{})
	release := make(chan struct{})
	var controller *interactiveTurnController
	controller = newInteractiveTurnController(nil, nil, nil, func(outcome interactiveTurnOutcome) {
		testApp.app.finishInteractiveTurn(controller, session, outcome)
	}, nil)
	if !testApp.app.startInteractiveTurn(context.Background(), controller, session, func(_ context.Context, worker *interactiveSession) (bool, error) {
		worker.runtime.replaceTodos([]todoItem{{ID: "9", Content: "doomed item", Status: todoStatusInProgress}})
		worker.todos = worker.runtime.snapshotTodos()
		close(started)
		<-release
		return false, errors.New("turn failed")
	}) {
		t.Fatal("failing turn did not start")
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("failing turn did not reach the blocking worker")
	}
	var systems []string
	testApp.app.sink.(*spySink).onSystem = func(message string) { systems = append(systems, message) }
	if stopped := testApp.app.handleInteractiveInputAsync(context.Background(), controller, session, nil, nil, "::todos"); stopped {
		t.Fatal("busy ::todos unexpectedly ended the session")
	}
	if len(systems) == 0 || !strings.Contains(systems[0], "doomed item") {
		t.Fatalf("live ::todos did not show the worker checklist: %#v", systems)
	}

	close(release)
	controller.wait()

	systems = nil
	if stopped := testApp.app.handleInteractiveInputAsync(context.Background(), controller, session, nil, nil, "::todos"); stopped {
		t.Fatal("idle ::todos unexpectedly ended the session")
	}
	if len(systems) != 1 {
		t.Fatalf("idle ::todos emitted %d system messages, want 1: %#v", len(systems), systems)
	}
	if strings.Contains(systems[0], "doomed item") {
		t.Fatalf("failed turn leaked discarded worker todos: %q", systems[0])
	}
	if !strings.Contains(systems[0], "- 1 [pending] committed work") {
		t.Fatalf("failed turn did not preserve the committed checklist: %q", systems[0])
	}
	if len(session.todos) != 1 || session.todos[0].ID != "1" {
		t.Fatalf("failed turn mutated the committed session checklist: %#v", session.todos)
	}
}

// TestInteractiveTodosDiscardsWorkerChecklistOnCancelledTurn covers the
// cancellation half of the transactional rule.
func TestInteractiveTodosDiscardsWorkerChecklistOnCancelledTurn(t *testing.T) {
	testApp := newInteractiveTurnTestApp(t)
	session, err := testApp.app.newInteractiveSession([]contracts.Message{{Role: "system", Content: "test"}})
	if err != nil {
		t.Fatal(err)
	}
	session.todos = []todoItem{{ID: "1", Content: "committed work", Status: todoStatusPending}}
	syncRuntimeTodos(session)

	started := make(chan struct{})
	var controller *interactiveTurnController
	controller = newInteractiveTurnController(nil, nil, nil, func(outcome interactiveTurnOutcome) {
		testApp.app.finishInteractiveTurn(controller, session, outcome)
	}, nil)
	if !testApp.app.startInteractiveTurn(context.Background(), controller, session, func(turnCtx context.Context, worker *interactiveSession) (bool, error) {
		worker.runtime.replaceTodos([]todoItem{{ID: "9", Content: "cancelled item", Status: todoStatusInProgress}})
		worker.todos = worker.runtime.snapshotTodos()
		close(started)
		<-turnCtx.Done()
		return false, turnCtx.Err()
	}) {
		t.Fatal("cancellable turn did not start")
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("cancellable turn did not reach the blocking worker")
	}
	var systems []string
	testApp.app.sink.(*spySink).onSystem = func(message string) { systems = append(systems, message) }
	if stopped := testApp.app.handleInteractiveInputAsync(context.Background(), controller, session, nil, nil, "::todos"); stopped {
		t.Fatal("busy ::todos unexpectedly ended the session")
	}
	if len(systems) == 0 || !strings.Contains(systems[0], "cancelled item") {
		t.Fatalf("live ::todos did not show the worker checklist: %#v", systems)
	}

	if !controller.cancelActive() {
		t.Fatal("cancelActive did not report an active turn")
	}
	controller.wait()

	systems = nil
	if stopped := testApp.app.handleInteractiveInputAsync(context.Background(), controller, session, nil, nil, "::todos"); stopped {
		t.Fatal("idle ::todos unexpectedly ended the session")
	}
	joined := strings.Join(systems, "\n")
	if strings.Contains(joined, "cancelled item") {
		t.Fatalf("cancelled turn leaked discarded worker todos: %q", joined)
	}
	if !strings.Contains(joined, "- 1 [pending] committed work") {
		t.Fatalf("cancelled turn did not preserve the committed checklist: %q", joined)
	}
}

// TestInteractiveTodosLiveEmptyChecklistIsDistinctFromCommittedEmpty proves the
// idle empty-list behavior is preserved while a busy turn with an empty worker
// checklist reports a distinct live result rather than the committed message.
func TestInteractiveTodosLiveEmptyChecklistIsDistinctFromCommittedEmpty(t *testing.T) {
	testApp := newInteractiveTurnTestApp(t)
	session, err := testApp.app.newInteractiveSession(nil)
	if err != nil {
		t.Fatal(err)
	}
	// Idle behavior is unchanged.
	var systems []string
	testApp.app.sink.(*spySink).onSystem = func(message string) { systems = append(systems, message) }
	if stopped := testApp.app.handleInteractiveInput(context.Background(), session, "::todos"); stopped {
		t.Fatal("idle ::todos unexpectedly stopped the session")
	}
	if len(systems) != 1 || !strings.Contains(systems[0], "no committed todos") {
		t.Fatalf("idle empty ::todos result missing: %#v", systems)
	}

	started := make(chan struct{})
	release := make(chan struct{})
	var controller *interactiveTurnController
	controller = newInteractiveTurnController(nil, nil, nil, func(outcome interactiveTurnOutcome) {
		testApp.app.finishInteractiveTurn(controller, session, outcome)
	}, nil)
	if !testApp.app.startInteractiveTurn(context.Background(), controller, session, func(_ context.Context, worker *interactiveSession) (bool, error) {
		worker.runtime.replaceTodos(nil)
		close(started)
		<-release
		return false, nil
	}) {
		t.Fatal("busy turn did not start")
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("busy turn did not reach the blocking worker")
	}
	systems = nil
	if stopped := testApp.app.handleInteractiveInputAsync(context.Background(), controller, session, nil, nil, "::todos"); stopped {
		t.Fatal("busy ::todos unexpectedly ended the session")
	}
	if len(systems) != 1 || !strings.Contains(systems[0], "no active todos") {
		t.Fatalf("live empty ::todos result missing: %#v", systems)
	}
	close(release)
	controller.wait()
}

// TestFormatInteractiveTodosFallsBackToCommittedSnapshot pins the direct
// formatter contract, including the nil-session preflight message and the rule
// that a nil worker never reads the parent runtime's mutable todos.
func TestFormatInteractiveTodosFallsBackToCommittedSnapshot(t *testing.T) {
	if got := formatInteractiveTodos(nil, nil); !strings.Contains(got, "no committed todo list is available") {
		t.Fatalf("nil session result = %q", got)
	}
	session := &interactiveSession{
		todos:   []todoItem{{ID: "1", Content: "committed", Status: todoStatusPending}},
		runtime: &agentRuntime{todos: []todoItem{{ID: "9", Content: "in flight", Status: todoStatusPending}}},
	}
	got := formatInteractiveTodos(session, nil)
	if !strings.Contains(got, "committed") || strings.Contains(got, "in flight") {
		t.Fatalf("idle formatter read mutable runtime todos: %q", got)
	}
	if strings.Contains(got, "(live)") {
		t.Fatalf("idle formatter claimed a live view: %q", got)
	}
	worker := &activeWorkerView{runtime: &agentRuntime{todos: []todoItem{{ID: "7", Content: "worker item", Status: todoStatusInProgress}}}}
	got = formatInteractiveTodos(session, worker)
	if !strings.Contains(got, "(live)") || !strings.Contains(got, "7 [in_progress] worker item") {
		t.Fatalf("live formatter output = %q", got)
	}
	if strings.Contains(got, "committed") {
		t.Fatalf("live formatter mixed in the committed snapshot: %q", got)
	}
}

// TestActiveWorkerPublicationIsClearedAfterTurn proves the controller only
// exposes a worker while a turn owns the session, which is what makes the idle
// fallback correct.
func TestActiveWorkerPublicationIsClearedAfterTurn(t *testing.T) {
	testApp := newInteractiveTurnTestApp(t)
	session, err := testApp.app.newInteractiveSession(nil)
	if err != nil {
		t.Fatal(err)
	}
	var controller *interactiveTurnController
	controller = newInteractiveTurnController(nil, nil, nil, func(outcome interactiveTurnOutcome) {
		testApp.app.finishInteractiveTurn(controller, session, outcome)
	}, nil)
	if controller.activeWorker() != nil {
		t.Fatal("controller published a worker before any turn started")
	}
	started := make(chan struct{})
	release := make(chan struct{})
	if !testApp.app.startInteractiveTurn(context.Background(), controller, session, func(_ context.Context, _ *interactiveSession) (bool, error) {
		close(started)
		<-release
		return false, nil
	}) {
		t.Fatal("turn did not start")
	}
	<-started
	if controller.activeWorker() == nil {
		t.Fatal("controller did not publish the active worker")
	}
	close(release)
	controller.wait()
	if controller.activeWorker() != nil {
		t.Fatal("controller did not clear the active worker after the turn")
	}
}

// TestLiveTodosAreRaceSafeUnderConcurrentWorkerUpdates drives concurrent
// checklist mutation and status inspection so `go test -race` can prove the
// live view is read through a synchronized snapshot.
func TestLiveTodosAreRaceSafeUnderConcurrentWorkerUpdates(t *testing.T) {
	testApp := newInteractiveTurnTestApp(t)
	session, err := testApp.app.newInteractiveSession(nil)
	if err != nil {
		t.Fatal(err)
	}
	var controller *interactiveTurnController
	controller = newInteractiveTurnController(nil, nil, nil, func(outcome interactiveTurnOutcome) {
		testApp.app.finishInteractiveTurn(controller, session, outcome)
	}, nil)
	started := make(chan struct{})
	stop := make(chan struct{})
	if !testApp.app.startInteractiveTurn(context.Background(), controller, session, func(_ context.Context, worker *interactiveSession) (bool, error) {
		close(started)
		for index := 0; ; index++ {
			select {
			case <-stop:
				return false, nil
			default:
			}
			worker.runtime.replaceTodos([]todoItem{{ID: fmt.Sprintf("%d", index), Content: "churn", Status: todoStatusInProgress}})
		}
	}) {
		t.Fatal("race turn did not start")
	}
	<-started
	testApp.app.sink.(*spySink).onSystem = func(string) {}
	for index := 0; index < 50; index++ {
		if stopped := testApp.app.handleInteractiveInputAsync(context.Background(), controller, session, nil, nil, "::todos"); stopped {
			t.Fatal("::todos unexpectedly ended the session")
		}
	}
	close(stop)
	controller.wait()
}

// --- Ticket 02: the ::session status command ---

// sessionUUIDPattern pins the rendered identifier to a complete RFC-4122 style
// UUID so a shortened or prefixed rendering cannot pass.
var sessionUUIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// TestInteractiveSessionShowsFullPersistedUUIDWhenIdle is the primary idle
// acceptance case: exactly one local status result carrying the session's full
// persisted UUID, with no model call and no state change.
func TestInteractiveSessionShowsFullPersistedUUIDWhenIdle(t *testing.T) {
	testApp := newInteractiveTurnTestApp(t)
	var systems []string
	testApp.app.sink.(*spySink).onSystem = func(message string) { systems = append(systems, message) }
	session, err := testApp.app.newInteractiveSession([]contracts.Message{{Role: "system", Content: "test"}})
	if err != nil {
		t.Fatal(err)
	}
	if stopped := testApp.app.handleInteractiveInput(context.Background(), session, "::session"); stopped {
		t.Fatal("::session unexpectedly stopped the session")
	}
	if got := testApp.userPrompts(); len(got) != 0 {
		t.Fatalf("::session reached the model: %#v", got)
	}
	if len(systems) != 1 {
		t.Fatalf("::session emitted %d system messages, want 1: %#v", len(systems), systems)
	}
	line := systems[0]
	if !strings.HasPrefix(line, "[::session] ") {
		t.Fatalf("::session output missing the status label: %q", line)
	}
	rendered := strings.TrimPrefix(line, "[::session] ")
	if rendered != session.id {
		t.Fatalf("::session rendered %q, want the full session id %q", rendered, session.id)
	}
	if !sessionUUIDPattern.MatchString(rendered) {
		t.Fatalf("::session did not render a full UUID: %q", rendered)
	}
	// The rendered value is the persisted identifier, not an in-memory alias.
	stored, err := testApp.app.sessionStore.resolve(session.id)
	if err != nil {
		t.Fatal(err)
	}
	if rendered != stored.SessionUUID {
		t.Fatalf("::session rendered %q, want persisted UUID %q", rendered, stored.SessionUUID)
	}
	// Read-only: no conversation, checklist, or response-file side effects.
	if len(session.messages) != 1 || len(session.todos) != 0 || session.lastResponse != "" {
		t.Fatalf("::session mutated session state: messages=%d todos=%#v response=%q", len(session.messages), session.todos, session.lastResponse)
	}
	if _, err := os.Stat(filepath.Join(testApp.workspaceRoot, interactiveResponseFile)); !os.IsNotExist(err) {
		t.Fatalf("::session wrote the response file: %v", err)
	}
}

// TestInteractiveSessionShowsSameUUIDDuringBusyTurn proves the command is
// answered locally while a turn owns the session: the same full UUID is
// emitted, the model is never invoked, and neither the in-memory session nor
// its persisted snapshot changes.
func TestInteractiveSessionShowsSameUUIDDuringBusyTurn(t *testing.T) {
	testApp := newInteractiveTurnTestApp(t)
	session, err := testApp.app.newInteractiveSession([]contracts.Message{{Role: "system", Content: "test"}})
	if err != nil {
		t.Fatal(err)
	}
	session.todos = []todoItem{{ID: "1", Content: "committed work", Status: todoStatusPending}}
	syncRuntimeTodos(session)
	session.lastResponse = "kept response"
	if err := testApp.app.saveInteractiveSession(session); err != nil {
		t.Fatal(err)
	}
	before, err := testApp.app.sessionStore.resolve(session.id)
	if err != nil {
		t.Fatal(err)
	}

	// Idle rendering first, so the busy rendering can be compared to it.
	var systems []string
	testApp.app.sink.(*spySink).onSystem = func(message string) { systems = append(systems, message) }
	if stopped := testApp.app.handleInteractiveInput(context.Background(), session, "::session"); stopped {
		t.Fatal("idle ::session unexpectedly stopped the session")
	}
	if len(systems) != 1 {
		t.Fatalf("idle ::session emitted %d system messages, want 1: %#v", len(systems), systems)
	}
	idle := systems[0]

	started := make(chan struct{})
	release := make(chan struct{})
	var controller *interactiveTurnController
	controller = newInteractiveTurnController(nil, nil, nil, func(outcome interactiveTurnOutcome) {
		testApp.app.finishInteractiveTurn(controller, session, outcome)
	}, nil)
	if !testApp.app.startInteractiveTurn(context.Background(), controller, session, func(_ context.Context, _ *interactiveSession) (bool, error) {
		close(started)
		<-release
		return false, nil
	}) {
		t.Fatal("busy turn did not start")
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("busy turn did not reach the blocking worker")
	}

	systems = nil
	if stopped := testApp.app.handleInteractiveInputAsync(context.Background(), controller, session, nil, nil, "::session"); stopped {
		t.Fatal("busy ::session unexpectedly ended the session")
	}
	if len(systems) != 1 {
		t.Fatalf("busy ::session emitted %d system messages, want 1: %#v", len(systems), systems)
	}
	busy := systems[0]
	if busy != idle {
		t.Fatalf("busy ::session output %q differs from idle output %q", busy, idle)
	}
	if !strings.Contains(busy, session.id) {
		t.Fatalf("busy ::session did not render the full session id %q: %q", session.id, busy)
	}
	if got := testApp.userPrompts(); len(got) != 0 {
		t.Fatalf("busy ::session reached the model: %#v", got)
	}
	if len(session.messages) != 1 || len(session.todos) != 1 || session.lastResponse != "kept response" {
		t.Fatalf("busy ::session mutated the parent session: messages=%d todos=%#v response=%q", len(session.messages), session.todos, session.lastResponse)
	}
	during, err := testApp.app.sessionStore.resolve(session.id)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before.Todos, during.Todos) || !reflect.DeepEqual(before.Messages, during.Messages) || before.LastContent != during.LastContent {
		t.Fatalf("busy ::session changed persisted session state: before=%#v during=%#v", before, during)
	}
	if _, err := os.Stat(filepath.Join(testApp.workspaceRoot, interactiveResponseFile)); !os.IsNotExist(err) {
		t.Fatalf("busy ::session wrote the response file: %v", err)
	}

	close(release)
	controller.wait()

	// The identity is stable after the turn completes.
	systems = nil
	if stopped := testApp.app.handleInteractiveInputAsync(context.Background(), controller, session, nil, nil, "::session"); stopped {
		t.Fatal("post-turn ::session unexpectedly ended the session")
	}
	if len(systems) != 1 || systems[0] != idle {
		t.Fatalf("post-turn ::session output changed: %#v, want %q", systems, idle)
	}
}

// TestInteractiveSessionRejectsArgumentsLocally proves extra arguments are
// consumed locally with the shared "accepts no arguments" phrasing and never
// become model input.
func TestInteractiveSessionRejectsArgumentsLocally(t *testing.T) {
	testApp := newInteractiveTurnTestApp(t)
	var systems []string
	testApp.app.sink.(*spySink).onSystem = func(message string) { systems = append(systems, message) }
	session, err := testApp.app.newInteractiveSession(nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, input := range []string{"::session extra", "::session  --full", "::session\tid"} {
		systems = nil
		if !testApp.app.handleStatusCommand(session, input, false, nil) {
			t.Fatalf("%q was not consumed locally", input)
		}
		if len(systems) != 1 || systems[0] != "[capelin-go] ::session accepts no arguments" {
			t.Fatalf("%q usage message mismatch: %#v", input, systems)
		}
		if strings.Contains(systems[0], session.id) {
			t.Fatalf("%q rendered the session id instead of a usage error: %q", input, systems[0])
		}
	}
	if got := testApp.userPrompts(); len(got) != 0 {
		t.Fatalf("::session with arguments reached the model: %#v", got)
	}
	// The same input through the interactive dispatcher stays local as well.
	systems = nil
	if stopped := testApp.app.handleInteractiveInput(context.Background(), session, "::session extra"); stopped {
		t.Fatal("::session extra unexpectedly stopped the session")
	}
	if len(systems) != 1 || !strings.Contains(systems[0], "accepts no arguments") {
		t.Fatalf("dispatcher ::session extra result: %#v", systems)
	}
	if got := testApp.userPrompts(); len(got) != 0 {
		t.Fatalf("dispatcher ::session extra reached the model: %#v", got)
	}
}

// TestInteractiveSessionWithoutAvailableSessionReportsLocally covers the nil
// and unusable-identifier cases: a clear local message and no panic.
func TestInteractiveSessionWithoutAvailableSessionReportsLocally(t *testing.T) {
	application := &app{cfg: config{}, sink: &spySink{}}
	var systems []string
	application.sink.(*spySink).onSystem = func(message string) { systems = append(systems, message) }

	for name, session := range map[string]*interactiveSession{
		"nil session":        nil,
		"empty id":           {id: ""},
		"whitespace-only id": {id: "  \t\n "},
	} {
		systems = nil
		if !application.handleStatusCommand(session, "::session", false, nil) {
			t.Fatalf("%s: ::session was not consumed locally", name)
		}
		if len(systems) != 1 {
			t.Fatalf("%s: ::session emitted %d system messages, want 1: %#v", name, len(systems), systems)
		}
		if systems[0] != "[::session] no active session is available" {
			t.Fatalf("%s: unavailable-session message mismatch: %q", name, systems[0])
		}
	}

	// A busy dispatch with no session must also stay local and panic-free.
	systems = nil
	if !application.handleStatusCommand(nil, "::session", true, &activeWorkerView{}) {
		t.Fatal("busy nil-session ::session was not consumed locally")
	}
	if len(systems) != 1 || !strings.Contains(systems[0], "no active session is available") {
		t.Fatalf("busy nil-session ::session result: %#v", systems)
	}
}

// TestFormatInteractiveSessionRendersFullIDOrUnavailable pins the renderer
// contract directly, including that the full identifier is never shortened.
func TestFormatInteractiveSessionRendersFullIDOrUnavailable(t *testing.T) {
	const id = "3f2b7c1d-9e4a-4a6b-8c2d-5f7e1a2b3c4d"
	if got := formatInteractiveSession(&interactiveSession{id: id}); got != "[::session] "+id {
		t.Fatalf("formatInteractiveSession(valid) = %q", got)
	}
	if got := formatInteractiveSession(nil); got != "[::session] no active session is available" {
		t.Fatalf("formatInteractiveSession(nil) = %q", got)
	}
	if got := formatInteractiveSession(&interactiveSession{}); got != "[::session] no active session is available" {
		t.Fatalf("formatInteractiveSession(empty id) = %q", got)
	}
	if got := formatInteractiveSession(&interactiveSession{id: "   "}); got != "[::session] no active session is available" {
		t.Fatalf("formatInteractiveSession(blank id) = %q", got)
	}
}

// TestStatusCommandCatalogContainsSessionAndStaysSorted proves the completion
// catalog advertises ::session, remains sorted, and still excludes the
// deprecated ::agent alias.
func TestStatusCommandCatalogContainsSessionAndStaysSorted(t *testing.T) {
	if !sort.StringsAreSorted(statusCommandNames) {
		t.Fatalf("statusCommandNames is not sorted: %#v", statusCommandNames)
	}
	seen := map[string]bool{}
	for _, name := range statusCommandNames {
		if seen[name] {
			t.Fatalf("statusCommandNames contains a duplicate %q: %#v", name, statusCommandNames)
		}
		seen[name] = true
	}
	for _, want := range []string{statusCommandTodos, statusCommandAgents, statusCommandStats, statusCommandSession} {
		if !seen[want] {
			t.Fatalf("statusCommandNames missing %q: %#v", want, statusCommandNames)
		}
	}
	if seen[statusCommandAgentDeprecated] {
		t.Fatalf("statusCommandNames advertises the deprecated alias: %#v", statusCommandNames)
	}
	if statusCommandSession != "::session" {
		t.Fatalf("statusCommandSession = %q, want ::session", statusCommandSession)
	}
}

// TestInteractiveSessionCompletionOffersTheCommand proves ::session is
// reachable through the interactive completer, including a unique-prefix
// completion, while the other status commands remain completable.
func TestInteractiveSessionCompletionOffersTheCommand(t *testing.T) {
	completer := interactiveCommandCompleter()
	candidates, offset := completer.Do([]rune("::"), 2)
	if offset != 2 {
		t.Fatalf("unexpected :: completion offset: got %d, want 2", offset)
	}
	got := runeStrings(candidates)
	want := []string{"agents ", "session ", "stats ", "todos "}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected :: candidates: got %#v, want %#v", got, want)
	}

	candidates, offset = completer.Do([]rune("::se"), 4)
	if offset != 4 || !reflect.DeepEqual(runeStrings(candidates), []string{"ssion "}) {
		t.Fatalf("unique ::session completion mismatch: candidates=%q offset=%d", candidates, offset)
	}

	// The shared prefix now resolves to both ::session and ::stats.
	candidates, offset = completer.Do([]rune("::s"), 3)
	if offset != 3 || !reflect.DeepEqual(runeStrings(candidates), []string{"ession ", "tats "}) {
		t.Fatalf("shared ::s completion mismatch: candidates=%q offset=%d", candidates, offset)
	}
}

// TestUnknownStatusCommandHelpListsSession proves the unknown-command help
// advertises ::session alongside every other supported status command and
// keeps the deprecated alias out of the list.
func TestUnknownStatusCommandHelpListsSession(t *testing.T) {
	testApp := newInteractiveTurnTestApp(t)
	var systems []string
	testApp.app.sink.(*spySink).onSystem = func(message string) { systems = append(systems, message) }
	session, err := testApp.app.newInteractiveSession(nil)
	if err != nil {
		t.Fatal(err)
	}
	if stopped := testApp.app.handleInteractiveInput(context.Background(), session, "::bogus"); stopped {
		t.Fatal("::bogus unexpectedly stopped the session")
	}
	if len(systems) != 1 {
		t.Fatalf("::bogus emitted %d system messages, want 1: %#v", len(systems), systems)
	}
	line := systems[0]
	if !strings.Contains(line, `unknown status command "::bogus"`) {
		t.Fatalf("unknown-command help lost its report: %q", line)
	}
	available := line[strings.Index(line, "available:"):]
	for _, want := range []string{"::todos", "::agents", "::stats", "::session"} {
		if !strings.Contains(available, want) {
			t.Fatalf("unknown-command help missing %q: %q", want, line)
		}
	}
	if strings.Contains(available, "::agent,") || strings.HasSuffix(available, "::agent") {
		t.Fatalf("unknown-command help advertises the deprecated alias: %q", line)
	}
	if got := testApp.userPrompts(); len(got) != 0 {
		t.Fatalf("::bogus reached the model: %#v", got)
	}
}

// TestExistingStatusCommandsRemainCompatibleWithSession is the compatibility
// regression for the addition: every pre-existing status command, including
// the deprecated ::agent alias and the argument rejections, keeps its exact
// behavior once ::session exists.
func TestExistingStatusCommandsRemainCompatibleWithSession(t *testing.T) {
	testApp := newInteractiveTurnTestApp(t)
	testApp.app.subagents = newSubagentManager(defaultSubagentRuntimeConfig(), func(context.Context, *agentRuntime, *subagentSession) (string, bool, error) {
		return "child result", false, nil
	})
	var systems []string
	testApp.app.sink.(*spySink).onSystem = func(message string) { systems = append(systems, message) }
	session, err := testApp.app.newInteractiveSession([]contracts.Message{{Role: "system", Content: "test"}})
	if err != nil {
		t.Fatal(err)
	}
	session.todos = []todoItem{{ID: "1", Content: "committed work", Status: todoStatusPending}}
	syncRuntimeTodos(session)

	for _, testCase := range []struct {
		input string
		want  []string
	}{
		{"::todos", []string{"[::todos]", "- 1 [pending] committed work"}},
		{"::agents", []string{"[::agents]", "root (coordinator, idle)", "aggregate: total=0;"}},
		{"::stats", []string{"[::stats]", "cwd:", "model: test-model"}},
		{"::todos extra", []string{"[capelin-go] ::todos accepts no arguments"}},
		{"::stats extra", []string{"[capelin-go] ::stats accepts no arguments"}},
	} {
		systems = nil
		if stopped := testApp.app.handleInteractiveInput(context.Background(), session, testCase.input); stopped {
			t.Fatalf("%q unexpectedly stopped the session", testCase.input)
		}
		if len(systems) != 1 {
			t.Fatalf("%q emitted %d system messages, want 1: %#v", testCase.input, len(systems), systems)
		}
		for _, want := range testCase.want {
			if !strings.Contains(systems[0], want) {
				t.Fatalf("%q output missing %q: %q", testCase.input, want, systems[0])
			}
		}
		if strings.Contains(systems[0], "[::session]") {
			t.Fatalf("%q leaked the ::session view: %q", testCase.input, systems[0])
		}
	}

	// The deprecated alias still emits its note before the ::agents view.
	systems = nil
	if stopped := testApp.app.handleInteractiveInput(context.Background(), session, "::agent"); stopped {
		t.Fatal("::agent unexpectedly stopped the session")
	}
	if len(systems) != 2 || systems[0] != statusAgentsDeprecationNote || !strings.Contains(systems[1], "[::agents]") {
		t.Fatalf("deprecated ::agent behavior changed: %#v", systems)
	}
	if got := testApp.userPrompts(); len(got) != 0 {
		t.Fatalf("existing status commands reached the model: %#v", got)
	}
}
