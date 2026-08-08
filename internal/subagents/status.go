package subagents

import "capelin-go/internal/contracts"

// StatusCounts aggregates the lifecycle statuses of a set of subagent nodes.
type StatusCounts struct {
	Pending   int
	Queued    int
	Running   int
	Completed int
	Failed    int
	Cancelled int
	TimedOut  int
}

// Active reports how many subagents have not reached a terminal state yet.
func (c StatusCounts) Active() int { return c.Pending + c.Queued + c.Running }

// CountStatuses tallies node statuses against the known subagent status
// values. Unknown statuses are ignored.
func CountStatuses(nodes []contracts.SubagentNode) StatusCounts {
	var counts StatusCounts
	for _, node := range nodes {
		switch node.Status {
		case string(subagentStatusPending):
			counts.Pending++
		case string(subagentStatusQueued):
			counts.Queued++
		case string(subagentStatusRunning):
			counts.Running++
		case string(subagentStatusCompleted):
			counts.Completed++
		case string(subagentStatusFailed):
			counts.Failed++
		case string(subagentStatusCancelled):
			counts.Cancelled++
		case string(subagentStatusTimedOut):
			counts.TimedOut++
		}
	}
	return counts
}
