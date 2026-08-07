package app

import (
	"capelin-go/internal/contracts"
	"capelin-go/internal/output"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/chzyer/readline"
)

const (
	interactiveIdlePrompt       = "> "
	interactiveBusyPrompt       = "[busy — Esc Esc cancels] > "
	interactiveCancellingPrompt = "[cancelling…] > "
)

type interactiveTurnOutcome struct {
	session *interactiveSession
	stopped bool
	err     error
}

type interactiveDraftRestorer struct {
	mu      sync.Mutex
	pending chan struct{}
}

func (r *interactiveDraftRestorer) restore(rl *readline.Instance, draft string) {
	if r == nil {
		restoreInteractiveDraftAsync(rl, draft)
		return
	}
	if rl == nil {
		return
	}
	done := make(chan struct{})
	r.mu.Lock()
	previous := r.pending
	r.pending = done
	r.mu.Unlock()
	go func() {
		if previous != nil {
			<-previous
		}
		rl.Operation.SetBuffer(draft)
		rl.Refresh()
		close(done)
	}()
}

func (r *interactiveDraftRestorer) wait() {
	if r == nil {
		return
	}
	r.mu.Lock()
	pending := r.pending
	r.mu.Unlock()
	if pending != nil {
		<-pending
	}
}

// interactiveTurnController is the single ownership point for an interactive
// turn. It reserves the session before starting a worker, never queues input,
// and closes its completion channel only after the worker's state has either
// been committed or discarded.
type interactiveTurnController struct {
	mu           sync.Mutex
	active       bool
	cancelling   bool
	cancel       context.CancelFunc
	done         chan struct{}
	sessionMu    sync.Mutex
	onBusy       func()
	onCancelling func()
	onIdle       func()
	onComplete   func(interactiveTurnOutcome)
	onIdleReady  func()
}

func newInteractiveTurnController(
	onBusy, onCancelling, onIdle func(),
	onComplete func(interactiveTurnOutcome),
	onIdleReady func(),
) *interactiveTurnController {
	return &interactiveTurnController{
		onBusy:       onBusy,
		onCancelling: onCancelling,
		onIdle:       onIdle,
		onComplete:   onComplete,
		onIdleReady:  onIdleReady,
	}
}

func (c *interactiveTurnController) start(parent context.Context, run func(context.Context) interactiveTurnOutcome) bool {
	if c == nil || run == nil {
		return false
	}
	workerCtx, cancel := context.WithCancel(parent)
	c.mu.Lock()
	if c.active {
		c.mu.Unlock()
		cancel()
		return false
	}
	c.active = true
	c.cancelling = false
	c.cancel = cancel
	c.done = make(chan struct{})
	c.mu.Unlock()
	if c.onBusy != nil {
		c.onBusy()
	}
	go func() {
		outcome := run(workerCtx)
		if c.onComplete != nil {
			c.onComplete(outcome)
		}
		if c.onIdle != nil {
			c.onIdle()
		}
		c.mu.Lock()
		c.active = false
		c.cancelling = false
		c.cancel = nil
		close(c.done)
		c.done = nil
		c.mu.Unlock()
		if c.onIdleReady != nil {
			c.onIdleReady()
		}
	}()
	return true
}

func (c *interactiveTurnController) busy() bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.active
}

func (c *interactiveTurnController) cancelActive() bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	if !c.active {
		c.mu.Unlock()
		return false
	}
	if c.cancelling {
		cancel := c.cancel
		c.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		return true
	}
	c.cancelling = true
	cancel := c.cancel
	c.mu.Unlock()
	if c.onCancelling != nil {
		c.onCancelling()
	}
	if cancel != nil {
		cancel()
	}
	return true
}

func (c *interactiveTurnController) wait() {
	if c == nil {
		return
	}
	c.mu.Lock()
	done := c.done
	c.mu.Unlock()
	if done != nil {
		<-done
	}
}

func (c *interactiveTurnController) shutdown() {
	if c == nil {
		return
	}
	c.cancelActive()
	c.wait()
}

func (c *interactiveTurnController) withSession(fn func()) {
	if c == nil || fn == nil {
		return
	}
	c.sessionMu.Lock()
	defer c.sessionMu.Unlock()
	fn()
}

func (c *interactiveTurnController) cloneSession(a *app, session *interactiveSession) *interactiveSession {
	return cloneInteractiveSession(a, session)
}

func cloneInteractiveSession(a *app, session *interactiveSession) *interactiveSession {
	if session == nil {
		return nil
	}
	clone := *session
	clone.messages = cloneMessages(session.messages)
	clone.providerState = cloneProviderState(session.providerState)
	clone.loadedSkills = make(map[string]bool, len(session.loadedSkills))
	for name, loaded := range session.loadedSkills {
		clone.loadedSkills[name] = loaded
	}
	clone.todos = cloneTodos(session.todos)
	clone.activeGoal = cloneGoalState(session.activeGoal)
	clone.runtime = cloneInteractiveRuntime(session.runtime)
	clone.save = nil
	clone.ephemeral = true
	clone.goalTurn = false
	a.attachInteractiveRuntime(&clone)
	return &clone
}

func cloneInteractiveRuntime(runtime *agentRuntime) *agentRuntime {
	if runtime == nil {
		return nil
	}
	clone := &agentRuntime{
		sessionID:         runtime.sessionID,
		depth:             runtime.depth,
		role:              runtime.role,
		allowedTools:      cloneAllowedTools(runtime.allowedTools),
		maxToolIterations: runtime.maxToolIterations,
		executionProfile:  runtime.executionProfile,
		model:             runtime.model,
		reasoning:         runtime.reasoning,
		emitOutput:        runtime.emitOutput,
		todos:             runtime.snapshotTodos(),
	}
	return clone
}

func commitInteractiveTurn(dst, src *interactiveSession) {
	if dst == nil || src == nil {
		return
	}
	saver := dst.save
	*dst = *src
	dst.save = saver
	dst.ephemeral = false
	dst.goalTurn = false
}

func (a *app) startInteractiveTurn(
	ctx context.Context,
	controller *interactiveTurnController,
	session *interactiveSession,
	run func(context.Context, *interactiveSession) (bool, error),
) bool {
	if controller == nil || session == nil || run == nil {
		return false
	}
	controller.mu.Lock()
	if controller.active {
		controller.mu.Unlock()
		return false
	}
	controller.active = true
	controller.cancelling = false
	workerCtx, cancel := context.WithCancel(ctx)
	controller.cancel = cancel
	controller.done = make(chan struct{})
	controller.mu.Unlock()
	if controller.onBusy != nil {
		controller.onBusy()
	}
	controller.sessionMu.Lock()
	workerSession := controller.cloneSession(a, session)
	controller.sessionMu.Unlock()
	go func() {
		stopped, err := run(workerCtx, workerSession)
		outcome := interactiveTurnOutcome{session: workerSession, stopped: stopped, err: err}
		if controller.onComplete != nil {
			controller.onComplete(outcome)
		}
		if controller.onIdle != nil {
			controller.onIdle()
		}
		controller.mu.Lock()
		controller.active = false
		controller.cancelling = false
		controller.cancel = nil
		close(controller.done)
		controller.done = nil
		controller.mu.Unlock()
		if controller.onIdleReady != nil {
			controller.onIdleReady()
		}
	}()
	return true
}

func (a *app) finishInteractiveTurn(controller *interactiveTurnController, session *interactiveSession, outcome interactiveTurnOutcome) {
	if controller == nil {
		return
	}
	worker := outcome.session
	preserveGoal := worker != nil && worker.goalTurn && worker.activeGoal != nil
	if worker == nil || worker.skipCommit || (!preserveGoal && (outcome.stopped || outcome.err != nil)) {
		if outcome.err != nil {
			if errors.Is(outcome.err, context.Canceled) {
				a.writeInteractiveSystem("[capelin-go] turn cancelled")
			} else {
				a.writeInteractiveSystem(fmt.Sprintf("[capelin-go] error: %v", outcome.err))
			}
		}
		return
	}
	controller.sessionMu.Lock()
	err := a.saveInteractiveSessionCandidate(worker)
	if err == nil {
		successMessage := worker.successMessage
		commitInteractiveTurn(session, worker)
		if session != nil {
			session.goalTurn = false
			session.successMessage = ""
			a.attachInteractiveRuntime(session)
			a.attachInteractiveSaver(session)
		}
		if successMessage != "" {
			a.writeInteractiveSystem(successMessage)
		}
	}
	controller.sessionMu.Unlock()
	if err != nil {
		a.writeInteractiveSystem(fmt.Sprintf("[capelin-go] warning: could not save session: %v", err))
		return
	}
	if outcome.err != nil {
		if errors.Is(outcome.err, context.Canceled) {
			a.writeInteractiveSystem("[capelin-go] turn cancelled")
		} else {
			a.writeInteractiveSystem(fmt.Sprintf("[capelin-go] error: %v", outcome.err))
		}
	}
}

func (a *app) handleInteractiveInputAsync(
	ctx context.Context,
	controller *interactiveTurnController,
	session *interactiveSession,
	rl *readline.Instance,
	draftRestorer *interactiveDraftRestorer,
	rawInput string,
) bool {
	if controller == nil {
		return a.handleInteractiveInput(ctx, session, rawInput)
	}
	if controller.busy() {
		if rl != nil {
			draftRestorer.restore(rl, rawInput)
		}
		return false
	}
	input := normalizeInteractiveInput(rawInput)
	if input == "" {
		return false
	}

	startTurn := func(run func(context.Context, *interactiveSession) (bool, error)) bool {
		_ = a.startInteractiveTurn(ctx, controller, session, run)
		return false
	}
	if session != nil && session.activeGoal != nil && naturalGoalContinuation(input) {
		return startTurn(func(turnCtx context.Context, worker *interactiveSession) (bool, error) {
			return a.runGoal(turnCtx, worker, ""), nil
		})
	}
	switch input {
	case "/exit", "/quit":
		controller.withSession(func() {
			if err := a.saveInteractiveSession(session); err != nil {
				a.writeInteractiveSystem(fmt.Sprintf("[capelin-go] warning: could not save session: %v", err))
			}
		})
		return true
	case "/session-list":
		controller.withSession(func() {
			if err := a.listInteractiveSessions(session); err != nil {
				a.writeInteractiveSystem(fmt.Sprintf("[capelin-go] /session-list failed: %v", err))
			}
		})
		return false
	case "/compact":
		return startTurn(func(turnCtx context.Context, worker *interactiveSession) (bool, error) {
			a.writeInteractiveSystem("[capelin-go] compacting conversation…")
			err := a.compactInteractiveSession(turnCtx, worker)
			if errors.Is(err, errNothingToCompact) {
				worker.skipCommit = true
				a.writeInteractiveSystem("[capelin-go] nothing to compact")
				return false, nil
			}
			if err != nil {
				return false, err
			}
			worker.successMessage = fmt.Sprintf("[capelin-go] compacted conversation to %d messages", len(worker.messages))
			return false, nil
		})
	case "/save":
		var response string
		controller.withSession(func() { response = session.lastResponse })
		if response == "" {
			a.writeInteractiveSystem("[capelin-go] /save: no assistant response is available")
			return false
		}
		path, err := a.interactiveResponsePath()
		if err == nil {
			err = os.WriteFile(path, []byte(response), 0o644)
		}
		if err != nil {
			a.writeInteractiveSystem(fmt.Sprintf("[capelin-go] /save failed: %v", err))
		} else {
			a.writeInteractiveSystem(fmt.Sprintf("[capelin-go] saved response to %s", filepath.Base(path)))
		}
		return false
	}

	if arg, ok := interactiveCommandArgument(input, "/compact"); ok {
		if arg != "" {
			a.writeInteractiveSystem("[capelin-go] /compact accepts no arguments")
			return false
		}
		return startTurn(func(turnCtx context.Context, worker *interactiveSession) (bool, error) {
			a.writeInteractiveSystem("[capelin-go] compacting conversation…")
			err := a.compactInteractiveSession(turnCtx, worker)
			if errors.Is(err, errNothingToCompact) {
				worker.skipCommit = true
				a.writeInteractiveSystem("[capelin-go] nothing to compact")
				return false, nil
			}
			if err != nil {
				return false, err
			}
			worker.successMessage = fmt.Sprintf("[capelin-go] compacted conversation to %d messages", len(worker.messages))
			return false, nil
		})
	}
	if arg, ok := interactiveCommandArgument(input, "/session-new"); ok {
		var err error
		controller.withSession(func() { err = a.switchToNewSession(session) })
		if err != nil {
			a.writeInteractiveSystem(fmt.Sprintf("[capelin-go] /session-new failed: %v", err))
			return false
		}
		if arg == "" {
			return false
		}
		return startTurn(func(turnCtx context.Context, worker *interactiveSession) (bool, error) {
			return a.runInteractiveTurnResult(turnCtx, worker, arg)
		})
	}
	if arg, ok := interactiveCommandArgument(input, "/session-resume"); ok {
		var err error
		controller.withSession(func() { err = a.switchToSavedSession(session, arg) })
		if err != nil {
			a.writeInteractiveSystem(fmt.Sprintf("[capelin-go] /session-resume failed: %v", err))
		}
		return false
	}
	if arg, ok := interactiveCommandArgument(input, "/goal"); ok {
		return startTurn(func(turnCtx context.Context, worker *interactiveSession) (bool, error) {
			return a.runGoal(turnCtx, worker, arg), nil
		})
	}
	return startTurn(func(turnCtx context.Context, worker *interactiveSession) (bool, error) {
		return a.runInteractiveTurnResult(turnCtx, worker, input)
	})
}

// readline delivers a submitted line before its internal input goroutine has
// finished processing the Enter key. Mutating or refreshing the operation from
// that callback can deadlock on readline's unbuffered result channel. Defer
// rejected-draft restoration until the callback has returned to the loop.
func restoreInteractiveDraftAsync(rl *readline.Instance, draft string) {
	if rl == nil {
		return
	}
	go func() {
		rl.Operation.SetBuffer(draft)
		rl.Refresh()
	}()
}

func (a *app) interactiveSinkForReadline(rl *readline.Instance) func() {
	if rl == nil {
		return func() {}
	}
	previous := a.sink
	var base contracts.OutputSink
	switch sink := previous.(type) {
	case *output.StdioSink:
		base = output.NewStdioSinkWithWriters(rl.Stdout(), rl.Stderr(), nil)
	case *output.FinalOnlySink:
		base = output.NewFinalOnlySink(
			output.NewStdioSinkWithWriters(rl.Stdout(), rl.Stderr(), nil),
			sink.RootAgentID,
		)
	default:
		base = previous
	}
	a.sink = output.NewRefreshingSink(base, rl.Refresh)
	return func() { a.sink = previous }
}
