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
- **Name collisions:** a name always holds the latest point of the work. `baton save name` (also `push`, `resume`, `import … as name`) from a different session moves the session saved under it to `name.<date>` (for example `my_session.2026-11-09_14-30`, the time it was saved) and takes the name. Older versions have no list numbers: `baton list --all` shows them, `baton load <version name>` loads one, and only the 3 newest are kept. `baton rm name` gives the name back to the newest older version. Only `baton import` without `as` (the name is the agent's own title, which unrelated sessions often share) uses suffixes: `name-2`, `name-3`, …

## Snapshot format and versions

A saved snapshot (`~/.config/baton/saved/<name>.baton.json`) is JSON. It records `format` (the snapshot format, now 1) and `saved_by` (the baton version that wrote it). New optional fields keep the format: older batons ignore fields they don't know. The format goes up only when an existing field changes meaning; a baton that meets a newer format asks for `baton update` instead of misreading the snapshot. Remote bundles carry the same fields.

## Links between agent sessions and names (`baton resume`)

`~/.config/baton/links.json` records which agent session holds a point of which name, and how many prompts the user had typed in it at that moment:

- `baton save` links the saved session to its name;
- `baton load`, `baton resume`, and `baton import` link the session they load into (the one running baton) to the loaded name.

`baton resume name` compares the saved copy with every agent session linked to the name or named so in the agent. A linked session counts as a newer point if the number of typed prompts changed since it was linked and it was written after the saved copy. The newest point wins:

- the saved copy: it is loaded;
- a session of the agent running baton (or baton runs in a terminal): baton prints the agent's native resume command, because a native resume loses nothing: `claude --resume <id>` (or `/resume <id>` inside Claude Code), `codex resume <id>`, `agy --conversation <id>`, `kimi --session session_<id>`, `opencode --session ses_<id>`, `agent --resume <id>` (Cursor), `pi --session <id>`;
- a session of another agent: it is imported under the name (the old copy becomes an older version) and loaded.

`--load` always loads into the current conversation. The session running baton is found by the ID its agent exports (`CLAUDE_CODE_SESSION_ID`, `CODEX_THREAD_ID`, `PI_SESSION_ID`, `CURSOR_CONVERSATION_ID`), otherwise as the newest session of the calling agent in the current folder.

**The session's folder.** `load`, `resume`, and `import` compare the session's folder with the folder baton runs in, after resolving symlinks. Only the same folder counts: a subfolder or a parent folder changes what relative paths, `go test ./...`, and project rule files (`CLAUDE.md`, `AGENTS.md`) refer to, and Claude Code resumes only sessions of the folder it runs in. If the session's folder exists on this machine and differs, baton refuses to load it (exit code 1), prints `cd <folder> && <agent>` and the command to rerun there, and accepts `--force` to load it here anyway. A folder that does not exist here (a session from another computer, a removed worktree) does not stop loading; neither does `baton load <URL>`, which restores files into the current folder. Native resume hints in another folder are terminal commands that start there (`cd <folder> && claude --resume <id>`).

**Naming the session in the agent.** After a load, baton names the session it loaded into, so the agent's own resume menu shows the name instead of an automatic title. It writes the record the agent itself writes on rename, only where logs are append-only: a `custom-title` row in the Claude Code transcript (a name the user set with `/rename` is kept) and a `thread_name` row in Codex's `session_index.jsonl`. Other agents keep their own titles for now.

**`baton save` without a name** uses the name the session is linked to, then a name the user gave it in the agent (Claude Code `/rename`, Pi, Kimi), then the folder name.

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

### Repository check

`baton push` records the checkout the session worked in: the version-control system (git, Mercurial, Fossil, or Subversion), the remote URL without credentials, the branch, the revision, and the session's subfolder inside the checkout. On the receiving computer `baton load <URL>` compares them with the current folder and only warns, with the commands that fix it: not a checkout of that repository (`git clone …`, `hg clone …`, `fossil open <URL>`, `svn checkout …`), another repository, another subfolder, another branch (`git switch …`), a revision that is missing (`git fetch`, or push it on the source computer) or not checked out. Remote URLs are compared in one form, so `git@github.com:u/r.git` and `https://github.com/u/r` are the same repository.

`baton push` itself warns about git commits that are not on the server (a branch without an upstream, or commits ahead of it): the bundle carries uncommitted changes, not commits. Pushing them after `baton push` is enough, since the bundle records the revision. Mercurial and Fossil would need the network to tell; the receiver's check covers them.

## Updates

Once every 24 hours, a normal `baton` invocation spawns a detached `baton update --check-quiet`. It fetches [`latest.txt`](../latest.txt) with a 3-second timeout and stays silent when offline or up to date. When a newer version exists, baton prints a one-line notice. With `BATON_AUTO_UPDATE=1` it upgrades automatically; `BATON_NO_AUTO_UPDATE=1` disables the check.

`baton update` rebuilds from source when Go is installed. Otherwise it downloads the release binary for your OS from GitHub Releases, verifies its SHA-256 checksum, and replaces the executable. The new version then refreshes `SKILL.md` and `/baton` in all agents.

Versions 0.3.4 and older did not replace `~/.local/bin/baton` when Go was installed; reinstall once following [INSTALL.md](../INSTALL.md).
