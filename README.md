# capelin-go

`capelin-go` is a lightweight Go runtime for AI agents. Write your agents using Claude Code, Codex, or any other AI tool — then run them with capelin-go, which is far more lightweight and controllable than the heavy runtimes those tools ship with.

## Features

- one-shot task execution
- **REPL mode** (`-i` / `--interactive`): multi-turn readline REPL sharing a single conversation history
- **TUI mode** (`-tui` / `--tui`): full terminal UI with multi-agent tree, auto-falls back to REPL on non-TTY
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

Suppress intermediate tool output, showing only the final answer (one-shot mode):

```bash
./capelin-go --final-only "summarize repository structure"
```

REPL mode (multi-turn readline REPL with shared conversation history):

```bash
# Start with an initial question, then follow up interactively
./capelin-go -i "summarize this repo"

# Or just open the REPL directly (no initial question required)
./capelin-go -i
```

TUI mode (full terminal UI with multi-agent tree; falls back to REPL on non-TTY):

```bash
./capelin-go -tui "summarize this repo"

# Or open the TUI directly
./capelin-go -tui
```

Type `exit`, `quit`, `/exit`, or `/quit` (or press Ctrl+D) to end a REPL session.

**Keybindings:**

| Key                    | Action                                    |
|------------------------|-------------------------------------------|
| `Enter`                | Insert newline in input panel *(TUI)*     |
| `Ctrl+Enter`           | Submit input *(TUI)*                      |
| `Ctrl+J`               | Insert newline in input panel *(TUI)*     |
| ↑ / ↓                 | Navigate history *(readline fallback)* or cursor movement in input *(TUI)* |
| ← / →                 | Move cursor *(readline fallback)* or TUI panel switch after `F12` |
| Home / Ctrl+A          | Jump to start of line                     |
| End / Ctrl+E           | Jump to end of line *(readline fallback only; in TUI mode `Ctrl+E` toggles copy mode)* |
| Backspace / Delete     | Delete character                          |
| Ctrl+W / Alt+Backspace | Delete previous word                      |
| Ctrl+K                 | Delete to end of line                     |
| Ctrl+U                 | Clear entire line                         |
| Ctrl+C *(non-empty)*   | Clear current line and reprompt           |
| Ctrl+C *(empty line)*  | Exit session                              |
| Ctrl+D                 | Exit session (EOF)                        |
| Ctrl+L                 | Clear screen                              |
| `Esc` `Esc` *(in input)* | Clear input field                       |
| `Esc` `Esc` `Esc`    | Cancel current agent                      |
| F1                     | Switch panels (agents → log → input) *(TUI)* |
| F2                     | Focus input panel *(TUI)*                 |
| F3                     | Hide / show agents panel *(TUI)*          |
| F4                     | Open / close log search *(TUI)*           |
| F12                    | Enter TUI panel-navigation focus mode     |

Command history is persisted to `~/.local/capelin-go/history`.

## TUI Mode

`-tui` launches a full TUI with three panels when running on an interactive terminal.

```
┌────────────────┬──────────────────────────────────────────────┐
│  Agents (2)    │  Agent 1                                     │
│                │                                              │
│ ▶ ⠹ Agent 1   │  [System Prompt]                             │
│   └ ⠹ search… │  You are a helpful assistant…                │
│   ◌ Agent 2   │                                              │
│                │  [tool] web_search(...)       ← tool colour  │
│                │  assistant response text...                  │
│                │                     ▼ more messages ▼        │
├────────────────┴──────────────────────────────────────────────┤
│  > input text here (type / for commands, %% for skill picker)  │
├───────────────────────────────────────────────────────────────┤
│  Enter: newline | Ctrl+Enter: submit | Triple Esc: cancel | F3: hide menu | F4: search | Ctrl+E: copy | /quit  gpt-5-mini  ~/workspace │
└───────────────────────────────────────────────────────────────┘
```

### Multi-agent architecture

The TUI uses a **Root → Agents → Subagents** model:

- **Root** is a non-agent container shown at the top of the tree. Selecting root and sending a message auto-creates a new top-level agent.
- **Top-level Agents** each have an independent conversation history. Labels auto-generate after the first turn (e.g. `1::research deepseek costs`). Create new ones with `/new`.
- **Subagents** are spawned by top-level agents using the `create_subagent` / `run_subagent` tools and appear as children in the tree.

The **input panel** routes messages to the currently selected top-level agent. When a subagent is selected, only `/save` is available; use `/save <file>` to export its log.

### Panels

- **Agents panel (left, 1/5 width):** live tree of agents and subagents. Top-level agents show auto-generated names (`N::short title`) after their first turn.
- **Log panel (right, 4/5 width):** shows the selected agent's transcript starting with the system prompt, with lightweight markdown rendering for headings, lists, inline code, code blocks, bold/italic, and tables. Tool calls appear dimmed. Panel title shows the agent name; a spinner (`⠋⠙⠹…`) and **bold** text appear when the agent is busy.
- **Input panel (bottom):** send messages to the selected top-level agent. Type `/` to see available commands. Type `%%` anywhere to open the **skill picker** (see below).
- **Status bar (bottom row):** hotkey hints on the left; selected LLM model and current working directory on the right.

**Hotkeys**

| Input | Action |
|-------|--------|
| `F1` | Switch between panels (agents → log → input) |
| `F2` | Focus input panel |
| `F3` | Hide / show agents panel |
| `F4` | Open / close log search (type to search, `Enter`/`n`=next, `N`=prev, `Esc`=close) |
| `F12` | Enter panel-focus mode (advanced navigation) |
| `←` / `→` *(in focus mode)* | Switch to agents / log panel |
| `↑` / `↓` *(in focus mode)* | Cycle focus between panels (exits focus mode) |
| `Tab` / `Shift+Tab` *(in focus mode)* | Cycle panels while staying in focus mode |
| `M` *(in focus mode)* | Maximize or restore the focused panel |
| `C` *(in focus mode)* | Cancel the selected agent/subagent |
| `Esc` | Cancel focus mode / close search; also restores a maximized panel |
| `Esc` `Esc` *(in input panel)* | Clear input field |
| `Esc` `Esc` `Esc` *(anywhere)* | Cancel current agent |
| `Ctrl+Enter` | Submit input (newline with plain Enter) |
| `Ctrl+J` | Insert newline in input panel |
| `Ctrl+C` | Press twice within 2 s to exit (single press shows a warning) |
| `Ctrl+E` | Toggle copy mode: releases mouse to terminal for text selection |
| Mouse click | Switch focus to clicked panel; click tree node to switch agent |
| `Enter` *(on tree node)* | Select agent and move keyboard focus to input panel |
| `PgUp` / `PgDn` | Scroll log and control auto-follow |

**Slash commands** (type `/` in input to show autocomplete; use `↑`/`↓` to navigate, `Enter`/`Tab` to accept)

| Command | Action |
|---------|--------|
| `/quit` or `/exit` | Exit the TUI |
| `/new` or `/session-new` | Create a new top-level agent and switch to it |
| `/session-resume` | Typing `/session-resume ` opens the interactive (filterable) session picker of non-open sessions. Pressing `Enter` on bare `/session-resume` also opens it |
| `/session-resume <uuid-prefix>` | Directly resume the session matching that UUID prefix (no popup) |
| `/session-abandon` | Remove current top-level agent from the menu (session saved on disk, can be resumed later) |
| `/session-destroy` | Remove current agent and permanently delete its session file from disk |
| `/session-cancel` | Cancel the currently running agent |
| `/session-fork [full\|last\|summary] [msg]` | Fork current agent. Typing `/session-fork ` opens the mode picker. No args → interactive mode selector. `full` (default) = copy full history; `last` = last assistant message only; `summary` = LLM-summarized. Optional `[msg]` queued as first input *(top-level only)* |
| `/workspace` | Typing `/workspace ` opens the interactive workspace picker. Pressing `Enter` on bare `/workspace` also opens it |
| `/workspace <name>` | Load a saved workspace by name (no popup) |
| `/workspace-new` | Create a new empty workspace (clears all agents) |
| `/workspace-save <name>` | Save current workspace under the given name |
| `/reset` | Reset current agent's conversation to system-prompt only *(top-level only)* |
| `/compact` | Summarize conversation to reduce context size *(top-level only)* |
| `/save <filename>` | Save the current agent's log to a file (color tags stripped) |
| `/append-to-agent [full\|last\|summary] <id> [extra text…]` | Copy context from this agent to another. Typing `/append-to-agent ` opens the mode + agent selector. No args → interactive mode + agent selector. Mode default = `last`: `last` = last assistant reply; `full` = full conversation; `summary` = LLM summary. Any text after `<id>` is appended to the message *(top-level only)* |
| `/help` | Print available commands in the log panel |

**`%%` skill picker**

Type `%%` anywhere in the input field to open a skill selection popup. The list shows all skills loaded from `.agents/skills/` and `~/.agents/skills/` with their descriptions. Navigate with `↑`/`↓`, press `Enter` to select (inserts `%%<skillname>%%` in place of `%%`), or press `Esc` to cancel (removes `%%`). The `%%name%%` marker keeps subsequent keystrokes from re-opening the picker. You can have multiple skill references in one message, e.g. `please use %%jira-cli%% to create a ticket`.

**Session persistence**

Each top-level agent's conversation is automatically saved to `.capelin-go/sessions/<UUID>.json`
after every turn. A short descriptive name (e.g. `1::research deepseek costs`) is generated
by the LLM after the first turn and stored in the snapshot — it appears in both the agents panel
and the `/session-resume` picker. The session UUID is shown at the top of the log panel when the
agent starts or when a prior session is resumed. Type `/session-resume ` (with trailing space) from
any agent to open the interactive (filterable) picker of non-open sessions, or use `/session-resume <uuid-prefix>`
to resume directly without the popup. Use `/session-abandon` to remove an agent from the menu
without deleting its session data, or `/session-destroy` to also delete the session file from disk.

**Agent status legend**

- `⠋⠙⠹…` busy/running = **bold** yellow (dark) / darkorange (light)
- `✓` completed = green / darkgreen
- `✗` failed/timed_out = red / darkred
- `◌` idle = white (dark) / black (light)
- `○` pending (subagent) = gray / darkgray
- `—` cancelled = darkgray / dimgray

**Theme**

Auto-detected from terminal background (`COLORFGBG`). Force with `--theme light|dark|auto` or `CAPELIN_THEME=light` env var. Dark and light themes supported.

Detection precedence:
1. `--theme` CLI flag
2. `CAPELIN_THEME` env var (config file also supported)
3. `COLORFGBG` environment variable (set by many terminals)
4. macOS `AppleInterfaceStyle` (system appearance preference)
5. Falls back to dark theme

Falls back to readline-based REPL on non-TTY environments.

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
- `TOOL_MAX_PARALLEL` — maximum concurrent tool calls per LLM turn (default: 8; overridden by `--tool-max-parallel`); set to 0 to disable parallelism (serial execution)
- `TOOL_TIMEOUT_SECONDS` — per-tool deadline in seconds (default: 60; overridden by `--tool-timeout-seconds`); set to 0 for no per-tool timeout
- `TOOL_RETRY_ON_TIMEOUT` — retry once on tool timeout (default: true; overridden by `--tool-retry-on-timeout` / `--no-tool-retry-on-timeout`)

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

# Parallel tool execution (env vars: TOOL_MAX_PARALLEL, TOOL_TIMEOUT_SECONDS, TOOL_RETRY_ON_TIMEOUT)
# 0 = disable (serial); empty = default. Also settable via CLI flags.
TOOL_MAX_PARALLEL = 8
TOOL_TIMEOUT_SECONDS = 60
TOOL_RETRY_ON_TIMEOUT = true
```

Edit that file to set your preferred model, server URL, or other defaults without needing environment variables every time. Environment variables and CLI flags still take priority over config file values.
