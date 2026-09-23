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
	"time"
	"unicode/utf8"
)

const (
	// idleHookDefaultTimeoutSec bounds a single hook invocation independently of
	// the execute_program 60s default / 120s maximum contract.
	idleHookDefaultTimeoutSec = 5
	// idleHookMaxTimeoutSec is the absolute ceiling for a configured hook timeout.
	idleHookMaxTimeoutSec = 600
	// idleHookExecTimeoutCap mirrors the dedicated idle-hook wait adapter's
	// timeout ceiling. Ordinary execute_program remains capped at 120 seconds.
	idleHookExecTimeoutCap = idleHookMaxTimeoutSec
	// idleHookLogCapBytes bounds a single failure diagnostic so a chatty or
	// failing hook cannot flood stderr.
	idleHookLogCapBytes = 1024
)

const idleHookLogTruncationMarker = "…[truncated]"

type idleHookMode string

const (
	idleHookModeDetached idleHookMode = "detached"
	idleHookModeWait     idleHookMode = "wait"
)

// idleHookRunner owns the one-shot hook queue. A queue rather than a single
// asynchronous call makes repeated lifecycle events deterministic and lets
// short-lived commands drain every pending invocation before returning.
type idleHookRunner struct {
	command    string
	args       []string
	workspace  string
	yolo       bool
	mode       idleHookMode
	timeoutSec int
	execute    idleHookExecutor
	launch     idleHookLauncher
	logger     io.Writer

	mu      sync.Mutex
	cond    *sync.Cond
	pending int
	running bool
	closed  bool
	worker  chan struct{}
}

type idleHookExecutor func(context.Context, string, string, bool, []string) (string, error)
type idleHookLauncher func(context.Context, string, string, bool, []string) error

// newIdleHookRunner builds the serialized hook queue.
//
// timeoutSec bounds a single hook invocation and is optional/trailing so the
// existing app-level wiring keeps compiling until it passes a configured value.
// 0, negative, or omitted selects idleHookDefaultTimeoutSec (5s); values above
// idleHookMaxTimeoutSec are clamped down. This deadline is owned by the runner
// and is independent of the ordinary execute_program timeout contract.
func newIdleHookRunner(command string, args []string, workspace string, yolo bool, execute idleHookExecutor, logger io.Writer, timeoutSec ...int) *idleHookRunner {
	return newIdleHookRunnerWithMode(idleHookModeWait, command, args, workspace, yolo, execute, nil, logger, timeoutSec...)
}

// newIdleHookRunnerWithMode creates the serialized lifecycle queue. Detached
// mode waits only for the launch adapter to return; wait mode observes the
// existing result envelope until completion or its configured deadline.
func newIdleHookRunnerWithMode(mode idleHookMode, command string, args []string, workspace string, yolo bool, execute idleHookExecutor, launch idleHookLauncher, logger io.Writer, timeoutSec ...int) *idleHookRunner {
	if strings.TrimSpace(command) == "" || execute == nil {
		if mode != idleHookModeDetached || launch == nil || strings.TrimSpace(command) == "" {
			return nil
		}
	}
	if logger == nil {
		logger = io.Discard
	}
	if mode != idleHookModeWait {
		mode = idleHookModeDetached
	}
	if mode == idleHookModeDetached && launch == nil {
		launch = defaultIdleHookLauncher
	}
	configuredTimeout := 0
	if len(timeoutSec) > 0 {
		configuredTimeout = timeoutSec[0]
	}
	runner := &idleHookRunner{
		command:    strings.TrimSpace(command),
		args:       append([]string(nil), args...),
		workspace:  workspace,
		yolo:       yolo,
		mode:       mode,
		timeoutSec: configuredTimeout,
		execute:    execute,
		launch:     launch,
		logger:     logger,
		worker:     make(chan struct{}),
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

// effectiveTimeoutSec normalizes the configured hook timeout: 0/negative falls
// back to the 5s default and anything above the ceiling is clamped down.
func (r *idleHookRunner) effectiveTimeoutSec() int {
	eff := r.timeoutSec
	if eff <= 0 {
		eff = idleHookDefaultTimeoutSec
	}
	if eff > idleHookMaxTimeoutSec {
		eff = idleHookMaxTimeoutSec
	}
	return eff
}

func (r *idleHookRunner) executeOne() {
	if r.mode == idleHookModeDetached {
		err := r.launch(context.Background(), r.command, r.workspace, r.yolo, r.args)
		if err != nil {
			r.logFailure(err)
		}
		return
	}
	r.executeOneWait()
}

func (r *idleHookRunner) executeOneWait() {
	effTimeout := r.effectiveTimeoutSec()
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(effTimeout)*time.Second)
	defer cancel()

	result, err := r.execute(ctx, r.command, r.workspace, r.yolo, r.args)
	if err == nil {
		err = idleHookResultError(result)
	}
	// The runner owns its own deadline, so an expired context is a hook failure
	// even when the executor reported a clean envelope after the fact.
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		switch {
		case err == nil:
			err = fmt.Errorf("timeout after %ds", effTimeout)
		case strings.Contains(err.Error(), "timeout"):
			// idleHookResultError already surfaced the timeout; keep its detail.
		default:
			err = fmt.Errorf("timeout after %ds: %v", effTimeout, err)
		}
	}
	if err != nil {
		r.logFailure(err)
	}
}

func (r *idleHookRunner) logFailure(err error) {
	line := fmt.Sprintf("[capelin-go] idle hook failed (command %q args %q): %v", r.command, r.args, err)
	fmt.Fprintln(r.logger, capHookLog(line))
}

// capHookLog bounds a single diagnostic to idleHookLogCapBytes UTF-8 bytes,
// cutting at a rune boundary so a chatty or failing hook cannot flood stderr.
func capHookLog(s string) string {
	if len(s) <= idleHookLogCapBytes {
		return s
	}
	limit := idleHookLogCapBytes - len(idleHookLogTruncationMarker)
	if limit < 0 {
		limit = 0
	}
	cut := s[:limit]
	// Drop at most one trailing partial rune (a rune is never longer than
	// utf8.UTFMax bytes) so the truncated text stays valid UTF-8.
	for i := 0; i < utf8.UTFMax && len(cut) > 0; i++ {
		if rn, size := utf8.DecodeLastRuneInString(cut); rn == utf8.RuneError && size <= 1 {
			cut = cut[:len(cut)-1]
			continue
		}
		break
	}
	return cut + idleHookLogTruncationMarker
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
	return tools.RunIdleHookProgram(ctx, workspace, yolo, tools.ExecuteProgramArgs{
		Command:        command,
		Args:           append([]string(nil), args...),
		TimeoutSeconds: idleHookExecTimeoutSeconds(ctx),
	})
}

func defaultIdleHookLauncher(ctx context.Context, command, workspace string, yolo bool, args []string) error {
	return tools.RunDetachedProgram(ctx, workspace, yolo, tools.ExecuteProgramArgs{
		Command: command,
		Args:    append([]string(nil), args...),
	})
}

// idleHookExecTimeoutSeconds derives the idle-hook wait timeout_seconds from the
// runner-owned deadline, clamped to [1, min(tools.ToolTimeoutMax, 600)]. The
// runner context remains the authoritative bound; this only keeps the adapter
// from outliving it.
func idleHookExecTimeoutSeconds(ctx context.Context) int {
	upper := idleHookExecTimeoutCap
	if tools.ToolTimeoutMax < upper {
		upper = tools.ToolTimeoutMax
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		return clampInt(idleHookDefaultTimeoutSec, 1, upper)
	}
	remaining := time.Until(deadline)
	secs := int((remaining + time.Second - 1) / time.Second) // round up
	return clampInt(secs, 1, upper)
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
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
