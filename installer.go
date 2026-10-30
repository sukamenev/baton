package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

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
