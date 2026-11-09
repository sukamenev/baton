# How baton Works

Implementation details and session formats. For installation and everyday use, see the [README](../README.md).

## Handoff pipeline

`baton` reads the raw session files that the previous agent stored on disk. It finds the latest compaction checkpoint (`/compact` or auto-compaction) and extracts every modified file and shell command since turn 1. Then it unpacks the full transcript into a private per-user temporary folder and prints a structured handoff summary for the next agent. The project directory and `git status` stay clean.

```text
┌──────────────────────┐       Raw Disk Logs (.jsonl / .pb / .db)      ┌──────────────────────┐
│  Claude / Codex /    │ ────────────────────────────────────────────► │        baton         │
│ AGY / Pi / OpenCode  │   1. Locate latest /compact checkpoint        │ (One-Shot Go CLI +   │
│  / Cursor / Kimi     │   2. Unpack full log -> /tmp/baton-<uid>/     │      SKILL.md)       │
│  (Quota Exhausted)   │   3. Print compact summary + file manifest    └──────────┬───────────┘
└──────────────────────┘                                                          │
                                                                                  ▼
                                                                       ┌──────────────────────┐
                                                                       │   Next CLI Agent     │
                                                                       │  Resumes on exact    │
                                                                       │  line of code        │
                                                                       └──────────────────────┘
```

- **Active context:** the latest compaction summary plus every turn after it. `baton load` prints it in full. If it is too large for one tool output, `baton load` splits it into `active_context_partN.md` files, which the skill tells the agent to read one by one.
- **Full transcript:** `full_dialogue.md`, including turns before the compaction and raw tool outputs. `baton turn <session_id> <turn_id>` prints any single turn untruncated.
- **Temporary folder:** `/tmp/baton-<uid>/handoff/<session_id>/` on Linux and macOS, `%TEMP%\baton\handoff\<session_id>\` on Windows.
- **Saved store:** `~/.config/baton/saved/*.baton.json` (`$BATON_CONFIG_DIR`). A snapshot keeps only the active context, so loading is instant.

Unlike always-on MCP servers that stay resident and add tool schemas to every conversation, baton is a skill plus a one-shot CLI. It runs no background daemons and costs no tokens while idle; it runs only when you hand off a session.

## Session storage formats

| Agent | Session location (Linux / macOS / Windows) | Format | Compaction |
|---|---|---|---|
| **Claude Code** (`claude`) | `~/.claude/projects/<slug>/<uuid>.jsonl` (`$CLAUDE_CONFIG_DIR`) | JSONL (`text`, `thinking`, `tool_use`, `tool_result`, `custom-title`) | `type: "summary"` or `isCompactSummary` |
| **Codex CLI** (`codex`) | `~/.codex/sessions/YYYY/MM/DD/rollout-*.jsonl` & `session_index.jsonl` (`$CODEX_HOME`) | JSONL (Responses API & shell events) | `type: "compaction"` or `<context_summary>` |
| **Antigravity CLI** (`agy`) | `~/.gemini/antigravity-cli/brain/<uuid>/.../transcript.jsonl` & `conversations/*.pb` | JSONL & schema-less Protobuf wire format | `CHECKPOINT` / embedded summary block |
| **Pi** (`pi`) | `~/.pi/agent/sessions/--<cwd-slug>--/<ts>_<id>.jsonl` (`$PI_CODING_AGENT_DIR`) | v3 Tree JSONL (`id`, `parentId`, `session_info`, `bashExecution`) | `type: "compaction"` (`firstKeptEntryId`, `details.modifiedFiles`) |
| **OpenCode** (`opencode`) | `~/.local/share/opencode/opencode.db` on every OS (`$XDG_DATA_HOME`) | SQLite (`session_v2` + `session_message` in OpenCode 2.x; `session`/`message`/`part` in 1.x), read via the `sqlite3` command | `compaction` message with `summary` (manual and auto) |
| **Cursor** (`cursor`) | `~/.cursor/projects/<slug>/agent-transcripts/<uuid>/<uuid>.jsonl` (IDE and `agent` CLI). Title and folder: `~/.cursor/chats/<workspace>/<uuid>/meta.json` | JSONL (`role` + `message.content` `text` / `tool_use`) | `compaction/segment_*.md` beside the JSONL (summary + verbatim turns); the JSONL is the post-compaction tail |
| **Kimi Code** (`kimi`) | `~/.kimi-code/sessions/<workspaceId>/session_<id>/agents/main/wire.jsonl` (CLI and Desktop; `$KIMI_CODE_HOME`). Metadata: `state.json`, `session_index.jsonl` | v2 wire JSONL (`agent.message.appended` history entries) | `context.apply_compaction` (string, context-string, or legacy message summary); `context.clear` |

### Cursor

baton reads the JSONL layout above and the optional Markdown compaction segments; older or other storage layouts are not parsed. Commands and modified files come from JSONL tool calls. Older Markdown turns remain in the full transcript.

### Kimi Code (experimental)

- baton imports the main agent's completed text, thinking, tool calls, and tool results from the current v2 layout. It never modifies Kimi files.
- It treats injected messages the same way Kimi's own conversation view does:
  - injected reminders, retries, and system triggers are skipped;
  - task notifications, shell commands, and skill bodies stay in the transcript but are never taken for the user's prompt.
- Not imported: streaming turns not yet appended, subagent wires (`agents/agent-N/`), and media content.
- The active context is the latest compaction summary (or `/clear`) plus the completed messages after it. The user messages that Kimi keeps verbatim across a compaction are in the full transcript.

## Telling user prompts from injected messages

The last-prompt line in `baton list` and `baton import` shows what the user typed, not command output or injected text. Where an agent marks injected messages structurally, baton relies on that marker:

| Agent | Marker |
|---|---|
| Claude Code | `isMeta: true` (`/context` output, skill bodies, notices) |
| Antigravity CLI | prompts are `USER_INPUT` with `<USER_REQUEST>`; injected text is `SYSTEM_MESSAGE` |
| Kimi Code | entry `meta.origin.kind`; text parts with `meta.source` |
| Others | injected blocks recognized by their tags (`<environment_context>`, `<user_shell_command>`, `# AGENTS.md instructions`, …) |

Bare `baton …` commands are skipped as well. If only commands follow the latest compaction, the list shows the last prompt before it. Saved snapshots store this prompt at save time, because they keep only the active context.

## Lists, sizes, and IDs

- **Size:** the active context that `baton load` would hand over, measured in baton's own normalized text. It is comparable across agents and survives transfers between agents and machines.
- **Import cache:** `baton import` has to parse each raw log to measure it, so results are cached in `/tmp/baton-<uid>/cache/` and an unchanged log is not parsed again. Sessions without any messages (the agent was opened and closed) and `baton load` stub sessions are hidden.
- **ID:** the first 8 characters of the agent's own session ID; the same in `baton list` and `baton import`. List numbers shift as new sessions appear, IDs don't, so scripts should use `baton load <id>` and `baton import <id>`.
- **Name collisions:** `baton save name` for a different session with an existing name saves it as `name-2`, `name-3`, and so on.

## Which session `baton save` picks

`baton save` saves the session of the agent that called it, in the current folder.

- **Environment variables:** Claude Code (`CLAUDECODE`), Codex (`CODEX_THREAD_ID`), Pi (`PI_SESSION_ID`), OpenCode (`OPENCODE`), Antigravity (`ANTIGRAVITY_CLI`), and Cursor (`CURSOR_CONVERSATION_ID`) are recognized this way.
- **Parent processes (Linux):** if no variable is set, baton walks up the parent processes. Shells running a command string (`sh -c …`) are skipped, because their command line contains the project path, not the agent.
- **Kimi Code:** Kimi passes no marker to the commands it runs, so it is recognized only by its process. On macOS and Windows use `baton import` instead.

## Skill installation

`baton install` (and every `baton update`) writes `SKILL.md` into:

`~/.claude/skills/baton/`, `~/.codex/skills/baton/`, `~/.gemini/config/skills/baton/`, `~/.pi/agent/skills/baton/`, `~/.config/opencode/skills/baton/`, `~/.cursor/skills/baton/`, and `$KIMI_CODE_HOME/skills/baton/` (default `~/.kimi-code/skills/baton/`).

It also installs the `/baton` slash command for Claude Code (`~/.claude/commands/baton.md`) and a Codex rule that allows running `baton` (`~/.codex/rules/baton.rules`). In Cursor the skill can also be invoked from the [`/` menu](https://cursor.com/docs/skills).

## Encrypted transfer (`baton push`)

`baton push` packs a single in-memory `tar.gz` stream (`archive/tar` + `compress/gzip`) and encrypts it client-side with AES-256-GCM. The stream contains:

- the full session transcript and compaction state;
- uncommitted and untracked files modified during the session (from the agent's tool history plus Git, Fossil, or SVN status);
- project-level rule files (`CLAUDE.md`, `AGENTS.md`, `GEMINI.md`, `.cursorrules`, `.agents/rules/`);
- a snapshot of global user instructions (`~/.claude/CLAUDE.md`, `~/.gemini/GEMINI.md`).

The ciphertext is uploaded through a fallback chain: `paste.rs` → `dpaste.org` → `termbin.com:9999`, or a local synced folder (Dropbox, Google Drive, Syncthing; `$BATON_STORAGE_PATH`). The 256-bit key travels only in the URL fragment after `#` and never reaches the paste server.

`baton load <URL>` then does the following:

- downloads and decrypts the bundle;
- restores the modified files, keeping a `.baton.bak` copy of any local file that differs;
- puts the source machine's global rules into `handoff/<session_id>/global_rules/`;
- saves the session into the saved store, so later loads work offline.

## Updates

Once every 24 hours, a normal `baton` invocation spawns a detached `baton update --check-quiet`. It fetches [`latest.txt`](../latest.txt) with a 3-second timeout and stays silent when offline or up to date. When a newer version exists, baton prints a one-line notice. With `BATON_AUTO_UPDATE=1` it upgrades automatically; `BATON_NO_AUTO_UPDATE=1` disables the check.

`baton update` rebuilds from source when Go is installed. Otherwise it downloads the release binary for your OS from GitHub Releases, verifies its SHA-256 checksum, and replaces the executable. The new version then refreshes `SKILL.md` and `/baton` in all agents.

Versions 0.3.4 and older did not replace `~/.local/bin/baton` when Go was installed; reinstall once following [INSTALL.md](../INSTALL.md).
