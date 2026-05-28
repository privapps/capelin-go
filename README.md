# capelin-go

`capelin-go` is a lightweight Go runtime for AI agents. Write your agents using Claude Code, Codex, or any other AI tool — then run them with capelin-go, which is far more lightweight and controllable than the heavy runtimes those tools ship with.

## Features

- one-shot task execution
- **interactive mode** (`-i` / `--interactive`): multi-turn REPL sharing a single conversation history
- model loop with tool calling (`/chat/completions`)
- Claude-style skill discovery from:
  - `.agents/skills` (project-local)
  - `~/.agents/skills` (user-level)
- web tools: `web_search`, `fetch_page`
- file tools: `list_files`, `read_file`
- always-on multi-agent orchestration tools:
  - `create_subagent`, `run_subagent`, `await_subagent`
  - `list_subagents`, `read_subagent`, `cancel_subagent`
- opt-in mutating/risky tools:
  - `write_file`, `edit_file`, `append_file`
  - `execute_program`, `execute_skill`

## Tool safety model

- `write_file`, `edit_file`, `append_file`, `execute_program`, and `execute_skill` are **disabled by default**
- enable explicitly with repeatable `--allow-tool` flags
- subagent orchestration tools are **always enabled** (no flag needed)
- file and execution cwd are constrained to current working directory
- dangerous command patterns are blocked when execution is enabled (bypassed by `--yolo`)
- fetch blocks localhost/private network targets
- subagents inherit parent tool policy and can only further restrict allowed tools
- subagents enforce depth/children/timeout limits and bounded parallel workers
- when a parent is at `MaxChildren`, `create_subagent` defaults to `overflow_mode="wait_for_slot"` (bounded by timeout, default 300s); use `overflow_mode="fail_fast"` for immediate rejection
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

Interactive mode (multi-turn REPL with shared conversation history):

```bash
# Start with an initial question, then follow up interactively
./capelin-go -i "summarize this repo"

# Or just open the REPL directly (no initial question required)
./capelin-go -i
```

Type `exit` or `quit` (or press Ctrl+D) to end an interactive session.

**Keybindings:**

| Key                    | Action                                    |
|------------------------|-------------------------------------------|
| ↑ / ↓                 | Navigate history                          |
| ← / →                 | Move cursor                               |
| Home / Ctrl+A          | Jump to start of line                     |
| End / Ctrl+E           | Jump to end of line                       |
| Backspace / Delete     | Delete character                          |
| Ctrl+W / Alt+Backspace | Delete previous word                      |
| Ctrl+K                 | Delete to end of line                     |
| Ctrl+U                 | Clear entire line                         |
| Ctrl+J                 | Submit line (same as Enter)               |
| Ctrl+C *(non-empty)*   | Clear current line and reprompt           |
| Ctrl+C *(empty line)*  | Exit session                              |
| Ctrl+D                 | Exit session (EOF)                        |
| Ctrl+L                 | Clear screen                              |

Command history is persisted to `~/.local/capelin-go/history`.

Enable extra tools:

```bash
./capelin-go --allow-tool write_file --allow-tool edit_file --allow-tool execute_program --allow-tool execute_skill "use vPass skill to share a secret"
```

Subagent orchestration is enabled by default — no flags needed:

```bash
./capelin-go "break this task into workers and aggregate results"
```

Tune subagent limits (all have env var equivalents, see below):

```bash
./capelin-go \
  --subagent-max-depth 1 --subagent-max-children 8 --subagent-max-parallel 4 --subagent-timeout-seconds 300 \
  "coordinate workers then combine outputs"
```

Use a different model and reasoning effort for subagents (leave unset to inherit root values):

```bash
./capelin-go \
  --subagent-model gpt-4o --subagent-reasoning-effort high \
  "break this task into workers and aggregate results"
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
- `MAX_ITERATIONS` — root agent tool-call iteration cap (default: 40; overridden by `--max-iterations`); always wraps up gracefully on limit
- `SUBAGENT_MAX_DEPTH` — maximum subagent nesting depth (default: 1; overridden by `--subagent-max-depth`)
- `SUBAGENT_MAX_CHILDREN` — maximum active subagents (pending/queued/running) a single parent can hold at once (default: 8; overridden by `--subagent-max-children`)
- `SUBAGENT_MAX_PARALLEL` — maximum concurrently running parallel subagents (default: 4; overridden by `--subagent-max-parallel`)
- `SUBAGENT_TIMEOUT_SECONDS` — default subagent execution timeout in seconds (default: 300; overridden by `--subagent-timeout-seconds`)
- `SUBAGENT_MAX_RESULT_CHARS` — maximum characters returned per subagent result (default: 8000; overridden by `--subagent-max-result-chars`)
- `SUBAGENT_MAX_AGGREGATE_CHARS` — maximum total characters across all subagent results in a single turn (default: 12000; overridden by `--subagent-max-aggregate-chars`)
- `SUBAGENT_MAX_ITERATIONS` — maximum tool-call iterations per subagent (default: 20; overridden by `--subagent-max-iterations`)
- `SUBAGENT_MODEL` — model ID used for subagents (default: inherits `MODEL`; overridden by `--subagent-model`)
- `SUBAGENT_REASONING_EFFORT` — reasoning effort for subagents (default: inherits `REASONING_EFFORT`; set to `none` to omit; overridden by `--subagent-reasoning-effort`)

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

# Subagent orchestration limits (env vars: SUBAGENT_MAX_DEPTH, SUBAGENT_MAX_CHILDREN,
# SUBAGENT_MAX_PARALLEL, SUBAGENT_TIMEOUT_SECONDS, SUBAGENT_MAX_RESULT_CHARS,
# SUBAGENT_MAX_AGGREGATE_CHARS, SUBAGENT_MAX_ITERATIONS; also settable via CLI flags)
SUBAGENT_MAX_DEPTH = 1
SUBAGENT_MAX_CHILDREN = 8
SUBAGENT_MAX_PARALLEL = 4
SUBAGENT_TIMEOUT_SECONDS = 300
SUBAGENT_MAX_RESULT_CHARS = 8000
SUBAGENT_MAX_AGGREGATE_CHARS = 12000
SUBAGENT_MAX_ITERATIONS = 20

# Subagent model and reasoning effort (leave blank to inherit root MODEL and REASONING_EFFORT)
# env vars: SUBAGENT_MODEL, SUBAGENT_REASONING_EFFORT; also settable via CLI flags
SUBAGENT_MODEL =
SUBAGENT_REASONING_EFFORT =
```

Edit that file to set your preferred model, server URL, or other defaults without needing environment variables every time. Environment variables and CLI flags still take priority over config file values.
