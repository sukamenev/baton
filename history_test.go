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
	for _, want := range []string{"summary is encrypted", "1. Count the shovels per device",
		"### [Turn #4] assistant\nCommitted acebf2042c", "- [Turn #2] Call `exec_command`", "- [Turn #3] output: ok shovel/accounting …"} {
		if !strings.Contains(ps.CompactSummary, want) {
			t.Fatalf("summary misses %q:\n%s", want, ps.CompactSummary)
		}
	}
	if strings.Contains(ps.CompactSummary, "AGENTS.md") {
		t.Fatalf("injected instructions must not be in the summary:\n%s", ps.CompactSummary)
	}
}

// The conversation before an unreadable compaction fits the budget, keeps the
// last assistant reply, and shortens tool output to one line.
func TestCompactionTailBudget(t *testing.T) {
	var turns []SessionTurn
	for i := 1; i <= 200; i++ {
		turns = append(turns,
			SessionTurn{TurnID: 3*i - 2, Role: "user", Text: fmt.Sprintf("task %d", i)},
			SessionTurn{TurnID: 3*i - 1, Role: "tool_result", Text: "first line\n" + strings.Repeat("x", 2000)},
			SessionTurn{TurnID: 3 * i, Role: "assistant", Text: strings.Repeat("answer ", 100)})
	}
	tail := compactionTail(turns, 32<<10)
	if len(tail) > 33<<10 || !strings.Contains(tail, "[Turn #600] assistant") || !strings.Contains(tail, "[Turn #599] output: first line …") || strings.Contains(tail, "xxx") {
		t.Fatalf("tail of %d bytes:\n%.600s", len(tail), tail)
	}
	if strings.Contains(tail, "[Turn #1] user") {
		t.Fatal("the budget must stop the tail")
	}
	// One huge last reply is shortened, not dropped.
	huge := []SessionTurn{{TurnID: 1, Role: "assistant", Text: "BEGIN " + strings.Repeat("y", 100<<10) + " END"}}
	if tail := compactionTail(huge, 32<<10); len(tail) > 33<<10 || !strings.Contains(tail, "BEGIN") || !strings.Contains(tail, "END") || !strings.Contains(tail, "shortened") {
		t.Fatalf("huge reply: %d bytes", len(tail))
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
	// Two Codex sessions started in the same minute share the first characters.
	const id2 = "01a1c6b0-ffff-7300-a866-a023062ad02f"
	writeLines(t, filepath.Join(tmp, "codex", "sessions", "2026", "11", "10", "rollout-2026-11-10T21-19-50-"+id2+".jsonl"), time.Now(),
		codexRow("session_meta", map[string]any{"id": id2, "cwd": "/work/proj"}),
		codexRow("response_item", codexMessage("user", "Another task")))
	if _, err := historyReport([]string{"codex-01a1c6b0"}); err == nil || !strings.Contains(err.Error(), "2 sessions start with") || !strings.Contains(err.Error(), id2) {
		t.Fatalf("expected an ambiguity error listing both, got %v", err)
	}
	if r, err := historyReport([]string{"01a1c6b0-ffff"}); err != nil || !strings.Contains(r, "Another task") {
		t.Fatalf("a longer prefix must pick one: %v\n%s", err, r)
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

func TestFirstOutputLine(t *testing.T) {
	for in, want := range map[string]string{
		"Script completed\nWall time 0.1 seconds\nOutput:\n\n---\nname: baton\nmore":                             "name: baton …",
		"Script completed\nOutput:\n" + `{"chunk_id":"34c4","exit_code":0,"output":"prod/ latest: 0.9.5\ndev/"}`: "prod/ latest: 0.9.5 …",
		`{"chunk_id":"2e18","output":""}`: "(no output)",
		"ok shovel":                       "ok shovel",
	} {
		if got := firstOutputLine(in); got != want {
			t.Errorf("firstOutputLine(%q) = %q, want %q", in, got, want)
		}
	}
}

// In a Codex session compacted on the server, save keeps the readable part and
// asks for a recap; with --summary the recap becomes the summary.
func TestCodexSaveAsksForRecap(t *testing.T) {
	tmp := isolateAgents(t)
	proj := filepath.Join(tmp, "proj")
	_ = os.MkdirAll(proj, 0755)
	chdirTemp(t, proj)
	cwd, _ := os.Getwd()
	_, _ = writeCompactedCodexSession(t, tmp, cwd)
	t.Setenv("CODEX_THREAD_ID", "01a1c6b0-06c4-7300-a866-a023062ad02f")
	t.Cleanup(func() { recapFile = "" })
	snapshot := func() *ParsedSession {
		ps, err := parseSessionFile("codex", filepath.Join(savedSessionsDir(), "mega.baton.json"), time.Now())
		if err != nil {
			t.Fatal(err)
		}
		return ps
	}

	msg, err := nameSession("mega")
	if err != nil || !strings.Contains(msg, "baton save mega --summary ") || !strings.Contains(msg, "earlier recap") {
		t.Fatalf("expected a saved session and a recap request: %v\n%s", err, msg)
	}
	if ps := snapshot(); !ps.SummaryEncrypted || !strings.Contains(ps.CompactSummary, "summary is encrypted") {
		t.Fatalf("the stand-in must be saved: %+v", ps.CompactSummary)
	}

	recapFile = filepath.Join(tmp, "recap.md")
	if err := os.WriteFile(recapFile, []byte("Goals: count shovels per device. Commit acebf2042c."), 0600); err != nil {
		t.Fatal(err)
	}
	msg, err = nameSession("mega")
	if err != nil || strings.Contains(msg, "--summary") {
		t.Fatalf("no request once the recap is given: %v\n%s", err, msg)
	}
	if ps := snapshot(); ps.SummaryEncrypted || !strings.Contains(ps.CompactSummary, "Commit acebf2042c") {
		t.Fatalf("the recap must be the summary: %+v", ps.CompactSummary)
	}

	// A recap for a session without an encrypted compaction is not used.
	if note, err := applyRecap(&ParsedSession{Agent: "codex"}, "baton save x", false); err != nil || !strings.Contains(note, "not used") {
		t.Fatalf("note %q, err %v", note, err)
	}
}

// push sends nothing from a Codex session with an encrypted summary until it
// has the recap, so only one link goes out.
func TestCodexPushWaitsForRecap(t *testing.T) {
	tmp := isolateAgents(t)
	proj := filepath.Join(tmp, "proj")
	_ = os.MkdirAll(proj, 0755)
	chdirTemp(t, proj)
	cwd, _ := os.Getwd()
	_, _ = writeCompactedCodexSession(t, tmp, cwd)
	t.Setenv("CODEX_THREAD_ID", "01a1c6b0-06c4-7300-a866-a023062ad02f")
	t.Cleanup(func() { recapFile = "" })
	vault := filepath.Join(tmp, "vault")

	if _, err := pushRemoteSession("mega", "local_dir", vault); err == nil || !strings.Contains(err.Error(), "baton push mega --summary ") {
		t.Fatalf("expected a recap request, got %v", err)
	}
	if entries, _ := os.ReadDir(vault); len(entries) != 0 {
		t.Fatal("nothing may be sent before the recap")
	}
	recapFile = filepath.Join(tmp, "recap.md")
	_ = os.WriteFile(recapFile, []byte("Goals: count shovels per device."), 0600)
	if r, err := pushRemoteSession("mega", "local_dir", vault); err != nil || r.Token == "" {
		t.Fatalf("push with the recap: %v", err)
	}
}
