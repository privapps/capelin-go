# Troubleshooting FAQ

## Capelin says the endpoint is invalid

Check `ENDPOINT` in `~/.local/capelin-go/config.ini` or in the environment. It must be a complete URL such as:

```text
https://example.com/v1/chat/completions
```

If the URL ends in `/responses`, Capelin uses the Responses request format. Otherwise it uses Chat Completions. Remove extra spaces and confirm the service is reachable.

## The default endpoint does not respond

The built-in endpoint is:

```text
http://localhost:8235/v1/chat/completions
```

That service is not started by Capelin. Set `ENDPOINT` to the URL of the AI service you intend to use, and set `TOKEN` if that service requires a token.

## I see `model request failed`

Check the full message after the error. Rate-limit responses and server failures are retried automatically, but a persistent problem usually means one of these:

- the AI service is down;
- the URL or path is wrong;
- the model name is not accepted;
- the token is missing or expired;
- the service rejected the request format.

Run once with `--debug` to inspect the request and response details. Treat the debug output as private.

## A tool says it is disabled

File changes, program execution, and skill execution are disabled by default. Enable only the required tool:

```bash
./capelin-go --allow-tool write_file "create the requested report"
```

The assistant cannot enable a tool by itself. A `$skill-name` reference also does not grant permission.

## A path is rejected as absolute or outside the workspace

In the normal safety mode, file tools may use only paths inside the folder where Capelin started. Start Capelin in the intended project folder and use relative paths such as `docs/guide.md`.

Use `--yolo` only when removing this boundary is genuinely necessary and safe.

## A web page cannot be fetched

The URL must use `http://` or `https://`. Localhost, private network addresses, and local-only hostnames are blocked. Try the public version of the page instead. A page may also fail because it is unavailable, requires a login, redirects too many times, or returns content that cannot be read as text.

## A file is too large or a line range is invalid

The file-reading tools have size and output limits. Ask for a smaller file, a specific section, or a line range that starts at `1` and ends after the start line. Directory listings are also limited.

## Program execution is blocked or times out

Safe mode rejects shell launchers and dangerous command patterns. Use a direct program name with separate arguments rather than a shell command. If execution is truly required, enable `execute_program` explicitly.

Programs have a time limit. Ask for a smaller operation or set a suitable task timeout, up to the allowed maximum. Avoid `--yolo` unless you accept the additional risk.

## A skill cannot be found

Check that the skill exists as a directory containing `SKILL.md` in either:

```text
.agents/skills/<skill-name>/SKILL.md
~/.agents/skills/<skill-name>/SKILL.md
```

The name must match exactly. If the file is malformed or missing its required header, Capelin may report a skill-loading error when it starts.

## The shell changes `$skill-name` before Capelin sees it

Quote a one-shot request containing `$`:

```bash
./capelin-go '$research compare these designs'
```

In interactive mode, type the reference directly at the prompt.

## A worker assistant timed out or could not start

Workers have limits for depth, active children, parallel work, result size, and time. Ask for fewer workers, split the task into smaller pieces, or increase the relevant `--subagent-*` setting. If the parent has reached its active-worker limit, wait for existing workers to finish or use a fail-fast request when supported.

## An async poll returns `404`

This is normal while the job is still running. Continue polling the same ID. A `404` after the expected completion time usually means the result expired, the server restarted, or the ID was copied incorrectly.

Async results are kept for one hour. The general `/data` store uses its own lifetime rules and is also cleared when the server stops.

## An async request returns `429`

The server already has the maximum number of async jobs in progress. Wait and submit again. The server returns this response before accepting the new job.

## The `/data` endpoint rejects my value

Check the key and value limits:

- key must be present and no longer than 40 characters;
- value must be 2 MB or smaller;
- a negative TTL is invalid;
- a TTL above seven days is reduced to seven days;
- the store has finite in-memory capacity.

## `/save` says there is no assistant response

Run a successful request first. `/save` stores only the latest successful text answer; it does not save tool logs or an unfinished request.

If saving fails, check that the current working folder is writable. The destination is `last-response.md`, and an existing file is replaced.

## Interactive mode falls back to basic input

Capelin uses a simpler line reader when the terminal cannot provide the full interactive interface. You can still submit ordinary prompts, use `/exit`, `/quit`, and `/save`, and press Ctrl+D to leave. Tab completion and some multiline-paste behavior may not be available.

## The settings file causes a startup error

Check these locations and values:

- `CAPELIN_CONFIG_FILE` must point to a regular file;
- the file must contain a valid `ENDPOINT` entry;
- numeric limits must be positive integers;
- command-line flags must use a value where required.

Capelin creates missing settings on first run and adds newly introduced keys to an existing file without replacing your saved values. If an old file contains a different worker timeout or other limit, that saved value remains active.

## Interactive session cannot be resumed

Session snapshots are project-local. Start Capelin from the same workspace that
contains `.capelin-go/sessions/`, then use:

```bash
./capelin-go -i --resume <full-id-or-prefix>
```

A bare `--resume` chooses the newest valid snapshot. If an explicit ID reports
`not found`, `ambiguous`, or `load session` failure, check the full ID with
`/session-list` or inspect whether the snapshot is malformed. Invalid snapshots
are skipped for bare resume and listing, but an explicitly selected invalid
snapshot is reported instead of silently selecting another conversation.

## A goal is reported incomplete

`/goal` requires `--yolo` and an authoritative non-empty `update_todos`
checklist. Success requires every checklist item to have status `completed`.
Provider or tool errors, cancellation, cancelled items, an empty checklist, two
unchanged incomplete checklist snapshots, three consecutive recoverable-tool
error turns, and the outer iteration limit all produce an incomplete outcome.
Deterministic tool errors are normally returned to the model so it can correct
the next call; persistent provider, persistence, cancellation, and runtime
errors stop the goal immediately. Use bare `/goal` to continue a saved
incomplete checklist, or start a new objective with `/goal <objective>`.

Set the outer limit independently with `--max-goal-iterations N` or
`MAX_GOAL_ITERATIONS`. This does not change the normal per-turn
`--max-iterations` setting.

## The server rejects my API request

Confirm all of the following:

1. the request uses `POST`;
2. the remote endpoint is supplied in the path or `endpoint` query parameter;
3. the request has `Authorization: Bearer <token>`;
4. the JSON contains a non-empty `messages` list;
5. `stream` is omitted or `false`;
6. the request body is no larger than 10 MB.

For asynchronous requests, use the same checks with `/async/` before the endpoint path.
