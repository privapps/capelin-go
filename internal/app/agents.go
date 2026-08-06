package app

import (
	configpkg "capelin-go/internal/config"
	"capelin-go/internal/contracts"
	"capelin-go/internal/policy"
	"capelin-go/internal/subagents"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
)

const rootAgentID = "root"

const (
	createSubagentOverflowWaitForSlot = "wait_for_slot"
	createSubagentOverflowFailFast    = "fail_fast"
)

const (
	defaultSubagentMaxDepth          = 1
	defaultSubagentMaxChildren       = 8
	defaultSubagentMaxParallel       = 4
	defaultSubagentDefaultTimeoutSec = 600
	defaultSubagentMaxTimeoutSec     = 1800
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

// subagentRuntimeConfig remains an application-level compatibility alias for
// parsed configuration. Lifecycle policy and execution are owned by the
// internal/subagents package.
type subagentRuntimeConfig = subagents.Config

func defaultSubagentRuntimeConfig() subagentRuntimeConfig {
	return subagents.DefaultConfig()
}

func toSubagentConfig(cfg configpkg.SubagentConfig) subagents.Config {
	return subagents.Config{
		MaxDepth:          cfg.MaxDepth,
		MaxChildren:       cfg.MaxChildren,
		MaxParallel:       cfg.MaxParallel,
		DefaultTimeoutSec: cfg.DefaultTimeoutSec,
		MaxTimeoutSec:     cfg.MaxTimeoutSec,
		MaxToolIterations: cfg.MaxToolIterations,
		MaxResultChars:    cfg.MaxResultChars,
		MaxAggregateCount: cfg.MaxAggregateCount,
		MaxAggregateChars: cfg.MaxAggregateChars,
		Model:             cfg.Model,
		ReasoningEffort:   cfg.ReasoningEffort,
	}
}

func fromSubagentConfig(cfg subagents.Config) configpkg.SubagentConfig {
	return configpkg.SubagentConfig{
		MaxDepth:          cfg.MaxDepth,
		MaxChildren:       cfg.MaxChildren,
		MaxParallel:       cfg.MaxParallel,
		DefaultTimeoutSec: cfg.DefaultTimeoutSec,
		MaxTimeoutSec:     cfg.MaxTimeoutSec,
		MaxToolIterations: cfg.MaxToolIterations,
		MaxResultChars:    cfg.MaxResultChars,
		MaxAggregateCount: cfg.MaxAggregateCount,
		MaxAggregateChars: cfg.MaxAggregateChars,
		Model:             cfg.Model,
		ReasoningEffort:   cfg.ReasoningEffort,
	}
}

func toSubagentProfile(profile configpkg.RuntimeProfile) subagents.RuntimeProfile {
	return subagents.RuntimeProfile{
		MaxIterations:      profile.MaxIterations,
		MaxGoalIterations:  profile.MaxGoalIterations,
		Subagents:          toSubagentConfig(profile.Subagents),
		ToolMaxParallel:    profile.ToolMaxParallel,
		ToolTimeoutSec:     profile.ToolTimeoutSec,
		ToolRetryOnTimeout: profile.ToolRetryOnTimeout,
	}
}

func fromSubagentProfile(profile subagents.RuntimeProfile) configpkg.RuntimeProfile {
	return configpkg.RuntimeProfile{
		MaxIterations:      profile.MaxIterations,
		MaxGoalIterations:  profile.MaxGoalIterations,
		Subagents:          fromSubagentConfig(profile.Subagents),
		ToolMaxParallel:    profile.ToolMaxParallel,
		ToolTimeoutSec:     profile.ToolTimeoutSec,
		ToolRetryOnTimeout: profile.ToolRetryOnTimeout,
	}
}

type agentRuntime struct {
	sessionID         string
	depth             int
	role              agentRole
	allowedTools      map[string]bool
	maxToolIterations int
	executionProfile  configpkg.RuntimeProfile
	model             string
	reasoning         string

	todosMu        sync.RWMutex
	todos          []todoItem
	todosChanged   func([]todoItem)
	toolErrorMu    sync.Mutex
	fatalError     error
	recoveryErr    error
	goalMu         sync.Mutex
	goalEnabled    bool
	goalGeneration uint64
	goalClaim      *goalCompletion
}

// selectExecutionProfile changes only this runtime's value profile and
// returns a restoration function for the surrounding turn or goal scope.
// Profiles are deliberately not stored on app config or session snapshots.
func (r *agentRuntime) selectExecutionProfile(profile configpkg.RuntimeProfile) func() {
	if r == nil {
		return func() {}
	}
	previous := r.executionProfile
	previousIterations := r.maxToolIterations
	r.executionProfile = profile
	if profile.MaxIterations > 0 {
		r.maxToolIterations = profile.MaxIterations
	}
	return func() {
		r.executionProfile = previous
		r.maxToolIterations = previousIterations
	}
}

func (r *agentRuntime) replaceTodos(todos []todoItem) {
	updated := cloneTodos(todos)
	// Serialize checklist replacement with completion-claim publication. If
	// todosMu were released before the claim was cleared, an old claim could be
	// published after an identical replacement and its fingerprint would look
	// current again.
	r.goalMu.Lock()
	r.todosMu.Lock()
	r.todos = updated
	changed := r.todosChanged
	callbackTodos := cloneTodos(updated)
	r.todosMu.Unlock()
	r.goalClaim = nil
	r.goalMu.Unlock()
	if changed != nil {
		changed(callbackTodos)
	}
}

func (r *agentRuntime) snapshotTodos() []todoItem {
	r.todosMu.RLock()
	defer r.todosMu.RUnlock()
	return cloneTodos(r.todos)
}

func (r *agentRuntime) resetToolError() {
	if r == nil {
		return
	}
	r.toolErrorMu.Lock()
	r.fatalError = nil
	r.recoveryErr = nil
	r.toolErrorMu.Unlock()
}

func (r *agentRuntime) recordFatalError(err error) {
	if r == nil || err == nil {
		return
	}
	r.toolErrorMu.Lock()
	if r.fatalError == nil {
		r.fatalError = err
	}
	r.toolErrorMu.Unlock()
}

func (r *agentRuntime) recordRecoverableToolError(err error) {
	if r == nil || err == nil {
		return
	}
	r.toolErrorMu.Lock()
	if r.recoveryErr == nil {
		r.recoveryErr = err
	}
	r.toolErrorMu.Unlock()
}

func (r *agentRuntime) recordedFatalError() error {
	if r == nil {
		return nil
	}
	r.toolErrorMu.Lock()
	defer r.toolErrorMu.Unlock()
	return r.fatalError
}

func (r *agentRuntime) hadRecoverableToolError() bool {
	if r == nil {
		return false
	}
	r.toolErrorMu.Lock()
	defer r.toolErrorMu.Unlock()
	return r.recoveryErr != nil
}

func (r *agentRuntime) enableGoal(generation uint64) {
	if r == nil {
		return
	}
	r.goalMu.Lock()
	r.goalEnabled = true
	r.goalGeneration = generation
	r.goalClaim = nil
	r.goalMu.Unlock()
}

func (r *agentRuntime) disableGoal() {
	if r == nil {
		return
	}
	r.goalMu.Lock()
	r.goalEnabled = false
	r.goalGeneration = 0
	r.goalClaim = nil
	r.goalMu.Unlock()
}

func (r *agentRuntime) goalIsEnabled() bool {
	if r == nil {
		return false
	}
	r.goalMu.Lock()
	defer r.goalMu.Unlock()
	return r.goalEnabled
}

func (r *agentRuntime) recordGoalClaim(summary string, evidence []string) error {
	if r == nil {
		return errors.New("goal completion capability is unavailable")
	}
	r.goalMu.Lock()
	defer r.goalMu.Unlock()
	if !r.goalEnabled {
		return errors.New("complete_goal is available only during an autonomous goal")
	}
	r.todosMu.RLock()
	todos := cloneTodos(r.todos)
	r.todosMu.RUnlock()
	claim := &goalCompletion{
		Summary:              strings.TrimSpace(summary),
		Evidence:             append([]string(nil), evidence...),
		Generation:           r.goalGeneration,
		ChecklistFingerprint: todosFingerprint(todos),
	}
	r.goalClaim = claim
	return nil
}

func (r *agentRuntime) invalidateGoalClaim() {
	if r == nil {
		return
	}
	r.goalMu.Lock()
	r.goalClaim = nil
	r.goalMu.Unlock()
}

func (r *agentRuntime) goalGenerationValue() uint64 {
	r.goalMu.Lock()
	defer r.goalMu.Unlock()
	return r.goalGeneration
}

func (r *agentRuntime) snapshotGoalClaim() *goalCompletion {
	if r == nil {
		return nil
	}
	r.goalMu.Lock()
	defer r.goalMu.Unlock()
	return cloneGoalCompletion(r.goalClaim)
}

// The old names remain local compatibility helpers for callers that only
// need the fatal latch semantics.
func (r *agentRuntime) recordToolError(err error) { r.recordFatalError(err) }
func (r *agentRuntime) recordedToolError() error  { return r.recordedFatalError() }

// These local value types are compatibility adapters for the existing tool
// hook seam. The subagents package owns their lifecycle implementation.
type subagentStatus = subagents.Status

const (
	subagentStatusPending   = subagents.Status("pending")
	subagentStatusQueued    = subagents.Status("queued")
	subagentStatusRunning   = subagents.Status("running")
	subagentStatusCompleted = subagents.Status("completed")
	subagentStatusFailed    = subagents.Status("failed")
	subagentStatusCancelled = subagents.Status("cancelled")
	subagentStatusTimedOut  = subagents.Status("timed_out")
)

type subagentSession = subagents.Session

type subagentRunner func(context.Context, *agentRuntime, *subagentSession) (string, error)

type subagentManager struct {
	cfg    subagentRuntimeConfig
	runner subagentRunner
	core   *subagents.Manager
}

func newSubagentManager(cfg subagentRuntimeConfig, runner subagentRunner) *subagentManager {
	m := &subagentManager{cfg: cfg, runner: runner}
	m.core = subagents.New(cfg, func(ctx context.Context, runtime *subagents.Runtime, session *subagents.Session) (string, error) {
		if m.runner == nil {
			return "", errors.New("subagent runner is nil")
		}
		return m.runner(ctx, appRuntime(runtime), appSession(session))
	})
	return m
}

func appRuntime(runtime *subagents.Runtime) *agentRuntime {
	if runtime == nil {
		return nil
	}
	profile := fromSubagentProfile(runtime.ExecutionProfile)
	return &agentRuntime{
		sessionID:         runtime.SessionID,
		depth:             runtime.Depth,
		role:              agentRole(runtime.Role),
		allowedTools:      cloneAllowedTools(runtime.AllowedTools),
		maxToolIterations: runtime.MaxToolIterations,
		executionProfile:  profile,
		model:             runtime.Model,
		reasoning:         runtime.Reasoning,
	}
}

func subagentRuntime(runtime *agentRuntime) *subagents.Runtime {
	if runtime == nil {
		return nil
	}
	return &subagents.Runtime{
		SessionID:         runtime.sessionID,
		Depth:             runtime.depth,
		Role:              string(runtime.role),
		AllowedTools:      cloneAllowedTools(runtime.allowedTools),
		MaxToolIterations: runtime.maxToolIterations,
		ExecutionProfile:  toSubagentProfile(runtime.executionProfile),
		Model:             runtime.model,
		Reasoning:         runtime.reasoning,
	}
}

func appSession(session *subagents.Session) *subagentSession {
	return session
}

func (m *subagentManager) ListAll() []contracts.SubagentNode {
	if m == nil || m.core == nil {
		return nil
	}
	return m.core.ListAll()
}

func (m *subagentManager) bindParentContext(parent *agentRuntime, ctx context.Context) {
	if m == nil || m.core == nil || parent == nil {
		return
	}
	m.core.BindParentContext(subagentRuntime(parent), ctx)
}

func (m *subagentManager) create(ctx context.Context, parent *agentRuntime, args createSubagentArgs) (*subagentSession, error) {
	session, err := m.core.Create(ctx, subagentRuntime(parent), subagents.CreateArgs(args))
	return appSession(session), err
}

func (m *subagentManager) run(ctx context.Context, parent *agentRuntime, args runSubagentArgs) (*subagentSession, error) {
	session, err := m.core.Run(ctx, subagentRuntime(parent), subagents.RunArgs(args))
	return appSession(session), err
}

func (m *subagentManager) await(ctx context.Context, parent *agentRuntime, args awaitSubagentArgs) (*subagentSession, error) {
	session, err := m.core.Await(ctx, subagentRuntime(parent), subagents.AwaitArgs(args))
	return appSession(session), err
}

func (m *subagentManager) cancel(parent *agentRuntime, args cancelSubagentArgs) (*subagentSession, error) {
	session, err := m.core.Cancel(subagentRuntime(parent), subagents.CancelArgs(args))
	return appSession(session), err
}

func (m *subagentManager) list(parent *agentRuntime, args listSubagentsArgs) ([]subagentEnvelope, error) {
	items, err := m.core.List(subagentRuntime(parent), subagents.ListArgs(args))
	if err != nil {
		return nil, err
	}
	return items, nil
}

func (m *subagentManager) read(parent *agentRuntime, args readSubagentArgs) (any, error) {
	value, err := m.core.Read(subagentRuntime(parent), subagents.ReadArgs(args))
	if err != nil {
		return nil, err
	}
	return value, nil
}

func (m *subagentManager) snapshotLocked(session *subagentSession, includeOutput bool) subagentEnvelope {
	return m.core.Snapshot(session, includeOutput)
}

type subagentEnvelope = subagents.Envelope
type subagentAggregateEnvelope = subagents.AggregateEnvelope

func deriveChildAllowedTools(parentAllowed map[string]bool, requested []string, depth, maxDepth int) (map[string]bool, error) {
	return policy.InheritChildTools(parentAllowed, requested, depth, maxDepth)
}

func cloneAllowedTools(in map[string]bool) map[string]bool { return policy.CloneAllowedTools(in) }

func truncateText(in string, max int) (string, bool) {
	if max <= 0 || len(in) <= max {
		return in, false
	}
	return in[:max] + "\n\n[... truncated ...]", true
}

func filterNonEmpty(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
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
