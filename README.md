# capelin-go

`capelin-go` is a lightweight Go runtime for AI agents. Write your agents using Claude Code, Codex, or any other AI tool — then run them with capelin-go, which is far more lightweight and controllable than the heavy runtimes those tools ship with.

## Features

- one-shot task execution (no interactive mode)
- model loop with tool calling (`/chat/completions`)
- Claude-style skill discovery from:
  - `.agents/skills` (project-local)
  - `~/.agents/skills` (user-level)
- web tools: `web_search`, `fetch_page`
- file tools: `list_files`, `read_file`
- opt-in mutating/risky tools:
  - `write_file`, `append_file`
  - `execute_program`, `execute_skill`
- opt-in multi-agent orchestration tools:
  - `create_subagent`, `run_subagent`, `await_subagent`
  - `list_subagents`, `read_subagent`, `cancel_subagent`

## Tool safety model

- `write_file`, `append_file`, `execute_program`, and `execute_skill` are **disabled by default**
- subagent orchestration tools are also **disabled by default**
- enable explicitly with repeatable `--allow-tool` flags
- file and execution cwd are constrained to current working directory
- dangerous command patterns are blocked even when execution is enabled
- fetch blocks localhost/private network targets
- subagents inherit parent tool policy and can only further restrict allowed tools
- subagents enforce depth/children/timeout limits and bounded parallel workers
- **`--yolo`**: enables all opt-in tools and removes path confinement (shorthand for enabling everything)

## Build

```bash
make build
```

## Test

```bash
make test
```

## Run

```bash
./capelin-go "summarize repository structure"
```

Enable extra tools:

```bash
./capelin-go --allow-tool write_file --allow-tool execute_program --allow-tool execute_skill "use vPass skill to share a secret"
```

Enable subagent orchestration:

```bash
./capelin-go \
  --allow-tool create_subagent \
  --allow-tool run_subagent \
  --allow-tool await_subagent \
  --allow-tool list_subagents \
  --allow-tool read_subagent \
  --allow-tool cancel_subagent \
  "break this task into workers and aggregate results"
```

Tune subagent limits (all have env var equivalents, see below):

```bash
./capelin-go --allow-tool create_subagent --allow-tool run_subagent --allow-tool await_subagent \
  --subagent-max-depth 1 --subagent-max-children 8 --subagent-max-parallel 4 --subagent-timeout-seconds 300 \
  "coordinate workers then combine outputs"
```

Tune iteration cap (useful for complex research tasks):

```bash
./capelin-go --max-iterations 80 "research farmers markets in the Lower Mainland and list hours"
```

Enable everything (all tools + unrestricted paths):

```bash
./capelin-go --yolo "refactor all Go files in this repo"
```

## Environment variables

- `BASE_URL` — model server base URL (default: `http://localhost:8235/v1`)
- `MODEL` — model ID (default: `gpt-5-mini`)
- `TOKEN` — optional API token
- `REASONING_EFFORT` — passed through to the model backend; set to `none` to omit the field entirely from the request
- `SYSTEM_PROMPT` (or `systemPrompt`) — prompt override
- `MAX_ITERATIONS` — root agent tool-call iteration cap (default: 40; overridden by `--max-iterations`)
- `ON_MAX_ITERATIONS` — behaviour when the cap is reached: `continue` (default, requests a final answer) or `error` (exits non-zero); overridden by `--on-max-iterations`
- `SUBAGENT_MAX_DEPTH` — maximum subagent nesting depth (default: 1; overridden by `--subagent-max-depth`)
- `SUBAGENT_MAX_CHILDREN` — maximum subagents a single parent can spawn (default: 8; overridden by `--subagent-max-children`)
- `SUBAGENT_MAX_PARALLEL` — maximum concurrently running parallel subagents (default: 4; overridden by `--subagent-max-parallel`)
- `SUBAGENT_TIMEOUT_SECONDS` — default subagent execution timeout in seconds (default: 300; overridden by `--subagent-timeout-seconds`)

## Config file

On first run capelin-go creates `~/.local/capelin-go/config.ini` with default values. If the file already exists, any keys added in newer versions are automatically appended (existing values are never changed).

```ini
# capelin-go configuration
# Edit this file to set persistent defaults.
# Priority: CLI flags > environment variables > this file > built-in defaults.

BASE_URL = http://localhost:8235/v1
MODEL = gpt-5-mini
TOKEN =
REASONING_EFFORT = medium
SYSTEM_PROMPT =
MAX_ITERATIONS = 40
ON_MAX_ITERATIONS = continue

# Subagent orchestration limits (env vars: SUBAGENT_MAX_DEPTH, SUBAGENT_MAX_CHILDREN,
# SUBAGENT_MAX_PARALLEL, SUBAGENT_TIMEOUT_SECONDS; also settable via CLI flags)
SUBAGENT_MAX_DEPTH = 1
SUBAGENT_MAX_CHILDREN = 8
SUBAGENT_MAX_PARALLEL = 4
SUBAGENT_TIMEOUT_SECONDS = 300
```

Edit that file to set your preferred model, server URL, or other defaults without needing environment variables every time. Environment variables and CLI flags still take priority over config file values.
