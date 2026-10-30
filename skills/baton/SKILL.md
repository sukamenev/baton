---
name: baton
description: >-
  Lossless session handoff across CLI coding agents (claude, codex, agy, pi, hermes)
  and client-side encrypted session + workspace transfer between machines. Activate
  this skill whenever the user asks in plain words, in any language, to continue/load/resume a session
  (by name, from another agent, or the latest session), name a session, list recent
  sessions, send/push a session to another computer, or load a baton:... token.
---

# Baton Skill: Cross-Agent & Cross-Machine Session Handoff

When the user asks to continue, list, save, import, or transfer a session, run the corresponding `baton` command via your shell tool.

The user may write in any language. The phrases in the table below are English examples: match the user's request by meaning, not by wording, and if it means the same as a row, run that row's command.

| What the user says (examples) | Command to run |
|---|---|
| "Load/continue the latest session" | `baton load` |
| "Load session my_session" | `baton load my_session` |
| "Load session from claude" | `baton load claude` |
| "Load session my_session from claude" | `baton load claude my_session` |
| "Show recent/saved sessions" (or the user forgot the name) | `baton list` |
| "Load session #1" / "Load the first session" | `baton load 1` |
| "Load session 726d401d" (a session ID from the list) | `baton load 726d401d` |
| "Save/name this session" | `baton save` |
| "Save this session as my_session" | `baton save my_session` |
| "Push/send this session to my laptop" | `baton push` |
| "Load remote session https://...#key or baton:..." | `baton load <URL\|token>` |
| "Delete saved session my_session" | `baton rm my_session` (or `baton rm <#>` with the number from `baton list`) |
| "Clear/delete all saved sessions" | `baton clear` |
| "Update baton" | `baton update` |
| "The session was not saved (context/quota ran out), take one of the existing sessions" | `baton import` (only lists the agents' own sessions that can be imported into baton's store; then ask which one to import) |
| "Import session #2" / "Import session my_session" / "Import session 726d401d" | `baton import 2` / `baton import my_session` / `baton import 726d401d` |
| "Save/name session #7 as my_session" (a session other than the current one) | `baton import 7 as my_session` |
| "Import session #3 as my_session without loading it" | `baton import 3 as my_session --no-load` |

`baton save` works only with the current session. Never pass it a number or another session; use `baton import <#|id> as <name>` instead.

Note for Codex CLI: if a remote `baton load <URL>` or `baton push` fails due to sandbox network restrictions and permission escalation is rejected by policy, tell the user to run `!baton load "<URL>"` (or `!baton push`) directly in the Codex prompt with a leading exclamation mark `!`, and then type `baton load`.

## Rules After Running `baton list`, `baton load`, or `baton import`

1. **If `baton` returned a numbered candidate list (`#1`, `#2`, ...):**
   - The user usually cannot see raw command output (CLI agents fold it), so your reply is the only place where they see the list. Reproduce **every** entry exactly as printed, keeping its Markdown (line breaks, indentation, bold, italics): number, time, agent, name, session ID, working directory, and the last prompt. Do not summarize, regroup, merge, or collapse entries (never write things like "#7–#15: older sessions"), do not drop previews, and do not add your own commentary between entries.
   - Then ask the user which number or name to load/import, and wait for the answer. Do not pick a session yourself after `baton list` or `baton import`; only after a `baton load` fallback list, if #1 clearly matches the current project directory, you may run `baton load 1` directly.
   - If `baton load` reports that there are no saved sessions (or none of them is the one the user wants), run `baton import` to list raw sessions of all agents and let the user pick one. `baton import <#|name>` saves the chosen session into baton and loads it in one step.
2. **Check `Handoff Status` at the bottom of the `baton load` output:**
   - If it says `Handoff Status: PARTIAL`, read **every file** listed under `Remaining Active Context Files` in numerical order (`part1`, `part2`, ...), issuing a **separate top-level tool call for each individual file** (never combine multiple part files into a single `cat`/shell command or a single `functions.exec` script, omit `StartLine`/`EndLine` so each part file is read in full without truncation, and in Codex `functions.exec` start the script with `// @exec: {"max_output_tokens": 50000}`) before replying to the user.
   - Once all part files are loaded (`Handoff Status: COMPLETE` or after reading all `Remaining Active Context Files`), review the modified files, recent shell commands, the latest compaction checkpoint (`/compact`), and post-compaction dialogue turns.
   - The complete pre-compaction transcript and raw tool outputs are also saved in `/tmp/baton-handoff/<session_id>/full_dialogue.md` (`%TEMP%\baton-handoff\<session_id>\full_dialogue.md` on Windows) if you ever need earlier pre-compaction turns or raw compiler logs.
   - Do NOT run extra shell commands (such as `go test`, `git status`, or `ls`) after `baton load` unless the user's message explicitly included a new task alongside `baton load`. Briefly confirm the last step reached by the previous agent and ask how the user would like to proceed (or continue if they already specified the next step).
