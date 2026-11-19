package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestImportLookupAcrossWeeksAndAgents(t *testing.T) {
	tmp := isolateAgents(t)
	now := time.Now()
	claudeDir := filepath.Join(tmp, "claude", "projects", "fixture")
	claudePath := filepath.Join(claudeDir, "abcdef12-1234-4234-8234-123456789abc.jsonl")
	// The name was never saved in Baton and comes after a long tool result.
	writeLines(t, claudePath, now.AddDate(0, 0, -10),
		claudeUser("Investigate the checkout"),
		strings.Repeat("x", 3*importTitleWindow),
		`{"type":"custom-title","customTitle":"Checkout"}`)
	codexDir := filepath.Join(tmp, "codex")
	const codexID = "fedcba98-1234-4234-8234-123456789abc"
	writeLines(t, filepath.Join(codexDir, "session_index.jsonl"), now,
		`{"id":"`+codexID+`","thread_name":"Checkout"}`)
	codexPath := filepath.Join(codexDir, "sessions", "rollout-fixture-"+codexID+".jsonl")
	writeLines(t, codexPath, now.AddDate(0, 0, -9),
		codexRow("response_item", codexMessage("user", "Use the newer checkout")))
	fragment := writeKiroFixture(t, filepath.Join(tmp, "kiro"), "fragment", "Checkout follow-up", tmp,
		kiroRow("user", map[string]any{"type": "user", "content": "Review the later change"}))
	if err := os.Chtimes(fragment, now, now); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(claudeDir, "12345678-1234-4234-8234-123456789abc.jsonl")
	writeLines(t, archive, now.AddDate(0, 0, -75), claudeUser("An archive prompt"),
		`{"type":"custom-title","customTitle":"Archive Session"}`)
	another := filepath.Join(claudeDir, "23456789-1234-4234-8234-123456789abc.jsonl")
	writeLines(t, another, now.AddDate(0, 0, -20), claudeUser("Checkout only appears in the conversation"),
		`{"type":"custom-title","customTitle":"Archive Notes"}`)
	if err := saveAliases(map[string]string{claudePath: "Older Baton alias"}); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		query string
		days  int
		path  string
	}{
		{"checkout", 60, codexPath},
		{"follow-up", 60, ""},
		{"Checkout follow-up", 60, fragment},
		{"Older Baton alias", 60, claudePath},
		{"Archive Session", 60, ""},
		{"Archive Session", 90, archive},
		{"12345678", 60, archive},
		{"only appears", 60, ""},
		{"Archive", 90, ""},
	} {
		c, ps, err := lookupImport(tc.query, tc.days, now)
		if err != nil || c.Path != tc.path || (ps == nil) != (tc.path == "") {
			t.Fatalf("lookup %q within %d days: path=%q, want %q, err=%v", tc.query, tc.days, c.Path, tc.path, err)
		}
	}
	for _, args := range [][]string{{"12345678", "--no-load"}, {"Archive Session", "--days=90", "--no-load"}} {
		if report, err := importSession(args); err != nil || !strings.Contains(report, "Imported [claude] session 12345678") {
			t.Fatalf("import %v: %q, %v", args, report, err)
		}
	}
}

func TestImportWorkerKeepsPositionAndRepliesWhenExhausted(t *testing.T) {
	tmp := isolateAgents(t)
	now := time.Now()
	path := filepath.Join(tmp, "claude", "projects", "fixture", "session.jsonl")
	writeLines(t, path, now, claudeUser("Inspect the project"), `{"type":"custom-title","customTitle":"Project"}`)
	worker := importAgentSearch{since: make(chan time.Time), reply: make(chan importAgentReply, 1)}
	done := make(chan struct{})
	go func() {
		defer close(done)
		searchImportAgent("claude", worker, nil, "Project")
	}()
	t.Cleanup(func() {
		close(worker.since)
		<-done
	})
	if initial := <-worker.reply; initial.next.IsZero() {
		t.Fatal("worker did not discover the session")
	}
	worker.since <- now.AddDate(0, 0, -7)
	if reply := <-worker.reply; len(reply.matches) != 1 || !reply.next.IsZero() {
		t.Fatalf("first week should consume the only candidate: %+v", reply)
	}
	writeLines(t, filepath.Join(filepath.Dir(path), "another.jsonl"), now, `{"type":"custom-title","customTitle":"Project Two"}`)
	worker.since <- now.AddDate(0, 0, -14)
	if reply := <-worker.reply; len(reply.matches) != 0 || !reply.next.IsZero() {
		t.Fatalf("exhausted worker re-enumerated or returned duplicate candidates: %+v", reply)
	}
}
