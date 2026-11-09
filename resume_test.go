package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestMain keeps tests from linking or renaming the real agent session that
// runs them (attachCurrentSession finds it through the agent's environment).
// Fixtures use folders like /tmp, so the folder check is off unless a test
// turns it on.
func TestMain(m *testing.M) {
	os.Setenv("BATON_NO_ATTACH", "1")
	allowOtherFolder = true
	os.Exit(m.Run())
}

func chdirTemp(t *testing.T, dir string) {
	t.Helper()
	old, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })
}

func TestLoadRefusesAnotherFolder(t *testing.T) {
	tmp := isolateAgents(t)
	allowOtherFolder = false
	t.Cleanup(func() { allowOtherFolder = true })
	proj := filepath.Join(tmp, "my proj")
	other := filepath.Join(tmp, "other")
	link := filepath.Join(tmp, "link")
	for _, d := range []string{proj, other} {
		_ = os.MkdirAll(d, 0755)
	}
	if err := os.Symlink(proj, link); err != nil {
		t.Fatal(err)
	}
	ps := &ParsedSession{Agent: "claude", SessionID: "claude-p", CWD: proj, PostCompact: []SessionTurn{{Role: "user", Text: "Fix it"}}}
	if _, _, err := saveSessionToBatonStore(ps, "proj"); err != nil {
		t.Fatal(err)
	}

	chdirTemp(t, other)
	_, err := loadSessionContext("", "proj")
	if err == nil || !strings.Contains(err.Error(), "cd '"+proj+"' && ") || !strings.Contains(err.Error(), "baton load proj --force") {
		t.Fatalf("expected a refusal with cd and --force, got %v", err)
	}
	if _, err := resumeSession([]string{"proj"}); err == nil || !strings.Contains(err.Error(), "baton resume proj --force") {
		t.Fatalf("resume must refuse too, got %v", err)
	}

	// The same folder through a symlink is the same folder.
	chdirTemp(t, link)
	if r, err := loadSessionContext("", "proj"); err != nil || !strings.Contains(r, "Fix it") {
		t.Fatalf("a symlink to the folder must load: %v %s", err, r)
	}

	// A folder that does not exist here (another computer) does not stop loading.
	chdirTemp(t, other)
	ps2 := &ParsedSession{Agent: "claude", SessionID: "claude-q", CWD: filepath.Join(tmp, "gone"), PostCompact: []SessionTurn{{Role: "user", Text: "Remote"}}}
	if _, _, err := saveSessionToBatonStore(ps2, "remote"); err != nil {
		t.Fatal(err)
	}
	if r, err := loadSessionContext("", "remote"); err != nil || !strings.Contains(r, "Remote") {
		t.Fatalf("a missing folder must not stop loading: %v", err)
	}

	// --force loads here anyway.
	allowOtherFolder = true
	if r, err := loadSessionContext("", "proj"); err != nil || !strings.Contains(r, "Fix it") {
		t.Fatalf("--force must load: %v", err)
	}

	// The native resume hint starts the agent in the session's folder.
	if h := nativeResumeHint(SessionCandidate{Agent: "claude", Path: "/x/aaaaaaaa-1111-4111-8111-111111111111.jsonl", CWD: proj}); strings.Contains(h, "/resume ") || !strings.Contains(h, "cd '"+proj+"' && claude --resume aaaaaaaa-1111-4111-8111-111111111111") {
		t.Fatalf("hint must cd into the folder: %s", h)
	}
}

// isolateAgents points every agent store and baton's own dirs into tmp and
// clears the variables that identify the calling agent session.
func isolateAgents(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("BATON_CONFIG_DIR", filepath.Join(tmp, "baton-cfg"))
	t.Setenv("HOME", filepath.Join(tmp, "home"))
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(tmp, "claude"))
	t.Setenv("CODEX_HOME", filepath.Join(tmp, "codex"))
	t.Setenv("PI_CODING_AGENT_DIR", filepath.Join(tmp, "pi"))
	t.Setenv("KIMI_CODE_HOME", filepath.Join(tmp, "kimi"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(tmp, "xdg-data"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(tmp, "xdg-cache"))
	t.Setenv("CURSOR_HOME", filepath.Join(tmp, "cursor-home"))
	t.Setenv("TMPDIR", filepath.Join(tmp, "tmp"))
	for _, v := range []string{"CLAUDECODE", "CLAUDE_CODE_ENTRYPOINT", "CLAUDE_CODE_SESSION_ID", "CODEX_THREAD_ID",
		"CODEX_SANDBOX", "CODEX_CI", "PI_SESSION_ID", "CURSOR_AGENT", "CURSOR_CONVERSATION_ID"} {
		t.Setenv(v, "")
	}
	_ = os.MkdirAll(filepath.Join(tmp, "tmp"), 0700)
	return tmp
}

func writeLines(t *testing.T, path string, mtime time.Time, lines ...string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

func appendLines(t *testing.T, path string, mtime time.Time, lines ...string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(strings.Join(lines, "\n") + "\n")
	f.Close()
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

func claudeUser(text string) string {
	return `{"type":"user","cwd":"/work/proj","message":{"role":"user","content":"` + text + `"}}`
}

func TestSaveKeepsOlderVersions(t *testing.T) {
	isolateAgents(t)
	mk := func(id, text string) *ParsedSession {
		return &ParsedSession{Agent: "claude", SessionID: "claude-" + id, PostCompact: []SessionTurn{{Role: "user", Text: text}}}
	}
	p1, n1, err := saveSessionToBatonStore(mk("aaa", "First"), "BookTrans")
	if err != nil || n1 != "BookTrans" {
		t.Fatalf("first save: %q %v", n1, err)
	}
	old := time.Now().Add(-2 * time.Hour)
	_ = os.Chtimes(p1, old, old)

	// Another session under the same name takes it; the first becomes BookTrans.<date>.
	p2, n2, err := saveSessionToBatonStore(mk("bbb", "Second"), "BookTrans")
	if err != nil || n2 != "BookTrans" || p2 != p1 {
		t.Fatalf("second save must take the name: %q %q %v", n2, p2, err)
	}
	versions := savedVersionNames("BookTrans")
	want := "BookTrans." + old.Format("2006-01-02_15-04")
	if len(versions) != 1 || !versions[want] {
		t.Fatalf("expected older version %s, got %v", want, versions)
	}
	if list := discoverSavedBatonSessions("all", ""); len(list) != 1 || list[0].Title != "BookTrans" {
		t.Fatalf("older versions must not be numbered in the list: %+v", list)
	}
	if r := listSessionsReport("", "", false); !strings.Contains(r, "1 older version(s)") || strings.Contains(r, want) {
		t.Fatalf("list must only mention hidden versions: %s", r)
	}
	if r := listSessionsReport("", "", true); !strings.Contains(r, want) {
		t.Fatalf("list --all must show versions: %s", r)
	}
	if r, err := loadSessionContext("", "BookTrans"); err != nil || !strings.Contains(r, "Second") {
		t.Fatalf("the name must load the latest session: %v %s", err, r)
	}
	if r, err := loadSessionContext("", want); err != nil || !strings.Contains(r, "First") {
		t.Fatalf("a version loads by its name: %v %s", err, r)
	}

	// Re-saving the same session updates it in place.
	if _, _, err := saveSessionToBatonStore(mk("bbb", "Second again"), "BookTrans"); err != nil || len(savedVersionNames("BookTrans")) != 1 {
		t.Fatalf("re-save must not add a version: %v %v", err, savedVersionNames("BookTrans"))
	}
	// Only keepVersions older versions survive.
	for i, id := range []string{"c1", "c2", "c3", "c4"} {
		at := time.Now().Add(-time.Duration(60-i*10) * time.Minute)
		path := filepath.Join(savedSessionsDir(), "BookTrans.baton.json")
		_ = os.Chtimes(path, at, at)
		if _, _, err := saveSessionToBatonStore(mk(id, "Next "+id), "BookTrans"); err != nil {
			t.Fatal(err)
		}
	}
	if v := savedVersionNames("BookTrans"); len(v) != keepVersions {
		t.Fatalf("expected %d versions, got %v", keepVersions, v)
	}
	// rm reaches versions by name.
	for v := range savedVersionNames("BookTrans") {
		if r, err := removeSavedSession(v); err != nil || !strings.Contains(r, "Removed") {
			t.Fatalf("rm %s: %v %s", v, err, r)
		}
		break
	}

	// Imports under an agent's own title keep the numeric suffix.
	if _, n, err := saveSessionToBatonStoreMode(mk("zzz", "Other"), "BookTrans", false); err != nil || n != "BookTrans-2" {
		t.Fatalf("import mode must use a suffix: %q %v", n, err)
	}
}

func TestSnapshotFromNewerBaton(t *testing.T) {
	isolateAgents(t)
	dir := savedSessionsDir()
	_ = os.MkdirAll(dir, 0700)
	data := `{"agent":"claude","session_id":"claude-x","title":"future","format":99,"saved_by":"9.9.9","post_compact":[{"role":"user","text":"hi"}]}`
	if err := os.WriteFile(filepath.Join(dir, "future.baton.json"), []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadSessionContext("", "future"); err == nil || !strings.Contains(err.Error(), "baton update") {
		t.Fatalf("expected a request to update baton, got %v", err)
	}
	ps := &ParsedSession{Agent: "claude", SessionID: "claude-y", PostCompact: []SessionTurn{{Role: "user", Text: "x"}}}
	path, _, err := saveSessionToBatonStore(ps, "now")
	if err != nil {
		t.Fatal(err)
	}
	got, err := parseSessionFile("claude", path, time.Now())
	if err != nil || got.Format != snapshotFormat || got.SavedBy != version {
		t.Fatalf("saved snapshot must record its format and baton version: %+v %v", got, err)
	}
}

func TestResumeFollowsTheLatestPoint(t *testing.T) {
	tmp := isolateAgents(t)
	t.Setenv("BATON_NO_ATTACH", "")
	t.Setenv("CLAUDECODE", "1")
	projects := filepath.Join(tmp, "claude", "projects", "-work-proj")
	const idA = "aaaaaaaa-1111-4111-8111-111111111111"
	const idR = "bbbbbbbb-2222-4222-8222-222222222222"
	const idR2 = "dddddddd-4444-4444-8444-444444444444"
	const idC = "cccccccc-3333-4333-8333-333333333333"
	now := time.Now()

	// Claude session A is saved as "proj".
	pathA := filepath.Join(projects, idA+".jsonl")
	writeLines(t, pathA, now.Add(-time.Hour), claudeUser("Fix the parser"))
	psA, err := parseSessionFile("claude", pathA, now.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := saveSessionToBatonStore(psA, "proj"); err != nil {
		t.Fatal(err)
	}

	// A new Claude session R resumes it: the saved copy is the latest point.
	pathR := filepath.Join(projects, idR+".jsonl")
	writeLines(t, pathR, now, claudeUser("Resume session proj using baton"))
	t.Setenv("CLAUDE_CODE_SESSION_ID", idR)
	// A was saved in this agent and nothing was typed there since: resume natively.
	r, err := resumeSession([]string{"proj"})
	if err != nil || !strings.Contains(r, "/resume "+idA) || strings.Contains(r, "BATON SESSION HANDOFF") {
		t.Fatalf("expected a native resume hint for A: %v\n%s", err, r)
	}
	r, err = resumeSession([]string{"proj", "--load"})
	if err != nil || !strings.Contains(r, "Fix the parser") || !strings.Contains(r, "now named **proj**") {
		t.Fatalf("expected the saved copy to load and R to be named: %v\n%s", err, r)
	}
	if data, _ := os.ReadFile(pathR); !strings.Contains(string(data), `"customTitle":"proj"`) {
		t.Fatalf("R must get a Claude custom title: %s", data)
	}
	psR, _ := parseSessionFile("claude", pathR, now)
	if n := defaultSaveName(psR); n != "proj" {
		t.Fatalf("baton save without a name in R must reuse proj, got %q", n)
	}

	// Work goes on in R without saving; a Codex session resumes proj and gets R.
	appendLines(t, pathR, now.Add(10*time.Minute), claudeUser("Add tests"))
	t.Setenv("CLAUDECODE", "")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "")
	t.Setenv("CODEX_THREAD_ID", idC)
	pathC := filepath.Join(tmp, "codex", "sessions", "2026", "11", "09", "rollout-2026-11-09T10-00-00-"+idC+".jsonl")
	writeLines(t, pathC, now.Add(11*time.Minute),
		`{"type":"session_meta","payload":{"id":"`+idC+`","cwd":"/work/proj"}}`,
		`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Resume session proj using baton"}]}}`)
	r, err = resumeSession([]string{"proj"})
	if err != nil || !strings.Contains(r, "Add tests") || !strings.Contains(r, "previous copy kept as **proj.") {
		t.Fatalf("expected R to be imported as proj: %v\n%s", err, r)
	}
	if data, _ := os.ReadFile(filepath.Join(tmp, "codex", "session_index.jsonl")); !strings.Contains(string(data), `"thread_name":"proj"`) {
		t.Fatalf("the Codex session must be named proj: %s", data)
	}

	// R goes on again; from another Claude session it is resumed natively.
	appendLines(t, pathR, now.Add(20*time.Minute), claudeUser("Polish docs"))
	t.Setenv("CODEX_THREAD_ID", "")
	t.Setenv("CLAUDECODE", "1")
	pathR2 := filepath.Join(projects, idR2+".jsonl")
	writeLines(t, pathR2, now.Add(21*time.Minute), claudeUser("Resume session proj using baton"))
	t.Setenv("CLAUDE_CODE_SESSION_ID", idR2)
	r, err = resumeSession([]string{"proj"})
	if err != nil || !strings.Contains(r, "/resume "+idR) || !strings.Contains(r, "claude --resume "+idR) || strings.Contains(r, "Polish docs") {
		t.Fatalf("expected a native resume hint for R: %v\n%s", err, r)
	}
	r, err = resumeSession([]string{"proj", "--load"})
	if err != nil || !strings.Contains(r, "Polish docs") {
		t.Fatalf("--load must load R through baton: %v\n%s", err, r)
	}

	// Unknown names show the saved list.
	if r, err := resumeSession([]string{"nope"}); err != nil || !strings.Contains(r, "No saved or agent session is named") {
		t.Fatalf("unknown name: %v %s", err, r)
	}
}

func TestRenameKeepsUserSetClaudeName(t *testing.T) {
	tmp := isolateAgents(t)
	path := filepath.Join(tmp, "claude", "projects", "-w", "eeeeeeee-5555-4555-8555-555555555555.jsonl")
	writeLines(t, path, time.Now(), claudeUser("hi"), `{"type":"custom-title","customTitle":"my name","sessionId":"eeeeeeee-5555-4555-8555-555555555555"}`)
	c := SessionCandidate{Agent: "claude", Path: path}
	if note := renameAgentSession(c, "proj", ""); note != "" {
		t.Fatalf("a name the user set must stay, got %q", note)
	}
	if note := renameAgentSession(c, "proj", "my name"); note == "" {
		t.Fatal("a name baton set earlier may be replaced")
	}
	if got := userSessionName("claude", path); got != "proj" {
		t.Fatalf("expected the new custom title, got %q", got)
	}
}

func TestNativeResumeCommands(t *testing.T) {
	cases := map[string]SessionCandidate{
		"codex resume 01a1c062-9389-71f3-b121-7d1c794a4409":           {Agent: "codex", Path: "/c/rollout-2026-11-09T12-57-58-01a1c062-9389-71f3-b121-7d1c794a4409.jsonl"},
		"agy --conversation 07471015-e864-4918-a08e-f0dd32413ba8":     {Agent: "agy", Path: "/a/brain/07471015-e864-4918-a08e-f0dd32413ba8/.system_generated/logs/transcript.jsonl"},
		"kimi --session session_3033d0e7-97e8-491f-a47d-225799dfe637": {Agent: "kimi", Path: "/k/sessions/wd/session_3033d0e7-97e8-491f-a47d-225799dfe637/agents/main/wire.jsonl"},
		"opencode --session ses_abc123":                               {Agent: "opencode", Path: "/o/opencode.db::ses_abc123"},
		"agent --resume 11111111-2222-4333-8444-555555555555":         {Agent: "cursor", Path: "/cu/11111111-2222-4333-8444-555555555555.jsonl"},
	}
	for want, c := range cases {
		if got := nativeResumeCommand(c); got != want {
			t.Errorf("%s: got %q, want %q", c.Agent, got, want)
		}
	}
	pi := SessionCandidate{Agent: "pi", Path: "/p/--work--/2026-10-30T12-00-00-000Z_0b4c9d2e-1f3a-4b5c-8d6e-7f8a9b0c1d2e.jsonl"}
	if got := nativeResumeCommand(pi); got != "pi --session 0b4c9d2e-1f3a-4b5c-8d6e-7f8a9b0c1d2e" {
		t.Errorf("pi: got %q", got)
	}
	t.Setenv("HOME", "/home/u")
	if got := shellShortPath("/home/u/My Docs/proj"); got != "~/'My Docs/proj'" {
		t.Errorf("home paths keep an unquoted ~: got %q", got)
	}
}

func TestRefusalOffersNativeResume(t *testing.T) {
	tmp := isolateAgents(t)
	allowOtherFolder = false
	t.Cleanup(func() { allowOtherFolder = true })
	t.Setenv("CLAUDECODE", "1")
	proj := filepath.Join(tmp, "proj")
	other := filepath.Join(tmp, "other")
	_ = os.MkdirAll(proj, 0755)
	_ = os.MkdirAll(other, 0755)
	const id = "ffffffff-6666-4666-8666-666666666666"
	path := filepath.Join(tmp, "claude", "projects", "-proj", id+".jsonl")
	writeLines(t, path, time.Now().Add(-time.Hour), `{"type":"user","cwd":"`+proj+`","message":{"role":"user","content":"Fix it"}}`)
	ps, err := parseSessionFile("claude", path, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := saveSessionToBatonStore(ps, "proj"); err != nil {
		t.Fatal(err)
	}
	chdirTemp(t, other)
	want := "cd " + proj + " && claude --resume " + id
	if _, err := loadSessionContext("", "proj"); err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("load refusal must offer %q, got %v", want, err)
	}
	if _, err := importSession([]string{id[:8]}); err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("import refusal must offer %q, got %v", want, err)
	}
	// Typed on after the save: the snapshot is another point, so no native offer.
	appendLines(t, path, time.Now(), `{"type":"user","cwd":"`+proj+`","message":{"role":"user","content":"More"}}`)
	if _, err := loadSessionContext("", "proj"); err == nil || strings.Contains(err.Error(), "--resume") || !strings.Contains(err.Error(), "baton load proj") {
		t.Fatalf("load refusal must fall back to baton load, got %v", err)
	}
}
