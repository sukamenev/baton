package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func codexRow(typ string, payload map[string]any) string {
	row, _ := json.Marshal(map[string]any{"timestamp": "2026-11-10T18:00:00Z", "type": typ, "payload": payload})
	return string(row)
}

func codexMessage(role, text string) map[string]any {
	kind := "input_text"
	if role == "assistant" {
		kind = "output_text"
	}
	return map[string]any{"type": "message", "role": role, "content": []any{map[string]any{"type": kind, "text": text}}}
}

// writeCompactedCodexSession writes a Codex rollout compacted on OpenAI's
// servers: `message` is empty and the summary is an encrypted item.
func writeCompactedCodexSession(t *testing.T, tmp, cwd string) (string, string) {
	const id = "01a1c6b0-06c4-7300-a866-a023062ad02f"
	path := filepath.Join(tmp, "codex", "sessions", "2026", "11", "10", "rollout-2026-11-10T21-19-47-"+id+".jsonl")
	writeLines(t, path, time.Now(),
		codexRow("session_meta", map[string]any{"id": id, "cwd": cwd}),
		codexRow("response_item", codexMessage("user", "Count the shovels per device")),
		codexRow("response_item", map[string]any{"type": "function_call", "name": "exec_command", "arguments": `{"cmd":"go test ./..."}`}),
		codexRow("response_item", map[string]any{"type": "function_call_output", "output": "ok shovel/accounting\n" + strings.Repeat("PASS line\n", 40) + "the rgox.sh build is fine"}),
		codexRow("response_item", codexMessage("assistant", "Committed acebf2042c with two tests.")),
		codexRow("compacted", map[string]any{"message": "", "replacement_history": []any{
			codexMessage("user", "# AGENTS.md instructions\n\n<INSTRUCTIONS>rules</INSTRUCTIONS>"),
			codexMessage("user", "Count the shovels per device"),
			map[string]any{"type": "compaction", "encrypted_content": "gAAAAABq82cA"},
		}}),
		codexRow("response_item", codexMessage("user", "What went into 0.9.5?")),
		codexRow("response_item", codexMessage("assistant", "The accounting changes.")),
	)
	return path, "codex-rollout-2026-11-10T21-19-47-" + id
}

func TestCodexServerCompaction(t *testing.T) {
	tmp := isolateAgents(t)
	path, _ := writeCompactedCodexSession(t, tmp, "/work/proj")
	ps, err := parseSessionFile("codex", path, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if ps.PreCompactTurns != 4 || len(ps.PostCompact) != 2 || len(ps.AllTurns) != 6 {
		t.Fatalf("compaction boundary: pre=%d post=%d all=%d", ps.PreCompactTurns, len(ps.PostCompact), len(ps.AllTurns))
	}
	for _, want := range []string{"summary is encrypted", "1. Count the shovels per device", "Committed acebf2042c", "(turn #4)"} {
		if !strings.Contains(ps.CompactSummary, want) {
			t.Fatalf("summary misses %q:\n%s", want, ps.CompactSummary)
		}
	}
	if strings.Contains(ps.CompactSummary, "AGENTS.md") {
		t.Fatalf("injected instructions must not be in the summary:\n%s", ps.CompactSummary)
	}
}

func TestHistory(t *testing.T) {
	tmp := isolateAgents(t)
	_, sid := writeCompactedCodexSession(t, tmp, "/work/proj")

	// From the agent's own log: also the turns before the compaction.
	idx, err := historyReport([]string{sid})
	if err != nil || !strings.Contains(idx, "#1 Count the shovels per device") ||
		!strings.Contains(idx, "--- latest compaction after turn #4 ---") || !strings.Contains(idx, "#5 What went into 0.9.5?") {
		t.Fatalf("index: %v\n%s", err, idx)
	}
	one, err := historyReport([]string{sid, "3"})
	if err != nil || !strings.Contains(one, "the rgox.sh build is fine") || strings.Contains(one, "collapsed") {
		t.Fatalf("turn 3 must be printed in full: %v\n%s", err, one)
	}
	rng, err := historyReport([]string{sid, "2", "3"})
	if err != nil || !strings.Contains(rng, "[Turn #2]") || !strings.Contains(rng, "collapsed") || strings.Contains(rng, "[Turn #4]") {
		t.Fatalf("range: %v\n%s", err, rng)
	}
	found, err := historyReport([]string{"01a1c6b0", "--grep", "RGOX.SH"})
	if err != nil || !strings.Contains(found, "#3 tool_result") {
		t.Fatalf("grep by ID prefix: %v\n%s", err, found)
	}
	// The old command name still works.
	if old, err := historyReport([]string{sid, "5"}); err != nil || !strings.Contains(old, "What went into 0.9.5?") {
		t.Fatalf("turn 5: %v\n%s", err, old)
	}

	// Saved, then the agent's log is gone: the snapshot keeps the turns after
	// the compaction only, and says so.
	ps, err := resolveHistorySession(sid)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := saveSessionToBatonStore(ps, "mega"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(ps.SourceFile); err != nil {
		t.Fatal(err)
	}
	if r, err := historyReport([]string{"mega", "5"}); err != nil || !strings.Contains(r, "What went into 0.9.5?") {
		t.Fatalf("snapshot turn 5: %v\n%s", err, r)
	}
	if _, err := historyReport([]string{"mega", "1"}); err == nil || !strings.Contains(err.Error(), "not on this machine") {
		t.Fatalf("expected a note that earlier turns are only in the agent's log, got %v", err)
	}
}
