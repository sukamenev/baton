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

When the user asks in plain language (Russian, English, or any language) to continue or transfer a session, run the corresponding `baton` command via your shell tool:

| What the user says (examples) | Command to run |
|---|---|
| "Load/continue the latest session" / "Загрузи последнюю сессию" | `baton load` |
| "Load session auth-refactor" / "Загрузи сессию auth-refactor" | `baton load auth-refactor` |
| "Load session from claude" / "Загрузи сессию от claude" | `baton load claude` |
| "Load session auth-refactor from claude" | `baton load claude auth-refactor` |
| "Show recent sessions" / "Какие есть сессии?" (or user forgot the name) | `baton list` |
| "Load candidate #1" / "Загрузи первую сессию" | `baton load 1` |
| "Save/name this session auth-refactor" / "Сохрани сессию auth-refactor" | `baton save auth-refactor` |
| "Push/send this session for my laptop" / "Отправь сессию на другой компьютер" | `baton push` |
| "Load remote session baton:..." / "Загрузи сессию baton:..." | `baton load <token>` |

## Rules After Running `baton load`

1. **If `baton` returned a numbered candidate list (`#1`, `#2`, ...):**
   - Show the numbered candidates to the user so they can pick one, or if #1 clearly matches the current project directory, run `baton load 1`.
2. **When the handoff summary is loaded:**
   - Review the modified files, recent shell commands, the latest compaction checkpoint (`/compact`), and post-compaction dialogue turns.
   - The complete, lossless transcript and raw tool outputs are saved in the OS temporary directory (`/tmp/baton-handoff/<session_id>/full_dialogue.md` on Linux/macOS or `%TEMP%\baton-handoff\<session_id>\full_dialogue.md` on Windows). Read that file or run `baton turn <session_id> <turn_id>` only if you need full compiler outputs or earlier pre-compaction turns.
   - Briefly confirm the last step reached by the previous agent, verify the workspace files, and immediately continue the task.
