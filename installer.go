package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

const skillMarkdownContent = `---
name: baton
description: >-
  Lossless session handoff across CLI coding agents (claude, codex, agy, pi, hermes)
  and client-side encrypted session + workspace transfer between machines. Activate
  this skill whenever the user asks in plain words to continue/load/resume a session
  (by name, from another agent, or the latest session), name a session, list recent
  sessions, send/push a session to another computer, or load a baton:... token.
---

# Baton Skill: Cross-Agent & Cross-Machine Session Handoff

When the user asks in plain language (Russian, English, or any language) to continue or transfer a session, run the corresponding ` + "`baton`" + ` command via your shell tool:

| What the user says (examples) | Command to run |
|---|---|
| "Load/continue the latest session" / "Загрузи последнюю сессию" | ` + "`baton load`" + ` |
| "Load session auth-refactor" / "Загрузи сессию auth-refactor" | ` + "`baton load auth-refactor`" + ` |
| "Load session from claude" / "Загрузи сессию от claude" | ` + "`baton load claude`" + ` |
| "Load session auth-refactor from claude" | ` + "`baton load claude auth-refactor`" + ` |
| "Show recent sessions" / "Какие есть сессии?" (or user forgot the name) | ` + "`baton list`" + ` |
| "Load candidate #1" / "Загрузи первую сессию" | ` + "`baton load 1`" + ` |
| "Save/name this session auth-refactor" / "Сохрани сессию auth-refactor" | ` + "`baton save auth-refactor`" + ` |
| "Push/send this session for my laptop" / "Отправь сессию на другой компьютер" | ` + "`baton push`" + ` |
| "Load remote session baton:..." / "Загрузи сессию baton:..." | ` + "`baton load <token>`" + ` |

## Rules After Running ` + "`baton load`" + `

1. **If ` + "`baton`" + ` returned a numbered candidate list (` + "`#1`" + `, ` + "`#2`" + `, ...):**
   - Show the numbered candidates to the user so they can pick one, or if #1 clearly matches the current project directory, run ` + "`baton load 1`" + `.
2. **When the handoff summary is loaded:**
   - Review the modified files, recent shell commands, the latest compaction checkpoint (` + "`/compact`" + `), and post-compaction dialogue turns.
   - The complete, lossless transcript and raw tool outputs are saved in the OS temporary directory (` + "`/tmp/baton-handoff/<session_id>/full_dialogue.md`" + ` on Linux/macOS or ` + "`%TEMP%\\baton-handoff\\<session_id>\\full_dialogue.md`" + ` on Windows). Read that file or run ` + "`baton turn <session_id> <turn_id>`" + ` only if you need full compiler outputs or earlier pre-compaction turns.
   - Briefly confirm the last step reached by the previous agent, verify the workspace files, and immediately continue the task.
`

const slashCommandContent = `Execute a session handoff operation via the baton CLI tool using arguments: $ARGUMENTS

Argument routing:
- If no arguments are given: run "baton load" to load the most recent session in the current project.
- If "load [target]" or a session name / agent name (claude, codex, agy, pi, hermes) / candidate number (1, 2) / baton:... token is given: run "baton load $ARGUMENTS" and continue the interrupted task.
- If "push" or "save --remote" is given: run "baton push" and show the resulting baton:... token and ready-to-paste prompt to the user.
- If "save <alias>" or "name <alias>" is given: run "baton save <alias>".
- If "list" is given: run "baton list".
`

func resolveBinaryPath() string {
	exe, err := os.Executable()
	if err == nil {
		if abs, err := filepath.Abs(exe); err == nil {
			return abs
		}
	}
	home, _ := os.UserHomeDir()
	binName := "baton"
	if runtime.GOOS == "windows" {
		binName = "baton.exe"
	}
	return filepath.Join(home, ".local", "bin", binName)
}

func skillTargetPaths(home string) []string {
	return []string{
		filepath.Join(claudeBaseDir(home), "skills", "baton", "SKILL.md"),
		filepath.Join(codexBaseDir(home), "skills", "baton", "SKILL.md"),
		filepath.Join(home, ".gemini", "config", "skills", "baton", "SKILL.md"),
		filepath.Join(piBaseDir(home), "skills", "baton", "SKILL.md"),
		filepath.Join(hermesBaseDir(home), "skills", "baton", "SKILL.md"),
	}
}

func syncSkillsSilent() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	for _, target := range skillTargetPaths(home) {
		if err := os.MkdirAll(filepath.Dir(target), 0755); err == nil {
			_ = os.WriteFile(target, []byte(skillMarkdownContent), 0644)
		}
	}
	cmdTarget := filepath.Join(claudeBaseDir(home), "commands", "baton.md")
	if err := os.MkdirAll(filepath.Dir(cmdTarget), 0755); err == nil {
		_ = os.WriteFile(cmdTarget, []byte(slashCommandContent), 0644)
	}
	return nil
}

func runInstall() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	binPath := resolveBinaryPath()
	fmt.Printf("Installing baton skills & commands (binary: %s, OS: %s)\n\n", binPath, runtime.GOOS)

	for _, target := range skillTargetPaths(home) {
		if err := os.MkdirAll(filepath.Dir(target), 0755); err == nil {
			if err := os.WriteFile(target, []byte(skillMarkdownContent), 0644); err == nil {
				fmt.Printf("[OK] Installed skill: %s\n", target)
			}
		}
	}

	cmdTarget := filepath.Join(claudeBaseDir(home), "commands", "baton.md")
	if err := os.MkdirAll(filepath.Dir(cmdTarget), 0755); err == nil {
		if err := os.WriteFile(cmdTarget, []byte(slashCommandContent), 0644); err == nil {
			fmt.Printf("[OK] Installed /baton slash command: %s\n", cmdTarget)
		}
	}

	fmt.Println("\nInstallation complete. Ensure baton is in your PATH (e.g. ~/.local/bin/baton).")
	return nil
}

func runStatus() {
	home, _ := os.UserHomeDir()
	binPath := resolveBinaryPath()
	fmt.Printf("baton %s (%s/%s)\nBinary path: %s\nHandoff temp directory: %s\n\n", version, runtime.GOOS, runtime.GOARCH, binPath, handoffBaseDir())

	checks := []struct {
		label string
		path  string
	}{
		{"Claude Skill (~/.claude/skills/baton/SKILL.md)", filepath.Join(claudeBaseDir(home), "skills", "baton", "SKILL.md")},
		{"Codex Skill (~/.codex/skills/baton/SKILL.md)", filepath.Join(codexBaseDir(home), "skills", "baton", "SKILL.md")},
		{"Antigravity Skill (~/.gemini/config/skills/baton/SKILL.md)", filepath.Join(home, ".gemini", "config", "skills", "baton", "SKILL.md")},
		{"Pi Skill (~/.pi/agent/skills/baton/SKILL.md)", filepath.Join(piBaseDir(home), "skills", "baton", "SKILL.md")},
		{"Hermes Skill (~/.hermes/skills/baton/SKILL.md)", filepath.Join(hermesBaseDir(home), "skills", "baton", "SKILL.md")},
		{"Slash Command /baton (~/.claude/commands/baton.md)", filepath.Join(claudeBaseDir(home), "commands", "baton.md")},
	}

	for _, c := range checks {
		data, err := os.ReadFile(c.path)
		if err == nil && strings.Contains(string(data), "baton") {
			fmt.Printf("  [INSTALLED] %s\n", c.label)
		} else {
			fmt.Printf("  [MISSING]   %s\n", c.label)
		}
	}
}
