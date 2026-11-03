package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCursorCompactionRetainsUnsummarizedHistory(t *testing.T) {
	for _, tc := range []struct{ name, segment string }{
		{"verbatim", "## Verbatim turns\n\n### Turn 0 (Human)\nKeep this later request\n"},
		{"unstructured", "Keep this later request\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			t.Setenv("CURSOR_HOME", root)
			path := filepath.Join(root, "session.jsonl")
			compaction := filepath.Join(root, "compaction")
			if err := os.MkdirAll(compaction, 0755); err != nil {
				t.Fatal(err)
			}
			files := map[string]string{
				path: `{"role":"user","message":{"content":"Newest request"}}` + "\n",
				filepath.Join(compaction, "segment_000.md"): "## Summary (curated by compaction step)\n\nFirst summary\n\n## Verbatim turns\n\n### Turn 0 (Human)\nEarlier request\n",
				filepath.Join(compaction, "segment_001.md"): tc.segment,
			}
			for p, body := range files {
				if err := os.WriteFile(p, []byte(body), 0644); err != nil {
					t.Fatal(err)
				}
			}
			ps, err := parseSessionFile("cursor", path, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			if ps.PreCompactTurns != 1 || ps.CompactSummary != "First summary" || len(ps.PostCompact) != 2 {
				t.Fatalf("wrong checkpoint: pre=%d summary=%q tail=%+v", ps.PreCompactTurns, ps.CompactSummary, ps.PostCompact)
			}
			if !strings.Contains(ps.PostCompact[0].Text, "Keep this later request") {
				t.Fatalf("lost tail: %+v", ps.PostCompact)
			}
			if len(ps.AllTurns) != 3 || !strings.Contains(ps.AllTurns[0].Text, "Earlier request") {
				t.Fatalf("lost history: %+v", ps.AllTurns)
			}
			// With no curated summary, every segment stays in active context.
			if err := os.WriteFile(filepath.Join(compaction, "segment_000.md"), []byte("Earlier request\n"), 0644); err != nil {
				t.Fatal(err)
			}
			ps, err = parseSessionFile("cursor", path, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			if ps.PreCompactTurns != 0 || len(ps.PostCompact) != 3 {
				t.Fatalf("unsummarized history lost: %+v", ps)
			}
			// A newer successful compaction covers both older segments.
			if err := os.WriteFile(filepath.Join(compaction, "segment_002.md"), []byte("## Summary (curated by compaction step)\n\nLatest summary\n"), 0644); err != nil {
				t.Fatal(err)
			}
			ps, err = parseSessionFile("cursor", path, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			if ps.PreCompactTurns != 3 || ps.CompactSummary != "Latest summary" || len(ps.PostCompact) != 1 {
				t.Fatalf("latest checkpoint not used: %+v", ps)
			}
		})
	}
}

func TestCursorUnreadableCompactionFails(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CURSOR_HOME", root)
	path := filepath.Join(root, "session.jsonl")
	if err := os.WriteFile(path, []byte("{}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	// A directory with a segment filename produces a portable read error.
	if err := os.MkdirAll(filepath.Join(root, "compaction", "segment_000.md"), 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := parseSessionFile("cursor", path, time.Now()); err == nil {
		t.Fatal("unreadable history silently ignored")
	}
}
