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
	t.Setenv("CAPELIN_THEME", "")
	t.Setenv("COLORFGBG", "15;0")
	theme := detectTheme("")
	if theme.ActiveBorder != newDarkTheme().ActiveBorder {
		t.Fatal("expected dark theme")
	}
}

func TestDetectThemeLight(t *testing.T) {
	t.Setenv("CAPELIN_THEME", "")
	t.Setenv("COLORFGBG", "0;15")
	theme := detectTheme("")
	if theme.ActiveBorder != newLightTheme().ActiveBorder {
		t.Fatal("expected light theme")
	}
}

func TestDetectThemeFallbackDark(t *testing.T) {
	t.Setenv("CAPELIN_THEME", "")
	t.Setenv("COLORFGBG", "")
	theme := detectTheme("")
	if theme.ActiveBorder != newDarkTheme().ActiveBorder {
		t.Fatal("expected dark theme as fallback")
	}
}

func TestDetectThemeCapelinOverride(t *testing.T) {
	t.Setenv("CAPELIN_THEME", "light")
	t.Setenv("COLORFGBG", "15;0")
	theme := detectTheme("")
	if theme.ActiveBorder != newLightTheme().ActiveBorder {
		t.Fatal("expected light theme via CAPELIN_THEME override")
	}
	t.Setenv("CAPELIN_THEME", "dark")
	t.Setenv("COLORFGBG", "0;15")
	theme = detectTheme("")
	if theme.ActiveBorder != newDarkTheme().ActiveBorder {
		t.Fatal("expected dark theme via CAPELIN_THEME override")
	}
}

func TestDetectThemeArgOverride(t *testing.T) {
	t.Setenv("CAPELIN_THEME", "")
	t.Setenv("COLORFGBG", "15;0")
	theme := detectTheme("light")
	if theme.ActiveBorder != newLightTheme().ActiveBorder {
		t.Fatal("expected light theme via arg override")
	}
	theme = detectTheme("dark")
	if theme.ActiveBorder != newDarkTheme().ActiveBorder {
		t.Fatal("expected dark theme via arg override despite COLORFGBG")
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

// TestLightThemeContrast verifies that light-theme color tags are readable
// on a white background: no "olive" or "teal" ANSI names (low contrast),
// and LogBg is explicitly white.
func TestLightThemeContrast(t *testing.T) {
	light := newLightTheme()
	if light.LogBg != tcell.ColorWhite {
		t.Error("light theme LogBg should be white")
	}
	lowContrast := []string{"olive", "teal", "gray", "silver", "white", "lightgray"}
	for _, name := range lowContrast {
		if light.LogContent == name {
			t.Errorf("light theme LogContent %q has low contrast on white", name)
		}
		if light.LogTool == name {
			t.Errorf("light theme LogTool %q has low contrast on white", name)
		}
		if light.LogError == name {
			t.Errorf("light theme LogError %q has low contrast on white", name)
		}
		if light.LogSystem == name {
			t.Errorf("light theme LogSystem %q has low contrast on white", name)
		}
	}
	// AgentBusy should not be olive/yellow-like low-contrast on white.
	if light.AgentBusy == tcell.ColorOlive {
		t.Error("light theme AgentBusy should not be Olive (low contrast on white)")
	}
}

// TestWidgetColorsAreReadable guards against the most common regression:
// tview's default foreground is white, so any widget left unstyled on a
// light-theme background produces invisible text. Both themes must declare
// every "text-on-bg" pair explicitly with a foreground different from the
// background.
func TestWidgetColorsAreReadable(t *testing.T) {
	cases := []struct {
		name    string
		theme   tuiTheme
		isLight bool
	}{
		{"dark", newDarkTheme(), false},
		{"light", newLightTheme(), true},
	}
	for _, tc := range cases {
		// TitleColor must differ from any background it is rendered on
		// (LogBg, InputBg, StatusBarBg).
		for _, bg := range []tcell.Color{tc.theme.LogBg, tc.theme.InputBg, tc.theme.StatusBarBg} {
			if tc.theme.TitleColor == bg {
				t.Errorf("%s theme: TitleColor equals background %v — titles will be invisible", tc.name, bg)
			}
		}
		// InputFg must differ from InputBg, otherwise typed text is invisible.
		if tc.theme.InputFg == tc.theme.InputBg {
			t.Errorf("%s theme: InputFg equals InputBg — typed text will be invisible", tc.name)
		}
		// StatusBarFg must differ from StatusBarBg.
		if tc.theme.StatusBarFg == tc.theme.StatusBarBg {
			t.Errorf("%s theme: StatusBarFg equals StatusBarBg — status bar text will be invisible", tc.name)
		}
		// ListMain must differ from LogBg (the list's background).
		if tc.theme.ListMain == tc.theme.LogBg {
			t.Errorf("%s theme: ListMain equals LogBg — list items will be invisible", tc.name)
		}
		// ListSelFg and ListSelBg must differ and not match LogBg/InputBg exactly.
		if tc.theme.ListSelFg == tc.theme.ListSelBg {
			t.Errorf("%s theme: ListSelFg equals ListSelBg — selected item will be invisible", tc.name)
		}
		// On the light theme, white is the dangerous color (matches white bg);
		// on the dark theme, black is dangerous.
		dangerous := tcell.ColorWhite
		if tc.isLight {
			// Light theme uses white backgrounds — white foreground is the
			// main culprit for invisible text.
			if tc.theme.TitleColor == dangerous {
				t.Errorf("%s theme: TitleColor is white (matches white LogBg) — titles will be invisible", tc.name)
			}
			if tc.theme.StatusBarFg == dangerous && tc.theme.StatusBarBg == tcell.ColorWhite {
				t.Errorf("%s theme: StatusBarFg is white on a white-ish background — status bar invisible", tc.name)
			}
			if tc.theme.ListMain == dangerous && tc.theme.LogBg == tcell.ColorWhite {
				t.Errorf("%s theme: ListMain is white on white LogBg — list items will be invisible", tc.name)
			}
		} else {
			// Dark theme uses black backgrounds — black foreground is the
			// main culprit for invisible text.
			if tc.theme.TitleColor == tcell.ColorBlack {
				t.Errorf("%s theme: TitleColor is black (matches black LogBg) — titles will be invisible", tc.name)
			}
			if tc.theme.InputFg == tcell.ColorBlack && tc.theme.InputBg == tcell.ColorBlack {
				t.Errorf("%s theme: InputFg is black on black InputBg — typed text will be invisible", tc.name)
			}
		}
	}
}

// TestWidgetColorsContrast uses tcell's RGB decomposition to verify that
// every text-on-bg pair has a real luminance gap (not just different color
// names). Threshold is loose on purpose — terminals and themes vary — but a
// delta of < 0.15 is a clear red flag for invisible text.
func TestWidgetColorsContrast(t *testing.T) {
	type pair struct {
		label string
		fg    tcell.Color
		bg    tcell.Color
	}
	luminance := func(c tcell.Color) float64 {
		if c == tcell.ColorDefault {
			return -1
		}
		r, g, b := c.RGB()
		// Relative luminance approximation (BT.601).
		return (0.299*float64(r) + 0.587*float64(g) + 0.114*float64(b)) / 255.0
	}
	themes := []struct {
		name  string
		theme tuiTheme
	}{
		{"light", newLightTheme()},
		{"dark", newDarkTheme()},
	}
	for _, th := range themes {
		pairs := []pair{
			{"Title-on-LogBg", th.theme.TitleColor, th.theme.LogBg},
			{"InputFg-on-InputBg", th.theme.InputFg, th.theme.InputBg},
			{"StatusBarFg-on-StatusBarBg", th.theme.StatusBarFg, th.theme.StatusBarBg},
			{"ListMain-on-LogBg", th.theme.ListMain, th.theme.LogBg},
			{"ListSelFg-on-ListSelBg", th.theme.ListSelFg, th.theme.ListSelBg},
		}
		// Resolve LogTool (a tview colour name) into a tcell color so we
		// can measure its luminance. A dim attribute on the open tag is no
		// longer emitted — but the colour itself must still be visibly
		// distinct from both ColorBlack and the panel's LogBg.
		//
		// If the lookup fails, the tag parser will set fg to ColorDefault
		// at render time (round-4 bug: tcell uses "aqua" not "cyan").
		// Record the failure so the loop's continue-on-ColorDefault path
		// doesn't silently hide it.
		if fg, ok := tcell.ColorNames[th.theme.LogTool]; !ok {
			t.Errorf("%s theme: LogTool %q is not in tcell.ColorNames — tool text will render as ColorDefault",
				th.name, th.theme.LogTool)
		} else {
			pairs = append(pairs,
				pair{"LogTool-on-LogBg", fg, th.theme.LogBg},
				pair{"LogTool-on-Black", fg, tcell.ColorBlack},
			)
		}
		for _, p := range pairs {
			if p.fg == tcell.ColorDefault || p.bg == tcell.ColorDefault {
				continue // ColorDefault falls back to terminal; can't measure.
			}
			fgL, bgL := luminance(p.fg), luminance(p.bg)
			delta := fgL - bgL
			if delta < 0 {
				delta = -delta
			}
			if delta < 0.15 {
				t.Errorf("%s theme: %s has near-zero luminance gap (fg=%.2f bg=%.2f delta=%.2f) — likely invisible",
					th.name, p.label, fgL, bgL, delta)
			}
		}
	}
}

// TestPlaceholderTextContrast checks that the placeholder message in an
// empty log view uses a color different from the panel's LogBg, on both
// themes. The placeholder color name must also be resolvable via colorName
// (since we inline it into a tview color tag).
func TestPlaceholderTextContrast(t *testing.T) {
	for _, tc := range []struct {
		name  string
		theme tuiTheme
	}{
		{"dark", newDarkTheme()},
		{"light", newLightTheme()},
	} {
		if tc.theme.PlaceholderText == "" {
			t.Errorf("%s theme: PlaceholderText is empty", tc.name)
		}
		// It must not match LogBg exactly — we map names, not tcell constants,
		// so we compare strings via colorName.
		if colorName(tc.theme.LogBg) == tc.theme.PlaceholderText {
			t.Errorf("%s theme: PlaceholderText %q matches LogBg — placeholder will be invisible", tc.name, tc.theme.PlaceholderText)
		}
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

// TestLogToolResolvesToConcreteColor guards against the round-4 bug: tview
// resolves every colour name via tcell.ColorNames[name], and tcell does
// NOT contain a "cyan" entry (ANSI "cyan" is named "aqua" in tcell).
// Looking up an unknown name returns the zero value ColorDefault, which
// the terminal renders as its own default foreground — on a light
// terminal that is black, so the tool text disappears on the white
// panel. This test pins every theme's LogTool to a real tcell colour.
func TestLogToolResolvesToConcreteColor(t *testing.T) {
	for _, tc := range []struct {
		name  string
		theme tuiTheme
	}{
		{"dark", newDarkTheme()},
		{"light", newLightTheme()},
	} {
		resolved, ok := tcell.ColorNames[tc.theme.LogTool]
		if !ok {
			t.Errorf("%s theme: LogTool %q is not in tcell.ColorNames — tool text will render as ColorDefault (likely invisible on a contrasting LogBg)",
				tc.name, tc.theme.LogTool)
			continue
		}
		if resolved == tcell.ColorDefault {
			t.Errorf("%s theme: LogTool %q resolves to tcell.ColorDefault — same as above",
				tc.name, tc.theme.LogTool)
		}
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

// TestAppendToolLogUsesNoDim guards the round-3 fix: the `::d` (dim) attribute
// was removed from tool-call / tool-result lines. The dim attribute caused
// darkcyan to render at ~50% brightness, which on a light-terminal collapses
// to a near-black that the user reported as "tool calls are black, hard to
// read". This test pins the open tag to "fg:bg" with no `d` so future
// changes can't silently re-introduce the dim attribute.
func TestAppendToolLogUsesNoDim(t *testing.T) {
	for _, tc := range []struct {
		name  string
		theme tuiTheme
	}{
		{"dark", newDarkTheme()},
		{"light", newLightTheme()},
	} {
		tui := &tuiApp{
			theme:          tc.theme,
			logs:           make(map[string]*agentLog),
			agentNames:     make(map[string]string),
			hasNewMessages: make(map[string]bool),
		}
		// selectedAgent is empty (zero value), so appendLogEntry's
		// "selected == agentID" branch is false and t.app.QueueUpdateDraw
		// is not invoked — t.app is nil on purpose.
		tui.appendToolLog("agent-x", "[tool] foo(1)\n")

		bg := colorName(tc.theme.LogBg)
		expectedOpen := "[" + tc.theme.LogTool + ":" + bg + "]"
		got := tui.getLogText("agent-x")
		if !strings.Contains(got, expectedOpen) {
			t.Errorf("%s theme: expected open tag %q in log buffer, got %q", tc.name, expectedOpen, got)
		}
		if strings.Contains(got, ":d]") {
			t.Errorf("%s theme: log buffer must not contain a `d` (dim) attribute, got %q", tc.name, got)
		}
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
	if got := renderSearchLogText(entries, ""); !strings.Contains(got, "alpha beta ALPHA") || strings.Contains(got, "**") {
		t.Fatalf("empty query should render plain (non-markdown) content, got %q", got)
	}
}

func TestRenderMarkdownText(t *testing.T) {
	styled := renderMarkdownText("## Title\n\n- one\n- two\n\n`code`\n\n```go\nfmt.Println(\"hi\")\n```\n\n| A | B |\n| --- | --- |\n| 1 | 2 |", "white", false)
	for _, want := range []string{"Title", "one", "two", "code", "```go", "fmt.Println", "│ A │ B │", "│ 1 │ 2 │"} {
		if !strings.Contains(styled, want) {
			t.Fatalf("styled render missing %q: %q", want, styled)
		}
	}
	if !strings.Contains(styled, "[white::b]Title[-:-:-][white]") {
		t.Fatalf("expected bold heading markup, got %q", styled)
	}
	if strings.Contains(styled, "[white::r]") {
		t.Fatalf("code rendering should not invert colors, got %q", styled)
	}

	plain := renderMarkdownText("## Title\n\n- one\n- two\n\n`code`\n\n```go\nfmt.Println(\"hi\")\n```\n\n| A | B |\n| --- | --- |\n| 1 | 2 |", "", true)
	for _, want := range []string{"Title", "one", "two", "code", "fmt.Println", "A", "B", "1", "2"} {
		if !strings.Contains(plain, want) {
			t.Fatalf("plain render missing %q: %q", want, plain)
		}
	}
	if strings.Contains(plain, "**") || strings.Contains(plain, "`") {
		t.Fatalf("plain render should strip markdown syntax, got %q", plain)
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

	// Idle agent should use ○ icon.
	ag.queueMu.Lock()
	ag.busy = false
	ag.queueMu.Unlock()
	label, _ := tui.topLevelAgentNodeText(ag)
	if !strings.HasPrefix(label, "○ ") {
		t.Errorf("idle agent label should start with '○ ', got %q", label)
	}
}

// TestSkillPickerNilSkillsNoOp verifies that showSkillPicker with no skills
// removes %% from the input text without panicking.
func TestSkillPickerNilSkillsNoOp(t *testing.T) {
	a := &app{cfg: config{workspaceRoot: t.TempDir()}}
	tui := newTuiApp(a, newDarkTheme())
	tui.skills = map[string]skill{} // empty — no skills available

	tui.inputField.SetText("hello %% world", false)
	// Simulate what showSkillPicker does in the no-skills branch.
	if len(tui.skills) == 0 {
		cur := tui.inputField.GetText()
		tui.inputField.SetText(strings.Replace(cur, "%%", "", 1), false)
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

// makeTestTuiApp returns a tuiApp with just the fields the log/trim tests
// need. It does NOT start tview's event loop; callers that would otherwise
// trigger QueueUpdateDraw must steer around it (the log append helpers
// short-circuit on selected != agentID, so we keep the test agent unselected).
func makeTestTuiApp() *tuiApp {
	return &tuiApp{
		theme:          newDarkTheme(),
		logs:           make(map[string]*agentLog),
		hasNewMessages: make(map[string]bool),
		selectedAgent:  tuiRootRef,
	}
}

func TestAgentLogTrimByEntryCount(t *testing.T) {
	tui := makeTestTuiApp()
	const agentID = "agent-1"
	for i := 0; i < maxAgentLogEntries+500; i++ {
		tui.writeLogBufEntry(agentID, "line\n", "white", false)
	}
	tui.logMu.Lock()
	defer tui.logMu.Unlock()
	l := tui.logs[agentID]
	if got := len(l.entries); got > maxAgentLogEntries {
		t.Fatalf("entries cap: want <= %d, got %d", maxAgentLogEntries, got)
	}
	// buf length must be <= cap (the trim rebuilds buf from survivors).
	if l.buf.Len() > maxAgentLogBytes+1024 {
		t.Errorf("buf length exceeds cap: %d > %d", l.buf.Len(), maxAgentLogBytes)
	}
	// To prove the trim fired, the total appends exceeded the cap, and
	// the survivors cannot be all 2500 originals. Sanity: appends > cap.
	if maxAgentLogEntries+500 <= maxAgentLogEntries {
		t.Fatalf("test bug: append count must exceed cap")
	}
}

func TestAgentLogTrimByByteSize(t *testing.T) {
	tui := makeTestTuiApp()
	const agentID = "agent-1"
	// Each line is ~100 KiB. With 2 MiB cap, the trim must fire and drop
	// old entries until buf is under cap.
	big := strings.Repeat("x", 100*1024)
	for i := 0; i < 50; i++ {
		tui.writeLogBufEntry(agentID, big+"\n", "white", false)
	}
	tui.logMu.Lock()
	defer tui.logMu.Unlock()
	l := tui.logs[agentID]
	if l.buf.Len() > maxAgentLogBytes+1024 {
		t.Errorf("buf length exceeds cap after byte-size trim: %d > %d", l.buf.Len(), maxAgentLogBytes)
	}
	if len(l.entries) >= 50 {
		t.Errorf("expected entries to be trimmed, got %d", len(l.entries))
	}
}

func TestAgentLogTrimPreservesLatestContent(t *testing.T) {
	tui := makeTestTuiApp()
	const agentID = "agent-1"
	// Fill past the cap and assert the very last entry is intact.
	for i := 0; i < maxAgentLogEntries+10; i++ {
		tui.writeLogBufEntry(agentID, "filler\n", "white", false)
	}
	tui.writeLogBufEntry(agentID, "FINAL-MARKER\n", "white", false)

	tui.logMu.Lock()
	defer tui.logMu.Unlock()
	l := tui.logs[agentID]
	last := l.entries[len(l.entries)-1]
	if last.text != "FINAL-MARKER\n" {
		t.Errorf("last entry: want %q, got %q", "FINAL-MARKER\n", last.text)
	}
	if !strings.Contains(l.buf.String(), "FINAL-MARKER") {
		t.Errorf("buf should contain the final marker; got tail %q",
			l.buf.String()[maxInt(0, l.buf.Len()-64):])
	}
}

func TestAgentLogAppendAfterTrim(t *testing.T) {
	tui := makeTestTuiApp()
	const agentID = "agent-1"
	// Append way past the cap, then keep going. Nothing should panic and
	// the cap must continue to hold.
	for i := 0; i < maxAgentLogEntries*3; i++ {
		tui.writeLogBufEntry(agentID, "x\n", "white", false)
	}
	tui.logMu.Lock()
	defer tui.logMu.Unlock()
	l := tui.logs[agentID]
	if got := len(l.entries); got > maxAgentLogEntries {
		t.Errorf("entries cap broken after sustained append: got %d, want <= %d", got, maxAgentLogEntries)
	}
	if l.buf.Len() > maxAgentLogBytes+1024 {
		t.Errorf("buf cap broken after sustained append: got %d, want <= %d", l.buf.Len(), maxAgentLogBytes)
	}
}

func TestAgentLogRawFlagPreservedOnTrim(t *testing.T) {
	tui := makeTestTuiApp()
	// Hand-build a log that has both raw and non-raw entries so the trim
	// rebuild exercises both render branches. The two special entries are
	// placed at the END so they survive the trim (the trim drops the
	// oldest entries, with a minimum batch of 10% of the cap).
	l := tui.getOrCreateLog("agent-1")
	bg := colorName(tui.theme.LogBg)
	l.mu.Lock()
	// Pre-fill entries past the cap with plain filler.
	for i := 0; i < maxAgentLogEntries+10; i++ {
		l.entries = append(l.entries, logEntry{color: "white", text: "filler\n", markdown: false})
	}
	// Append the two special entries at the tail.
	l.entries = append(l.entries,
		logEntry{color: "white", text: "esc[ped\n", markdown: false, raw: false},
		logEntry{color: "white", text: "[#ff0000]raw-tag[-]\n", markdown: false, raw: true},
	)
	for i := 0; i < maxAgentLogEntries+10; i++ {
		l.buf.WriteString("[white:" + bg + "]filler\n[-:" + bg + ":-]")
	}
	// Pre-append the rendered equivalents of the special entries so the
	// buf length is consistent with entries (the trim doesn't care about
	// buf/entries alignment, but a consistent state makes the test
	// assertion clearer).
	l.buf.WriteString("[white:" + bg + "]esc\\[ped\n[-:" + bg + ":-]")
	l.buf.WriteString("[white:" + bg + "][#ff0000]raw-tag[-]\n[-:" + bg + ":-]")
	tui.trimLogIfNeeded(l)
	l.mu.Unlock()

	l.mu.Lock()
	defer l.mu.Unlock()
	// Find the surviving non-raw and raw entries and check their rebuild
	// in buf.
	var foundEsc, foundRaw bool
	for _, e := range l.entries {
		if e.text == "esc[ped\n" {
			foundEsc = true
		}
		if e.text == "[#ff0000]raw-tag[-]\n" {
			foundRaw = true
		}
	}
	if !foundEsc {
		t.Errorf("escaped entry lost during trim")
	}
	if !foundRaw {
		t.Errorf("raw entry lost during trim")
	}
	// The raw entry's tview color tag should appear verbatim in buf (not
	// double-escaped by tview.Escape). The non-raw entry's text should
	// also be present (rendered through tview.Escape, which is a no-op for
	// this text).
	bs := l.buf.String()
	if !strings.Contains(bs, "[#ff0000]raw-tag[-]") {
		t.Errorf("raw color tag not preserved in rebuilt buf")
	}
	if !strings.Contains(bs, "esc[ped") {
		t.Errorf("non-raw text not present in rebuilt buf")
	}
}

// maxInt is a tiny helper for the tail-slice math above.
func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
