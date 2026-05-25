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
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	defaultBaseURL       = "http://localhost:8235/v1"
	defaultModel         = "gpt-5-mini"
	defaultToken         = ""
	defaultReasoning     = "medium"
	defaultMaxIterations = 40
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

Tool efficiency rules:
- Limit web_search calls to at most 5 per task; prefer broad, precise queries over many narrow ones.
- Do not retry the same search intent with only minor query variations.
- If the first search yields insufficient results, widen the query instead of repeating it.
- Prefer fetch_page on a known URL over a new web_search when you already have a relevant link.

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
	toolCreateSubagent = "create_subagent"
	toolRunSubagent    = "run_subagent"
	toolAwaitSubagent  = "await_subagent"
	toolListSubagents  = "list_subagents"
	toolReadSubagent   = "read_subagent"
	toolCancelSubagent = "cancel_subagent"
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
	toolCreateSubagent: {},
	toolRunSubagent:    {},
	toolAwaitSubagent:  {},
	toolListSubagents:  {},
	toolReadSubagent:   {},
	toolCancelSubagent: {},
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
	maxIterations   int
	subagents       subagentRuntimeConfig
}

type app struct {
	cfg       config
	client    *client
	skills    map[string]skill
	toolset   []apiTool
	subagents *subagentManager
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

	instance := &app{
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
	}
	instance.subagents = newSubagentManager(cfg.subagents, instance.runSubagentSession)
	return instance, nil
}

var errHelpRequested = errors.New("help requested")

func loadConfig(args []string) (config, error) {
	fileCfg, err := ensureConfigFile()
	if err != nil {
		fmt.Fprintf(os.Stderr, "[capelin-go] warning: config file: %v\n", err)
		fileCfg = map[string]string{}
	}

	filtered := make([]string, 0, len(args))
	allowedTools := map[string]bool{}
	for _, name := range alwaysEnabledTools {
		allowedTools[name] = true
	}
	yolo := false
	maxIter := 0
	subagentCfg := defaultSubagentRuntimeConfig()

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
		case arg == "--subagent-max-depth":
			if i+1 >= len(args) {
				return config{}, errors.New("--subagent-max-depth requires a value")
			}
			i++
			value, err := parsePositiveInt(args[i], "--subagent-max-depth")
			if err != nil {
				return config{}, err
			}
			subagentCfg.MaxDepth = value
		case strings.HasPrefix(arg, "--subagent-max-depth="):
			value, err := parsePositiveInt(strings.TrimPrefix(arg, "--subagent-max-depth="), "--subagent-max-depth")
			if err != nil {
				return config{}, err
			}
			subagentCfg.MaxDepth = value
		case arg == "--subagent-max-children":
			if i+1 >= len(args) {
				return config{}, errors.New("--subagent-max-children requires a value")
			}
			i++
			value, err := parsePositiveInt(args[i], "--subagent-max-children")
			if err != nil {
				return config{}, err
			}
			subagentCfg.MaxChildren = value
		case strings.HasPrefix(arg, "--subagent-max-children="):
			value, err := parsePositiveInt(strings.TrimPrefix(arg, "--subagent-max-children="), "--subagent-max-children")
			if err != nil {
				return config{}, err
			}
			subagentCfg.MaxChildren = value
		case arg == "--subagent-max-parallel":
			if i+1 >= len(args) {
				return config{}, errors.New("--subagent-max-parallel requires a value")
			}
			i++
			value, err := parsePositiveInt(args[i], "--subagent-max-parallel")
			if err != nil {
				return config{}, err
			}
			subagentCfg.MaxParallel = value
		case strings.HasPrefix(arg, "--subagent-max-parallel="):
			value, err := parsePositiveInt(strings.TrimPrefix(arg, "--subagent-max-parallel="), "--subagent-max-parallel")
			if err != nil {
				return config{}, err
			}
			subagentCfg.MaxParallel = value
		case arg == "--subagent-timeout-seconds":
			if i+1 >= len(args) {
				return config{}, errors.New("--subagent-timeout-seconds requires a value")
			}
			i++
			value, err := parsePositiveInt(args[i], "--subagent-timeout-seconds")
			if err != nil {
				return config{}, err
			}
			subagentCfg.DefaultTimeoutSec = value
		case strings.HasPrefix(arg, "--subagent-timeout-seconds="):
			value, err := parsePositiveInt(strings.TrimPrefix(arg, "--subagent-timeout-seconds="), "--subagent-timeout-seconds")
			if err != nil {
				return config{}, err
			}
			subagentCfg.DefaultTimeoutSec = value
		case arg == "--max-iterations":
			if i+1 >= len(args) {
				return config{}, errors.New("--max-iterations requires a value")
			}
			i++
			value, err := parsePositiveInt(args[i], "--max-iterations")
			if err != nil {
				return config{}, err
			}
			maxIter = value
		case strings.HasPrefix(arg, "--max-iterations="):
			value, err := parsePositiveInt(strings.TrimPrefix(arg, "--max-iterations="), "--max-iterations")
			if err != nil {
				return config{}, err
			}
			maxIter = value
		case strings.HasPrefix(arg, "-"):
			return config{}, fmt.Errorf("unknown flag %q", arg)
		default:
			filtered = append(filtered, arg)
		}
	}

	baseURL, err := readBaseURL(fileCfg)
	if err != nil {
		return config{}, err
	}
	reasoning, err := readReasoningEffort(fileCfg)
	if err != nil {
		return config{}, err
	}
	workspaceRoot, err := os.Getwd()
	if err != nil {
		return config{}, fmt.Errorf("resolving workspace root: %w", err)
	}
	subagentCfg.normalize()

	// Resolve max iterations: flag > env > file > default
	if maxIter == 0 {
		if env := readCfg("MAX_ITERATIONS", fileCfg, ""); env != "" {
			if v, err := parsePositiveInt(env, "MAX_ITERATIONS"); err == nil {
				maxIter = v
			}
		}
	}
	if maxIter == 0 {
		maxIter = defaultMaxIterations
	}

	return config{
		baseURL:         baseURL,
		model:           readCfg("MODEL", fileCfg, defaultModel),
		token:           readCfg("TOKEN", fileCfg, defaultToken),
		reasoning:       reasoning,
		systemPrompt:    readSystemPrompt(fileCfg),
		initialQuestion: strings.TrimSpace(strings.Join(filtered, " ")),
		workspaceRoot:   workspaceRoot,
		allowedTools:    allowedTools,
		yolo:            yolo,
		maxIterations:   maxIter,
		subagents:       subagentCfg,
	}, nil
}

func parsePositiveInt(raw, flagName string) (int, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return 0, fmt.Errorf("%s requires a non-empty value", flagName)
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed <= 0 {
		return 0, fmt.Errorf("%s expects a positive integer, got %q", flagName, value)
	}
	return parsed, nil
}

func readEnv(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

// readCfg returns the first non-empty value from: env var → config file → fallback.
func readCfg(key string, fileCfg map[string]string, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	if value := strings.TrimSpace(fileCfg[key]); value != "" {
		return value
	}
	return fallback
}

func readSystemPrompt(fileCfg map[string]string) string {
	if value := strings.TrimSpace(os.Getenv("SYSTEM_PROMPT")); value != "" {
		return value
	}
	if value := strings.TrimSpace(os.Getenv("systemPrompt")); value != "" {
		return value
	}
	if value := strings.TrimSpace(fileCfg["SYSTEM_PROMPT"]); value != "" {
		return value
	}
	return defaultSystemPrompt
}

func readBaseURL(fileCfg map[string]string) (string, error) {
	value := readCfg("BASE_URL", fileCfg, defaultBaseURL)
	parsed, err := url.Parse(value)
	if err != nil {
		return "", fmt.Errorf("invalid URL: %w", err)
	}
	if parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("invalid URL: %s", value)
	}
	return parsed.String(), nil
}

func readReasoningEffort(fileCfg map[string]string) (string, error) {
	value := readCfg("REASONING_EFFORT", fileCfg, defaultReasoning)
	if strings.EqualFold(value, "none") {
		return "", nil
	}
	return value, nil
}

// configFilePath returns the path to the user-level config file.
func configFilePath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".local", "capelin-go", "config.ini")
}

const defaultConfigFileContent = `# capelin-go configuration
# Edit this file to set persistent defaults.
# Priority: CLI flags > environment variables > this file > built-in defaults.

BASE_URL = http://localhost:8235/v1
MODEL = gpt-5-mini
TOKEN =
REASONING_EFFORT = medium
SYSTEM_PROMPT =
MAX_ITERATIONS = 40
`

// ensureConfigFile creates the config file with defaults if it does not exist,
// then reads and returns its key=value pairs.
func ensureConfigFile() (map[string]string, error) {
	path := configFilePath()
	if path == "" {
		return map[string]string{}, nil
	}

	if _, err := os.Stat(path); os.IsNotExist(err) {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return map[string]string{}, fmt.Errorf("creating config dir: %w", err)
		}
		if err := os.WriteFile(path, []byte(defaultConfigFileContent), 0o644); err != nil {
			return map[string]string{}, fmt.Errorf("writing default config: %w", err)
		}
	}

	return readConfigFile(path)
}

// readConfigFile parses a simple KEY = VALUE file, ignoring blank lines and # comments.
func readConfigFile(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return map[string]string{}, fmt.Errorf("reading config file: %w", err)
	}
	result := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		idx := strings.IndexByte(line, '=')
		if idx < 0 {
			continue
		}
		key := strings.TrimSpace(line[:idx])
		value := strings.TrimSpace(line[idx+1:])
		if key != "" {
			result[key] = value
		}
	}
	return result, nil
}

func printUsage(w io.Writer) {
	fmt.Fprintf(w, usageMessageTemplate, filepath.Base(os.Args[0]))
	fmt.Fprintln(w, "One-shot mode only. No interactive mode.")
	fmt.Fprintln(w, "Env: BASE_URL, MODEL, TOKEN, REASONING_EFFORT, SYSTEM_PROMPT (or systemPrompt), MAX_ITERATIONS")
	fmt.Fprintln(w, "Opt-in tools (repeatable): --allow-tool write_file --allow-tool append_file --allow-tool execute_program --allow-tool execute_skill --allow-tool create_subagent --allow-tool run_subagent --allow-tool await_subagent --allow-tool list_subagents --allow-tool read_subagent --allow-tool cancel_subagent")
	fmt.Fprintln(w, "Iteration limit: --max-iterations N (default 40; env MAX_ITERATIONS)")
	fmt.Fprintln(w, "Subagent limits: --subagent-max-depth N --subagent-max-children N --subagent-max-parallel N --subagent-timeout-seconds N")
	fmt.Fprintln(w, "All tools + unrestricted paths:  --yolo")
}

func (a *app) runQuestion(ctx context.Context, question string) error {
	fmt.Fprintf(os.Stderr, "[capelin-go] Task: %s\n\n", question)
	_, err := a.runConversation(ctx, question, a.rootRuntime(), a.toolset, true)
	return err
}

func (a *app) runConversation(ctx context.Context, question string, runtime *agentRuntime, toolset []apiTool, emitOutput bool) (string, error) {
	messages := []apiMessage{
		{Role: "system", Content: a.systemPromptWithSkills()},
		{Role: "user", Content: question},
	}
	maxIterations := defaultMaxIterations
	if runtime != nil && runtime.maxToolIterations > 0 {
		maxIterations = runtime.maxToolIterations
	}
	lastContent := ""

	for iter := 0; iter < maxIterations; iter++ {
		// Warn the model when it's 3 iterations from the cap so it can wrap up gracefully.
		if iter == maxIterations-3 && maxIterations > 3 {
			messages = append(messages, apiMessage{
				Role:    "user",
				Content: fmt.Sprintf("[SYSTEM] You have %d iterations remaining. Wrap up and produce a final answer now.", maxIterations-iter),
			})
		}

		resp, err := a.client.complete(ctx, messages, toolset)
		if err != nil {
			return "", err
		}

		if content := strings.TrimSpace(resp.Content()); content != "" {
			lastContent = content
			if emitOutput {
				fmt.Fprintln(os.Stdout, content)
			}
		}

		messages = append(messages, resp.asMessage())
		if len(resp.ToolCalls()) == 0 {
			if emitOutput {
				fmt.Fprintln(os.Stdout)
			}
			return lastContent, nil
		}

		for _, call := range resp.ToolCalls() {
			if emitOutput {
				fmt.Fprintf(os.Stderr, "[tool] %s(%s)\n", call.Function.Name, call.Function.Arguments)
			}
			out, err := a.runToolForRuntime(ctx, runtime, call)
			if err != nil {
				if emitOutput {
					fmt.Fprintf(os.Stderr, "[tool] %s error: %v\n", call.Function.Name, err)
				}
				out = fmt.Sprintf("Tool error: %v", err)
			} else if emitOutput {
				fmt.Fprintf(os.Stderr, "[tool] %s done\n", call.Function.Name)
			}

			messages = append(messages, apiMessage{
				Role:       "tool",
				ToolCallID: call.ID,
				Content:    out,
			})
		}
	}
	return "", fmt.Errorf("exceeded maximum tool iterations (%d)", maxIterations)
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
	b.WriteString("Subagent tools are opt-in and enforce inherited limits/policies.\n")
	return b.String()
}

func (a *app) runTool(ctx context.Context, call apiToolCall) (string, error) {
	return a.runToolForRuntime(ctx, a.rootRuntime(), call)
}

func (a *app) runToolForRuntime(ctx context.Context, runtime *agentRuntime, call apiToolCall) (string, error) {
	if runtime == nil {
		runtime = a.rootRuntime()
	}
	switch call.Function.Name {
	case toolWebSearch:
		if !a.isToolEnabled(runtime, toolWebSearch) {
			return "", fmt.Errorf("%s is disabled by current policy", toolWebSearch)
		}
		var args webSearchArgs
		if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
			return "", fmt.Errorf("invalid web_search arguments: %w", err)
		}
		return runWebSearch(ctx, args.Query)
	case toolFetchPage:
		if !a.isToolEnabled(runtime, toolFetchPage) {
			return "", fmt.Errorf("%s is disabled by current policy", toolFetchPage)
		}
		var args fetchPageArgs
		if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
			return "", fmt.Errorf("invalid fetch_page arguments: %w", err)
		}
		return runFetchPage(ctx, args.URL)
	case toolListFiles:
		if !a.isToolEnabled(runtime, toolListFiles) {
			return "", fmt.Errorf("%s is disabled by current policy", toolListFiles)
		}
		var args listFilesArgs
		if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
			return "", fmt.Errorf("invalid list_files arguments: %w", err)
		}
		return runListFiles(a.cfg.workspaceRoot, a.cfg.yolo, args)
	case toolReadFile:
		if !a.isToolEnabled(runtime, toolReadFile) {
			return "", fmt.Errorf("%s is disabled by current policy", toolReadFile)
		}
		var args readFileArgs
		if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
			return "", fmt.Errorf("invalid read_file arguments: %w", err)
		}
		return runReadFile(a.cfg.workspaceRoot, a.cfg.yolo, args)
	case toolWriteFile:
		if !a.isToolEnabled(runtime, toolWriteFile) {
			return "", fmt.Errorf("%s is disabled; enable with --allow-tool %s", toolWriteFile, toolWriteFile)
		}
		var args writeFileArgs
		if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
			return "", fmt.Errorf("invalid write_file arguments: %w", err)
		}
		return runWriteFile(a.cfg.workspaceRoot, a.cfg.yolo, args)
	case toolAppendFile:
		if !a.isToolEnabled(runtime, toolAppendFile) {
			return "", fmt.Errorf("%s is disabled; enable with --allow-tool %s", toolAppendFile, toolAppendFile)
		}
		var args appendFileArgs
		if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
			return "", fmt.Errorf("invalid append_file arguments: %w", err)
		}
		return runAppendFile(a.cfg.workspaceRoot, a.cfg.yolo, args)
	case toolExecuteProgram:
		if !a.isToolEnabled(runtime, toolExecuteProgram) {
			return "", fmt.Errorf("%s is disabled; enable with --allow-tool %s", toolExecuteProgram, toolExecuteProgram)
		}
		var args executeProgramArgs
		if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
			return "", fmt.Errorf("invalid execute_program arguments: %w", err)
		}
		return runExecuteProgram(ctx, a.cfg.workspaceRoot, a.cfg.yolo, args)
	case toolExecuteSkill:
		if !a.isToolEnabled(runtime, toolExecuteSkill) {
			return "", fmt.Errorf("%s is disabled; enable with --allow-tool %s", toolExecuteSkill, toolExecuteSkill)
		}
		var args executeSkillArgs
		if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
			return "", fmt.Errorf("invalid execute_skill arguments: %w", err)
		}
		return runExecuteSkill(ctx, a.cfg.workspaceRoot, a.cfg.yolo, a.skills, args)
	case toolListSkills:
		if !a.isToolEnabled(runtime, toolListSkills) {
			return "", fmt.Errorf("%s is disabled by current policy", toolListSkills)
		}
		return runListSkills(a.skills), nil
	case toolReadSkill:
		if !a.isToolEnabled(runtime, toolReadSkill) {
			return "", fmt.Errorf("%s is disabled by current policy", toolReadSkill)
		}
		var args readSkillArgs
		if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
			return "", fmt.Errorf("invalid read_skill arguments: %w", err)
		}
		return runReadSkill(a.skills, args)
	case toolCreateSubagent:
		if !a.isToolEnabled(runtime, toolCreateSubagent) {
			return "", fmt.Errorf("%s is disabled; enable with --allow-tool %s", toolCreateSubagent, toolCreateSubagent)
		}
		var args createSubagentArgs
		if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
			return "", fmt.Errorf("invalid create_subagent arguments: %w", err)
		}
		session, err := a.subagents.create(runtime, args)
		if err != nil {
			return "", err
		}
		return marshalToolResult(a.subagents.snapshotLocked(session, false))
	case toolRunSubagent:
		if !a.isToolEnabled(runtime, toolRunSubagent) {
			return "", fmt.Errorf("%s is disabled; enable with --allow-tool %s", toolRunSubagent, toolRunSubagent)
		}
		var args runSubagentArgs
		if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
			return "", fmt.Errorf("invalid run_subagent arguments: %w", err)
		}
		session, err := a.subagents.run(ctx, runtime, args)
		if err != nil {
			return "", err
		}
		return marshalToolResult(a.subagents.snapshotLocked(session, true))
	case toolAwaitSubagent:
		if !a.isToolEnabled(runtime, toolAwaitSubagent) {
			return "", fmt.Errorf("%s is disabled; enable with --allow-tool %s", toolAwaitSubagent, toolAwaitSubagent)
		}
		var args awaitSubagentArgs
		if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
			return "", fmt.Errorf("invalid await_subagent arguments: %w", err)
		}
		session, err := a.subagents.await(ctx, runtime, args)
		if err != nil {
			return "", err
		}
		return marshalToolResult(a.subagents.snapshotLocked(session, true))
	case toolListSubagents:
		if !a.isToolEnabled(runtime, toolListSubagents) {
			return "", fmt.Errorf("%s is disabled; enable with --allow-tool %s", toolListSubagents, toolListSubagents)
		}
		var args listSubagentsArgs
		if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
			return "", fmt.Errorf("invalid list_subagents arguments: %w", err)
		}
		items, err := a.subagents.list(runtime, args)
		if err != nil {
			return "", err
		}
		return marshalToolResult(items)
	case toolReadSubagent:
		if !a.isToolEnabled(runtime, toolReadSubagent) {
			return "", fmt.Errorf("%s is disabled; enable with --allow-tool %s", toolReadSubagent, toolReadSubagent)
		}
		var args readSubagentArgs
		if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
			return "", fmt.Errorf("invalid read_subagent arguments: %w", err)
		}
		payload, err := a.subagents.read(runtime, args)
		if err != nil {
			return "", err
		}
		return marshalToolResult(payload)
	case toolCancelSubagent:
		if !a.isToolEnabled(runtime, toolCancelSubagent) {
			return "", fmt.Errorf("%s is disabled; enable with --allow-tool %s", toolCancelSubagent, toolCancelSubagent)
		}
		var args cancelSubagentArgs
		if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
			return "", fmt.Errorf("invalid cancel_subagent arguments: %w", err)
		}
		session, err := a.subagents.cancel(runtime, args)
		if err != nil {
			return "", err
		}
		return marshalToolResult(a.subagents.snapshotLocked(session, true))
	default:
		return "", fmt.Errorf("unknown tool %q", call.Function.Name)
	}
}

func (a *app) isToolEnabled(runtime *agentRuntime, name string) bool {
	if runtime == nil {
		return a.cfg.allowedTools[name]
	}
	return runtime.allowedTools[name]
}

func (a *app) rootRuntime() *agentRuntime {
	return &agentRuntime{
		sessionID:         rootAgentID,
		depth:             0,
		role:              agentRoleCoordinator,
		allowedTools:      cloneAllowedTools(a.cfg.allowedTools),
		maxToolIterations: a.cfg.maxIterations,
	}
}

func (a *app) runSubagentSession(ctx context.Context, runtime *agentRuntime, session *subagentSession) (string, error) {
	if runtime == nil {
		return "", errors.New("runtime is required")
	}
	toolset := buildAgentTools(runtime.allowedTools)
	question := strings.TrimSpace(session.Question)
	if question == "" {
		return "", errors.New("subagent question is empty")
	}
	return a.runConversation(ctx, question, runtime, toolset, false)
}

func marshalToolResult(value any) (string, error) {
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return "", err
	}
	return string(raw), nil
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
	tools := []apiTool{}
	if enabled[toolWebSearch] {
		tools = append(tools, specWebSearch())
	}
	if enabled[toolFetchPage] {
		tools = append(tools, specFetchPage())
	}
	if enabled[toolListFiles] {
		tools = append(tools, specListFiles())
	}
	if enabled[toolReadFile] {
		tools = append(tools, specReadFile())
	}
	if enabled[toolListSkills] {
		tools = append(tools, specListSkills())
	}
	if enabled[toolReadSkill] {
		tools = append(tools, specReadSkill())
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
	if enabled[toolCreateSubagent] {
		tools = append(tools, specCreateSubagent())
	}
	if enabled[toolRunSubagent] {
		tools = append(tools, specRunSubagent())
	}
	if enabled[toolAwaitSubagent] {
		tools = append(tools, specAwaitSubagent())
	}
	if enabled[toolListSubagents] {
		tools = append(tools, specListSubagents())
	}
	if enabled[toolReadSubagent] {
		tools = append(tools, specReadSubagent())
	}
	if enabled[toolCancelSubagent] {
		tools = append(tools, specCancelSubagent())
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
