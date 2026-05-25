package main

import (
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
	defaultSubagentMaxDepth          = 1
	defaultSubagentMaxChildren       = 8
	defaultSubagentMaxParallel       = 1
	defaultSubagentDefaultTimeoutSec = 120
	defaultSubagentMaxTimeoutSec     = 300
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

	started   bool
	parentCtx context.Context // inherited from the parent agent's run() call
	done      chan struct{}
	doneOnce  sync.Once
	cancel    context.CancelFunc
}

type subagentRunner func(ctx context.Context, runtime *agentRuntime, session *subagentSession) (string, error)

type subagentManager struct {
	cfg    subagentRuntimeConfig
	runner subagentRunner

	mu             sync.Mutex
	serialMu       sync.Mutex
	nextID         atomic.Uint64
	sessions       map[string]*subagentSession
	childrenByNode map[string][]string
	parallelSem    chan struct{}
}

func newSubagentManager(cfg subagentRuntimeConfig, runner subagentRunner) *subagentManager {
	cfg.normalize()
	return &subagentManager{
		cfg:            cfg,
		runner:         runner,
		sessions:       map[string]*subagentSession{},
		childrenByNode: map[string][]string{},
		parallelSem:    make(chan struct{}, cfg.MaxParallel),
	}
}

func (m *subagentManager) create(parent *agentRuntime, args createSubagentArgs) (*subagentSession, error) {
	if parent == nil {
		return nil, errors.New("parent runtime is required")
	}
	question := strings.TrimSpace(args.Question)
	if question == "" {
		return nil, errors.New("create_subagent question is required")
	}
	depth := parent.depth + 1
	if depth > m.cfg.MaxDepth {
		return nil, fmt.Errorf("max subagent depth exceeded: requested depth %d, max %d", depth, m.cfg.MaxDepth)
	}
	timeoutSec, err := m.resolveTimeoutSeconds(args.TimeoutSeconds)
	if err != nil {
		return nil, err
	}
	allowed, err := deriveChildAllowedTools(parent.allowedTools, args.AllowedTools, depth, m.cfg.MaxDepth)
	if err != nil {
		return nil, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	parentChildren := m.childrenByNode[parent.sessionID]
	if len(parentChildren) >= m.cfg.MaxChildren {
		return nil, fmt.Errorf("max children exceeded for parent %q (%d)", parent.sessionID, m.cfg.MaxChildren)
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
		CreatedAt:     now,
		Status:        subagentStatusPending,
		done:          make(chan struct{}),
	}
	if session.ExecutionMode == "" {
		session.ExecutionMode = "sequential"
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
	session.parentCtx = ctx
	runSession := session
	queued := cloneSession(session)
	m.mu.Unlock()

	go m.execute(runSession)

	if args.Wait {
		return m.await(ctx, parent, awaitSubagentArgs{ID: id, TimeoutSeconds: args.TimeoutSeconds})
	}
	return queued, nil
}

func (m *subagentManager) execute(session *subagentSession) {
	if session.ExecutionMode == "sequential" {
		m.serialMu.Lock()
	} else {
		m.parallelSem <- struct{}{}
	}
	defer func() {
		if session.ExecutionMode == "sequential" {
			m.serialMu.Unlock()
		} else {
			<-m.parallelSem
		}
	}()

	m.mu.Lock()
	if session.Status == subagentStatusCancelled {
		m.mu.Unlock()
		return
	}
	startedAt := time.Now().UTC()
	session.StartedAt = startedAt
	session.Status = subagentStatusRunning
	baseCtx := session.parentCtx
	if baseCtx == nil {
		baseCtx = context.Background()
	}
	execCtx, cancel := context.WithTimeout(baseCtx, session.Timeout)
	session.cancel = cancel
	m.mu.Unlock()

	runtime := &agentRuntime{
		sessionID:         session.ID,
		depth:             session.Depth,
		role:              session.Role,
		allowedTools:      cloneAllowedTools(session.AllowedTools),
		maxToolIterations: m.cfg.MaxToolIterations,
	}
	output, runErr := m.runner(execCtx, runtime, cloneSession(session))
	cancel()

	m.mu.Lock()
	defer m.mu.Unlock()
	session.cancel = nil
	session.FinishedAt = time.Now().UTC()
	session.Output, session.OutputTruncated = truncateText(output, m.cfg.MaxResultChars)

	switch {
	case errors.Is(execCtx.Err(), context.DeadlineExceeded):
		session.Status = subagentStatusTimedOut
		session.Error = fmt.Sprintf("subagent timed out after %s", session.Timeout)
	case runErr != nil:
		if errors.Is(runErr, context.Canceled) || errors.Is(execCtx.Err(), context.Canceled) {
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
}

func (m *subagentManager) await(ctx context.Context, parent *agentRuntime, args awaitSubagentArgs) (*subagentSession, error) {
	if parent == nil {
		return nil, errors.New("parent runtime is required")
	}
	id := strings.TrimSpace(args.ID)
	if id == "" {
		return nil, errors.New("await_subagent id is required")
	}
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
		if args.TimeoutSeconds > m.cfg.MaxTimeoutSec {
			return nil, fmt.Errorf("await_subagent timeout_seconds exceeds %d", m.cfg.MaxTimeoutSec)
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

	if len(ids) > m.cfg.MaxAggregateCount {
		return nil, fmt.Errorf("aggregation exceeds max ids (%d)", m.cfg.MaxAggregateCount)
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
	aggregate := buildAggregateEnvelope(items, m.cfg.MaxAggregateChars)
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

func (m *subagentManager) resolveTimeoutSeconds(requested int) (int, error) {
	if requested <= 0 {
		return m.cfg.DefaultTimeoutSec, nil
	}
	if requested > m.cfg.MaxTimeoutSec {
		return 0, fmt.Errorf("timeout_seconds exceeds %d", m.cfg.MaxTimeoutSec)
	}
	return requested, nil
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
	if len(parentAllowed) == 0 {
		return nil, errors.New("parent has no allowed tools")
	}
	child := map[string]bool{}
	if len(requested) == 0 {
		for name, enabled := range parentAllowed {
			if enabled {
				child[name] = true
			}
		}
	} else {
		for _, raw := range requested {
			name := strings.TrimSpace(raw)
			if name == "" {
				continue
			}
			if _, ok := optInTools[name]; !ok && !slices.Contains(alwaysEnabledTools, name) {
				return nil, fmt.Errorf("unknown tool %q in allowed_tools", name)
			}
			if !parentAllowed[name] {
				return nil, fmt.Errorf("tool %q is not allowed by parent policy", name)
			}
			child[name] = true
		}
	}
	if depth >= maxDepth {
		delete(child, toolCreateSubagent)
		delete(child, toolRunSubagent)
		delete(child, toolAwaitSubagent)
		delete(child, toolListSubagents)
		delete(child, toolReadSubagent)
		delete(child, toolCancelSubagent)
	}
	return child, nil
}

func cloneAllowedTools(in map[string]bool) map[string]bool {
	out := make(map[string]bool, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
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
