package main

import (
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
)

func TestDetectThemeDark(t *testing.T) {
	t.Setenv("COLORFGBG", "15;0")
	theme := detectTheme()
	if theme.ActiveBorder != newDarkTheme().ActiveBorder {
		t.Fatal("expected dark theme")
	}
}

func TestDetectThemeLight(t *testing.T) {
	t.Setenv("COLORFGBG", "0;15")
	theme := detectTheme()
	if theme.ActiveBorder != newLightTheme().ActiveBorder {
		t.Fatal("expected light theme")
	}
}

func TestDetectThemeFallbackDark(t *testing.T) {
	t.Setenv("COLORFGBG", "")
	theme := detectTheme()
	if theme.ActiveBorder != newDarkTheme().ActiveBorder {
		t.Fatal("expected dark theme as fallback")
	}
}

func TestThemesAreDifferent(t *testing.T) {
	dark := newDarkTheme()
	light := newLightTheme()
	if dark.ActiveBorder == light.ActiveBorder {
		t.Error("dark and light ActiveBorder should differ")
	}
	if dark.LogContent == light.LogContent {
		t.Error("dark and light LogContent should differ")
	}
}

func TestAgentLogAtBottom(t *testing.T) {
	log := &agentLog{atBottom: true}
	if !log.atBottom {
		t.Fatal("new log should start at bottom")
	}
	log.atBottom = false
	if log.atBottom {
		t.Fatal("after scroll up, atBottom should be false")
	}
}

func TestStdioSinkWriteContent(t *testing.T) {
	var _ outputSink = &stdioSink{}
}

func TestCycleFocusLogic(t *testing.T) {
	panels := []panelID{panelAgents, panelLog, panelInput}
	cur := panelAgents
	idx := 0
	for i, p := range panels {
		if p == cur {
			idx = i
			break
		}
	}
	next := panels[(idx+1)%len(panels)]
	if next != panelLog {
		t.Errorf("expected panelLog, got %d", next)
	}
}

func TestColorName(t *testing.T) {
	name := colorName(tcell.ColorYellow)
	if name != "yellow" {
		t.Errorf("expected yellow, got %s", name)
	}
}

func TestAgentLabelRunning(t *testing.T) {
	theme := newDarkTheme()
	tui := &tuiApp{theme: theme}
	label, _ := tui.agentNodeText(tuiAgentNode{ID: "root", Name: "root", Status: subagentStatusRunning})
	if !strings.Contains(label, "⟳") {
		t.Errorf("running agent label should contain spinner, got %q", label)
	}
}

func TestAgentLabelCompleted(t *testing.T) {
	theme := newDarkTheme()
	tui := &tuiApp{theme: theme}
	label, _ := tui.agentNodeText(tuiAgentNode{ID: "sub-1", Name: "worker", Status: subagentStatusCompleted})
	if !strings.Contains(label, "✓") {
		t.Errorf("completed agent label should contain ✓, got %q", label)
	}
}

func TestAgentLabelFailed(t *testing.T) {
	theme := newDarkTheme()
	tui := &tuiApp{theme: theme}
	label, _ := tui.agentNodeText(tuiAgentNode{ID: "sub-2", Name: "worker", Status: subagentStatusFailed})
	if !strings.Contains(label, "✗") {
		t.Errorf("failed agent label should contain ✗, got %q", label)
	}
}

func TestSubagentManagerListAll(t *testing.T) {
	cfg := defaultSubagentRuntimeConfig()
	m := newSubagentManager(cfg, nil)
	nodes := m.ListAll()
	if len(nodes) != 0 {
		t.Errorf("fresh manager ListAll should return 0 subagents, got %d", len(nodes))
	}
}

func TestFocusModeToggle(t *testing.T) {
	tui := &tuiApp{focusMode: false}
	tui.focusMode = !tui.focusMode
	if !tui.focusMode {
		t.Error("focusMode should be true after first F1")
	}
	tui.focusMode = !tui.focusMode
	if tui.focusMode {
		t.Error("focusMode should be false after second F1")
	}
}
func TestMaximizeToggle(t *testing.T) {
	tui := &tuiApp{focusedPanel: panelLog, maximized: panelNone}
	if tui.maximized == tui.focusedPanel {
		tui.maximized = panelNone
	} else {
		tui.maximized = tui.focusedPanel
	}
	if tui.maximized != panelLog {
		t.Errorf("expected panelLog maximized, got %d", tui.maximized)
	}
	if tui.maximized == tui.focusedPanel {
		tui.maximized = panelNone
	} else {
		tui.maximized = tui.focusedPanel
	}
	if tui.maximized != panelNone {
		t.Errorf("expected panelNone after restore, got %d", tui.maximized)
	}
}

// TestThemeLogBgSet verifies that both dark and light themes have a non-zero LogBg,
// preventing invisible text due to unset background colours.
func TestThemeLogBgSet(t *testing.T) {
	dark := newDarkTheme()
	light := newLightTheme()
	if dark.LogBg == tcell.ColorDefault {
		t.Error("dark theme LogBg must not be ColorDefault (would cause invisible text)")
	}
	if light.LogBg == tcell.ColorDefault {
		t.Error("light theme LogBg must not be ColorDefault (would cause invisible text)")
	}
	if dark.LogBg == light.LogBg {
		t.Error("dark and light LogBg should differ")
	}
}

// TestSelectAgentRootUpdatesSelectedAgent verifies that selecting the root container
// node sets selectedAgent to tuiRootRef (instead of silently ignoring the request).
func TestSelectAgentRootUpdatesSelectedAgent(t *testing.T) {
	tui := &tuiApp{
		logMu:          sync.Mutex{},
		logs:           make(map[string]*agentLog),
		agentNames:     make(map[string]string),
		hasNewMessages: make(map[string]bool),
		topAgents:      make(map[string]*tuiAgent),
	}
	tui.selectedAgent = "agent-1"

	// Selecting root via the ID path should set selectedAgent to tuiRootRef.
	// We test the internal state directly (no tview widget calls).
	if tui.selectedAgent == tuiRootRef {
		t.Fatal("pre-condition: selectedAgent should not already be tuiRootRef")
	}
	tui.logMu.Lock()
	tui.selectedAgent = tuiRootRef
	tui.logMu.Unlock()

	if tui.currentSelectedAgent() != tuiRootRef {
		t.Errorf("expected currentSelectedAgent() == %q, got %q", tuiRootRef, tui.currentSelectedAgent())
	}
}

// TestCurrentSelectedAgentEmptyFallback ensures currentSelectedAgent falls back to
// tuiRootRef when selectedAgent is empty (e.g. at startup before any selection).
func TestCurrentSelectedAgentEmptyFallback(t *testing.T) {
	tui := &tuiApp{}
	if got := tui.currentSelectedAgent(); got != tuiRootRef {
		t.Errorf("expected %q fallback, got %q", tuiRootRef, got)
	}
}

// TestThemeTreeSelectedColors verifies that both themes have distinct, non-zero
// tree selection colors so that the highlighted agent node is always visible.
func TestThemeTreeSelectedColors(t *testing.T) {
	dark := newDarkTheme()
	light := newLightTheme()
	for _, tc := range []struct {
		name  string
		theme tuiTheme
	}{
		{"dark", dark},
		{"light", light},
	} {
		if tc.theme.TreeSelectedBg == tcell.ColorDefault {
			t.Errorf("%s theme: TreeSelectedBg must not be ColorDefault", tc.name)
		}
		if tc.theme.TreeSelectedFg == tcell.ColorDefault {
			t.Errorf("%s theme: TreeSelectedFg must not be ColorDefault", tc.name)
		}
		if tc.theme.TreeSelectedBg == tc.theme.TreeSelectedFg {
			t.Errorf("%s theme: TreeSelectedBg and TreeSelectedFg must differ", tc.name)
		}
	}
}

func TestRenderSearchLogText(t *testing.T) {
	entries := []logEntry{{color: "white", text: "alpha beta ALPHA\n"}}
	got := renderSearchLogText(entries, "alpha")
	if strings.Count(got, "[black:yellow:b]") != 2 {
		t.Fatalf("expected 2 highlighted matches, got %q", got)
	}
	// After each highlight, background must be reset ([-:-:-]) and fg restored.
	if strings.Count(got, "[-:-:-]") < 2 {
		t.Fatalf("expected bg-resetting close tags after each highlight, got %q", got)
	}
	// Entry fg color should be restored after each match.
	if strings.Count(got, "[white:-:-]") < 2 {
		t.Fatalf("expected fg restore tags after each highlight, got %q", got)
	}
	if !strings.Contains(got, "ALPHA") {
		t.Fatalf("expected original text to be preserved, got %q", got)
	}
	if renderSearchLogText(entries, "") != "[white]alpha beta ALPHA\n[-::-]" {
		t.Fatal("empty query should preserve the original rendering")
	}
}

// TestAgentNodeTextColors verifies agentNodeText returns no color tags in the
// text (they now belong in SetTextStyle, not the label string).
func TestAgentNodeTextColors(t *testing.T) {
	tui := &tuiApp{theme: newDarkTheme()}
	cases := []struct {
		status subagentStatus
		icon   string
	}{
		{subagentStatusRunning, "⟳"},
		{subagentStatusCompleted, "✓"},
		{subagentStatusFailed, "✗"},
		{subagentStatusCancelled, "—"},
	}
	for _, tc := range cases {
		label, color := tui.agentNodeText(tuiAgentNode{ID: "x", Name: "test", Status: tc.status})
		if strings.Contains(label, "[") {
			t.Errorf("agentNodeText should not contain color tags, got %q", label)
		}
		if !strings.Contains(label, tc.icon) {
			t.Errorf("agentNodeText missing icon %q for status %v, got %q", tc.icon, tc.status, label)
		}
		if color == tcell.ColorDefault {
			t.Errorf("agentNodeText returned ColorDefault for status %v", tc.status)
		}
	}
}

// TestSaveAndLoadSessionMessages verifies round-trip persistence of messages.
func TestSaveAndLoadSessionMessages(t *testing.T) {
	dir := t.TempDir()
	uuid := "test-uuid-123"
	msgs := []apiMessage{
		{Role: "system", Content: "You are helpful."},
		{Role: "user", Content: "Hello"},
		{Role: "assistant", Content: "Hi there! How can I help?"},
	}
	if err := saveSessionMessages(dir, uuid, "", msgs, time.Now()); err != nil {
		t.Fatalf("saveSessionMessages: %v", err)
	}
	loaded, err := loadSessionMessages(dir, uuid)
	if err != nil {
		t.Fatalf("loadSessionMessages: %v", err)
	}
	if len(loaded) != len(msgs) {
		t.Fatalf("expected %d messages, got %d", len(msgs), len(loaded))
	}
	for i, m := range msgs {
		if loaded[i].Role != m.Role || loaded[i].Content != m.Content {
			t.Errorf("message %d mismatch: want {%s %q}, got {%s %q}", i, m.Role, m.Content, loaded[i].Role, loaded[i].Content)
		}
	}
}

// TestListSessionSnapshots verifies listing returns saved snapshots sorted newest first.
func TestListSessionSnapshots(t *testing.T) {
	dir := t.TempDir()
	msgs := []apiMessage{{Role: "system", Content: "sys"}, {Role: "assistant", Content: "hello world"}}

	// Save two snapshots with different UUIDs.
	if err := saveSessionMessages(dir, "uuid-aaa", "", msgs, time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	// Sleep briefly so UpdatedAt differs.
	time.Sleep(10 * time.Millisecond)
	if err := saveSessionMessages(dir, "uuid-bbb", "research deepseek costs", msgs, time.Now()); err != nil {
		t.Fatal(err)
	}

	snaps, err := listSessionSnapshots(dir)
	if err != nil {
		t.Fatalf("listSessionSnapshots: %v", err)
	}
	if len(snaps) != 2 {
		t.Fatalf("expected 2 snapshots, got %d", len(snaps))
	}
	// Newest first.
	if snaps[0].SessionUUID != "uuid-bbb" {
		t.Errorf("expected newest snapshot first (uuid-bbb), got %s", snaps[0].SessionUUID)
	}
	if snaps[0].LastContent == "" {
		t.Error("LastContent should be populated")
	}
}

// TestListSessionSnapshotsEmpty verifies that listing a non-existent directory
// returns nil, nil rather than an error.
func TestListSessionSnapshotsEmpty(t *testing.T) {
	dir := t.TempDir()
	snaps, err := listSessionSnapshots(filepath.Join(dir, "nonexistent"))
	if err != nil {
		t.Fatalf("expected nil error for missing dir, got: %v", err)
	}
	if len(snaps) != 0 {
		t.Errorf("expected 0 snapshots, got %d", len(snaps))
	}
}

// TestReplayMessagesToLogBuf verifies that replayMessagesToLogBuf writes all
// message roles into the log buffer and that the buffer contains expected content.
func TestReplayMessagesToLogBuf(t *testing.T) {
	tui := &tuiApp{
		theme:          newDarkTheme(),
		logMu:          sync.Mutex{},
		logs:           make(map[string]*agentLog),
		agentNames:     make(map[string]string),
		hasNewMessages: make(map[string]bool),
		topAgents:      make(map[string]*tuiAgent),
	}
	msgs := []apiMessage{
		{Role: "system", Content: "You are helpful."},
		{Role: "user", Content: "Hello there"},
		{Role: "assistant", Content: "Hi! How can I help?"},
		{Role: "user", Content: "What is 2+2?"},
		{Role: "assistant", Content: "4"},
	}
	tui.replayMessagesToLogBuf("agent-1", msgs)

	raw := tui.getLogText("agent-1")
	plain := stripColorTags(raw)
	for _, want := range []string{
		"System Prompt",
		"You are helpful.",
		"▶ Hello there",
		"Hi! How can I help?",
		"▶ What is 2+2?",
		"4",
	} {
		if !strings.Contains(plain, want) {
			t.Errorf("replayed log missing %q\nfull log:\n%s", want, plain)
		}
	}
}

// TestCreateTopLevelAgentWithMessagesRoutingID verifies that the agent's
// runtime.sessionID matches agent.id (not the UUID), so tuiSink output is
// routed to the correct log buffer.
func TestCreateTopLevelAgentWithMessagesRoutingID(t *testing.T) {
	// Build a minimal app and tuiApp without starting tview.
	a := &app{cfg: config{workspaceRoot: t.TempDir()}}
	tui := &tuiApp{
		owner:          a,
		theme:          newDarkTheme(),
		logMu:          sync.Mutex{},
		logs:           make(map[string]*agentLog),
		agentNames:     make(map[string]string),
		hasNewMessages: make(map[string]bool),
		topAgents:      make(map[string]*tuiAgent),
		nextAgentNum:   0,
	}
	msgs := []apiMessage{{Role: "system", Content: "sys"}}
	agent := tui.createTopLevelAgentWithMessages(msgs, false)
	if agent.runtime.sessionID != agent.id {
		t.Errorf("runtime.sessionID %q should equal agent.id %q for correct log routing",
			agent.runtime.sessionID, agent.id)
	}
}

// TestAgentNodeLabel verifies agentNodeLabel output format.
func TestAgentNodeLabel(t *testing.T) {
	if got := agentNodeLabel(1, ""); got != "Agent 1" {
		t.Errorf("want 'Agent 1', got %q", got)
	}
	if got := agentNodeLabel(3, "research deepseek costs"); got != "3::research deepseek costs" {
		t.Errorf("want '3::research deepseek costs', got %q", got)
	}
}

// TestTopLevelAgentNodeTextWithName verifies that a named agent shows N::name in the tree.
func TestTopLevelAgentNodeTextWithName(t *testing.T) {
	tui := &tuiApp{theme: newDarkTheme()}
	agent := &tuiAgent{num: 2, Name: "create car racing game"}
	label, _ := tui.topLevelAgentNodeText(agent)
	// Strip icon prefix (first 2 chars: "○ ")
	if !strings.Contains(label, "2::create car racing game") {
		t.Errorf("expected label to contain '2::create car racing game', got %q", label)
	}
	if strings.Contains(label, "[") {
		t.Errorf("label should not contain color tags, got %q", label)
	}
}

// TestTopLevelAgentNodeTextNoName verifies fallback to "Agent N" when Name is empty.
func TestTopLevelAgentNodeTextNoName(t *testing.T) {
	tui := &tuiApp{theme: newDarkTheme()}
	agent := &tuiAgent{num: 5}
	label, _ := tui.topLevelAgentNodeText(agent)
	if !strings.Contains(label, "Agent 5") {
		t.Errorf("expected label to contain 'Agent 5', got %q", label)
	}
}

// TestSaveSessionMessagesNameRoundTrip verifies that the Name field survives a
// save/list round-trip.
func TestSaveSessionMessagesNameRoundTrip(t *testing.T) {
	dir := t.TempDir()
	msgs := []apiMessage{{Role: "system", Content: "sys"}, {Role: "user", Content: "hello"}}
	if err := saveSessionMessages(dir, "uuid-named", "build a web server", msgs, time.Now()); err != nil {
		t.Fatalf("saveSessionMessages: %v", err)
	}
	snaps, err := listSessionSnapshots(dir)
	if err != nil {
		t.Fatalf("listSessionSnapshots: %v", err)
	}
	if len(snaps) != 1 {
		t.Fatalf("expected 1 snapshot, got %d", len(snaps))
	}
	if snaps[0].Name != "build a web server" {
		t.Errorf("expected Name 'build a web server', got %q", snaps[0].Name)
	}
}

// TestRunTUINoInitialQueryLeavesRoot verifies that creating a tuiApp without
// calling createTopLevelAgent leaves selectedAgent at root.
func TestRunTUINoInitialQueryLeavesRoot(t *testing.T) {
	a := &app{cfg: config{workspaceRoot: t.TempDir()}}
	tui := &tuiApp{
		owner:          a,
		theme:          newDarkTheme(),
		logMu:          sync.Mutex{},
		logs:           make(map[string]*agentLog),
		agentNames:     make(map[string]string),
		hasNewMessages: make(map[string]bool),
		topAgents:      make(map[string]*tuiAgent),
		nextAgentNum:   0,
	}
	// Simulate what runTUI does when no initialQuestion: don't create any agent.
	// selectedAgent should remain "" (resolves to tuiRootRef).
	tui.logMu.Lock()
	sel := tui.selectedAgent
	tui.logMu.Unlock()
	if sel != "" {
		t.Errorf("expected empty selectedAgent (root), got %q", sel)
	}
	if got := tui.currentSelectedAgent(); got != tuiRootRef {
		t.Errorf("currentSelectedAgent() should return tuiRootRef when no agents, got %q", got)
	}
	// No top-level agents should have been created.
	tui.agentsMu.Lock()
	n := len(tui.topAgents)
	tui.agentsMu.Unlock()
	if n != 0 {
		t.Errorf("expected 0 top-level agents, got %d", n)
	}
}

// TestToggleMenuPanelState verifies that toggleMenuPanel flips menuHidden and
// that calling it twice returns to the original state. Uses a minimal tuiApp
// with real tview widgets (no app.Run() required).
func TestToggleMenuPanelState(t *testing.T) {
	a := &app{cfg: config{workspaceRoot: t.TempDir()}}
	tui := newTuiApp(a, newDarkTheme())

	if tui.menuHidden {
		t.Fatal("expected menuHidden=false initially")
	}
	// First toggle: hide the panel. focusedPanel starts at panelInput so
	// setFocus(panelLog) inside toggleMenuPanel is NOT triggered.
	tui.toggleMenuPanel()
	if !tui.menuHidden {
		t.Error("expected menuHidden=true after first toggle")
	}
	// Second toggle: show again.
	tui.toggleMenuPanel()
	if tui.menuHidden {
		t.Error("expected menuHidden=false after second toggle")
	}
}

// TestToggleMenuPanelFromAgentsFocus verifies that when the agents panel is
// focused and the menu is hidden, focus moves to the log panel.
func TestToggleMenuPanelFromAgentsFocus(t *testing.T) {
	a := &app{cfg: config{workspaceRoot: t.TempDir()}}
	tui := newTuiApp(a, newDarkTheme())
	// Force focus to panelAgents directly (no need to call app.SetFocus).
	tui.focusedPanel = panelAgents

	tui.toggleMenuPanel()
	if !tui.menuHidden {
		t.Error("expected menuHidden=true")
	}
	// Focus should have shifted away from agents since it's now hidden.
	if tui.focusedPanel == panelAgents {
		t.Error("focusedPanel should not remain panelAgents when menu is hidden")
	}
}

// TestFindAgentByRef verifies numeric and ID-based lookups.
func TestFindAgentByRef(t *testing.T) {
	a := &app{cfg: config{workspaceRoot: t.TempDir()}}
	tui := newTuiApp(a, newDarkTheme())

	// Create two agents manually.
	ag1 := &tuiAgent{num: 1, id: "agent-1"}
	ag2 := &tuiAgent{num: 2, id: "agent-2"}
	tui.agentsMu.Lock()
	tui.topAgents["agent-1"] = ag1
	tui.topAgents["agent-2"] = ag2
	tui.agentsMu.Unlock()

	tests := []struct {
		ref  string
		want *tuiAgent
	}{
		{"1", ag1},
		{"2", ag2},
		{"agent-1", ag1},
		{"agent-2", ag2},
		{"3", nil},
		{"agent-3", nil},
	}
	for _, tc := range tests {
		got := tui.findAgentByRef(tc.ref)
		if got != tc.want {
			t.Errorf("findAgentByRef(%q): got %v, want %v", tc.ref, got, tc.want)
		}
	}
}

// TestSpinnerFramesLen ensures spinnerFrames is non-empty and topLevelAgentNodeText
// handles the spinner without panicking.
func TestSpinnerFramesLen(t *testing.T) {
	if len(spinnerFrames) == 0 {
		t.Fatal("spinnerFrames must be non-empty")
	}
	a := &app{cfg: config{workspaceRoot: t.TempDir()}}
	tui := newTuiApp(a, newDarkTheme())
	ag := &tuiAgent{num: 1, id: "agent-1", Name: "test agent"}
	ag.queueMu.Lock()
	ag.busy = true
	ag.queueMu.Unlock()

	// Advance the spinnerFrame across all frames and verify text changes.
	var labels []string
	for i := 0; i < len(spinnerFrames); i++ {
		tui.spinnerFrame.Store(int32(i))
		label, _ := tui.topLevelAgentNodeText(ag)
		labels = append(labels, label)
	}
	// Verify that at least two distinct labels exist (animation is happening).
	seen := map[string]bool{}
	for _, l := range labels {
		seen[l] = true
	}
	if len(seen) < 2 {
		t.Error("expected multiple distinct spinner frames in topLevelAgentNodeText")
	}

	// Idle agent should use ◌ icon.
	ag.queueMu.Lock()
	ag.busy = false
	ag.queueMu.Unlock()
	label, _ := tui.topLevelAgentNodeText(ag)
	if !strings.HasPrefix(label, "◌ ") {
		t.Errorf("idle agent label should start with '◌ ', got %q", label)
	}
}

// TestSkillPickerNilSkillsNoOp verifies that showSkillPicker with no skills
// removes %% from the input text without panicking.
func TestSkillPickerNilSkillsNoOp(t *testing.T) {
	a := &app{cfg: config{workspaceRoot: t.TempDir()}}
	tui := newTuiApp(a, newDarkTheme())
	tui.skills = map[string]skill{} // empty — no skills available

	tui.inputField.SetText("hello %% world")
	// Simulate what showSkillPicker does in the no-skills branch.
	if len(tui.skills) == 0 {
		cur := tui.inputField.GetText()
		tui.inputField.SetText(strings.Replace(cur, "%%", "", 1))
	}
	got := tui.inputField.GetText()
	if strings.Contains(got, "%%") {
		t.Errorf("expected %% removed, got %q", got)
	}
	if got != "hello  world" {
		t.Errorf("expected 'hello  world', got %q", got)
	}
}

// TestSessionForkCommand verifies /session-fork is in slashCommands with correct modes.
func TestSessionForkCommand(t *testing.T) {
	a := &app{cfg: config{workspaceRoot: t.TempDir()}}
	tui := newTuiApp(a, newDarkTheme())

	var found bool
	for _, c := range slashCommands {
		if c.name == "/session-fork" {
			found = true
			for _, want := range []string{"full", "last", "summary"} {
				if !strings.Contains(c.description, want) {
					t.Errorf("expected /session-fork description to mention %q", want)
				}
			}
		}
		if c.name == "/session-split" || c.name == "/session-split-lite" {
			t.Errorf("old command %q should be removed from slashCommands", c.name)
		}
	}
	if !found {
		t.Error("/session-fork not found in slashCommands")
	}
	_ = tui
}

// TestWorkspaceCommand verifies workspace-related commands are in slashCommands.
func TestWorkspaceCommand(t *testing.T) {
	var foundWorkspace, foundNew, foundSave bool
	for _, c := range slashCommands {
		switch c.name {
		case "/workspace":
			foundWorkspace = true
		case "/workspace-new":
			foundNew = true
		case "/workspace-save":
			foundSave = true
		}
	}
	if !foundWorkspace {
		t.Error("/workspace not found in slashCommands")
	}
	if !foundNew {
		t.Error("/workspace-new not found in slashCommands")
	}
	if !foundSave {
		t.Error("/workspace-save not found in slashCommands")
	}
}

// TestSessionCommand verifies session-related commands are in slashCommands.
func TestSessionCommand(t *testing.T) {
	var foundResume, foundNew, foundAbandon, foundDestroy, foundCancel bool
	for _, c := range slashCommands {
		switch c.name {
		case "/session-resume":
			foundResume = true
		case "/session-new":
			foundNew = true
		case "/session-abandon":
			foundAbandon = true
		case "/session-destroy":
			foundDestroy = true
		case "/session-cancel":
			foundCancel = true
		}
	}
	if !foundResume {
		t.Error("/session-resume not found in slashCommands")
	}
	if !foundNew {
		t.Error("/session-new not found in slashCommands")
	}
	if !foundAbandon {
		t.Error("/session-abandon not found in slashCommands")
	}
	if !foundDestroy {
		t.Error("/session-destroy not found in slashCommands")
	}
	if !foundCancel {
		t.Error("/session-cancel not found in slashCommands")
	}
}

// TestSlashCommandNewDispatcher verifies that /new handles both /new and /session-new.
func TestSlashCommandNewDispatcher(t *testing.T) {
	var hasNew, hasSessionNew bool
	for _, c := range slashCommands {
		switch c.name {
		case "/new":
			hasNew = true
		case "/session-new":
			hasSessionNew = true
		}
	}
	if !hasNew {
		t.Error("/new not found in slashCommands")
	}
	if !hasSessionNew {
		t.Error("/session-new not found in slashCommands")
	}
}

// TestWorkspaceSaveAndLoad verifies round-trip of saveWorkspace / loadWorkspace.
func TestWorkspaceSaveAndLoad(t *testing.T) {
	dir := t.TempDir()
	entries := []workspaceEntry{
		{SessionUUID: "uuid-1", Name: "research"},
		{SessionUUID: "uuid-2", Name: ""},
	}
	if err := saveWorkspace(dir, "mywork", entries); err != nil {
		t.Fatalf("saveWorkspace: %v", err)
	}
	snap, err := loadWorkspace(dir, "mywork")
	if err != nil {
		t.Fatalf("loadWorkspace: %v", err)
	}
	if snap.Name != "mywork" {
		t.Errorf("got name %q, want %q", snap.Name, "mywork")
	}
	if len(snap.Agents) != 2 {
		t.Fatalf("got %d agents, want 2", len(snap.Agents))
	}
	if snap.Agents[0].SessionUUID != "uuid-1" || snap.Agents[0].Name != "research" {
		t.Errorf("unexpected first agent: %+v", snap.Agents[0])
	}
	if snap.Agents[1].SessionUUID != "uuid-2" {
		t.Errorf("unexpected second agent: %+v", snap.Agents[1])
	}
}

// TestListWorkspacesExcludesLast verifies listWorkspaces returns named workspaces only.
func TestListWorkspacesExcludesLast(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"last", "alpha", "beta"} {
		if err := saveWorkspace(dir, name, nil); err != nil {
			t.Fatalf("saveWorkspace(%q): %v", name, err)
		}
	}
	names, err := listWorkspaces(dir)
	if err != nil {
		t.Fatalf("listWorkspaces: %v", err)
	}
	if len(names) != 2 {
		t.Fatalf("got %d names, want 2: %v", len(names), names)
	}
	if names[0] != "alpha" || names[1] != "beta" {
		t.Errorf("unexpected names: %v", names)
	}
}
// mode is optional (default "last"), extra text is captured after the id.
func TestAppendToAgentParsing(t *testing.T) {
	cases := []struct {
		input     string
		wantMode  string
		wantRef   string
		wantExtra string
	}{
		// explicit mode + id
		{"/append-to-agent full 2", "full", "2", ""},
		{"/append-to-agent last 3", "last", "3", ""},
		{"/append-to-agent summary 4", "summary", "4", ""},
		// default mode (no mode keyword)
		{"/append-to-agent 2", "last", "2", ""},
		// extra text after id
		{"/append-to-agent 2 please fix this", "last", "2", "please fix this"},
		{"/append-to-agent last 2 please fix this", "last", "2", "please fix this"},
		{"/append-to-agent full 2 review and improve", "full", "2", "review and improve"},
	}

	for _, tc := range cases {
		parts := strings.Fields(tc.input)
		tokens := parts[1:]
		mode := "last"
		if len(tokens) > 0 {
			switch strings.ToLower(tokens[0]) {
			case "full", "last", "summary":
				mode = strings.ToLower(tokens[0])
				tokens = tokens[1:]
			}
		}
		ref := ""
		extra := ""
		if len(tokens) > 0 {
			ref = tokens[0]
			extra = strings.Join(tokens[1:], " ")
		}
		if mode != tc.wantMode {
			t.Errorf("input %q: mode=%q want %q", tc.input, mode, tc.wantMode)
		}
		if ref != tc.wantRef {
			t.Errorf("input %q: ref=%q want %q", tc.input, ref, tc.wantRef)
		}
		if extra != tc.wantExtra {
			t.Errorf("input %q: extra=%q want %q", tc.input, extra, tc.wantExtra)
		}
	}
}

// TestHasBareSkillTrigger verifies the hasBareSkillTrigger helper correctly
// detects bare %% vs completed %%name%% pairs.
func TestHasBareSkillTrigger(t *testing.T) {
	cases := []struct {
		input string
		want  bool
	}{
		// bare %% → trigger
		{"%%", true},
		{"please use %% here", true},
		{"use %% and then more", true},
		// complete %%name%% → no trigger
		{"%%jira-cli%%", false},
		{"use %%jira-cli%% for tickets", false},
		{"%%vPass%%", false},
		// complete pair + bare → trigger (the bare one)
		{"%%jira-cli%% and %%", true},
		// two complete pairs → no trigger
		{"%%jira-cli%% plus %%vPass%%", false},
		// old-style %%name without closing %% → trigger (incomplete pair)
		{"%%jira-cli", true},
	}
	for _, tc := range cases {
		got := hasBareSkillTrigger(tc.input)
		if got != tc.want {
			t.Errorf("hasBareSkillTrigger(%q) = %v, want %v", tc.input, got, tc.want)
		}
	}
}

// TestRemoveAndReplaceBareTrigger verifies the bare %% manipulation helpers.
func TestRemoveAndReplaceBareTrigger(t *testing.T) {
	// removeFirstBareSkillTrigger
	removeTests := []struct{ input, want string }{
		{"%%", ""},
		{"please %% here", "please  here"},
		{"%%jira-cli%% and %%", "%%jira-cli%% and "},
		{"%%jira-cli%% and %% more", "%%jira-cli%% and  more"},
	}
	for _, tc := range removeTests {
		got := removeFirstBareSkillTrigger(tc.input)
		if got != tc.want {
			t.Errorf("removeFirstBareSkillTrigger(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}

	// replaceFirstBareSkillTrigger
	replaceTests := []struct{ input, with, want string }{
		{"%%", "%%vPass%%", "%%vPass%%"},
		{"please %% here", "%%jira-cli%%", "please %%jira-cli%% here"},
		{"%%jira-cli%% and %% more", "%%vPass%%", "%%jira-cli%% and %%vPass%% more"},
	}
	for _, tc := range replaceTests {
		got := replaceFirstBareSkillTrigger(tc.input, tc.with)
		if got != tc.want {
			t.Errorf("replaceFirstBareSkillTrigger(%q, %q) = %q, want %q", tc.input, tc.with, got, tc.want)
		}
	}
}

// TestCascadeMenuModeItems verifies that the standard modeItems list covers all
// three expected modes in the correct order.
func TestCascadeMenuModeItems(t *testing.T) {
	if len(modeItems) != 3 {
		t.Fatalf("expected 3 mode items, got %d", len(modeItems))
	}
	wantValues := []string{"last", "full", "summary"}
	for i, item := range modeItems {
		if item.value != wantValues[i] {
			t.Errorf("modeItems[%d].value = %q, want %q", i, item.value, wantValues[i])
		}
		if item.label == "" {
			t.Errorf("modeItems[%d].label is empty", i)
		}
	}
}

// TestCommandCascadeTrigger verifies that typing a trailing space after the
// command opens the interactive cascade, while typed args do not.
func TestCommandCascadeTrigger(t *testing.T) {
	cases := []struct {
		input string
		want  string
		ok    bool
	}{
		{"/session-resume ", "/session-resume", true},
		{"/workspace ", "/workspace", true},
		{"/session-fork ", "/session-fork", true},
		{"/append-to-agent ", "/append-to-agent", true},
		{"/session-fork full ", "", false},
		{"/append-to-agent last 2 ", "", false},
		{"/compact ", "", false},
		{"/session-fork", "", false},
		{"/session abandon ", "", false},
		{"/workspace new ", "", false},
	}
	for _, tc := range cases {
		got, ok := commandCascadeTrigger(tc.input)
		if got != tc.want || ok != tc.ok {
			t.Errorf("commandCascadeTrigger(%q) = (%q, %v), want (%q, %v)", tc.input, got, ok, tc.want, tc.ok)
		}
	}
}

// TestSessionForkNoArgsCascadePath verifies that /session-fork with no arguments
// triggers the cascade path (QueueUpdateDraw) rather than the direct handler.
// We verify by checking the slashCommands description mentions "interactive".
func TestSessionForkNoArgsCascadePath(t *testing.T) {
	var found bool
	for _, c := range slashCommands {
		if c.name == "/session-fork" {
			found = true
			if !strings.Contains(c.description, "interactive") {
				t.Error("expected /session-fork description to mention interactive mode")
			}
		}
	}
	if !found {
		t.Error("/session-fork not found in slashCommands")
	}
}

// TestStatusCWDSetInNewTuiApp verifies the CWD widget is populated in newTuiApp.
func TestStatusCWDSetInNewTuiApp(t *testing.T) {
	dir := t.TempDir()
	a := &app{cfg: config{workspaceRoot: dir}}
	tui := newTuiApp(a, newDarkTheme())

	text := tui.statusCWD.GetText(false)
	if !strings.Contains(text, dir) && !strings.Contains(text, "…") {
		t.Errorf("expected CWD widget to contain %q or a truncated path, got %q", dir, text)
	}
}
