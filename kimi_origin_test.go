package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Fixture rows follow Kimi Code's history entry schema
// (packages/agent-core-v2/src/human/agent/historySchema.ts): the prompt origin
// lives in the entry's meta, not inside the message.
func TestKimiOriginsAndInjectedParts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wire.jsonl")
	lines := []string{
		`{"type":"agent.message.appended","time":1,"message":{"message":{"role":"user","content":[{"type":"text","text":"Fix the build"},{"type":"text","text":"SKILL BODY","meta":{"source":"skill activation","activationId":"a1"}}]},"meta":{"origin":{"kind":"user"}}}}`,
		`{"type":"agent.message.appended","time":2,"message":{"message":{"role":"assistant","content":[{"type":"text","text":"Building"}],"toolCalls":[]}}}`,
		`{"type":"agent.message.appended","time":3,"message":{"message":{"role":"user","content":[{"type":"text","text":"<system-reminder>injected</system-reminder>"}]},"meta":{"origin":{"kind":"injection"}}}}`,
		`{"type":"agent.message.appended","time":4,"message":{"message":{"role":"user","content":[{"type":"text","text":"Task 7 finished"}]},"meta":{"origin":{"kind":"task","taskId":"7"}}}}`,
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	ps, err := parseSessionFile("kimi", path, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	var texts []string
	for _, turn := range ps.AllTurns {
		texts = append(texts, turn.Text)
		if strings.Contains(turn.Text, "injected") {
			t.Fatalf("hidden injection imported: %+v", turn)
		}
	}
	if len(ps.AllTurns) != 4 {
		t.Fatalf("expected prompt, skill body, answer, task notification; got %q", texts)
	}
	if ps.AllTurns[0].Meta || !ps.AllTurns[1].Meta || !ps.AllTurns[3].Meta {
		t.Fatalf("meta flags: %+v", ps.AllTurns)
	}
	if got := sessionLastPrompt(ps); got != "Fix the build" {
		t.Fatalf("last prompt %q", got)
	}

	// The quick preview of raw sessions applies the same rules.
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if got := lastUserPromptInTail(f, 1<<20); got != "Fix the build" {
		t.Fatalf("tail preview %q", got)
	}
}

func TestKimiClearStartsNewContext(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wire.jsonl")
	lines := []string{
		`{"type":"agent.message.appended","message":{"message":{"role":"user","content":[{"type":"text","text":"Old topic"}]},"meta":{"origin":{"kind":"user"}}}}`,
		`{"type":"context.clear","agentId":"main"}`,
		`{"type":"agent.message.appended","message":{"message":{"role":"user","content":[{"type":"text","text":"New topic"}]},"meta":{"origin":{"kind":"user"}}}}`,
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0600); err != nil {
		t.Fatal(err)
	}
	ps, err := parseSessionFile("kimi", path, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(ps.AllTurns) != 2 || len(ps.PostCompact) != 1 || ps.PostCompact[0].Text != "New topic" || ps.PreCompactTurns != 1 {
		t.Fatalf("clear not applied: %+v", ps)
	}
}

func TestCallerCommandLines(t *testing.T) {
	for _, tc := range []struct {
		cmd   string
		kimi  bool
		shell bool
	}{
		{"node /home/u/.npm-global/bin/kimi ", true, false},
		{"/usr/local/bin/kimi --continue ", true, false},
		{"node /usr/lib/node_modules/@moonshot-ai/kimi-code/dist/main.mjs ", true, false},
		{"sh -c cd '/home/u/claude-tools' && baton save ", false, true},
		{"/bin/bash -lc baton save ", false, true},
		{"/bin/bash --rcfile x ", false, false},
		{"node /usr/bin/claude ", false, false},
		{"vim /home/u/notes/kimi ", false, false},
	} {
		if got := isKimiCmdline(tc.cmd); got != tc.kimi {
			t.Errorf("isKimiCmdline(%q) = %v", tc.cmd, got)
		}
		if got := isShellCommandLine(tc.cmd); got != tc.shell {
			t.Errorf("isShellCommandLine(%q) = %v", tc.cmd, got)
		}
	}
}
