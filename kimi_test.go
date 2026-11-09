package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeKimiFixture(t *testing.T, root, bucket, id, title, prompt, answer string) string {
	t.Helper()
	sessionDir := filepath.Join(root, "sessions", bucket, "session_"+id)
	wireDir := filepath.Join(sessionDir, "agents", "main")
	if err := os.MkdirAll(wireDir, 0755); err != nil {
		t.Fatal(err)
	}
	state := `{"id":"session_` + id + `","version":2,"cwd":"/work/` + id + `","title":"` + title + `","lastPrompt":"` + prompt + `","isCustomTitle":true}`
	if err := os.WriteFile(filepath.Join(sessionDir, "state.json"), []byte(state), 0600); err != nil {
		t.Fatal(err)
	}
	lines := []string{
		`{"type":"metadata","protocol_version":"1.5","created_at":1794068879338}`,
		`{"type":"agent.message.appended","time":1794068879372,"message":{"message":{"role":"user","content":[{"type":"text","text":"` + prompt + `"}],"origin":{"kind":"user"}}}}`,
		`{"type":"context.append_message","message":{"role":"user","content":[{"type":"text","text":"duplicate context copy"}]}}`,
		`{"type":"context.append_loop_event","event":{"type":"tool.call","name":"Read","args":{"path":"duplicate.txt"}}}`,
		`{"type":"agent.message.appended","time":1794068890451,"message":{"message":{"role":"assistant","content":[{"type":"think","think":"Reading the fixture"}],"toolCalls":[{"type":"function","id":"call-1","name":"Read","arguments":"{\"path\":\"fixture.txt\"}"}]}}}`,
		`{"type":"agent.message.appended","time":1794068890451,"message":{"message":{"role":"tool","content":[{"type":"text","text":"fixture contents"}],"toolCallId":"call-1"}}}`,
		`{"type":"agent.message.appended","time":1794068890452,"message":{"message":{"role":"assistant","content":[{"type":"text","text":"` + answer + `"}],"toolCalls":[]}}}`,
	}
	path := filepath.Join(wireDir, "wire.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestKimiDiscoveryAndCompletedMessages(t *testing.T) {
	root := t.TempDir()
	cliPath := writeKimiFixture(t, root, "wd_test", "cli-fixture-123", "CLI fixture", "Read CLI fixture", "CLI answer")
	desktopPath := writeKimiFixture(t, root, "wm_test", "desktop-fixture-456", "Desktop fixture", "Read Desktop fixture", "Desktop answer")
	t.Setenv("KIMI_CODE_HOME", root)
	t.Setenv("BATON_CONFIG_DIR", t.TempDir())

	candidates := discoverSessions("kimi", "")
	if len(candidates) != 2 {
		t.Fatalf("expected CLI and Desktop sessions, got %+v", candidates)
	}
	byID := make(map[string]SessionCandidate)
	for _, c := range candidates {
		byID[c.SessionID()] = c
	}
	if c := byID["cli-fixture-123"]; c.Path != cliPath || c.Title != "CLI fixture" || c.CWD != "/work/cli-fixture-123" || c.Preview != "Read CLI fixture" {
		t.Fatalf("unexpected CLI candidate: %+v", c)
	}
	if c := byID["desktop-fixture-456"]; c.Path != desktopPath || c.Title != "Desktop fixture" {
		t.Fatalf("unexpected Desktop candidate: %+v", c)
	}
	if ag, q := resolveAgentAndQuery([]string{"kimi"}, "latest"); ag != "kimi" || q != "latest" {
		t.Fatalf("resolve kimi: %q %q", ag, q)
	}

	ps, err := parseSessionFile("kimi", desktopPath, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if ps.SessionID != "kimi-desktop-fixture-456" || ps.Title != "Desktop fixture" || ps.CWD != "/work/desktop-fixture-456" {
		t.Fatalf("unexpected metadata: %+v", ps)
	}
	if len(ps.AllTurns) != 5 {
		t.Fatalf("expected user, thinking, call, result, answer without duplicates; got %+v", ps.AllTurns)
	}
	if ps.AllTurns[2].Role != "tool_use" || ps.AllTurns[2].ToolName != "Read" || !strings.Contains(ps.AllTurns[2].RawOutput, "fixture.txt") {
		t.Fatalf("tool call not preserved: %+v", ps.AllTurns[2])
	}
	if ps.AllTurns[3].Role != "tool_result" || ps.AllTurns[3].ToolName != "Read" || ps.AllTurns[3].RawOutput != "fixture contents" {
		t.Fatalf("tool result not preserved: %+v", ps.AllTurns[3])
	}
	if ps.AllTurns[4].Text != "Desktop answer" || ps.AllTurns[4].Timestamp == "" {
		t.Fatalf("assistant answer not preserved: %+v", ps.AllTurns[4])
	}
	if _, _, preview := inspectSessionQuickMeta("kimi", cliPath); preview != "Read CLI fixture" {
		t.Fatalf("quick preview: %q", preview)
	}
}

func TestKimiCompactionVariants(t *testing.T) {
	for _, tc := range []struct {
		name   string
		record string
		want   string
	}{
		{"string", `{"type":"context.apply_compaction","summary":"Current summary","compactedCount":2}`, "Current summary"},
		{"context string", `{"type":"context.apply_compaction","contextSummary":"Media-safe summary","compactedCount":2}`, "Media-safe summary"},
		{"legacy message", `{"type":"context.apply_compaction","summary":{"role":"user","content":[{"type":"text","text":"Legacy summary"}]},"count":2}`, "Legacy summary"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "wire.jsonl")
			lines := []string{
				`{"type":"agent.message.appended","message":{"message":{"role":"user","content":[{"type":"text","text":"Before compaction"}]}}}`,
				tc.record,
				`{"type":"agent.message.appended","message":{"message":{"role":"assistant","content":[{"type":"text","text":"After compaction"}]}}}`,
			}
			if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0600); err != nil {
				t.Fatal(err)
			}
			ps, err := parseSessionFile("kimi", path, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			if ps.CompactSummary != tc.want || ps.PreCompactTurns != 1 || len(ps.AllTurns) != 2 || len(ps.PostCompact) != 1 || ps.PostCompact[0].Text != "After compaction" {
				t.Fatalf("unexpected compaction: %+v", ps)
			}
		})
	}
}

func TestKimiImportAndSkillPath(t *testing.T) {
	root := t.TempDir()
	id := "import-fixture-789"
	writeKimiFixture(t, root, "wd_test", id, "Kimi import", "Import this Kimi session", "Imported answer")
	t.Setenv("KIMI_CODE_HOME", root)
	t.Setenv("BATON_CONFIG_DIR", t.TempDir())
	t.Setenv("TMPDIR", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(t.TempDir(), "claude"))
	t.Setenv("CODEX_HOME", filepath.Join(t.TempDir(), "codex"))
	t.Setenv("CURSOR_HOME", filepath.Join(t.TempDir(), "cursor"))
	t.Setenv("PI_CODING_AGENT_DIR", filepath.Join(t.TempDir(), "pi"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(t.TempDir(), "xdg"))

	report, err := importSession([]string{id[:8], "as", "kimi-check", "--no-load"})
	if err != nil || !strings.Contains(report, "Imported [kimi]") {
		t.Fatalf("import failed: %v\n%s", err, report)
	}
	ps, err := findAndParseSession("kimi", "kimi-check")
	if err != nil || len(ps.AllTurns) != 5 || ps.AllTurns[3].RawOutput != "fixture contents" {
		t.Fatalf("saved import is incomplete: err=%v session=%+v", err, ps)
	}
	wantSkill := filepath.Join(root, "skills", "baton", "SKILL.md")
	found := false
	for _, path := range skillTargetPaths("unused-home") {
		if path == wantSkill {
			found = true
		}
	}
	if !found {
		t.Fatalf("Kimi skill path %s not installed", wantSkill)
	}
}
