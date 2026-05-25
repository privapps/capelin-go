# capelin-go

`capelin-go` is a lightweight Go runtime for repeatedly executing skills written by Claude Code, Codex, or other agents.

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

Tune subagent limits (conservative defaults):

```bash
./capelin-go --allow-tool create_subagent --allow-tool run_subagent --allow-tool await_subagent \
  --subagent-max-depth 1 --subagent-max-children 8 --subagent-max-parallel 2 --subagent-timeout-seconds 300 \
  "coordinate two workers then combine outputs"
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
- `REASONING_EFFORT` — passed through as a string to the model backend
- `SYSTEM_PROMPT` (or `systemPrompt`) — prompt override
- `MAX_ITERATIONS` — root agent tool-call iteration cap (default: 40; overridden by `--max-iterations`)
