// Package output owns user-facing runtime event sinks.
package output

import (
	"bytes"
	"capelin-go/internal/contracts"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"unicode"
)

const toolDisplayMaxChars = 140

// StdioSink writes assistant content to stdout and operational events to stderr.
type StdioSink struct {
	mu      sync.Mutex
	stdout  io.Writer
	stderr  io.Writer
	refresh func()
}

func NewStdioSink() *StdioSink {
	return NewStdioSinkWithWriters(os.Stdout, os.Stderr, nil)
}

// NewStdioSinkWithWriters creates a sink with explicit destinations. The
// interactive REPL uses readline's writers so background events can redraw the
// current draft without changing one-shot output streams.
func NewStdioSinkWithWriters(stdout, stderr io.Writer, refresh func()) *StdioSink {
	if stdout == nil {
		stdout = io.Discard
	}
	if stderr == nil {
		stderr = io.Discard
	}
	return &StdioSink{stdout: stdout, stderr: stderr, refresh: refresh}
}

func (s *StdioSink) WriteContent(_ string, content string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fmt.Fprintln(s.stdout, content)
	s.refreshOutput()
}

func (s *StdioSink) WriteToolCall(_ string, toolName, args string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fprint(s.stderr, FormatToolCallDisplay(toolName, args))
	s.refreshOutput()
}

func (s *StdioSink) WriteToolResult(_ string, toolName string, isError bool, detail string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fprint(s.stderr, FormatToolResultDisplay(toolName, isError, detail))
	s.refreshOutput()
}

func (s *StdioSink) WriteSystem(_ string, msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fmt.Fprintln(s.stderr, msg)
	s.refreshOutput()
}

func (s *StdioSink) refreshOutput() {
	if s.refresh != nil {
		s.refresh()
	}
}

// FormatToolCallDisplay formats the compact diagnostic shown for a tool call.
func FormatToolCallDisplay(toolName, args string) string {
	line := "[tool] " + sanitizeDisplay(toolName)
	if formatted := formatToolArguments(args); formatted != "" {
		line += " " + formatted
	}
	return TruncateDisplay(line, toolDisplayMaxChars) + "\n"
}

// FormatToolResultDisplay formats a bounded completion or error diagnostic.
func FormatToolResultDisplay(toolName string, isError bool, detail string) string {
	line := "[tool] " + sanitizeDisplay(toolName)
	if isError {
		line += " error"
		if detail = sanitizeDisplay(detail); detail != "" {
			line += ": " + detail
		}
	} else {
		line += " done"
	}
	return TruncateDisplay(line, toolDisplayMaxChars) + "\n"
}

func formatToolArguments(args string) string {
	args = strings.TrimSpace(args)
	if args == "" {
		return ""
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(args), &fields); err != nil || fields == nil {
		return "arguments=" + sanitizeDisplay(args)
	}
	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, sanitizeDisplay(key)+"="+formatToolArgumentValue(fields[key]))
	}
	return strings.Join(parts, " ")
}

func formatToolArgumentValue(raw json.RawMessage) string {
	var stringValue string
	if json.Unmarshal(raw, &stringValue) == nil {
		return sanitizeDisplay(stringValue)
	}
	var compact bytes.Buffer
	if json.Compact(&compact, raw) == nil {
		return sanitizeDisplay(compact.String())
	}
	return sanitizeDisplay(string(raw))
}

func sanitizeDisplay(value string) string {
	var out strings.Builder
	for _, r := range value {
		if unicode.IsControl(r) || r == '\n' || r == '\r' || r == '\t' {
			r = ' '
		}
		out.WriteRune(r)
	}
	return strings.Join(strings.Fields(out.String()), " ")
}

func fprint(w io.Writer, value string) {
	_, _ = io.WriteString(w, value)
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

// RefreshingSink adds readline refresh coordination to an arbitrary sink.
// This keeps test and embedding sinks useful while the production stdio sink
// receives injected readline writers directly.
type RefreshingSink struct {
	wrapped contracts.OutputSink
	refresh func()
	mu      sync.Mutex
}

func NewRefreshingSink(wrapped contracts.OutputSink, refresh func()) *RefreshingSink {
	return &RefreshingSink{wrapped: wrapped, refresh: refresh}
}

func (s *RefreshingSink) WriteContent(agentID, content string) {
	s.write(func() { s.wrapped.WriteContent(agentID, content) })
}
func (s *RefreshingSink) WriteToolCall(agentID, toolName, args string) {
	s.write(func() { s.wrapped.WriteToolCall(agentID, toolName, args) })
}
func (s *RefreshingSink) WriteToolResult(agentID, toolName string, isError bool, detail string) {
	s.write(func() { s.wrapped.WriteToolResult(agentID, toolName, isError, detail) })
}
func (s *RefreshingSink) WriteSystem(agentID, msg string) {
	s.write(func() { s.wrapped.WriteSystem(agentID, msg) })
}
func (s *RefreshingSink) write(fn func()) {
	if s == nil || s.wrapped == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	fn()
	if s.refresh != nil {
		s.refresh()
	}
}

var _ contracts.OutputSink = (*StdioSink)(nil)
var _ contracts.OutputSink = (*FinalOnlySink)(nil)
var _ contracts.OutputSink = (*RefreshingSink)(nil)
