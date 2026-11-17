package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func kiroRow(id string, payload map[string]any) string {
	data, _ := json.Marshal(map[string]any{"id": id, "timestamp": "2026-11-17T11:09:35Z", "payload": payload})
	return string(data)
}

func writeKiroFixture(t *testing.T, root, id, title, cwd string, rows ...string) string {
	t.Helper()
	path := filepath.Join(root, "sessions", "workspace-hash", "sess_"+id, "messages.jsonl")
	writeLines(t, path, time.Now(), rows...)
	data, _ := json.Marshal(kiroSessionMeta{ID: "sess_" + id, Title: title, WorkspacePaths: []string{cwd}})
	if err := os.WriteFile(filepath.Join(filepath.Dir(path), "session.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestKiroV3TranscriptAndCompaction(t *testing.T) {
	tmp := isolateAgents(t)
	root := filepath.Join(tmp, "kiro")
	path := writeKiroFixture(t, root, "12345678-1234-4234-8234-123456789abc", "Automatic title", "/work/project",
		kiroRow("context_snapshot_1", map[string]any{"type": "agent_note", "content": "injected model name"}),
		kiroRow("user-1", map[string]any{"type": "user", "content": "Read README.md"}),
		kiroRow("call-read", map[string]any{"type": "tool_call", "toolCallId": "read-1", "toolName": "read_file", "args": map[string]any{"path": "README.md"}}),
		kiroRow("result-read", map[string]any{"type": "tool_result", "toolCallId": "read-1", "content": "Full README contents"}),
		kiroRow("reasoning-hidden", map[string]any{"type": "assistant", "operationType": "Reasoning", "content": "...", "reasoningSignature": "opaque"}),
		kiroRow("answer-1", map[string]any{"type": "assistant", "operationType": "Say", "content": "Project explained"}),
		kiroRow("tombstone", map[string]any{"type": "tombstone", "kind": "summarization", "effectiveFromMessageId": "user-1"}),
		kiroRow("summary-1", map[string]any{"type": "assistant", "operationType": "Summary", "content": "First summary"}),
		kiroRow("user-2", map[string]any{"type": "user", "content": "Check and update config"}),
		kiroRow("reasoning", map[string]any{"type": "assistant", "operationType": "Reasoning", "content": "Check before editing"}),
		kiroRow("call-shell", map[string]any{"type": "tool_call", "toolCallId": "bash-1", "toolName": "execute_bash", "args": map[string]any{"command": "git status --short"}}),
		kiroRow("result-shell", map[string]any{"type": "tool_result", "toolCallId": "bash-1", "content": ""}),
		kiroRow("call-write", map[string]any{"type": "tool_call", "toolCallId": "write-1", "toolName": "fs_write", "args": map[string]any{"path": "/work/project/config.json", "text": "{}"}}),
		kiroRow("result-write", map[string]any{"type": "tool_result", "toolCallId": "write-1", "content": "Created config"}),
		kiroRow("start-sub", map[string]any{"type": "sub_agent_start", "subSessionId": "child-1", "subAgentName": "inspect", "prompt": "Check config"}),
		kiroRow("end-sub", map[string]any{"type": "sub_agent_complete", "subSessionId": "child-1", "response": "Config is valid"}),
		kiroRow("note", map[string]any{"type": "agent_note", "content": "Hook: use the project conventions"}),
		kiroRow("usage", map[string]any{"type": "usage_summary"}),
		kiroRow("approval", map[string]any{"type": "pending_interaction", "question": "Allow?"}),
		kiroRow("answer-2", map[string]any{"type": "assistant", "operationType": "Say", "content": "Done"}),
	)
	ps, err := parseSessionFile("kiro", path, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if ps.SessionID != "kiro-12345678-1234-4234-8234-123456789abc" || ps.CWD != "/work/project" || ps.Title != "Automatic title" {
		t.Fatalf("unexpected metadata: %+v", ps)
	}
	if ps.CompactSummary != "First summary" || ps.PreCompactTurns != 4 || len(ps.PostCompact) != 10 || len(ps.AllTurns) != 14 {
		t.Fatalf("unexpected compaction: pre=%d post=%d all=%d summary=%q", ps.PreCompactTurns, len(ps.PostCompact), len(ps.AllTurns), ps.CompactSummary)
	}
	if ps.AllTurns[2].RawOutput != "Full README contents" || ps.AllTurns[2].ToolName != "read_file" {
		t.Fatalf("tool output lost: %+v", ps.AllTurns[2])
	}
	if ps.PostCompact[3].Role != "tool_result" || ps.PostCompact[3].ToolName != "execute_bash" {
		t.Fatalf("empty tool result lost: %+v", ps.PostCompact[3])
	}
	if ps.PostCompact[7].ToolName != "sub_agent:inspect" || ps.PostCompact[7].RawOutput != "Config is valid" || countTypedPrompts(ps.AllTurns) != 2 {
		t.Fatalf("subagent results counted as user turns or lost: %+v", ps.PostCompact)
	}
	if strings.Join(ps.Commands, "|") != "git status --short" || strings.Join(ps.ModifiedFiles, "|") != "/work/project/config.json" {
		t.Fatalf("side effects: commands=%q files=%q", ps.Commands, ps.ModifiedFiles)
	}
	if _, _, preview := inspectSessionQuickMeta("kiro", path); preview != "Check and update config" {
		t.Fatalf("preview: %q", preview)
	}
	// Only a completed Summary changes the active context. The latest one wins.
	appendLines(t, path, time.Now(), kiroRow("summary-2", map[string]any{"type": "assistant", "operationType": "Summary", "content": "Latest summary"}),
		kiroRow("pending-summary", map[string]any{"type": "tombstone", "kind": "summarization"}),
		kiroRow("user-3", map[string]any{"type": "user", "content": "Continue after the second compaction"}))
	ps, err = parseSessionFile("kiro", path, time.Now())
	if err != nil || ps.CompactSummary != "Latest summary" || len(ps.PostCompact) != 1 || len(ps.AllTurns) != 15 || ps.PreCompactTurns != 14 {
		t.Fatalf("latest compaction: %v %+v", err, ps)
	}
}

func TestKiroNamesImportHistoryAndCurrentSession(t *testing.T) {
	tmp := isolateAgents(t)
	root := filepath.Join(tmp, "kiro")
	const id = "aaaaaaaa-1234-4234-8234-123456789abc"
	path := writeKiroFixture(t, root, id, "Automatic title", tmp,
		kiroRow("u1", map[string]any{"type": "user", "content": "Explore the project"}),
		kiroRow("s1", map[string]any{"type": "assistant", "operationType": "Summary", "content": "Project summary"}),
		kiroRow("u2", map[string]any{"type": "user", "content": "Continue the work"}),
		kiroRow("a2", map[string]any{"type": "assistant", "content": "Work continued"}))
	// The override can differ from the generated title in session.json.
	writeLines(t, kiroDashboardPath(path), time.Now(), `{"sess_`+id+`":{"bookmarked":true,"tags":["project"],"title":"Named Kiro Session"}}`)
	cands := discoverSessions("kiro", "")
	if len(cands) != 1 || cands[0].Title != "Named Kiro Session" || cands[0].Preview != "Continue the work" {
		t.Fatalf("discovery: %+v", cands)
	}
	if ag, q := resolveAgentAndQuery([]string{"kiro"}, "latest"); ag != "kiro" || q != "latest" {
		t.Fatalf("resolve agent: %q %q", ag, q)
	}
	if got := nativeResumeCommand(cands[0]); got != "kiro-cli --resume-id sess_"+id || agentLaunchCommand("kiro") != "kiro-cli" {
		t.Fatalf("native resume: %s", got)
	}
	if c, ok := findImportCandidate(cands, "sess_"+id); !ok || c.Path != path {
		t.Fatalf("full Kiro ID not found: %+v", c)
	}
	// These formats must not be mistaken for V3 sessions.
	writeLines(t, filepath.Join(root, "sessions", "cli", "old.jsonl"), time.Now(), `{"kind":"Prompt","data":{}}`)
	writeLines(t, filepath.Join(root, "data.sqlite3"), time.Now(), "not a V3 transcript")
	if got := discoverSessions("kiro", ""); len(got) != 1 {
		t.Fatalf("legacy formats discovered: %+v", got)
	}

	// Leave the named session outside the 15 entries shown by baton import.
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < importListLimit+1; i++ {
		writeKiroFixture(t, root, fmt.Sprintf("bbbbbbbb-1234-4234-8234-%012d", i), fmt.Sprintf("Other %d", i), tmp,
			kiroRow("other", map[string]any{"type": "user", "content": "Unrelated project"}))
	}
	report, err := importSession([]string{"Named Kiro Session", "--no-load"})
	if err != nil || !strings.Contains(report, "Imported [kiro]") {
		t.Fatalf("import by name failed: %v\n%s", err, report)
	}
	ps, err := findAndParseSession("kiro", "Named Kiro Session")
	if err != nil || ps.CompactSummary != "Project summary" || len(ps.PostCompact) != 2 {
		t.Fatalf("saved import: %v %+v", err, ps)
	}
	report, err = importSession([]string{"Named Kiro Session", "as", "kiro-loaded"})
	if err != nil || !strings.Contains(report, "source [KIRO]") || !strings.Contains(report, "Project summary") || !strings.Contains(report, "Work continued") {
		t.Fatalf("default import must save and return the active context: %v\n%s", err, report)
	}
	if report, err := historyReport([]string{"kiro-" + id[:8], "--grep", "Explore the project"}); err != nil || !strings.Contains(report, "Explore the project") {
		t.Fatalf("pre-compaction history lost: %v\n%s", err, report)
	}
	t.Setenv("KIRO_SESSION_ID", "sess_"+id)
	t.Setenv("CODEX_THREAD_ID", "inherited-parent-session")
	writeLines(t, filepath.Join(tmp, "codex", "sessions", "inherited-parent-session.jsonl"), time.Now(),
		codexRow("session_meta", map[string]any{"id": "inherited-parent-session", "cwd": tmp}),
		codexRow("response_item", codexMessage("user", "Parent agent's unrelated work")))
	if detectCallerAgent() != "kiro" || !isCallerOwnSession(path) {
		t.Fatal("Kiro's current session not recognized")
	}
	ps, err = parseCurrentSession()
	if err != nil || ps.SourceFile != path || userSessionName("kiro", path) != "Named Kiro Session" || defaultSaveName(ps) != "kiro-loaded" {
		t.Fatalf("current session/save name: %v %+v", err, ps)
	}
	t.Setenv("KIRO_SESSION_ID", "sess_missing")
	if _, err := parseCurrentSession(); err == nil {
		t.Fatal("missing current session must not fall back to another session")
	}
}

func TestKiroRenamePreservesMetadataAndLocks(t *testing.T) {
	tmp := isolateAgents(t)
	const id = "12345678-1234-4234-8234-123456789abc"
	path := writeKiroFixture(t, filepath.Join(tmp, "kiro"), id, "Automatic title", tmp,
		kiroRow("u1", map[string]any{"type": "user", "content": "Read project"}))
	dashboard := kiroDashboardPath(path)
	before, _ := os.ReadFile(filepath.Join(filepath.Dir(path), "session.json"))
	writeLines(t, dashboard, time.Now(), `{"`+id+`":{"bookmarked":true,"tags":["keep"],"unknown":42},"other":{"title":"Other session"}}`)
	c := SessionCandidate{Agent: "kiro", Path: path}
	if note := renameAgentSession(c, "loaded-name", ""); !strings.Contains(note, "loaded-name") || kiroUserTitle(path) != "loaded-name" {
		t.Fatalf("rename failed: %s", note)
	}
	data, _ := os.ReadFile(dashboard)
	for _, want := range []string{`"unknown": 42`, `"keep"`, `"bookmarked": true`, `"Other session"`} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("lost metadata %s in %s", want, data)
		}
	}
	after, _ := os.ReadFile(filepath.Join(filepath.Dir(path), "session.json"))
	if string(before) != string(after) {
		t.Fatal("rename must not write the live session metadata")
	}
	if renameKiroSession(path, "overwrite-user-name", "") || !renameKiroSession(path, "new-loaded-name", "loaded-name") {
		t.Fatal("user titles must be kept; a previous Baton title may be replaced")
	}
	if err := os.Mkdir(dashboard+".lock", 0700); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(dashboard)
	if renameKiroSession(path, "locked", "new-loaded-name") {
		t.Fatal("must respect Kiro's active metadata lock")
	}
	after, _ = os.ReadFile(dashboard)
	if string(data) != string(after) {
		t.Fatal("locked metadata was changed")
	}
}

func TestKiroInstallAndProcessDetection(t *testing.T) {
	tmp := isolateAgents(t)
	if err := runInstall(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(tmp, "kiro", "skills", "baton", "SKILL.md"))
	if err != nil || string(data) != skillMarkdownContent {
		t.Fatalf("Kiro skill not installed: %v", err)
	}
	for cmd, want := range map[string]bool{
		"/home/u/.local/bin/kiro-cli --v3":                 true,
		"/home/u/.local/share/kiro-cli/run/chat-cli-3.4.0": true,
		"bun /home/u/.local/share/kiro-cli/tui.js":         true,
		"vim /home/u/notes/kiro-cli":                       false,
		"sh -c kiro-cli --version":                         false,
	} {
		if got := isKiroCmdline(cmd); got != want {
			t.Errorf("isKiroCmdline(%q)=%v, want %v", cmd, got, want)
		}
	}
}

func TestKiroCheckpointRevert(t *testing.T) {
	tmp := isolateAgents(t)
	path := writeKiroFixture(t, filepath.Join(tmp, "kiro"), "rewind", "Rewind", tmp,
		kiroRow("u1", map[string]any{"type": "user", "content": "Keep this"}),
		kiroRow("u2", map[string]any{"type": "user", "content": "Revert this"}),
		kiroRow("s1", map[string]any{"type": "assistant", "operationType": "Summary", "content": "Discarded branch summary"}),
		kiroRow("revert", map[string]any{"type": "tombstone", "kind": "checkpoint_revert", "effectiveFromMessageId": "u2"}),
		kiroRow("u3", map[string]any{"type": "user", "content": "Replacement branch"}))
	ps, err := parseSessionFile("kiro", path, time.Now())
	if err != nil || ps.CompactSummary != "" || len(ps.AllTurns) != 3 || len(ps.PostCompact) != 2 || ps.PostCompact[0].Text != "Keep this" || ps.PostCompact[1].Text != "Replacement branch" {
		t.Fatalf("reverted branch in active context: %v %+v", err, ps)
	}
}
