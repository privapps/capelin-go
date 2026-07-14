# capelin-go

`capelin-go` is a lightweight Go runtime for AI agents. Write your agents using Claude Code, Codex, or any other AI tool — then run them with capelin-go, which is far more lightweight and controllable than the heavy runtimes those tools ship with.

## Features

- one-shot task execution
- **interactive mode** (`-i` / `--interactive`): multi-turn REPL sharing a single conversation history
- **server mode** (`--server-port PORT`): HTTP server accepting OpenAI-format requests (sync + async)
- **async mode** (`/async/...`): non-blocking requests that return a UUID; poll `/data` for results
- model loop with tool calling (`/chat/completions` and `/responses`)
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

**Server mode** (OpenAI-compatible proxy):

```bash
# Start server on port 8899
./capelin-go --server-port 8899

# Option 1: URL-encoded path (recommended)
curl -X POST "http://localhost:8899/https%3A%2F%2Fopencode.ai/zen/v1/chat/completions" \
  -H "Authorization: Bearer public" \
  -d '{"model":"mimo-v2.5-free","messages":[{"role":"user","content":"search for go http best practices"}]}'

# Option 2: Hex-encoded path (useful for obfuscating the endpoint)
# Generate hex: echo -n "https://opencode.ai/zen/v1/chat/completions" | xxd -p
curl -X POST "http://localhost:8899/~68747470733a2f2f6f70656e636f64652e61692f7a656e2f76312f636861742f636f6d706c6574696f6e73" \
  -H "Authorization: Bearer public" \
  -d '{"model":"mimo-v2.5-free","messages":[{"role":"user","content":"search for go http best practices"}]}'

# Option 3: Query parameter
curl -X POST "http://localhost:8899/?endpoint=https://opencode.ai/zen/v1/chat/completions" \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer public" \
  -d '{"model":"mimo-v2.5-free","messages":[{"role":"user","content":"search for go http best practices"}]}'
```

In server mode:
- CORS: browser requests are allowed from any origin; `GET`, `POST`, and `OPTIONS` are supported for local proxy access
- Endpoint: full URL including `/chat/completions` — use URL-encoded path (`%3A%2F%2F` for `://`), hex-encoded path (`~<hex>` for obfuscated endpoints), or `?endpoint=` query parameter
- API token: standard `Authorization: Bearer <token>` header
- Available tools: `web_search`, `fetch_page`, and subagent orchestration (`create_subagent`, `run_subagent`, `await_subagent`, `list_subagents`, `read_subagent`, `cancel_subagent`)
- Response includes a `reasoning` field with LLM thinking, tool call traces, and subagent results
- Connection stays open until the agent completes (may take several minutes)
- Returns OpenAI-format response with the final result

**Raw CORS proxy** (any HTTP(S) endpoint):

```bash
# The target can be URL-encoded in the path, hex-encoded, or passed as ?endpoint=
curl -X PUT "http://localhost:8899/-/https%3A%2F%2Fexample.com%2Fapi%3Fx%3D1" \
  -H "Content-Type: application/json" \
  -d '{"hello":"world"}'
```

Requests to `/-/` preserve the method, body, query, and application headers. Upstream status, body, response headers, and redirects are relayed; browser `OPTIONS` preflight is answered locally with permissive CORS headers. The proxy accepts absolute `http://` and `https://` targets and does not require an Authorization header. To prevent SSRF, the proxy refuses to connect to loopback, private, link-local, multicast, and unspecified addresses (including cloud metadata endpoints); this includes hostnames that resolve to such addresses, so only public destinations are reachable.

**Data endpoint** (in-memory key-value store):

```bash
# Store a value (default TTL: 8 hours)
curl -X PUT "http://localhost:8899/data?key=mykey" -d "myvalue"

# Store with custom TTL (in minutes, max 10080 = 7 days)
curl -X PUT "http://localhost:8899/data?key=mykey&ttl=60" -d "myvalue"

# Retrieve a value
curl "http://localhost:8899/data?key=mykey"
```

Data endpoint details:
- **GET** `/data?key=<key>` — retrieve a value (returns `text/plain`)
- **PUT** `/data?key=<key>` — store a value (body is the value, returns JSON with `ok`, `key`, `ttl`)
- Optional `ttl` parameter: minutes until expiration (default 480, max 10080); `ttl=0` uses default
- Keys: max 40 characters, must be non-empty
- Values: max 2MB
- Expired entries are cleaned up hourly

**Async endpoint** (non-blocking chat completion):

```bash
# Submit async request — returns UUID immediately (HTTP 202)
curl -X POST "http://localhost:8899/async/https%3A%2F%2Fopencode.ai/zen/v1/chat/completions" \
  -H "Authorization: Bearer public" \
  -d '{"model":"mimo-v2.5-free","messages":[{"role":"user","content":"search for go http best practices"}]}'
# → 202 {"id": "550e8400-e29b-41d4-a716-446655440000"}

# Poll for result (404 while processing, full JSON when done)
curl "http://localhost:8899/data?key=550e8400-e29b-41d4-a716-446655440000"
```

Async endpoint details:
- Routes mirror the sync endpoints: `/async/https%3A%2F%2F...`, `/async/~<hex>`, `/async/?endpoint=...`
- Returns `202 Accepted` with `{"id": "<uuid>"}` immediately
- The request runs in the background; result is stored in `/data` with a 1-hour TTL
- Poll `GET /data?key=<uuid>` — returns 404 while processing, full OpenAI-format JSON when complete
- On failure (LLM error, timeout, or internal panic), an error JSON is stored: `{"error": {"message": "...", "type": "async_error"}}`
- Background tasks have a 15-minute timeout; stuck tasks fail gracefully with an error result
- Maximum 16 concurrent async tasks (excess requests block until a slot frees)
- Same request format, auth, and available tools as the sync endpoint

**Response format:**

The response follows the OpenAI chat completion format with an additional `reasoning` field:

```json
{
  "id": "capelin-...",
  "object": "chat.completion",
  "choices": [{
    "index": 0,
    "message": {
      "role": "assistant",
      "content": "The final answer...",
      "reasoning": "[Turn 1]\nThinking: ...\n\nTool calls:\n  web_search(query=\"...\")\n  > ..."
    },
    "finish_reason": "stop"
  }]
}
```

The `reasoning` field is omitted when the model doesn't provide reasoning content and no tools are used.

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

- `--server-port PORT` — start HTTP server on given port (server mode)
- `ENDPOINT` — complete model URL (default: `http://localhost:8235/v1/chat/completions`). When its URL path ends with `/responses` (with an optional trailing slash), capelin-go uses the official OpenAI Responses API schema; all other paths use Chat Completions.
- `MODEL` — model ID (default: `gpt-5-mini`)
- `TOKEN` — optional API token
- `REASONING_EFFORT` — passed through to the model backend; set to `none` or `nil` to omit the field entirely from the request
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
- `SUBAGENT_REASONING_EFFORT` — reasoning effort for subagents (default: inherits `REASONING_EFFORT`; set to `none` or `nil` to omit; overridden by `--subagent-reasoning-effort`)

## Config file

On first run capelin-go creates `~/.local/capelin-go/config.ini` with default values. If the file already exists, any keys added in newer versions are automatically appended (existing values are never changed).

```ini
# capelin-go configuration
# Edit this file to set persistent defaults.
# Priority: CLI flags > environment variables > this file > built-in defaults.

ENDPOINT = http://localhost:8235/v1/chat/completions
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

# Subagent model and reasoning effort (leave blank to inherit root MODEL and REASONING_EFFORT;
# set reasoning effort to none or nil to omit it from requests)
# env vars: SUBAGENT_MODEL, SUBAGENT_REASONING_EFFORT; also settable via CLI flags
SUBAGENT_MODEL =
SUBAGENT_REASONING_EFFORT =
```

Edit that file to set your preferred model, server URL, or other defaults without needing environment variables every time. Environment variables and CLI flags still take priority over config file values.

### Responses API mode

Set `ENDPOINT` to an OpenAI Responses endpoint such as:

```ini
ENDPOINT = https://api.openai.com/v1/responses
```

When the endpoint path ends with `/responses`, capelin-go sends the official Responses API format, including `input`, flat function tools, and `reasoning.effort`. Function calls are continued with matching `function_call_output` items, and requests are stateless: capelin-go does not use `previous_response_id`.

Endpoints that do not end with `/responses` continue to use Chat Completions. Provider-specific Responses variants and streaming are not supported.
