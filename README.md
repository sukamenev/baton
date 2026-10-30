# baton — Lossless Session Handoff Across CLI Coding Agents

[![Go Standard Library Only](https://img.shields.io/badge/Go-zero%20dependencies-00ADD8?logo=go)](go.mod)
[![Agent Skills](https://img.shields.io/badge/Agent-SKILL.md%20%2B%20CLI-purple)](skills/baton/SKILL.md)
[![Platform](https://img.shields.io/badge/Platform-Linux%20%7C%20macOS%20%7C%20Windows-informational)](parsers.go)
[![License: MIT](https://img.shields.io/badge/License-MIT-green.svg)](LICENSE)

![baton — Lossless Session Handoff Across CLI Coding Agents](images/baton.png)

`baton` is a zero-dependency Go CLI tool and `SKILL.md` agent skill for **Linux, macOS, and Windows** that passes session context between **Claude Code (`claude`)**, **OpenAI Codex CLI (`codex`)**, **Antigravity CLI (`agy`)**, **Pi (`pi`)**, and **Hermes (`hermes`)** without losing history when 5-hour quota limits run out — or transfers an active local session (along with uncommitted files and rule configs) to another laptop with client-side **AES-256-GCM** encryption.

Unlike always-on MCP servers that stay resident in memory and inject tool schemas into every conversation's system prompt, `baton` uses **progressive skill loading (`SKILL.md`) + one-shot CLI execution**: zero background daemons, zero idle token overhead, and 20ms execution only when you actually hand off a session.

---

## Why `baton`?

The common advice for switching agents is to ask the current model to write a `HANDOFF.md` file before quitting. In practice, that approach is suboptimal for three reasons:

1. **Hard quota walls.** When your 5-hour subscription window hits zero mid-debug, the agent refuses further prompts and cannot generate a summary file.
2. **Lossy LLM summarization.** Input context windows reach 200k–1M tokens, whereas max output tokens are capped at 8k–32k. That output ceiling forces aggressive lossy compression: exact compiler flags, line numbers, rejected hypotheses, and subtle edits are dropped, degrading the quality of work in the next agent.
3. **5x output token cost.** Output tokens typically cost 5 times more than input tokens and burn through remaining subscription quota 5 times faster.

`baton` reads the raw session files stored on disk by the previous agent, locates the latest `/compact` checkpoint, extracts the complete list of modified files and shell commands since turn 1, unpacks the full lossless transcript into the OS temporary directory (`/tmp/baton-handoff/<session_id>/` on Linux/macOS or `%TEMP%\baton-handoff\<session_id>\` on Windows, keeping your project directory and `git status` clean), and prints a structured handoff summary for the next agent.

```text
┌──────────────────────┐       Raw Disk Logs (.jsonl / .pb / .db)      ┌──────────────────────┐
│  Claude / Codex /    │ ────────────────────────────────────────────► │        baton         │
│  AGY / Pi / Hermes   │   1. Locate latest /compact checkpoint        │ (One-Shot Go CLI +   │
│  (Quota Exhausted)   │   2. Unpack full log -> /tmp/baton-handoff/   │      SKILL.md)       │
└──────────────────────┘   3. Print compact summary + file manifest    └──────────┬───────────┘
                                                                                  │
                                                                                  ▼
                                                                       ┌──────────────────────┐
                                                                       │   Next CLI Agent     │
                                                                       │  Resumes on exact    │
                                                                       │  line of code        │
                                                                       └──────────────────────┘
```

---

## Supported Agents & Cross-Platform Storage Schemas

| Agent | Session Location (Linux / macOS / Windows) | Format | Compaction & Lineage |
|---|---|---|---|
| **Claude Code** (`claude`) | `~/.claude/projects/<slug>/<uuid>.jsonl` (`$CLAUDE_CONFIG_DIR`) | JSONL (`text`, `thinking`, `tool_use`, `tool_result`, `custom-title`) | `type: "summary"` or `isCompactSummary` |
| **Codex CLI** (`codex`) | `~/.codex/sessions/YYYY/MM/DD/rollout-*.jsonl` & `session_index.jsonl` (`$CODEX_HOME`) | JSONL (Responses API & shell events) | `type: "compaction"` or `<context_summary>` |
| **Antigravity CLI** (`agy`) | `~/.gemini/antigravity-cli/brain/<uuid>/.../transcript.jsonl` & `conversations/*.pb` | JSONL & schema-less Protobuf wire format | `CHECKPOINT` / embedded summary block |
| **Pi** (`pi`) | `~/.pi/agent/sessions/--<cwd-slug>--/<ts>_<id>.jsonl` (`$PI_CODING_AGENT_DIR`) | v3 Tree JSONL (`id`, `parentId`, `session_info`, `bashExecution`) | `type: "compaction"` (`firstKeptEntryId`, `details.modifiedFiles`) |
| **Hermes** (`hermes`) | `~/.hermes/state.db` (`%LOCALAPPDATA%\hermes\state.db` on Windows, `$HERMES_HOME`) | SQLite v32 (`sessions` + `messages` tables) & JSON trajectories | `parent_session_id` compression lineage & `_compressed_summary=1` |

> **Using DeepSeek, GLM, Grok, or Ollama?** Most developers run those models inside **Pi** or **Hermes**. Because `baton` natively supports both `pi` and `hermes`, you can hand off a session from a $20 Claude/Codex subscription directly to DeepSeek V3/R1, GLM, or Grok.

---

## Quick Install (AI-Assisted From Source)

`baton` is written in pure Go using **only the standard library** (`go.mod` has zero external dependencies). Ask your current agent to audit, build, and configure `baton` for all agents on your machine in one prompt:

```text
Read https://raw.githubusercontent.com/sukamenev/baton/main/INSTALL.md and install baton from source for all CLI agents installed on my system (claude, codex, agy, pi, hermes). Verify that the Go compiler is available, build the binary into ~/.local/bin/baton, run baton install to install the SKILL.md skill and the /baton slash command, and then verify the installation by running baton status.
```

<details>
<summary><b>Russian Prompt (Промпт на русском языке)</b></summary>

```text
Прочитай https://raw.githubusercontent.com/sukamenev/baton/main/INSTALL.md и установи инструмент baton из исходного кода для всех найденных у меня консольных агентов (claude, codex, agy, pi, hermes). Проверь наличие компилятора Go, собери бинарник в ~/.local/bin/baton, выполни baton install для установки навыка SKILL.md и слэш-команды /baton, а затем проверь работоспособность вызовом baton status.
```
</details>

### Manual Build & Install (3 Commands)

Ensure Go 1.22+ is installed (`dev-lang/go` on Calculate/Gentoo, `go` on Arch/openSUSE, `golang-go` on Debian/Ubuntu, `golang` on Fedora):

```bash
git clone https://github.com/sukamenev/baton.git /tmp/baton
cd /tmp/baton && go build -ldflags="-s -w" -o ~/.local/bin/baton .
~/.local/bin/baton install
```

### Safe Updates (`latest.txt` Check + Two-Stage `baton update`)

Once every 24 hours on normal CLI invocations, `baton` spawns a detached `baton update --check-quiet` process that fetches [`latest.txt`](latest.txt) with a 3-second timeout. If offline or up to date, it **exits immediately with zero output and zero errors**. If a newer version is available, `baton` prints a one-line notice (`Run 'baton update' to upgrade`, or upgrades automatically if you set `BATON_AUTO_UPDATE=1`).

Running `baton update` upgrades baton in place:
1. **If Go is installed:** rebuilds baton from source.
2. **Otherwise:** downloads the release binary for your OS from GitHub Releases, verifies its SHA-256 checksum, and replaces the executable.

The new version then refreshes `SKILL.md` and `/baton` in all installed agents.

> **Upgrading from v0.3.4 or older:** if Go is installed, the old `baton update` did not replace `~/.local/bin/baton`. Reinstall once following [INSTALL.md](INSTALL.md).

---

## Usage

### 1. Recommended Workflow: `baton save` & `baton load`

Before switching agents, save your active session into baton's store under a memorable name (`baton save my_session`; without a name, the current folder name is used):

```text
Save this session as my_session
```

`baton save` outputs a ready-to-paste prompt that works even in an agent where `baton` has never been installed yet:

```text
Load session my_session using baton (if baton is not installed, install it from https://github.com/sukamenev/baton following INSTALL.md)
```

When `claude` hits its 5-hour quota limit, open `codex`, `agy`, `pi`, or `hermes` and ask in plain words (you don't even need to specify which agent had the session):

```text
Continue session my_session
```

Or to grab the most recently saved session:

```text
Continue the latest session
```

Or via the slash command:

```text
/baton load my_session
```

> **Load into a clean context.** If the agent already has a conversation going, start a new one first (`/clear` in Claude Code and agy, `/new` in Codex), then ask to load the session. Otherwise the loaded session lands on top of the old conversation: two tasks get mixed and the context window fills up faster. A freshly launched agent needs nothing extra. The same applies to `baton import`.

**Forgot the session name?** Ask the agent *"What saved sessions are there?"* (or run `baton list`). It shows a numbered list of sessions saved into baton, newest first, with save time, agent, name, session ID, folder, and the last prompt, so you can simply say *"Load session #1"* (or run `/baton load 1`):

```text
/baton load 1
```

**The agent hit its limit before you ran `baton save`?** Ask the next agent *"Take one of the existing sessions"* (or run `baton import`). Without arguments, `baton import` only shows a list and never imports anything by itself. Unlike `baton load`, it lists the 15 most recent sessions of all five agents on disk (current folder first), each with the last prompt, i.e. where the work stopped. Pick one by number, name, or session ID:

```text
baton import 1
baton import my_session
baton import 726d401d
baton import 7 as my_session
```

The chosen session is saved into baton and loaded right away; afterwards it appears in `baton list`. If you renamed the session in the agent itself, that name is shown in the list and used when saving.

Each list row looks like **#1** 2026-10-31 00:37 [claude] **my_session** · id 726d401d, with the folder and the last prompt below it. The ID is the agent's own session ID (first 8 characters of the UUID; the full ID for Hermes) and stays the same in `baton list` and `baton import`. Numbers shift as new sessions appear, IDs don't, so scripts should use `baton load <id>` and `baton import <id>`. To import a session without loading it, add `--no-load`: `baton import 726d401d as my_session --no-load`.

`baton save` always saves the current session; to save any other session under a name, use `baton import <#|id> as <name>`.

### 2. Encrypted Cross-Machine Transfer (Desktop -> Laptop)

Need to leave your desktop and continue on your laptop without committing broken WIP code to Git (or working in a folder with Fossil, SVN, or no VCS)?

`baton` packs four components into a single in-memory `tar.gz` stream (`archive/tar` + `compress/gzip`, built directly into the binary for Windows, macOS, and Linux) and encrypts it client-side with `AES-256-GCM`:
- Full session transcript and compaction state;
- All uncommitted/untracked workspace files modified during the session (detected via agent tool history + Git/Fossil/SVN status);
- Project-level unversioned rule files (`CLAUDE.md`, `AGENTS.md`, `GEMINI.md`, `.cursorrules`, `.agents/rules/`);
- A snapshot of your global user instructions (`~/.claude/CLAUDE.md`, `~/.gemini/GEMINI.md`).

In your active agent, type:

```text
/baton push
```

`baton` encrypts the archive locally and uploads the ciphertext through an automatic zero-config fallback chain (`paste.rs` -> `dpaste.org` -> `termbin.com:9999`, or a local synced folder like Dropbox/Google Drive/Syncthing), returning a self-contained transfer token and a self-bootstrapping prompt:

```text
Encrypted session URL / token:
  https://paste.rs/x9K2m#8f4a2c91e7b03d...

To continue in another agent or on a machine where baton is not installed yet, paste this prompt:
  Load session https://paste.rs/x9K2m#8f4a2c91e7b03d... using baton (if baton is not installed, install it from https://github.com/sukamenev/baton following INSTALL.md)
or if baton is already installed, run:
  /baton load https://paste.rs/x9K2m#8f4a2c91e7b03d...
```

> **Zero-Knowledge Transport:** The 256-bit hex key after `#` stays in the URL fragment and is never transmitted to the paste server.

On your laptop, open the project folder in any agent and run the exact same `load` command with the URL:

```text
/baton load https://paste.rs/x9K2m#8f4a2c91e7b03d...
```

`baton` downloads and decrypts the bundle, restores modified files (creating `.baton.bak` backups if any local file differs), places global rules from the source machine into `/tmp/baton-handoff/<session_id>/global_rules/`, saves the downloaded session into Baton's saved sessions store (`~/.config/baton/saved/` so subsequent `baton load` or `baton list` calls work offline without re-downloading), and loads the full active session context.

> **Codex CLI Sandbox Note:** If Codex CLI's workspace sandbox blocks network access during a remote `baton load <URL>` and policy rejects permission escalation, run `!baton load "<URL>"` directly at the Codex prompt (with a leading exclamation mark `!`) to download and save the bundle into `~/.config/baton/saved/`, and then type `baton load`.

---

## CLI Commands Reference

| Command | Description |
|---|---|
| `baton save [name]` | Saves the current session into baton (`~/.config/baton/saved/`) under a name, the folder name by default, and prints a ready-to-paste prompt for the next agent. If another session already has that name, this one is saved as `name-2`. |
| `baton push [name]` | Saves the current session locally, packs (`tar.gz`), encrypts (`AES-256-GCM`), and uploads the session + uncommitted files + rules, returning an encrypted URL with `#key`. |
| `baton load [name\|#\|id\|URL\|token]` | With no arguments (`baton load`), always loads the most recently saved session from Baton's saved store (`~/.config/baton/saved/`); also loads by name (`baton load my_session`), number (`baton load 1`), session ID (`baton load 726d401d`), or remote URL/token (`baton load https://...#key`). |
| `baton list [agent] [query]` | Lists numbered saved Baton sessions (`#1`, `#2`, ...), newest first. |
| `baton import [#\|name\|id] [as name] [--no-load]` | Lists recent sessions of all agents (`#1`, `#2`, ...); with a number, name, or session ID, saves that session into baton (under the `as` name, if given) and loads it (`--no-load`: only saves). For sessions that were never saved. |
| `baton rm <name\|#>` | Deletes one saved session (exact name or number from `baton list`). The agent's own session log is not touched. |
| `baton clear` | Removes all locally saved Baton sessions (`~/.config/baton/saved/`) and temporary handoff caches (`/tmp/baton-handoff/`). |
| `baton turn <session_id> <turn_id>` | Prints the complete, untruncated output of any specific turn on demand. |
| `baton update` | Updates baton to the latest version and refreshes `SKILL.md` and `/baton` in all installed agents. |

---

## License

[MIT](LICENSE) © 2026 Sergey Kamenev ([sukamenev](https://github.com/sukamenev))
