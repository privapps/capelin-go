// Package output owns user-facing runtime event sinks.
package output

import (
	"capelin-go/internal/contracts"
	"fmt"
	"os"
	"strings"
	"sync"
)

const toolDisplayMaxChars = 180

// StdioSink writes assistant content to stdout and operational events to stderr.
type StdioSink struct {
	mu sync.Mutex
}

func NewStdioSink() *StdioSink { return &StdioSink{} }

func (s *StdioSink) WriteContent(_ string, content string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fmt.Fprintln(os.Stdout, content)
}

func (s *StdioSink) WriteToolCall(_ string, toolName, args string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fmt.Fprint(os.Stderr, FormatToolCallDisplay(toolName, args))
}

func (s *StdioSink) WriteToolResult(_ string, toolName string, isError bool, detail string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if isError {
		fmt.Fprintf(os.Stderr, "[tool] %s error: %s\n", toolName, detail)
		return
	}
	fmt.Fprintf(os.Stderr, "[tool] %s done\n", toolName)
}

func (s *StdioSink) WriteSystem(_ string, msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fmt.Fprintln(os.Stderr, msg)
}

// FormatToolCallDisplay formats the compact diagnostic shown for a tool call.
func FormatToolCallDisplay(toolName, args string) string {
	return fmt.Sprintf("[tool] %s(%s)\n", toolName, TruncateDisplay(args, toolDisplayMaxChars))
}

// TruncateDisplay limits a displayed value without splitting UTF-8 text.
func TruncateDisplay(value string, max int) string {
	if max <= 0 {
		return ""
	}
	runes := []rune(value)
	if len(runes) <= max {
		return value
	}
	const ellipsis = "..."
	if max <= len(ellipsis) {
		return ellipsis[:max]
	}
	return string(runes[:max-len(ellipsis)]) + ellipsis
}

// FinalOnlySink buffers the latest root-agent content and suppresses all other
// events. FlushContent sends the buffered answer through the wrapped sink.
type FinalOnlySink struct {
	Wrapped     contracts.OutputSink
	RootAgentID string
	mu          sync.Mutex
	lastContent string
}

func NewFinalOnlySink(wrapped contracts.OutputSink, rootAgentID string) *FinalOnlySink {
	return &FinalOnlySink{Wrapped: wrapped, RootAgentID: rootAgentID}
}

func (s *FinalOnlySink) WriteContent(agentID string, content string) {
	if agentID != s.RootAgentID && agentID != "" {
		return
	}
	s.mu.Lock()
	s.lastContent = content
	s.mu.Unlock()
}

func (*FinalOnlySink) WriteToolCall(_, _, _ string)                  {}
func (*FinalOnlySink) WriteToolResult(_, _ string, _ bool, _ string) {}
func (*FinalOnlySink) WriteSystem(_, _ string)                       {}

func (s *FinalOnlySink) FlushContent() {
	s.mu.Lock()
	content := s.lastContent
	s.lastContent = ""
	s.mu.Unlock()
	if strings.TrimSpace(content) != "" && s.Wrapped != nil {
		s.Wrapped.WriteContent(s.RootAgentID, content)
	}
}

var _ contracts.OutputSink = (*StdioSink)(nil)
var _ contracts.OutputSink = (*FinalOnlySink)(nil)
