package tools

import (
	"capelin-go/internal/contracts"
	"capelin-go/internal/skills"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
)

// The exported aliases keep application-level tests and composition adapters
// from depending on the concrete implementation names.
type WebSearchArgs = webSearchArgs
type FetchPageArgs = fetchPageArgs
type ListFilesArgs = listFilesArgs
type ReadFileArgs = readFileArgs
type WriteFileArgs = writeFileArgs
type AppendFileArgs = appendFileArgs
type EditFileArgs = editFileArgs
type ExecuteProgramArgs = executeProgramArgs
type ExecuteSkillArgs = executeSkillArgs

// SetNetworkOverrides is intended for controlled application tests. Production
// callers use the package's hardened client and fixed search endpoints.
func SetNetworkOverrides(allowPrivate bool, client *http.Client, duckDuckGo, bing string) {
	allowPrivateFetch = allowPrivate
	if client != nil {
		toolHTTPClient = client
	}
	if strings.TrimSpace(duckDuckGo) != "" {
		ddgSearchURL = duckDuckGo
	}
	if strings.TrimSpace(bing) != "" {
		bingSearchURL = bing
	}
}

func DefaultHTTPClient() *http.Client { return toolHTTPClient }
func DefaultDuckDuckGoURL() string    { return ddgSearchURL }
func DefaultBingURL() string          { return bingSearchURL }

func RunFetchPage(ctx context.Context, targetURL string) (string, error) {
	return runFetchPage(ctx, targetURL)
}
func ValidateFetchURL(ctx context.Context, raw string) (*url.URL, error) {
	return validateFetchURL(ctx, raw)
}
func RunListFiles(workspaceRoot string, yolo bool, args ListFilesArgs) (string, error) {
	return runListFiles(workspaceRoot, yolo, args)
}
func RunReadFile(workspaceRoot string, yolo bool, args ReadFileArgs) (string, error) {
	return runReadFile(workspaceRoot, yolo, args)
}
func RunWriteFile(workspaceRoot string, yolo bool, args WriteFileArgs) (string, error) {
	return runWriteFile(workspaceRoot, yolo, args)
}
func RunAppendFile(workspaceRoot string, yolo bool, args AppendFileArgs) (string, error) {
	return runAppendFile(workspaceRoot, yolo, args)
}
func RunEditFile(workspaceRoot string, yolo bool, args EditFileArgs) (string, error) {
	return runEditFile(workspaceRoot, yolo, args)
}
func RunExecuteProgram(ctx context.Context, workspaceRoot string, yolo bool, args ExecuteProgramArgs) (string, error) {
	return runExecuteProgram(ctx, workspaceRoot, yolo, args)
}
func RunExecuteSkill(ctx context.Context, workspaceRoot string, yolo bool, loaded map[string]skills.Skill, args ExecuteSkillArgs) (string, error) {
	return runExecuteSkill(ctx, workspaceRoot, yolo, loaded, args)
}
func ContainsDangerousPattern(command string, args []string) bool {
	return containsDangerousPattern(command, args)
}
func ResolveWorkspacePath(workspaceRoot, userPath string) (string, error) {
	return resolveWorkspacePath(workspaceRoot, userPath)
}

// Build returns the provider-facing catalog in stable name order.
func Build(enabled map[string]bool) []contracts.Tool {
	result := make([]contracts.Tool, 0, len(enabled))
	appendIf := func(name string, spec contracts.Tool) {
		if enabled[name] {
			result = append(result, spec)
		}
	}
	appendIf(WebSearch, specWebSearch())
	appendIf(FetchPage, specFetchPage())
	appendIf(ListFiles, specListFiles())
	appendIf(ReadFile, specReadFile())
	appendIf(ListSkills, specListSkills())
	appendIf(ReadSkill, specReadSkill())
	appendIf(WriteFile, specWriteFile())
	appendIf(EditFile, specEditFile())
	appendIf(AppendFile, specAppendFile())
	appendIf(ExecuteProgram, specExecuteProgram())
	appendIf(ExecuteSkill, specExecuteSkill())
	appendIf(CreateSubagent, specCreateSubagent())
	appendIf(RunSubagent, specRunSubagent())
	appendIf(AwaitSubagent, specAwaitSubagent())
	appendIf(ListSubagents, specListSubagents())
	appendIf(ReadSubagent, specReadSubagent())
	appendIf(CancelSubagent, specCancelSubagent())
	appendIf(UpdateTodos, updateTodosSpec())
	slices.SortFunc(result, func(a, b contracts.Tool) int {
		return strings.Compare(a.Function.Name, b.Function.Name)
	})
	return result
}

// Hooks are the only application-facing part of the dispatcher. Concrete web,
// filesystem, process, and skill execution remains owned by this package;
// subagents and todo state are application workflows supplied as callbacks.
type Hooks struct {
	IsEnabled      func(runtime any, name string) bool
	CreateSubagent func(context.Context, any, json.RawMessage) (any, error)
	RunSubagent    func(context.Context, any, json.RawMessage) (any, error)
	AwaitSubagent  func(context.Context, any, json.RawMessage) (any, error)
	ListSubagents  func(any, json.RawMessage) (any, error)
	ReadSubagent   func(any, json.RawMessage) (any, error)
	CancelSubagent func(any, json.RawMessage) (any, error)
	UpdateTodos    func(any, json.RawMessage) (string, error)
	MarshalResult  func(any) (string, error)
}

type Dispatcher struct {
	WorkspaceRoot     string
	Yolo              bool
	Skills            map[string]skills.Skill
	AllowPrivateFetch bool         // explicit application/test override; the zero value preserves the safety default
	HTTPClient        *http.Client // optional policy-aware client for fetch_page
	Hooks             Hooks
}

func (d Dispatcher) enabled(runtime any, name string) bool {
	if d.Hooks.IsEnabled == nil {
		return false
	}
	return d.Hooks.IsEnabled(runtime, name)
}

func (d Dispatcher) marshal(value any) (string, error) {
	if d.Hooks.MarshalResult != nil {
		return d.Hooks.MarshalResult(value)
	}
	raw, err := json.MarshalIndent(value, "", "  ")
	return string(raw), err
}

func (d Dispatcher) Run(ctx context.Context, runtime any, call contracts.ToolCall) (string, error) {
	if runtime == nil {
		return "", errors.New("runtime is required")
	}
	// Keep the private-network exception scoped to this composed dispatcher.
	// The zero value explicitly installs the hardened default even if a legacy
	// direct-call test override is present elsewhere in the process.
	ctx = withPrivateFetchOverride(ctx, d.AllowPrivateFetch)
	name := call.Function.Name
	if !d.enabled(runtime, name) {
		if name == WriteFile || name == EditFile || name == AppendFile || name == ExecuteProgram || name == ExecuteSkill {
			return "", fmt.Errorf("%s is disabled; enable with --allow-tool %s", name, name)
		}
		return "", fmt.Errorf("%s is disabled by current policy", name)
	}

	decode := func(value any, label string) error {
		if err := json.Unmarshal([]byte(call.Function.Arguments), value); err != nil {
			return fmt.Errorf("invalid %s arguments: %w", label, err)
		}
		return nil
	}
	switch name {
	case WebSearch:
		var args WebSearchArgs
		if err := decode(&args, name); err != nil {
			return "", err
		}
		return runWebSearch(ctx, args.Query)
	case FetchPage:
		var args FetchPageArgs
		if err := decode(&args, name); err != nil {
			return "", err
		}
		if d.HTTPClient != nil {
			return runFetchPageWithClient(ctx, args.URL, d.HTTPClient)
		}
		return runFetchPage(ctx, args.URL)
	case ListFiles:
		var args ListFilesArgs
		if err := decode(&args, name); err != nil {
			return "", err
		}
		return runListFiles(d.WorkspaceRoot, d.Yolo, args)
	case ReadFile:
		var args ReadFileArgs
		if err := decode(&args, name); err != nil {
			return "", err
		}
		return runReadFile(d.WorkspaceRoot, d.Yolo, args)
	case WriteFile:
		var args WriteFileArgs
		if err := decode(&args, name); err != nil {
			return "", err
		}
		return runWriteFile(d.WorkspaceRoot, d.Yolo, args)
	case AppendFile:
		var args AppendFileArgs
		if err := decode(&args, name); err != nil {
			return "", err
		}
		return runAppendFile(d.WorkspaceRoot, d.Yolo, args)
	case EditFile:
		var args EditFileArgs
		if err := decode(&args, name); err != nil {
			return "", err
		}
		return runEditFile(d.WorkspaceRoot, d.Yolo, args)
	case ExecuteProgram:
		var args ExecuteProgramArgs
		if err := decode(&args, name); err != nil {
			return "", err
		}
		return runExecuteProgram(ctx, d.WorkspaceRoot, d.Yolo, args)
	case ExecuteSkill:
		var args ExecuteSkillArgs
		if err := decode(&args, name); err != nil {
			return "", err
		}
		return runExecuteSkill(ctx, d.WorkspaceRoot, d.Yolo, d.Skills, args)
	case ListSkills:
		return listSkills(d.Skills), nil
	case ReadSkill:
		var args readSkillArgs
		if err := decode(&args, name); err != nil {
			return "", err
		}
		return readSkill(d.Skills, args)
	case CreateSubagent:
		if d.Hooks.CreateSubagent == nil {
			return "", errors.New("subagent capability is unavailable")
		}
		var args json.RawMessage = append(json.RawMessage(nil), []byte(call.Function.Arguments)...)
		value, err := d.Hooks.CreateSubagent(ctx, runtime, args)
		if err != nil {
			return "", err
		}
		return d.marshal(value)
	case RunSubagent:
		if d.Hooks.RunSubagent == nil {
			return "", errors.New("subagent capability is unavailable")
		}
		value, err := d.Hooks.RunSubagent(ctx, runtime, []byte(call.Function.Arguments))
		if err != nil {
			return "", err
		}
		return d.marshal(value)
	case AwaitSubagent:
		if d.Hooks.AwaitSubagent == nil {
			return "", errors.New("subagent capability is unavailable")
		}
		value, err := d.Hooks.AwaitSubagent(ctx, runtime, []byte(call.Function.Arguments))
		if err != nil {
			return "", err
		}
		return d.marshal(value)
	case ListSubagents:
		if d.Hooks.ListSubagents == nil {
			return "", errors.New("subagent capability is unavailable")
		}
		value, err := d.Hooks.ListSubagents(runtime, []byte(call.Function.Arguments))
		if err != nil {
			return "", err
		}
		return d.marshal(value)
	case ReadSubagent:
		if d.Hooks.ReadSubagent == nil {
			return "", errors.New("subagent capability is unavailable")
		}
		value, err := d.Hooks.ReadSubagent(runtime, []byte(call.Function.Arguments))
		if err != nil {
			return "", err
		}
		return d.marshal(value)
	case CancelSubagent:
		if d.Hooks.CancelSubagent == nil {
			return "", errors.New("subagent capability is unavailable")
		}
		value, err := d.Hooks.CancelSubagent(runtime, []byte(call.Function.Arguments))
		if err != nil {
			return "", err
		}
		return d.marshal(value)
	case UpdateTodos:
		if d.Hooks.UpdateTodos == nil {
			return "", errors.New("todo capability is unavailable")
		}
		return d.Hooks.UpdateTodos(runtime, []byte(call.Function.Arguments))
	default:
		return "", fmt.Errorf("unknown tool %q", name)
	}
}

func listSkills(loaded map[string]skills.Skill) string {
	if len(loaded) == 0 {
		return "(no skills found)"
	}
	names := make([]string, 0, len(loaded))
	for name := range loaded {
		names = append(names, name)
	}
	slices.Sort(names)
	var b strings.Builder
	for i, name := range names {
		if i > 0 {
			b.WriteString("\n\n")
		}
		sk := loaded[name]
		description := sk.Description
		if description == "" {
			description = "(no description)"
		}
		commands := "(none parsed)"
		if len(sk.Commands) > 0 {
			commands = strings.Join(sk.Commands, ", ")
		}
		fmt.Fprintf(&b, "%d. %s\n   Source: %s\n   Path: %s\n   Description: %s\n   Commands: %s", i+1, sk.Name, sk.Source, sk.Path, description, commands)
	}
	return b.String()
}

type readSkillArgs struct {
	Name string `json:"name"`
}

func readSkill(loaded map[string]skills.Skill, args readSkillArgs) (string, error) {
	name := strings.TrimSpace(args.Name)
	if name == "" {
		return "", errors.New("skill name is required")
	}
	sk, ok := loaded[name]
	if !ok {
		return "", fmt.Errorf("skill %q not found", name)
	}
	if len(sk.Content) <= 100000 {
		return sk.Content, nil
	}
	return truncateUTF8(sk.Content, 100000) + "\n\n[... skill content truncated ...]", nil
}

func updateTodosSpec() contracts.Tool {
	return contracts.Tool{Type: "function", Function: contracts.ToolSpec{Name: UpdateTodos, Description: "Replace the authoritative ordered checklist with the complete current list. Use pending, in_progress, completed, or cancelled for each item.", Parameters: map[string]any{
		"type": "object", "properties": map[string]any{"todos": map[string]any{"type": "array", "description": "The complete checklist. This replaces the previous list; use an empty array to clear it.", "items": map[string]any{"type": "object", "properties": map[string]any{"id": map[string]any{"type": "string"}, "content": map[string]any{"type": "string"}, "source": map[string]any{"type": "string"}, "status": map[string]any{"type": "string", "enum": []string{"pending", "in_progress", "completed", "cancelled"}}}, "required": []string{"id", "content", "status"}, "additionalProperties": false}}}, "required": []string{"todos"}, "additionalProperties": false,
	}}}
}

func truncateUTF8(value string, maxBytes int) string {
	if maxBytes <= 0 || len(value) <= maxBytes {
		return value
	}
	cut := value[:maxBytes]
	for len(cut) > 0 && (cut[len(cut)-1]&0xc0) == 0x80 {
		cut = cut[:len(cut)-1]
	}
	return cut
}
