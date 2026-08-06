package subagents

import (
	"capelin-go/internal/contracts"
	"capelin-go/internal/policy"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	rootAgentID = "root"
)

const (
	createSubagentOverflowWaitForSlot = "wait_for_slot"
	createSubagentOverflowFailFast    = "fail_fast"
)

const (
	defaultSubagentMaxDepth          = 1
	defaultSubagentMaxChildren       = 8
	defaultSubagentMaxParallel       = 4
	defaultSubagentDefaultTimeoutSec = 600  // 10 minutes
	defaultSubagentMaxTimeoutSec     = 1800 // 30 minutes
	defaultSubagentToolIterations    = 20
	defaultSubagentResultChars       = 8000
	defaultSubagentAggregateCount    = 12
	defaultSubagentAggregateChars    = 12000
)

type agentRole string

const (
	agentRoleCoordinator agentRole = "coordinator"
	agentRoleWorker      agentRole = "worker"
)

type subagentRuntimeConfig struct {
	MaxDepth          int
	MaxChildren       int
	MaxParallel       int
	DefaultTimeoutSec int
	MaxTimeoutSec     int
	MaxToolIterations int
	MaxResultChars    int
	MaxAggregateCount int
	MaxAggregateChars int
	Model             string
	ReasoningEffort   string
}

func defaultSubagentRuntimeConfig() subagentRuntimeConfig {
	return subagentRuntimeConfig{
		MaxDepth:          defaultSubagentMaxDepth,
		MaxChildren:       defaultSubagentMaxChildren,
		MaxParallel:       defaultSubagentMaxParallel,
		DefaultTimeoutSec: defaultSubagentDefaultTimeoutSec,
		MaxTimeoutSec:     defaultSubagentMaxTimeoutSec,
		MaxToolIterations: defaultSubagentToolIterations,
		MaxResultChars:    defaultSubagentResultChars,
		MaxAggregateCount: defaultSubagentAggregateCount,
		MaxAggregateChars: defaultSubagentAggregateChars,
	}
}

func (c *subagentRuntimeConfig) normalize() {
	if c.MaxDepth <= 0 {
		c.MaxDepth = defaultSubagentMaxDepth
	}
	if c.MaxChildren <= 0 {
		c.MaxChildren = defaultSubagentMaxChildren
	}
	if c.MaxParallel <= 0 {
		c.MaxParallel = defaultSubagentMaxParallel
	}
	if c.DefaultTimeoutSec <= 0 {
		c.DefaultTimeoutSec = defaultSubagentDefaultTimeoutSec
	}
	if c.MaxTimeoutSec <= 0 {
		c.MaxTimeoutSec = defaultSubagentMaxTimeoutSec
	}
	if c.MaxTimeoutSec < c.DefaultTimeoutSec {
		c.MaxTimeoutSec = c.DefaultTimeoutSec
	}
	if c.MaxToolIterations <= 0 {
		c.MaxToolIterations = defaultSubagentToolIterations
	}
	if c.MaxResultChars <= 0 {
		c.MaxResultChars = defaultSubagentResultChars
	}
	if c.MaxAggregateCount <= 0 {
		c.MaxAggregateCount = defaultSubagentAggregateCount
	}
	if c.MaxAggregateChars <= 0 {
		c.MaxAggregateChars = defaultSubagentAggregateChars
	}
}

type agentRuntime struct {
	sessionID         string
	depth             int
	role              agentRole
	allowedTools      map[string]bool
	maxToolIterations int
	executionProfile  RuntimeProfile
	model             string
	reasoning         string
}

// RuntimeProfile carries the selected application budget into a child-agent
// execution. The manager uses Subagents for child lifecycle limits and keeps
// the remaining fields available to the application runner for the child's
// own turn and tool execution.
type RuntimeProfile struct {
	MaxIterations      int
	MaxGoalIterations  int
	Subagents          Config
	ToolMaxParallel    int
	ToolTimeoutSec     int
	ToolRetryOnTimeout bool
}

type subagentStatus string

const (
	subagentStatusPending   subagentStatus = "pending"
	subagentStatusQueued    subagentStatus = "queued"
	subagentStatusRunning   subagentStatus = "running"
	subagentStatusCompleted subagentStatus = "completed"
	subagentStatusFailed    subagentStatus = "failed"
	subagentStatusCancelled subagentStatus = "cancelled"
	subagentStatusTimedOut  subagentStatus = "timed_out"
)

type subagentSession struct {
	ID            string
	Name          string
	Question      string
	ParentID      string
	Depth         int
	Role          agentRole
	ExecutionMode string
	AllowedTools  map[string]bool
	Timeout       time.Duration

	CreatedAt  time.Time
	StartedAt  time.Time
	FinishedAt time.Time

	Status          subagentStatus
	Error           string
	Output          string
	OutputTruncated bool
	profile         RuntimeProfile
	limits          subagentRuntimeConfig

	started   bool
	parentCtx context.Context // inherited from the parent agent's run() call
	done      chan struct{}
	doneOnce  sync.Once
	cancel    context.CancelFunc
	// queueDeadline bounds the time a started child may remain queued before
	// execution. It is set when run_subagent starts the child so the existing
	// create-then-run timeout behavior remains intact.
	queueDeadline time.Time
}

type subagentRunner func(ctx context.Context, runtime *agentRuntime, session *subagentSession) (string, error)

type subagentManager struct {
	cfg    subagentRuntimeConfig
	runner subagentRunner

	mu             sync.Mutex
	slotCond       *sync.Cond
	nextID         atomic.Uint64
	sessions       map[string]*subagentSession
	childrenByNode map[string][]string
	// activeChildren tracks children that have acquired an execution slot. It
	// keeps MaxChildren independent from the bounded admission queue: pending
	// handles can be created promptly, but only the configured number may run.
	activeChildren map[string]int
	parallelActive int
	serialActive   bool

	// parentContexts tracks the lifetime of the currently active parent turn.
	// It is separate from a child session context so create-then-run remains
	// usable across tool calls while cancellation can still clean up work that
	// has not reached execution yet.
	parentContexts map[string]*parentContextBinding
}

type parentContextBinding struct {
	ctx  context.Context
	stop chan struct{}
}

// Config controls the lifecycle and resource policy for child agents. The
// manager applies defaults for zero-valued limits so callers only need to set
// the policy that differs from the normal runtime.
type Config = subagentRuntimeConfig

// DefaultConfig returns the production subagent limits.
func DefaultConfig() Config { return defaultSubagentRuntimeConfig() }

// Runtime is the normalized execution context passed to a subagent runner.
// It deliberately contains no application or tool implementation details.
type Runtime struct {
	SessionID         string
	Depth             int
	Role              string
	AllowedTools      map[string]bool
	MaxToolIterations int
	ExecutionProfile  RuntimeProfile
	Model             string
	Reasoning         string
}

func internalRuntime(runtime *Runtime) *agentRuntime {
	if runtime == nil {
		return nil
	}
	return &agentRuntime{
		sessionID:         runtime.SessionID,
		depth:             runtime.Depth,
		role:              agentRole(runtime.Role),
		allowedTools:      cloneAllowedTools(runtime.AllowedTools),
		maxToolIterations: runtime.MaxToolIterations,
		executionProfile:  runtime.ExecutionProfile,
		model:             runtime.Model,
		reasoning:         runtime.Reasoning,
	}
}

func publicRuntime(runtime *agentRuntime) *Runtime {
	if runtime == nil {
		return nil
	}
	return &Runtime{
		SessionID:         runtime.sessionID,
		Depth:             runtime.depth,
		Role:              string(runtime.role),
		AllowedTools:      cloneAllowedTools(runtime.allowedTools),
		MaxToolIterations: runtime.maxToolIterations,
		ExecutionProfile:  runtime.executionProfile,
		Model:             runtime.model,
		Reasoning:         runtime.reasoning,
	}
}

// Session and the argument/envelope aliases keep the public capability seam
// small without duplicating the JSON contract used by the tool dispatcher.
type Session = subagentSession
type Status = subagentStatus
type Role = agentRole
type CreateArgs = createSubagentArgs
type RunArgs = runSubagentArgs
type AwaitArgs = awaitSubagentArgs
type ListArgs = listSubagentsArgs
type ReadArgs = readSubagentArgs
type CancelArgs = cancelSubagentArgs
type Envelope = subagentEnvelope
type AggregateEnvelope = subagentAggregateEnvelope

type Runner func(context.Context, *Runtime, *Session) (string, error)

// Manager owns child-agent lifecycle, visibility, limits, scheduling, and
// result aggregation. Application code supplies only the runner adapter.
type Manager struct{ core *subagentManager }

// New constructs a subagent manager behind the capability seam.
func New(cfg Config, runner Runner) *Manager {
	manager := &Manager{}
	manager.core = newSubagentManager(cfg, func(ctx context.Context, runtime *agentRuntime, session *subagentSession) (string, error) {
		if runner == nil {
			return "", errors.New("subagent runner is nil")
		}
		return runner(ctx, publicRuntime(runtime), session)
	})
	return manager
}

func (m *Manager) ListAll() []contracts.SubagentNode {
	if m == nil || m.core == nil {
		return nil
	}
	return m.core.ListAll()
}

func (m *Manager) Create(ctx context.Context, parent *Runtime, args CreateArgs) (*Session, error) {
	return m.core.create(ctx, internalRuntime(parent), args)
}

func (m *Manager) Run(ctx context.Context, parent *Runtime, args RunArgs) (*Session, error) {
	return m.core.run(ctx, internalRuntime(parent), args)
}

func (m *Manager) Await(ctx context.Context, parent *Runtime, args AwaitArgs) (*Session, error) {
	return m.core.await(ctx, internalRuntime(parent), args)
}

func (m *Manager) Cancel(parent *Runtime, args CancelArgs) (*Session, error) {
	return m.core.cancel(internalRuntime(parent), args)
}

func (m *Manager) List(parent *Runtime, args ListArgs) ([]Envelope, error) {
	return m.core.list(internalRuntime(parent), args)
}

func (m *Manager) Read(parent *Runtime, args ReadArgs) (any, error) {
	return m.core.read(internalRuntime(parent), args)
}

// BindParentContext associates a parent turn context with its child tree so
// cancellation can finalize pending work and stop queued or running children.
func (m *Manager) BindParentContext(parent *Runtime, ctx context.Context) {
	if m == nil || m.core == nil || parent == nil {
		return
	}
	m.core.bindParentContext(parent.SessionID, ctx)
}

// Snapshot returns a stable tool-result view of a session.
func (m *Manager) Snapshot(session *Session, includeOutput bool) Envelope {
	if m == nil || m.core == nil || session == nil {
		return Envelope{}
	}
	m.core.mu.Lock()
	defer m.core.mu.Unlock()
	return m.core.snapshotLocked(session, includeOutput)
}

func newSubagentManager(cfg subagentRuntimeConfig, runner subagentRunner) *subagentManager {
	cfg.normalize()
	m := &subagentManager{
		cfg:            cfg,
		runner:         runner,
		sessions:       map[string]*subagentSession{},
		childrenByNode: map[string][]string{},
		activeChildren: map[string]int{},
		parentContexts: map[string]*parentContextBinding{},
	}
	m.slotCond = sync.NewCond(&m.mu)
	return m
}

// ListAll returns a snapshot of all known subagent sessions (used by TUI agent tree panel).
// Does NOT include the top-level agents — those are managed by the TUI model.
func (m *subagentManager) ListAll() []contracts.SubagentNode {
	m.mu.Lock()
	defer m.mu.Unlock()
	nodes := make([]contracts.SubagentNode, 0, len(m.sessions))
	for _, s := range m.sessions {
		nodes = append(nodes, contracts.SubagentNode{
			ID:       s.ID,
			Name:     s.Name,
			Question: s.Question,
			ParentID: s.ParentID,
			Status:   string(s.Status),
			Depth:    s.Depth,
		})
	}
	slices.SortFunc(nodes, func(a, b contracts.SubagentNode) int {
		return strings.Compare(a.ID, b.ID)
	})
	return nodes
}

// profileFor resolves the profile active at a parent runtime. A runtime that
// predates profile propagation falls back to the manager's ordinary config so
// existing capability callers retain their established behavior.
func (m *subagentManager) profileFor(parent *agentRuntime) RuntimeProfile {
	profile := RuntimeProfile{}
	if parent != nil {
		profile = parent.executionProfile
	}
	limits := profile.Subagents
	if limits == (subagentRuntimeConfig{}) {
		limits = m.cfg
		if parent != nil && parent.maxToolIterations > 0 {
			profile.MaxIterations = parent.maxToolIterations
		}
	}
	limits.normalize()
	profile.Subagents = limits
	// A child loop is bounded by the selected subagent iteration limit, while
	// tool and policy limits continue to come from the inherited full profile.
	profile.MaxIterations = limits.MaxToolIterations
	return profile
}

func (m *subagentManager) create(ctx context.Context, parent *agentRuntime, args createSubagentArgs) (*subagentSession, error) {
	if parent == nil {
		return nil, errors.New("parent runtime is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("create_subagent: %w", err)
	}
	question := strings.TrimSpace(args.Question)
	if question == "" {
		return nil, errors.New("create_subagent question is required")
	}
	profile := m.profileFor(parent)
	limits := profile.Subagents
	depth := parent.depth + 1
	if depth > limits.MaxDepth {
		return nil, fmt.Errorf("max subagent depth exceeded: requested depth %d, max %d", depth, limits.MaxDepth)
	}
	timeoutSec, err := m.resolveTimeoutSeconds(args.TimeoutSeconds, limits)
	if err != nil {
		return nil, err
	}
	overflowMode, err := normalizeCreateSubagentOverflowMode(args.OverflowMode)
	if err != nil {
		return nil, err
	}
	if overflowMode == createSubagentOverflowWaitForSlot {
		// Retain validation of the legacy argument, but never spend the
		// parent tool-batch budget waiting for it to become available.
		if _, err := m.resolveWaitTimeoutSeconds(args.WaitTimeoutSeconds, limits); err != nil {
			return nil, err
		}
	}
	allowed, err := deriveChildAllowedTools(parent.allowedTools, args.AllowedTools, depth, limits.MaxDepth)
	if err != nil {
		return nil, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("create_subagent: %w", err)
	}

	// Creation is deliberately non-blocking. A parent tool batch may contain
	// creates that can only be made runnable by a later batch of run calls, so
	// waiting here would deadlock the batch at the child-capacity boundary.
	// MaxChildren limits work that may hold an execution slot, while a bounded
	// admission allowance keeps overflow handles recoverable without permitting
	// unbounded session growth. Count every non-terminal handle here: once a
	// queued handle has been run it must still consume queue capacity until it
	// finishes, otherwise repeated create/run batches could grow without bound.
	admitted := m.admittedChildrenLocked(parent.sessionID)
	if admitted >= limits.MaxChildren && overflowMode == createSubagentOverflowFailFast {
		return nil, fmt.Errorf("max children exceeded for parent %q (%d)", parent.sessionID, limits.MaxChildren)
	}
	if admitted >= limits.MaxChildren+limits.MaxParallel {
		return nil, fmt.Errorf("subagent admission capacity exceeded for parent %q; run admitted children and retry after a child reaches a terminal state", parent.sessionID)
	}

	id := fmt.Sprintf("subagent-%d", m.nextID.Add(1))
	now := time.Now().UTC()
	session := &subagentSession{
		ID:            id,
		Name:          strings.TrimSpace(args.Name),
		Question:      question,
		ParentID:      parent.sessionID,
		Depth:         depth,
		Role:          agentRoleWorker,
		ExecutionMode: strings.ToLower(strings.TrimSpace(args.ExecutionMode)),
		AllowedTools:  allowed,
		Timeout:       time.Duration(timeoutSec) * time.Second,
		profile:       profile,
		limits:        limits,
		CreatedAt:     now,
		Status:        subagentStatusPending,
		done:          make(chan struct{}),
	}
	if session.ExecutionMode == "" {
		if limits.MaxParallel > 1 {
			session.ExecutionMode = "parallel"
		} else {
			session.ExecutionMode = "sequential"
		}
	}
	if session.ExecutionMode != "sequential" && session.ExecutionMode != "parallel" {
		return nil, fmt.Errorf("invalid execution_mode %q", session.ExecutionMode)
	}
	m.sessions[id] = session
	m.childrenByNode[parent.sessionID] = append(m.childrenByNode[parent.sessionID], id)
	return cloneSession(session), nil
}

func (m *subagentManager) run(ctx context.Context, parent *agentRuntime, args runSubagentArgs) (*subagentSession, error) {
	if parent == nil {
		return nil, errors.New("parent runtime is required")
	}
	id := strings.TrimSpace(args.ID)
	if id == "" {
		return nil, errors.New("run_subagent id is required")
	}
	modeOverride := strings.ToLower(strings.TrimSpace(args.ExecutionMode))
	if modeOverride != "" && modeOverride != "sequential" && modeOverride != "parallel" {
		return nil, fmt.Errorf("invalid execution_mode %q", modeOverride)
	}

	m.mu.Lock()
	session, err := m.getVisibleSessionLocked(parent, id)
	if err != nil {
		m.mu.Unlock()
		return nil, err
	}
	switch session.Status {
	case subagentStatusCompleted, subagentStatusFailed, subagentStatusTimedOut, subagentStatusCancelled:
		current := cloneSession(session)
		m.mu.Unlock()
		return current, nil
	}
	if session.ParentID != parent.sessionID {
		m.mu.Unlock()
		return nil, fmt.Errorf("subagent %q can only be started by its direct parent", id)
	}
	if session.started {
		current := cloneSession(session)
		m.mu.Unlock()
		if args.Wait {
			return m.await(ctx, parent, awaitSubagentArgs{ID: id, TimeoutSeconds: args.TimeoutSeconds})
		}
		return current, nil
	}
	mode := modeOverride
	if mode == "" {
		mode = strings.ToLower(strings.TrimSpace(session.ExecutionMode))
	}
	if mode == "" {
		mode = "sequential"
	}

	session.started = true
	session.ExecutionMode = mode
	session.Status = subagentStatusQueued
	session.queueDeadline = time.Now().Add(session.Timeout)
	if args.Wait {
		session.parentCtx = ctx
	} else {
		// The application tool dispatcher receives a short-lived per-call
		// context, so use the parent-turn binding when one is available. Direct
		// manager callers still get useful cancellation semantics from ctx.
		session.parentCtx = context.Background()
		if binding := m.parentContexts[parent.sessionID]; binding != nil {
			session.parentCtx = binding.ctx
		} else if ctx != nil && ctx.Done() != nil {
			session.parentCtx = ctx
		}
	}
	runSession := session
	queued := cloneSession(session)
	m.mu.Unlock()

	go m.execute(runSession)

	if args.Wait {
		return m.await(ctx, parent, awaitSubagentArgs{ID: id, TimeoutSeconds: args.TimeoutSeconds})
	}
	return queued, nil
}

func (m *subagentManager) acquireChildSlot(session *subagentSession) bool {
	return m.waitForAdmission(session,
		func() bool { return m.activeChildren[session.ParentID] >= session.limits.MaxChildren },
		func() { m.activeChildren[session.ParentID]++ },
	)
}

func (m *subagentManager) releaseChildSlot(session *subagentSession) {
	m.mu.Lock()
	if active := m.activeChildren[session.ParentID]; active > 1 {
		m.activeChildren[session.ParentID] = active - 1
	} else {
		delete(m.activeChildren, session.ParentID)
	}
	m.slotCond.Broadcast()
	m.mu.Unlock()
}

func (m *subagentManager) acquireParallel(session *subagentSession) bool {
	return m.waitForAdmission(session,
		func() bool { return m.parallelActive >= session.limits.MaxParallel },
		func() { m.parallelActive++ },
	)
}

func (m *subagentManager) acquireSerial(session *subagentSession) bool {
	return m.waitForAdmission(session,
		func() bool { return m.serialActive },
		func() { m.serialActive = true },
	)
}

// waitForAdmission waits for one scheduler resource while also observing the
// parent context and the child deadline. The condition variable has no native
// timed wait, so both cancellation and deadline callbacks wake it explicitly.
func (m *subagentManager) waitForAdmission(session *subagentSession, unavailable func() bool, reserve func()) bool {
	ctx := session.parentCtx
	stopCtxWatch := make(chan struct{})
	if ctx != nil && ctx.Done() != nil {
		go func() {
			select {
			case <-ctx.Done():
				m.mu.Lock()
				m.slotCond.Broadcast()
				m.mu.Unlock()
			case <-stopCtxWatch:
			}
		}()
	}
	var deadlineTimer *time.Timer
	if !session.queueDeadline.IsZero() {
		deadlineTimer = time.AfterFunc(time.Until(session.queueDeadline), func() {
			m.mu.Lock()
			m.slotCond.Broadcast()
			m.mu.Unlock()
		})
	}
	defer close(stopCtxWatch)
	if deadlineTimer != nil {
		defer deadlineTimer.Stop()
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	for unavailable() {
		if m.admissionUnavailableLocked(session) {
			return false
		}
		m.slotCond.Wait()
	}
	if m.admissionUnavailableLocked(session) {
		return false
	}
	reserve()
	return true
}

func (m *subagentManager) admissionUnavailableLocked(session *subagentSession) bool {
	if session.Status == subagentStatusCancelled {
		return true
	}
	if ctx := session.parentCtx; ctx != nil && ctx.Err() != nil {
		return true
	}
	return !session.queueDeadline.IsZero() && !time.Now().Before(session.queueDeadline)
}

func (m *subagentManager) releaseParallel() {
	m.mu.Lock()
	if m.parallelActive > 0 {
		m.parallelActive--
	}
	m.slotCond.Broadcast()
	m.mu.Unlock()
}

func (m *subagentManager) releaseSerial() {
	m.mu.Lock()
	m.serialActive = false
	m.slotCond.Broadcast()
	m.mu.Unlock()
}

func (m *subagentManager) execute(session *subagentSession) {
	childAcquired := m.acquireChildSlot(session)
	if !childAcquired {
		m.finalizeBeforeExecution(session)
		return
	}
	defer m.releaseChildSlot(session)

	parallelAcquired := false
	serialAcquired := false
	if session.ExecutionMode == "sequential" {
		serialAcquired = m.acquireSerial(session)
		if !serialAcquired {
			m.finalizeBeforeExecution(session)
			return
		}
	} else {
		parallelAcquired = m.acquireParallel(session)
		if !parallelAcquired {
			m.finalizeBeforeExecution(session)
			return
		}
	}
	defer func() {
		if serialAcquired {
			m.releaseSerial()
		} else if parallelAcquired {
			m.releaseParallel()
		}
	}()

	m.mu.Lock()
	if session.Status == subagentStatusCancelled || m.admissionUnavailableLocked(session) {
		m.mu.Unlock()
		m.finalizeBeforeExecution(session)
		return
	}
	startedAt := time.Now().UTC()
	session.StartedAt = startedAt
	session.Status = subagentStatusRunning
	baseCtx := session.parentCtx
	if baseCtx == nil {
		baseCtx = context.Background()
	}
	execTimeout := session.Timeout
	if !session.queueDeadline.IsZero() {
		remaining := time.Until(session.queueDeadline)
		if remaining < execTimeout {
			execTimeout = remaining
		}
	}
	if execTimeout <= 0 {
		session.Status = subagentStatusTimedOut
		session.Error = fmt.Sprintf("subagent timed out after %s", session.Timeout)
		session.FinishedAt = time.Now().UTC()
		session.closeDone()
		m.mu.Unlock()
		return
	}
	execCtx, cancel := context.WithTimeout(baseCtx, execTimeout)
	session.cancel = cancel
	m.mu.Unlock()

	runtime := &agentRuntime{
		sessionID:         session.ID,
		depth:             session.Depth,
		role:              session.Role,
		allowedTools:      cloneAllowedTools(session.AllowedTools),
		maxToolIterations: session.profile.MaxIterations,
		executionProfile:  session.profile,
		model:             session.limits.Model,
		reasoning:         session.limits.ReasoningEffort,
	}
	output, runErr := m.runner(execCtx, runtime, cloneSession(session))

	// Capture context error BEFORE cancel() since cancel() sets execCtx.Err() to context.Canceled
	execCtxErr := execCtx.Err()
	cancel()

	m.mu.Lock()
	defer m.mu.Unlock()
	session.cancel = nil
	session.FinishedAt = time.Now().UTC()
	session.Output, session.OutputTruncated = truncateText(output, session.limits.MaxResultChars)

	switch {
	case errors.Is(execCtxErr, context.DeadlineExceeded):
		session.Status = subagentStatusTimedOut
		session.Error = fmt.Sprintf("subagent timed out after %s", session.Timeout)
	case runErr != nil:
		if errors.Is(runErr, context.Canceled) || errors.Is(execCtxErr, context.Canceled) {
			session.Status = subagentStatusCancelled
			session.Error = "subagent cancelled"
		} else {
			session.Status = subagentStatusFailed
			session.Error = runErr.Error()
		}
	default:
		session.Status = subagentStatusCompleted
		session.Error = ""
	}
	session.closeDone()
	m.slotCond.Broadcast()
}

func (m *subagentManager) finalizeBeforeExecution(session *subagentSession) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if isTerminalSubagentStatus(session.Status) {
		return
	}
	if !session.queueDeadline.IsZero() && !time.Now().Before(session.queueDeadline) {
		session.Status = subagentStatusTimedOut
		session.Error = fmt.Sprintf("subagent timed out after %s", session.Timeout)
	} else {
		session.Status = subagentStatusCancelled
		session.Error = "subagent cancelled before execution"
	}
	session.FinishedAt = time.Now().UTC()
	session.closeDone()
	m.slotCond.Broadcast()
}

func (m *subagentManager) await(ctx context.Context, parent *agentRuntime, args awaitSubagentArgs) (*subagentSession, error) {
	if parent == nil {
		return nil, errors.New("parent runtime is required")
	}
	id := strings.TrimSpace(args.ID)
	if id == "" {
		return nil, errors.New("await_subagent id is required")
	}
	limits := m.profileFor(parent).Subagents
	m.mu.Lock()
	session, err := m.getVisibleSessionLocked(parent, id)
	if err != nil {
		m.mu.Unlock()
		return nil, err
	}
	done := session.done
	m.mu.Unlock()

	waitCtx := ctx
	var cancel context.CancelFunc
	if args.TimeoutSeconds > 0 {
		if args.TimeoutSeconds > limits.MaxTimeoutSec {
			return nil, fmt.Errorf("await_subagent timeout_seconds exceeds %d", limits.MaxTimeoutSec)
		}
		waitCtx, cancel = context.WithTimeout(ctx, time.Duration(args.TimeoutSeconds)*time.Second)
		defer cancel()
	}
	select {
	case <-done:
	case <-waitCtx.Done():
		return nil, fmt.Errorf("await_subagent: %w", waitCtx.Err())
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	latest, err := m.getVisibleSessionLocked(parent, id)
	if err != nil {
		return nil, err
	}
	return cloneSession(latest), nil
}

func (m *subagentManager) cancel(parent *agentRuntime, args cancelSubagentArgs) (*subagentSession, error) {
	if parent == nil {
		return nil, errors.New("parent runtime is required")
	}
	id := strings.TrimSpace(args.ID)
	if id == "" {
		return nil, errors.New("cancel_subagent id is required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	session, err := m.getVisibleSessionLocked(parent, id)
	if err != nil {
		return nil, err
	}
	switch session.Status {
	case subagentStatusCompleted, subagentStatusFailed, subagentStatusTimedOut, subagentStatusCancelled:
		return cloneSession(session), nil
	}
	if session.cancel != nil {
		session.cancel()
	} else {
		session.Status = subagentStatusCancelled
		session.Error = "subagent cancelled before execution"
		session.FinishedAt = time.Now().UTC()
		session.closeDone()
		m.slotCond.Broadcast()
	}
	return cloneSession(session), nil
}

func (m *subagentManager) list(parent *agentRuntime, args listSubagentsArgs) ([]subagentEnvelope, error) {
	if parent == nil {
		return nil, errors.New("parent runtime is required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	out := make([]subagentEnvelope, 0, len(m.sessions))
	for _, session := range m.sessions {
		if !m.isVisibleLocked(parent, session) {
			continue
		}
		if !args.IncludeDescendants && session.ParentID != parent.sessionID {
			continue
		}
		out = append(out, m.snapshotLocked(session, false))
	}
	slices.SortFunc(out, func(a, b subagentEnvelope) int { return strings.Compare(a.ID, b.ID) })
	return out, nil
}

func (m *subagentManager) read(parent *agentRuntime, args readSubagentArgs) (any, error) {
	if parent == nil {
		return nil, errors.New("parent runtime is required")
	}
	ids := filterNonEmpty(args.IDs)
	id := strings.TrimSpace(args.ID)
	switch {
	case id == "" && len(ids) == 0:
		return nil, errors.New("read_subagent requires id or ids")
	case id != "" && len(ids) > 0:
		return nil, errors.New("read_subagent accepts either id or ids, not both")
	}
	includeOutput := true
	if args.IncludeOutput != nil {
		includeOutput = *args.IncludeOutput
	}

	if id != "" {
		m.mu.Lock()
		defer m.mu.Unlock()
		session, err := m.getVisibleSessionLocked(parent, id)
		if err != nil {
			return nil, err
		}
		snap := m.snapshotLocked(session, includeOutput)
		if !includeOutput {
			snap.Output = ""
		}
		return snap, nil
	}

	limits := m.profileFor(parent).Subagents
	if len(ids) > limits.MaxAggregateCount {
		return nil, fmt.Errorf("aggregation exceeds max ids (%d)", limits.MaxAggregateCount)
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	items := make([]subagentEnvelope, 0, len(ids))
	for _, itemID := range ids {
		session, err := m.getVisibleSessionLocked(parent, itemID)
		if err != nil {
			return nil, err
		}
		items = append(items, m.snapshotLocked(session, includeOutput))
	}
	aggregate := buildAggregateEnvelope(items, limits.MaxAggregateChars)
	if !includeOutput {
		for i := range aggregate.Items {
			aggregate.Items[i].Output = ""
		}
		aggregate.CombinedOutput = ""
	}
	return aggregate, nil
}

func (m *subagentManager) snapshotLocked(session *subagentSession, includeOutput bool) subagentEnvelope {
	allowedTools := make([]string, 0, len(session.AllowedTools))
	for name, enabled := range session.AllowedTools {
		if enabled {
			allowedTools = append(allowedTools, name)
		}
	}
	slices.Sort(allowedTools)

	snap := subagentEnvelope{
		ID:              session.ID,
		Name:            session.Name,
		ParentID:        session.ParentID,
		Role:            string(session.Role),
		Depth:           session.Depth,
		Status:          string(session.Status),
		ExecutionMode:   session.ExecutionMode,
		TimeoutSeconds:  int(session.Timeout.Seconds()),
		AllowedTools:    allowedTools,
		CreatedAt:       session.CreatedAt.Format(time.RFC3339Nano),
		OutputTruncated: session.OutputTruncated,
		Error:           session.Error,
	}
	if !session.StartedAt.IsZero() {
		snap.StartedAt = session.StartedAt.Format(time.RFC3339Nano)
	}
	if !session.FinishedAt.IsZero() {
		snap.FinishedAt = session.FinishedAt.Format(time.RFC3339Nano)
	}
	if includeOutput {
		snap.Output = session.Output
	}
	return snap
}

func (m *subagentManager) getVisibleSessionLocked(parent *agentRuntime, id string) (*subagentSession, error) {
	session, ok := m.sessions[id]
	if !ok {
		return nil, fmt.Errorf("subagent %q not found", id)
	}
	if !m.isVisibleLocked(parent, session) {
		return nil, fmt.Errorf("subagent %q is not visible to this agent", id)
	}
	return session, nil
}

func (m *subagentManager) isVisibleLocked(parent *agentRuntime, session *subagentSession) bool {
	if parent.sessionID == rootAgentID {
		return true
	}
	current := session.ParentID
	for current != "" {
		if current == parent.sessionID {
			return true
		}
		next := m.sessions[current]
		if next == nil {
			break
		}
		current = next.ParentID
	}
	return false
}

func (m *subagentManager) resolveTimeoutSeconds(requested int, limits subagentRuntimeConfig) (int, error) {
	if requested <= 0 {
		return limits.DefaultTimeoutSec, nil
	}
	if requested > limits.MaxTimeoutSec {
		return 0, fmt.Errorf("timeout_seconds exceeds %d", limits.MaxTimeoutSec)
	}
	return requested, nil
}

func (m *subagentManager) resolveWaitTimeoutSeconds(requested int, limits subagentRuntimeConfig) (int, error) {
	if requested <= 0 {
		return limits.DefaultTimeoutSec, nil
	}
	if requested > limits.MaxTimeoutSec {
		return 0, fmt.Errorf("wait_timeout_seconds exceeds %d", limits.MaxTimeoutSec)
	}
	return requested, nil
}

func (m *subagentManager) admittedChildrenLocked(parentID string) int {
	count := 0
	for _, id := range m.childrenByNode[parentID] {
		session := m.sessions[id]
		if session != nil && !isTerminalSubagentStatus(session.Status) {
			count++
		}
	}
	return count
}

func isTerminalSubagentStatus(status subagentStatus) bool {
	switch status {
	case subagentStatusCompleted, subagentStatusFailed, subagentStatusCancelled, subagentStatusTimedOut:
		return true
	default:
		return false
	}
}

func normalizeCreateSubagentOverflowMode(raw string) (string, error) {
	mode := strings.ToLower(strings.TrimSpace(raw))
	if mode == "" {
		return createSubagentOverflowWaitForSlot, nil
	}
	if mode != createSubagentOverflowWaitForSlot && mode != createSubagentOverflowFailFast {
		return "", fmt.Errorf("invalid overflow_mode %q", raw)
	}
	return mode, nil
}

type subagentEnvelope struct {
	ID              string   `json:"id"`
	Name            string   `json:"name,omitempty"`
	ParentID        string   `json:"parent_id"`
	Role            string   `json:"role"`
	Depth           int      `json:"depth"`
	Status          string   `json:"status"`
	ExecutionMode   string   `json:"execution_mode"`
	TimeoutSeconds  int      `json:"timeout_seconds"`
	AllowedTools    []string `json:"allowed_tools"`
	CreatedAt       string   `json:"created_at"`
	StartedAt       string   `json:"started_at,omitempty"`
	FinishedAt      string   `json:"finished_at,omitempty"`
	Output          string   `json:"output,omitempty"`
	OutputTruncated bool     `json:"output_truncated"`
	Error           string   `json:"error,omitempty"`
}

type subagentAggregateEnvelope struct {
	Kind            string             `json:"kind"`
	Count           int                `json:"count"`
	Completed       int                `json:"completed"`
	Failed          int                `json:"failed"`
	Cancelled       int                `json:"cancelled"`
	TimedOut        int                `json:"timed_out"`
	Running         int                `json:"running"`
	Pending         int                `json:"pending"`
	Queued          int                `json:"queued"`
	QueuedOrPending int                `json:"queued_or_pending"`
	Items           []subagentEnvelope `json:"items"`
	CombinedOutput  string             `json:"combined_output,omitempty"`
	CombinedTrimmed bool               `json:"combined_trimmed"`
}

func buildAggregateEnvelope(items []subagentEnvelope, maxChars int) subagentAggregateEnvelope {
	agg := subagentAggregateEnvelope{
		Kind:  "aggregate",
		Count: len(items),
		Items: items,
	}
	var b strings.Builder
	for i, item := range items {
		switch item.Status {
		case string(subagentStatusCompleted):
			agg.Completed++
		case string(subagentStatusFailed):
			agg.Failed++
		case string(subagentStatusCancelled):
			agg.Cancelled++
		case string(subagentStatusTimedOut):
			agg.TimedOut++
		case string(subagentStatusRunning):
			agg.Running++
		case string(subagentStatusPending):
			agg.Pending++
			agg.QueuedOrPending++
		case string(subagentStatusQueued):
			agg.Queued++
			agg.QueuedOrPending++
		default:
			agg.QueuedOrPending++
		}
		if strings.TrimSpace(item.Output) == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		fmt.Fprintf(&b, "## %d) %s [%s]\n%s", i+1, item.ID, item.Status, item.Output)
	}
	agg.CombinedOutput, agg.CombinedTrimmed = truncateText(b.String(), maxChars)
	return agg
}

func deriveChildAllowedTools(parentAllowed map[string]bool, requested []string, depth, maxDepth int) (map[string]bool, error) {
	return policy.InheritChildTools(parentAllowed, requested, depth, maxDepth)
}

func cloneAllowedTools(in map[string]bool) map[string]bool {
	return policy.CloneAllowedTools(in)
}

func cloneSession(in *subagentSession) *subagentSession {
	if in == nil {
		return nil
	}
	return &subagentSession{
		ID:              in.ID,
		Name:            in.Name,
		Question:        in.Question,
		ParentID:        in.ParentID,
		Depth:           in.Depth,
		Role:            in.Role,
		ExecutionMode:   in.ExecutionMode,
		AllowedTools:    cloneAllowedTools(in.AllowedTools),
		Timeout:         in.Timeout,
		CreatedAt:       in.CreatedAt,
		StartedAt:       in.StartedAt,
		FinishedAt:      in.FinishedAt,
		Status:          in.Status,
		Error:           in.Error,
		Output:          in.Output,
		OutputTruncated: in.OutputTruncated,
		profile:         in.profile,
		limits:          in.limits,
		started:         in.started,
		parentCtx:       in.parentCtx,
		done:            in.done,
		cancel:          in.cancel,
	}
}

func truncateText(in string, max int) (string, bool) {
	if max <= 0 || len(in) <= max {
		return in, false
	}
	return in[:max] + "\n\n[... truncated ...]", true
}

func filterNonEmpty(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		trimmed := strings.TrimSpace(value)
		if trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

func (s *subagentSession) closeDone() {
	s.doneOnce.Do(func() {
		close(s.done)
	})
}

// bindParentContext connects a parent turn's lifetime to its child tree. A
// non-cancellable context is still retained as the lifetime source for normal
// two-phase tool calls, but does not need a watcher.
func (m *subagentManager) bindParentContext(parentID string, ctx context.Context) {
	if m == nil || strings.TrimSpace(parentID) == "" || ctx == nil {
		return
	}
	binding := &parentContextBinding{ctx: ctx, stop: make(chan struct{})}
	m.mu.Lock()
	if previous := m.parentContexts[parentID]; previous != nil {
		close(previous.stop)
	}
	m.parentContexts[parentID] = binding
	m.mu.Unlock()
	if ctx.Done() == nil {
		return
	}

	go func() {
		select {
		case <-ctx.Done():
			m.mu.Lock()
			if m.parentContexts[parentID] == binding {
				delete(m.parentContexts, parentID)
				m.cancelChildrenLocked(parentID)
			}
			m.mu.Unlock()
		case <-binding.stop:
		}
	}()
}

// cancelChildrenLocked finalizes work that has not entered the runner and
// requests cancellation from work that is already running. It is called while
// m.mu is held so every terminal transition wakes slot waiters.
func (m *subagentManager) cancelChildrenLocked(parentID string) {
	for _, session := range m.sessions {
		if session.ParentID != parentID && !m.isDescendantOfLocked(session, parentID) {
			continue
		}
		if isTerminalSubagentStatus(session.Status) {
			continue
		}
		if session.cancel != nil {
			session.cancel()
			continue
		}
		session.Status = subagentStatusCancelled
		session.Error = "subagent cancelled with its parent"
		session.FinishedAt = time.Now().UTC()
		session.closeDone()
	}
	m.slotCond.Broadcast()
}

func (m *subagentManager) isDescendantOfLocked(session *subagentSession, ancestorID string) bool {
	current := session.ParentID
	for current != "" {
		if current == ancestorID {
			return true
		}
		parent := m.sessions[current]
		if parent == nil {
			return false
		}
		current = parent.ParentID
	}
	return false
}
