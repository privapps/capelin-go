package main

import (
	"bytes"
	"capelin-go/internal/types"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

type todoStatus string

const (
	todoStatusPending    todoStatus = "pending"
	todoStatusInProgress todoStatus = "in_progress"
	todoStatusCompleted  todoStatus = "completed"
	todoStatusCancelled  todoStatus = "cancelled"
)

var supportedTodoStatuses = map[todoStatus]struct{}{
	todoStatusPending:    {},
	todoStatusInProgress: {},
	todoStatusCompleted:  {},
	todoStatusCancelled:  {},
}

// todoItem is the authoritative checklist entry. The slice containing these
// entries is ordered; callers must not infer ordering from IDs.
type todoItem struct {
	ID      string     `json:"id"`
	Content string     `json:"content"`
	Source  string     `json:"source,omitempty"`
	Status  todoStatus `json:"status"`
}

type updateTodosArgs struct {
	Todos json.RawMessage `json:"todos"`
}

func specUpdateTodos() types.Tool {
	return types.Tool{
		Type: "function",
		Function: types.ToolSpec{
			Name:        toolUpdateTodos,
			Description: "Replace the authoritative ordered checklist with the complete current list. Use pending, in_progress, completed, or cancelled for each item.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"todos": map[string]any{
						"type":        "array",
						"description": "The complete checklist. This replaces the previous list; use an empty array to clear it.",
						"items": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"id":      map[string]any{"type": "string", "description": "Stable unique item ID"},
								"content": map[string]any{"type": "string", "description": "The actionable checklist item"},
								"source":  map[string]any{"type": "string", "description": "Optional origin or provenance for the item"},
								"status": map[string]any{
									"type": "string",
									"enum": []string{
										string(todoStatusPending),
										string(todoStatusInProgress),
										string(todoStatusCompleted),
										string(todoStatusCancelled),
									},
								},
							},
							"required":             []string{"id", "content", "status"},
							"additionalProperties": false,
						},
					},
				},
				"required":             []string{"todos"},
				"additionalProperties": false,
			},
		},
	}
}

// parseUpdateTodosArgs validates the tool boundary independently of the model
// provider's schema validation. This keeps direct tool execution and provider
// adapters subject to the same replacement contract.
func parseUpdateTodosArgs(arguments string) ([]todoItem, error) {
	var args updateTodosArgs
	decoder := json.NewDecoder(strings.NewReader(arguments))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&args); err != nil {
		return nil, fmt.Errorf("invalid update_todos arguments: %w", err)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return nil, fmt.Errorf("invalid update_todos arguments: %w", err)
	}
	if len(args.Todos) == 0 || bytes.Equal(bytes.TrimSpace(args.Todos), []byte("null")) {
		return nil, errors.New("invalid update_todos arguments: todos is required and must be an array")
	}

	var todos []todoItem
	itemsDecoder := json.NewDecoder(bytes.NewReader(args.Todos))
	itemsDecoder.DisallowUnknownFields()
	if err := itemsDecoder.Decode(&todos); err != nil {
		return nil, fmt.Errorf("invalid update_todos todos: %w", err)
	}
	if err := ensureJSONEOF(itemsDecoder); err != nil {
		return nil, fmt.Errorf("invalid update_todos todos: %w", err)
	}
	if todos == nil {
		return nil, errors.New("invalid update_todos arguments: todos must be an array")
	}
	if err := validateTodos(todos); err != nil {
		return nil, fmt.Errorf("invalid update_todos todos: %w", err)
	}
	return cloneTodos(todos), nil
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return errors.New("unexpected trailing JSON")
		}
		return err
	}
	return nil
}

func validateTodos(todos []todoItem) error {
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
		if _, ok := supportedTodoStatuses[todo.Status]; !ok {
			return fmt.Errorf("item %d has unsupported status %q", i, todo.Status)
		}
		if strings.TrimSpace(todo.Source) == "" && todo.Source != "" {
			return fmt.Errorf("item %d source must not be blank", i)
		}
	}
	return nil
}

func cloneTodos(todos []todoItem) []todoItem {
	if len(todos) == 0 {
		return []todoItem{}
	}
	cloned := make([]todoItem, len(todos))
	copy(cloned, todos)
	return cloned
}

func todoListResult(todos []todoItem) (string, error) {
	return marshalToolResult(struct {
		Todos []todoItem `json:"todos"`
	}{Todos: cloneTodos(todos)})
}
