package app

import "capelin-go/internal/subagents"

// These aliases keep application workflows on the subagent lifecycle seam;
// JSON decoding remains owned by internal/tools.
type createSubagentArgs = subagents.CreateArgs
type runSubagentArgs = subagents.RunArgs
type awaitSubagentArgs = subagents.AwaitArgs
type listSubagentsArgs = subagents.ListArgs
type readSubagentArgs = subagents.ReadArgs
type cancelSubagentArgs = subagents.CancelArgs
