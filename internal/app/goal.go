package app

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

const toolCompleteGoal = "complete_goal"

// goalState is the durable state for one objective generation. The
// ChecklistFingerprint on a completion claim binds the model's declaration to
// the exact authoritative checklist that existed when it was made.
type goalState struct {
	Objective  string          `json:"objective"`
	Generation uint64          `json:"generation"`
	Completion *goalCompletion `json:"completion,omitempty"`
}

type goalCompletion struct {
	Summary              string   `json:"summary"`
	Evidence             []string `json:"evidence"`
	Generation           uint64   `json:"generation"`
	ChecklistFingerprint string   `json:"checklistFingerprint"`
}

type completeGoalArgs struct {
	Summary  string   `json:"summary"`
	Evidence []string `json:"evidence"`
}

func cloneGoalState(state *goalState) *goalState {
	if state == nil {
		return nil
	}
	clone := *state
	clone.Objective = strings.TrimSpace(state.Objective)
	clone.Completion = cloneGoalCompletion(state.Completion)
	return &clone
}

func cloneGoalCompletion(completion *goalCompletion) *goalCompletion {
	if completion == nil {
		return nil
	}
	clone := *completion
	clone.Summary = strings.TrimSpace(completion.Summary)
	clone.Evidence = append([]string(nil), completion.Evidence...)
	return &clone
}

func parseCompleteGoalArgs(arguments string) (completeGoalArgs, error) {
	var args completeGoalArgs
	decoder := json.NewDecoder(strings.NewReader(arguments))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&args); err != nil {
		return completeGoalArgs{}, fmt.Errorf("invalid complete_goal arguments: %w", err)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return completeGoalArgs{}, fmt.Errorf("invalid complete_goal arguments: %w", err)
	}
	args.Summary = strings.TrimSpace(args.Summary)
	if args.Summary == "" {
		return completeGoalArgs{}, errors.New("invalid complete_goal arguments: summary is required")
	}
	if len(args.Evidence) == 0 {
		return completeGoalArgs{}, errors.New("invalid complete_goal arguments: evidence must contain at least one statement")
	}
	for i, evidence := range args.Evidence {
		args.Evidence[i] = strings.TrimSpace(evidence)
		if args.Evidence[i] == "" {
			return completeGoalArgs{}, fmt.Errorf("invalid complete_goal arguments: evidence item %d is required", i)
		}
	}
	return args, nil
}

func (a *app) completeGoalForRuntime(runtime *agentRuntime, raw []byte) (string, error) {
	args, err := parseCompleteGoalArgs(string(raw))
	if err != nil {
		return "", err
	}
	if runtime == nil || !runtime.goalIsEnabled() || !runtime.allowedTools[toolCompleteGoal] {
		return "", errors.New("complete_goal is available only during an autonomous goal")
	}
	if err := runtime.recordGoalClaim(args.Summary, args.Evidence); err != nil {
		return "", err
	}
	claim := runtime.snapshotGoalClaim()
	todos := runtime.snapshotTodos()
	complete := todosComplete(todos) && firstCancelledTodoIsAbsent(todos)
	generation := uint64(0)
	if claim != nil {
		generation = claim.Generation
	}
	return marshalToolResult(struct {
		Recorded          bool   `json:"recorded"`
		ChecklistComplete bool   `json:"checklist_complete"`
		Generation        uint64 `json:"generation"`
	}{Recorded: claim != nil, ChecklistComplete: complete, Generation: generation})
}

func todosFingerprint(todos []todoItem) string {
	raw, err := json.Marshal(cloneTodos(todos))
	if err != nil {
		return ""
	}
	hash := sha256.Sum256(raw)
	return hex.EncodeToString(hash[:])
}

func validGoalCompletion(state *goalState, todos []todoItem) bool {
	if state == nil || strings.TrimSpace(state.Objective) == "" || state.Generation == 0 || state.Completion == nil {
		return false
	}
	claim := state.Completion
	if claim.Generation != state.Generation || strings.TrimSpace(claim.Summary) == "" || len(claim.Evidence) == 0 {
		return false
	}
	for _, evidence := range claim.Evidence {
		if strings.TrimSpace(evidence) == "" {
			return false
		}
	}
	if !todosComplete(todos) || firstCancelledTodoExists(todos) {
		return false
	}
	return claim.ChecklistFingerprint != "" && claim.ChecklistFingerprint == todosFingerprint(todos)
}

func firstCancelledTodoIsAbsent(todos []todoItem) bool {
	_, ok := firstCancelledTodo(todos)
	return !ok
}

func firstCancelledTodoExists(todos []todoItem) bool {
	_, ok := firstCancelledTodo(todos)
	return ok
}

func nextGoalGeneration(state *goalState) uint64 {
	if state == nil || state.Generation == 0 {
		return 1
	}
	if state.Generation == ^uint64(0) {
		return 1
	}
	return state.Generation + 1
}

func cloneGoalForPersistence(state *goalState) *goalState {
	return cloneGoalState(state)
}
