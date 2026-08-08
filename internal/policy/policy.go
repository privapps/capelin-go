// Package policy owns the shared tool and execution safety rules.
// It has no dependency on the command or application composition layers.
package policy

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	WebSearch      = "web_search"
	FetchPage      = "fetch_page"
	ListFiles      = "list_files"
	ReadFile       = "read_file"
	WriteFile      = "write_file"
	EditFile       = "edit_file"
	AppendFile     = "append_file"
	ExecuteProgram = "execute_program"
	ExecuteSkill   = "execute_skill"
	IdleHook       = "idle_hook"
	ListSkills     = "list_skills"
	ReadSkill      = "read_skill"
	CreateSubagent = "create_subagent"
	RunSubagent    = "run_subagent"
	AwaitSubagent  = "await_subagent"
	ListSubagents  = "list_subagents"
	ReadSubagent   = "read_subagent"
	CancelSubagent = "cancel_subagent"
	UpdateTodos    = "update_todos"
)

var alwaysEnabled = []string{
	WebSearch, FetchPage, ListFiles, ReadFile, ListSkills, ReadSkill,
	CreateSubagent, RunSubagent, AwaitSubagent, ListSubagents, ReadSubagent,
	CancelSubagent, UpdateTodos,
}

var optIn = map[string]struct{}{
	WriteFile: {}, EditFile: {}, AppendFile: {}, ExecuteProgram: {}, ExecuteSkill: {},
	IdleHook: {},
}

// AlwaysEnabledTools returns a fresh copy of the baseline safe catalog.
func AlwaysEnabledTools() []string { return append([]string(nil), alwaysEnabled...) }

// OptInTools returns a fresh set of tools that require explicit permission.
func OptInTools() map[string]struct{} {
	result := make(map[string]struct{}, len(optIn))
	for name := range optIn {
		result[name] = struct{}{}
	}
	return result
}

func IsKnownTool(name string) bool {
	for _, candidate := range alwaysEnabled {
		if candidate == name {
			return true
		}
	}
	_, ok := optIn[name]
	return ok
}

func IsOptInTool(name string) bool { _, ok := optIn[name]; return ok }

// DefaultAllowedTools returns the exact normal-mode permission set.
func DefaultAllowedTools() map[string]bool {
	result := make(map[string]bool, len(alwaysEnabled))
	for _, name := range alwaysEnabled {
		result[name] = true
	}
	return result
}

// ExpandYOLO adds only the opt-in tools. Callers must opt into this behavior
// explicitly; selecting a skill never calls this function.
func ExpandYOLO(allowed map[string]bool) map[string]bool {
	result := CloneAllowedTools(allowed)
	for name := range optIn {
		result[name] = true
	}
	return result
}

func CloneAllowedTools(in map[string]bool) map[string]bool {
	out := make(map[string]bool, len(in))
	for name, enabled := range in {
		out[name] = enabled
	}
	return out
}

// ValidateAllowTool preserves the CLI's compatible error wording.
func ValidateAllowTool(name string) error {
	if !IsOptInTool(name) {
		return fmt.Errorf("unknown or non-opt-in tool %q", name)
	}
	return nil
}

// InheritChildTools copies a parent's permissions and permits a child only to
// restrict them. Subagent orchestration is removed at the configured depth.
func InheritChildTools(parent map[string]bool, requested []string, depth, maxDepth int) (map[string]bool, error) {
	if len(parent) == 0 {
		return nil, errors.New("parent has no allowed tools")
	}
	child := map[string]bool{}
	if len(requested) == 0 {
		for name, enabled := range parent {
			if enabled {
				child[name] = true
			}
		}
	} else {
		for _, raw := range requested {
			name := strings.TrimSpace(raw)
			if name == "" {
				continue
			}
			if !IsKnownTool(name) {
				return nil, fmt.Errorf("unknown tool %q in allowed_tools", name)
			}
			if !parent[name] {
				return nil, fmt.Errorf("tool %q is not allowed by parent policy", name)
			}
			child[name] = true
		}
	}
	if depth >= maxDepth {
		delete(child, CreateSubagent)
		delete(child, RunSubagent)
		delete(child, AwaitSubagent)
		delete(child, ListSubagents)
		delete(child, ReadSubagent)
		delete(child, CancelSubagent)
	}
	return child, nil
}

func ContainsDangerousPattern(command string, args []string) bool {
	denyCommands := map[string]struct{}{"sh": {}, "bash": {}, "zsh": {}, "fish": {}, "ksh": {}, "dash": {}, "cmd": {}, "cmd.exe": {}, "powershell": {}, "pwsh": {}}
	if _, blocked := denyCommands[strings.ToLower(filepath.Base(command))]; blocked {
		return true
	}
	if strings.TrimSpace(command) == "" || strings.ContainsAny(command, " \t\r\n\x00") {
		return true
	}
	for _, pattern := range []string{";", "&&", "||", "|", "`", "$(", "${", "<(", ">("} {
		if strings.Contains(command, pattern) {
			return true
		}
	}
	for _, arg := range args {
		if strings.Contains(arg, "\x00") {
			return true
		}
	}
	return false
}

// ResolveWorkspacePath enforces the non-YOLO workspace boundary, including
// symlink escapes. YOLO callers deliberately bypass this function.
func ResolveWorkspacePath(workspaceRoot, userPath string) (string, error) {
	if strings.TrimSpace(userPath) == "" {
		return "", errors.New("path is required")
	}
	clean := filepath.Clean(userPath)
	if filepath.IsAbs(clean) {
		return "", fmt.Errorf("absolute paths are not allowed: %q", userPath)
	}
	candidate := filepath.Join(workspaceRoot, clean)
	base, err := filepath.Abs(workspaceRoot)
	if err != nil {
		return "", fmt.Errorf("resolve path: %w", err)
	}
	absolute, err := filepath.Abs(candidate)
	if err != nil {
		return "", fmt.Errorf("resolve path: %w", err)
	}
	resolvedBase, err := resolveExistingPath(base)
	if err != nil {
		return "", fmt.Errorf("resolve path: %w", err)
	}
	resolvedCandidate, err := resolveExistingPath(absolute)
	if err != nil {
		return "", fmt.Errorf("resolve path: %w", err)
	}
	rel, err := filepath.Rel(resolvedBase, resolvedCandidate)
	if err != nil {
		return "", fmt.Errorf("resolve path: %w", err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q escapes workspace root", userPath)
	}
	return absolute, nil
}

func resolveExistingPath(path string) (string, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err == nil {
		return resolved, nil
	}
	if !os.IsNotExist(err) {
		return "", err
	}
	parent := filepath.Dir(path)
	if parent == path {
		return path, nil
	}
	resolvedParent, err := resolveExistingPath(parent)
	if err != nil {
		return "", err
	}
	return filepath.Join(resolvedParent, filepath.Base(path)), nil
}
