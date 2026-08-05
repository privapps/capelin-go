# Command-Line Guide

This guide explains how to use Capelin directly from a terminal.

## Before you start

You need:

1. a Capelin executable;
2. access to an AI service that accepts the configured request format;
3. the service URL and, if required, an access token.

If you are working from the repository, `make build` creates a platform-specific executable under `dist/`. The examples use `./capelin-go`; replace that path with the executable you have.

## Run one task

Put the task in quotes so the shell passes the complete sentence as one request:

```bash
./capelin-go "summarize this project for a new user"
```

Capelin may search the web, read files, or ask worker assistants for help before it returns the answer. Normal answers are printed to standard output. Tool activity and status messages are printed separately, so scripts can use the answer without also receiving the progress messages.

### Show only the final answer

Use `--final-only` when another program needs clean output:

```bash
./capelin-go --final-only "list the three most important risks in this folder"
```

Intermediate tool messages are hidden, but the assistant still uses the tools it is allowed to use.

### See request details

Use `--debug` when diagnosing a connection or request problem:

```bash
./capelin-go --debug "answer a short test question"
```

Debug output is written to the error stream and can contain request details, prompts, and returned data. Do not share it if it contains private information.

## Use interactive mode

Interactive mode keeps the conversation history while the program is open:

```bash
./capelin-go --interactive
```

You can also start with a first question:

```bash
./capelin-go --interactive "inspect this folder and suggest a cleanup plan"
```

Every ordinary line is sent as a new turn. Follow-up questions can refer to earlier answers without repeating the full context.

### Interactive commands

- `/exit` or `/quit` closes the session without sending the command to the assistant.
- `/save` writes the latest successful text answer to `last-response.md` in the current working folder. It replaces that file if it already exists.
- `/compact` asks the configured model for a no-tools summary of retained
  history. It accepts no arguments, preserves the system prompt, session
  metadata, and checklist, clears provider continuation and loaded-skill
  bookkeeping, and supports the existing cancellation controls. A provider,
  validation, cancellation, or persistence failure leaves the prior session
  unchanged. Compaction is an explicit command; it is not automatic.
- `/session-new [prompt]` saves the current session, creates a blank one, and
  optionally submits `prompt` as its first normal turn.
- `/session-list` lists valid saved UUIDs newest-first and marks the current
  session. It also shows a deterministic label, recent direct input, message
  count, update timestamp, and checklist progress. `/session-resume [ID|PREFIX]`
  switches to an exact ID, unique prefix, or newest valid snapshot.
- `/session-rename <name>` sets a durable display name; `/session-rename --clear`
  restores the derived topic label.
- `/goal <objective>` starts a fresh checklist-driven objective, while bare
  `/goal` continues an incomplete checklist. These commands require `--yolo`.

Sessions are persisted atomically in `.capelin-go/sessions/`. Use
`--resume [ID|PREFIX]` with `-i` to resume at startup; a bare `--resume` picks
the newest valid snapshot. On exit, Capelin prints only the current UUID and a
`--resume <id>` continuation hint.

The ordinary goal-loop baseline is 20 outer iterations. An accepted goal uses
the separate goal profile's default of 64. Set
`--max-goal-iterations N` or `MAX_GOAL_ITERATIONS`; this is independent of the
per-turn `--max-iterations` limit (ordinary default 40; goal profile default
256). A goal is complete only when the authoritative `update_todos` checklist
is non-empty, every item is `completed`, and the model makes a valid goal-only
`complete_goal` call containing a non-empty summary and evidence list. A
completed checklist without the handshake is progress, not success; Capelin
requests another continuation and eventually reports an explicit incomplete
safeguard outcome. Later checklist changes and new objectives invalidate prior
claims. Deterministic tool errors—such as a missing path, invalid arguments, a
disabled tool, or a failed command—are returned to the model as recoverable
results. Provider failures, parent cancellation, persistence failures, and
unrecoverable runtime errors stop with an explicit incomplete outcome while
retaining the active objective for bare `/goal` resume. Tool-scoped timeouts
retain one bounded retry. While an accepted goal turn is active, Capelin emits
an immediate iteration status and generic working heartbeat (about five
seconds initially, then every ten seconds) with total and current-turn elapsed
time.

The four built-in provider defaults are `ENDPOINT=https://opencode.ai/zen/v1/chat/completions`,
`MODEL=deepseek-v4-flash-free`, `TOKEN=public`, and
`REASONING_EFFORT=high`. CLI flags override environment settings, which
override saved `config.ini` values, which override built-in defaults. Saved
numeric values equal to the ordinary defaults are treated as generated
baselines when a goal profile is resolved; non-default saved values remain
customizations. Goal profile resolution is in memory only and never adds
goal-specific configuration keys.

The goal profile is active only while an accepted `/goal` is running. It is
recomputed for a resumed active goal from the current in-memory configuration,
without a persisted budget-mode field, and ordinary limits are restored after
the goal completes or stops. `--yolo` remains the permission and path-safety
gate; using `--yolo` without `/goal` keeps ordinary limits.

### Reasoning and resumed turns

Set `REASONING_EFFORT` when the selected model supports provider-native
reasoning. Chat Completions replays native `reasoning_content` with the
assistant tool call before sending its tool result. Responses preserves the
provider's reasoning and function-call output items in opaque continuation
state. Interactive sessions save that state only after a successful turn, so a
later prompt or saved-session resume can continue without losing the provider's
reasoning contract.

Older snapshots without continuation state remain valid. If saved state is
stale, malformed, or belongs to another provider, Capelin ignores it and
rebuilds the request from normalized conversation history. An error on the
initial provider request remains visible; Capelin does not silently disable
reasoning and retry with a different configuration.

The command prompt supports Tab completion for slash commands and local skill names. Command history is stored at `~/.local/capelin-go/history`.

`exit` and `quit` without a leading slash are ordinary prompts sent to the model.

On compatible Unix-like terminals, pasting inserts the content into the current prompt without submitting it. Press Enter to send it; multiline paste keeps its line breaks. Piped input and fallback input remain line-based.

While a model or tool turn is running, the prompt shows
`[busy — Esc Esc cancels] > `. Printable and multiline editing remain available,
but Enter and local commands are rejected and the draft is restored instead of
being queued. Press Escape twice, or Ctrl+C, to cancel; the prompt shows
`[cancelling…] > ` until the worker exits, then returns to idle. Background
assistant and tool output redraws the prompt safely. Fallback input reports busy
status and rejects lines until the turn ends; it does not provide editable
drafts or Escape-twice cancellation.

### Keyboard controls

| Key | Action |
| --- | --- |
| Up / Down | Move through command history |
| Left / Right | Move the cursor |
| Home / Ctrl+A | Move to the beginning |
| End / Ctrl+E | Move to the end |
| Ctrl+W / Alt+Backspace | Delete the previous word |
| Ctrl+K | Delete to the end of the line |
| Ctrl+U | Clear the line |
| Ctrl+J | Submit the line |
| Ctrl+C while busy | Cancel the active turn and keep the session open |
| Ctrl+C on a non-empty idle line | Clear the line and try again |
| Ctrl+C on an empty idle line | Exit the session |
| Escape twice while busy | Cancel the active turn and keep the session open |
| Ctrl+D | Exit the session |
| Ctrl+L | Clear the screen |

## Use a local skill

A skill is a local set of task guidance. Capelin looks for skills in:

- `.agents/skills` in the current working folder;
- `.agents/skills` in your home folder.

Project skills take precedence when the same name exists in both places.

In interactive mode, select a skill with `$name`:

```text
$research compare these two approaches
```

The skill supplies guidance for that request. It does not automatically run a command or grant permission to change files or execute programs. Those actions still require the normal tool permissions.

For a one-shot shell request, quote the dollar sign so your shell does not treat it as a variable:

```bash
./capelin-go '$research compare these two approaches'
```

Unknown `$tokens`, escaped `\$tokens`, and server requests remain ordinary text.

## Settings and configuration

On the first run, Capelin creates:

```text
~/.local/capelin-go/config.ini
```

You can point to another settings file with `CAPELIN_CONFIG_FILE`.

Settings are applied in this order, from strongest to weakest:

1. command-line flags;
2. environment settings;
3. `config.ini`;
4. built-in defaults.

Interactive session snapshots and goal checklists are stored separately under
`.capelin-go/sessions/` in the current workspace. They are not part of the
user-level `config.ini` file; use `/session-list` or `--resume` to access them.

Common settings:

| Setting | Purpose | Default |
| --- | --- | --- |
| `ENDPOINT` | Complete AI service URL | `https://opencode.ai/zen/v1/chat/completions` |
| `MODEL` | Model name sent to the service | `deepseek-v4-flash-free` |
| `TOKEN` | Token for the AI service | `public` |
| `REASONING_EFFORT` | Reasoning setting passed to the service | `high` |
| `SYSTEM_PROMPT` | Extra instructions for the assistant | built-in instructions |
| `MAX_ITERATIONS` | Maximum tool-use rounds for the main task | `40` ordinary (`256` goal profile) |
| `MAX_GOAL_ITERATIONS` | Maximum outer iterations for a YOLO-gated `/goal` | `20` ordinary (`64` goal profile) |
| `TOOL_MAX_PARALLEL` | Maximum tools used at the same time | `8` ordinary (`16` goal profile) |
| `TOOL_TIMEOUT_SECONDS` | Default time allowed for one tool | `60` ordinary (`300` goal profile) |
| `TOOL_RETRY_ON_TIMEOUT` | Retry a tool once after a timeout | `true` |

Set reasoning to `none` or `nil` to leave it out of the request.

If the endpoint path ends in `/responses`, Capelin uses the Responses request format. Other endpoint paths use the Chat Completions format.

### Common flags

```text
-i, --interactive                 Keep a multi-turn conversation open
--resume [ID|PREFIX]               Resume newest, exact, or unique-prefix session
--max-goal-iterations N            Set the `/goal` outer iteration limit
--final-only                      Print only the final answer in one-shot mode
--debug                           Show request and response diagnostics
--max-iterations N                Set the main task tool-use limit
--allow-tool NAME                 Enable one optional tool; repeat as needed
--yolo                            Enable all optional tools and remove path limits
--server-port PORT                Start HTTP server mode
--help                            Show command help
--version                         Show the executable version
```

Optional tools are `write_file`, `edit_file`, `append_file`, `execute_program`, and `execute_skill`. Read-only tools and worker-assistant tools are available without an `--allow-tool` flag in local mode. See [Tools and safety](tools-and-safety.md).

Accepted goals use a separate bounded worker profile: depth `2`, parallel
workers `8`, worker iterations `100`, aggregate output `48000` characters,
and a `600`-second worker timeout. Flags and environment settings take
precedence; custom saved values are preserved, while saved values equal to
ordinary defaults are treated as generated baselines for goal fallback. Goal
profile resolution is in memory only. `--yolo` remains the permission and
safety gate and does not select these budgets by itself.

## Worker assistants

For a large task, ask Capelin to divide the work:

```bash
./capelin-go "split this review into independent checks, then combine the findings"
```

Worker assistants can run one after another or in parallel. They have limits so a task cannot create unlimited work. New installations use these defaults:

- maximum nesting depth: `1`;
- maximum active children per parent: `8`;
- maximum parallel workers: `4`;
- default worker timeout: `600` seconds;
- maximum result per worker: `8000` characters;
- maximum combined result: `12000` characters;
- maximum tool-use rounds per worker: `20`.

These values can be changed with matching `--subagent-*` flags, environment settings, or entries in `config.ini`. An existing settings file keeps its saved values, so an older installation may have different defaults.

| Flag | Environment setting | Purpose |
| --- | --- | --- |
| `--subagent-max-depth N` | `SUBAGENT_MAX_DEPTH` | Maximum worker nesting |
| `--subagent-max-children N` | `SUBAGENT_MAX_CHILDREN` | Maximum active workers for one parent |
| `--subagent-max-parallel N` | `SUBAGENT_MAX_PARALLEL` | Maximum workers running together |
| `--subagent-timeout-seconds N` | `SUBAGENT_TIMEOUT_SECONDS` | Default worker time limit |
| `--subagent-max-result-chars N` | `SUBAGENT_MAX_RESULT_CHARS` | Maximum result from one worker |
| `--subagent-max-aggregate-chars N` | `SUBAGENT_MAX_AGGREGATE_CHARS` | Maximum combined worker output |
| `--subagent-max-iterations N` | `SUBAGENT_MAX_ITERATIONS` | Maximum tool-use rounds per worker |
| `--subagent-model NAME` | `SUBAGENT_MODEL` | Model used by workers |
| `--subagent-reasoning-effort VALUE` | `SUBAGENT_REASONING_EFFORT` | Reasoning setting used by workers |

## Graceful limits and retries

The assistant retries temporary model failures such as rate limits and server errors. If a task reaches its tool-use limit, Capelin asks for the best final answer using the information collected so far. If the final request also fails, see the [Troubleshooting FAQ](troubleshooting.md).
