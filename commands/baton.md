Execute a session handoff operation via the baton CLI tool using arguments: $ARGUMENTS

Argument routing:
- If no arguments are given: run "baton load" to load the most recent session in the current project.
- If "load [target]" or a session name / agent name (claude, codex, agy, pi, hermes) / number (1, 2) / baton:... token is given: run "baton load $ARGUMENTS" and continue the interrupted task.
- If "push" or "save --remote" is given: run "baton push" and show the resulting baton:... token and ready-to-paste prompt to the user.
- If "save <name>" or "name <name>" is given: run "baton save <name>" (current session only; for another session run "baton import <#|id> as <name>").
- If "list" is given: run "baton list".
- If "import [#|name|id] [as <name>] [--no-load]" is given: run "baton import $ARGUMENTS" (without a number/name/id it lists raw sessions of all agents).
