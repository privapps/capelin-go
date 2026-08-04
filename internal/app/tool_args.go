package app

// These request values are composition adapters for the subagent capability.
// Tool schemas and dispatch remain owned by internal/tools; app only decodes
// the values needed to invoke its subagent manager.
type createSubagentArgs struct {
	Name               string   `json:"name"`
	Question           string   `json:"question"`
	AllowedTools       []string `json:"allowed_tools"`
	TimeoutSeconds     int      `json:"timeout_seconds"`
	ExecutionMode      string   `json:"execution_mode"`
	OverflowMode       string   `json:"overflow_mode"`
	WaitTimeoutSeconds int      `json:"wait_timeout_seconds"`
}

type runSubagentArgs struct {
	ID             string `json:"id"`
	Wait           bool   `json:"wait"`
	TimeoutSeconds int    `json:"timeout_seconds"`
	ExecutionMode  string `json:"execution_mode"`
}

type awaitSubagentArgs struct {
	ID             string `json:"id"`
	TimeoutSeconds int    `json:"timeout_seconds"`
}

type listSubagentsArgs struct {
	IncludeDescendants bool `json:"include_descendants"`
}

type readSubagentArgs struct {
	ID            string   `json:"id"`
	IDs           []string `json:"ids"`
	IncludeOutput *bool    `json:"include_output"`
}

type cancelSubagentArgs struct {
	ID string `json:"id"`
}
