package app

import (
	"capelin-go/internal/contracts"
	"capelin-go/internal/output"
	"capelin-go/internal/subagents"
	"fmt"
	"sync"
	"time"
)

const (
	goalHeartbeatInitialDelay = 5 * time.Second
	goalHeartbeatCadence      = 30 * time.Second
)

// goalHeartbeat reports diagnostic progress for one accepted goal. It owns
// only timer state; all output still travels through the application's sink.
// The mutex serializes progress writes with stop so terminal goal output can
// be written only after the reporter has ceased emitting.
type goalHeartbeat struct {
	emitFn        func(string)
	initialDelay  time.Duration
	cadence       time.Duration
	now           func() time.Time
	todosSnapshot func() []todoItem
	agentSnapshot func() []contracts.SubagentNode
	startedAt     time.Time
	stopCh        chan struct{}
	done          chan struct{}
	wg            sync.WaitGroup
	mu            sync.Mutex
	stopped       bool
	started       bool
	iteration     int
	iterationMax  int
	turnStartedAt time.Time
}

type goalHeartbeatProgress struct {
	todosSnapshot func() []todoItem
	agentSnapshot func() []contracts.SubagentNode
}

func newGoalHeartbeat(emit func(string), iterationMax int, initialDelay, cadence time.Duration, progress ...goalHeartbeatProgress) *goalHeartbeat {
	return newGoalHeartbeatAt(time.Now(), emit, iterationMax, initialDelay, cadence, progress...)
}

func newGoalHeartbeatAt(startedAt time.Time, emit func(string), iterationMax int, initialDelay, cadence time.Duration, progress ...goalHeartbeatProgress) *goalHeartbeat {
	if initialDelay <= 0 {
		initialDelay = goalHeartbeatInitialDelay
	}
	if cadence <= 0 {
		cadence = goalHeartbeatCadence
	}
	if startedAt.IsZero() {
		startedAt = time.Now()
	}
	h := &goalHeartbeat{
		emitFn:       emit,
		initialDelay: initialDelay,
		cadence:      cadence,
		now:          time.Now,
		startedAt:    startedAt,
		stopCh:       make(chan struct{}),
		done:         make(chan struct{}),
		iterationMax: iterationMax,
	}
	if len(progress) > 0 {
		h.todosSnapshot = progress[0].todosSnapshot
		h.agentSnapshot = progress[0].agentSnapshot
	}
	return h
}

// beginIteration starts the reporter on the first iteration and emits the
// immediate status for every iteration. The current-turn clock starts before
// the synchronous turn begins, so provider, tool, retry, and subagent waits
// are covered by the diagnostic.
func (h *goalHeartbeat) beginIteration(iteration int) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.stopped {
		return
	}
	h.iteration = iteration
	h.turnStartedAt = h.now()
	if !h.started {
		h.started = true
		h.wg.Add(1)
		go h.run()
	}
	h.emitLocked(h.now())
}

func (h *goalHeartbeat) endTurn() {
	if h == nil {
		return
	}
	h.mu.Lock()
	if !h.stopped {
		h.turnStartedAt = time.Time{}
	}
	h.mu.Unlock()
}

// stop is idempotent and waits for the reporter goroutine. Callers must stop
// before writing any terminal goal status; waiting also guarantees no output
// can race past runGoal's return.
func (h *goalHeartbeat) stop() {
	if h == nil {
		return
	}
	h.mu.Lock()
	if !h.stopped {
		h.stopped = true
		close(h.stopCh)
	}
	started := h.started
	h.mu.Unlock()
	if started {
		h.wg.Wait()
		h.mu.Lock()
		h.started = false
		h.mu.Unlock()
	}
}

func (h *goalHeartbeat) run() {
	defer h.wg.Done()
	defer close(h.done)

	timer := time.NewTimer(h.initialDelay)
	defer timer.Stop()
	select {
	case <-timer.C:
		h.emit()
	case <-h.stopCh:
		return
	}

	ticker := time.NewTicker(h.cadence)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			h.emit()
		case <-h.stopCh:
			return
		}
	}
}

func (h *goalHeartbeat) emit() {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.stopped {
		return
	}
	h.emitLocked(h.now())
}

// emitLocked writes one recurring heartbeat line. The format is stable and
// intentionally compact:
//
//	[goal] iteration 2/8 working; total elapsed 1m02s; current turn elapsed 00s; todos 1/5 completed; current todos: three item (+1 more); subagents 3 active
//
// Field contract:
//   - "[goal] iteration N/M working;" keeps the prefix and the " working;"
//     lifecycle marker that distinguishes progress from terminal status lines.
//   - "total elapsed" is always present; "current turn elapsed" appears only
//     while a turn is in flight.
//   - The todo block always precedes the agent block: todo completion count,
//     then the explicitly named current todo field, then "subagents N active".
//   - The current todo field is never a bare "current:"; it is "current todo:"
//     for zero or one in-progress item and "current todos:" for more than one.
//     Its value is a bounded output.Preview of the first in-progress item plus
//     an "(+N more)" count, or the explicit literal "none".
//   - The per-status subagent breakdown is deliberately omitted here; it stays
//     in the ::agents view, as the detailed checklist stays in ::todos.
func (h *goalHeartbeat) emitLocked(now time.Time) {
	if h.emitFn == nil || h.stopped || h.iteration <= 0 {
		return
	}
	total := elapsedSince(h.startedAt, now)
	turn := ""
	if !h.turnStartedAt.IsZero() {
		turn = "; current turn elapsed " + formatGoalHeartbeatDuration(elapsedSince(h.turnStartedAt, now))
	}
	progress := h.todoProgress()
	subagents := h.subagentProgress()
	label, current := progress.currentTodoField()
	h.emitFn(fmt.Sprintf(
		"[goal] iteration %d/%d working; total elapsed %s%s; todos %d/%d completed; %s: %s; subagents %d active",
		h.iteration,
		h.iterationMax,
		formatGoalHeartbeatDuration(total),
		turn,
		progress.completed,
		progress.total,
		label,
		current,
		subagents.Active(),
	))
}

const goalHeartbeatNoCurrentTodo = "none"

// goalHeartbeatBlankCurrentTodo is the placeholder rendered for an in-progress
// todo whose content is empty or whitespace-only. It is deliberately distinct
// from goalHeartbeatNoCurrentTodo so a real but blank checklist item can never
// be mistaken for the genuine "no current todo" state.
const goalHeartbeatBlankCurrentTodo = "(blank)"

type goalHeartbeatTodoProgress struct {
	completed int
	total     int
	// hasCurrent records whether at least one todo is in progress. It is
	// tracked separately from current so a blank in-progress todo is never
	// collapsed into the "no current todo" state.
	hasCurrent bool
	current    []string
}

// currentTodoField returns the explicit field label and bounded value for the
// in-progress checklist items. Only the first item is previewed; any remaining
// items are represented by a count so heartbeat output stays bounded no matter
// how many todos are in progress. The literal "none" is reserved for the case
// where nothing is in progress at all.
func (progress goalHeartbeatTodoProgress) currentTodoField() (string, string) {
	if !progress.hasCurrent {
		return "current todo", goalHeartbeatNoCurrentTodo
	}
	switch len(progress.current) {
	case 0:
		return "current todo", goalHeartbeatNoCurrentTodo
	case 1:
		return "current todo", progress.current[0]
	default:
		return "current todos", fmt.Sprintf("%s (+%d more)", progress.current[0], len(progress.current)-1)
	}
}

func (h *goalHeartbeat) todoProgress() goalHeartbeatTodoProgress {
	progress := goalHeartbeatTodoProgress{}
	if h == nil || h.todosSnapshot == nil {
		return progress
	}
	for _, todo := range h.todosSnapshot() {
		progress.total++
		if todo.Status == todoStatusCompleted {
			progress.completed++
		}
		if todo.Status == todoStatusInProgress {
			progress.hasCurrent = true
			progress.current = append(progress.current, normalizeGoalHeartbeatTodoContent(todo.Content))
		}
	}
	return progress
}

// normalizeGoalHeartbeatTodoContent collapses whitespace and truncates todo
// text with the shared preview helper so one long checklist entry cannot blow
// up the recurring status line. Blank or whitespace-only content renders as the
// explicit "(blank)" placeholder rather than "none", which is reserved for
// having no in-progress todo at all.
func normalizeGoalHeartbeatTodoContent(content string) string {
	preview := output.Preview(content, false)
	if preview == "" {
		return goalHeartbeatBlankCurrentTodo
	}
	return preview
}

// subagentProgress returns the shared status counter for the current agent
// snapshot. A nil snapshot yields a zero-valued count.
func (h *goalHeartbeat) subagentProgress() subagents.StatusCounts {
	if h == nil || h.agentSnapshot == nil {
		return subagents.StatusCounts{}
	}
	return subagents.CountStatuses(h.agentSnapshot())
}

func elapsedSince(start, now time.Time) time.Duration {
	if start.IsZero() || now.Before(start) {
		return 0
	}
	return now.Sub(start)
}

func formatGoalHeartbeatDuration(value time.Duration) string {
	if value < 0 {
		value = 0
	}
	value = value.Round(time.Second)
	hours := value / time.Hour
	minutes := (value % time.Hour) / time.Minute
	seconds := (value % time.Minute) / time.Second
	switch {
	case hours > 0:
		return fmt.Sprintf("%dh%dm%02ds", hours, minutes, seconds)
	case minutes > 0:
		return fmt.Sprintf("%dm%02ds", minutes, seconds)
	default:
		return fmt.Sprintf("%02ds", seconds)
	}
}
