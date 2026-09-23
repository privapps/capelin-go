package contracts

// Subagent lifecycle arguments are shared by the dispatcher boundary and the
// subagent manager without coupling either package to the other.
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
