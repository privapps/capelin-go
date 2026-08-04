package app

import (
	"bytes"
	"capelin-go/internal/types"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/chzyer/readline"
)

func readAllInteractiveInput(t *testing.T, input string) string {
	t.Helper()
	reader := newBracketedPasteReader(strings.NewReader(input))
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read transformed input: %v", err)
	}
	return string(data)
}

type oneByteReader struct {
	reader *strings.Reader
}

func (r *oneByteReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	return r.reader.Read(p[:1])
}

func TestBracketedPasteReaderHandlesMarkersSplitAcrossReads(t *testing.T) {
	reader := newBracketedPasteReader(&oneByteReader{reader: strings.NewReader("\x1b[200~one\n\x1b[201~")})
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read fragmented transformed input: %v", err)
	}
	if got := normalizeInteractiveInput(strings.TrimSuffix(string(data), "\r")); got != "one" {
		t.Fatalf("unexpected fragmented paste prompt: %q", got)
	}
}

func TestBracketedPasteReaderSubmitsMultilinePasteOnce(t *testing.T) {
	transformed := readAllInteractiveInput(t, "\x1b[200~first\nsecond\r\nthird\x1b[201~")

	if got := bytes.Count([]byte(transformed), []byte{'\r'}); got != 1 {
		t.Fatalf("expected one paste submission, got %d in %q", got, transformed)
	}
	if strings.Contains(transformed, "\x1b[200~") || strings.Contains(transformed, "\x1b[201~") {
		t.Fatalf("bracketed-paste markers leaked into transformed input: %q", transformed)
	}

	submitted := strings.TrimSuffix(transformed, "\r")
	if got := normalizeInteractiveInput(submitted); got != "first\nsecond\nthird" {
		t.Fatalf("unexpected submitted prompt: %q", got)
	}
}

func TestBracketedPasteReaderSingleLinePasteAndTypedEnter(t *testing.T) {
	transformed := readAllInteractiveInput(t, "\x1b[200~single line\x1b[201~typed line\r")
	turns := strings.Split(strings.TrimSuffix(transformed, "\r"), "\r")
	if len(turns) != 2 {
		t.Fatalf("expected two input boundaries, got %d in %q", len(turns), transformed)
	}
	if got := normalizeInteractiveInput(turns[0]); got != "single line" {
		t.Fatalf("unexpected pasted prompt: %q", got)
	}
	if got := normalizeInteractiveInput(turns[1]); got != "typed line" {
		t.Fatalf("unexpected typed prompt: %q", got)
	}
}

func TestBracketedPasteReaderSubmitsConsecutivePastesSeparately(t *testing.T) {
	transformed := readAllInteractiveInput(t, "\x1b[200~first\nline\x1b[201~\x1b[200~second\x1b[201~")
	if got := bytes.Count([]byte(transformed), []byte{'\r'}); got != 2 {
		t.Fatalf("expected two paste submissions, got %d in %q", got, transformed)
	}
	turns := strings.Split(strings.TrimSuffix(transformed, "\r"), "\r")
	if got := normalizeInteractiveInput(turns[0]); got != "first\nline" {
		t.Fatalf("unexpected first prompt: %q", got)
	}
	if got := normalizeInteractiveInput(turns[1]); got != "second" {
		t.Fatalf("unexpected second prompt: %q", got)
	}
}

func TestBracketedPasteReaderPreservesOrdinaryInput(t *testing.T) {
	input := "ordinary line\rnext line\n"
	if got := readAllInteractiveInput(t, input); got != input {
		t.Fatalf("ordinary input changed: got %q, want %q", got, input)
	}
}

func TestBracketedPasteReaderNormalizesCRLFWithoutDuplicateBreaks(t *testing.T) {
	transformed := readAllInteractiveInput(t, "\x1b[200~one\r\ntwo\rthree\n\x1b[201~")
	submitted := strings.TrimSuffix(transformed, "\r")
	if got := normalizeInteractiveInput(submitted); got != "one\ntwo\nthree" {
		t.Fatalf("unexpected normalized CRLF prompt: %q", got)
	}
}

func TestBracketedPasteHistoryRecordIsSingleLineAndReversible(t *testing.T) {
	transformed := readAllInteractiveInput(t, "\x1b[200~line one\nline two\x1b[201~")
	record := strings.TrimSuffix(transformed, "\r")
	if strings.ContainsAny(record, "\r\n") {
		t.Fatalf("history record contains a physical line break: %q", record)
	}
	if strings.Contains(record, "\x1b[200~") || strings.Contains(record, "\x1b[201~") {
		t.Fatalf("history record contains bracketed-paste markers: %q", record)
	}
	if got := normalizeInteractiveInput(record); got != "line one\nline two" {
		t.Fatalf("history record did not round-trip: %q", got)
	}
}

func TestBracketedPasteReaderEscapesHistoryControlBytes(t *testing.T) {
	payload := string([]byte{'a', pasteEscape, pasteStart, pasteEnd, pasteLineBreak, 'b'})
	transformed := readAllInteractiveInput(t, "\x1b[200~"+payload+"\x1b[201~")
	record := strings.TrimSuffix(transformed, "\r")
	if got := normalizeInteractiveInput(record); got != payload {
		t.Fatalf("escaped paste payload did not round-trip: got %q, want %q", got, payload)
	}
}

func newInteractiveTestReadline(t *testing.T, input, historyFile string) *readline.Instance {
	rl, _ := newInteractiveTestReadlineWithOutput(t, input, historyFile)
	return rl
}

func newInteractiveTestReadlineWithOutput(t *testing.T, input, historyFile string) (*readline.Instance, *bytes.Buffer) {
	t.Helper()
	stdin := io.ReadCloser(newBracketedPasteReader(strings.NewReader(input)))
	stdin = newModifiedEnterReader(stdin)
	stdout := &bytes.Buffer{}
	rl, err := readline.NewEx(&readline.Config{
		Prompt:              "> ",
		HistoryFile:         historyFile,
		Stdin:               stdin,
		Stdout:              stdout,
		Stderr:              io.Discard,
		Painter:             interactiveNewlinePainter{},
		AutoComplete:        interactiveCommandCompleter(),
		Listener:            readline.FuncListener(newInteractiveNewlineListener(func() int { return 80 })),
		FuncFilterInputRune: func(r rune) (rune, bool) { return r, true },
		ForceUseInteractive: true,
		FuncIsTerminal:      func() bool { return true },
		FuncMakeRaw:         func() error { return nil },
		FuncExitRaw:         func() error { return nil },
		FuncGetWidth:        func() int { return 80 },
		FuncOnWidthChanged:  func(func()) {},
	})
	if err != nil {
		t.Fatalf("create test readline: %v", err)
	}
	t.Cleanup(func() { _ = rl.Close() })
	return rl, stdout
}

func interactiveInputFilter(r rune) (rune, bool) {
	return r, true
}

type interactiveTurnTestApp struct {
	app           *app
	workspaceRoot string
	responses     []string
	responseIndex int
	requests      []types.Request
	mu            sync.Mutex
}

func newInteractiveTurnTestApp(t *testing.T) *interactiveTurnTestApp {
	return newInteractiveTurnTestAppWithResponses(t, `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
}

func newInteractiveTurnTestAppWithResponses(t *testing.T, responses ...string) *interactiveTurnTestApp {
	t.Helper()
	testApp := &interactiveTurnTestApp{
		workspaceRoot: t.TempDir(),
		responses:     responses,
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request types.Request
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		testApp.mu.Lock()
		testApp.requests = append(testApp.requests, request)
		response := testApp.responses[len(testApp.responses)-1]
		if testApp.responseIndex < len(testApp.responses) {
			response = testApp.responses[testApp.responseIndex]
		}
		testApp.responseIndex++
		testApp.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, response)
	}))
	t.Cleanup(server.Close)
	testApp.app = &app{
		cfg: config{
			model:           "test-model",
			maxIterations:   1,
			workspaceRoot:   testApp.workspaceRoot,
			toolMaxParallel: 1,
			toolTimeoutSec:  1,
		},
		client: &client{
			endpoint: server.URL,
			model:    "test-model",
			http:     server.Client(),
		},
		sink: &spySink{},
	}
	return testApp
}

func (a *interactiveTurnTestApp) userPrompts() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	prompts := make([]string, 0, len(a.requests))
	for _, request := range a.requests {
		latest := ""
		for _, message := range request.Messages {
			if message.Role == "user" {
				latest = message.Content
			}
		}
		prompts = append(prompts, latest)
	}
	return prompts
}

func TestModifiedEnterReaderTranslatesCommonSequences(t *testing.T) {
	input := "before\x1b[13;2uafter\x1b[27;2;13~done"
	reader := newModifiedEnterReader(strings.NewReader(input))
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read modified Enter input: %v", err)
	}
	want := "before" + string(interactiveNewlineMarker) + "after" + string(interactiveNewlineMarker) + "done"
	if string(data) != want {
		t.Fatalf("translated modified Enter input = %q, want %q", data, want)
	}
}

func TestInteractiveNewlineListenerInsertsAtCursor(t *testing.T) {
	line, pos, ok := insertInteractiveNewline([]rune("ab\ue000CD"), 3, interactiveNewlineMarker)
	if !ok || string(line) != "ab\nCD" || pos != 3 {
		t.Fatalf("inserted newline = %q at %d, ok=%v", string(line), pos, ok)
	}
}

func TestInteractiveReadlineLoopTreatsCtrlJAsNewline(t *testing.T) {
	testApp := newInteractiveTurnTestApp(t)
	rl := newInteractiveTestReadline(t, "first\nsecond\r/exit\r", "")

	err := testApp.app.runInteractiveReadlineLoop(
		context.Background(),
		[]types.Message{{Role: "system", Content: "test"}},
		testApp.app.rootRuntime(),
		rl,
	)
	if err != nil {
		t.Fatalf("interactive Ctrl+J loop: %v", err)
	}
	if got := testApp.userPrompts(); len(got) != 1 || got[0] != "first\nsecond" {
		t.Fatalf("expected Ctrl+J to create one multiline turn, got %#v", got)
	}
}

func TestInteractiveReadlineLoopDoesNotRepeatEarlierLinesWhileEditing(t *testing.T) {
	testApp := newInteractiveTurnTestApp(t)
	rl, output := newInteractiveTestReadlineWithOutput(t, "tell me\nabout your\nself\r/exit\r", "")

	err := testApp.app.runInteractiveReadlineLoop(
		context.Background(),
		[]types.Message{{Role: "system", Content: "test"}},
		testApp.app.rootRuntime(),
		rl,
	)
	if err != nil {
		t.Fatalf("interactive multiline redraw loop: %v", err)
	}
	if got := testApp.userPrompts(); len(got) != 1 || got[0] != "tell me\nabout your\nself" {
		t.Fatalf("expected one exact multiline turn, got %#v", got)
	}
	// readline redraws the complete buffer on each keystroke. For embedded
	// newlines it must move back over all prior physical rows before doing so;
	// otherwise each redraw remains visible as a repeated prompt on screen.
	if got := bytes.Count(output.Bytes(), []byte("\x1b[A")); got < 2 {
		t.Fatalf("multiline redraw did not clear prior rows (%d cursor-up sequences); output=%q", got, output.Bytes())
	}
}

func TestInteractiveReadlineLoopTreatsModifiedEnterAsNewline(t *testing.T) {
	testApp := newInteractiveTurnTestApp(t)
	rl := newInteractiveTestReadline(t, "first\x1b[13;2usecond\r/exit\r", "")

	err := testApp.app.runInteractiveReadlineLoop(
		context.Background(),
		[]types.Message{{Role: "system", Content: "test"}},
		testApp.app.rootRuntime(),
		rl,
	)
	if err != nil {
		t.Fatalf("interactive modified Enter loop: %v", err)
	}
	if got := testApp.userPrompts(); len(got) != 1 || got[0] != "first\nsecond" {
		t.Fatalf("expected modified Enter to create one multiline turn, got %#v", got)
	}
}

func TestInteractiveReadlineLoopSubmitsOneMultilineTurn(t *testing.T) {
	testApp := newInteractiveTurnTestApp(t)
	rl := newInteractiveTestReadline(t, "\x1b[200~first\r\nsecond\nthird\x1b[201~/exit\r", "")

	err := testApp.app.runInteractiveReadlineLoop(
		context.Background(),
		[]types.Message{{Role: "system", Content: "test"}},
		testApp.app.rootRuntime(),
		rl,
	)
	if err != nil {
		t.Fatalf("interactive readline loop: %v", err)
	}
	if got := testApp.userPrompts(); len(got) != 1 || got[0] != "first\nsecond\nthird" {
		t.Fatalf("expected one exact multiline turn, got %#v", got)
	}
}

func TestInteractiveReadlineLoopKeepsOrdinaryEntersLineOriented(t *testing.T) {
	testApp := newInteractiveTurnTestApp(t)
	rl := newInteractiveTestReadline(t, "ordinary one\rordinary two\r/exit\r", "")

	err := testApp.app.runInteractiveReadlineLoop(
		context.Background(),
		[]types.Message{{Role: "system", Content: "test"}},
		testApp.app.rootRuntime(),
		rl,
	)
	if err != nil {
		t.Fatalf("interactive ordinary-input loop: %v", err)
	}
	got := testApp.userPrompts()
	if len(got) != 2 || got[0] != "ordinary one" || got[1] != "ordinary two" {
		t.Fatalf("expected two ordinary line turns, got %#v", got)
	}
}

func TestInteractiveReadlineLoopRecallsMultilineHistoryAsOneTurn(t *testing.T) {
	testApp := newInteractiveTurnTestApp(t)
	historyFile := filepath.Join(t.TempDir(), "history")
	input := "\x1b[200~line one\nline two\x1b[201~\x10\r"
	rl := newInteractiveTestReadline(t, input, historyFile)

	err := testApp.app.runInteractiveReadlineLoop(
		context.Background(),
		[]types.Message{{Role: "system", Content: "test"}},
		testApp.app.rootRuntime(),
		rl,
	)
	if err != nil {
		t.Fatalf("interactive history loop: %v", err)
	}
	if got := testApp.userPrompts(); len(got) != 2 || got[0] != "line one\nline two" || got[1] != got[0] {
		t.Fatalf("expected original and one recalled prompt, got %#v", got)
	}
	history, err := os.ReadFile(historyFile)
	if err != nil {
		t.Fatalf("read history: %v", err)
	}
	if strings.Count(string(history), "\n") != 1 || strings.Contains(string(history), "\x1b[200~") || strings.Contains(string(history), "\x1b[201~") {
		t.Fatalf("history was not one safe record: %q", history)
	}
}

func TestInteractiveCommandCompleterOffersAllCommands(t *testing.T) {
	completer := interactiveCommandCompleter()
	candidates, offset := completer.Do([]rune("/"), 1)
	if offset != 1 {
		t.Fatalf("unexpected completion offset: got %d, want 1", offset)
	}
	got := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		got = append(got, string(candidate))
	}
	want := []string{"exit ", "goal ", "quit ", "save ", "session-list ", "session-new ", "session-rename ", "session-resume "}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected slash-command candidates: got %#v, want %#v", got, want)
	}

	candidates, offset = completer.Do([]rune("/ex"), 3)
	if offset != 3 || len(candidates) != 1 || string(candidates[0]) != "it " {
		t.Fatalf("unique /exit completion mismatch: candidates=%q offset=%d", candidates, offset)
	}
}

func TestInteractiveSlashExitCommandsDoNotReachModel(t *testing.T) {
	testApp := newInteractiveTurnTestApp(t)
	rl := newInteractiveTestReadline(t, "  /exit  \r", "")

	if err := testApp.app.runInteractiveReadlineLoop(
		context.Background(),
		[]types.Message{{Role: "system", Content: "test"}},
		testApp.app.rootRuntime(),
		rl,
	); err != nil {
		t.Fatalf("interactive slash exit loop: %v", err)
	}
	if got := testApp.userPrompts(); len(got) != 0 {
		t.Fatalf("slash exit unexpectedly reached model: %#v", got)
	}
}

func TestInteractiveBareExitAndQuitRemainModelPrompts(t *testing.T) {
	testApp := newInteractiveTurnTestApp(t)
	rl := newInteractiveTestReadline(t, " exit \rquit\r/quit\r", "")

	if err := testApp.app.runInteractiveReadlineLoop(
		context.Background(),
		[]types.Message{{Role: "system", Content: "test"}},
		testApp.app.rootRuntime(),
		rl,
	); err != nil {
		t.Fatalf("interactive bare exit/quit loop: %v", err)
	}
	got := testApp.userPrompts()
	want := []string{"exit", "quit"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("bare exit/quit were not model prompts: got %#v, want %#v", got, want)
	}
}

func TestInteractiveSaveRequiresResponseAndContinues(t *testing.T) {
	testApp := newInteractiveTurnTestApp(t)
	rl := newInteractiveTestReadline(t, "/save\r/exit\r", "")

	if err := testApp.app.runInteractiveReadlineLoop(
		context.Background(),
		[]types.Message{{Role: "system", Content: "test"}},
		testApp.app.rootRuntime(),
		rl,
	); err != nil {
		t.Fatalf("interactive save-before-response loop: %v", err)
	}
	if _, err := os.Stat(filepath.Join(testApp.workspaceRoot, interactiveResponseFile)); !os.IsNotExist(err) {
		t.Fatalf("/save before a response created a file: err=%v", err)
	}
	if got := testApp.userPrompts(); len(got) != 0 {
		t.Fatalf("/save before a response reached model: %#v", got)
	}
}

func TestInteractiveSaveWritesAndOverwritesLatestResponse(t *testing.T) {
	testApp := newInteractiveTurnTestAppWithResponses(t,
		`{"choices":[{"message":{"role":"assistant","content":"first response"}}]}`,
		`{"choices":[{"message":{"role":"assistant","content":"second response"}}]}`,
	)
	rl := newInteractiveTestReadline(t, "first prompt\r/save\rsecond prompt\r/save\r/exit\r", "")

	if err := testApp.app.runInteractiveReadlineLoop(
		context.Background(),
		[]types.Message{{Role: "system", Content: "test"}},
		testApp.app.rootRuntime(),
		rl,
	); err != nil {
		t.Fatalf("interactive save loop: %v", err)
	}
	saved, err := os.ReadFile(filepath.Join(testApp.workspaceRoot, interactiveResponseFile))
	if err != nil {
		t.Fatalf("read saved response: %v", err)
	}
	if string(saved) != "second response" {
		t.Fatalf("saved response was not overwritten with latest text: %q", saved)
	}
	if got := testApp.userPrompts(); !reflect.DeepEqual(got, []string{"first prompt", "second prompt"}) {
		t.Fatalf("save commands reached model or changed prompt routing: %#v", got)
	}
}

func TestInteractiveFailedTurnPreservesResponseForSave(t *testing.T) {
	testApp := newInteractiveTurnTestAppWithResponses(t,
		`{"choices":[{"message":{"role":"assistant","content":"recoverable response"}}]}`,
		`{`,
	)
	rl := newInteractiveTestReadline(t, "successful prompt\rfailed prompt\r/save\r/exit\r", "")

	if err := testApp.app.runInteractiveReadlineLoop(
		context.Background(),
		[]types.Message{{Role: "system", Content: "test"}},
		testApp.app.rootRuntime(),
		rl,
	); err != nil {
		t.Fatalf("interactive failed-turn loop: %v", err)
	}
	saved, err := os.ReadFile(filepath.Join(testApp.workspaceRoot, interactiveResponseFile))
	if err != nil {
		t.Fatalf("read preserved response: %v", err)
	}
	if string(saved) != "recoverable response" {
		t.Fatalf("failed turn replaced saved response: %q", saved)
	}
}

func TestInteractiveSaveFileErrorDoesNotEndSession(t *testing.T) {
	testApp := newInteractiveTurnTestApp(t)
	session := &interactiveSession{
		messages: []types.Message{{Role: "system", Content: "test"}},
		runtime:  testApp.app.rootRuntime(),
	}
	if testApp.app.runInteractiveTurn(context.Background(), session, "first prompt") {
		t.Fatal("first turn unexpectedly stopped the session")
	}
	testApp.app.cfg.workspaceRoot = filepath.Join(testApp.workspaceRoot, "missing-parent")
	if testApp.app.handleInteractiveInput(context.Background(), session, "/save") {
		t.Fatal("save failure unexpectedly stopped the session")
	}
	if testApp.app.handleInteractiveInput(context.Background(), session, "second prompt") {
		t.Fatal("session did not continue after save failure")
	}
	if got := testApp.userPrompts(); !reflect.DeepEqual(got, []string{"first prompt", "second prompt"}) {
		t.Fatalf("save failure changed model routing: %#v", got)
	}
}

func TestInteractiveSaveUsesOnlyFinalAssistantText(t *testing.T) {
	testApp := newInteractiveTurnTestAppWithResponses(t,
		`{"choices":[{"message":{"role":"assistant","content":"intermediate text","tool_calls":[{"id":"call-1","type":"function","function":{"name":"list_files","arguments":"{\"path\":\".\"}"}}]}}]}`,
		`{"choices":[{"message":{"role":"assistant","content":"final response","reasoning":"private reasoning"}}]}`,
	)
	rl := newInteractiveTestReadline(t, "prompt with a tool\r/save\r/exit\r", "")

	if err := testApp.app.runInteractiveReadlineLoop(
		context.Background(),
		[]types.Message{{Role: "system", Content: "test"}},
		testApp.app.rootRuntime(),
		rl,
	); err != nil {
		t.Fatalf("interactive final-text loop: %v", err)
	}
	saved, err := os.ReadFile(filepath.Join(testApp.workspaceRoot, interactiveResponseFile))
	if err != nil {
		t.Fatalf("read final response: %v", err)
	}
	if string(saved) != "final response" {
		t.Fatalf("saved non-final assistant material: %q", saved)
	}
}

func TestInteractiveInitialTurnCanBeSaved(t *testing.T) {
	testApp := newInteractiveTurnTestApp(t)
	session := &interactiveSession{
		messages: []types.Message{{Role: "system", Content: "test"}},
		runtime:  testApp.app.rootRuntime(),
	}
	if testApp.app.runInteractiveTurn(context.Background(), session, "initial question") {
		t.Fatal("initial turn unexpectedly stopped the session")
	}
	if testApp.app.handleInteractiveInput(context.Background(), session, " /save ") {
		t.Fatal("save unexpectedly stopped the session")
	}
	saved, err := os.ReadFile(filepath.Join(testApp.workspaceRoot, interactiveResponseFile))
	if err != nil {
		t.Fatalf("read initial response: %v", err)
	}
	if string(saved) != "ok" {
		t.Fatalf("initial response was not saved: %q", saved)
	}
}
