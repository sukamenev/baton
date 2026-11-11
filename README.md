# baton — Lossless Session Handoff Across CLI Coding Agents

[![Go Standard Library Only](https://img.shields.io/badge/Go-zero%20dependencies-00ADD8?logo=go)](go.mod)
[![Agent Skills](https://img.shields.io/badge/Agent-SKILL.md%20%2B%20CLI-purple)](skills/baton/SKILL.md)
[![Platform](https://img.shields.io/badge/Platform-Linux%20%7C%20macOS%20%7C%20Windows-informational)](parsers.go)
[![License: MIT](https://img.shields.io/badge/License-MIT-green.svg)](LICENSE)

![baton — Lossless Session Handoff Across CLI Coding Agents](images/baton.png)

`baton` moves a coding-agent session to another agent without losing history: for example, when Claude Code hits its 5-hour quota mid-task, you continue the same session in Codex. It can also move a session to another computer, together with uncommitted files, encrypted end to end.

baton is a small Go program plus an agent skill (`SKILL.md`) for **Linux, macOS, and Windows**. It has no dependencies, runs no background processes, and costs no tokens until you hand off a session.

## Supported agents

| Agent | Name in baton commands |
|---|---|
| Claude Code | `claude` |
| OpenAI Codex CLI | `codex` |
| Antigravity CLI | `agy` |
| Pi | `pi` |
| OpenCode | `opencode` |
| Cursor (IDE agent and `agent` CLI) | `cursor` |
| Kimi Code (CLI and Desktop), experimental | `kimi` |

Running DeepSeek, GLM, Grok, or local models? Run them inside Pi or OpenCode, and baton hands sessions to them from Claude or Codex.

## Why not just ask the agent for a summary?

The usual advice is to have the agent write a `HANDOFF.md` before you switch. That falls short in three ways:

1. **Hard quota walls.** When the quota runs out mid-task, the agent cannot write anything anymore.
2. **Lossy summaries.** A summary squeezes a 200k–1M token context into a few thousand output tokens. Exact flags, line numbers, rejected hypotheses, and small edits get lost.
3. **Cost.** Output tokens cost about 5 times more than input tokens and burn the remaining quota faster.

baton reads the session log that the agent already keeps on disk. It hands the next agent the latest `/compact` summary, every turn after it, and the list of modified files and commands. The full transcript stays available on request. See [How baton works](docs/INTERNALS.md) for details.

## Installation

Ask your current agent to install baton (it builds baton from source; Go is required):

```text
Read https://raw.githubusercontent.com/sukamenev/baton/main/INSTALL.md and install baton from source for all CLI agents installed on my system (claude, codex, agy, pi, opencode, cursor, kimi). Verify that the Go compiler is available, build the binary into ~/.local/bin/baton, run baton install to install the SKILL.md skill and the /baton slash command, and then verify the installation by running baton status.
```

Or install manually (Go 1.22+):

```bash
git clone https://github.com/sukamenev/baton.git /tmp/baton
cd /tmp/baton && go build -ldflags="-s -w" -o ~/.local/bin/baton .
~/.local/bin/baton install
```

baton checks for new versions once a day and tells you when one is out; `baton update` upgrades it.

## Usage

Talk to your agent in plain words and mention baton, or run `baton` commands directly. Requests that don't mention baton (or saving a session for another agent or computer) don't activate the skill.

### Hand a session to another agent

1. In the current agent, save the session:

   ```text
   Save this session as my_session using baton
   ```

   baton prints a ready-to-paste prompt for the next agent. It works even where baton is not installed yet.

2. Open the next agent in the same folder and ask:

   ```text
   Resume session my_session using baton
   ```

   In Claude Code you can also type `/baton resume my_session`.

> **Start in the session's folder.** If the session worked in another folder on this computer, `load`, `resume`, and `import` refuse to load it and print `cd <folder> && <agent>` to restart there: relative paths, commands, and project rules belong to that folder. Add `--force` to load it in the current folder anyway.

> **Start from a clean context.** If the agent already has a conversation going, start a new one first (`/clear` in Claude Code and agy, `/new` in Codex). Otherwise the loaded session lands on top of the old one.

### Forgot which agent you worked in last?

`baton resume my_session` finds the latest point of the session, wherever it is:

- the copy saved in baton, if nothing happened after it;
- an agent session that went on after it, even without `baton save`. If that session belongs to the agent you are in, baton tells you how to resume it natively there (for example `/resume <id>` in Claude Code), with no loss at all. Otherwise it imports that session and loads it.

A session loaded through baton keeps the name: Claude Code and Codex show it in their own `/resume` menus, and a later `baton save` without a name saves it under the same name. The session saved under that name before is kept as `my_session.2026-11-09_14-30` (`baton list --all` shows such older versions; the 3 newest are kept).

### Find a saved session

Ask *"What sessions are saved in baton?"* or run `baton list`. Each session is shown on one line like this, with its folder and your last prompt below:

**#1** 2026-10-31 00:37 [claude] **my_session** · 384 KB · id 726d401d

Then say *"Load session #1"*, or *"Load session 726d401d"* (numbers change as new sessions appear; IDs don't).

### The session was never saved

If the agent hit its limit before you saved, ask the next agent *"Use baton to list sessions available for import"* or run `baton import`. It lists the 15 most recent sessions of all agents, current folder first. Then import one by number, name, or ID; it is saved into baton and loaded:

```text
baton import 1
baton import 7 as my_session
```

> **Tip: see the list yourself.** In Claude Code, Codex CLI, OpenCode, agy, and the Cursor CLI, type `!baton list` or `!baton import`: the `!` prefix runs the command without the model. You see the list exactly as baton printed it, and the agent spends no tokens on it. Then tell the agent *"Load session #2"*.

### Move a session to another computer

In the current agent:

```text
/baton push
```

baton packs the session, the files changed during it (even uncommitted ones), and your rule files. It encrypts the bundle on your machine, uploads it, and prints a link like `https://paste.rs/x9K2m#8f4a…`. The key after `#` never leaves your machine. On the other computer, open the project folder in any agent and run:

```text
/baton load https://paste.rs/x9K2m#8f4a…
```

Local files that differ are kept as `.baton.bak`. baton also checks that the folder is a checkout of the same repository (git, Mercurial, Fossil, or Subversion) on the same branch and revision, and tells you how to fix it if not. `baton push` warns about commits that are not on the server yet: the bundle carries uncommitted changes, not commits.

> **Codex sandbox:** if Codex blocks the network, type `!baton load "<URL>"` at the Codex prompt, then `baton load`.

## Commands

| Command | Description |
|---|---|
| `baton resume <name\|#>` | Continue a session from its latest point in any agent (`--load` always loads it here). |
| `baton save [name]` | Save the current session (default name: the one it was loaded or saved as, else the folder name) and print a prompt for the next agent. In Codex, after a compaction on OpenAI's servers (its summary is encrypted), baton asks the agent for a recap and a rerun with `--summary <file>`; `baton push` waits for the recap before sending. |
| `baton load [name\|#\|id\|URL]` | Load a session saved in baton; without arguments, the most recently saved one. |
| `baton list [--all] [agent] [query]` | List saved sessions, newest first; `--all` adds older versions. |
| `baton import [#\|name\|id] [as name] [--no-load]` | List recent sessions of all agents, or save one into baton and load it. |
| `baton push [name]` | Encrypt and upload the session with changed files; prints a link for `baton load`. |
| `baton rm <name\|#>` | Delete a saved session. The agent's own log is not touched. |
| `baton clear` | Delete all saved sessions and temporary files. |
| `baton hist <session> [N \| from to \| --grep text]` | Show a session's turns: the user's prompts by turn number, turn N in full, a range, or the turns containing text. It reads the agent's own log while it is on this machine (also the turns before the latest compaction), else the snapshot. |
| `baton update` | Update baton and the installed skills. |

Session formats, size measurement, encryption, and other details: [docs/INTERNALS.md](docs/INTERNALS.md).

## License

[MIT](LICENSE) © 2026 Sergey Kamenev ([sukamenev](https://github.com/sukamenev))
