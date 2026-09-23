package app

import "capelin-go/internal/tools"

// These aliases preserve the application package's narrow compatibility seam
// while tool schemas and argument decoding remain owned by internal/tools.
type createSubagentArgs = tools.CreateSubagentArgs
type runSubagentArgs = tools.RunSubagentArgs
type awaitSubagentArgs = tools.AwaitSubagentArgs
type listSubagentsArgs = tools.ListSubagentsArgs
type readSubagentArgs = tools.ReadSubagentArgs
type cancelSubagentArgs = tools.CancelSubagentArgs
