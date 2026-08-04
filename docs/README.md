# Capelin User Documentation

Capelin (`capelin-go`) lets you give an AI assistant useful work to do from your terminal or through a local web service. It can answer questions, inspect a working folder, search the public web, read web pages, coordinate worker assistants, and—when you explicitly allow it—change files or run local commands.

It is designed to be safe by default. Reading and web tools are available for ordinary tasks. Tools that can change your files or run programs must be enabled separately.

## Choose a guide

- [Command-line guide](cli-guide.md) — run one task, hold a conversation, save answers, select skills, and configure the assistant.
- [Server and API guide](server-api.md) — use Capelin as a local OpenAI-compatible proxy, submit background jobs, and use the temporary data store.
- [Tools and safety](tools-and-safety.md) — understand what the assistant can do, what is disabled by default, and how worker assistants and skills work.
- [Architecture and source placement](architecture.md) — package ownership, dependency direction, seams, adapters, and validation rules.
- [Troubleshooting FAQ](troubleshooting.md) — fix common startup, model, tool, server, and async-job problems.

Completed feature specifications and their validation records are archived in
the repository's `.archive/` directory after implementation. The active
`.scratch/` directory is reserved for work that has not yet been archived.

## Quick start

Use the Capelin executable from your download or build it with `make build`. The examples below assume the executable is available as `./capelin-go`.

```bash
./capelin-go "summarize the files in this folder"
```

For a conversation with follow-up questions:

```bash
./capelin-go --interactive
```

Before the first request, Capelin creates a settings file at `~/.local/capelin-go/config.ini`. Set the model service URL and token there, or use the `ENDPOINT`, `MODEL`, and `TOKEN` environment settings. See the [command-line guide](cli-guide.md#settings-and-configuration).

## What can I do with Capelin?

You can ask it to:

- explain or summarize a project;
- research a topic using web search and public pages;
- inspect local files and report what they contain;
- ask several worker assistants to investigate different parts of a large task;
- apply a locally defined skill as task guidance;
- optionally create, edit, or append to files;
- optionally run an approved local program;
- expose the same assistant through a local HTTP endpoint.

The assistant decides when a tool is useful. You describe the outcome you want in ordinary language; the linked guides explain the available controls when you need more control.
