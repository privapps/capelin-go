package app

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"capelin-go/internal/policy"
)

// This file contains only the small config-file compatibility fixtures still
// exercised by the app package's historical assertions. Production config
// parsing belongs exclusively to internal/config and does not use any of the
// helpers below.

const (
	defaultEndpoint      = "http://localhost:8235/v1/chat/completions"
	defaultModel         = "gpt-5-mini"
	defaultReasoning     = "medium"
	defaultMaxIterations = 40
)

var optInTools = policy.OptInTools()

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

# Subagent model and reasoning effort (leave blank to inherit root MODEL and REASONING_EFFORT;
# set reasoning effort to none or nil to omit it from requests)
# env vars: SUBAGENT_MODEL, SUBAGENT_REASONING_EFFORT; also settable via CLI flags
SUBAGENT_MODEL =
SUBAGENT_REASONING_EFFORT =

# Parallel tool execution (env vars: TOOL_MAX_PARALLEL, TOOL_TIMEOUT_SECONDS, TOOL_RETRY_ON_TIMEOUT)
# Empty = default (8). Also settable via CLI flags.
TOOL_MAX_PARALLEL = 8
TOOL_TIMEOUT_SECONDS = 60
TOOL_RETRY_ON_TIMEOUT = true
`

// readCfg is retained for tests that assert the old env-over-file precedence.
func readCfg(key string, fileCfg map[string]string, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	if value := strings.TrimSpace(fileCfg[key]); value != "" {
		return value
	}
	return fallback
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

// ensureConfigFile is a test-only compatibility fixture. The canonical
// production implementation is internal/config.ensureConfigFile.
func ensureConfigFile() (map[string]string, error) {
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
		if strings.TrimSpace(existing["ENDPOINT"]) == "" {
			return map[string]string{}, fmt.Errorf("config file %s is missing ENDPOINT", path)
		}
		if err := upsertConfigFileKeys(path); err != nil {
			fmt.Fprintf(os.Stderr, "[capelin-go] warning: updating config file: %v\n", err)
		}
	}

	return readConfigFile(path)
}

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
