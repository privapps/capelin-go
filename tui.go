package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

// spinnerFrames are cycled per-frame while a top-level agent is busy.
var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

type agentLog struct {
	mu       sync.Mutex
	entries  []logEntry
	buf      strings.Builder
	atBottom bool
}

type logEntry struct {
	color string
	text  string
}

// tuiAgent holds all per-agent state for a top-level TUI agent.
type tuiAgent struct {
	num         int
	id          string    // "agent-1", "agent-2", ... (local routing label)
	sessionUUID string    // stable UUID used for session persistence
	createdAt   time.Time // when this agent was created
	msgMu       sync.Mutex
	messages    []apiMessage
	runtime     *agentRuntime

	queueMu    sync.Mutex
	busy       bool
	Name       string // short LLM-generated title, e.g. "research deepseek costs"
	inputQueue []string

	cancelMu sync.Mutex
	cancel   context.CancelFunc // non-nil while a turn is in flight
}

// tuiRootRef is the reference ID for the non-agent root container node in the agent tree.
const tuiRootRef = "__root__"

type tuiTheme struct {
	ActiveBorder   tcell.Color
	InactiveBorder tcell.Color
	AgentBusy      tcell.Color
	AgentIdle      tcell.Color
	AgentPending   tcell.Color
	AgentCompleted tcell.Color
	AgentFailed    tcell.Color
	AgentCancelled tcell.Color
	TreeSelectedBg tcell.Color
	TreeSelectedFg tcell.Color
	LogContent     string
	LogTool        string
	LogError       string
	LogSystem      string
	LogUserInput   string
	LogBg          tcell.Color
	InputBg        tcell.Color
	StatusBarBg    tcell.Color
}

func newDarkTheme() tuiTheme {
	return tuiTheme{
		ActiveBorder:   tcell.ColorAqua,
		InactiveBorder: tcell.ColorGray,
		AgentBusy:      tcell.ColorYellow,
		AgentIdle:      tcell.ColorWhite,
		AgentPending:   tcell.ColorGray,
		AgentCompleted: tcell.ColorGreen,
		AgentFailed:    tcell.ColorRed,
		AgentCancelled: tcell.ColorDarkGray,
		TreeSelectedBg: tcell.ColorNavy,
		TreeSelectedFg: tcell.ColorWhite,
		LogContent:     "white",
		LogTool:        "cyan",
		LogError:       "red",
		LogSystem:      "yellow",
		LogUserInput:   "aqua::b",
		LogBg:          tcell.ColorBlack,
		InputBg:        tcell.ColorNavy,
		StatusBarBg:    tcell.ColorDarkSlateGray,
	}
}

func newLightTheme() tuiTheme {
	return tuiTheme{
		ActiveBorder:   tcell.ColorBlue,
		InactiveBorder: tcell.ColorDarkGray,
		AgentBusy:      tcell.ColorOlive,
		AgentIdle:      tcell.ColorBlack,
		AgentPending:   tcell.ColorDarkGray,
		AgentCompleted: tcell.ColorDarkGreen,
		AgentFailed:    tcell.ColorMaroon,
		AgentCancelled: tcell.ColorGray,
		TreeSelectedBg: tcell.ColorBlue,
		TreeSelectedFg: tcell.ColorWhite,
		LogContent:     "black",
		LogTool:        "teal",
		LogError:       "maroon",
		LogSystem:      "olive",
		LogUserInput:   "navy::b",
		LogBg:          tcell.ColorWhite,
		InputBg:        tcell.ColorLightBlue,
		StatusBarBg:    tcell.ColorSilver,
	}
}

// detectTheme checks COLORFGBG env var. Format is "fg;bg" where bg is
// a color number. Values >= 8 are typically light themes.
// Falls back to dark if detection fails.
func detectTheme() tuiTheme {
	fgbg := os.Getenv("COLORFGBG")
	if fgbg != "" {
		parts := strings.Split(fgbg, ";")
		if len(parts) >= 2 {
			bg := strings.TrimSpace(parts[len(parts)-1])
			n, err := strconv.Atoi(bg)
			if err == nil && n >= 8 {
				return newLightTheme()
			}
		}
	}
	return newDarkTheme()
}

type panelID int

const (
	panelNone   panelID = 0
	panelAgents panelID = 1
	panelLog    panelID = 2
	panelInput  panelID = 3
)

type tuiApp struct {
	owner        *app
	ctx          context.Context
	app          *tview.Application
	theme        tuiTheme
	sysPrompt    string      // system prompt used when creating new agents
	normalLayout *tview.Flex // built once; re-used every time maximized=panelNone
	searchLayout *tview.Flex // search mode: topFlex + searchBar + statusFlex
	topFlex      *tview.Flex // top row (agents + log); stored for hide/show
	agentTree    *tview.TreeView
	logView      *tview.TextView
	inputField   *tview.InputField
	searchField  *tview.InputField // search input (shown in place of inputField during search)
	searchBar    *tview.Flex       // flex row containing search label + searchField
	statusBar    *tview.TextView
	statusCWD    *tview.TextView // right-aligned current working directory
	focusMode    bool
	menuHidden   bool // true when agents panel is hidden
	focusedPanel panelID
	maximized    panelID
	mouseCapture bool // true = tview handles mouse; false = terminal copy mode

	// search state
	searchMode         bool
	searchMatches      []int // line indices matching the current query
	searchCurrentMatch int

	// Skills loaded at startup, used for the %% inline skill picker.
	skills            map[string]skill
	skillPickerActive bool // true while the skill picker overlay is visible

	// lastCtrlCAt is used to require two Ctrl+C presses within 2 s to exit.
	lastCtrlCAt time.Time

	// spinnerFrame is incremented every 150 ms while any agent is busy,
	// driving the Braille spinner icon in the agents panel.
	spinnerFrame atomic.Int32

	// Top-level agents (numbered 1, 2, 3...). Each has its own conversation.
	agentsMu     sync.Mutex
	topAgents    map[string]*tuiAgent
	nextAgentNum int

	logMu          sync.Mutex
	logs           map[string]*agentLog
	selectedAgent  string            // currently displayed agent ID (or tuiRootRef)
	agentNames     map[string]string // ID -> display name (updated by rebuildAgentTree)
	subagents      *subagentManager
	hasNewMessages map[string]bool
}

type tuiSink struct {
	tui *tuiApp
}

func (s *tuiSink) WriteContent(agentID, content string) {
	s.tui.appendLog(agentID, content+"\n", s.tui.theme.LogContent)
}

func (s *tuiSink) WriteToolCall(agentID, toolName, args string) {
	s.tui.appendLog(agentID, fmt.Sprintf("[tool] %s(%s)\n", toolName, args), s.tui.theme.LogTool+"::d")
}

func (s *tuiSink) WriteToolResult(agentID, toolName string, isError bool, detail string) {
	if isError {
		s.tui.appendLog(agentID, fmt.Sprintf("[tool] %s error: %s\n", toolName, detail), s.tui.theme.LogError+"::d")
		return
	}
	s.tui.appendLog(agentID, fmt.Sprintf("[tool] %s done\n", toolName), s.tui.theme.LogTool+"::d")
}

func (s *tuiSink) WriteSystem(agentID, msg string) {
	s.tui.appendLog(agentID, msg+"\n", s.tui.theme.LogSystem)
}

func newTuiApp(a *app, theme tuiTheme) *tuiApp {
	tui := &tuiApp{
		owner:          a,
		app:            tview.NewApplication(),
		theme:          theme,
		logs:           make(map[string]*agentLog),
		agentNames:     make(map[string]string),
		hasNewMessages: make(map[string]bool),
		selectedAgent:  tuiRootRef,
		subagents:      a.subagents,
		focusedPanel:   panelInput,
		mouseCapture:   true,
		topAgents:      make(map[string]*tuiAgent),
	}

	// Load skills for the %% inline picker. Failure is non-fatal.
	if skills, err := loadSkills(a.cfg.workspaceRoot); err == nil {
		tui.skills = skills
	} else {
		tui.skills = map[string]skill{}
	}

	// --- Agents panel (left 1/5) ---
	tui.agentTree = tview.NewTreeView()
	tui.agentTree.SetRoot(tview.NewTreeNode("Agents").SetExpanded(true))
	tui.agentTree.SetBorder(true)
	tui.agentTree.SetTitle(" Agents ")
	tui.agentTree.SetBorderColor(theme.InactiveBorder)

	// --- Log / messages panel (right 4/5) ---
	tui.logView = tview.NewTextView()
	tui.logView.SetDynamicColors(true)
	tui.logView.SetScrollable(true)
	tui.logView.SetWrap(true)
	tui.logView.SetBorder(true)
	tui.logView.SetTitle(" Agent 1 ")
	tui.logView.SetBorderColor(theme.InactiveBorder)
	tui.logView.SetBackgroundColor(theme.LogBg)

	// --- Input panel (full width, bottom) ---
	tui.inputField = tview.NewInputField()
	tui.inputField.SetLabel("> ")
	tui.inputField.SetFieldBackgroundColor(theme.InputBg)
	tui.inputField.SetBorder(true)
	tui.inputField.SetTitle(" Input -> Agent 1 ")
	tui.inputField.SetBorderColor(theme.ActiveBorder) // starts focused

	// --- Status bar (1 row, no border) ---
	tui.statusBar = tview.NewTextView()
	tui.statusBar.SetDynamicColors(true)
	tui.statusBar.SetBackgroundColor(theme.StatusBarBg)
	tui.updateStatusBar()

	// --- CWD display (right side of status bar row) ---
	tui.statusCWD = tview.NewTextView()
	tui.statusCWD.SetDynamicColors(true)
	tui.statusCWD.SetBackgroundColor(theme.StatusBarBg)
	tui.statusCWD.SetTextAlign(tview.AlignRight)
	cwd := a.cfg.workspaceRoot
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	// Truncate from the left so the basename is always visible.
	const maxCWDLen = 45
	runes := []rune(cwd)
	if len(runes) > maxCWDLen {
		cwd = "…" + string(runes[len(runes)-maxCWDLen+1:])
	}
	cwdDisplay := cwd + " "
	tui.statusCWD.SetText(cwdDisplay)

	// --- Wire agent tree callbacks ---
	tui.agentTree.SetSelectedFunc(func(node *tview.TreeNode) {
		if node == nil {
			return
		}
		id, _ := node.GetReference().(string)
		if id == "" {
			return
		}
		tui.selectAgent(id)
		// Move keyboard focus to the input panel so the user can start typing immediately.
		tui.setFocus(panelInput)
	})
	// Use SetChangedFunc only for keyboard navigation (not initial render)
	tui.agentTree.SetChangedFunc(func(node *tview.TreeNode) {
		if node == nil {
			return
		}
		id, _ := node.GetReference().(string)
		if id == "" {
			return
		}
		tui.selectAgent(id)
	})

	// --- Mouse capture ---
	tui.agentTree.SetMouseCapture(func(action tview.MouseAction, event *tcell.EventMouse) (tview.MouseAction, *tcell.EventMouse) {
		if action == tview.MouseLeftClick {
			tui.setFocus(panelAgents)
		}
		return action, event
	})
	tui.logView.SetMouseCapture(func(action tview.MouseAction, event *tcell.EventMouse) (tview.MouseAction, *tcell.EventMouse) {
		switch action {
		case tview.MouseLeftClick:
			tui.setFocus(panelLog)
		case tview.MouseScrollUp:
			// Mouse handlers are called by the event loop — call directly, no QueueUpdateDraw.
			tui.setAgentAtBottom(tui.currentSelectedAgent(), false)
			tui.updateMoreIndicator()
		case tview.MouseScrollDown:
			tui.setAgentAtBottom(tui.currentSelectedAgent(), true)
			tui.updateMoreIndicator()
		}
		return action, event
	})
	tui.inputField.SetMouseCapture(func(action tview.MouseAction, event *tcell.EventMouse) (tview.MouseAction, *tcell.EventMouse) {
		if action == tview.MouseLeftClick {
			tui.setFocus(panelInput)
		}
		return action, event
	})

	// --- Log view keyboard input ---
	tui.logView.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		if event.Key() == tcell.KeyEscape && tui.maximized == panelLog {
			tui.maximized = panelNone
			tui.applyLayout()
			tui.updateStatusBar()
			return nil
		}
		switch event.Key() {
		case tcell.KeyPgUp:
			tui.setAgentAtBottom(tui.currentSelectedAgent(), false)
			tui.updateMoreIndicator()
		case tcell.KeyPgDn:
			tui.setAgentAtBottom(tui.currentSelectedAgent(), true)
			tui.updateMoreIndicator()
		}
		return event
	})

	// --- Slash command autocomplete ---
	tui.inputField.SetAutocompleteFunc(func(currentText string) []string {
		if !strings.HasPrefix(currentText, "/") {
			return nil
		}
		entries := []string{}
		for _, cmd := range slashCommands {
			if cmd.topLevelOnly && !tui.isTopLevelAgent(tui.currentSelectedAgent()) {
				continue
			}
			label := cmd.name + " — " + cmd.description
			if strings.HasPrefix(label, currentText) || strings.HasPrefix(cmd.name, currentText) {
				entries = append(entries, label)
			}
		}
		return entries
	})
	tui.inputField.SetAutocompletedFunc(func(text string, index int, source int) bool {
		// When the user is just navigating (arrow keys), don't accept — let them keep browsing.
		if source == tview.AutocompletedNavigate {
			return false
		}
		// Strip the description suffix so only the command is placed in the input.
		if idx := strings.Index(text, " — "); idx != -1 {
			text = text[:idx]
		}
		// For commands that need arguments, keep trailing space for easy typing.
		if text == "/save" || text == "/session-resume" || text == "/workspace" || text == "/workspace-save" {
			text = text + " "
		}
		tui.inputField.SetText(text)
		return true // close drop-down
	})

	// --- %% trigger: show skill picker ---
	// When the user types %% anywhere in the input, pop up the skill selector.
	tui.inputField.SetChangedFunc(func(text string) {
		if hasBareSkillTrigger(text) && !tui.skillPickerActive {
			tui.showSkillPicker()
			return
		}

		if cmd, ok := commandCascadeTrigger(text); ok {
			switch cmd {
			case "/session-resume":
				go tui.handleSessionResume(tui.currentSelectedAgent(), "")
			case "/workspace":
				go tui.handleWorkspacePicker(tui.currentSelectedAgent())
			case "/session-fork":
				// Keep the command in the input so the user can continue typing args.
				tui.inputField.SetText(strings.TrimSpace(cmd))
				if agent := tui.getTopLevelAgent(tui.currentSelectedAgent()); agent != nil {
					tui.showSessionForkCascade(agent)
				}
			case "/append-to-agent":
				// Keep the command in the input so the user can continue typing args.
				tui.inputField.SetText(strings.TrimSpace(cmd))
				if agent := tui.getTopLevelAgent(tui.currentSelectedAgent()); agent != nil {
					tui.showAppendToAgentCascade(agent)
				}
			}
		}
	})

	// --- Input submission ---
	tui.inputField.SetDoneFunc(func(key tcell.Key) {
		if key != tcell.KeyEnter {
			return
		}
		text := strings.TrimSpace(tui.inputField.GetText())
		if text == "" {
			return
		}
		// Handle slash commands first — must use goroutine since appendLog calls QueueUpdateDraw.
		if strings.HasPrefix(text, "/") {
			tui.inputField.SetText("")
			go tui.handleSlashCommand(text) // goroutine: appendLog inside calls QueueUpdateDraw
			return
		}
		// Legacy bare keywords — also treat as exit.
		if text == "exit" || text == "quit" {
			tui.app.Stop()
			return
		}
		tui.inputField.SetText("")

		selected := tui.currentSelectedAgent()
		agent := tui.getTopLevelAgent(selected)
		if agent == nil {
			// Root container — auto-create a new top-level agent and send to it.
			if selected == tuiRootRef {
				newAgent := tui.createTopLevelAgent(tui.sysPrompt)
				tui.selectAgent(newAgent.id)
				agent = newAgent
			} else {
				// Subagent — read-only.
				go tui.appendLog(selected, "[capelin-go] Cannot send messages to a subagent directly. Use /save to save logs.\n", tui.theme.LogError)
				return
			}
		}

		// Route to the selected top-level agent. SetDoneFunc is called by the event loop
		// so we call tview methods directly (no QueueUpdateDraw).
		agent.queueMu.Lock()
		if !agent.busy {
			agent.busy = true
			agent.queueMu.Unlock()
			tui.inputField.SetLabel("> [busy] ")
			go tui.submitInputForAgent(agent, text)
		} else {
			agent.inputQueue = append(agent.inputQueue, text)
			n := len(agent.inputQueue)
			agent.queueMu.Unlock()
			tui.inputField.SetLabel(fmt.Sprintf("> [%d queued] ", n))
		}
	})

	// --- Input field keyboard: Esc unmaximizes when input panel is maximized ---
	tui.inputField.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		if event.Key() == tcell.KeyEscape && tui.maximized == panelInput {
			tui.maximized = panelNone
			tui.applyLayout()
			tui.updateStatusBar()
			return nil
		}
		return event
	})

	// --- Global key handler (Ctrl+C, Ctrl+D, Tab, F1) ---
	tui.app.SetInputCapture(tui.handleGlobalKeys)
	tui.app.EnableMouse(true)

	// --- Build layouts ONCE ---
	// All layout changes reuse these same widget references.
	topFlex := tview.NewFlex().
		SetDirection(tview.FlexColumn).
		AddItem(tui.agentTree, 0, 1, false).
		AddItem(tui.logView, 0, 4, false)
	tui.topFlex = topFlex
	cwdWidth := len([]rune(cwd)) + 2 // +1 for trailing space, +1 margin
	statusFlex := tview.NewFlex().
		SetDirection(tview.FlexColumn).
		AddItem(tui.statusBar, 0, 1, false).
		AddItem(tui.statusCWD, cwdWidth, 0, false)
	tui.normalLayout = tview.NewFlex().
		SetDirection(tview.FlexRow).
		AddItem(topFlex, 0, 1, false).
		AddItem(tui.inputField, 3, 0, false).
		AddItem(statusFlex, 1, 0, false)

	// searchField + searchBar row for search mode (F4).
	tui.searchField = tview.NewInputField().
		SetLabel(" Search: ").
		SetFieldWidth(0)
	tui.searchField.SetBorder(false)
	tui.searchBar = tview.NewFlex().
		SetDirection(tview.FlexColumn).
		AddItem(tui.searchField, 0, 1, true)
	tui.searchBar.SetBorder(true).SetTitle(" F4 Search  Enter/Tab: next  Shift+Tab: prev  Esc: close ")
	tui.searchLayout = tview.NewFlex().
		SetDirection(tview.FlexRow).
		AddItem(topFlex, 0, 1, false).
		AddItem(tui.searchBar, 3, 0, false).
		AddItem(statusFlex, 1, 0, false)

	// Wire search field callbacks.
	tui.searchField.SetChangedFunc(func(text string) {
		tui.updateSearch(text)
	})
	tui.searchField.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		switch event.Key() {
		case tcell.KeyEscape:
			tui.closeSearch()
			return nil
		case tcell.KeyEnter:
			tui.nextSearchMatch(1)
			return nil
		case tcell.KeyTab:
			tui.nextSearchMatch(1)
			return nil
		case tcell.KeyBacktab:
			tui.nextSearchMatch(-1)
			return nil
		}
		return event
	})

	// Set initial root/focus — do this LAST, after all widgets are configured.
	tui.app.SetRoot(tui.normalLayout, true).SetFocus(tui.inputField)

	return tui
}

func (a *app) runTUI(ctx context.Context) error {
	theme := detectTheme()
	tui := newTuiApp(a, theme)
	a.sink = &tuiSink{tui: tui}
	sysPrompt := a.systemPromptWithSkills()
	tui.sysPrompt = sysPrompt

	if a.cfg.initialQuestion != "" {
		// Create Agent 1 and submit the initial query.
		agent1 := tui.createTopLevelAgent(sysPrompt)
		// Pre-populate log before Run() starts (no QueueUpdateDraw yet).
		tui.logView.SetText(tui.getLogText(agent1.id))
		tui.logView.ScrollToEnd()
		// Mark busy directly — app.Run() hasn't started so QueueUpdateDraw would deadlock.
		agent1.queueMu.Lock()
		agent1.busy = true
		agent1.queueMu.Unlock()
		tui.inputField.SetLabel("> [busy] ")
		go func() {
			tui.submitInputForAgent(agent1, a.cfg.initialQuestion)
		}()
	} else if a.cfg.workspaceRoot != "" {
		// Try to restore the last workspace. Fall back to the default placeholder
		// if the file is missing or all sessions fail to load.
		tui.restoreLastWorkspace()
		if len(tui.topAgents) == 0 {
			tui.logView.SetText("[gray]Select an agent from the panel, or type a message here to start a new Agent.[-::-]")
			tui.updateMoreIndicator()
		}
	} else {
		// No initial query: show root placeholder, user creates the first agent by typing.
		tui.logView.SetText("[gray]Select an agent from the panel, or type a message here to start a new Agent.[-::-]")
		tui.updateMoreIndicator()
	}
	return tui.run(ctx)
}

// restoreLastWorkspace loads agents from the "last" workspace snapshot.
// Called once before tui.run() — no QueueUpdateDraw available yet.
func (t *tuiApp) restoreLastWorkspace() {
	snap, err := loadWorkspace(t.owner.cfg.workspaceRoot, "last")
	if err != nil {
		return // file absent or unreadable — not an error
	}
	lastID := ""
	for _, entry := range snap.Agents {
		msgs, err := loadSessionMessages(t.owner.cfg.workspaceRoot, entry.SessionUUID)
		if err != nil {
			continue // session file missing — skip silently
		}
		agent := t.createTopLevelAgentWithMessages(msgs, false)
		// Restore stable UUID, creation time, and name.
		agent.sessionUUID = entry.SessionUUID
		if entry.Name != "" {
			agent.queueMu.Lock()
			agent.Name = entry.Name
			agent.queueMu.Unlock()
			label := agentNodeLabel(agent.num, entry.Name)
			t.logMu.Lock()
			t.agentNames[agent.id] = label
			t.logMu.Unlock()
		}
		t.replayMessagesToLogBuf(agent.id, msgs)
		t.writeLogBuf(agent.id,
			fmt.Sprintf("\n--- Session resumed: %s (%d messages) ---\n\n", entry.SessionUUID[:8], len(msgs)),
			t.theme.LogSystem)
		lastID = agent.id
	}
	// Show the last restored agent in the log view.
	if lastID != "" {
		t.logMu.Lock()
		t.selectedAgent = lastID
		t.logMu.Unlock()
		t.logView.SetText(t.getLogText(lastID))
		t.logView.ScrollToEnd()
	}
}

func (t *tuiApp) run(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	t.ctx = ctx
	go func() {
		<-ctx.Done()
		t.app.Stop()
	}()
	// Rebuild agent tree + advance busy spinner periodically after Run() has started.
	go func() {
		ticker := time.NewTicker(150 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				// Advance spinner frame when any top-level agent is busy.
				t.agentsMu.Lock()
				anyBusy := false
				for _, a := range t.topAgents {
					a.queueMu.Lock()
					if a.busy {
						anyBusy = true
					}
					a.queueMu.Unlock()
					if anyBusy {
						break
					}
				}
				t.agentsMu.Unlock()
				if anyBusy {
					t.spinnerFrame.Add(1)
				}
				var subNodes []tuiAgentNode
				if t.subagents != nil {
					subNodes = t.subagents.ListAll()
				}
				topLevel := t.snapshotTopLevelAgents()
				t.app.QueueUpdateDraw(func() {
					t.rebuildAgentTree(topLevel, subNodes)
				})
			}
		}
	}()
	return t.app.Run()
}

// submitInputForAgent sends text to the given top-level agent's conversation.
// Must be called from a goroutine (not the event loop).
func (t *tuiApp) submitInputForAgent(agent *tuiAgent, text string) {
	baseCtx := t.ctx
	if baseCtx == nil {
		baseCtx = context.Background()
	}
	ctx, cancelFn := context.WithCancel(baseCtx)
	agent.cancelMu.Lock()
	agent.cancel = cancelFn
	agent.cancelMu.Unlock()
	defer func() {
		agent.cancelMu.Lock()
		agent.cancel = nil
		agent.cancelMu.Unlock()
		cancelFn()
	}()

	t.appendLog(agent.id, "▶ "+text+"\n", t.theme.LogUserInput)

	agent.msgMu.Lock()
	msgs := make([]apiMessage, len(agent.messages))
	copy(msgs, agent.messages)
	rt := agent.runtime
	agent.msgMu.Unlock()

	preTurnLen := len(msgs)
	msgs, _, err := t.owner.runTurnLoop(ctx, msgs, text, rt, t.owner.toolset, true)
	if err != nil {
		if ctx.Err() == nil {
			t.appendLog(agent.id, "[capelin-go] error: "+err.Error()+"\n", t.theme.LogError)
		}
		msgs = msgs[:preTurnLen]
	}
	agent.msgMu.Lock()
	agent.messages = make([]apiMessage, len(msgs))
	copy(agent.messages, msgs)
	agent.msgMu.Unlock()

	// Persist messages snapshot so the session can be resumed via /attach.
	if t.owner.cfg.workspaceRoot != "" {
		snapMsgs := make([]apiMessage, len(msgs))
		copy(snapMsgs, msgs)
		agent.queueMu.Lock()
		agentName := agent.Name
		agent.queueMu.Unlock()
		go func() {
			if err := saveSessionMessages(t.owner.cfg.workspaceRoot, agent.sessionUUID, agentName, snapMsgs, agent.createdAt); err != nil {
				t.appendLog(agent.id, "[capelin-go] warning: session save failed: "+err.Error()+"\n", t.theme.LogError)
			}
			// Keep workspace in sync (captures any name updates that happened during the turn).
			t.saveCurrentWorkspace("last")
		}()
	}

	// Generate a short descriptive name after the first user turn.
	agent.queueMu.Lock()
	needsName := agent.Name == ""
	agent.queueMu.Unlock()
	if needsName && text != "" {
		go t.generateAgentName(agent, text)
	}

	t.processQueueForAgent(agent)
}

// processQueueForAgent dequeues the next input for the given agent and submits it,
// or marks the agent idle if the queue is empty.
// Must be called from a goroutine (not the event loop).
func (t *tuiApp) processQueueForAgent(agent *tuiAgent) {
	agent.queueMu.Lock()
	if len(agent.inputQueue) > 0 {
		next := agent.inputQueue[0]
		agent.inputQueue = agent.inputQueue[1:]
		remaining := len(agent.inputQueue)
		agent.queueMu.Unlock()
		t.app.QueueUpdateDraw(func() {
			if t.currentSelectedAgent() == agent.id {
				if remaining > 0 {
					t.inputField.SetLabel(fmt.Sprintf("> [%d queued] ", remaining))
				} else {
					t.inputField.SetLabel("> [busy] ")
				}
			}
		})
		go t.submitInputForAgent(agent, next)
		return
	}
	agent.busy = false
	agent.queueMu.Unlock()
	t.app.QueueUpdateDraw(func() {
		if t.currentSelectedAgent() == agent.id {
			t.inputField.SetLabel("> ")
			t.setFocus(panelInput)
		}
	})
}

// createTopLevelAgent creates a new top-level agent with the given system prompt,
// initializes its conversation, pre-populates its log, and selects it.
// Safe to call before or after app.Run() (uses writeLogBuf, not appendLog).
func (t *tuiApp) createTopLevelAgent(sysPrompt string) *tuiAgent {
	return t.createTopLevelAgentWithMessages([]apiMessage{{Role: "system", Content: sysPrompt}}, sysPrompt != "")
}

// createTopLevelAgentWithMessages creates a new top-level agent seeded with the
// provided message history. If writeSystemBlock is true, the system prompt is
// shown in the log panel (set false when loading an existing session — the
// caller should call replayMessagesToLogBuf instead).
func (t *tuiApp) createTopLevelAgentWithMessages(messages []apiMessage, writeSystemBlock bool) *tuiAgent {
	t.agentsMu.Lock()
	t.nextAgentNum++
	num := t.nextAgentNum
	id := fmt.Sprintf("agent-%d", num)
	uuid := newUUID()
	// runtime.sessionID MUST match agent.id so that tuiSink.WriteContent routes
	// output to the correct log buffer (keyed by agent.id, not by uuid).
	rt := t.owner.namedRuntime(id)
	agent := &tuiAgent{
		num:         num,
		id:          id,
		sessionUUID: uuid,
		createdAt:   time.Now().UTC(),
		messages:    messages,
		runtime:     rt,
	}
	t.topAgents[id] = agent
	t.agentsMu.Unlock()

	// Always show session UUID at the top of the log so the user can see it
	// without needing a /session-id command.
	t.writeLogBuf(id, fmt.Sprintf("[capelin-go] Session: %s\n", uuid), t.theme.LogSystem)

	if writeSystemBlock {
		sysContent := ""
		for _, m := range messages {
			if m.Role == "system" {
				sysContent = m.Content
				break
			}
		}
		t.writeLogBuf(id, "[System Prompt]\n"+sysContent+"\n\n", t.theme.LogSystem)
	}

	t.logMu.Lock()
	t.agentNames[id] = fmt.Sprintf("Agent %d", num)
	t.selectedAgent = id
	t.hasNewMessages[id] = false
	t.logMu.Unlock()

	// Async workspace auto-save whenever a new agent is added.
	go t.saveCurrentWorkspace("last")

	return agent
}

// saveCurrentWorkspace collects the current top-level agent list and writes it
// to .capelin-go/workspace/<name>.json. Safe to call from any goroutine.
func (t *tuiApp) saveCurrentWorkspace(name string) {
	if t.owner.cfg.workspaceRoot == "" {
		return
	}
	agents := t.snapshotTopLevelAgents()
	entries := make([]workspaceEntry, 0, len(agents))
	for _, a := range agents {
		a.queueMu.Lock()
		agentName := a.Name
		a.queueMu.Unlock()
		entries = append(entries, workspaceEntry{
			SessionUUID: a.sessionUUID,
			Name:        agentName,
		})
	}
	_ = saveWorkspace(t.owner.cfg.workspaceRoot, name, entries)
}

// replayMessagesToLogBuf writes the conversation history from msgs into the log
// buffer for agentID. Uses writeLogBuf (no QueueUpdateDraw) so the caller must
// trigger a redraw (e.g. via QueueUpdateDraw → selectAgent) afterwards.
func (t *tuiApp) replayMessagesToLogBuf(agentID string, msgs []apiMessage) {
	for _, m := range msgs {
		switch m.Role {
		case "system":
			t.writeLogBuf(agentID, "[System Prompt]\n"+m.Content+"\n\n", t.theme.LogSystem)
		case "user":
			t.writeLogBuf(agentID, "▶ "+m.Content+"\n", t.theme.LogUserInput)
		case "assistant":
			if strings.TrimSpace(m.Content) != "" {
				t.writeLogBuf(agentID, m.Content+"\n", t.theme.LogContent)
			}
			for _, tc := range m.ToolCalls {
				t.writeLogBuf(agentID,
					fmt.Sprintf("[tool] %s(%s)\n", tc.Function.Name, tc.Function.Arguments),
					t.theme.LogTool+"::d")
			}
		case "tool":
			name := m.Name
			if name == "" {
				name = m.ToolCallID
			}
			t.writeLogBuf(agentID, fmt.Sprintf("[tool] %s done\n", name), t.theme.LogTool+"::d")
		}
	}
}

// getTopLevelAgent returns the tuiAgent for the given ID, or nil if not found.
func (t *tuiApp) getTopLevelAgent(id string) *tuiAgent {
	t.agentsMu.Lock()
	defer t.agentsMu.Unlock()
	return t.topAgents[id]
}

// isTopLevelAgent returns true if the given ID belongs to a top-level agent.
func (t *tuiApp) isTopLevelAgent(id string) bool {
	return t.getTopLevelAgent(id) != nil
}

// snapshotTopLevelAgents returns a sorted copy of all top-level agents.
func (t *tuiApp) snapshotTopLevelAgents() []*tuiAgent {
	t.agentsMu.Lock()
	defer t.agentsMu.Unlock()
	agents := make([]*tuiAgent, 0, len(t.topAgents))
	for _, a := range t.topAgents {
		agents = append(agents, a)
	}
	sort.Slice(agents, func(i, j int) bool {
		return agents[i].num < agents[j].num
	})
	return agents
}

// topLevelAgentNodeText returns the plain label text and display color for a
// top-level agent tree node. Use SetTextStyle instead of color tags to ensure
// the node's selectedTextStyle is not overridden by embedded color tags.
func (t *tuiApp) topLevelAgentNodeText(agent *tuiAgent) (string, tcell.Color) {
	agent.queueMu.Lock()
	busy := agent.busy
	agentName := agent.Name
	agent.queueMu.Unlock()
	label := agentNodeLabel(agent.num, agentName)
	if busy {
		frame := int(t.spinnerFrame.Load()) % len(spinnerFrames)
		return spinnerFrames[frame] + " " + label, t.theme.AgentBusy
	}
	return "◌ " + label, t.theme.AgentIdle
}

// agentNodeLabel returns the display label for an agent given its number and optional name.
// Format: "N::short name" when name is set, "Agent N" otherwise.
func agentNodeLabel(num int, name string) string {
	if name != "" {
		return fmt.Sprintf("%d::%s", num, name)
	}
	return fmt.Sprintf("Agent %d", num)
}

// updateInputLabel updates the input field title and label to reflect the given agent.
// Must be called from the event loop (direct or via QueueUpdateDraw callback).
func (t *tuiApp) updateInputLabel(agent *tuiAgent) {
	if agent == nil {
		t.inputField.SetTitle(" Input -> New Agent ")
		t.inputField.SetLabel("> ")
		return
	}
	agent.queueMu.Lock()
	busy := agent.busy
	n := len(agent.inputQueue)
	agentName := agent.Name
	agent.queueMu.Unlock()
	label := agentNodeLabel(agent.num, agentName)
	t.inputField.SetTitle(fmt.Sprintf(" Input -> %s ", label))
	if busy {
		if n > 0 {
			t.inputField.SetLabel(fmt.Sprintf("> [%d queued] ", n))
		} else {
			t.inputField.SetLabel("> [busy] ")
		}
	} else {
		t.inputField.SetLabel("> ")
	}
}

func (t *tuiApp) handleGlobalKeys(event *tcell.EventKey) *tcell.EventKey {
	// Ctrl+C: require two presses within 2 s to exit.
	if event.Key() == tcell.KeyCtrlC {
		if time.Since(t.lastCtrlCAt) < 2*time.Second {
			t.app.Stop()
			return nil
		}
		t.lastCtrlCAt = time.Now()
		go t.appendLog(t.currentSelectedAgent(), "[capelin-go] Press Ctrl+C again to exit\n", t.theme.LogSystem)
		return nil
	}

	// Ctrl+E: toggle mouse copy mode.
	if event.Key() == tcell.KeyCtrlE {
		t.mouseCapture = !t.mouseCapture
		t.app.EnableMouse(t.mouseCapture)
		t.updateStatusBar()
		return nil
	}

	// F1: focus agents panel (un-hide menu if hidden, unmaximize if maximized).
	if event.Key() == tcell.KeyF1 {
		if t.maximized != panelNone {
			t.maximized = panelNone
			t.applyLayout()
		}
		if t.menuHidden {
			t.toggleMenuPanel()
		}
		t.focusMode = false
		t.setFocus(panelAgents)
		t.updateStatusBar()
		return nil
	}

	// F2: focus input panel (unmaximize if maximized).
	if event.Key() == tcell.KeyF2 {
		if t.maximized != panelNone {
			t.maximized = panelNone
			t.applyLayout()
		}
		if t.searchMode {
			t.closeSearch()
			return nil
		}
		t.focusMode = false
		t.setFocus(panelInput)
		t.updateStatusBar()
		return nil
	}

	// F3: toggle agents panel hide/show.
	if event.Key() == tcell.KeyF3 {
		t.toggleMenuPanel()
		t.updateStatusBar()
		return nil
	}

	// F4: open/close log search.
	if event.Key() == tcell.KeyF4 {
		if t.searchMode {
			t.closeSearch()
		} else {
			t.openSearch()
		}
		return nil
	}

	// F12: focus mode for panel navigation, maximize, stop/kill.
	if event.Key() == tcell.KeyF12 {
		t.focusMode = !t.focusMode
		t.updateStatusBar()
		return nil
	}

	// Escape: exit focus mode or restore from maximize.
	if event.Key() == tcell.KeyEscape {
		if t.maximized != panelNone {
			t.maximized = panelNone
			t.applyLayout()
			t.updateStatusBar()
			return nil
		}
		if t.focusMode {
			t.focusMode = false
			t.updateStatusBar()
			return nil
		}
	}

	if t.focusMode {
		switch event.Key() {
		case tcell.KeyLeft:
			t.setFocus(panelAgents)
			t.focusMode = false
			t.updateStatusBar()
			return nil
		case tcell.KeyRight:
			t.setFocus(panelLog)
			t.focusMode = false
			t.updateStatusBar()
			return nil
		case tcell.KeyUp:
			t.cycleFocus(-1)
			t.focusMode = false
			t.updateStatusBar()
			return nil
		case tcell.KeyDown:
			t.cycleFocus(1)
			t.focusMode = false
			t.updateStatusBar()
			return nil
		case tcell.KeyTab:
			// Tab in focus mode cycles panels without exiting focus mode.
			t.cycleFocus(1)
			return nil
		case tcell.KeyBacktab:
			t.cycleFocus(-1)
			return nil
		case tcell.KeyEnter:
			t.focusMode = false
			t.updateStatusBar()
			return nil
		case tcell.KeyRune:
			if event.Rune() == 'm' || event.Rune() == 'M' {
				t.toggleMaximize()
				t.focusMode = false
				t.updateStatusBar()
				return nil
			}
			// C = cancel the currently running agent.
			if event.Rune() == 'c' || event.Rune() == 'C' {
				selected := t.currentSelectedAgent()
				if err := t.cancelAgent(selected); err == nil {
					go t.appendLog(selected, "[capelin-go] canceled\n", t.theme.LogSystem)
				}
				t.focusMode = false
				t.updateStatusBar()
				return nil
			}
		}
		return nil // swallow unrecognised keys in focus mode
	}

	return event
}

// applyLayout sets the tview root based on current maximized state.
// normalLayout is always reused (never rebuilt); maximize just sets the
// individual widget as root so it fills the whole screen.
func (t *tuiApp) applyLayout() {
	switch t.maximized {
	case panelAgents:
		t.app.SetRoot(t.agentTree, true)
		t.app.SetFocus(t.agentTree)
	case panelLog:
		t.app.SetRoot(t.logView, true)
		t.app.SetFocus(t.logView)
	case panelInput:
		t.app.SetRoot(t.inputField, true)
		t.app.SetFocus(t.inputField)
	default:
		if t.searchMode {
			t.app.SetRoot(t.searchLayout, true)
			t.app.SetFocus(t.searchField)
		} else {
			t.app.SetRoot(t.normalLayout, true)
			t.setFocus(t.focusedPanel)
		}
	}
}

// openSearch switches to search mode, showing the search bar.
// Must be called from the tview event loop.
func (t *tuiApp) openSearch() {
	t.searchMode = true
	t.searchMatches = nil
	t.searchCurrentMatch = 0
	t.searchField.SetText("")
	t.app.SetRoot(t.searchLayout, true)
	t.app.SetFocus(t.searchField)
	t.updateStatusBar()
}

// closeSearch restores normal mode from search mode.
// Must be called from the tview event loop.
func (t *tuiApp) closeSearch() {
	t.searchMode = false
	t.searchMatches = nil
	// Restore the original log content before leaving search mode.
	agentID := t.currentSelectedAgent()
	t.logView.SetText(t.getLogText(agentID))
	t.logView.ScrollToEnd()
	t.app.SetRoot(t.normalLayout, true)
	t.setFocus(panelInput)
	t.updateStatusBar()
}

// updateSearch recomputes search matches for query and scrolls to the first one.
// Called from searchField SetChangedFunc (event loop).
func (t *tuiApp) updateSearch(query string) {
	agentID := t.currentSelectedAgent()
	rawText := t.getLogText(agentID)
	plainText := t.getLogPlainText(agentID)

	if query == "" {
		t.searchMatches = nil
		t.searchCurrentMatch = 0
		t.logView.SetText(rawText)
		// Restore to end (normal auto-scroll behavior).
		t.logView.ScrollToEnd()
		t.updateSearchStatusBar(query)
		return
	}
	plainLines := strings.Split(plainText, "\n")

	lq := strings.ToLower(query)
	var matches []int
	for i, line := range plainLines {
		if strings.Contains(strings.ToLower(line), lq) {
			matches = append(matches, i)
		}
	}
	t.searchMatches = matches
	t.searchCurrentMatch = 0
	t.logView.SetText(renderSearchLogText(t.getLogEntries(agentID), query))
	if len(matches) > 0 {
		t.scrollToPlainLine(matches[0], plainLines)
	}
	t.updateSearchStatusBar(query)
}

// scrollToPlainLine scrolls logView to the given raw-text line index,
// accounting for word-wrap so the rendered line offset is correct.
func (t *tuiApp) scrollToPlainLine(rawLine int, plainLines []string) {
	_, _, viewWidth, _ := t.logView.GetRect()
	// Subtract border (2) from box width to get usable content width.
	contentWidth := viewWidth - 2
	if contentWidth <= 0 {
		contentWidth = 80
	}
	rendered := 0
	for i := 0; i < rawLine && i < len(plainLines); i++ {
		lineLen := len([]rune(plainLines[i]))
		if lineLen == 0 {
			rendered++
		} else {
			rendered += (lineLen + contentWidth - 1) / contentWidth
		}
	}
	t.logView.ScrollTo(rendered, 0)
}

// nextSearchMatch advances to the next (dir=1) or previous (dir=-1) match.
func (t *tuiApp) nextSearchMatch(dir int) {
	if len(t.searchMatches) == 0 {
		return
	}
	t.searchCurrentMatch = (t.searchCurrentMatch + dir + len(t.searchMatches)) % len(t.searchMatches)
	plainText := t.getLogPlainText(t.currentSelectedAgent())
	plainLines := strings.Split(plainText, "\n")
	t.scrollToPlainLine(t.searchMatches[t.searchCurrentMatch], plainLines)
	t.updateSearchStatusBar(t.searchField.GetText())
}

func (t *tuiApp) getLogEntries(agentID string) []logEntry {
	t.logMu.Lock()
	defer t.logMu.Unlock()
	l, ok := t.logs[agentID]
	if !ok {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]logEntry, len(l.entries))
	copy(out, l.entries)
	return out
}

func renderSearchLogText(entries []logEntry, query string) string {
	if query == "" {
		var out strings.Builder
		for _, entry := range entries {
			out.WriteString("[")
			out.WriteString(entry.color)
			out.WriteString("]")
			out.WriteString(tview.Escape(entry.text))
			out.WriteString("[-::-]")
		}
		return out.String()
	}

	var out strings.Builder
	for _, entry := range entries {
		out.WriteString(renderHighlightedLogEntry(entry, query))
	}
	return out.String()
}

func renderHighlightedLogEntry(entry logEntry, query string) string {
	if query == "" {
		return "[" + entry.color + "]" + tview.Escape(entry.text) + "[-::-]"
	}

	// Convert to runes and fold the query once. unicode.ToLower maps each rune
	// to exactly one rune (unlike strings.ToLower which can expand byte length
	// for certain Unicode codepoints), so rune offsets are safe to use as slice
	// indices into the original text.
	textRunes := []rune(entry.text)
	queryRunes := []rune(query)
	if len(queryRunes) == 0 {
		return "[" + entry.color + "]" + tview.Escape(entry.text) + "[-::-]"
	}
	lowerQuery := make([]rune, len(queryRunes))
	for i, r := range queryRunes {
		lowerQuery[i] = unicode.ToLower(r)
	}
	qLen := len(lowerQuery)

	// highlightOpen sets fg=black, bg=yellow, bold.
	// highlightClose uses [-:-:-] (not [-::-]) so the background is explicitly
	// reset to default — an empty bg field in tview means "no change", which
	// would leave the yellow background bleeding into subsequent text.
	const highlightOpen = "[black:yellow:b]"
	const highlightClose = "[-:-:-]"
	// restoreColor re-applies the entry's own fg color after the highlight.
	restoreColor := "[" + entry.color + ":-:-]"

	var out strings.Builder
	out.WriteString("[")
	out.WriteString(entry.color)
	out.WriteString("]")

	start := 0
	tLen := len(textRunes)
	for start <= tLen-qLen {
		idx := -1
		for i := start; i <= tLen-qLen; i++ {
			match := true
			for j := 0; j < qLen; j++ {
				if unicode.ToLower(textRunes[i+j]) != lowerQuery[j] {
					match = false
					break
				}
			}
			if match {
				idx = i
				break
			}
		}
		if idx < 0 {
			break
		}
		out.WriteString(tview.Escape(string(textRunes[start:idx])))
		out.WriteString(highlightOpen)
		out.WriteString(tview.Escape(string(textRunes[idx : idx+qLen])))
		out.WriteString(highlightClose)
		out.WriteString(restoreColor)
		start = idx + qLen
	}
	out.WriteString(tview.Escape(string(textRunes[start:])))
	out.WriteString("[-:-:-]")
	return out.String()
}

// updateSearchStatusBar updates the status bar with current search match info.
func (t *tuiApp) updateSearchStatusBar(query string) {
	if !t.searchMode {
		return
	}
	n := len(t.searchMatches)
	if query == "" || n == 0 {
		if query != "" && n == 0 {
			t.statusBar.SetText(fmt.Sprintf(" [search] no matches for %q  |  Enter/Tab: next  Shift+Tab: prev  Esc: close", query))
		} else {
			t.statusBar.SetText(" [search] type to search  |  Enter/Tab: next  Shift+Tab: prev  Esc: close")
		}
		return
	}
	t.statusBar.SetText(fmt.Sprintf(" [search] match %d/%d  |  Enter/Tab: next  Shift+Tab: prev  Esc: close", t.searchCurrentMatch+1, n))
}

func (t *tuiApp) updateStatusBar() {
	text := " F1: agents | F2: input | F3: hide menu | F4: search | F12: focus mode | Ctrl+E: copy mode | /quit to exit"
	if t.menuHidden {
		text = " [menu hidden] F3: show menu | F2: input | F4: search | F12: focus mode | Ctrl+E: copy mode | /quit to exit"
	}
	if t.searchMode {
		t.updateSearchStatusBar(t.searchField.GetText())
		return
	}
	if t.focusMode {
		text = " [focus mode] ←→ agents/log | ↑↓ cycle | Tab cycle panels | M maximize | C cancel | Esc cancel"
	}
	if t.maximized != panelNone {
		text += " | [maximized — Esc to restore]"
	}
	if !t.mouseCapture {
		text += " | [copy mode — Ctrl+E to exit]"
	}
	t.statusBar.SetText(text)
}

// toggleMenuPanel hides or shows the agents panel (F4). Safe to call from the
// event loop — modifies topFlex proportions in-place; tview redraws on next frame.
func (t *tuiApp) toggleMenuPanel() {
	t.menuHidden = !t.menuHidden
	if t.menuHidden {
		// Hide: set proportion to 0 so agentTree takes no space.
		t.topFlex.ResizeItem(t.agentTree, 0, 0)
		// If agents panel was focused, move focus to log.
		if t.focusedPanel == panelAgents {
			t.setFocus(panelLog)
		}
	} else {
		// Show: restore proportion 1 (log keeps proportion 4).
		t.topFlex.ResizeItem(t.agentTree, 0, 1)
	}
}

func (t *tuiApp) setFocus(p panelID) {
	t.focusedPanel = p
	t.agentTree.SetBorderColor(t.theme.InactiveBorder)
	t.logView.SetBorderColor(t.theme.InactiveBorder)
	t.inputField.SetBorderColor(t.theme.InactiveBorder)
	switch p {
	case panelAgents:
		t.agentTree.SetBorderColor(t.theme.ActiveBorder)
		t.app.SetFocus(t.agentTree)
	case panelLog:
		t.logView.SetBorderColor(t.theme.ActiveBorder)
		t.app.SetFocus(t.logView)
	case panelInput:
		t.inputField.SetBorderColor(t.theme.ActiveBorder)
		t.app.SetFocus(t.inputField)
	}
}

func (t *tuiApp) cycleFocus(dir int) {
	panels := []panelID{panelAgents, panelLog, panelInput}
	idx := 0
	for i, p := range panels {
		if p == t.focusedPanel {
			idx = i
			break
		}
	}
	next := panels[(idx+len(panels)+dir)%len(panels)]
	t.setFocus(next)
}

func (t *tuiApp) toggleMaximize() {
	if t.maximized == t.focusedPanel {
		t.maximized = panelNone
	} else {
		t.maximized = t.focusedPanel
	}
	t.applyLayout()
}

func (t *tuiApp) getOrCreateLog(agentID string) *agentLog {
	if l, ok := t.logs[agentID]; ok {
		return l
	}
	l := &agentLog{atBottom: true}
	t.logs[agentID] = l
	return l
}

func (t *tuiApp) getLogText(agentID string) string {
	t.logMu.Lock()
	defer t.logMu.Unlock()
	if l, ok := t.logs[agentID]; ok {
		l.mu.Lock()
		defer l.mu.Unlock()
		return l.buf.String()
	}
	return ""
}

func (t *tuiApp) getLogPlainText(agentID string) string {
	t.logMu.Lock()
	defer t.logMu.Unlock()
	l, ok := t.logs[agentID]
	if !ok {
		return ""
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	var out strings.Builder
	for _, entry := range l.entries {
		out.WriteString(entry.text)
	}
	return out.String()
}

// writeLogBuf writes directly to the agent's log buffer without triggering a
// screen update. Safe to call before app.Run() has started.
func (t *tuiApp) writeLogBuf(agentID, text, color string) {
	if agentID == "" {
		agentID = rootAgentID
	}
	t.logMu.Lock()
	defer t.logMu.Unlock()
	l := t.getOrCreateLog(agentID)
	bg := colorName(t.theme.LogBg)
	l.mu.Lock()
	l.entries = append(l.entries, logEntry{color: color, text: text})
	l.buf.WriteString("[" + withBg(color, bg) + "]" + tview.Escape(text) + "[-:" + bg + ":-]")
	l.mu.Unlock()
}

func (t *tuiApp) getLogRef(agentID string) *agentLog {
	t.logMu.Lock()
	defer t.logMu.Unlock()
	return t.logs[agentID]
}

func (t *tuiApp) currentSelectedAgent() string {
	t.logMu.Lock()
	defer t.logMu.Unlock()
	if t.selectedAgent == "" {
		return tuiRootRef
	}
	return t.selectedAgent
}

func (t *tuiApp) setAgentAtBottom(agentID string, atBottom bool) {
	t.logMu.Lock()
	defer t.logMu.Unlock()
	l := t.getOrCreateLog(agentID)
	l.mu.Lock()
	l.atBottom = atBottom
	l.mu.Unlock()
	if atBottom {
		t.hasNewMessages[agentID] = false
	}
}

func (t *tuiApp) appendLog(agentID, text, color string) {
	if agentID == "" {
		agentID = rootAgentID
	}
	t.logMu.Lock()
	log := t.getOrCreateLog(agentID)
	log.mu.Lock()
	log.entries = append(log.entries, logEntry{color: color, text: text})
	bg := colorName(t.theme.LogBg)
	log.buf.WriteString("[" + withBg(color, bg) + "]" + tview.Escape(text) + "[-:" + bg + ":-]")
	atBottom := log.atBottom
	selected := t.selectedAgent
	if selected == "" {
		selected = tuiRootRef
	}
	if !atBottom {
		t.hasNewMessages[agentID] = true
	} else if selected != agentID {
		t.hasNewMessages[agentID] = true
	}
	log.mu.Unlock()
	t.logMu.Unlock()

	if selected == agentID && !t.searchMode {
		t.app.QueueUpdateDraw(func() {
			t.logView.SetText(t.getLogText(agentID))
			if atBottom {
				t.logView.ScrollToEnd()
			} else {
				t.logMu.Lock()
				t.hasNewMessages[agentID] = true
				t.logMu.Unlock()
			}
			t.updateMoreIndicator()
		})
	}
}

// selectAgent switches the log panel to show the given agent's logs.
// Must be called from within the event loop (event callbacks or queued updates).
// Do NOT call via QueueUpdateDraw — this function calls tview methods directly.
func (t *tuiApp) selectAgent(id string) {
	if id == "" {
		return
	}
	// Allow selecting the root container — it acts as a "create new agent" target.
	if id == tuiRootRef {
		t.logMu.Lock()
		t.selectedAgent = tuiRootRef
		t.logMu.Unlock()
		t.logView.SetText("[gray]Select an agent from the panel, or type a message here to start a new Agent.[-]")
		t.updateInputLabel(nil)
		t.updateMoreIndicator()
		return
	}
	t.logMu.Lock()
	t.selectedAgent = id
	if _, ok := t.hasNewMessages[id]; !ok {
		t.hasNewMessages[id] = false
	}
	t.logMu.Unlock()
	text := t.getLogText(id)
	// Direct tview calls — safe because we're always in event-loop context here.
	// During search mode, don't update logView at all — the search function
	// manages scroll position and we must not reset lineIndex via SetText.
	if !t.searchMode {
		t.logView.SetText(text)
		if l := t.getLogRef(id); l != nil {
			l.mu.Lock()
			atBottom := l.atBottom
			l.mu.Unlock()
			if atBottom {
				t.logView.ScrollToEnd()
			}
		}
	}
	t.updateMoreIndicator()
	t.updateInputLabel(t.getTopLevelAgent(id))
}

func (t *tuiApp) updateMoreIndicator() {
	t.logMu.Lock()
	selected := t.selectedAgent
	if selected == "" {
		selected = tuiRootRef
	}
	l := t.logs[selected]
	hasNew := t.hasNewMessages[selected]
	displayName := t.agentNames[selected]
	t.logMu.Unlock()
	if displayName == "" {
		if selected == tuiRootRef {
			displayName = "Agents"
		} else {
			displayName = selected
		}
	}
	// Check if the selected agent is busy.
	var busy bool
	if selected != tuiRootRef {
		if a := t.getTopLevelAgent(selected); a != nil {
			a.queueMu.Lock()
			busy = a.busy
			a.queueMu.Unlock()
		}
	}
	title := fmt.Sprintf(" %s ", displayName)
	if busy {
		title = fmt.Sprintf(" %s ⟳ ", displayName)
	}
	if l != nil {
		l.mu.Lock()
		atBottom := l.atBottom
		l.mu.Unlock()
		if !atBottom && hasNew {
			title = fmt.Sprintf(" %s [▼ new messages] ", displayName)
			if busy {
				title = fmt.Sprintf(" %s ⟳ [▼ new messages] ", displayName)
			}
		}
	}
	t.logView.SetTitle(title)
}

func (t *tuiApp) rebuildAgentTree(topLevel []*tuiAgent, subNodes []tuiAgentNode) {
	selStyle := tcell.StyleDefault.Foreground(t.theme.TreeSelectedFg).Background(t.theme.TreeSelectedBg)

	// Build subagent nodes map.
	subNodeMap := map[string]*tview.TreeNode{}
	subNames := map[string]string{}
	for _, n := range subNodes {
		label, color := t.agentNodeText(n)
		tn := tview.NewTreeNode(label)
		tn.SetReference(n.ID)
		tn.SetExpanded(true)
		tn.SetTextStyle(tcell.StyleDefault.Foreground(color))
		tn.SetSelectedTextStyle(selStyle)
		subNodeMap[n.ID] = tn
		subNames[n.ID] = agentDisplayName(n)
	}

	// Build top-level agent nodes.
	topNodeMap := map[string]*tview.TreeNode{}
	topNames := map[string]string{}
	for _, a := range topLevel {
		label, color := t.topLevelAgentNodeText(a)
		tn := tview.NewTreeNode(label)
		tn.SetReference(a.id)
		tn.SetExpanded(true)
		a.queueMu.Lock()
		busy := a.busy
		aName := a.Name
		a.queueMu.Unlock()
		style := tcell.StyleDefault.Foreground(color)
		if busy {
			style = style.Bold(true)
		}
		tn.SetTextStyle(style)
		tn.SetSelectedTextStyle(selStyle)
		topNodeMap[a.id] = tn
		topNames[a.id] = agentNodeLabel(a.num, aName)
	}

	// Publish merged names map (read under logMu in updateMoreIndicator).
	t.logMu.Lock()
	merged := make(map[string]string, len(topNames)+len(subNames))
	for k, v := range topNames {
		merged[k] = v
	}
	for k, v := range subNames {
		merged[k] = v
	}
	t.agentNames = merged
	t.logMu.Unlock()

	// Root container node (non-agent; not selectable as a conversation agent).
	rootNode := tview.NewTreeNode("Agents").
		SetReference(tuiRootRef).
		SetExpanded(true)
	rootNode.SetSelectedTextStyle(selStyle)

	// Wire top-level agents under root.
	for _, a := range topLevel {
		if tn, ok := topNodeMap[a.id]; ok {
			rootNode.AddChild(tn)
		}
	}

	// Wire subagents under their parent (top-level or another subagent).
	for _, n := range subNodes {
		tn := subNodeMap[n.ID]
		if tn == nil {
			continue
		}
		if parentTL, ok := topNodeMap[n.ParentID]; ok {
			parentTL.AddChild(tn)
		} else if parentSub, ok := subNodeMap[n.ParentID]; ok {
			parentSub.AddChild(tn)
		} else if len(topLevel) > 0 {
			if firstTL, ok := topNodeMap[topLevel[0].id]; ok {
				firstTL.AddChild(tn)
			} else {
				rootNode.AddChild(tn)
			}
		} else {
			rootNode.AddChild(tn)
		}
	}

	t.agentTree.SetRoot(rootNode)
	t.agentTree.SetTitle(fmt.Sprintf(" Agents (%d) ", len(topLevel)+len(subNodes)))

	// Re-select current agent node.
	selected := t.currentSelectedAgent()
	if selected == tuiRootRef {
		t.agentTree.SetCurrentNode(rootNode)
	} else if current, ok := topNodeMap[selected]; ok {
		t.agentTree.SetCurrentNode(current)
	} else if current, ok := subNodeMap[selected]; ok {
		t.agentTree.SetCurrentNode(current)
	} else if len(topLevel) > 0 {
		if firstTL, ok := topNodeMap[topLevel[0].id]; ok {
			t.agentTree.SetCurrentNode(firstTL)
		}
	}
}

// agentDisplayName returns the human-readable name for a subagent node
// (used for the log panel title). Falls back to question snippet then ID.
func agentDisplayName(n tuiAgentNode) string {
	name := strings.TrimSpace(n.Name)
	if name == "" {
		q := strings.TrimSpace(n.Question)
		if q != "" {
			runes := []rune(q)
			if len(runes) > 20 {
				return string(runes[:20]) + "…"
			}
			return q
		}
		return n.ID
	}
	return name
}

// agentNodeText returns the plain label text and display color for a subagent tree node.
// Use SetTextStyle instead of color tags to preserve selectedTextStyle.
func (t *tuiApp) agentNodeText(n tuiAgentNode) (string, tcell.Color) {
	var icon string
	var color tcell.Color
	switch n.Status {
	case subagentStatusRunning:
		icon, color = "⟳", t.theme.AgentBusy
	case subagentStatusCompleted:
		icon, color = "✓", t.theme.AgentCompleted
	case subagentStatusFailed, subagentStatusTimedOut:
		icon, color = "✗", t.theme.AgentFailed
	case subagentStatusCancelled:
		icon, color = "—", t.theme.AgentCancelled
	default:
		icon, color = "○", t.theme.AgentPending
	}
	name := strings.TrimSpace(n.Name)
	if name == "" {
		q := strings.TrimSpace(n.Question)
		if q != "" {
			runes := []rune(q)
			if len(runes) > 20 {
				name = string(runes[:20]) + "…"
			} else {
				name = q
			}
		} else {
			name = n.ID
		}
	}
	return icon + " " + name, color
}

// slashCommand describes a TUI slash command shown in autocomplete.
type slashCommand struct {
	name         string
	description  string
	topLevelOnly bool // only shown/accepted when a top-level agent is selected
}

var slashCommands = []slashCommand{
	{name: "/quit", description: "Exit the TUI"},
	{name: "/exit", description: "Exit the TUI"},
	{name: "/new", description: "Create a new top-level agent"},
	{name: "/session-new", description: "Create a new top-level agent (alias of /new)"},
	{name: "/session-abandon", description: "Remove current agent from the session list without deleting history"},
	{name: "/session-destroy", description: "Remove current agent and permanently delete its session file from disk"},
	{name: "/session-cancel", description: "Cancel the currently running agent"},
	{name: "/session-resume", description: "Open session picker or resume by UUID: /session-resume <uuid-prefix>"},
	{name: "/session-fork", description: "Fork agent history into new agent: /session-fork [full|last|summary] [msg]  (no args → interactive mode selector)", topLevelOnly: true},
	{name: "/workspace", description: "Workspace management: /workspace (picker), /workspace <name> (load)"},
	{name: "/workspace-new", description: "Create a new empty workspace"},
	{name: "/workspace-save", description: "Save current workspace: /workspace-save <name>"},
	{name: "/reset", description: "Reset current agent conversation to initial state", topLevelOnly: true},
	{name: "/compact", description: "Summarize conversation to reduce context size", topLevelOnly: true},
	{name: "/save", description: "Save current log to file: /save <filename>"},
	{name: "/append-to-agent", description: "Append context from this agent to another: /append-to-agent [full|last|summary] <id> [extra text…]  (no args → interactive selector)", topLevelOnly: true},
	{name: "/help", description: "Show available commands"},
}

// handleSlashCommand processes a slash command. Must be called from a goroutine.
func (t *tuiApp) handleSlashCommand(text string) {
	parts := strings.Fields(text)
	if len(parts) == 0 {
		return
	}
	cmd := strings.ToLower(parts[0])
	agentID := t.currentSelectedAgent()

	switch cmd {
	case "/quit", "/exit":
		t.app.Stop()

	case "/new", "/session-new":
		query := strings.TrimSpace(strings.TrimPrefix(text, parts[0]))
		agent := t.createTopLevelAgent(t.sysPrompt)
		if query != "" {
			agent.queueMu.Lock()
			agent.busy = true
			agent.queueMu.Unlock()
		}
		t.app.QueueUpdateDraw(func() {
			t.selectAgent(agent.id)
		})
		if query != "" {
			t.submitInputForAgent(agent, query)
		}

	case "/session-abandon":
		t.handleSessionAbandon(agentID)

	case "/session-destroy":
		t.handleSessionDestroy(agentID)

	case "/session-cancel":
		if err := t.cancelAgent(agentID); err != nil {
			t.appendLog(agentID, "[capelin-go] /session-cancel: "+err.Error()+"\n", t.theme.LogError)
		} else {
			t.appendLog(agentID, "[capelin-go] session canceled\n", t.theme.LogSystem)
		}

	case "/session-resume":
		// /session-resume              → show interactive session picker
		// /session-resume <uuid-prefix> → directly resume a session by UUID prefix
		if len(parts) >= 2 {
			go t.handleSessionResume(agentID, parts[1])
		} else {
			go t.handleSessionResume(agentID, "")
		}

	case "/workspace-new":
		go t.handleWorkspaceNew(agentID)

	case "/workspace-save":
		if len(parts) < 2 {
			t.appendLog(agentID, "[capelin-go] Usage: /workspace-save <name>\n", t.theme.LogError)
			return
		}
		go t.handleWorkspaceSave(agentID, parts[1])

	case "/workspace":
		// /workspace        → show interactive workspace picker
		// /workspace <name> → load named workspace (replaces current)
		if len(parts) >= 2 {
			go t.handleWorkspaceLoad(agentID, parts[1])
		} else {
			go t.handleWorkspacePicker(agentID)
		}

	case "/session-fork":
		agent := t.getTopLevelAgent(agentID)
		if agent == nil {
			t.appendLog(agentID, "[capelin-go] /session-fork is only available when a top-level agent is selected\n", t.theme.LogError)
			return
		}
		// No arguments → open interactive mode selector.
		if len(parts) == 1 {
			t.app.QueueUpdateDraw(func() {
				t.showSessionForkCascade(agent)
			})
			return
		}
		// Syntax: /session-fork [full|last|summary] [msg]
		mode := "full"
		rest := strings.TrimSpace(strings.TrimPrefix(text, parts[0]))
		if len(parts) >= 2 {
			switch strings.ToLower(parts[1]) {
			case "last", "summary":
				mode = strings.ToLower(parts[1])
				rest = strings.TrimSpace(strings.TrimPrefix(rest, parts[1]))
			case "full":
				rest = strings.TrimSpace(strings.TrimPrefix(rest, parts[1]))
			}
		}
		go t.handleSessionFork(agent, rest, mode)

	case "/reset":
		agent := t.getTopLevelAgent(agentID)
		if agent == nil {
			t.appendLog(agentID, "[capelin-go] /reset is only available when a top-level agent is selected\n", t.theme.LogError)
			return
		}
		t.resetAgentSession(agent)

	case "/compact":
		agent := t.getTopLevelAgent(agentID)
		if agent == nil {
			t.appendLog(agentID, "[capelin-go] /compact is only available when a top-level agent is selected\n", t.theme.LogError)
			return
		}
		t.compactAgentSession(agent)

	case "/save":
		if len(parts) < 2 {
			t.appendLog(agentID, "[capelin-go] Usage: /save <filename>\n", t.theme.LogError)
			return
		}
		filename := parts[1]
		go t.saveLog(agentID, filename)

	case "/append-to-agent":
		srcAgent := t.getTopLevelAgent(agentID)
		if srcAgent == nil {
			t.appendLog(agentID, "[capelin-go] /append-to-agent requires a top-level agent\n", t.theme.LogError)
			return
		}
		// No arguments → open interactive mode + agent selector.
		if len(parts) == 1 {
			t.app.QueueUpdateDraw(func() {
				t.showAppendToAgentCascade(srcAgent)
			})
			return
		}
		// Syntax: /append-to-agent [full|last|summary] <id> [extra text…]
		// mode is optional; default is "last".
		tokens := parts[1:]
		mode := "last"
		if len(tokens) > 0 {
			switch strings.ToLower(tokens[0]) {
			case "full", "last", "summary":
				mode = strings.ToLower(tokens[0])
				tokens = tokens[1:]
			}
		}
		if len(tokens) == 0 {
			t.appendLog(agentID, "[capelin-go] Usage: /append-to-agent [full|last|summary] <id> [extra text…]\n", t.theme.LogError)
			return
		}
		targetRef := tokens[0]
		extraText := strings.Join(tokens[1:], " ")
		dstAgent := t.findAgentByRef(targetRef)
		if dstAgent == nil {
			t.appendLog(agentID, fmt.Sprintf("[capelin-go] /append-to-agent: agent %q not found\n", targetRef), t.theme.LogError)
			return
		}
		if dstAgent.id == srcAgent.id {
			t.appendLog(agentID, "[capelin-go] /append-to-agent: source and destination are the same agent\n", t.theme.LogError)
			return
		}
		go t.handleAppendToAgent(srcAgent, dstAgent, mode, extraText)

	case "/help":
		var sb strings.Builder
		sb.WriteString("[yellow]Available commands:[-]\n")
		for _, c := range slashCommands {
			sb.WriteString(fmt.Sprintf("  [cyan]%-8s[-]  %s", c.name, c.description))
			if c.topLevelOnly {
				sb.WriteString(" [gray](top-level agent only)[-]")
			}
			sb.WriteString("\n")
		}
		t.appendLog(agentID, sb.String(), t.theme.LogSystem)

	default:
		t.appendLog(agentID, fmt.Sprintf("[capelin-go] Unknown command: %s (type /help for list)\n", cmd), t.theme.LogError)
	}
}

// saveLog strips color tags from the given agent's log and writes it to filename.
func (t *tuiApp) saveLog(agentID, filename string) {
	raw := t.getLogText(agentID)
	plain := stripColorTags(raw)
	err := os.WriteFile(filename, []byte(plain), 0644)
	if err != nil {
		t.appendLog(agentID, fmt.Sprintf("[capelin-go] save failed: %s\n", err.Error()), t.theme.LogError)
		return
	}
	t.appendLog(agentID, fmt.Sprintf("[capelin-go] log saved to %s\n", filename), t.theme.LogSystem)
}

// showSkillPicker opens a modal overlay listing available skills.
// The user can navigate the list and press Enter to select a skill; a bare
// %% in the current input text is replaced with %%<skillname>.
// Pressing Esc dismisses the picker and removes the %% trigger.
// Must be called from the tview event loop (not via QueueUpdateDraw).
func (t *tuiApp) showSkillPicker() {
	t.skillPickerActive = true

	// Build sorted skill list.
	names := make([]string, 0, len(t.skills))
	for name := range t.skills {
		names = append(names, name)
	}
	sort.Strings(names)

	if len(names) == 0 {
		// No skills loaded — remove %% and inform.
		cur := t.inputField.GetText()
		t.inputField.SetText(strings.Replace(cur, "%%", "", 1))
		t.skillPickerActive = false
		go t.appendLog(t.currentSelectedAgent(), "[capelin-go] No skills available\n", t.theme.LogSystem)
		return
	}

	list := tview.NewList()
	list.ShowSecondaryText(true)
	for _, name := range names {
		sk := t.skills[name]
		desc := sk.Description
		if len([]rune(desc)) > 70 {
			desc = string([]rune(desc)[:70]) + "…"
		}
		list.AddItem(name, desc, 0, nil)
	}
	list.SetBorder(true)
	list.SetTitle(" Select Skill  [Esc] cancel ")
	list.SetBorderColor(t.theme.ActiveBorder)

	dismiss := func() {
		cur := t.inputField.GetText()
		t.inputField.SetText(removeFirstBareSkillTrigger(cur))
		t.app.SetRoot(t.normalLayout, true)
		t.skillPickerActive = false
		t.setFocus(panelInput)
	}

	list.SetSelectedFunc(func(_ int, name, _ string, _ rune) {
		cur := t.inputField.GetText()
		t.inputField.SetText(replaceFirstBareSkillTrigger(cur, "%%"+name+"%%"))
		t.app.SetRoot(t.normalLayout, true)
		t.skillPickerActive = false
		t.setFocus(panelInput)
	})
	list.SetDoneFunc(func() { dismiss() })

	// Height: list items + border rows, minimum 14 (≥10 visible), maximum 24.
	h := len(names) + 4
	if h < 14 {
		h = 14
	}
	if h > 24 {
		h = 24
	}
	overlay := tview.NewFlex().
		AddItem(nil, 0, 1, false).
		AddItem(tview.NewFlex().SetDirection(tview.FlexRow).
			AddItem(nil, 0, 1, false).
			AddItem(list, h, 0, true).
			AddItem(nil, 0, 1, false), 60, 0, true).
		AddItem(nil, 0, 1, false)

	t.app.SetRoot(overlay, true).SetFocus(list)
}

// findAgentByRef looks up a top-level agent by numeric ID (e.g. "1", "2")
// or by its full agent ID string (e.g. "agent-1").
func (t *tuiApp) findAgentByRef(ref string) *tuiAgent {
	t.agentsMu.Lock()
	defer t.agentsMu.Unlock()
	// Direct ID match.
	if a, ok := t.topAgents[ref]; ok {
		return a
	}
	// Numeric shorthand: "1" -> "agent-1".
	if n, err := strconv.Atoi(ref); err == nil {
		id := fmt.Sprintf("agent-%d", n)
		if a, ok := t.topAgents[id]; ok {
			return a
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Cascade menu component
// ---------------------------------------------------------------------------

// cascadeMenuItem is one selectable item within a cascade step.
type cascadeMenuItem struct {
	label string // primary text shown in list
	desc  string // secondary (dimmed) text
	value string // returned to onDone
}

// cascadeStep describes one step (one list screen) in a cascade sequence.
// dynamicItems, if non-nil, is called right before the step is displayed
// so that the item list can reflect live state (e.g. current agents).
type cascadeStep struct {
	title        string
	items        []cascadeMenuItem
	dynamicItems func() []cascadeMenuItem
}

// showCascadeMenu opens a sequence of modal list pickers, one per step.
// It collects one value per step and calls onDone(values) when all steps
// are completed, or onCancel if the user presses Esc at any step.
// Must be called from the tview event loop (not via QueueUpdateDraw).
func (t *tuiApp) showCascadeMenu(steps []cascadeStep, onDone func([]string), onCancel func()) {
	collected := make([]string, 0, len(steps))

	var showStep func(idx int)
	showStep = func(idx int) {
		if idx >= len(steps) {
			onDone(collected)
			return
		}
		step := steps[idx]
		items := step.items
		if step.dynamicItems != nil {
			items = step.dynamicItems()
		}

		list := tview.NewList()
		list.ShowSecondaryText(len(items) > 0 && items[0].desc != "")
		for _, it := range items {
			it := it
			list.AddItem(it.label, it.desc, 0, nil)
		}
		list.SetBorder(true)
		list.SetTitle(fmt.Sprintf(" %s  [Esc] cancel ", step.title))
		list.SetBorderColor(t.theme.ActiveBorder)

		list.SetSelectedFunc(func(i int, _, _ string, _ rune) {
			if i >= len(items) {
				return
			}
			collected = append(collected, items[i].value)
			t.app.SetRoot(t.normalLayout, true)
			showStep(idx + 1)
		})
		list.SetDoneFunc(func() {
			t.app.SetRoot(t.normalLayout, true)
			t.setFocus(panelInput)
			onCancel()
		})

		h := len(items) + 4
		if h < 14 {
			h = 14
		}
		if h > 24 {
			h = 24
		}
		overlay := tview.NewFlex().
			AddItem(nil, 0, 1, false).
			AddItem(tview.NewFlex().SetDirection(tview.FlexRow).
				AddItem(nil, 0, 1, false).
				AddItem(list, h, 0, true).
				AddItem(nil, 0, 1, false), 0, 3, true).
			AddItem(nil, 0, 1, false)
		t.app.SetRoot(overlay, true).SetFocus(list)
	}
	showStep(0)
}

// modeItems returns the standard mode cascade items used by session-fork and append-to-agent.
var modeItems = []cascadeMenuItem{
	{label: "last", desc: "last assistant message only (smallest context)", value: "last"},
	{label: "full", desc: "copy full conversation history", value: "full"},
	{label: "summary", desc: "LLM-summarized history", value: "summary"},
}

// showSessionForkCascade opens an interactive mode selector for /session-fork.
// Must be called from the tview event loop.
func (t *tuiApp) showSessionForkCascade(src *tuiAgent) {
	steps := []cascadeStep{
		{title: "Select fork mode", items: modeItems},
	}
	t.showCascadeMenu(steps, func(sel []string) {
		if len(sel) < 1 {
			return
		}
		t.inputField.SetText("")
		t.setFocus(panelInput)
		go t.handleSessionFork(src, "", sel[0])
	}, func() {
		t.setFocus(panelInput)
	})
}

// showAppendToAgentCascade opens an interactive mode + target-agent selector for
// /append-to-agent.  Must be called from the tview event loop.
func (t *tuiApp) showAppendToAgentCascade(src *tuiAgent) {
	steps := []cascadeStep{
		{title: "Select context mode", items: modeItems},
		{
			title: "Select target agent",
			dynamicItems: func() []cascadeMenuItem {
				t.agentsMu.Lock()
				defer t.agentsMu.Unlock()
				var items []cascadeMenuItem
				for _, a := range t.topAgents {
					if a.id == src.id {
						continue // skip self
					}
					a.queueMu.Lock()
					name := a.Name
					busy := a.busy
					a.queueMu.Unlock()
					label := agentNodeLabel(a.num, name)
					desc := "idle"
					if busy {
						desc = "busy"
					}
					items = append(items, cascadeMenuItem{
						label: label,
						desc:  desc,
						value: a.id,
					})
				}
				sort.Slice(items, func(i, j int) bool { return items[i].label < items[j].label })
				return items
			},
		},
	}
	t.showCascadeMenu(steps, func(sel []string) {
		if len(sel) < 2 {
			return
		}
		mode := sel[0]
		dstID := sel[1]
		dst := t.getTopLevelAgent(dstID)
		if dst == nil {
			go t.appendLog(src.id, "[capelin-go] /append-to-agent: target agent no longer exists\n", t.theme.LogError)
			return
		}
		// Pre-fill input so user can append optional extra text before submitting.
		prefill := fmt.Sprintf("/append-to-agent %s %s ", mode, dst.id)
		t.inputField.SetText(prefill)
		t.setFocus(panelInput)
	}, func() {
		t.inputField.SetText("")
		t.setFocus(panelInput)
	})
}

// handleAppendToAgent copies context from src to dst as a user message.
//   - mode "full":    formats the entire src conversation and submits to dst.
//   - mode "last":    takes only the last assistant message content from src.
//   - mode "summary": calls the LLM to compact src conversation, sends the summary.
//
// extraText (may be empty) is appended after the context block.
// Must be called from a goroutine (not the event loop).
func (t *tuiApp) handleAppendToAgent(src, dst *tuiAgent, mode, extraText string) {
	src.msgMu.Lock()
	msgs := make([]apiMessage, len(src.messages))
	copy(msgs, src.messages)
	rt := src.runtime
	src.msgMu.Unlock()

	src.queueMu.Lock()
	srcName := src.Name
	src.queueMu.Unlock()
	srcLabel := agentNodeLabel(src.num, srcName)

	var content string
	switch mode {
	case "full":
		var sb strings.Builder
		sb.WriteString(fmt.Sprintf("[Context from %s]\n", srcLabel))
		for _, m := range msgs {
			switch m.Role {
			case "user":
				if m.Content != "" {
					sb.WriteString("User: " + m.Content + "\n")
				}
			case "assistant":
				if m.Content != "" {
					sb.WriteString("Assistant: " + m.Content + "\n")
				}
			}
		}
		content = sb.String()
	case "last":
		for i := len(msgs) - 1; i >= 0; i-- {
			if msgs[i].Role == "assistant" && strings.TrimSpace(msgs[i].Content) != "" {
				content = fmt.Sprintf("[From %s]\n%s", srcLabel, msgs[i].Content)
				break
			}
		}
		if content == "" {
			t.appendLog(src.id, "[capelin-go] /append-to-agent: no assistant message found in this agent\n", t.theme.LogError)
			return
		}
	case "summary":
		compactMsgs := append(msgs, apiMessage{Role: "user", Content: compactPrompt})
		ctx := t.ctx
		if ctx == nil {
			ctx = context.Background()
		}
		_, summary, err := t.owner.runTurnLoop(ctx, compactMsgs, compactPrompt, rt, t.owner.toolset, false)
		if err != nil {
			if ctx.Err() == nil {
				t.appendLog(src.id, "[capelin-go] /append-to-agent: summary failed: "+err.Error()+"\n", t.theme.LogError)
			}
			return
		}
		content = fmt.Sprintf("[Summary from %s]\n%s", srcLabel, summary)
	}

	if extraText != "" {
		content += "\n" + extraText
	}

	dst.queueMu.Lock()
	if !dst.busy {
		dst.busy = true
		dst.queueMu.Unlock()
		t.app.QueueUpdateDraw(func() {
			t.updateInputLabel(dst)
		})
		t.submitInputForAgent(dst, content)
	} else {
		dst.inputQueue = append(dst.inputQueue, content)
		dst.queueMu.Unlock()
	}

	dst.queueMu.Lock()
	dstName := dst.Name
	dst.queueMu.Unlock()
	dstLabel := agentNodeLabel(dst.num, dstName)
	t.appendLog(src.id, fmt.Sprintf("[capelin-go] Appended %s context to %s\n", mode, dstLabel), t.theme.LogSystem)
}

// generateAgentName calls the LLM to generate a concise 3-6 word title for the
// agent based on the first user message. Runs in a goroutine. Sets agent.Name
// (under queueMu), updates agentNames (under logMu), refreshes the UI, and
// re-saves the session snapshot with the new name.
func (t *tuiApp) generateAgentName(agent *tuiAgent, firstUserMsg string) {
	if t.owner.client == nil || firstUserMsg == "" {
		return
	}
	runes := []rune(firstUserMsg)
	if len(runes) > 300 {
		runes = runes[:300]
	}
	prompt := []apiMessage{
		{Role: "system", Content: "Generate a concise 3-6 word title for this task. Output ONLY the title in lowercase, no punctuation, no quotes."},
		{Role: "user", Content: string(runes)},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	resp, err := t.owner.client.complete(ctx, prompt, nil, t.owner.client.model, "")
	if err != nil || resp == nil {
		return
	}
	name := strings.TrimSpace(resp.Content())
	if name == "" {
		return
	}
	nameRunes := []rune(name)
	if len(nameRunes) > 40 {
		nameRunes = nameRunes[:40]
	}
	name = string(nameRunes)

	agent.queueMu.Lock()
	agent.Name = name
	agent.queueMu.Unlock()

	label := agentNodeLabel(agent.num, name)
	t.logMu.Lock()
	t.agentNames[agent.id] = label
	t.logMu.Unlock()

	// Refresh the input label and more indicator if this agent is currently selected.
	t.app.QueueUpdateDraw(func() {
		if t.currentSelectedAgent() == agent.id {
			t.updateInputLabel(agent)
			t.updateMoreIndicator()
		}
	})

	// Re-save the session snapshot so /attach shows the name.
	if t.owner.cfg.workspaceRoot != "" {
		agent.msgMu.Lock()
		snapMsgs := make([]apiMessage, len(agent.messages))
		copy(snapMsgs, agent.messages)
		agent.msgMu.Unlock()
		go func() {
			_ = saveSessionMessages(t.owner.cfg.workspaceRoot, agent.sessionUUID, name, snapMsgs, agent.createdAt)
		}()
	}
}

// resetAgentSession clears the agent's conversation back to the system prompt and wipes its log.
// Safe to call from goroutines.
func (t *tuiApp) resetAgentSession(agent *tuiAgent) {
	sysPrompt := t.owner.systemPromptWithSkills()
	agent.msgMu.Lock()
	agent.messages = []apiMessage{{Role: "system", Content: sysPrompt}}
	agent.msgMu.Unlock()
	t.logMu.Lock()
	if l, ok := t.logs[agent.id]; ok {
		l.mu.Lock()
		l.buf.Reset()
		l.entries = l.entries[:0]
		l.atBottom = true
		l.mu.Unlock()
	}
	t.hasNewMessages[agent.id] = false
	t.logMu.Unlock()
	agent.queueMu.Lock()
	agent.inputQueue = agent.inputQueue[:0]
	agent.queueMu.Unlock()
	t.writeLogBuf(agent.id, "[System Prompt]\n"+sysPrompt+"\n\n", t.theme.LogSystem)
	t.appendLog(agent.id, "[capelin-go] New session started\n", t.theme.LogSystem)
}

const compactPrompt = "Please provide a compact summary of our conversation so far. " +
	"Preserve all key context, decisions, outcomes, and any important details. " +
	"The summary will replace the full conversation history to reduce context size."

// compactAgentSession summarizes the agent's conversation via an LLM call and
// replaces the history with the summary. Must be called from a goroutine.
func (t *tuiApp) compactAgentSession(agent *tuiAgent) {
	agent.queueMu.Lock()
	if agent.busy {
		agent.queueMu.Unlock()
		t.appendLog(agent.id, "[capelin-go] Cannot compact while busy; try again after the current request completes\n", t.theme.LogError)
		return
	}
	agent.busy = true
	agent.queueMu.Unlock()
	t.app.QueueUpdateDraw(func() {
		if t.currentSelectedAgent() == agent.id {
			t.inputField.SetLabel("> [compacting...] ")
		}
	})

	agent.msgMu.Lock()
	msgs := make([]apiMessage, len(agent.messages))
	copy(msgs, agent.messages)
	rt := agent.runtime
	agent.msgMu.Unlock()

	compactMsgs := append(msgs, apiMessage{Role: "user", Content: compactPrompt})
	ctx := t.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	_, summary, err := t.owner.runTurnLoop(ctx, compactMsgs, compactPrompt, rt, t.owner.toolset, false)
	if err != nil {
		if ctx.Err() == nil {
			t.appendLog(agent.id, "[capelin-go] compact failed: "+err.Error()+"\n", t.theme.LogError)
		}
		t.processQueueForAgent(agent)
		return
	}
	sysPrompt := t.owner.systemPromptWithSkills()
	newMsgs := []apiMessage{
		{Role: "system", Content: sysPrompt},
		{Role: "assistant", Content: "[Conversation summary]\n" + summary},
	}
	agent.msgMu.Lock()
	agent.messages = newMsgs
	agent.msgMu.Unlock()
	t.appendLog(agent.id, "[capelin-go] Conversation compacted\n", t.theme.LogSystem)
	t.processQueueForAgent(agent)
}

// handleSessionResume handles /session-resume [uuid-prefix].
// uuidPrefix="" shows the interactive picker; otherwise it tries to directly
// resume the session whose UUID starts with uuidPrefix.
// Must be called from a goroutine.
func (t *tuiApp) handleSessionResume(callerAgentID string, uuidPrefix string) {
	snapshots, err := listSessionSnapshots(t.owner.cfg.workspaceRoot)
	if err != nil {
		t.appendLog(callerAgentID, "[capelin-go] /session-resume: error listing sessions: "+err.Error()+"\n", t.theme.LogError)
		return
	}

	// Filter out sessions that are already open.
	t.agentsMu.Lock()
	openUUIDs := make(map[string]bool, len(t.topAgents))
	for _, a := range t.topAgents {
		openUUIDs[a.sessionUUID] = true
	}
	t.agentsMu.Unlock()

	var available []sessionSnapshot
	for _, s := range snapshots {
		if !openUUIDs[s.SessionUUID] {
			available = append(available, s)
		}
	}

	// Direct UUID-prefix match — no popup needed.
	if uuidPrefix != "" {
		lp := strings.ToLower(uuidPrefix)
		var matched *sessionSnapshot
		for i := range available {
			if strings.HasPrefix(strings.ToLower(available[i].SessionUUID), lp) {
				if matched != nil {
					t.appendLog(callerAgentID, fmt.Sprintf("[capelin-go] /session-resume: ambiguous prefix %q — multiple sessions match\n", uuidPrefix), t.theme.LogError)
					return
				}
				s := available[i]
				matched = &s
			}
		}
		if matched == nil {
			t.appendLog(callerAgentID, fmt.Sprintf("[capelin-go] /session-resume: no session found for prefix %q\n", uuidPrefix), t.theme.LogError)
			return
		}
		t.resumeSession(callerAgentID, *matched)
		return
	}

	if len(available) == 0 {
		t.appendLog(callerAgentID, "[capelin-go] /session-resume: no prior sessions found (or all already open)\n", t.theme.LogSystem)
		return
	}

	// Build session label and preview helpers.
	sessionLabel := func(s sessionSnapshot) string {
		name := s.Name
		if name == "" {
			name = "(unnamed)"
		}
		return fmt.Sprintf("%s  %s  %d msgs  %s",
			s.SessionUUID[:8],
			name,
			s.MessageCount,
			s.UpdatedAt.Local().Format("2006-01-02 15:04"),
		)
	}
	sessionPreview := func(s sessionSnapshot) string {
		preview := s.LastContent
		if len([]rune(preview)) > 120 {
			preview = string([]rune(preview)[:120]) + "…"
		}
		return preview
	}

	// Build the list from a filtered snapshot slice.
	list := tview.NewList()
	list.SetBorder(true).SetTitle(" Resume Session (Enter to open, Esc to cancel) ")

	var selectedIdx int
	var filtered []sessionSnapshot

	populateList := func(filter string) {
		list.Clear()
		lower := strings.ToLower(filter)
		filtered = nil
		for _, s := range available {
			if lower == "" ||
				strings.Contains(strings.ToLower(s.SessionUUID), lower) ||
				strings.Contains(strings.ToLower(s.Name), lower) ||
				strings.Contains(strings.ToLower(s.LastContent), lower) {
				list.AddItem(sessionLabel(s), sessionPreview(s), 0, nil)
				filtered = append(filtered, s)
			}
		}
		n := len(filtered)
		if selectedIdx >= n {
			selectedIdx = n - 1
		}
		if selectedIdx < 0 && n > 0 {
			selectedIdx = 0
		}
		if n > 0 {
			list.SetCurrentItem(selectedIdx)
		}
	}

	// Initial population with no filter.
	populateList("")

	// dismissOverlay restores the normal layout. Called from event-loop callbacks
	// so we call tview methods directly — no QueueUpdateDraw.
	dismissOverlay := func() {
		t.app.SetRoot(t.normalLayout, true).SetFocus(t.inputField)
	}

	// Filter input field above the list (always keeps focus).
	filterInput := tview.NewInputField()
	filterInput.SetPlaceholder("Type to filter by UUID, name, or content...")
	filterInput.SetLabel("/")
	filterInput.SetChangedFunc(func(text string) {
		selectedIdx = 0
		populateList(text)
	})
	// All keyboard navigation stays in the filter input; the list is visual only.
	filterInput.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		switch event.Key() {
		case tcell.KeyDown:
			if selectedIdx < len(filtered)-1 {
				selectedIdx++
				list.SetCurrentItem(selectedIdx)
			}
			return nil
		case tcell.KeyUp:
			if selectedIdx > 0 {
				selectedIdx--
				list.SetCurrentItem(selectedIdx)
			}
			return nil
		case tcell.KeyEnter:
			if selectedIdx >= 0 && selectedIdx < len(filtered) {
				snap := filtered[selectedIdx]
				dismissOverlay()
				go t.resumeSession(callerAgentID, snap)
			}
			return nil
		case tcell.KeyEscape:
			dismissOverlay()
			return nil
		}
		return event
	})

	// Safety nets for mouse clicks on the list (they get focus via mouse).
	list.SetDoneFunc(func() { dismissOverlay() })
	list.SetSelectedFunc(func(index int, _ string, _ string, _ rune) {
		if index >= 0 && index < len(filtered) {
			snap := filtered[index]
			dismissOverlay()
			go t.resumeSession(callerAgentID, snap)
		}
	})

	// Fixed height: 3 rows for filter input + 2 rows per item + 2 border rows,
	// minimum 24 so at least ~10 sessions are visible, maximum 40.
	h := len(available)*2 + 4 + 1
	if h < 24 {
		h = 24
	}
	if h > 40 {
		h = 40
	}
	// Center the list in a flex overlay.
	overlay := tview.NewFlex().
		AddItem(nil, 0, 1, false).
		AddItem(tview.NewFlex().SetDirection(tview.FlexRow).
			AddItem(nil, 0, 1, false).
			AddItem(filterInput, 1, 0, true).
			AddItem(list, h, 0, false).
			AddItem(nil, 0, 1, false), 0, 3, true).
		AddItem(nil, 0, 1, false)

	// We're in a goroutine — use QueueUpdateDraw to show the overlay.
	t.app.QueueUpdateDraw(func() {
		t.inputField.SetText("")
		t.app.SetRoot(overlay, true).SetFocus(filterInput)
	})
}

// resumeSession loads a saved session snapshot and opens it as a new top-level agent.
// Must be called from a goroutine.
func (t *tuiApp) resumeSession(callerAgentID string, snap sessionSnapshot) {
	msgs, err := loadSessionMessages(t.owner.cfg.workspaceRoot, snap.SessionUUID)
	if err != nil {
		t.appendLog(callerAgentID, "[capelin-go] /session: failed to load session: "+err.Error()+"\n", t.theme.LogError)
		return
	}
	newAgent := t.createTopLevelAgentWithMessages(msgs, false)
	// Restore the session UUID, creation time, and name so further saves update the same file.
	newAgent.sessionUUID = snap.SessionUUID
	newAgent.createdAt = snap.CreatedAt
	if snap.Name != "" {
		newAgent.queueMu.Lock()
		newAgent.Name = snap.Name
		newAgent.queueMu.Unlock()
		label := agentNodeLabel(newAgent.num, snap.Name)
		t.logMu.Lock()
		t.agentNames[newAgent.id] = label
		t.logMu.Unlock()
	}
	// Replay the loaded conversation into the log buffer before showing.
	t.replayMessagesToLogBuf(newAgent.id, msgs)
	t.writeLogBuf(newAgent.id,
		fmt.Sprintf("\n--- Session resumed: %s (%d messages) ---\n\n", snap.SessionUUID[:8], len(msgs)),
		t.theme.LogSystem)
	// selectAgent calls tview methods — must be queued from a goroutine.
	t.app.QueueUpdateDraw(func() {
		t.selectAgent(newAgent.id)
	})
}

// cancelAgent attempts to cancel the currently running agent (top-level or subagent).
// Returns nil if cancellation was triggered, or an error describing why it could not.
func (t *tuiApp) cancelAgent(agentID string) error {
	if agentID == tuiRootRef {
		return fmt.Errorf("no agent selected")
	}
	if agent := t.getTopLevelAgent(agentID); agent != nil {
		agent.cancelMu.Lock()
		c := agent.cancel
		agent.cancelMu.Unlock()
		if c == nil {
			return fmt.Errorf("agent is not currently running")
		}
		c()
		return nil
	}
	if agentID != tuiRootRef && t.subagents != nil {
		if t.subagents.CancelSessionByID(agentID) {
			return nil
		}
	}
	return fmt.Errorf("is only available when a top-level agent is selected")
}

// handleSessionAbandon removes the current top-level agent from the menu list.
// The session JSON on disk is untouched and can be resumed later with /session-resume.
// Must be called from a goroutine.
func (t *tuiApp) handleSessionAbandon(agentID string) {
	if agentID == tuiRootRef {
		t.appendLog(agentID, "[capelin-go] /session-abandon: no agent selected\n", t.theme.LogError)
		return
	}
	agent := t.getTopLevelAgent(agentID)
	if agent == nil {
		t.appendLog(agentID, "[capelin-go] /session-abandon is only available when a top-level agent is selected\n", t.theme.LogError)
		return
	}

	// Cancel any in-flight request.
	agent.cancelMu.Lock()
	cancel := agent.cancel
	agent.cancelMu.Unlock()
	if cancel != nil {
		cancel()
	}

	// Remove from maps so the tree refresh no longer shows it.
	t.agentsMu.Lock()
	delete(t.topAgents, agentID)
	t.agentsMu.Unlock()

	t.logMu.Lock()
	delete(t.logs, agentID)
	delete(t.hasNewMessages, agentID)
	t.logMu.Unlock()

	// Update workspace to reflect the removed agent.
	go t.saveCurrentWorkspace("last")

	// Switch view back to root.
	t.app.QueueUpdateDraw(func() {
		t.selectAgent(tuiRootRef)
	})
}

// handleSessionDestroy shows a confirmation modal before permanently destroying
// the current agent and its session file on disk. Must be called from a goroutine.
func (t *tuiApp) handleSessionDestroy(agentID string) {
	if agentID == tuiRootRef {
		t.appendLog(agentID, "[capelin-go] /session-destroy: no agent selected\n", t.theme.LogError)
		return
	}
	agent := t.getTopLevelAgent(agentID)
	if agent == nil {
		t.appendLog(agentID, "[capelin-go] /session-destroy is only available when a top-level agent is selected\n", t.theme.LogError)
		return
	}
	t.app.QueueUpdateDraw(func() {
		modal := tview.NewModal().
			SetText("Destroy this session?\n\nThis will remove the agent and permanently delete the session file from disk.").
			AddButtons([]string{"Destroy session", "Cancel"}).
			SetDoneFunc(func(buttonIndex int, buttonLabel string) {
				t.app.SetRoot(t.normalLayout, true).SetFocus(t.inputField)
				if buttonLabel == "Destroy session" {
					go t.doHandleSessionDestroy(agentID)
				}
			})
		t.app.SetRoot(modal, false)
	})
}

// doHandleSessionDestroy removes the agent from the tree and deletes the session
// file from disk. Must be called from a goroutine.
func (t *tuiApp) doHandleSessionDestroy(agentID string) {
	agent := t.getTopLevelAgent(agentID)
	if agent == nil {
		return
	}

	// Capture the session UUID before removing from maps.
	sessionUUID := agent.sessionUUID

	// Cancel any in-flight request.
	agent.cancelMu.Lock()
	cancel := agent.cancel
	agent.cancelMu.Unlock()
	if cancel != nil {
		cancel()
	}

	// Remove from maps so the tree refresh no longer shows it.
	t.agentsMu.Lock()
	delete(t.topAgents, agentID)
	t.agentsMu.Unlock()

	t.logMu.Lock()
	delete(t.logs, agentID)
	delete(t.hasNewMessages, agentID)
	t.logMu.Unlock()

	// Delete the session file from disk.
	if t.owner.cfg.workspaceRoot != "" && sessionUUID != "" {
		path := filepath.Join(sessionsDir(t.owner.cfg.workspaceRoot), sessionUUID+".json")
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			t.appendLog(agentID, "[capelin-go] failed to delete session file: "+err.Error()+"\n", t.theme.LogError)
		}
	}

	// Update workspace to reflect the removed agent.
	go t.saveCurrentWorkspace("last")

	t.appendLog(agentID, "[capelin-go] session destroyed and deleted from disk\n", t.theme.LogSystem)

	// Switch view back to root.
	t.app.QueueUpdateDraw(func() {
		t.selectAgent(tuiRootRef)
	})
}

// handleWorkspaceNew prompts for confirmation if agents exist, then clears all agents.
// Must be called from a goroutine.
func (t *tuiApp) handleWorkspaceNew(callerAgentID string) {
	t.agentsMu.Lock()
	hasAgents := len(t.topAgents) > 0
	t.agentsMu.Unlock()

	if hasAgents {
		t.app.QueueUpdateDraw(func() {
			modal := tview.NewModal().
				SetText("Create a new empty workspace?\n\nCurrent agents and sessions will be cleared.").
				AddButtons([]string{"Create new workspace", "Cancel"}).
				SetDoneFunc(func(buttonIndex int, buttonLabel string) {
					if buttonLabel == "Create new workspace" {
						go t.doHandleWorkspaceNew(callerAgentID)
					} else {
						t.app.SetRoot(t.normalLayout, true).SetFocus(t.inputField)
					}
				})
			t.app.SetRoot(modal, false)
		})
		return
	}
	t.doHandleWorkspaceNew(callerAgentID)
}

// doHandleWorkspaceNew clears all top-level agents and resets last.json to empty.
// Must be called from a goroutine.
func (t *tuiApp) doHandleWorkspaceNew(callerAgentID string) {
	// Save current state as a safety net before wiping.
	if t.owner.cfg.workspaceRoot != "" {
		t.saveCurrentWorkspace("last")
	}

	// Collect current agent IDs.
	t.agentsMu.Lock()
	ids := make([]string, 0, len(t.topAgents))
	for id := range t.topAgents {
		ids = append(ids, id)
	}
	t.agentsMu.Unlock()

	// Cancel and remove each agent.
	for _, id := range ids {
		agent := t.getTopLevelAgent(id)
		if agent == nil {
			continue
		}
		agent.cancelMu.Lock()
		cancel := agent.cancel
		agent.cancelMu.Unlock()
		if cancel != nil {
			cancel()
		}
		t.agentsMu.Lock()
		delete(t.topAgents, id)
		t.agentsMu.Unlock()
		t.logMu.Lock()
		delete(t.logs, id)
		delete(t.hasNewMessages, id)
		t.logMu.Unlock()
	}

	// Reset workspace file to empty.
	if t.owner.cfg.workspaceRoot != "" {
		_ = saveWorkspace(t.owner.cfg.workspaceRoot, "last", nil)
	}

	t.appendLog(tuiRootRef, "[capelin-go] New workspace created.\n", t.theme.LogSystem)
	t.app.QueueUpdateDraw(func() {
		t.selectAgent(tuiRootRef)
	})
}

// handleWorkspaceSave saves the current workspace under the given name.
// Must be called from a goroutine.
func (t *tuiApp) handleWorkspaceSave(callerAgentID, name string) {
	if t.owner.cfg.workspaceRoot == "" {
		t.appendLog(callerAgentID, "[capelin-go] /workspace save: no workspace root configured\n", t.theme.LogError)
		return
	}
	t.saveCurrentWorkspace(name)
	t.appendLog(callerAgentID, fmt.Sprintf("[capelin-go] Workspace saved as %q\n", name), t.theme.LogSystem)
}

// handleWorkspaceLoad replaces the current workspace with a named saved workspace.
// Must be called from a goroutine.
func (t *tuiApp) handleWorkspaceLoad(callerAgentID, name string) {
	if t.owner.cfg.workspaceRoot == "" {
		t.appendLog(callerAgentID, "[capelin-go] /workspace: no workspace root configured\n", t.theme.LogError)
		return
	}
	snap, err := loadWorkspace(t.owner.cfg.workspaceRoot, name)
	if err != nil {
		t.appendLog(callerAgentID, fmt.Sprintf("[capelin-go] /workspace: workspace %q not found\n", name), t.theme.LogError)
		return
	}

	// Clear current agents first (same logic as /workspace new).
	t.agentsMu.Lock()
	ids := make([]string, 0, len(t.topAgents))
	for id := range t.topAgents {
		ids = append(ids, id)
	}
	t.agentsMu.Unlock()
	for _, id := range ids {
		agent := t.getTopLevelAgent(id)
		if agent == nil {
			continue
		}
		agent.cancelMu.Lock()
		cancel := agent.cancel
		agent.cancelMu.Unlock()
		if cancel != nil {
			cancel()
		}
		t.agentsMu.Lock()
		delete(t.topAgents, id)
		t.agentsMu.Unlock()
		t.logMu.Lock()
		delete(t.logs, id)
		delete(t.hasNewMessages, id)
		t.logMu.Unlock()
	}

	// Load sessions from the named workspace.
	loaded := 0
	var lastAgent *tuiAgent
	for _, entry := range snap.Agents {
		msgs, err := loadSessionMessages(t.owner.cfg.workspaceRoot, entry.SessionUUID)
		if err != nil {
			continue
		}
		agent := t.createTopLevelAgentWithMessages(msgs, false)
		agent.sessionUUID = entry.SessionUUID
		if entry.Name != "" {
			agent.queueMu.Lock()
			agent.Name = entry.Name
			agent.queueMu.Unlock()
			label := agentNodeLabel(agent.num, entry.Name)
			t.logMu.Lock()
			t.agentNames[agent.id] = label
			t.logMu.Unlock()
		}
		t.replayMessagesToLogBuf(agent.id, msgs)
		t.writeLogBuf(agent.id,
			fmt.Sprintf("\n--- Session resumed: %s (%d messages) ---\n\n", entry.SessionUUID[:8], len(msgs)),
			t.theme.LogSystem)
		lastAgent = agent
		loaded++
	}

	// Persist as last.json so the app can restore it next time.
	t.saveCurrentWorkspace("last")

	if loaded == 0 {
		t.appendLog(callerAgentID, fmt.Sprintf("[capelin-go] Workspace %q loaded (no restorable sessions found)\n", name), t.theme.LogSystem)
		t.app.QueueUpdateDraw(func() { t.selectAgent(tuiRootRef) })
		return
	}
	t.appendLog(callerAgentID, fmt.Sprintf("[capelin-go] Workspace %q loaded (%d session(s))\n", name, loaded), t.theme.LogSystem)
	if lastAgent != nil {
		t.app.QueueUpdateDraw(func() { t.selectAgent(lastAgent.id) })
	}
}

// handleWorkspacePicker shows an interactive picker of named saved workspaces.
// Must be called from a goroutine.
func (t *tuiApp) handleWorkspacePicker(callerAgentID string) {
	if t.owner.cfg.workspaceRoot == "" {
		t.appendLog(callerAgentID, "[capelin-go] /workspace: no workspace root configured\n", t.theme.LogError)
		return
	}
	names, err := listWorkspaces(t.owner.cfg.workspaceRoot)
	if err != nil {
		t.appendLog(callerAgentID, "[capelin-go] /workspace: error listing workspaces: "+err.Error()+"\n", t.theme.LogError)
		return
	}
	if len(names) == 0 {
		t.appendLog(callerAgentID, "[capelin-go] /workspace: no saved workspaces found (use /workspace-save <name> to create one)\n", t.theme.LogSystem)
		return
	}

	list := tview.NewList()
	list.SetBorder(true).SetTitle(" Load Workspace (Enter to load, Esc to cancel) ")
	list.SetBorderColor(t.theme.ActiveBorder)
	for _, name := range names {
		snap, err := loadWorkspace(t.owner.cfg.workspaceRoot, name)
		secondary := "(error loading)"
		if err == nil && snap != nil {
			agents := snap.Agents
			switch n := len(agents); {
			case n == 0:
				secondary = "(empty)"
			case n <= 3:
				parts := make([]string, n)
				for i, a := range agents {
					if a.Name != "" {
						parts[i] = a.Name
					} else {
						parts[i] = a.SessionUUID[:8]
					}
				}
				secondary = fmt.Sprintf("%d agents: %s", n, strings.Join(parts, ", "))
			default:
				parts := make([]string, 3)
				for i, a := range agents[:3] {
					if a.Name != "" {
						parts[i] = a.Name
					} else {
						parts[i] = a.SessionUUID[:8]
					}
				}
				secondary = fmt.Sprintf("%d agents: %s, …", n, strings.Join(parts, ", "))
			}
		}
		list.AddItem(name, secondary, 0, nil)
	}

	dismissOverlay := func() {
		t.app.SetRoot(t.normalLayout, true).SetFocus(t.inputField)
	}
	list.SetDoneFunc(func() { dismissOverlay() })
	list.SetSelectedFunc(func(index int, _ string, _ string, _ rune) {
		dismissOverlay()
		chosen := names[index]
		go t.handleWorkspaceLoad(callerAgentID, chosen)
	})

	h := len(names)*2 + 4
	if h < 10 {
		h = 10
	}
	if h > 30 {
		h = 30
	}
	overlay := tview.NewFlex().
		AddItem(nil, 0, 1, false).
		AddItem(tview.NewFlex().SetDirection(tview.FlexRow).
			AddItem(nil, 0, 1, false).
			AddItem(list, h, 0, true).
			AddItem(nil, 0, 1, false), 0, 3, true).
		AddItem(nil, 0, 1, false)

	t.app.QueueUpdateDraw(func() {
		t.inputField.SetText("")
		t.app.SetRoot(overlay, true).SetFocus(list)
	})
}

// handleSessionFork forks the given agent into a new top-level agent.
//   - "full":    copy the entire message history (default).
//   - "last":    system prompt + the last non-empty assistant message only.
//   - "summary": compact the history via LLM first, then fork with the summary.
//
// extraMsg (may be empty) is submitted to the new agent as the first user turn.
// Must be called from a goroutine.
func (t *tuiApp) handleSessionFork(src *tuiAgent, extraMsg string, mode string) {
	src.msgMu.Lock()
	msgs := make([]apiMessage, len(src.messages))
	copy(msgs, src.messages)
	rt := src.runtime
	src.msgMu.Unlock()

	switch mode {
	case "summary":
		// Compact the history via LLM first.
		compactMsgs := append(msgs, apiMessage{Role: "user", Content: compactPrompt})
		ctx := t.ctx
		if ctx == nil {
			ctx = context.Background()
		}
		_, summary, err := t.owner.runTurnLoop(ctx, compactMsgs, compactPrompt, rt, t.owner.toolset, false)
		if err != nil {
			if ctx.Err() == nil {
				t.appendLog(src.id, "[capelin-go] /session-fork: compact failed: "+err.Error()+"\n", t.theme.LogError)
			}
			return
		}
		sysContent := ""
		for _, m := range msgs {
			if m.Role == "system" {
				sysContent = m.Content
				break
			}
		}
		msgs = []apiMessage{
			{Role: "system", Content: sysContent},
			{Role: "assistant", Content: "[Conversation summary]\n" + summary},
		}
	case "last":
		// Keep only the system prompt and the last assistant message.
		sysContent := ""
		for _, m := range msgs {
			if m.Role == "system" {
				sysContent = m.Content
				break
			}
		}
		lastAssistant := ""
		for i := len(msgs) - 1; i >= 0; i-- {
			if msgs[i].Role == "assistant" && strings.TrimSpace(msgs[i].Content) != "" {
				lastAssistant = msgs[i].Content
				break
			}
		}
		if lastAssistant == "" {
			t.appendLog(src.id, "[capelin-go] /session-fork: no assistant message found to fork with 'last' mode\n", t.theme.LogError)
			return
		}
		msgs = []apiMessage{
			{Role: "system", Content: sysContent},
			{Role: "assistant", Content: lastAssistant},
		}
		// "full" falls through: msgs already contains the full history.
	}

	newAgent := t.createTopLevelAgentWithMessages(msgs, false)
	// Inherit the source agent's name (new name generates on first turn if empty).
	src.queueMu.Lock()
	srcName := src.Name
	src.queueMu.Unlock()
	if srcName != "" {
		newAgent.queueMu.Lock()
		newAgent.Name = srcName
		newAgent.queueMu.Unlock()
		label := agentNodeLabel(newAgent.num, srcName)
		t.logMu.Lock()
		t.agentNames[newAgent.id] = label
		t.logMu.Unlock()
	}
	// Replay history into the log so the panel shows previous context.
	t.replayMessagesToLogBuf(newAgent.id, msgs)
	t.writeLogBuf(newAgent.id,
		fmt.Sprintf("\n--- Forked from Agent %d (%d messages) ---\n\n", src.num, len(msgs)),
		t.theme.LogSystem)
	t.app.QueueUpdateDraw(func() {
		t.selectAgent(newAgent.id)
	})

	if extraMsg != "" {
		newAgent.queueMu.Lock()
		newAgent.busy = true
		newAgent.queueMu.Unlock()
		go t.submitInputForAgent(newAgent, extraMsg)
	}
}

var colorTagRe = regexp.MustCompile(`\[[a-zA-Z#,:\-]*\]`)

// completedSkillRe matches a fully-resolved %%name%% skill reference.
var completedSkillRe = regexp.MustCompile(`%%[A-Za-z0-9_-]+%%`)

// hasBareSkillTrigger returns true when text contains a bare %% that is NOT
// part of a complete %%name%% pair.  Used to decide whether to open the skill
// picker without re-triggering it after a skill has already been inserted.
func hasBareSkillTrigger(text string) bool {
	cleaned := completedSkillRe.ReplaceAllString(text, "")
	return strings.Contains(cleaned, "%%")
}

// removeFirstBareSkillTrigger removes the first bare %% (not part of a
// complete %%name%% pair) from text.
func removeFirstBareSkillTrigger(text string) string {
	parts := completedSkillRe.Split(text, -1)
	pairs := completedSkillRe.FindAllString(text, -1)
	for i, part := range parts {
		if idx := strings.Index(part, "%%"); idx >= 0 {
			parts[i] = part[:idx] + part[idx+2:]
			break
		}
	}
	var sb strings.Builder
	for i, part := range parts {
		sb.WriteString(part)
		if i < len(pairs) {
			sb.WriteString(pairs[i])
		}
	}
	return sb.String()
}

// replaceFirstBareSkillTrigger replaces the first bare %% in text with with.
func replaceFirstBareSkillTrigger(text, with string) string {
	parts := completedSkillRe.Split(text, -1)
	pairs := completedSkillRe.FindAllString(text, -1)
	done := false
	for i, part := range parts {
		if !done {
			if idx := strings.Index(part, "%%"); idx >= 0 {
				parts[i] = part[:idx] + with + part[idx+2:]
				done = true
			}
		}
	}
	var sb strings.Builder
	for i, part := range parts {
		sb.WriteString(part)
		if i < len(pairs) {
			sb.WriteString(pairs[i])
		}
	}
	return sb.String()
}

// commandCascadeTrigger returns the slash command that should open an
// interactive cascade when the user types a trailing space after it.
// Only commands without inline arguments are supported here.
func commandCascadeTrigger(text string) (string, bool) {
	if !strings.HasSuffix(text, " ") {
		return "", false
	}
	trimmed := strings.TrimSpace(text)
	switch trimmed {
	case "/session-resume", "/workspace", "/session-fork", "/append-to-agent":
		return trimmed, true
	default:
		return "", false
	}
}

// stripColorTags removes tview color/style tags like [red], [-], [#ffffff] from s.
func stripColorTags(s string) string {
	return colorTagRe.ReplaceAllString(s, "")
}

// withBg inserts bg into a tview color string at the background position.
// tview format is "fg:bg:attrs". Examples:
//   - "cyan"    → "cyan:black"
//   - "cyan::d" → "cyan:black:d"  (was fg=cyan, bg=empty, attrs=d)
//   - "white::" → "white:black:"
func withBg(color, bg string) string {
	parts := strings.SplitN(color, ":", 3)
	if len(parts) == 1 {
		return color + ":" + bg
	}
	parts[1] = bg
	return strings.Join(parts, ":")
}

func colorName(c tcell.Color) string {
	switch c {
	case tcell.ColorYellow:
		return "yellow"
	case tcell.ColorGreen:
		return "green"
	case tcell.ColorRed:
		return "red"
	case tcell.ColorGray:
		return "gray"
	case tcell.ColorDarkGray:
		return "darkgray"
	case tcell.ColorWhite:
		return "white"
	case tcell.ColorBlack:
		return "black"
	case tcell.ColorOlive:
		return "olive"
	case tcell.ColorDarkGreen:
		return "darkgreen"
	case tcell.ColorMaroon:
		return "maroon"
	case tcell.ColorBlue:
		return "blue"
	case tcell.ColorAqua:
		return "aqua"
	default:
		return "white"
	}
}
