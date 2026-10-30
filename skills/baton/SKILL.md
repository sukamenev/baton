---
name: baton
description: >-
  Lossless session handoff across CLI coding agents (claude, codex, agy, pi, hermes)
  and client-side encrypted session + workspace transfer between machines. Activate
  this skill whenever the user asks in plain words to continue/load/resume a session
  (by name, from another agent, or the latest session), name a session, list recent
  sessions, send/push a session to another computer, or load a baton:... token.
---

# Baton Skill: Cross-Agent & Cross-Machine Session Handoff

When the user asks in plain language (Russian, English, or any language) to continue or transfer a session, run the corresponding `baton` command via your shell tool (Note for Codex CLI: run `baton` normally first; if a remote `baton load <URL>` or `baton push` fails due to sandbox network restrictions and permission escalation is rejected by policy, tell the user to run `!baton load "<URL>"` directly in the Codex prompt with a leading exclamation mark `!` — which downloads and saves the session into the local index — and then type `baton load`; or `!baton push` to upload):

| What the user says (examples) | Command to run |
|---|---|
| "Load/continue the latest session" / "Загрузи последнюю сессию" | `baton load` |
| "Load session auth-refactor" / "Загрузи сессию auth-refactor" | `baton load auth-refactor` |
| "Load session from claude" / "Загрузи сессию от claude" | `baton load claude` |
| "Load session auth-refactor from claude" | `baton load claude auth-refactor` |
| "Show recent sessions" / "Какие есть сессии?" (or user forgot the name) | `baton list` |
| "Load candidate #1" / "Загрузи первую сессию" | `baton load 1` |
| "Save/name this session" / "Сохрани текущую сессию" | `baton save` |
| "Save/name this session auth-refactor" / "Сохрани сессию auth-refactor" | `baton save auth-refactor` |
| "Push/send this session for my laptop" / "Отправь сессию на другой компьютер" | `baton push` |
| "Load remote session https://...#key or baton:..." / "Загрузи сессию по ссылке/токену" | `baton load <URL\|token>` |
| "Delete saved session auth-refactor" / "Удали сохранённую сессию auth-refactor" | `baton rm auth-refactor` (or `baton rm <#>` with the number from `baton list`) |
| "Clear/delete all saved sessions" / "Очисти/удали все сохранённые сессии" | `baton clear` |
| "The session was not saved (context/quota ran out), take one of the existing sessions" / "Импортируй сессию" / "Возьми сессию из существующих" | `baton import` |
| "Import session #2" / "Импортируй вторую сессию" / "Импортируй сессию auth-refactor" | `baton import 2` / `baton import auth-refactor` |

## Rules After Running `baton list`, `baton load`, or `baton import`

1. **If `baton` returned a numbered candidate list (`#1`, `#2`, ...):**
   - The user usually cannot see raw command output (CLI agents fold it), so your reply is the only place where they see the list. Reproduce **every** entry exactly as printed: number, agent, name, file, time, working directory, and preview. Do not summarize, regroup, merge, or collapse entries (never write things like "#7–#15: older sessions"), do not drop previews, and do not add your own commentary between entries.
   - Then ask the user which number or name to load/import, and wait for the answer. Do not pick a session yourself after `baton list` or `baton import`; only after a `baton load` fallback list, if #1 clearly matches the current project directory, you may run `baton load 1` directly.
   - If `baton load` reports that there are no saved sessions (or none of them is the one the user wants), run `baton import` to list raw sessions of all agents and let the user pick one. `baton import <#|name>` saves the chosen session into baton and loads it in one step.
2. **Check `Handoff Status` at the bottom of the `baton load` output:**
   - If it says `Handoff Status: PARTIAL`, read **every file** listed under `Remaining Active Context Files` in numerical order (`part1`, `part2`, ...), issuing a **separate top-level tool call for each individual file** (never combine multiple part files into a single `cat`/shell command or a single `functions.exec` script, omit `StartLine`/`EndLine` so each part file is read in full without truncation, and in Codex `functions.exec` start the script with `// @exec: {"max_output_tokens": 50000}`) before replying to the user.
   - Once all part files are loaded (`Handoff Status: COMPLETE` or after reading all `Remaining Active Context Files`), review the modified files, recent shell commands, the latest compaction checkpoint (`/compact`), and post-compaction dialogue turns.
   - The complete pre-compaction transcript and raw tool outputs are also saved in `/tmp/baton-handoff/<session_id>/full_dialogue.md` (`%TEMP%\baton-handoff\<session_id>\full_dialogue.md` on Windows) if you ever need earlier pre-compaction turns or raw compiler logs.
   - Do NOT run extra shell commands (such as `go test`, `git status`, or `ls`) after `baton load` unless the user's message explicitly included a new task alongside `baton load`. Briefly confirm the last step reached by the previous agent and ask how the user would like to proceed (or continue if they already specified the next step).
