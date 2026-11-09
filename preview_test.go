package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeClaudeRows(t *testing.T, rows ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "11111111-2222-3333-4444-555555555555.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(rows, "\n")+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLastPromptSkipsMetaAndFallsBackBeforeCompaction(t *testing.T) {
	path := writeClaudeRows(t,
		`{"type":"user","cwd":"/w","message":{"role":"user","content":"Fix the parser"}}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Done"}]}}`,
		`{"type":"user","isCompactSummary":true,"message":{"role":"user","content":"Summary of the work"}}`,
		`{"type":"system","subtype":"local_command","content":"<command-name>/context</command-name>"}`,
		`{"type":"user","isMeta":true,"message":{"role":"user","content":"## Context Usage\n\n**Tokens:** 1k"}}`,
		`{"type":"user","message":{"role":"user","content":"baton save work"}}`,
	)
	ps, err := parseSessionFile("claude", path, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if ps.PreCompactTurns != 2 || len(ps.PostCompact) != 2 {
		t.Fatalf("unexpected compaction split: pre=%d post=%+v", ps.PreCompactTurns, ps.PostCompact)
	}
	if !ps.PostCompact[0].Meta || ps.PostCompact[1].Meta {
		t.Fatalf("isMeta not carried to turns: %+v", ps.PostCompact)
	}
	if got := sessionLastPrompt(ps); got != "Fix the parser" {
		t.Fatalf("expected the prompt before compaction, got %q", got)
	}

	// The preview survives saving (snapshots drop pre-compaction turns) and re-saving.
	t.Setenv("BATON_CONFIG_DIR", t.TempDir())
	savePath, _, err := saveSessionToBatonStore(ps, "work")
	if err != nil {
		t.Fatal(err)
	}
	saved, err := parseSessionFile("claude", savePath, time.Now())
	if err != nil || saved.LastPrompt != "Fix the parser" {
		t.Fatalf("saved preview: %v %q", err, saved.LastPrompt)
	}
	if _, _, err := saveSessionToBatonStore(saved, "work"); err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, c := range discoverSavedBatonSessions("all", "") {
		if c.Title == "work" {
			found = true
			if c.Preview != "Fix the parser" {
				t.Fatalf("list preview after re-save: %q", c.Preview)
			}
		}
	}
	if !found {
		t.Fatal("saved session not listed")
	}
}

func TestLastPromptPrefersPromptAfterCompaction(t *testing.T) {
	path := writeClaudeRows(t,
		`{"type":"user","message":{"role":"user","content":"Old task"}}`,
		`{"type":"user","isCompactSummary":true,"message":{"role":"user","content":"Summary"}}`,
		`{"type":"user","message":{"role":"user","content":"New task"}}`,
		`{"type":"user","isMeta":true,"message":{"role":"user","content":"Usage notice"}}`,
	)
	ps, err := parseSessionFile("claude", path, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if got := sessionLastPrompt(ps); got != "New task" {
		t.Fatalf("got %q", got)
	}
}

func TestImportHidesSessionsWithoutMessages(t *testing.T) {
	empty := writeClaudeRows(t,
		`{"type":"mode","mode":"default"}`,
		`{"type":"system","subtype":"init"}`,
		`{"type":"cost-state"}`,
	)
	real := writeClaudeRows(t, `{"type":"user","message":{"role":"user","content":"Hello"}}`)
	cands := []SessionCandidate{{Agent: "claude", Path: empty}, {Agent: "claude", Path: real}}
	cache := map[string]importMeta{}
	// A cache entry from older rules must not keep the empty session visible.
	mt, sz := candidateFingerprint(cands[0])
	cache[empty] = importMeta{ModTime: mt, Bytes: sz}
	metas := measureCandidates(cands, cache, map[string]importMeta{})
	if !metas[0].Stub {
		t.Fatalf("empty session not hidden: %+v", metas[0])
	}
	if metas[1].Stub || metas[1].Context == 0 {
		t.Fatalf("real session hidden or unmeasured: %+v", metas[1])
	}
}

// A message typed while Claude Code was working is logged as a queued_command
// attachment; the wrapped copy in api-request-blob rows is not a turn.
func TestClaudeQueuedPrompts(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.jsonl")
	lines := []string{
		`{"type":"user","cwd":"/w","message":{"role":"user","content":"Start the task"}}`,
		`{"type":"attachment","attachment":{"type":"queued_command","commandMode":"prompt","origin":{"kind":"human"},"prompt":"Use this computer as the reference"}}`,
		`{"type":"attachment","attachment":{"type":"queued_command","commandMode":"task-notification","prompt":"<task-notification>done</task-notification>"}}`,
		`{"type":"attachment","attachment":{"type":"queued_command","commandMode":"prompt","origin":{"kind":"peer","from":"a1"},"prompt":"Message from another agent"}}`,
		`{"type":"api-request-blob","message":{"role":"user","content":[{"type":"text","text":"The user sent a new message while you were working:\nUse this computer as the reference\n\nThis is how Claude Code surfaces messages the user sends mid-turn."}]}}`,
		`{"type":"user","message":{"role":"user","content":"Use this computer as the reference"}}`,
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	ps, err := parseSessionFile("claude", path, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	var users []string
	for _, turn := range ps.AllTurns {
		if turn.Role == "user" {
			users = append(users, turn.Text)
		}
	}
	if strings.Join(users, "|") != "Start the task|Use this computer as the reference" {
		t.Fatalf("unexpected user turns: %q", users)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if got := lastUserPromptInTail(f, 1<<20); got != "Use this computer as the reference" {
		t.Fatalf("preview must be the typed text, got %q", got)
	}
}

// A stray backtick or tilde in a prompt must not open a span that swallows
// the following rows of the list.
func TestPreviewMarkdownIsEscaped(t *testing.T) {
	var sb strings.Builder
	writeCandidateRows(&sb, []SessionCandidate{
		{Agent: "claude", Title: "a", Path: "/x/a.jsonl", CWD: "/w", Preview: "j,`hnre ~/dir *x* _y_ [l](u) <b> c:\\d"},
		{Agent: "claude", Title: "b", Path: "/x/b.jsonl", CWD: "/w2"},
	})
	got := sb.String()
	want := "`j,'hnre ~/dir *x* _y_ [l](u) <b> c:\\d`"
	if !strings.Contains(got, want) {
		t.Fatalf("preview not escaped:\n%s", got)
	}
	if !strings.Contains(got, "`/w2`") {
		t.Fatalf("folder lines keep their code spans:\n%s", got)
	}
}

func TestCompactSummaryIsNotAPrompt(t *testing.T) {
	row := map[string]interface{}{"type": "user", "isCompactSummary": true,
		"message": map[string]interface{}{"role": "user", "content": "This session is being continued from a previous conversation"}}
	if got := userPromptFromRow(row); got != "" {
		t.Fatalf("the /compact summary is not a prompt, got %q", got)
	}
}

func TestSlashCommandIsNotAPrompt(t *testing.T) {
	cases := map[string]string{
		"/compact":          "",
		"/context":          "",
		"/mnt/data is full": "/mnt/data is full",
		"/mnt/data":         "/mnt/data",
	}
	for in, want := range cases {
		if got := cleanUserPrompt(in); got != want {
			t.Errorf("%q: got %q, want %q", in, got, want)
		}
	}
}

func TestPastedTextInPreview(t *testing.T) {
	cases := map[string]string{
		"Looks like an extra slash:\n\n\n\n<pasted_content id=\"253d\">\n> baton import\n  #1 ...\n</pasted_content id=\"253d\">": "Looks like an extra slash: [+ pasted text]",
		"<pasted_content id=\"1\">\nError: build failed\n   at main.go:12\n</pasted_content id=\"1\">":                            "[pasted] Error: build failed at main.go:12",
		"See this:\n<pasted_content id=\"2\">\nunterminated":                                                                      "See this: [+ pasted text]",
		"Two  spaces\n\nand   lines": "Two spaces and lines",
	}
	for in, want := range cases {
		if got := cleanUserPrompt(in); got != want {
			t.Errorf("%q:\n got %q\nwant %q", in, got, want)
		}
	}
}
