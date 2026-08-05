package app

import (
	"capelin-go/internal/contracts"
	"capelin-go/internal/output"
)

const toolDisplayMaxChars = 140

// These aliases keep the historical package-local sink tests focused on the
// same observable contract while the production sink lives in internal/output.
type finalOnlySink struct {
	wrapped     contracts.OutputSink
	delegate    *output.FinalOnlySink
	lastContent string
}

func (s *finalOnlySink) ensureDelegate() *output.FinalOnlySink {
	if s.delegate == nil {
		s.delegate = output.NewFinalOnlySink(s.wrapped, rootAgentID)
	}
	return s.delegate
}

func (s *finalOnlySink) WriteContent(agentID, content string) {
	s.ensureDelegate().WriteContent(agentID, content)
}
func (*finalOnlySink) WriteToolCall(_, _, _ string)                  {}
func (*finalOnlySink) WriteToolResult(_, _ string, _ bool, _ string) {}
func (*finalOnlySink) WriteSystem(_, _ string)                       {}
func (s *finalOnlySink) FlushContent()                               { s.ensureDelegate().FlushContent() }

func formatToolCallDisplay(toolName, args string) string {
	return output.FormatToolCallDisplay(toolName, args)
}

var _ contracts.OutputSink = (*finalOnlySink)(nil)
