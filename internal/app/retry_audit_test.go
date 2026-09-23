package app

import (
	configpkg "capelin-go/internal/config"
	"capelin-go/internal/contracts"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func retryAuditProfile() configpkg.RuntimeProfile {
	return configpkg.RuntimeProfile{
		MaxIterations: 4,
		Subagents: configpkg.SubagentConfig{
			MaxDepth:          1,
			MaxChildren:       3,
			MaxParallel:       3,
			DefaultTimeoutSec: 2,
			MaxTimeoutSec:     2,
			MaxToolIterations: 4,
			MaxResultChars:    8000,
			MaxAggregateCount: 12,
			MaxAggregateChars: 12000,
		},
		ToolMaxParallel:    1,
		ToolTimeoutSec:     2,
		ToolRetryOnTimeout: false,
	}
}

func TestCapabilityRecoveryIsExplicitAndFailClosed(t *testing.T) {
	profile := retryAuditProfile()
	a := &app{cfg: config{
		workspaceRoot:    t.TempDir(),
		yolo:             true,
		allowedTools:     map[string]bool{toolCreateSubagent: true, toolReadFile: true},
		ordinaryProfile:  profile,
		profilesResolved: true,
	}}
	a.subagents = newSubagentManager(toSubagentConfig(profile.Subagents), func(context.Context, *agentRuntime, *subagentSession) (string, bool, error) {
		return "worker", false, nil
	})
	root := &agentRuntime{
		sessionID:         rootAgentID,
		role:              agentRoleCoordinator,
		allowedTools:      cloneAllowedTools(a.cfg.allowedTools),
		maxToolIterations: profile.MaxIterations,
		executionProfile:  profile,
	}
	capability := newAppToolCapability(nil, a, root)

	rejected := capability.Run(context.Background(), []contracts.ToolCall{{
		ID: "invalid-capability",
		Function: contracts.FunctionCall{
			Name:      toolCreateSubagent,
			Arguments: `{"question":"inspect","allowed_tools":["read_file","write_file"]}`,
		},
	}})[0]
	if !rejected.IsError {
		t.Fatalf("invalid capability request unexpectedly succeeded: %#v", rejected)
	}
	if !strings.Contains(rejected.Output, `tool "write_file" is not allowed by parent policy`) {
		t.Fatalf("policy diagnostic changed: %q", rejected.Output)
	}
	if got := len(a.subagents.ListAll()); got != 0 {
		t.Fatalf("rejected request created %d workers", got)
	}
	if rejected.Recovery == nil {
		t.Fatal("invalid capability request did not carry recovery audit")
	}
	recovery := rejected.Recovery
	if recovery.Kind != contracts.RecoveryKindCorrectedRetry ||
		recovery.Phase != contracts.RecoveryPhaseCapabilityAdmission ||
		recovery.Attempt != 1 || recovery.MaxRetries != 1 ||
		!recovery.Retryable || !recovery.RequiresExplicitDecision {
		t.Fatalf("unexpected capability recovery: %#v", recovery)
	}
	if recovery.PermissionScope == nil ||
		!recovery.PermissionScope.RestrictOnly ||
		!sameStrings(recovery.PermissionScope.AllowedTools, []string{toolCreateSubagent, toolReadFile}) ||
		!sameStrings(recovery.PermissionScope.RequestedTools, []string{toolReadFile, toolWriteFile}) ||
		len(recovery.PermissionScope.EffectiveTools) != 0 {
		t.Fatalf("capability recovery lost permission scope: %#v", recovery.PermissionScope)
	}
	if strings.Contains(strings.ToLower(recovery.Guidance), "omit") {
		t.Fatalf("recovery guidance permits an implicit permission broadening: %q", recovery.Guidance)
	}

	corrected := capability.Run(context.Background(), []contracts.ToolCall{{
		ID: "corrected-capability",
		Function: contracts.FunctionCall{
			Name:      toolCreateSubagent,
			Arguments: `{"question":"inspect","allowed_tools":["read_file"]}`,
		},
	}})[0]
	if corrected.IsError {
		t.Fatalf("corrected capability request failed: %s", corrected.Output)
	}
	var snapshot subagentEnvelope
	if err := json.Unmarshal([]byte(corrected.Output), &snapshot); err != nil {
		t.Fatalf("decode corrected worker: %v", err)
	}
	if !sameStrings(snapshot.AllowedTools, []string{toolReadFile}) {
		t.Fatalf("corrected worker scope = %v, want [%s]", snapshot.AllowedTools, toolReadFile)
	}
}

func TestCommandRecoveryIsExplicitAndDoesNotChangeTaskScope(t *testing.T) {
	profile := retryAuditProfile()
	a := &app{cfg: config{
		workspaceRoot:    t.TempDir(),
		yolo:             true,
		allowedTools:     map[string]bool{toolExecuteProgram: true, toolReadFile: true},
		ordinaryProfile:  profile,
		profilesResolved: true,
	}}
	root := &agentRuntime{
		sessionID:         rootAgentID,
		role:              agentRoleCoordinator,
		allowedTools:      cloneAllowedTools(a.cfg.allowedTools),
		maxToolIterations: profile.MaxIterations,
		executionProfile:  profile,
	}
	capability := newAppToolCapability(nil, a, root)

	failed := capability.Run(context.Background(), []contracts.ToolCall{{
		ID: "bad-command",
		Function: contracts.FunctionCall{
			Name:      toolExecuteProgram,
			Arguments: `{"command":"definitely-not-a-real-executable"}`,
		},
	}})[0]
	if !failed.IsError || failed.Recovery == nil {
		t.Fatalf("failed command did not produce recoverable result: %#v", failed)
	}
	if failed.Recovery.Phase != contracts.RecoveryPhaseCommandExecution ||
		failed.Recovery.MaxRetries != 1 ||
		!failed.Recovery.RequiresExplicitDecision {
		t.Fatalf("unexpected command recovery: %#v", failed.Recovery)
	}
	if failed.Recovery.PermissionScope == nil ||
		!sameStrings(failed.Recovery.PermissionScope.AllowedTools, []string{toolExecuteProgram, toolReadFile}) {
		t.Fatalf("command recovery omitted permission scope: %#v", failed.Recovery.PermissionScope)
	}
	if root.recordedFatalError() != nil || !root.hadRecoverableToolError() {
		t.Fatalf("recoverable command failure changed final task classification")
	}

	// The next call is an intentional corrected retry, not an automatic retry
	// or a permission change. The test binary exits successfully without
	// running a test, making the command deterministic and PATH-independent.
	corrected := capability.Run(context.Background(), []contracts.ToolCall{{
		ID: "corrected-command",
		Function: contracts.FunctionCall{
			Name:      toolExecuteProgram,
			Arguments: `{"command":` + mustJSON(os.Args[0]) + `,"args":["-test.run","^$"]}`,
		},
	}})[0]
	if corrected.IsError {
		t.Fatalf("corrected command failed: %s", corrected.Output)
	}
	if corrected.Recovery != nil {
		t.Fatalf("successful corrected command retained recovery audit: %#v", corrected.Recovery)
	}
	var result struct {
		Failed bool `json:"failed"`
	}
	if err := json.Unmarshal([]byte(corrected.Output), &result); err != nil {
		t.Fatalf("decode corrected command result: %v", err)
	}
	if result.Failed {
		t.Fatalf("corrected command reported failure: %s", corrected.Output)
	}
}

func sameStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func mustJSON(value string) string {
	raw, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return string(raw)
}
