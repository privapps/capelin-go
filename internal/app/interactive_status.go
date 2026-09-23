package app

import (
	configpkg "capelin-go/internal/config"
	"capelin-go/internal/contracts"
	"capelin-go/internal/output"
	"capelin-go/internal/subagents"
	"fmt"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
)

// The :: namespace is reserved for local interactive status commands. These
// commands never invoke the model, never append conversation history, and
// never persist output; they are emitted through the existing interactive
// system sink so they work while a model, tool, or subagent turn is active.
const (
	statusCommandTodos  = "::todos"
	statusCommandAgents = "::agents"
	// statusCommandAgentDeprecated is the retired singular spelling. It keeps
	// working so existing muscle memory and scripts do not break, but every
	// use emits a single deprecation note before the ::agents view is
	// rendered.
	statusCommandAgentDeprecated = "::agent"
	statusCommandStats           = "::stats"
	// statusCommandSession reports the current interactive session's full
	// persisted UUID. It accepts no arguments and, like every other command in
	// the namespace, is answered locally so it stays available during a busy
	// turn without invoking the provider or touching persisted state.
	statusCommandSession = "::session"
	// statusCommandLimits reports the effective runtime limits bounding the
	// current interactive session: the active execution profile, the root and
	// goal iteration budgets, the subagent and tool limits, the agent-question
	// preview budget, and the idle-hook state. It accepts no arguments and,
	// like every other command in the namespace, is answered locally so it
	// stays available during a busy turn or an active goal without invoking
	// the provider or touching persisted state.
	statusCommandLimits = "::limits"
)

// statusAgentsDeprecationNote is the one-line note emitted before the view
// when the deprecated ::agent spelling is used.
const statusAgentsDeprecationNote = "[capelin-go] ::agent is deprecated; use ::agents"

// statusCommandNames is the completion catalog for the :: namespace. Keep it
// sorted for stable completion output. The deprecated ::agent alias is
// deliberately absent so completion only advertises the supported spelling.
var statusCommandNames = []string{statusCommandAgents, statusCommandLimits, statusCommandSession, statusCommandStats, statusCommandTodos}

// handleStatusCommand owns interpretation of the reserved :: namespace. It
// returns true when the input was consumed locally. Unknown commands and
// known commands with extra arguments produce a local usage error instead of
// becoming model input. The busy flag reflects the application-level
// interactive activity state used for the synthetic root agent display, and
// worker is the live view of the session that owns an active turn, or nil when
// the session is idle.
func (a *app) handleStatusCommand(session *interactiveSession, input string, busy bool, worker *activeWorkerView) bool {
	if !strings.HasPrefix(input, "::") {
		return false
	}
	command, rest := input, ""
	if index := strings.IndexAny(input, " \t\r\n"); index >= 0 {
		command = strings.TrimSpace(input[:index])
		rest = strings.TrimSpace(input[index:])
	}
	switch command {
	case statusCommandTodos:
		if a.rejectArgs(command, rest) {
			return true
		}
		a.writeInteractiveSystem(formatInteractiveTodos(session, worker))
	case statusCommandAgents, statusCommandAgentDeprecated:
		if command == statusCommandAgentDeprecated {
			a.writeInteractiveSystem(statusAgentsDeprecationNote)
		}
		options := parseAgentsViewOptions(rest)
		a.writeInteractiveSystem(a.formatInteractiveAgents(busy, options.full))
	case statusCommandStats:
		if a.rejectArgs(command, rest) {
			return true
		}
		a.writeInteractiveSystem(formatInteractiveStats(a.cfg))
	case statusCommandSession:
		if a.rejectArgs(command, rest) {
			return true
		}
		a.writeInteractiveSystem(formatInteractiveSession(session))
	case statusCommandLimits:
		if a.rejectArgs(command, rest) {
			return true
		}
		a.writeInteractiveSystem(a.formatInteractiveLimits(session))
	default:
		a.writeInteractiveSystem(fmt.Sprintf("[capelin-go] unknown status command %q; available: %s", command, strings.Join(statusCommandNames, ", ")))
	}
	return true
}

// rejectArgs enforces the shared "accepts no arguments" contract of the
// argument-free :: commands. It reports true when the command was rejected,
// after emitting the single local usage error, so each case can return
// immediately without repeating the message.
func (a *app) rejectArgs(command, rest string) bool {
	if rest != "" {
		a.writeInteractiveSystem("[capelin-go] " + command + " accepts no arguments")
		return true
	}
	return false
}

// agentsViewOptions holds the parsed display options of the ::agents status
// view. The :: namespace never invokes the model and never spawns subagents,
// so these are display options only: full controls preview expansion, while
// model and timeout are accepted for forward compatibility with the spawn
// vocabulary and have no runtime effect in a status view.
type agentsViewOptions struct {
	full bool
	// model and timeout are recorded so the parser accepts the flags without
	// error. They intentionally do not influence the rendered status view.
	model   string
	timeout string
	// positional collects unrecognized or terminator-escaped text. The status
	// view ignores it rather than failing, so "::agents -- -weird" renders.
	positional []string
}

// parseAgentsViewOptions interprets the argument tail of ::agents/::agent as
// display options. Parsing never fails: unknown tokens and text after the
// "--" terminator are collected as positional input and ignored by the status
// view, so a dash-leading token can never be mistaken for a bad flag. Flag
// order is irrelevant; flags before and after the terminator are both
// honoured, and "--" only escapes the single token that follows it.
func parseAgentsViewOptions(rest string) agentsViewOptions {
	var options agentsViewOptions
	fields := strings.Fields(rest)
	for index := 0; index < len(fields); index++ {
		token := fields[index]
		switch token {
		case "-f", "--full":
			options.full = true
		case "--model":
			if index+1 < len(fields) {
				index++
				options.model = fields[index]
			}
		case "--timeout":
			if index+1 < len(fields) {
				index++
				options.timeout = fields[index]
			}
		case "--":
			// The terminator escapes exactly one following token so a
			// dash-leading value is never parsed as a flag. Any remaining
			// tokens keep their normal flag meaning.
			if index+1 < len(fields) {
				index++
				options.positional = append(options.positional, fields[index])
			}
		default:
			options.positional = append(options.positional, token)
		}
	}
	return options
}

// formatInteractiveTodos renders the interactive todo list in its existing
// order. While a turn owns the session it reads the active worker's live
// checklist through the worker runtime's synchronized snapshot, so the view
// agrees with the running turn before it commits. When no worker is published
// it falls back to the committed session snapshot and never reads the parent
// session's own mutable runtime todos.
func formatInteractiveTodos(session *interactiveSession, worker *activeWorkerView) string {
	todos, live := worker.todos()
	if !live {
		if session == nil {
			return "[::todos] no committed todo list is available"
		}
		todos = cloneTodos(session.todos)
	}
	if len(todos) == 0 {
		if live {
			return "[::todos] (live) no active todos"
		}
		return "[::todos] no committed todos"
	}
	var builder strings.Builder
	builder.WriteString("[::todos]")
	if live {
		builder.WriteString(" (live)")
	}
	for _, todo := range todos {
		fmt.Fprintf(&builder, "\n- %s [%s] %s", todo.ID, todo.Status, todo.Content)
	}
	return builder.String()
}

// formatInteractiveSession renders the current interactive session's identity
// for the ::session view. The session's persisted UUID is the sole source of
// truth and is rendered in full rather than shortened, so the value can be
// pasted directly into resume and session-inspection workflows. A nil session,
// or one whose identifier is empty or whitespace-only, is a normal local
// outcome rather than an error: it reports an explicit unavailable message so
// status inspection never panics. The function only reads the session; it
// never appends messages, mutates todos, or persists anything.
func formatInteractiveSession(session *interactiveSession) string {
	if session == nil {
		return "[::session] no active session is available"
	}
	id := strings.TrimSpace(session.id)
	if id == "" {
		return "[::session] no active session is available"
	}
	return "[::session] " + id
}

// countAgentDescendants returns the recursive descendant count for a parent in
// the supplied hierarchy, including the synthetic root. The count includes all
// nested descendants rather than only direct children. Results are memoized
// per parent, and the in-progress set makes a malformed cyclic or
// self-parenting hierarchy terminate instead of recursing forever.
func countAgentDescendants(children map[string][]contracts.SubagentNode, parent string, visiting map[string]bool, memo map[string]int) int {
	if total, ok := memo[parent]; ok {
		return total
	}
	if visiting[parent] {
		return 0
	}
	visiting[parent] = true
	total := 0
	for _, node := range children[parent] {
		total += 1 + countAgentDescendants(children, node.ID, visiting, memo)
	}
	delete(visiting, parent)
	memo[parent] = total
	return total
}

// agentDetailSuffix renders the optional trailing detail fields of one agent
// entry. Fields are appended after the existing parenthesized identity group
// so the established hierarchy, role, status, and name rendering is preserved
// byte for byte. Empty questions and zero descendant counts are omitted. The
// question text is display-only: it is rendered through the shared
// grapheme-aware preview helper and the stored node question is never
// modified.
func agentDetailSuffix(question string, descendants int) string {
	return agentDetailSuffixPreview(question, descendants, false)
}

// agentDetailSuffixPreview is agentDetailSuffix with explicit control over
// preview expansion. full renders the whitespace-collapsed question in its
// entirety; otherwise the shared preview truncation applies.
func agentDetailSuffixPreview(question string, descendants int, full bool) string {
	var fields []string
	if rendered := output.Preview(question, full); rendered != "" {
		fields = append(fields, "question: "+rendered)
	}
	if descendants > 0 {
		fields = append(fields, fmt.Sprintf("descendants: %d", descendants))
	}
	if len(fields) == 0 {
		return ""
	}
	return " " + strings.Join(fields, "; ")
}

// formatInteractiveAgents renders a synthetic root coordinator entry and the
// live subagent manager snapshot as a nested tree with aggregate status
// counts. The root status reflects the application-level interactive activity
// state passed by the dispatcher. Each entry may carry a previewed question
// and, when it has nested work, a recursive descendant count. full disables
// question truncation; the rendering is display-only and never mutates the
// stored node question.
func (a *app) formatInteractiveAgents(busy bool, full bool) string {
	rootStatus := "idle"
	if busy {
		rootStatus = "active"
	}
	var builder strings.Builder
	builder.WriteString("[::agents]")

	// Only the active interactive agent scope is rendered. Workers created by
	// a previous conversation belong to a retired scope and are invisible
	// here, so a new session never appears to inherit work it did not create.
	var nodes []contracts.SubagentNode
	if a != nil && a.subagents != nil {
		nodes = a.subagents.listScope(a.currentAgentScope())
	}
	known := make(map[string]bool, len(nodes))
	for _, node := range nodes {
		known[node.ID] = true
	}
	children := make(map[string][]contracts.SubagentNode)
	for _, node := range nodes {
		parent := node.ParentID
		if parent == "" || !known[parent] {
			parent = rootAgentID
		}
		children[parent] = append(children[parent], node)
	}
	for parent := range children {
		sort.Slice(children[parent], func(i, j int) bool {
			return children[parent][i].ID < children[parent][j].ID
		})
	}
	descendants := make(map[string]int, len(nodes)+1)
	countAgentDescendants(children, rootAgentID, make(map[string]bool, len(nodes)+1), descendants)

	fmt.Fprintf(&builder, "\n- %s (coordinator, %s)%s", rootAgentID, rootStatus, agentDetailSuffixPreview("", descendants[rootAgentID], full))

	var render func(parent string, depth int)
	render = func(parent string, depth int) {
		for _, node := range children[parent] {
			role := strings.TrimSpace(node.Role)
			if role == "" {
				role = "worker"
			}
			fmt.Fprintf(&builder, "\n%s- %s (%s, %s", strings.Repeat("  ", depth+1), node.ID, role, node.Status)
			if name := strings.TrimSpace(node.Name); name != "" {
				builder.WriteString(", name: " + name)
			}
			builder.WriteString(")")
			builder.WriteString(agentDetailSuffixPreview(node.Question, descendants[node.ID], full))
			render(node.ID, depth+1)
		}
	}
	render(rootAgentID, 0)

	counts := subagents.CountStatuses(nodes)
	fmt.Fprintf(&builder, "\naggregate: total=%d; active=%d; pending=%d queued=%d running=%d; completed=%d failed=%d cancelled=%d timed_out=%d",
		len(nodes), counts.Active(), counts.Pending, counts.Queued, counts.Running, counts.Completed, counts.Failed, counts.Cancelled, counts.TimedOut)
	return builder.String()
}

// bytesPerKB is the integer divisor used to render runtime memory counters in
// kilobytes. Diagnostics use 1024-byte KB deliberately so the reported value
// matches the binary units used by the Go runtime allocator.
const bytesPerKB = 1024

// formatGroupedUint renders an unsigned integer in base 10 with ASCII comma
// separators every three digits ("1234567" becomes "1,234,567"). It is a local
// standard-library-only substitute for a localized number printer: the output
// is intentionally locale-independent so status output is stable across
// environments. Values below 1000 are returned unchanged, and the whole uint64
// range including math.MaxUint64 is handled without overflow because grouping
// operates on the decimal digit string rather than on arithmetic.
func formatGroupedUint(value uint64) string {
	digits := strconv.FormatUint(value, 10)
	if len(digits) <= 3 {
		return digits
	}
	lead := len(digits) % 3
	if lead == 0 {
		lead = 3
	}
	grouped := make([]byte, 0, len(digits)+(len(digits)-1)/3)
	grouped = append(grouped, digits[:lead]...)
	for index := lead; index < len(digits); index += 3 {
		grouped = append(grouped, ',')
		grouped = append(grouped, digits[index:index+3]...)
	}
	return string(grouped)
}

// formatInteractiveStats renders portable process and runtime diagnostics. It
// deliberately reports only portable Go runtime values and represents
// unavailable or unset values with explicit markers instead of failing. Memory
// counters are reported in integer 1024-byte KB with comma grouping so the
// values stay readable in a terminal.
func formatInteractiveStats(cfg config) string {
	var memory runtime.MemStats
	runtime.ReadMemStats(&memory)
	model := strings.TrimSpace(cfg.model)
	if model == "" {
		model = "unset"
	}
	reasoning := strings.TrimSpace(cfg.reasoning)
	if reasoning == "" {
		reasoning = "unset"
	}
	cwd := "unavailable"
	if directory, err := os.Getwd(); err == nil {
		cwd = directory
	}
	return fmt.Sprintf(
		"[::stats] cwd: %s; model: %s; reasoning effort: %s; os: %s; arch: %s; go version: %s; cpus: %d; gomaxprocs: %d; goroutines: %d; memory alloc: %s KB; memory sys: %s KB",
		cwd, model, reasoning, runtime.GOOS, runtime.GOARCH, runtime.Version(), runtime.NumCPU(), runtime.GOMAXPROCS(0), runtime.NumGoroutine(),
		formatGroupedUint(memory.Alloc/bytesPerKB), formatGroupedUint(memory.Sys/bytesPerKB),
	)
}

// formatInteractiveLimits renders the effective runtime limits of the current
// interactive session as one line. The format is stable and intentionally
// compact:
//
//	[::limits] profile: ordinary; root iterations: 40; goal iterations: 20; subagent depth: 1; subagent children: 8; subagent parallel: 4; subagent iterations: 20; subagent timeout: 600s; tool parallel: 8; tool timeout: 60s; tool retry: true; preview budget: 160; idle hook: "my-hook" (source=env, mode=detached, timeout=disabled)
//
// Field contract:
//   - "[::limits] " is the fixed label; every remaining field is a
//     "label: value" pair joined by "; " in the order shown above.
//   - "profile:" is exactly "ordinary" or "goal". It reports "goal" only when
//     the session runtime's selected execution profile equals the resolved
//     goal profile and that profile actually differs from the ordinary one; a
//     nil session or nil runtime always reports "ordinary".
//   - Iteration, subagent, and tool values are read from the active profile
//     through the ordinaryRuntimeProfile/goalRuntimeProfile accessors so the
//     view cannot drift from what the runtime applies. Subagent and tool
//     timeouts carry a trailing "s" for seconds; "tool retry" is the bare Go
//     boolean literal true or false.
//   - "preview budget:" is the resolved agent-question preview cap, or the
//     built-in default 160 when no positive value is configured.
//   - "idle hook:" is either the quoted command with its source and timeout,
//     or the literal "disabled" when no command is configured or the hook was
//     turned off with --no-idle-hook.
//
// The renderer is read-only: it reads a value copy of the resolved
// configuration and the session runtime's selected profile, and it mutates
// nothing.
func (a *app) formatInteractiveLimits(session *interactiveSession) string {
	var cfg config
	if a != nil {
		cfg = a.cfg
	}
	ordinary := cfg.ordinaryRuntimeProfile()
	goal := cfg.goalRuntimeProfile()
	active, label := ordinary, "ordinary"
	if session != nil && session.runtime != nil && goal != ordinary && session.runtime.executionProfile == goal {
		active, label = goal, "goal"
	}
	// The defensive fallback still matters for synthetic configurations such
	// as &app{cfg: config{}}, which carry no resolved preview budget.
	preview := cfg.agentQuestionPreviewMax
	if preview <= 0 {
		preview = configpkg.DefaultAgentQuestionPreviewMax
	}
	fields := []string{
		"profile: " + label,
		fmt.Sprintf("root iterations: %d", active.MaxIterations),
		fmt.Sprintf("goal iterations: %d", active.MaxGoalIterations),
		fmt.Sprintf("subagent depth: %d", active.Subagents.MaxDepth),
		fmt.Sprintf("subagent children: %d", active.Subagents.MaxChildren),
		fmt.Sprintf("subagent parallel: %d", active.Subagents.MaxParallel),
		fmt.Sprintf("subagent iterations: %d", active.Subagents.MaxToolIterations),
		fmt.Sprintf("subagent timeout: %ds", active.Subagents.DefaultTimeoutSec),
		fmt.Sprintf("tool parallel: %d", active.ToolMaxParallel),
		fmt.Sprintf("tool timeout: %ds", active.ToolTimeoutSec),
		fmt.Sprintf("tool retry: %t", active.ToolRetryOnTimeout),
		fmt.Sprintf("preview budget: %d", preview),
		formatIdleHookLimitsField(cfg),
	}
	return fmt.Sprintf("[::limits] %s", strings.Join(fields, "; "))
}

// formatIdleHookLimitsField renders the idle-hook portion of the ::limits
// line. A blank command or an explicit --no-idle-hook renders the literal
// "idle hook: disabled" so the operator can tell a disabled hook from an
// unreported one.
func formatIdleHookLimitsField(cfg config) string {
	command := strings.TrimSpace(cfg.idleHookCommand)
	if command == "" || cfg.noIdleHook {
		return "idle hook: disabled"
	}
	mode := cfg.idleHookMode
	if mode == "" {
		mode = idleHookModeDetached
	}
	timeoutLabel := fmt.Sprintf("%ds", cfg.idleHookTimeoutSec)
	if mode == idleHookModeDetached {
		timeoutLabel = "disabled"
	}
	return fmt.Sprintf("idle hook: %q (source=%s, mode=%s, timeout=%s)", command, idleHookSourceLabel(cfg.idleHookSource), mode, timeoutLabel)
}
