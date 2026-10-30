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

When the user asks in plain language (Russian, English, or any language) to continue or transfer a session, run the corresponding ` + "`baton`" + ` command via your shell tool (Note for Codex CLI: run ` + "`baton`" + ` normally first; if a remote ` + "`baton load <URL>`" + ` or ` + "`baton push`" + ` fails due to sandbox network restrictions and permission escalation is rejected by policy, tell the user to run ` + "`!baton load \"<URL>\"`" + ` directly in the Codex prompt with a leading exclamation mark ` + "`!`" + ` — which downloads and saves the session into the local index — and then type ` + "`baton load`" + `; or ` + "`!baton push`" + ` to upload):

| What the user says (examples) | Command to run |
|---|---|
| "Load/continue the latest session" / "Загрузи последнюю сессию" | ` + "`baton load`" + ` |
| "Load session auth-refactor" / "Загрузи сессию auth-refactor" | ` + "`baton load auth-refactor`" + ` |
| "Load session from claude" / "Загрузи сессию от claude" | ` + "`baton load claude`" + ` |
| "Load session auth-refactor from claude" | ` + "`baton load claude auth-refactor`" + ` |
| "Show recent sessions" / "Какие есть сессии?" (or user forgot the name) | ` + "`baton list`" + ` |
| "Load candidate #1" / "Загрузи первую сессию" | ` + "`baton load 1`" + ` |
| "Save/name this session" / "Сохрани текущую сессию" | ` + "`baton save`" + ` |
| "Save/name this session auth-refactor" / "Сохрани сессию auth-refactor" | ` + "`baton save auth-refactor`" + ` |
| "Push/send this session for my laptop" / "Отправь сессию на другой компьютер" | ` + "`baton push`" + ` |
| "Load remote session https://...#key or baton:..." / "Загрузи сессию по ссылке/токену" | ` + "`baton load <URL|token>`" + ` |

## Rules After Running ` + "`baton load`" + `

1. **If ` + "`baton`" + ` returned a numbered candidate list (` + "`#1`" + `, ` + "`#2`" + `, ...):**
   - Show the numbered candidates to the user so they can pick one, or if #1 clearly matches the current project directory, run ` + "`baton load 1`" + `.
2. **Check ` + "`Handoff Status`" + ` at the bottom of the ` + "`baton load`" + ` output:**
   - If it says ` + "`Handoff Status: PARTIAL`" + `, read **every file** listed under ` + "`Remaining Active Context Files`" + ` in numerical order (` + "`part1`" + `, ` + "`part2`" + `, ...), issuing a **separate top-level tool call for each individual file** (never combine multiple part files into a single ` + "`cat`" + `/shell command or a single ` + "`functions.exec`" + ` script, omit ` + "`StartLine`" + `/` + "`EndLine`" + ` so each part file is read in full without truncation, and in Codex ` + "`functions.exec`" + ` start the script with ` + "`// @exec: {\"max_output_tokens\": 50000}`" + `) before replying to the user.
   - Once all part files are loaded (` + "`Handoff Status: COMPLETE`" + ` or after reading all ` + "`Remaining Active Context Files`" + `), review the modified files, recent shell commands, the latest compaction checkpoint (` + "`/compact`" + `), and post-compaction dialogue turns.
   - The complete pre-compaction transcript and raw tool outputs are also saved in ` + "`/tmp/baton-handoff/<session_id>/full_dialogue.md`" + ` (` + "`%TEMP%\\baton-handoff\\<session_id>\\full_dialogue.md`" + ` on Windows) if you ever need earlier pre-compaction turns or raw compiler logs.
   - Do NOT run extra shell commands (such as ` + "`go test`" + `, ` + "`git status`" + `, or ` + "`ls`" + `) after ` + "`baton load`" + ` unless the user's message explicitly included a new task alongside ` + "`baton load`" + `. Briefly confirm the last step reached by the previous agent and ask how the user would like to proceed (or continue if they already specified the next step).
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

func codexRulesContent(binPath string) string {
	return fmt.Sprintf("prefix_rule(pattern=[\"baton\"], decision=\"allow\")\nprefix_rule(pattern=[%q], decision=\"allow\")\n", binPath)
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
	rulesTarget := filepath.Join(codexBaseDir(home), "rules", "baton.rules")
	if err := os.MkdirAll(filepath.Dir(rulesTarget), 0755); err == nil {
		_ = os.WriteFile(rulesTarget, []byte(codexRulesContent(resolveBinaryPath())), 0644)
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

	rulesTarget := filepath.Join(codexBaseDir(home), "rules", "baton.rules")
	if err := os.MkdirAll(filepath.Dir(rulesTarget), 0755); err == nil {
		if err := os.WriteFile(rulesTarget, []byte(codexRulesContent(binPath)), 0644); err == nil {
			fmt.Printf("[OK] Installed Codex execpolicy rule: %s\n", rulesTarget)
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
		{"Codex Execpolicy Rule (~/.codex/rules/baton.rules)", filepath.Join(codexBaseDir(home), "rules", "baton.rules")},
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
