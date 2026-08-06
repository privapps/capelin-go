package app

import (
	"capelin-go/internal/contracts"
	"fmt"
	"strings"
	"sync"
	"time"
)

const (
	goalHeartbeatInitialDelay = 5 * time.Second
	goalHeartbeatCadence      = 10 * time.Second
)

// goalHeartbeat reports diagnostic progress for one accepted goal. It owns
// only timer state; all output still travels through the application's sink.
// The mutex serializes progress writes with stop so terminal goal output can
// be written only after the reporter has ceased emitting.
type goalHeartbeat struct {
	emitFn            func(string)
	snapshotTodos     func() []todoItem
	snapshotSubagents func() []contracts.SubagentNode
	initialDelay      time.Duration
	cadence           time.Duration
	now               func() time.Time
	startedAt         time.Time
	stopCh            chan struct{}
	done              chan struct{}
	wg                sync.WaitGroup
	mu                sync.Mutex
	stopped           bool
	started           bool
	iteration         int
	iterationMax      int
	turnStartedAt     time.Time
}

func newGoalHeartbeat(emit func(string), iterationMax int, initialDelay, cadence time.Duration, snapshotTodos func() []todoItem, snapshotSubagents func() []contracts.SubagentNode) *goalHeartbeat {
	if initialDelay <= 0 {
		initialDelay = goalHeartbeatInitialDelay
	}
	if cadence <= 0 {
		cadence = goalHeartbeatCadence
	}
	return &goalHeartbeat{
		emitFn:            emit,
		snapshotTodos:     snapshotTodos,
		snapshotSubagents: snapshotSubagents,
		initialDelay:      initialDelay,
		cadence:           cadence,
		now:               time.Now,
		startedAt:         time.Now(),
		stopCh:            make(chan struct{}),
		done:              make(chan struct{}),
		iterationMax:      iterationMax,
	}
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

func (h *goalHeartbeat) emitLocked(now time.Time) {
	if h.emitFn == nil || h.stopped || h.iteration <= 0 {
		return
	}
	todos := []todoItem{}
	if h.snapshotTodos != nil {
		todos = h.snapshotTodos()
	}
	subagents := []contracts.SubagentNode{}
	if h.snapshotSubagents != nil {
		subagents = h.snapshotSubagents()
	}
	total := elapsedSince(h.startedAt, now)
	turn := ""
	if !h.turnStartedAt.IsZero() {
		turn = "; current turn elapsed " + formatGoalHeartbeatDuration(elapsedSince(h.turnStartedAt, now))
	}
	completed := 0
	current := make([]string, 0)
	for _, todo := range todos {
		if todo.Status == todoStatusCompleted {
			completed++
		}
		if todo.Status == todoStatusInProgress {
			current = append(current, normalizeGoalHeartbeatTodoContent(todo.Content))
		}
	}
	currentText := "none"
	if len(current) > 0 {
		currentText = strings.Join(current, ", ")
	}
	activeSubagents := 0
	for _, subagent := range subagents {
		switch subagentStatus(subagent.Status) {
		case subagentStatusPending, subagentStatusQueued, subagentStatusRunning:
			activeSubagents++
		}
	}
	h.emitFn(fmt.Sprintf("[goal] iteration %d/%d working; total elapsed %s%s; subagents %d active; todos %d/%d completed; current: %s", h.iteration, h.iterationMax, formatGoalHeartbeatDuration(total), turn, activeSubagents, completed, len(todos), currentText))
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
	value %= time.Hour
	minutes := value / time.Minute
	seconds := (value % time.Minute) / time.Second
	if hours > 0 {
		return fmt.Sprintf("%dh%dm%02ds", hours, minutes, seconds)
	}
	if minutes > 0 {
		return fmt.Sprintf("%dm%02ds", minutes, seconds)
	}
	return fmt.Sprintf("%02ds", seconds)
}

func normalizeGoalHeartbeatTodoContent(content string) string {
	return strings.Join(strings.Fields(content), " ")
}
