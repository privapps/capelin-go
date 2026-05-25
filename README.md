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

## Tool safety model

- `write_file`, `append_file`, `execute_program`, and `execute_skill` are **disabled by default**
- enable explicitly with repeatable `--allow-tool` flags
- file and execution cwd are constrained to current working directory
- dangerous command patterns are blocked even when execution is enabled
- fetch blocks localhost/private network targets
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
