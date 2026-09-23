package tools

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

type CreateSubagentArgs struct {
	Name               string   `json:"name"`
	Question           string   `json:"question"`
	AllowedTools       []string `json:"allowed_tools"`
	TimeoutSeconds     int      `json:"timeout_seconds"`
	ExecutionMode      string   `json:"execution_mode"`
	OverflowMode       string   `json:"overflow_mode"`
	WaitTimeoutSeconds int      `json:"wait_timeout_seconds"`
}

type RunSubagentArgs struct {
	ID             string `json:"id"`
	Wait           bool   `json:"wait"`
	TimeoutSeconds int    `json:"timeout_seconds"`
	ExecutionMode  string `json:"execution_mode"`
}

type AwaitSubagentArgs struct {
	ID             string `json:"id"`
	TimeoutSeconds int    `json:"timeout_seconds"`
}

type ListSubagentsArgs struct {
	IncludeDescendants bool `json:"include_descendants"`
}

type ReadSubagentArgs struct {
	ID            string   `json:"id"`
	IDs           []string `json:"ids"`
	IncludeOutput *bool    `json:"include_output"`
}

type CancelSubagentArgs struct {
	ID string `json:"id"`
}

type TodoItem struct {
	ID      string `json:"id"`
	Content string `json:"content"`
	Source  string `json:"source,omitempty"`
	Status  string `json:"status"`
}

type UpdateTodosArgs struct {
	Todos []TodoItem `json:"todos"`
}

type CompleteGoalArgs struct {
	Summary  string   `json:"summary"`
	Evidence []string `json:"evidence"`
}

func DecodeUpdateTodosArgs(arguments string) (UpdateTodosArgs, error) {
	var args struct {
		Todos json.RawMessage `json:"todos"`
	}
	decoder := json.NewDecoder(strings.NewReader(arguments))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&args); err != nil {
		return UpdateTodosArgs{}, fmt.Errorf("invalid update_todos arguments: %w", err)
	}
	if err := ensureArgumentsEOF(decoder); err != nil {
		return UpdateTodosArgs{}, fmt.Errorf("invalid update_todos arguments: %w", err)
	}
	if len(args.Todos) == 0 || bytes.Equal(bytes.TrimSpace(args.Todos), []byte("null")) {
		return UpdateTodosArgs{}, errors.New("invalid update_todos arguments: todos is required and must be an array")
	}

	var todos []TodoItem
	itemsDecoder := json.NewDecoder(bytes.NewReader(args.Todos))
	itemsDecoder.DisallowUnknownFields()
	if err := itemsDecoder.Decode(&todos); err != nil {
		return UpdateTodosArgs{}, fmt.Errorf("invalid update_todos todos: %w", err)
	}
	if err := ensureArgumentsEOF(itemsDecoder); err != nil {
		return UpdateTodosArgs{}, fmt.Errorf("invalid update_todos todos: %w", err)
	}
	if todos == nil {
		return UpdateTodosArgs{}, errors.New("invalid update_todos arguments: todos must be an array")
	}
	if err := validateTodoItems(todos); err != nil {
		return UpdateTodosArgs{}, fmt.Errorf("invalid update_todos todos: %w", err)
	}
	return UpdateTodosArgs{Todos: append([]TodoItem(nil), todos...)}, nil
}

func DecodeCompleteGoalArgs(arguments string) (CompleteGoalArgs, error) {
	var args CompleteGoalArgs
	decoder := json.NewDecoder(strings.NewReader(arguments))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&args); err != nil {
		return CompleteGoalArgs{}, fmt.Errorf("invalid complete_goal arguments: %w", err)
	}
	if err := ensureArgumentsEOF(decoder); err != nil {
		return CompleteGoalArgs{}, fmt.Errorf("invalid complete_goal arguments: %w", err)
	}
	args.Summary = strings.TrimSpace(args.Summary)
	if args.Summary == "" {
		return CompleteGoalArgs{}, errors.New("invalid complete_goal arguments: summary is required")
	}
	if len(args.Evidence) == 0 {
		return CompleteGoalArgs{}, errors.New("invalid complete_goal arguments: evidence must contain at least one statement")
	}
	for i, evidence := range args.Evidence {
		args.Evidence[i] = strings.TrimSpace(evidence)
		if args.Evidence[i] == "" {
			return CompleteGoalArgs{}, fmt.Errorf("invalid complete_goal arguments: evidence item %d is required", i)
		}
	}
	return args, nil
}

func ensureArgumentsEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); err != nil {
		if errors.Is(err, io.EOF) {
			return nil
		}
		return err
	}
	return errors.New("unexpected trailing JSON")
}

func validateTodoItems(todos []TodoItem) error {
	seen := make(map[string]struct{}, len(todos))
	for i, todo := range todos {
		if strings.TrimSpace(todo.ID) == "" {
			return fmt.Errorf("item %d id is required", i)
		}
		if _, ok := seen[todo.ID]; ok {
			return fmt.Errorf("duplicate id %q", todo.ID)
		}
		seen[todo.ID] = struct{}{}
		if strings.TrimSpace(todo.Content) == "" {
			return fmt.Errorf("item %d content is required", i)
		}
		switch todo.Status {
		case "pending", "in_progress", "completed", "cancelled":
		default:
			return fmt.Errorf("item %d has unsupported status %q", i, todo.Status)
		}
		if strings.TrimSpace(todo.Source) == "" && todo.Source != "" {
			return fmt.Errorf("item %d source must not be blank", i)
		}
	}
	return nil
}
