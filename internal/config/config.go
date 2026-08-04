// Package config owns command configuration parsing, persistent defaults, and precedence rules.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"capelin-go/internal/policy"
)

const (
	defaultEndpoint           = "http://localhost:8235/v1/chat/completions"
	defaultModel              = "gpt-5-mini"
	defaultToken              = ""
	defaultReasoning          = "medium"
	defaultMaxIterations      = 40
	defaultMaxGoalIterations  = 20
	defaultToolMaxParallel    = 8
	defaultToolTimeoutSec     = 60
	defaultToolRetryOnTimeout = true
)

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

var alwaysEnabledTools = policy.AlwaysEnabledTools()
var optInTools = policy.OptInTools()

type Config struct {
	Endpoint                  string
	Model                     string
	Token                     string
	Reasoning                 string
	SystemPrompt              string
	ShowVersion               bool
	Interactive               bool
	FinalOnly                 bool
	InitialQuestion           string
	ResumeID                  string
	ResumeRequested           bool
	WorkspaceRoot             string
	AllowedTools              map[string]bool
	Yolo                      bool // enables all tools and unrestricted paths
	MaxIterations             int
	MaxGoalIterations         int
	Subagents                 SubagentConfig
	ServerPort                int
	ServerAllowedOrigins      string
	ServerAllowedTargets      string
	ServerAllowPrivateTargets string
	ServerSecurityEnabled     bool
	ToolMaxParallel           int  // max concurrent tool calls per LLM turn (0 = serial; empty = default 8)
	ToolTimeoutSec            int  // per-tool deadline in seconds (0 = no per-tool cap; empty = default 60)
	ToolRetryOnTimeout        bool // retry once on timeout (0 = disable; empty = default true)
	AsyncTimeout              time.Duration
	Debug                     bool
}

const (
	rootAgentID = "root"
)

const (
	createSubagentOverflowWaitForSlot = "wait_for_slot"
	createSubagentOverflowFailFast    = "fail_fast"
)

const (
	defaultSubagentMaxDepth          = 1
	defaultSubagentMaxChildren       = 8
	defaultSubagentMaxParallel       = 4
	defaultSubagentDefaultTimeoutSec = 600  // 10 minutes
	defaultSubagentMaxTimeoutSec     = 1800 // 30 minutes
	defaultSubagentToolIterations    = 20
	defaultSubagentResultChars       = 8000
	defaultSubagentAggregateCount    = 12
	defaultSubagentAggregateChars    = 12000
)

type agentRole string

const (
	agentRoleCoordinator agentRole = "coordinator"
	agentRoleWorker      agentRole = "worker"
)

type SubagentConfig struct {
	MaxDepth          int
	MaxChildren       int
	MaxParallel       int
	DefaultTimeoutSec int
	MaxTimeoutSec     int
	MaxToolIterations int
	MaxResultChars    int
	MaxAggregateCount int
	MaxAggregateChars int
	Model             string
	ReasoningEffort   string
}

func defaultSubagentRuntimeConfig() SubagentConfig {
	return SubagentConfig{
		MaxDepth:          defaultSubagentMaxDepth,
		MaxChildren:       defaultSubagentMaxChildren,
		MaxParallel:       defaultSubagentMaxParallel,
		DefaultTimeoutSec: defaultSubagentDefaultTimeoutSec,
		MaxTimeoutSec:     defaultSubagentMaxTimeoutSec,
		MaxToolIterations: defaultSubagentToolIterations,
		MaxResultChars:    defaultSubagentResultChars,
		MaxAggregateCount: defaultSubagentAggregateCount,
		MaxAggregateChars: defaultSubagentAggregateChars,
	}
}

func (c *SubagentConfig) normalize() {
	if c.MaxDepth <= 0 {
		c.MaxDepth = defaultSubagentMaxDepth
	}
	if c.MaxChildren <= 0 {
		c.MaxChildren = defaultSubagentMaxChildren
	}
	if c.MaxParallel <= 0 {
		c.MaxParallel = defaultSubagentMaxParallel
	}
	if c.DefaultTimeoutSec <= 0 {
		c.DefaultTimeoutSec = defaultSubagentDefaultTimeoutSec
	}
	if c.MaxTimeoutSec <= 0 {
		c.MaxTimeoutSec = defaultSubagentMaxTimeoutSec
	}
	if c.MaxTimeoutSec < c.DefaultTimeoutSec {
		c.MaxTimeoutSec = c.DefaultTimeoutSec
	}
	if c.MaxToolIterations <= 0 {
		c.MaxToolIterations = defaultSubagentToolIterations
	}
	if c.MaxResultChars <= 0 {
		c.MaxResultChars = defaultSubagentResultChars
	}
	if c.MaxAggregateCount <= 0 {
		c.MaxAggregateCount = defaultSubagentAggregateCount
	}
	if c.MaxAggregateChars <= 0 {
		c.MaxAggregateChars = defaultSubagentAggregateChars
	}
}

var ErrHelpRequested = errors.New("help requested")

func Load(args []string) (Config, error) {
	fileCfg, err := ensureConfigFileForMode(hasServerPortFlag(args))
	if err != nil {
		return Config{}, fmt.Errorf("config file: %w", err)
	}

	filtered := make([]string, 0, len(args))
	allowedTools := map[string]bool{}
	for _, name := range alwaysEnabledTools {
		allowedTools[name] = true
	}
	yolo := false
	debug := false
	interactive := false
	finalOnly := false
	maxIter := 0
	maxGoalIter := 0
	resumeID := ""
	resumeRequested := false
	serverPort := 0
	subagentCfg := SubagentConfig{} // zero = "not set by flag"; env/file/normalize fills gaps
	toolMaxParallel := 0            // zero = "not set by flag"
	toolTimeoutSec := 0             // zero = "not set by flag"
	toolRetryOnTimeout := -1        // -1 = "not set by flag"; 0 = explicitly false; 1 = explicitly true

	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "-h" || arg == "--help" || arg == "-help":
			return Config{}, ErrHelpRequested
		case arg == "--version" || arg == "-version":
			return Config{ShowVersion: true}, nil
		case arg == "-i" || arg == "--interactive":
			interactive = true
		case arg == "--yolo":
			yolo = true
			for name := range optInTools {
				allowedTools[name] = true
			}
		case arg == "--allow-tool":
			if i+1 >= len(args) {
				return Config{}, errors.New("--allow-tool requires a value")
			}
			i++
			name := strings.TrimSpace(args[i])
			if _, ok := optInTools[name]; !ok {
				return Config{}, fmt.Errorf("unknown or non-opt-in tool %q", name)
			}
			allowedTools[name] = true
		case strings.HasPrefix(arg, "--allow-tool="):
			name := strings.TrimSpace(strings.TrimPrefix(arg, "--allow-tool="))
			if _, ok := optInTools[name]; !ok {
				return Config{}, fmt.Errorf("unknown or non-opt-in tool %q", name)
			}
			allowedTools[name] = true
		case arg == "--subagent-max-depth":
			if i+1 >= len(args) {
				return Config{}, errors.New("--subagent-max-depth requires a value")
			}
			i++
			value, err := parsePositiveInt(args[i], "--subagent-max-depth")
			if err != nil {
				return Config{}, err
			}
			subagentCfg.MaxDepth = value
		case strings.HasPrefix(arg, "--subagent-max-depth="):
			value, err := parsePositiveInt(strings.TrimPrefix(arg, "--subagent-max-depth="), "--subagent-max-depth")
			if err != nil {
				return Config{}, err
			}
			subagentCfg.MaxDepth = value
		case arg == "--subagent-max-children":
			if i+1 >= len(args) {
				return Config{}, errors.New("--subagent-max-children requires a value")
			}
			i++
			value, err := parsePositiveInt(args[i], "--subagent-max-children")
			if err != nil {
				return Config{}, err
			}
			subagentCfg.MaxChildren = value
		case strings.HasPrefix(arg, "--subagent-max-children="):
			value, err := parsePositiveInt(strings.TrimPrefix(arg, "--subagent-max-children="), "--subagent-max-children")
			if err != nil {
				return Config{}, err
			}
			subagentCfg.MaxChildren = value
		case arg == "--subagent-max-parallel":
			if i+1 >= len(args) {
				return Config{}, errors.New("--subagent-max-parallel requires a value")
			}
			i++
			value, err := parsePositiveInt(args[i], "--subagent-max-parallel")
			if err != nil {
				return Config{}, err
			}
			subagentCfg.MaxParallel = value
		case strings.HasPrefix(arg, "--subagent-max-parallel="):
			value, err := parsePositiveInt(strings.TrimPrefix(arg, "--subagent-max-parallel="), "--subagent-max-parallel")
			if err != nil {
				return Config{}, err
			}
			subagentCfg.MaxParallel = value
		case arg == "--subagent-timeout-seconds":
			if i+1 >= len(args) {
				return Config{}, errors.New("--subagent-timeout-seconds requires a value")
			}
			i++
			value, err := parsePositiveInt(args[i], "--subagent-timeout-seconds")
			if err != nil {
				return Config{}, err
			}
			subagentCfg.DefaultTimeoutSec = value
		case strings.HasPrefix(arg, "--subagent-timeout-seconds="):
			value, err := parsePositiveInt(strings.TrimPrefix(arg, "--subagent-timeout-seconds="), "--subagent-timeout-seconds")
			if err != nil {
				return Config{}, err
			}
			subagentCfg.DefaultTimeoutSec = value
		case arg == "--subagent-max-result-chars":
			if i+1 >= len(args) {
				return Config{}, errors.New("--subagent-max-result-chars requires a value")
			}
			i++
			value, err := parsePositiveInt(args[i], "--subagent-max-result-chars")
			if err != nil {
				return Config{}, err
			}
			subagentCfg.MaxResultChars = value
		case strings.HasPrefix(arg, "--subagent-max-result-chars="):
			value, err := parsePositiveInt(strings.TrimPrefix(arg, "--subagent-max-result-chars="), "--subagent-max-result-chars")
			if err != nil {
				return Config{}, err
			}
			subagentCfg.MaxResultChars = value
		case arg == "--subagent-max-aggregate-chars":
			if i+1 >= len(args) {
				return Config{}, errors.New("--subagent-max-aggregate-chars requires a value")
			}
			i++
			value, err := parsePositiveInt(args[i], "--subagent-max-aggregate-chars")
			if err != nil {
				return Config{}, err
			}
			subagentCfg.MaxAggregateChars = value
		case strings.HasPrefix(arg, "--subagent-max-aggregate-chars="):
			value, err := parsePositiveInt(strings.TrimPrefix(arg, "--subagent-max-aggregate-chars="), "--subagent-max-aggregate-chars")
			if err != nil {
				return Config{}, err
			}
			subagentCfg.MaxAggregateChars = value
		case arg == "--subagent-max-iterations":
			if i+1 >= len(args) {
				return Config{}, errors.New("--subagent-max-iterations requires a value")
			}
			i++
			value, err := parsePositiveInt(args[i], "--subagent-max-iterations")
			if err != nil {
				return Config{}, err
			}
			subagentCfg.MaxToolIterations = value
		case strings.HasPrefix(arg, "--subagent-max-iterations="):
			value, err := parsePositiveInt(strings.TrimPrefix(arg, "--subagent-max-iterations="), "--subagent-max-iterations")
			if err != nil {
				return Config{}, err
			}
			subagentCfg.MaxToolIterations = value
		case arg == "--subagent-model":
			if i+1 >= len(args) {
				return Config{}, errors.New("--subagent-Model requires a value")
			}
			i++
			subagentCfg.Model = strings.TrimSpace(args[i])
		case strings.HasPrefix(arg, "--subagent-model="):
			subagentCfg.Model = strings.TrimSpace(strings.TrimPrefix(arg, "--subagent-model="))
		case arg == "--subagent-reasoning-effort":
			if i+1 >= len(args) {
				return Config{}, errors.New("--subagent-reasoning-effort requires a value")
			}
			i++
			subagentCfg.ReasoningEffort = strings.TrimSpace(args[i])
		case strings.HasPrefix(arg, "--subagent-reasoning-effort="):
			subagentCfg.ReasoningEffort = strings.TrimSpace(strings.TrimPrefix(arg, "--subagent-reasoning-effort="))
		case arg == "--server-port" || arg == "--server":
			if i+1 >= len(args) {
				return Config{}, errors.New("--server-port requires a value")
			}
			i++
			value, err := parsePositiveInt(args[i], "--server-port")
			if err != nil {
				return Config{}, err
			}
			serverPort = value
		case strings.HasPrefix(arg, "--server-port=") || strings.HasPrefix(arg, "--server="):
			valueText := strings.TrimPrefix(arg, "--server-port=")
			if valueText == arg {
				valueText = strings.TrimPrefix(arg, "--server=")
			}
			value, err := parsePositiveInt(valueText, "--server-port")
			if err != nil {
				return Config{}, err
			}
			serverPort = value
		case arg == "--max-iterations":
			if i+1 >= len(args) {
				return Config{}, errors.New("--max-iterations requires a value")
			}
			i++
			value, err := parsePositiveInt(args[i], "--max-iterations")
			if err != nil {
				return Config{}, err
			}
			maxIter = value
		case strings.HasPrefix(arg, "--max-iterations="):
			value, err := parsePositiveInt(strings.TrimPrefix(arg, "--max-iterations="), "--max-iterations")
			if err != nil {
				return Config{}, err
			}
			maxIter = value
		case arg == "--max-goal-iterations":
			if i+1 >= len(args) {
				return Config{}, errors.New("--max-goal-iterations requires a value")
			}
			i++
			value, err := parsePositiveInt(args[i], "--max-goal-iterations")
			if err != nil {
				return Config{}, err
			}
			maxGoalIter = value
		case strings.HasPrefix(arg, "--max-goal-iterations="):
			value, err := parsePositiveInt(strings.TrimPrefix(arg, "--max-goal-iterations="), "--max-goal-iterations")
			if err != nil {
				return Config{}, err
			}
			maxGoalIter = value
		case arg == "--resume":
			resumeRequested = true
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				i++
				resumeID = strings.TrimSpace(args[i])
			} else {
				resumeID = ""
			}
		case strings.HasPrefix(arg, "--resume="):
			resumeRequested = true
			resumeID = strings.TrimSpace(strings.TrimPrefix(arg, "--resume="))
		case arg == "--final-only":
			finalOnly = true
		case arg == "--tool-max-parallel":
			if i+1 >= len(args) {
				return Config{}, errors.New("--tool-max-parallel requires a value")
			}
			i++
			value, err := parsePositiveInt(args[i], "--tool-max-parallel")
			if err != nil {
				return Config{}, err
			}
			toolMaxParallel = value
		case strings.HasPrefix(arg, "--tool-max-parallel="):
			value, err := parsePositiveInt(strings.TrimPrefix(arg, "--tool-max-parallel="), "--tool-max-parallel")
			if err != nil {
				return Config{}, err
			}
			toolMaxParallel = value
		case arg == "--tool-timeout-seconds":
			if i+1 >= len(args) {
				return Config{}, errors.New("--tool-timeout-seconds requires a value")
			}
			i++
			value, err := parsePositiveInt(args[i], "--tool-timeout-seconds")
			if err != nil {
				return Config{}, err
			}
			toolTimeoutSec = value
		case strings.HasPrefix(arg, "--tool-timeout-seconds="):
			value, err := parsePositiveInt(strings.TrimPrefix(arg, "--tool-timeout-seconds="), "--tool-timeout-seconds")
			if err != nil {
				return Config{}, err
			}
			toolTimeoutSec = value
		case arg == "--tool-retry-on-timeout":
			toolRetryOnTimeout = 1
		case arg == "--no-tool-retry-on-timeout":
			toolRetryOnTimeout = 0
		case arg == "--debug" || arg == "-debug":
			debug = true
		case strings.HasPrefix(arg, "-"):
			return Config{}, fmt.Errorf("unknown flag %q", arg)
		default:
			filtered = append(filtered, arg)
		}
	}

	endpoint, err := readEndpoint(fileCfg)
	if err != nil {
		return Config{}, err
	}
	reasoning, err := readReasoningEffort(fileCfg)
	if err != nil {
		return Config{}, err
	}
	workspaceRoot, err := os.Getwd()
	if err != nil {
		return Config{}, fmt.Errorf("resolving workspace root: %w", err)
	}
	// Resolve subagent limits: flag (non-zero) > env > file > built-in default (via normalize).
	// Check env/file only for fields the flag loop left at zero (i.e. not explicitly provided).
	if subagentCfg.MaxDepth == 0 {
		if v, err := parsePositiveInt(readCfg("SUBAGENT_MAX_DEPTH", fileCfg, ""), "SUBAGENT_MAX_DEPTH"); err == nil {
			subagentCfg.MaxDepth = v
		}
	}
	if subagentCfg.MaxChildren == 0 {
		if v, err := parsePositiveInt(readCfg("SUBAGENT_MAX_CHILDREN", fileCfg, ""), "SUBAGENT_MAX_CHILDREN"); err == nil {
			subagentCfg.MaxChildren = v
		}
	}
	if subagentCfg.MaxParallel == 0 {
		if v, err := parsePositiveInt(readCfg("SUBAGENT_MAX_PARALLEL", fileCfg, ""), "SUBAGENT_MAX_PARALLEL"); err == nil {
			subagentCfg.MaxParallel = v
		}
	}
	if subagentCfg.DefaultTimeoutSec == 0 {
		if v, err := parsePositiveInt(readCfg("SUBAGENT_TIMEOUT_SECONDS", fileCfg, ""), "SUBAGENT_TIMEOUT_SECONDS"); err == nil {
			subagentCfg.DefaultTimeoutSec = v
		}
	}
	if subagentCfg.MaxResultChars == 0 {
		if v, err := parsePositiveInt(readCfg("SUBAGENT_MAX_RESULT_CHARS", fileCfg, ""), "SUBAGENT_MAX_RESULT_CHARS"); err == nil {
			subagentCfg.MaxResultChars = v
		}
	}
	if subagentCfg.MaxAggregateChars == 0 {
		if v, err := parsePositiveInt(readCfg("SUBAGENT_MAX_AGGREGATE_CHARS", fileCfg, ""), "SUBAGENT_MAX_AGGREGATE_CHARS"); err == nil {
			subagentCfg.MaxAggregateChars = v
		}
	}
	if subagentCfg.MaxToolIterations == 0 {
		if v, err := parsePositiveInt(readCfg("SUBAGENT_MAX_ITERATIONS", fileCfg, ""), "SUBAGENT_MAX_ITERATIONS"); err == nil {
			subagentCfg.MaxToolIterations = v
		}
	}
	subagentCfg.normalize() // fills any remaining zeros with built-in defaults

	// Resolve subagent Model: flag > env > file > inherit root model.
	// Empty string means "not explicitly set"; fall through to next source.
	if subagentCfg.Model == "" {
		subagentCfg.Model = readCfg("SUBAGENT_MODEL", fileCfg, "")
	}
	rootModel := readCfg("MODEL", fileCfg, defaultModel)
	if subagentCfg.Model == "" {
		subagentCfg.Model = rootModel
	}

	// Resolve subagent reasoning effort: flag > env > file > inherit root reasoning.
	// "none" is converted to "" so it is omitted from API requests (same as root reasoning).
	rawSubagentReasoning := subagentCfg.ReasoningEffort
	if rawSubagentReasoning == "" {
		rawSubagentReasoning = readCfg("SUBAGENT_REASONING_EFFORT", fileCfg, "")
	}
	if rawSubagentReasoning == "" {
		subagentCfg.ReasoningEffort = reasoning // inherit already-resolved root reasoning
	} else if strings.EqualFold(rawSubagentReasoning, "none") {
		subagentCfg.ReasoningEffort = ""
	} else {
		subagentCfg.ReasoningEffort = rawSubagentReasoning
	}

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

	// Resolve the outer goal limit independently from the per-turn tool limit.
	// Unlike the older MAX_ITERATIONS handling, an explicitly supplied invalid
	// environment/config value is an error rather than silently falling back.
	if maxGoalIter == 0 {
		if raw := readCfg("MAX_GOAL_ITERATIONS", fileCfg, ""); raw != "" {
			value, err := parsePositiveInt(raw, "MAX_GOAL_ITERATIONS")
			if err != nil {
				return Config{}, err
			}
			maxGoalIter = value
		}
	}
	if maxGoalIter == 0 {
		maxGoalIter = defaultMaxGoalIterations
	}

	// Resolve tool config: flag (non-zero) > env > file > built-in default.
	if toolMaxParallel == 0 {
		if env := readCfg("TOOL_MAX_PARALLEL", fileCfg, ""); env != "" {
			if v, err := parsePositiveInt(env, "TOOL_MAX_PARALLEL"); err == nil {
				toolMaxParallel = v
			}
		}
	}
	if toolMaxParallel == 0 {
		toolMaxParallel = defaultToolMaxParallel
	}
	if toolTimeoutSec == 0 {
		if env := readCfg("TOOL_TIMEOUT_SECONDS", fileCfg, ""); env != "" {
			if v, err := parsePositiveInt(env, "TOOL_TIMEOUT_SECONDS"); err == nil {
				toolTimeoutSec = v
			}
		}
	}
	if toolTimeoutSec == 0 {
		toolTimeoutSec = defaultToolTimeoutSec
	}
	if toolRetryOnTimeout == -1 {
		toolRetryOnTimeout = boolToInt(readBoolCfg("TOOL_RETRY_ON_TIMEOUT", fileCfg, defaultToolRetryOnTimeout))
	}

	return Config{
		Endpoint:                  endpoint,
		Model:                     rootModel,
		Token:                     readCfg("TOKEN", fileCfg, defaultToken),
		Reasoning:                 reasoning,
		SystemPrompt:              readSystemPrompt(fileCfg),
		Interactive:               interactive,
		FinalOnly:                 finalOnly,
		InitialQuestion:           strings.TrimSpace(strings.Join(filtered, " ")),
		ResumeID:                  resumeID,
		ResumeRequested:           resumeRequested,
		WorkspaceRoot:             workspaceRoot,
		AllowedTools:              allowedTools,
		Yolo:                      yolo,
		MaxIterations:             maxIter,
		MaxGoalIterations:         maxGoalIter,
		Subagents:                 subagentCfg,
		ServerPort:                serverPort,
		ServerAllowedOrigins:      readCfg("SERVER_ALLOWED_ORIGINS", fileCfg, ""),
		ServerAllowedTargets:      readCfg("SERVER_ALLOWED_TARGETS", fileCfg, ""),
		ServerAllowPrivateTargets: readCfg("SERVER_ALLOW_PRIVATE_TARGETS", fileCfg, "false"),
		ServerSecurityEnabled:     serverPort > 0,
		ToolMaxParallel:           toolMaxParallel,
		ToolTimeoutSec:            toolTimeoutSec,
		ToolRetryOnTimeout:        toolRetryOnTimeout != 0,
		Debug:                     debug,
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

func readBoolCfg(key string, fileCfg map[string]string, fallback bool) bool {
	value := readCfg(key, fileCfg, "")
	if value == "" {
		return fallback
	}
	switch strings.ToLower(value) {
	case "true", "1", "yes", "on":
		return true
	case "false", "0", "no", "off":
		return false
	default:
		return fallback
	}
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
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

func readEndpoint(fileCfg map[string]string) (string, error) {
	value := readCfg("ENDPOINT", fileCfg, defaultEndpoint)
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
	if strings.EqualFold(value, "none") || strings.EqualFold(value, "nil") {
		return "", nil
	}
	return value, nil
}

// configFilePath returns the path to the user-level config file.
// If the env var CAPELIN_CONFIG_FILE is set, it is used as-is (useful for tests
// and users who want a non-default location).
func configFilePath() string {
	if override := strings.TrimSpace(os.Getenv("CAPELIN_CONFIG_FILE")); override != "" {
		return override
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".local", "capelin-go", "config.ini")
}

const defaultConfigFileContent = `# capelin-go configuration
# Edit this file to set persistent defaults.
# Priority: CLI flags > environment variables > this file > built-in defaults.

ENDPOINT = http://localhost:8235/v1/chat/completions
MODEL = gpt-5-mini
TOKEN =
REASONING_EFFORT = medium
SYSTEM_PROMPT =
MAX_ITERATIONS = 40
MAX_GOAL_ITERATIONS = 20

# Subagent orchestration limits (env vars: SUBAGENT_MAX_DEPTH, SUBAGENT_MAX_CHILDREN,
# SUBAGENT_MAX_PARALLEL, SUBAGENT_TIMEOUT_SECONDS, SUBAGENT_MAX_RESULT_CHARS,
# SUBAGENT_MAX_AGGREGATE_CHARS, SUBAGENT_MAX_ITERATIONS; also settable via CLI flags)
SUBAGENT_MAX_DEPTH = 1
SUBAGENT_MAX_CHILDREN = 8
SUBAGENT_MAX_PARALLEL = 4
SUBAGENT_TIMEOUT_SECONDS = 600
SUBAGENT_MAX_RESULT_CHARS = 8000
SUBAGENT_MAX_AGGREGATE_CHARS = 12000
SUBAGENT_MAX_ITERATIONS = 20

# Subagent Model and Reasoning effort (leave blank to inherit root MODEL and REASONING_EFFORT;
# set Reasoning effort to none or nil to omit it from requests)
# env vars: SUBAGENT_MODEL, SUBAGENT_REASONING_EFFORT; also settable via CLI flags
SUBAGENT_MODEL =
SUBAGENT_REASONING_EFFORT =

# Parallel tool execution (env vars: TOOL_MAX_PARALLEL, TOOL_TIMEOUT_SECONDS, TOOL_RETRY_ON_TIMEOUT)
# Empty = default (8). Also settable via CLI flags.
TOOL_MAX_PARALLEL = 8
TOOL_TIMEOUT_SECONDS = 60
TOOL_RETRY_ON_TIMEOUT = true

# Server mode security (empty means no browser or dynamic outbound access).
SERVER_ALLOWED_ORIGINS =
SERVER_ALLOWED_TARGETS =
SERVER_ALLOW_PRIVATE_TARGETS = false
`

// ensureConfigFile creates the config file with defaults if it does not exist,
// appends any keys missing from an existing file, then reads and returns its
// key=value pairs.
func ensureConfigFile() (map[string]string, error) {
	return ensureConfigFileForMode(false)
}

func ensureConfigFileForMode(serverMode bool) (map[string]string, error) {
	path := configFilePath()
	if path == "" {
		return map[string]string{}, nil
	}

	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return map[string]string{}, fmt.Errorf("creating config dir: %w", err)
		}
		if err := os.WriteFile(path, []byte(defaultConfigFileContent), 0o644); err != nil {
			return map[string]string{}, fmt.Errorf("writing default config: %w", err)
		}
	} else if err != nil {
		return map[string]string{}, fmt.Errorf("checking config file: %w", err)
	} else {
		if !info.Mode().IsRegular() {
			return map[string]string{}, fmt.Errorf("config path is not a regular file: %s", path)
		}
		existing, err := readConfigFile(path)
		if err != nil {
			return map[string]string{}, err
		}
		if !serverMode && strings.TrimSpace(existing["ENDPOINT"]) == "" {
			return map[string]string{}, fmt.Errorf("config file %s is missing ENDPOINT", path)
		}
		// File exists: append any keys present in the default template but absent in the file.
		if err := upsertConfigFileKeys(path); err != nil {
			// Non-fatal: warn but continue with whatever is in the file.
			fmt.Fprintf(os.Stderr, "[capelin-go] warning: updating config file: %v\n", err)
		}
	}

	return readConfigFile(path)
}

func hasServerPortFlag(args []string) bool {
	for _, arg := range args {
		if arg == "--server-port" || arg == "--server" || strings.HasPrefix(arg, "--server-port=") || strings.HasPrefix(arg, "--server=") {
			return true
		}
	}
	return false
}

// upsertConfigFileKeys appends any keys defined in defaultConfigFileContent that are
// missing from the existing file at path. User-set values are never touched.
func upsertConfigFileKeys(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading config file: %w", err)
	}
	existing := map[string]bool{}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if idx := strings.IndexByte(line, '='); idx >= 0 {
			if key := strings.TrimSpace(line[:idx]); key != "" {
				existing[key] = true
			}
		}
	}

	// Collect keys+defaults from defaultConfigFileContent that are absent in the file.
	var additions strings.Builder
	for _, line := range strings.Split(defaultConfigFileContent, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		idx := strings.IndexByte(trimmed, '=')
		if idx < 0 {
			continue
		}
		key := strings.TrimSpace(trimmed[:idx])
		if key != "" && !existing[key] {
			if additions.Len() == 0 {
				additions.WriteString("\n# Keys added by capelin-go upgrade.\n")
			}
			additions.WriteString(line + "\n")
		}
	}
	if additions.Len() == 0 {
		return nil
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("opening config file for update: %w", err)
	}
	defer f.Close()
	_, err = f.WriteString(additions.String())
	return err
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
