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
