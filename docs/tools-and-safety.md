# Tools and Safety

Capelin gives the assistant tools so it can do useful work instead of only writing suggestions. You normally describe the result you want, and the assistant chooses the appropriate tool.

## Web tools

### Search the public web

Ask for current information, sources, or comparisons. Capelin searches DuckDuckGo first and uses Bing if the first service fails. Search results include titles, links, and short descriptions.

Example:

```bash
./capelin-go "find reliable sources about the current bicycle rules in Vancouver"
```

Search is limited to a manageable number of results per search. Ask for a focused question when you need better results.

### Read a public page

Ask the assistant to open a known public URL. HTML pages are converted into readable text; other public content is returned as text when possible.

```bash
./capelin-go "open https://example.com and summarize the pricing"
```

For safety, page fetching rejects localhost, private network addresses, and local-only hostnames. This means it cannot be used to inspect a private service running on your computer.

## Workspace tools

In local command-line mode, the assistant can:

- list files and folders;
- read a file, optionally using a line range;
- inspect the current working folder while answering a question.

The normal working boundary is the folder where Capelin starts. Absolute paths, parent-folder escapes, and symbolic-link escapes are rejected. Hidden folders are skipped when listing. Large files and very large directory listings are limited so one request does not overwhelm the assistant.

Example:

```bash
./capelin-go "review the README and list the three clearest improvements"
```

## Optional file changes

These tools are disabled unless you explicitly enable them:

- `write_file` — create or replace a file;
- `edit_file` — replace one exact piece of text;
- `append_file` — add text to the end of a file.

Enable only the tools needed for a task:

```bash
./capelin-go \
  --allow-tool write_file \
  --allow-tool edit_file \
  "update the documentation and show me what changed"
```

The assistant still operates inside the working-folder boundary. `edit_file` requires the text to be found exactly once, which helps prevent an unintended broad replacement.

## Optional program execution

`execute_program` is disabled by default. When enabled, it runs a named program with separate arguments and a chosen working folder. It does not run a command through a shell. Unsafe command patterns, shell launchers, and malformed command names are blocked unless you use `--yolo`.

```bash
./capelin-go --allow-tool execute_program \
  "run the project's tests and summarize the result"
```

Long-running programs can time out. Use a task-specific timeout only when necessary, and avoid enabling execution for tasks that only require reading or research.

## Skills

Skills are local guidance files that teach Capelin a repeatable workflow. They can be discovered and read by the assistant, or selected explicitly with `$skill-name` in local one-shot or interactive mode.

Selecting a skill does not run its commands and does not grant new permissions. If a skill needs to run a command, `execute_skill` must be enabled and the command must be declared by that skill. This keeps a skill from silently bypassing the normal safety checks.

Server mode does not load local skills. A `$name` sent through the server is ordinary user text.

## Worker assistants

Worker assistants are useful when a task contains independent parts. Ask Capelin to split the work, investigate the parts, and combine the results:

```bash
./capelin-go "ask separate workers to inspect security, usability, and missing tests, then combine the report"
```

Workers inherit the parent task's permissions and can only use a smaller set of tools, never a larger one. They can be started in parallel, monitored, read, or cancelled. Their results and combined output are length-limited.

New-install defaults are:

| Limit | Default |
| --- | ---: |
| Worker nesting depth | 1 |
| Active workers for one parent | 8 |
| Parallel workers | 4 |
| Default worker timeout | 600 seconds |
| Maximum worker result | 8000 characters |
| Maximum combined result | 12000 characters |
| Tool-use rounds per worker | 20 |

## Interactive sessions and goals

Interactive sessions are saved as UUID-named snapshots in
`.capelin-go/sessions/` under the current workspace. A snapshot includes the
conversation, timestamps, session metadata, and the authoritative todo list.
Writes are atomic, and malformed snapshots are skipped when listing sessions
or choosing the newest valid resume target.

Use `/session-list` to inspect saved conversations, `/session-new` to start a
separate conversation, `/session-resume [ID|PREFIX]` to switch conversations,
and `/session-rename <name>` to assign a durable label. A bare
`/session-resume` selects the newest valid snapshot. Startup resume uses
`capelin-go -i --resume [ID|PREFIX]`.

The `update_todos` tool replaces the complete ordered checklist. Valid statuses
are `pending`, `in_progress`, `completed`, and `cancelled`. An accepted `/goal`
requires a non-empty checklist with every item completed and a goal-only
`complete_goal` call containing a non-empty summary and evidence list before it
reports success. The completion claim is tied to the current goal generation
and final checklist; a later checklist replacement or new objective invalidates
it. Deterministic tool failures (for example missing paths, invalid arguments,
disabled tools, and failed commands) are returned to the model as recoverable
results. Provider failures, cancellation, persistence failures, stalled
checklists, and iteration exhaustion are reported as incomplete and retain the
active objective for bare `/goal` resume. Three consecutive recoverable-error
turns also activate a bounded recovery guard. Goal execution is bounded
independently from normal tool iterations by `--max-goal-iterations N` or
`MAX_GOAL_ITERATIONS` (ordinary default `20`, goal profile default `64`).

The accepted-goal profile is larger but still bounded: root tool-use
iterations `256`, goal-loop iterations `64`, worker depth `2`, worker
parallelism `8`, worker tool-use iterations `100`, worker aggregate output
`48000` characters, tool parallelism `16`, and per-tool timeout `300` seconds.
Explicit flags and environment settings override these fallbacks. Customized
saved values remain in effect; saved values equal to ordinary built-in defaults
are treated as generated baselines for goal fallback. The profile is resolved
in memory, and the generated settings file always retains the ordinary
defaults. `--yolo` remains required for `/goal` but does not select these
budgets by itself.

Ordinary non-goal work, including a non-goal `--yolo` invocation, retains root
iterations `40`, worker depth/parallelism/iterations `1/4/20`, worker aggregate
output `12000` characters, tool parallelism `8`, and a `60`-second tool
timeout. Only an accepted `/goal` selects the larger values above.

Provider defaults are `https://opencode.ai/zen/v1/chat/completions`,
`deepseek-v4-flash-free`, `public`, and `high` for endpoint, model, token, and
reasoning effort. CLI values override environment values, which override saved
configuration, which override built-in defaults. Saved numeric values equal to
ordinary defaults are generated baselines for goal fallback; custom saved
values remain explicit. An active goal snapshot stores no budget-mode key:
resuming it recomputes the current goal profile, while completing or stopping
the goal restores ordinary limits for later turns.

During long accepted-goal turns, the existing output sink emits a generic
working heartbeat after about five seconds and every ten seconds, including
total and current-turn elapsed time. It stops before terminal goal status
output and is not persisted.

The default mode favors reading, research, and controlled workspace access. Use repeatable `--allow-tool` flags for specific actions that change files or run programs.

`--yolo` enables all optional tools and removes the working-folder path restriction. It also bypasses the normal dangerous-command checks. In interactive mode it is additionally required for `/goal`; without it, `/goal` cannot reset the checklist or make a provider request. Use it only when you fully trust the task and the working environment:

```bash
./capelin-go --yolo "make the requested changes across the project"
```

In server mode, the tool set is always restricted to public web access and worker-assistant tools. `--yolo` does not expose local file or program tools through the server.
