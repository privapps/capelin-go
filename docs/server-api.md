# Server and API Guide

Server mode turns Capelin into a local HTTP proxy. A caller sends an OpenAI-style chat request to Capelin; Capelin forwards the task to the remote AI service and returns the completed answer.

## Start the server

```bash
./capelin-go --server-port 8899
```

The server listens on port `8899` in this example. Keep the terminal open while clients use it.

Check that it is running:

```bash
curl http://localhost:8899/health
```

Expected response:

```json
{"status":"ok"}
```

## Send a synchronous request

Every chat request must identify the remote AI endpoint. The simplest form uses the `endpoint` query parameter:

```bash
curl -X POST \
  'http://localhost:8899/?endpoint=https%3A%2F%2Fexample.com%2Fv1%2Fchat%2Fcompletions' \
  -H 'Authorization: Bearer YOUR_REMOTE_TOKEN' \
  -H 'Content-Type: application/json' \
  -d '{
    "model": "your-model",
    "messages": [
      {"role": "user", "content": "Summarize this topic in five points."}
    ]
  }'
```

The caller waits for the answer. A task that uses tools may take several minutes.

You can identify the remote endpoint in either of these ways:

1. URL-encoded path, such as `/https%3A%2F%2Fexample.com%2Fv1%2Fchat%2Fcompletions`;
2. hexadecimal path beginning with `~`, such as `/~68747470733a...`;
3. `?endpoint=https://example.com/v1/chat/completions`.

The query-parameter form is usually easiest to generate safely.

### Request rules

- Use `POST` for chat requests.
- Include a non-empty `messages` list.
- Include `Authorization: Bearer <token>`. Capelin forwards this token to the remote service.
- Omit `stream` or set it to `false`; streaming is not supported.
- `model` is optional and falls back to the configured model.
- `reasoning.effort` is optional and can override the configured reasoning setting.
- Request bodies are limited to 10 MB.

The server adds its own instructions to the conversation. Through server mode, the assistant can search the public web, read public pages, and use worker assistants. It cannot access local files, change files, run local programs, or load local skills.

Server-mode worker scheduling uses the same bounded two-phase contract as local
mode. A batch of `create_subagent` calls returns handles without waiting for
active-child capacity; `run_subagent` queues admitted handles and the scheduler
honors the configured child and parallel-worker limits. A full bounded
allowance returns a recoverable capacity error for retry after terminal work
releases capacity. Cancellation finalizes pending/queued children and stops
running descendants. Worker listings and aggregate reads expose pending,
queued, running, completed, failed, cancelled, and timed-out states. Server
permissions and network restrictions are unchanged.

## Read the response

The response follows the usual chat-completion shape:

```json
{
  "id": "capelin-...",
  "object": "chat.completion",
  "model": "your-model",
  "choices": [
    {
      "index": 0,
      "message": {
        "role": "assistant",
        "content": "The completed answer."
      },
      "finish_reason": "stop"
    }
  ]
}
```

When the remote service provides reasoning or tool activity, Capelin may add a `reasoning` field to the assistant message. It is omitted when there is no reasoning content or tool activity.

## Submit an asynchronous request

Use the same request body and authentication, but add `/async/` before the remote endpoint:

```bash
curl -X POST \
  'http://localhost:8899/async/?endpoint=https%3A%2F%2Fexample.com%2Fv1%2Fchat%2Fcompletions' \
  -H 'Authorization: Bearer YOUR_REMOTE_TOKEN' \
  -H 'Content-Type: application/json' \
  -d '{
    "model": "your-model",
    "messages": [
      {"role": "user", "content": "Research this topic and compare the options."}
    ]
  }'
```

Capelin immediately returns HTTP `202` with an ID:

```json
{"id":"550e8400-e29b-41d4-a716-446655440000"}
```

Poll for the result:

```bash
curl 'http://localhost:8899/data?key=550e8400-e29b-41d4-a716-446655440000'
```

While the job is running, the poll returns `404`. When it finishes, the value is the same response shape as a synchronous request. A failed or timed-out job returns an error object instead.

Async results remain available for one hour. Up to 16 async jobs can run at once; when that capacity is full, a new request receives HTTP `429`.

## Use the temporary data store

The `/data` endpoint is an in-memory key-value store. It is useful for passing a result ID between processes or keeping a small temporary value.

Store a value:

```bash
curl -X PUT 'http://localhost:8899/data?key=note&ttl=60' \
  --data 'temporary text'
```

Read it:

```bash
curl 'http://localhost:8899/data?key=note'
```

Rules:

- default lifetime: 8 hours;
- maximum lifetime: 7 days;
- `ttl=0` uses the default lifetime;
- key length: up to 40 characters;
- value size: up to 2 MB;
- values disappear when they expire or when the server stops;
- the store has a finite overall capacity and removes older entries when necessary.

## Browser access and safety

The server allows cross-origin browser requests and accepts `GET`, `PUT`, `POST`, and `OPTIONS` where appropriate. It requires a Bearer token for requests sent to the remote AI service, but it is not a user account system. Do not expose the server to an untrusted network without adding protection around it.

## Common API errors

| Error | Likely cause |
| --- | --- |
| `endpoint required` | No valid endpoint path or `endpoint` query parameter was supplied |
| `Authorization: Bearer <token> header is required` | The request has no usable Bearer token |
| `messages array is required and must not be empty` | The request has no conversation messages |
| `streaming is not supported` | The request set `stream` to `true` |
| `key not found` | An async job is still running, has expired, or used the wrong ID |
| `async capacity exhausted` | Too many async jobs are already active |

For more cases, see the [Troubleshooting FAQ](troubleshooting.md).

## Local idle hooks and server mode

`IDLE_HOOK_COMMAND` and `IDLE_HOOK_ARGS` configure a local-only idle hook. The
command is run once after a local one-shot terminal outcome or after an
interactive turn has been finalized and the prompt is idle. `IDLE_HOOK_ARGS`
must be a JSON string array. Environment values override non-empty saved values
using the normal CLI > environment > saved config > built-in defaults
precedence. A blank command is disabled.

A configured hook requires `--allow-tool execute_program` or `--yolo`. It uses
direct executable invocation without a shell and retains the existing workspace,
dangerous-command, bounded-output, timeout, and process-cancellation safeguards.
Interactive hooks run in the background and are serialized; one-shot shutdown
drains them. Hook failures are reported as bounded stderr diagnostics without
changing the original task result.

These settings never apply to this HTTP server. Both synchronous and
asynchronous requests ignore a configured local hook, even with YOLO or
program-execution permission. Server responses, async result delivery,
admission limits, error handling, and the restricted server tool catalog are
unchanged.
