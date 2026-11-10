package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A session that began with a baton command is still the current one: save and
// push used to skip it as a "handoff stub" and take an older session in the
// same folder.
func TestCurrentSessionIsNotSkippedAsStub(t *testing.T) {
	tmp := isolateAgents(t)
	proj := filepath.Join(tmp, "proj")
	if err := os.MkdirAll(proj, 0755); err != nil {
		t.Fatal(err)
	}
	chdirTemp(t, proj)
	cwd, _ := os.Getwd()
	user := func(text string) string {
		row, _ := json.Marshal(map[string]any{
			"type": "user", "cwd": cwd,
			"message": map[string]any{"role": "user", "content": text},
		})
		return string(row)
	}
	projects := filepath.Join(tmp, "claude", "projects", "-proj")
	const idOld = "aaaaaaaa-1111-4111-8111-111111111111"
	const idCur = "bbbbbbbb-2222-4222-8222-222222222222"
	now := time.Now()
	writeLines(t, filepath.Join(projects, idOld+".jsonl"), now.Add(-time.Hour),
		user("Install baton from source"))
	writeLines(t, filepath.Join(projects, idCur+".jsonl"), now,
		user("baton resume research"), user("baton import 4"), user("Compress the attention weights"), user("baton save research"))

	t.Setenv("CLAUDECODE", "1")
	for _, id := range []string{idCur, ""} {
		t.Setenv("CLAUDE_CODE_SESSION_ID", id)
		ps, err := parseCurrentSession()
		if err != nil || !strings.Contains(ps.SourceFile, idCur) {
			t.Fatalf("session ID %q: expected the current session, got %v %+v", id, err, ps)
		}
	}

	// The agent named its session, but it is not on disk: take nothing else.
	t.Setenv("CLAUDE_CODE_SESSION_ID", "cccccccc-3333-4333-8333-333333333333")
	if ps, err := parseCurrentSession(); err == nil {
		t.Fatalf("expected an error, got %s", ps.SourceFile)
	}

	// The save report names the session and its last real prompt.
	t.Setenv("CLAUDE_CODE_SESSION_ID", idCur)
	msg, err := nameSession("research")
	if err != nil || !strings.Contains(msg, "[claude] session bbbbbbbb") || !strings.Contains(msg, "`Compress the attention weights`") {
		t.Fatalf("save report must name the session: %v\n%s", err, msg)
	}
}

func TestPushSummary(t *testing.T) {
	s := pushSummary(pushReport{Source: "[claude] session bbbbbbbb", Context: 30 << 10, FullContext: 3 << 20,
		Files: 2, FilesSize: 12 << 10, Rules: 1, Archive: 9 << 10})
	for _, want := range []string{"Sent: [claude] session bbbbbbbb", "30 KB of context (cut from 3.0 MB",
		"2 workspace files (12 KB)", "1 rule file;", "encrypted archive 9 KB"} {
		if !strings.Contains(s, want) {
			t.Fatalf("missing %q in:\n%s", want, s)
		}
	}
}

// agy keeps the name the user gave a conversation only in its prompt history,
// as a `/rename <name>` slash command.
func TestAgyRenameNamesTheSession(t *testing.T) {
	tmp := isolateAgents(t)
	proj := filepath.Join(tmp, "proj")
	if err := os.MkdirAll(proj, 0755); err != nil {
		t.Fatal(err)
	}
	chdirTemp(t, proj)
	cwd, _ := os.Getwd()
	root := filepath.Join(tmp, "home", ".gemini", "antigravity-cli")
	const cid = "7e82424e-1111-4111-8111-111111111111"
	const cur = "99999999-2222-4222-8222-222222222222"
	hist := func(id, display, typ string) string {
		row, _ := json.Marshal(map[string]any{"display": display, "workspace": cwd, "conversationId": id, "type": typ})
		return string(row)
	}
	now := time.Now()
	writeLines(t, filepath.Join(root, "history.jsonl"), now,
		hist(cid, "/rename draft", "slash_command"), hist(cid, "Compress the weights", "user_input"),
		hist(cid, "/rename llm_compress", "slash_command"), hist(cid, "/usage", "slash_command"),
		hist(cur, "baton resume llm_compress", "user_input"))
	step := func(i int, typ, content string) string {
		row, _ := json.Marshal(map[string]any{"step_index": i, "source": "USER_EXPLICIT", "type": typ, "status": "DONE",
			"created_at": now.UTC().Format(time.RFC3339), "content": content})
		return string(row)
	}
	path := filepath.Join(root, "brain", cid, ".system_generated", "logs", "transcript_full.jsonl")
	writeLines(t, path, now.Add(-time.Hour), step(0, "USER_INPUT", "<USER_REQUEST>\nCompress the weights\n</USER_REQUEST>"))
	// The new agy conversation that runs `baton resume`.
	writeLines(t, filepath.Join(root, "brain", cur, ".system_generated", "logs", "transcript_full.jsonl"),
		now, step(0, "USER_INPUT", "<USER_REQUEST>\nbaton resume llm_compress\n</USER_REQUEST>"))

	// A conversation's title as agy's /resume shows it, here one agy made up.
	if err := os.MkdirAll(filepath.Join(root, "annotations"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "annotations", cur+".pbtxt"), []byte(`title:"Baton \"Resume\" Request"`), 0644); err != nil {
		t.Fatal(err)
	}
	if n := userSessionName("agy", path); n != "llm_compress" {
		t.Fatalf("agy name: %q", n)
	}
	titles := map[string]string{}
	for _, c := range discoverSessions("all", "") {
		titles[c.SessionID()] = c.Title
	}
	if titles[cid] != "llm_compress" || titles[cur] != `Baton "Resume" Request` {
		t.Fatalf("agy titles: %v", titles)
	}
	t.Setenv("PI_CODING_AGENT_DIR", "") // it would make baton take Pi for the caller
	t.Setenv("ANTIGRAVITY_CLI", "1")
	r, err := resumeSession([]string{"llm_compress"})
	if err != nil || !strings.Contains(r, "agy --conversation "+cid) {
		t.Fatalf("expected a native agy resume hint: %v\n%s", err, r)
	}
}
