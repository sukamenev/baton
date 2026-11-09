---
name: baton
description: >-
  Save, load, and transfer agent sessions with Baton.
  Always activate for explicit baton commands or baton: tokens.
  Also activate when the user explicitly mentions Baton, or asks to
  save or transfer a session for use in another agent or on another computer.
  Otherwise, generic requests to find, list, name, save, or resume
  sessions do not activate this skill.
---

# Baton Skill: Cross-Agent & Cross-Machine Session Handoff

Execute explicit `baton` commands as requested via your shell tool.
The natural-language examples below apply only after this skill is activated;
they are not independent activation triggers.

The user may write in any language. The phrases in the table below are English examples: match the user's request by meaning, not by wording, and if it means the same as a row, run that row's command.

| What the user says (examples) | Command to run |
|---|---|
| "Resume/continue session my_session" (a session named by the user, wherever they worked on it last) | `baton resume my_session` |
| "Load/continue the latest session" (no name given) | `baton load` |
| "Load session my_session" | `baton load my_session` |
| "Load session from claude" | `baton load claude` |
| "Load session my_session from claude" | `baton load claude my_session` |
| "Show recent/saved sessions" (or the user forgot the name) | `baton list` |
| "Load session #1" / "Load the first session" | `baton load 1` |
| "Load session 726d401d" (a session ID from the list) | `baton load 726d401d` |
| "Save/name this session" | `baton save` (keeps the name the session was loaded or saved as) |
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

Note for Antigravity CLI (agy): run every `baton` command with `BypassSandbox: true`. The default command sandbox sees neither `~/.local/bin/baton` nor the agents' session folders in the home directory, so a sandboxed call only fails with `command not found`. If running outside the sandbox is denied, tell the user to run `!baton <arguments>` at the agy prompt themselves.

In Cursor (IDE agent and `agent` CLI), use the installed Baton skill to run the same `baton` commands. The activation conditions above apply in Cursor too.

`baton resume <name>` continues a session from its latest point: the copy saved in baton or an agent session that went on after it. If its output says to continue the session natively, do not load anything: show the user the commands from the output exactly as printed and stop. Run `baton resume <name> --load` only if the user asks to load it into this conversation anyway.

`baton load`, `baton resume`, and `baton import` refuse to load a session that worked in another folder that exists on this machine (you cannot change your own working folder). Then show the user baton's message, including its `cd … && <agent>` command, and ask whether they will restart the agent in that folder (recommended) or want to load the session here anyway. Rerun the same command with `--force` only if the user chooses to load it here.

If a remote `baton load <URL>` prints `Repository Check` warnings (this folder is not the session's repository, branch, or revision), show them to the user with their commands before doing anything else. If `baton push` prints warnings about commits that are not on the server, show them too: the other computer cannot get those commits until they are pushed.

## Rules After Running `baton list`, `baton load`, `baton resume`, or `baton import`

1. **If `baton` returned a numbered candidate list (`#1`, `#2`, ...):**
   - The user usually cannot see raw command output (CLI agents fold it), so your reply is the only place where they see the list. Copy the list into your reply **character for character**, from the first `**#1**` line to the last entry, keeping its Markdown (line breaks, indentation, bold, inline code). This overrides your usual formatting habits. Forbidden:
     - converting the list into a table or columns, or changing its layout;
     - translating anything, including agent names and `(unnamed)`;
     - inventing or "improving" session names or descriptions (an `(unnamed)` session stays `(unnamed)`);
     - reformatting times or dropping the session ID, folder, or last prompt;
     - summarizing, regrouping, merging, or collapsing entries (never write things like "#9–#15: older sessions");
     - adding your own commentary between entries.
   - Then ask the user which number or name to load/import/resume, and wait for the answer. Do not pick a session yourself after `baton list`, `baton import`, or a `baton resume` list; only after a `baton load` fallback list, if #1 clearly matches the current project directory, you may run `baton load 1` directly.
   - If `baton load` reports that there are no saved sessions (or none of them is the one the user wants), run `baton import` to list raw sessions of all agents and let the user pick one. `baton import <#|name>` saves the chosen session into baton and loads it in one step.
2. **Check `Handoff Status` at the bottom of the output of `baton load` (and of `baton resume` or `baton import` when they loaded a session):**
   - If it says `Handoff Status: PARTIAL`, read **every file** listed under `Remaining Active Context Files` in numerical order (`part1`, `part2`, ...), issuing a **separate top-level tool call for each individual file** (never combine multiple part files into a single `cat`/shell command or a single `functions.exec` script, omit `StartLine`/`EndLine` so each part file is read in full without truncation, and in Codex `functions.exec` start the script with `// @exec: {"max_output_tokens": 50000}`) before replying to the user.
   - Once all part files are loaded (`Handoff Status: COMPLETE` or after reading all `Remaining Active Context Files`), review the modified files, recent shell commands, the latest compaction checkpoint (`/compact`), and post-compaction dialogue turns.
   - The complete pre-compaction transcript and raw tool outputs are also saved in `full_dialogue.md` next to the active context files (`/tmp/baton-<uid>/handoff/<session_id>/` on Linux/macOS, `%TEMP%\baton\handoff\<session_id>\` on Windows; the exact path is printed in the load output) if you ever need earlier pre-compaction turns or raw compiler logs.
   - Do NOT run extra shell commands (such as `go test`, `git status`, or `ls`) after loading a session (`baton load`, `baton resume`, `baton import`) unless the user's message explicitly included a new task alongside the load. Briefly confirm the last step reached by the previous agent and ask how the user would like to proceed (or continue if they already specified the next step).
