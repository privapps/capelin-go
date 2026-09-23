package app

type todoStatus string

const (
	todoStatusPending    todoStatus = "pending"
	todoStatusInProgress todoStatus = "in_progress"
	todoStatusCompleted  todoStatus = "completed"
	todoStatusCancelled  todoStatus = "cancelled"
)

// todoItem is the authoritative checklist entry. The slice containing these
// entries is ordered; callers must not infer ordering from IDs.
type todoItem struct {
	ID      string     `json:"id"`
	Content string     `json:"content"`
	Source  string     `json:"source,omitempty"`
	Status  todoStatus `json:"status"`
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
