# Architecture and source placement

Capelin is organized by capability. The command entrypoints are thin, the
application package is the composition root, and lower-level modules expose
small seams with no upward imports.

## Runtime shape

```text
main.go / cmd/capelin-go
          |
      internal/cli
          |
      internal/app  (composition and workflows)
       /  |   |   |   |   |   |   \
 agent server subagents tools providers sessions interactive
   |      |      |      |      |        |
 contracts contracts policy contracts contracts contracts
```

`internal/app` constructs the provider adapter, agent engine, tool dispatcher,
subagent manager, output sink, session store, interactive runner, and server
handler. It supplies application-owned execution callbacks through the server
and subagent seams. It does not own the concrete server delivery, data store,
or subagent lifecycle implementations.

## Ownership

| Package or path | Owns | Does not own |
| --- | --- | --- |
| `main.go`, `cmd/capelin-go/main.go` | Process startup and release-version variables | Parsing, workflows, providers, tools, sessions, or HTTP |
| `internal/cli` | Process dispatch, signals, exit status, and startup errors | Configuration precedence or runtime behavior |
| `internal/config` | CLI/environment/config-file parsing, defaults, precedence, and validation | Turns, providers, tools, or HTTP delivery |
| `internal/app` | Composition root, one-shot/interactive workflows, application adapters, and request-scoped execution construction | Concrete tools, provider wire formats, server delivery, data storage, and subagent lifecycle |
| `internal/agent` | Protocol-neutral turns, model retry, iteration limits, continuation, output events, tool batch scheduling, deadlines, timeout retry, and result ordering | Provider payloads, HTTP, concrete tools, and safety implementation |
| `internal/providers` | Chat Completions and Responses transport, wire DTOs, decoding, continuation state, and retry markers | Common turn policy and tool execution |
| `internal/tools` | Tool catalog, schemas, argument decoding, web/filesystem/process/skill implementations, and dispatcher hooks | Turn policy, provider formats, output sinks, and subagent state |
| `internal/subagents` | Child-agent lifecycle, visibility, limits, inherited permissions, scheduling, cancellation, timeouts, and result aggregation | Provider construction, tool schemas, and application workflows |
| `internal/server` | HTTP intake, endpoint resolution, authorization, request limits, sync/async delivery, admission, cancellation, polling storage, panic recovery, response formatting, and CORS | Providers, agents, tools, or application composition |
| `internal/sessions` | Durable snapshots, legacy decoding, selector resolution, validation, and atomic replacement | Interactive commands and provider state |
| `internal/interactive` | Terminal input normalization, paste handling, and readline/fallback mechanics | Slash commands and model turns |
| `internal/output` | Runtime output sinks and final-only composition | Choosing a sink, HTTP responses, and turn policy |
| `internal/policy` | Tool permission sets, safe/opt-in rules, path confinement, dangerous-command checks, and child inheritance | Tool execution and orchestration |
| `internal/skills` | Skill discovery, parsing, precedence, and bounded metadata/content | Skill execution permissions and model orchestration |
| `internal/contracts` | Small protocol-neutral values and interfaces shared by modules | Provider-specific wire models and application services |
| `internal/types` | Compatibility aliases for the former shared-value path | New behavior or new shared models |

## Dependency direction and placement rules

1. Depend on seams, not implementations. The agent consumes normalized provider,
   tool-runner, tool-catalog, and output-sink interfaces.
2. Keep protocol translation in `internal/providers`. Do not add provider wire
   DTOs to `internal/contracts` for convenience.
3. Keep common execution policy in `internal/agent`; concrete dispatch and
   safety checks remain in `internal/tools` and `internal/policy`.
4. Keep `internal/server` delivery-neutral. Its executor receives an
   `ExecutionRequest` and returns an `ExecutionResult`; it never constructs a
   provider, agent, or runtime.
5. Keep `internal/subagents` delivery- and provider-neutral. Its runner receives
   a normalized runtime/session view; the application supplies the runner.
6. Lower-level packages must not import `internal/app`, `internal/cli`, or a
   command entrypoint. Pass narrow values or callbacks instead.
7. Keep persistence below workflows: snapshot serialization belongs in
   `internal/sessions`, while session commands remain in `internal/app`.
8. Use `internal/types` only for compatibility. New production code imports
   `internal/contracts` directly.
9. Keep entrypoints thin and preserve external CLI, HTTP, configuration, tool,
   snapshot, and build contracts at explicit adapters.

## Important seams

### Agent and tools

`internal/app` composes `internal/providers`, `internal/tools`,
`internal/output`, and `internal/agent`. The agent engine owns the normalized
turn loop and common tool execution policy. The tools dispatcher owns schemas,
argument decoding, permission checks, and concrete calls; application hooks
handle only workflows such as todos and subagent invocation.

### Server delivery

`internal/server.NormalizeRequest` is shared by sync and async delivery. It
resolves encoded, hex, and query endpoint forms, applies authorization and
body limits, selects model/reasoning defaults, validates messages, and applies
the server tool policy. `internal/server.Delivery` owns admission, timeout,
panic recovery, result storage, polling, and response formatting. The
application supplies `server.Executor` at composition time.

### Subagent orchestration

`internal/subagents.Manager` owns child session state and resource policy. Its
`Runner` seam receives only a normalized `Runtime` and `Session`; the
application adapter creates the normal provider/agent/tool execution context.
Child permission inheritance is delegated to `internal/policy`, so subagents
cannot escalate tool access.

### Sessions and interactive behavior

`internal/interactive` turns terminal bytes into normalized lines. The
application interprets slash commands, runs turns, and connects session state
to `internal/sessions`, which owns atomic durable snapshot replacement and
legacy optional-field decoding.

## Validation

Inspect dependencies with:

```bash
go list -f '{{.ImportPath}}: {{join .Imports " "}}' ./...
```

The list must remain acyclic, and `internal/server` and `internal/subagents`
must not import `internal/app` or `internal/cli`. For implementation changes,
run:

```bash
go test ./...
go vet ./...
go test -race ./...
go build .
go build ./cmd/capelin-go
BUILD_DIR="$(mktemp -d)" make dist
git diff --check
```
