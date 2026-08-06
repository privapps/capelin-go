package app

import (
	"capelin-go/internal/tools"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
)

// idleHookRunner owns the one-shot hook queue. A queue rather than a single
// asynchronous call makes repeated lifecycle events deterministic and lets
// short-lived commands drain every pending invocation before returning.
type idleHookRunner struct {
	command   string
	args      []string
	workspace string
	yolo      bool
	execute   idleHookExecutor
	logger    io.Writer

	mu      sync.Mutex
	cond    *sync.Cond
	pending int
	running bool
	closed  bool
	worker  chan struct{}
}

type idleHookExecutor func(context.Context, string, string, bool, []string) (string, error)

func newIdleHookRunner(command string, args []string, workspace string, yolo bool, execute idleHookExecutor, logger io.Writer) *idleHookRunner {
	if strings.TrimSpace(command) == "" || execute == nil {
		return nil
	}
	if logger == nil {
		logger = io.Discard
	}
	runner := &idleHookRunner{
		command:   strings.TrimSpace(command),
		args:      append([]string(nil), args...),
		workspace: workspace,
		yolo:      yolo,
		execute:   execute,
		logger:    logger,
		worker:    make(chan struct{}),
	}
	runner.cond = sync.NewCond(&runner.mu)
	go runner.run()
	return runner
}

func (r *idleHookRunner) run() {
	for {
		r.mu.Lock()
		for r.pending == 0 && !r.closed {
			r.cond.Wait()
		}
		if r.pending == 0 && r.closed {
			r.mu.Unlock()
			close(r.worker)
			return
		}
		r.pending--
		r.running = true
		r.mu.Unlock()

		r.executeOne()

		r.mu.Lock()
		r.running = false
		r.cond.Broadcast()
		r.mu.Unlock()
	}
}

func (r *idleHookRunner) executeOne() {
	result, err := r.execute(context.Background(), r.command, r.workspace, r.yolo, r.args)
	if err == nil {
		err = idleHookResultError(result)
	}
	if err != nil {
		fmt.Fprintf(r.logger, "[capelin-go] idle hook failed (command %q args %q): %v\n", r.command, r.args, err)
	}
}

func (r *idleHookRunner) trigger() {
	if r == nil {
		return
	}
	r.mu.Lock()
	if !r.closed {
		r.pending++
		r.cond.Signal()
	}
	r.mu.Unlock()
}

func (r *idleHookRunner) drainAndClose() {
	if r == nil {
		return
	}
	r.mu.Lock()
	if !r.closed {
		r.closed = true
		r.cond.Broadcast()
	}
	for r.pending != 0 || r.running {
		r.cond.Wait()
	}
	r.mu.Unlock()
	<-r.worker
}

func (a *app) finishOneShotIdleHook() {
	if a == nil || a.idleHooks == nil {
		return
	}
	a.idleHooks.trigger()
	a.idleHooks.drainAndClose()
}

func (a *app) finishInteractiveIdleHook() {
	if a == nil || a.idleHooks == nil {
		return
	}
	a.idleHooks.drainAndClose()
}

func (a *app) triggerInteractiveIdleHook() {
	if a == nil {
		return
	}
	if a.interactiveIdleHook != nil {
		a.interactiveIdleHook()
	}
	if a.idleHooks != nil {
		a.idleHooks.trigger()
	}
}

func defaultIdleHookExecutor(ctx context.Context, command, workspace string, yolo bool, args []string) (string, error) {
	return tools.RunExecuteProgram(ctx, workspace, yolo, tools.ExecuteProgramArgs{
		Command: command,
		Args:    append([]string(nil), args...),
	})
}

// idleHookResultError turns the existing execute_program result envelope into
// an operational error. The direct-program adapter reports process failures in
// its bounded JSON result while reserving Go errors for policy/start failures.
func idleHookResultError(raw string) error {
	if strings.TrimSpace(raw) == "" {
		return errors.New("empty execute_program result")
	}
	var result struct {
		ExitCode int    `json:"exit_code"`
		Stderr   string `json:"stderr"`
		Error    string `json:"error"`
		Failed   bool   `json:"failed"`
		TimedOut bool   `json:"timed_out"`
	}
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		return fmt.Errorf("invalid execute_program result: %w", err)
	}
	if !result.Failed && !result.TimedOut && result.ExitCode == 0 {
		return nil
	}
	detail := strings.TrimSpace(result.Error)
	if detail == "" {
		detail = strings.TrimSpace(result.Stderr)
	}
	if result.TimedOut {
		if detail == "" {
			detail = "timeout"
		} else {
			detail = "timeout: " + detail
		}
	}
	if detail == "" {
		detail = fmt.Sprintf("exit code %d", result.ExitCode)
	}
	return errors.New(detail)
}
