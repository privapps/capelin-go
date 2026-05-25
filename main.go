package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"
)

const (
	defaultBaseURL       = "http://localhost:8235/v1"
	defaultModel         = "gpt-5-mini"
	defaultToken         = ""
	defaultReasoning     = "medium"
	maxToolIterations    = 40
	requestTimeout       = 10 * time.Minute
	usageMessageTemplate = "Usage: %s [--allow-tool TOOL] \"your task\"\n"
)

var version = "dev"

const defaultSystemPrompt = `You are an execution-focused AI assistant.

Primary objective:
- Complete the task fully in a single response whenever possible.
- Minimize unnecessary back-and-forth.
- Do not ask questions if reasonable assumptions can be made.
- Make the best assumption and proceed.

Behavior rules:
- Infer intent from context.
- Prefer action over clarification.
- If information is missing but non-critical, choose sensible defaults and state them briefly.
- Only ask follow-up questions when the missing information would materially change the outcome.
- Provide complete, directly usable outputs.
- Structure responses clearly.
- Anticipate edge cases and handle them proactively.
- Do not explain your chain of thought.
- Be concise but thorough.

Do Not Ask Heuristics:
Never ask the user for:
- obvious preferences
- easily inferred defaults
- information already present in context
- details that do not materially affect the answer
Instead:
- choose a smart default
- state the assumption briefly
- continue execution

Decision policy:
1. Determine the actual user goal.
2. Identify constraints.
3. Fill in gaps using reasonable assumptions.
4. Produce the finished deliverable.
5. Include optional improvements if high value.

Output policy:
- Return final answers, not partial work.
- Avoid hedging language.
- Avoid excessive disclaimers.
- Use markdown formatting for readability.`

const (
	toolWebSearch      = "web_search"
	toolFetchPage      = "fetch_page"
	toolListFiles      = "list_files"
	toolReadFile       = "read_file"
	toolWriteFile      = "write_file"
	toolAppendFile     = "append_file"
	toolExecuteProgram = "execute_program"
	toolExecuteSkill   = "execute_skill"
	toolListSkills     = "list_skills"
	toolReadSkill      = "read_skill"
)

var alwaysEnabledTools = []string{
	toolWebSearch,
	toolFetchPage,
	toolListFiles,
	toolReadFile,
	toolListSkills,
	toolReadSkill,
}

var optInTools = map[string]struct{}{
	toolWriteFile:      {},
	toolAppendFile:     {},
	toolExecuteProgram: {},
	toolExecuteSkill:   {},
}

type config struct {
	baseURL         string
	model           string
	token           string
	reasoning       string
	systemPrompt    string
	showVersion     bool
	initialQuestion string
	workspaceRoot   string
	allowedTools    map[string]bool
	yolo            bool // enables all tools and unrestricted paths
}

type app struct {
	cfg     config
	client  *client
	skills  map[string]skill
	toolset []apiTool
}

type client struct {
	baseURL   string
	token     string
	model     string
	reasoning string
	http      *http.Client
}

type apiMessage struct {
	Role       string        `json:"role"`
	Content    string        `json:"content,omitempty"`
	ToolCallID string        `json:"tool_call_id,omitempty"`
	Name       string        `json:"name,omitempty"`
	ToolCalls  []apiToolCall `json:"tool_calls,omitempty"`
}

type apiToolCall struct {
	ID       string          `json:"id"`
	Type     string          `json:"type"`
	Function apiFunctionCall `json:"function"`
}

type apiFunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type apiRequest struct {
	Model           string       `json:"model"`
	Messages        []apiMessage `json:"messages"`
	Tools           []apiTool    `json:"tools,omitempty"`
	ToolChoice      string       `json:"tool_choice,omitempty"`
	ReasoningEffort string       `json:"reasoning_effort,omitempty"`
}

type apiTool struct {
	Type     string      `json:"type"`
	Function apiToolSpec `json:"function"`
}

type apiToolSpec struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

type apiResponse struct {
	Choices []struct {
		Message      apiCompletionMessage `json:"message"`
		FinishReason string               `json:"finish_reason"`
	} `json:"choices"`
}

type apiCompletionMessage struct {
	Role      string        `json:"role"`
	Content   *string       `json:"content"`
	ToolCalls []apiToolCall `json:"tool_calls,omitempty"`
}

func main() {
	os.Exit(run())
}

func run() int {
	cfg, err := loadConfig(os.Args[1:])
	if err != nil {
		if errors.Is(err, errHelpRequested) {
			printUsage(os.Stdout)
			return 0
		}
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	if cfg.showVersion {
		fmt.Fprintf(os.Stdout, "%s %s\n", filepath.Base(os.Args[0]), version)
		return 0
	}
	if cfg.initialQuestion == "" {
		printUsage(os.Stderr)
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	app, err := newApp(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	if err := app.runQuestion(ctx, cfg.initialQuestion); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

func newApp(cfg config) (*app, error) {
	skills, err := loadSkills(cfg.workspaceRoot)
	if err != nil {
		return nil, err
	}

	return &app{
		cfg: cfg,
		client: &client{
			baseURL:   strings.TrimRight(cfg.baseURL, "/"),
			token:     cfg.token,
			model:     cfg.model,
			reasoning: cfg.reasoning,
			http:      &http.Client{Timeout: requestTimeout},
		},
		skills:  skills,
		toolset: buildAgentTools(cfg.allowedTools),
	}, nil
}

var errHelpRequested = errors.New("help requested")

func loadConfig(args []string) (config, error) {
	filtered := make([]string, 0, len(args))
	allowedTools := map[string]bool{}
	for _, name := range alwaysEnabledTools {
		allowedTools[name] = true
	}
	yolo := false

	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "-h" || arg == "--help" || arg == "-help":
			return config{}, errHelpRequested
		case arg == "--version" || arg == "-version":
			return config{showVersion: true}, nil
		case arg == "--yolo":
			yolo = true
			for name := range optInTools {
				allowedTools[name] = true
			}
		case arg == "--allow-tool":
			if i+1 >= len(args) {
				return config{}, errors.New("--allow-tool requires a value")
			}
			i++
			name := strings.TrimSpace(args[i])
			if _, ok := optInTools[name]; !ok {
				return config{}, fmt.Errorf("unknown or non-opt-in tool %q", name)
			}
			allowedTools[name] = true
		case strings.HasPrefix(arg, "--allow-tool="):
			name := strings.TrimSpace(strings.TrimPrefix(arg, "--allow-tool="))
			if _, ok := optInTools[name]; !ok {
				return config{}, fmt.Errorf("unknown or non-opt-in tool %q", name)
			}
			allowedTools[name] = true
		case strings.HasPrefix(arg, "-"):
			return config{}, fmt.Errorf("unknown flag %q", arg)
		default:
			filtered = append(filtered, arg)
		}
	}

	baseURL, err := readBaseURL()
	if err != nil {
		return config{}, err
	}
	reasoning, err := readReasoningEffort()
	if err != nil {
		return config{}, err
	}
	workspaceRoot, err := os.Getwd()
	if err != nil {
		return config{}, fmt.Errorf("resolving workspace root: %w", err)
	}

	return config{
		baseURL:         baseURL,
		model:           readEnv("MODEL", defaultModel),
		token:           readEnv("TOKEN", defaultToken),
		reasoning:       reasoning,
		systemPrompt:    readSystemPrompt(),
		initialQuestion: strings.TrimSpace(strings.Join(filtered, " ")),
		workspaceRoot:   workspaceRoot,
		allowedTools:    allowedTools,
		yolo:            yolo,
	}, nil
}

func readEnv(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func readSystemPrompt() string {
	if value := strings.TrimSpace(os.Getenv("SYSTEM_PROMPT")); value != "" {
		return value
	}
	if value := strings.TrimSpace(os.Getenv("systemPrompt")); value != "" {
		return value
	}
	return defaultSystemPrompt
}

func readBaseURL() (string, error) {
	value := readEnv("BASE_URL", defaultBaseURL)
	parsed, err := url.Parse(value)
	if err != nil {
		return "", fmt.Errorf("invalid URL: %w", err)
	}
	if parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("invalid URL: %s", value)
	}
	return parsed.String(), nil
}

func readReasoningEffort() (string, error) {
	value := strings.TrimSpace(os.Getenv("REASONING_EFFORT"))
	if value == "" {
		return defaultReasoning, nil
	}
	return value, nil
}

func printUsage(w io.Writer) {
	fmt.Fprintf(w, usageMessageTemplate, filepath.Base(os.Args[0]))
	fmt.Fprintln(w, "One-shot mode only. No interactive mode.")
	fmt.Fprintln(w, "Env: BASE_URL, MODEL, TOKEN, REASONING_EFFORT, SYSTEM_PROMPT (or systemPrompt)")
	fmt.Fprintln(w, "Opt-in tools (repeatable): --allow-tool write_file --allow-tool append_file --allow-tool execute_program --allow-tool execute_skill")
	fmt.Fprintln(w, "All tools + unrestricted paths:  --yolo")
}

func (a *app) runQuestion(ctx context.Context, question string) error {
	fmt.Fprintf(os.Stderr, "[capelin-go] Task: %s\n\n", question)

	messages := []apiMessage{
		{Role: "system", Content: a.systemPromptWithSkills()},
		{Role: "user", Content: question},
	}

	for iter := 0; iter < maxToolIterations; iter++ {
		resp, err := a.client.complete(ctx, messages, a.toolset)
		if err != nil {
			return err
		}

		if content := strings.TrimSpace(resp.Content()); content != "" {
			fmt.Fprintln(os.Stdout, content)
		}

		messages = append(messages, resp.asMessage())
		if len(resp.ToolCalls()) == 0 {
			fmt.Fprintln(os.Stdout)
			return nil
		}

		for _, call := range resp.ToolCalls() {
			fmt.Fprintf(os.Stderr, "[tool] %s(%s)\n", call.Function.Name, call.Function.Arguments)
			out, err := a.runTool(ctx, call)
			if err != nil {
				fmt.Fprintf(os.Stderr, "[tool] %s error: %v\n", call.Function.Name, err)
				out = fmt.Sprintf("Tool error: %v", err)
			} else {
				fmt.Fprintf(os.Stderr, "[tool] %s done\n", call.Function.Name)
			}

			messages = append(messages, apiMessage{
				Role:       "tool",
				ToolCallID: call.ID,
				Content:    out,
			})
		}
	}
	return fmt.Errorf("exceeded maximum tool iterations (%d)", maxToolIterations)
}

func (a *app) systemPromptWithSkills() string {
	var b strings.Builder
	b.WriteString(strings.TrimSpace(a.cfg.systemPrompt))
	b.WriteString("\n\n")
	b.WriteString("You can inspect skills using list_skills and read_skill.\n")
	b.WriteString("When user asks to use a skill, execute the relevant skill command instead of only summarizing.\n")
	b.WriteString("Prefer execute_skill for skill-driven actions.\n")
	b.WriteString("Follow loaded skill instructions when relevant to the user task.\n")
	b.WriteString("Write and execute tools are disabled by default unless explicitly enabled.\n")
	return b.String()
}

func (a *app) runTool(ctx context.Context, call apiToolCall) (string, error) {
	switch call.Function.Name {
	case toolWebSearch:
		var args webSearchArgs
		if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
			return "", fmt.Errorf("invalid web_search arguments: %w", err)
		}
		return runWebSearch(ctx, args.Query)
	case toolFetchPage:
		var args fetchPageArgs
		if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
			return "", fmt.Errorf("invalid fetch_page arguments: %w", err)
		}
		return runFetchPage(ctx, args.URL)
	case toolListFiles:
		var args listFilesArgs
		if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
			return "", fmt.Errorf("invalid list_files arguments: %w", err)
		}
		return runListFiles(a.cfg.workspaceRoot, a.cfg.yolo, args)
	case toolReadFile:
		var args readFileArgs
		if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
			return "", fmt.Errorf("invalid read_file arguments: %w", err)
		}
		return runReadFile(a.cfg.workspaceRoot, a.cfg.yolo, args)
	case toolWriteFile:
		if !a.isToolEnabled(toolWriteFile) {
			return "", fmt.Errorf("%s is disabled; enable with --allow-tool %s", toolWriteFile, toolWriteFile)
		}
		var args writeFileArgs
		if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
			return "", fmt.Errorf("invalid write_file arguments: %w", err)
		}
		return runWriteFile(a.cfg.workspaceRoot, a.cfg.yolo, args)
	case toolAppendFile:
		if !a.isToolEnabled(toolAppendFile) {
			return "", fmt.Errorf("%s is disabled; enable with --allow-tool %s", toolAppendFile, toolAppendFile)
		}
		var args appendFileArgs
		if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
			return "", fmt.Errorf("invalid append_file arguments: %w", err)
		}
		return runAppendFile(a.cfg.workspaceRoot, a.cfg.yolo, args)
	case toolExecuteProgram:
		if !a.isToolEnabled(toolExecuteProgram) {
			return "", fmt.Errorf("%s is disabled; enable with --allow-tool %s", toolExecuteProgram, toolExecuteProgram)
		}
		var args executeProgramArgs
		if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
			return "", fmt.Errorf("invalid execute_program arguments: %w", err)
		}
		return runExecuteProgram(ctx, a.cfg.workspaceRoot, a.cfg.yolo, args)
	case toolExecuteSkill:
		if !a.isToolEnabled(toolExecuteSkill) {
			return "", fmt.Errorf("%s is disabled; enable with --allow-tool %s", toolExecuteSkill, toolExecuteSkill)
		}
		var args executeSkillArgs
		if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
			return "", fmt.Errorf("invalid execute_skill arguments: %w", err)
		}
		return runExecuteSkill(ctx, a.cfg.workspaceRoot, a.cfg.yolo, a.skills, args)
	case toolListSkills:
		return runListSkills(a.skills), nil
	case toolReadSkill:
		var args readSkillArgs
		if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
			return "", fmt.Errorf("invalid read_skill arguments: %w", err)
		}
		return runReadSkill(a.skills, args)
	default:
		return "", fmt.Errorf("unknown tool %q", call.Function.Name)
	}
}

func (a *app) isToolEnabled(name string) bool {
	return a.cfg.allowedTools[name]
}

func (c *client) complete(ctx context.Context, messages []apiMessage, tools []apiTool) (*completionMessage, error) {
	reqBody := apiRequest{
		Model:           c.model,
		Messages:        messages,
		Tools:           tools,
		ToolChoice:      "auto",
		ReasoningEffort: c.reasoning,
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}

	endpoint := c.baseURL + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("model request failed: %s: %s", resp.Status, strings.TrimSpace(string(raw)))
	}

	var decoded apiResponse
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		return nil, err
	}
	if len(decoded.Choices) == 0 {
		return nil, errors.New("model returned no choices")
	}
	return &completionMessage{message: decoded.Choices[0].Message}, nil
}

type completionMessage struct {
	message apiCompletionMessage
}

func (m *completionMessage) Content() string {
	if m.message.Content == nil {
		return ""
	}
	return *m.message.Content
}

func (m *completionMessage) ToolCalls() []apiToolCall {
	return m.message.ToolCalls
}

func (m *completionMessage) asMessage() apiMessage {
	msg := apiMessage{Role: m.message.Role, ToolCalls: m.message.ToolCalls}
	if m.message.Content != nil {
		msg.Content = *m.message.Content
	}
	return msg
}

func buildAgentTools(enabled map[string]bool) []apiTool {
	tools := []apiTool{
		specWebSearch(),
		specFetchPage(),
		specListFiles(),
		specReadFile(),
		specListSkills(),
		specReadSkill(),
	}
	if enabled[toolWriteFile] {
		tools = append(tools, specWriteFile())
	}
	if enabled[toolAppendFile] {
		tools = append(tools, specAppendFile())
	}
	if enabled[toolExecuteProgram] {
		tools = append(tools, specExecuteProgram())
	}
	if enabled[toolExecuteSkill] {
		tools = append(tools, specExecuteSkill())
	}
	slices.SortFunc(tools, func(a, b apiTool) int {
		return strings.Compare(a.Function.Name, b.Function.Name)
	})
	return tools
}

func runListSkills(skills map[string]skill) string {
	if len(skills) == 0 {
		return "(no skills found)"
	}
	keys := make([]string, 0, len(skills))
	for name := range skills {
		keys = append(keys, name)
	}
	slices.Sort(keys)

	var b strings.Builder
	for i, name := range keys {
		sk := skills[name]
		if i > 0 {
			b.WriteString("\n\n")
		}
		desc := sk.Description
		if desc == "" {
			desc = "(no description)"
		}
		commands := "(none parsed)"
		if len(sk.Commands) > 0 {
			commands = strings.Join(sk.Commands, ", ")
		}
		fmt.Fprintf(&b, "%d. %s\n   Source: %s\n   Path: %s\n   Description: %s\n   Commands: %s", i+1, sk.Name, sk.Source, sk.Path, desc, commands)
	}
	return b.String()
}

type readSkillArgs struct {
	Name string `json:"name"`
}

func runReadSkill(skills map[string]skill, args readSkillArgs) (string, error) {
	name := strings.TrimSpace(args.Name)
	if name == "" {
		return "", errors.New("skill name is required")
	}
	sk, ok := skills[name]
	if !ok {
		return "", fmt.Errorf("skill %q not found", name)
	}
	const maxSkillContent = 24_000
	content := sk.Content
	if len(content) > maxSkillContent {
		content = content[:maxSkillContent] + "\n\n[... skill content truncated ...]"
	}
	return content, nil
}
