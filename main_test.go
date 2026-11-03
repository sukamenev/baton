package main

import (
	"encoding/binary"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestParseClaudeCompactionAndHandoff(t *testing.T) {
	tmp := t.TempDir()
	sessionFile := filepath.Join(tmp, "sample-session.jsonl")

	lines := []string{
		`{"type":"custom-title","customTitle":"auth-refactor"}`,
		`{"type":"user","cwd":"/workspace/demo","message":{"content":"Initial task before compaction"}}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"Early response"},{"type":"tool_use","name":"Edit","input":{"file_path":"pkg/core.go"}}]}}`,
		`{"type":"summary","summary":"Summary of part 1: configured module pkg/core.go"}`,
		`{"type":"user","message":{"content":"Continue after compact: add bounds check"}}`,
		`{"type":"assistant","message":{"content":[{"type":"thinking","thinking":"Checking slice bounds"},{"type":"text","text":"Adding check and running go test"},{"type":"tool_use","name":"Bash","input":{"command":"go test ./..."}}]}}`,
	}
	if err := os.WriteFile(sessionFile, []byte(strings.Join(lines, "\n")+"\n"), 0644); err != nil {
		t.Fatal(err)
	}

	ps, err := parseSessionFile("claude", sessionFile, time.Now())
	if err != nil {
		t.Fatalf("parseSessionFile error: %v", err)
	}

	if ps.Title != "auth-refactor" {
		t.Fatalf("expected title 'auth-refactor', got %q", ps.Title)
	}
	if ps.PreCompactTurns != 3 {
		t.Fatalf("expected 3 pre-compact turns, got %d", ps.PreCompactTurns)
	}
	if !strings.Contains(ps.CompactSummary, "pkg/core.go") {
		t.Fatalf("unexpected compaction summary: %q", ps.CompactSummary)
	}
	if len(ps.PostCompact) != 4 {
		t.Fatalf("expected 4 post-compact turns, got %d", len(ps.PostCompact))
	}
	if len(ps.ModifiedFiles) != 1 || ps.ModifiedFiles[0] != "pkg/core.go" {
		t.Fatalf("modified files across session not preserved: %v", ps.ModifiedFiles)
	}
	if len(ps.Commands) != 1 || ps.Commands[0] != "go test ./..." {
		t.Fatalf("commands list not preserved: %v", ps.Commands)
	}
}

func TestExtractUTF8StringsFromPB(t *testing.T) {
	msg := "Sample UTF-8 conversation turn extracted from schema-less Protobuf"
	var buf []byte
	tag := uint64((1 << 3) | 2) // field 1, wire type 2
	tmp := make([]byte, binary.MaxVarintLen64)
	n := binary.PutUvarint(tmp, tag)
	buf = append(buf, tmp[:n]...)
	n = binary.PutUvarint(tmp, uint64(len(msg)))
	buf = append(buf, tmp[:n]...)
	buf = append(buf, []byte(msg)...)

	got := extractUTF8StringsFromPB(buf)
	if len(got) != 1 || got[0] != msg {
		t.Fatalf("expected %q, got %v", msg, got)
	}
}

func TestEncryptDecryptAndBackupRoundtrip(t *testing.T) {
	t.Setenv("BATON_CONFIG_DIR", t.TempDir())
	origSession := &ParsedSession{
		Agent:          "claude",
		SessionID:      "claude-test-1",
		Title:          "my-feature",
		SourceFile:     "/tmp/test.jsonl",
		ModTime:        time.Now(),
		CompactSummary: "Compacted summary",
		AllTurns:       []SessionTurn{{TurnID: 1, Role: "user", Text: "Hello"}},
		PostCompact:    []SessionTurn{{TurnID: 1, Role: "user", Text: "Hello"}},
	}

	fileContent := []byte("package main\n// New version\n")
	bundle := &RemoteBundle{
		Version:       version,
		CreatedAt:     time.Now(),
		SourceWorkdir: "/tmp/src",
		Session:       origSession,
		Files: []FileEntry{
			{
				RelPath: "hello.go",
				Mode:    0644,
				SHA256:  sha256Hex(fileContent),
				Content: fileContent,
			},
		},
		GlobalRules: map[string]string{
			"global_CLAUDE.md": "# Author rules",
		},
	}

	b64, key, err := compressAndEncryptBundle(bundle)
	if err != nil {
		t.Fatalf("encryption error: %v", err)
	}

	vaultDir := t.TempDir()
	vaultFile := filepath.Join(vaultDir, "test.baton")
	if err := os.WriteFile(vaultFile, []byte(b64), 0600); err != nil {
		t.Fatal(err)
	}

	workDir := t.TempDir()
	oldWd, _ := os.Getwd()
	defer os.Chdir(oldWd)
	_ = os.Chdir(workDir)

	// Create a differing local file to verify .baton.bak backup creation
	_ = os.WriteFile(filepath.Join(workDir, "hello.go"), []byte("package main\n// Old version\n"), 0644)

	token := "baton:file:" + vaultFile + "#" + key
	report, err := pullRemoteSession(token)
	if err != nil {
		t.Fatalf("pullRemoteSession error: %v", err)
	}

	if !strings.Contains(report, "hello.go.baton.bak") {
		t.Fatalf("expected backup hello.go.baton.bak in report:\n%s", report)
	}
	if _, err := os.Stat(filepath.Join(workDir, "hello.go.baton.bak")); err != nil {
		t.Fatalf("backup file not created: %v", err)
	}
}

func TestParsePiTreeAndOpenCodeSQLite(t *testing.T) {
	tmp := t.TempDir()

	// 1. Test Pi v3 tree JSONL (with abandoned branch, session_info name, and compaction details)
	piFile := filepath.Join(tmp, "pi-session.jsonl")
	piLines := []string{
		`{"type":"session","version":3,"id":"sess-1","cwd":"/work/pi-proj"}`,
		`{"type":"session_info","id":"info-1","parentId":"","name":"pi-parser-task"}`,
		`{"type":"message","id":"m1","parentId":"info-1","message":{"role":"user","content":[{"type":"text","text":"Write parser in Pi"}]}}`,
		`{"type":"message","id":"m-dead","parentId":"m1","message":{"role":"assistant","content":[{"type":"text","text":"Abandoned branch response"}]}}`,
		`{"type":"compaction","id":"c1","parentId":"m1","summary":"Compacted Pi session summary","details":{"modifiedFiles":["src/parser.ts"]}}`,
		`{"type":"message","id":"m2","parentId":"c1","message":{"role":"assistant","content":[{"type":"text","text":"Continuing after compaction in Pi"}]}}`,
		`{"type":"message","id":"m3","parentId":"m2","message":{"role":"bashExecution","command":"npm test","output":"PASS"}}`,
	}
	if err := os.WriteFile(piFile, []byte(strings.Join(piLines, "\n")+"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	psPi, err := parseSessionFile("pi", piFile, time.Now())
	if err != nil {
		t.Fatalf("parseSessionFile pi: %v", err)
	}
	if psPi.Title != "pi-parser-task" || psPi.CWD != "/work/pi-proj" {
		t.Fatalf("unexpected Pi metadata: title=%q cwd=%q", psPi.Title, psPi.CWD)
	}
	if psPi.CompactSummary != "Compacted Pi session summary" || len(psPi.PostCompact) != 2 {
		t.Fatalf("unexpected Pi parse result: %+v", psPi)
	}
	if len(psPi.ModifiedFiles) != 1 || psPi.ModifiedFiles[0] != "src/parser.ts" {
		t.Fatalf("expected modified file src/parser.ts, got %v", psPi.ModifiedFiles)
	}
	for _, turn := range psPi.AllTurns {
		if strings.Contains(turn.Text, "Abandoned branch") {
			t.Fatalf("abandoned tree branch should not be in active path: %+v", turn)
		}
	}

	// 2. Deprecated OpenCode 1.x schema (session/message/part), the fallback when
	// session_v2 is absent. Remove after 2027-04-01 together with parseOpenCodeV1Session.
	if _, err := exec.LookPath("sqlite3"); err == nil {
		dbPath := filepath.Join(tmp, "opencode.db")
		schemaSQL := `
		CREATE TABLE session (id TEXT PRIMARY KEY, parent_id TEXT, directory TEXT NOT NULL, title TEXT NOT NULL,
			time_created INTEGER NOT NULL, time_updated INTEGER NOT NULL, time_archived INTEGER);
		CREATE TABLE message (id TEXT PRIMARY KEY, session_id TEXT NOT NULL, time_created INTEGER NOT NULL, data TEXT NOT NULL);
		CREATE TABLE part (id TEXT PRIMARY KEY, message_id TEXT NOT NULL, session_id TEXT NOT NULL, time_created INTEGER NOT NULL, data TEXT NOT NULL);
		INSERT INTO session VALUES ('ses_0b4c9d2e1f3aAbCdEfGhIjKlMn', NULL, '/srv/app', 'Fix add in calc.py', 1700000000000, 1700000900000, NULL);
		INSERT INTO session VALUES ('ses_child', 'ses_0b4c9d2e1f3aAbCdEfGhIjKlMn', '/srv/app', 'Subagent task', 1700000100000, 1700000200000, NULL);
		INSERT INTO message VALUES ('msg_1', 'ses_0b4c9d2e1f3aAbCdEfGhIjKlMn', 1, '{"role":"user"}');
		INSERT INTO part VALUES ('prt_1', 'msg_1', 'ses_0b4c9d2e1f3aAbCdEfGhIjKlMn', 1, '{"type":"text","text":"Initial request before compaction"}');
		INSERT INTO message VALUES ('msg_2', 'ses_0b4c9d2e1f3aAbCdEfGhIjKlMn', 2, '{"role":"assistant"}');
		INSERT INTO part VALUES ('prt_2', 'msg_2', 'ses_0b4c9d2e1f3aAbCdEfGhIjKlMn', 2, '{"type":"tool","tool":"edit","state":{"status":"completed","input":{"filePath":"/srv/app/calc.py","oldString":"a - b","newString":"a + b"},"output":"Edit applied successfully."}}');
		INSERT INTO message VALUES ('msg_3', 'ses_0b4c9d2e1f3aAbCdEfGhIjKlMn', 3, '{"role":"user"}');
		INSERT INTO part VALUES ('prt_3', 'msg_3', 'ses_0b4c9d2e1f3aAbCdEfGhIjKlMn', 3, '{"type":"compaction","auto":true}');
		INSERT INTO message VALUES ('msg_4', 'ses_0b4c9d2e1f3aAbCdEfGhIjKlMn', 4, '{"role":"assistant","summary":true,"mode":"compaction"}');
		INSERT INTO part VALUES ('prt_4a', 'msg_4', 'ses_0b4c9d2e1f3aAbCdEfGhIjKlMn', 4, '{"type":"reasoning","text":"Summarizing"}');
		INSERT INTO part VALUES ('prt_4b', 'msg_4', 'ses_0b4c9d2e1f3aAbCdEfGhIjKlMn', 5, '{"type":"text","text":"## Objective\nFix calc.py"}');
		INSERT INTO message VALUES ('msg_5', 'ses_0b4c9d2e1f3aAbCdEfGhIjKlMn', 6, '{"role":"user","summary":{"diffs":[]}}');
		INSERT INTO part VALUES ('prt_5', 'msg_5', 'ses_0b4c9d2e1f3aAbCdEfGhIjKlMn', 6, '{"type":"text","text":"Now run the tests"}');
		INSERT INTO message VALUES ('msg_6', 'ses_0b4c9d2e1f3aAbCdEfGhIjKlMn', 7, '{"role":"assistant"}');
		INSERT INTO part VALUES ('prt_6', 'msg_6', 'ses_0b4c9d2e1f3aAbCdEfGhIjKlMn', 7, '{"type":"tool","tool":"bash","state":{"status":"completed","input":{"command":"python3 -m pytest"},"output":"1 passed"}}');
		`
		if out, err := exec.Command("sqlite3", dbPath, schemaSQL).CombinedOutput(); err != nil {
			t.Fatalf("sqlite3 init failed: %v (%s)", err, string(out))
		}
		candidates := discoverOpenCodeSessions(dbPath, map[string]string{}, "")
		if len(candidates) != 1 || candidates[0].Title != "Fix add in calc.py" || candidates[0].Preview != "Now run the tests" || candidates[0].CWD != "/srv/app" {
			t.Fatalf("expected one top-level OpenCode session with the last prompt, got %+v", candidates)
		}
		if id := shortSessionID(candidates[0].SessionID()); id != "0b4c9d2e" {
			t.Fatalf("expected short OpenCode ID 0b4c9d2e, got %q", id)
		}
		ps, err := parseSessionFile("opencode", candidates[0].Path, time.Now())
		if err != nil {
			t.Fatalf("parseSessionFile opencode: %v", err)
		}
		if ps.CompactSummary != "## Objective\nFix calc.py" || ps.Title != "Fix add in calc.py" || ps.CWD != "/srv/app" {
			t.Fatalf("unexpected OpenCode parse: %+v", ps)
		}
		if len(ps.PostCompact) != 3 || ps.PostCompact[0].Text != "Now run the tests" || ps.PostCompact[2].RawOutput != "1 passed" {
			t.Fatalf("unexpected OpenCode post-compact turns: %+v", ps.PostCompact)
		}
		if len(ps.ModifiedFiles) != 1 || ps.ModifiedFiles[0] != "/srv/app/calc.py" || len(ps.Commands) != 1 || ps.Commands[0] != "python3 -m pytest" {
			t.Fatalf("unexpected OpenCode files/commands: %v %v", ps.ModifiedFiles, ps.Commands)
		}
	}
}

func TestParseOpenCodeV2(t *testing.T) {
	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Skip("sqlite3 CLI is not installed")
	}
	dbPath := filepath.Join(t.TempDir(), "opencode.db")
	const id = "ses_0b4c9d2e1f3aAbCdEfGhIjKlMn"
	schemaSQL := `
	CREATE TABLE session_v2 (id TEXT PRIMARY KEY, parent_id TEXT, directory TEXT NOT NULL, title TEXT,
		time_created INTEGER NOT NULL, time_updated INTEGER NOT NULL, time_archived INTEGER);
	CREATE TABLE session_message (id TEXT PRIMARY KEY, session_id TEXT NOT NULL, type TEXT NOT NULL, seq INTEGER NOT NULL, data TEXT NOT NULL);
	INSERT INTO session_v2 VALUES ('` + id + `', NULL, '/srv/app', 'Fix mul in calc2.py', 1, 1700000900000, NULL);
	INSERT INTO session_v2 VALUES ('ses_child', '` + id + `', '/srv/app', NULL, 1, 1700000200000, NULL);
	INSERT INTO session_message VALUES ('m1', '` + id + `', 'user', 4, '{"text":"Initial request before compaction","files":[]}');
	INSERT INTO session_message VALUES ('m2', '` + id + `', 'assistant', 7, '{"content":[{"type":"reasoning","text":"Look at the file"},{"type":"tool","name":"edit","state":{"status":"completed","input":{"path":"/srv/app/calc2.py","oldString":"a + b","newString":"a * b"},"content":[{"type":"text","text":"Edited calc2.py (1 replacement)"}]}}]}');
	INSERT INTO session_message VALUES ('m3', '` + id + `', 'idle', 8, '{"outcome":"succeeded"}');
	INSERT INTO session_message VALUES ('m4', '` + id + `', 'compaction', 9, '{"status":"completed","reason":"auto","summary":"## Objective\nFix calc2.py"}');
	INSERT INTO session_message VALUES ('m5', '` + id + `', 'system', 10, '{"text":"The available tools have changed."}');
	INSERT INTO session_message VALUES ('m6', '` + id + `', 'user', 11, '{"text":"Now run the tests","files":[]}');
	INSERT INTO session_message VALUES ('m7', '` + id + `', 'assistant', 12, '{"content":[{"type":"tool","name":"shell","state":{"status":"completed","input":{"command":"python3 -m pytest"},"content":[{"type":"text","text":"1 passed"}]}},{"type":"text","text":"Tests pass."}]}');
	`
	if out, err := exec.Command("sqlite3", dbPath, schemaSQL).CombinedOutput(); err != nil {
		t.Fatalf("sqlite3 init failed: %v (%s)", err, string(out))
	}
	candidates := discoverOpenCodeSessions(dbPath, map[string]string{}, "")
	if len(candidates) != 1 || candidates[0].Title != "Fix mul in calc2.py" || candidates[0].Preview != "Now run the tests" {
		t.Fatalf("expected one top-level OpenCode 2.x session with the last prompt, got %+v", candidates)
	}
	ps, err := parseSessionFile("opencode", candidates[0].Path, time.Now())
	if err != nil {
		t.Fatalf("parseSessionFile opencode v2: %v", err)
	}
	if ps.CompactSummary != "## Objective\nFix calc2.py" || ps.Title != "Fix mul in calc2.py" || ps.CWD != "/srv/app" {
		t.Fatalf("unexpected OpenCode 2.x parse: %+v", ps)
	}
	if len(ps.PostCompact) != 4 || ps.PostCompact[0].Text != "Now run the tests" || ps.PostCompact[2].RawOutput != "1 passed" || ps.PostCompact[3].Text != "Tests pass." {
		t.Fatalf("unexpected OpenCode 2.x post-compact turns: %+v", ps.PostCompact)
	}
	if len(ps.ModifiedFiles) != 1 || ps.ModifiedFiles[0] != "/srv/app/calc2.py" || len(ps.Commands) != 1 || ps.Commands[0] != "python3 -m pytest" {
		t.Fatalf("unexpected OpenCode 2.x files/commands: %v %v", ps.ModifiedFiles, ps.Commands)
	}
}

func TestCrossPlatformSlugsAndCandidateFallback(t *testing.T) {
	// Verify Pi safe session dir naming for Unix and Windows paths
	if got := piSafeSessionDirName("/home/user/proj"); got != "--home-user-proj--" {
		t.Fatalf("unexpected Unix piSafeSessionDirName: %q", got)
	}
	if got := piSafeSessionDirName(`C:\Users\dev\project`); got != "--C--Users-dev-project--" {
		t.Fatalf("unexpected Windows piSafeSessionDirName: %q", got)
	}

	// Verify Claude project slugs for Windows drive paths
	winSlugs := claudeProjectSlugs(`C:\Users\dev\project`)
	if len(winSlugs) == 0 || winSlugs[0] != "C--Users-dev-project" {
		t.Fatalf("unexpected Windows claudeProjectSlugs: %v", winSlugs)
	}

	// Verify semantic version comparison against latest.txt and release checksums
	if !isNewerVersion("0.3.0", "0.2.0") || !isNewerVersion("1.0.0", "0.9.9") {
		t.Fatalf("expected newer version to return true")
	}
	if isNewerVersion("0.2.0", "0.2.0") || isNewerVersion("0.1.9", "0.2.0") {
		t.Fatalf("expected equal or older version to return false")
	}
	if got := releaseAssetName("windows", "amd64"); got != "baton_windows_amd64.exe" {
		t.Fatalf("unexpected Windows asset name: %q", got)
	}
	sampleChecksums := "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855  baton_linux_amd64\n"
	if got := parseExpectedSHA256(sampleChecksums, "baton_linux_amd64"); got != "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855" {
		t.Fatalf("unexpected parsed SHA256: %q", got)
	}

	// Verify smart single-argument CLI resolution (no agent name required)
	if ag, q := resolveAgentAndQuery(nil, "latest"); ag != "all" || q != "latest" {
		t.Fatalf("unexpected zero-arg resolution: %q %q", ag, q)
	}
	if ag, q := resolveAgentAndQuery([]string{"auth-refactor"}, "latest"); ag != "all" || q != "auth-refactor" {
		t.Fatalf("unexpected session-name resolution: %q %q", ag, q)
	}
	if ag, q := resolveAgentAndQuery([]string{"claude"}, "latest"); ag != "claude" || q != "latest" {
		t.Fatalf("unexpected agent-only resolution: %q %q", ag, q)
	}

	// Verify Saved Baton Sessions Store (`baton save`, `baton list`, `baton load`, `baton clear`)
	tmp := t.TempDir()
	t.Setenv("BATON_CONFIG_DIR", filepath.Join(tmp, "baton-cfg"))
	t.Setenv("PI_CODING_AGENT_DIR", tmp)
	// Isolate the other agents' session stores so `baton import` sees only the test session
	t.Setenv("HOME", filepath.Join(tmp, "home"))
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(tmp, "claude"))
	t.Setenv("CODEX_HOME", filepath.Join(tmp, "codex"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(tmp, "xdg-data"))
	t.Setenv("CURSOR_HOME", filepath.Join(tmp, "cursor-home"))
	t.Setenv("CURSOR_AGENT", "")
	t.Setenv("CURSOR_CONVERSATION_ID", "")
	// clearSavedSessions removes os.TempDir()/baton-handoff — keep it away from the real /tmp
	t.Setenv("TMPDIR", filepath.Join(tmp, "tmp"))
	sessDir := filepath.Join(tmp, "sessions", "--tmp--")
	if err := os.MkdirAll(sessDir, 0755); err != nil {
		t.Fatal(err)
	}
	sFile := filepath.Join(sessDir, "2026-10-30T12-00-00-000Z_0b4c9d2e-1f3a-4b5c-8d6e-7f8a9b0c1d2e.jsonl")
	content := `{"type":"session","version":3,"id":"s1","cwd":"/tmp"}` + "\n" +
		`{"type":"session_info","id":"i1","name":"billing-fix"}` + "\n" +
		`{"type":"message","id":"m1","parentId":"i1","message":{"role":"user","content":[{"type":"text","text":"Fix invoice rounding"}]}}` + "\n"
	if err := os.WriteFile(sFile, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	// 0. Save session into Baton's saved sessions store (`baton save billing-fix`)
	saveMsg, err := nameSession("billing-fix")
	if err != nil || !strings.Contains(saveMsg, "billing-fix") {
		t.Fatalf("expected nameSession to save session, err=%v, msg=%s", err, saveMsg)
	}

	// 1. Load by session name ("billing-fix")
	reportNamed, err := loadSessionContext("pi", "billing-fix")
	if err != nil || !strings.Contains(reportNamed, "Fix invoice rounding") {
		t.Fatalf("expected named session load to succeed, err=%v, report=%s", err, reportNamed)
	}

	// 2. When user forgot the session name or gave an unknown name, return numbered candidate list
	reportCandidates, err := loadSessionContext("pi", "unknown-session-name")
	if err != nil || !strings.Contains(reportCandidates, "**#1**") || !strings.Contains(reportCandidates, "· id 0b4c9d2e") || !strings.Contains(reportCandidates, "billing-fix") {
		t.Fatalf("expected numbered candidate list fallback, err=%v, report=%s", err, reportCandidates)
	}

	// 3. Load by candidate number ("1")
	reportByNum, err := loadSessionContext("pi", "1")
	if err != nil || !strings.Contains(reportByNum, "Fix invoice rounding") {
		t.Fatalf("expected candidate #1 load to succeed, err=%v, report=%s", err, reportByNum)
	}

	// 3a. Load by session ID prefix shown in the list
	reportByID, err := loadSessionContext("pi", "0b4c9d2e")
	if err != nil || !strings.Contains(reportByID, "Fix invoice rounding") {
		t.Fatalf("expected load by session ID to succeed, err=%v, report=%s", err, reportByID)
	}

	// 4. Clear all saved Baton sessions (`baton clear`)
	clearMsg, err := clearSavedSessions()
	if err != nil || !strings.Contains(clearMsg, "1 saved session") {
		t.Fatalf("expected clearSavedSessions to remove 1 session, err=%v, msg=%s", err, clearMsg)
	}
	listAfterClear := listSessionsReport("", "")
	if !strings.Contains(listAfterClear, "No saved Baton sessions found") {
		t.Fatalf("expected empty saved session list after clear, got: %s", listAfterClear)
	}

	// 5. Import a raw agent session that was never saved (`baton import`, `baton import 1`)
	importList, err := importSession(nil)
	if err != nil || !strings.Contains(importList, "**#1**") || !strings.Contains(importList, "· id 0b4c9d2e") || !strings.Contains(importList, "billing-fix") {
		t.Fatalf("expected numbered raw session list from importSession, err=%v, report=%s", err, importList)
	}
	if unmatched, err := importSession([]string{"claude"}); err != nil || !strings.Contains(unmatched, "No agent session matched") {
		t.Fatalf("expected unmatched import name to return the list, err=%v, report=%s", err, unmatched)
	}
	importReport, err := importSession([]string{"0b4c9d2e"})
	if err != nil || !strings.Contains(importReport, "Imported [pi]") || !strings.Contains(importReport, "Fix invoice rounding") {
		t.Fatalf("expected importSession to import and load raw session by ID, err=%v, report=%s", err, importReport)
	}
	if listAfterImport := listSessionsReport("", ""); !strings.Contains(listAfterImport, "billing-fix") {
		t.Fatalf("expected imported session in saved list, got: %s", listAfterImport)
	}
	reportImportedByName, err := loadSessionContext("", "billing-fix")
	if err != nil || !strings.Contains(reportImportedByName, "Fix invoice rounding") {
		t.Fatalf("expected imported session to load by name, err=%v, report=%s", err, reportImportedByName)
	}

	// 6. Import under another name without loading (`baton import 1 as invoices --no-load`)
	noLoad, err := importSession([]string{"1", "as", "invoices", "--no-load"})
	if err != nil || !strings.Contains(noLoad, "as **invoices**") || strings.Contains(noLoad, "Fix invoice rounding") {
		t.Fatalf("expected --no-load import to save under the new name without loading, err=%v, report=%s", err, noLoad)
	}
	if report, err := loadSessionContext("", "invoices"); err != nil || !strings.Contains(report, "Fix invoice rounding") {
		t.Fatalf("expected session imported as invoices to load, err=%v, report=%s", err, report)
	}

	// 7. An unknown number never falls back to another session
	if report, err := importSession([]string{"99999", "as", "x"}); err != nil || !strings.Contains(report, "No agent session matched") {
		t.Fatalf("expected unknown import number to return the list, err=%v, report=%s", err, report)
	}
}

func TestDisplayPath(t *testing.T) {
	t.Setenv("HOME", "/home/u")
	for in, want := range map[string]string{
		"/home/u/proj/x": "~/proj/x",
		"/home/u":        "~",
		"/home/user2/x":  "/home/user2/x",
		"/home/u..x":     "/home/u..x",
		"/tmp":           "/tmp",
		"":               "",
	} {
		if got := displayPath(in); got != want {
			t.Errorf("displayPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestImportAndSaveArgs(t *testing.T) {
	q, as, noLoad := parseImportArgs([]string{"Подтверди", "присутствие", "as", "my", "name", "--no-load"})
	if q != "Подтверди присутствие" || as != "my name" || !noLoad {
		t.Fatalf("parseImportArgs: got q=%q as=%q noLoad=%v", q, as, noLoad)
	}
	if q, as, _ := parseImportArgs([]string{"as"}); q != "as" || as != "" {
		t.Fatalf("a lone 'as' must be a query, got q=%q as=%q", q, as)
	}

	if saveArgsError(nil) != "" || saveArgsError([]string{"auth-refactor"}) != "" {
		t.Fatal("baton save [name] must stay valid")
	}
	if msg := saveArgsError([]string{"7"}); !strings.Contains(msg, "baton import 7") {
		t.Fatalf("baton save 7 must point to baton import, got %q", msg)
	}
	if msg := saveArgsError([]string{"auth", "7"}); !strings.Contains(msg, "baton import <#|id> as auth") {
		t.Fatalf("baton save <name> <#> must point to baton import ... as, got %q", msg)
	}

	candidates := []SessionCandidate{
		{Agent: "claude", Path: "/x/0b4c9d2e-1f3a-4b5c-8d6e-7f8a9b0c1d2e.jsonl", Title: "billing-fix"},
		{Agent: "claude", Path: "/x/1c5d0e3f-1f3a-4b5c-8d6e-7f8a9b0c1d2e.jsonl", Title: "billing-report"},
	}
	if _, ok := findImportCandidate(candidates, "billing"); ok {
		t.Fatal("an ambiguous name fragment must not pick a session")
	}
	if c, ok := findImportCandidate(candidates, "report"); !ok || c.Title != "billing-report" {
		t.Fatal("a unique name fragment must pick its session")
	}
	if c, ok := findImportCandidate(candidates, "1c5d0e3f"); !ok || c.Title != "billing-report" {
		t.Fatal("an ID prefix must pick its session")
	}
}

func TestSavedSessionNameCollision(t *testing.T) {
	t.Setenv("BATON_CONFIG_DIR", t.TempDir())
	t.Setenv("TMPDIR", t.TempDir())
	first := &ParsedSession{Agent: "claude", SessionID: "claude-aaa", PostCompact: []SessionTurn{{Role: "user", Text: "First"}}}
	second := &ParsedSession{Agent: "codex", SessionID: "codex-bbb", PostCompact: []SessionTurn{{Role: "user", Text: "Second"}}}

	p1, n1, err := saveSessionToBatonStore(first, "BookTrans")
	if err != nil || n1 != "BookTrans" {
		t.Fatalf("first save: name=%q err=%v", n1, err)
	}
	// A different session under the same name must not overwrite the first one
	p2, n2, err := saveSessionToBatonStore(second, "BookTrans")
	if err != nil || n2 != "BookTrans-2" || p2 == p1 {
		t.Fatalf("expected BookTrans-2 for a different session, got name=%q path=%q err=%v", n2, p2, err)
	}
	// Re-saving an already saved session updates its own file in place
	if p, n, err := saveSessionToBatonStore(second, "BookTrans"); err != nil || n != "BookTrans-2" || p != p2 {
		t.Fatalf("expected re-save to reuse BookTrans-2, got name=%q path=%q err=%v", n, p, err)
	}
	if p, n, err := saveSessionToBatonStore(first, "BookTrans"); err != nil || n != "BookTrans" || p != p1 {
		t.Fatalf("expected re-save to reuse BookTrans, got name=%q path=%q err=%v", n, p, err)
	}
	if report, err := loadSessionContext("", "BookTrans-2"); err != nil || !strings.Contains(report, "Second") {
		t.Fatalf("expected BookTrans-2 to load the second session, err=%v report=%s", err, report)
	}
}

func TestRemoveSavedSession(t *testing.T) {
	t.Setenv("BATON_CONFIG_DIR", t.TempDir())
	t.Setenv("TMPDIR", t.TempDir())
	for _, ps := range []*ParsedSession{
		{Agent: "claude", SessionID: "claude-aaa", SourceFile: "/src/a.jsonl"},
		{Agent: "codex", SessionID: "codex-bbb", SourceFile: "/src/b.jsonl"},
	} {
		if _, _, err := saveSessionToBatonStore(ps, "yudkowsky"); err != nil {
			t.Fatal(err)
		}
	}
	// A partial or unknown name must not delete anything
	if report, err := removeSavedSession("yudk"); err != nil || !strings.Contains(report, "No saved session matched") {
		t.Fatalf("expected list for a non-exact name, err=%v report=%s", err, report)
	}
	if report, err := removeSavedSession("yudkowsky-2"); err != nil || !strings.Contains(report, "Removed saved session **yudkowsky-2**") {
		t.Fatalf("expected removal by name, err=%v report=%s", err, report)
	}
	if aliases := loadAliases(); aliases["/src/b.jsonl"] != "" {
		t.Fatalf("expected alias of the removed session to be dropped, got %v", aliases)
	}
	if report, err := removeSavedSession("1"); err != nil || !strings.Contains(report, "Removed saved session **yudkowsky**") {
		t.Fatalf("expected removal by number, err=%v report=%s", err, report)
	}
	if list := listSessionsReport("", ""); !strings.Contains(list, "No saved Baton sessions found") {
		t.Fatalf("expected empty store, got: %s", list)
	}
}

func TestCursorSessionParseAndCaller(t *testing.T) {
	tmp := t.TempDir()
	id := "11111111-2222-3333-4444-555555555555"
	jsonl := filepath.Join(tmp, "projects", "demo", "agent-transcripts", id, id+".jsonl")
	if err := os.MkdirAll(filepath.Dir(jsonl), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(tmp, "chats", "ws", id), 0755); err != nil {
		t.Fatal(err)
	}
	meta := `{"schemaVersion":1,"title":"Tool Upgrade Cursor","cwd":"/data/sources/baton"}`
	if err := os.WriteFile(filepath.Join(tmp, "chats", "ws", id, "meta.json"), []byte(meta), 0644); err != nil {
		t.Fatal(err)
	}
	segDir := filepath.Join(filepath.Dir(jsonl), "compaction")
	if err := os.MkdirAll(segDir, 0755); err != nil {
		t.Fatal(err)
	}
	segment := strings.Join([]string{
		"# HISTORICAL -- DO NOT EDIT",
		"",
		"## Summary (curated by compaction step)",
		"",
		"Summary:",
		"1. The parser was being extended for Cursor.",
		"",
		"## Verbatim turns",
		"",
		"### Turn 0 (System)",
		"system prompt",
		"",
		"### Turn 1 (Human)",
		"<user_query>",
		"Earlier request before compact",
		"</user_query>",
		"",
		"### Turn 2 (Assistant)",
		"I started reading parsers.go",
		"",
	}, "\n")
	if err := os.WriteFile(filepath.Join(segDir, "segment_000.md"), []byte(segment), 0644); err != nil {
		t.Fatal(err)
	}
	lines := []string{
		`{"role":"user","message":{"content":[{"type":"text","text":"<timestamp>t</timestamp>\n<user_query>\nFix the parser\n</user_query>"}]}}`,
		`{"role":"assistant","message":{"content":[{"type":"text","text":"Editing parsers.go"},{"type":"tool_use","name":"Shell","input":{"command":"go test"}},{"type":"tool_use","name":"StrReplace","input":{"path":"parsers.go","old_string":"a","new_string":"b"}}]}}`,
		`{"type":"turn_ended","status":"success"}`,
	}
	if err := os.WriteFile(jsonl, []byte(strings.Join(lines, "\n")+"\n"), 0644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("CURSOR_HOME", tmp)
	t.Setenv("BATON_CONFIG_DIR", t.TempDir())
	candidates := discoverSessions("cursor", "Tool Upgrade Cursor")
	if len(candidates) != 1 || candidates[0].Path != jsonl || candidates[0].CWD != "/data/sources/baton" {
		t.Fatalf("Cursor fixture discovery: %+v", candidates)
	}
	ps, err := parseSessionFile("cursor", jsonl, time.Now())
	if err != nil {
		t.Fatalf("parse cursor: %v", err)
	}
	if ps.Title != "Tool Upgrade Cursor" || ps.CWD != "/data/sources/baton" {
		t.Fatalf("meta not applied: title=%q cwd=%q", ps.Title, ps.CWD)
	}
	if ps.PreCompactTurns != 3 || !strings.Contains(ps.CompactSummary, "extended for Cursor") {
		t.Fatalf("compaction: pre=%d summary=%q", ps.PreCompactTurns, ps.CompactSummary)
	}
	if len(ps.PostCompact) != 4 {
		t.Fatalf("expected 4 post-compact turns (user, text, shell, edit), got %d", len(ps.PostCompact))
	}
	if !strings.Contains(ps.PostCompact[0].Text, "Fix the parser") {
		t.Fatalf("post-compact user text: %q", ps.PostCompact[0].Text)
	}
	if len(ps.Commands) != 1 || ps.Commands[0] != "go test" {
		t.Fatalf("commands: %v", ps.Commands)
	}
	if len(ps.ModifiedFiles) != 1 || ps.ModifiedFiles[0] != "parsers.go" {
		t.Fatalf("modified files: %v", ps.ModifiedFiles)
	}
	_, _, preview := inspectSessionQuickMeta("cursor", jsonl)
	if preview != "Fix the parser" {
		t.Fatalf("preview: %q", preview)
	}

	t.Setenv("CURSOR_CONVERSATION_ID", id)
	if !isCallerOwnSession(jsonl) {
		t.Fatal("expected the live Cursor conversation to be excluded from import")
	}
	picked := selectCurrentSessionCandidate("all", []SessionCandidate{{
		Agent: "cursor", Path: jsonl, Title: ps.Title, CWD: ps.CWD,
	}})
	if picked.Path != jsonl {
		t.Fatalf("current session pick: %s", picked.Path)
	}
	if ag, q := resolveAgentAndQuery([]string{"cursor"}, "latest"); ag != "cursor" || q != "latest" {
		t.Fatalf("resolve cursor: %q %q", ag, q)
	}
	wantSkill := filepath.Join(tmp, "skills", "baton", "SKILL.md")
	foundSkill := false
	for _, p := range skillTargetPaths(tmp) {
		if p == wantSkill {
			foundSkill = true
		}
	}
	if !foundSkill {
		t.Fatalf("cursor skill path %s missing from %v", wantSkill, skillTargetPaths(tmp))
	}

	report := formatHandoffSummary(ps, t.TempDir(), nil)
	if !strings.Contains(report, "source [CURSOR]") || !strings.Contains(report, "Fix the parser") || !strings.Contains(report, "Handoff Status:") {
		t.Fatalf("cursor handoff missing expected text:\n%s", report)
	}
}

// recentSessionFiles returns jsonl transcripts under root, newest first, skipping
// empty files, files larger than maxBytes, and Claude subagent logs.
func recentSessionFiles(root string, maxBytes int64) []string {
	type stamped struct {
		mod  time.Time
		path string
	}
	var found []stamped
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".jsonl") {
			return nil
		}
		if strings.Contains(filepath.ToSlash(path), "/subagents/") {
			return nil
		}
		info, err := d.Info()
		if err != nil || info.Size() == 0 || info.Size() > maxBytes {
			return nil
		}
		found = append(found, stamped{mod: info.ModTime(), path: path})
		return nil
	})
	sort.Slice(found, func(i, j int) bool { return found[i].mod.After(found[j].mod) })
	out := make([]string, len(found))
	for i, f := range found {
		out[i] = f.path
	}
	return out
}

// userSnippet is a short slice of the stored user text that the handoff keeps verbatim.
func userSnippet(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	const max = 48
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

func firstUserText(ps *ParsedSession) string {
	for _, t := range ps.AllTurns {
		if t.Role == "user" && strings.TrimSpace(t.Text) != "" {
			return t.Text
		}
	}
	return ""
}

func handoffHas(t *testing.T, ps *ParsedSession, want string) {
	t.Helper()
	dir, err := writeHandoffDirectory(ps, nil)
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(dir, "active_context.md"))
	if err != nil {
		t.Fatal(err)
	}
	full, err := os.ReadFile(filepath.Join(dir, "full_dialogue.md"))
	if err != nil {
		t.Fatal(err)
	}
	combined := string(body) + "\n" + string(full)
	if !strings.Contains(combined, want) {
		t.Fatalf("handoff for [%s] %s missing %q", ps.Agent, ps.SourceFile, want)
	}
}

// TestRealHandoffDirections reads transcripts already on this machine.
// Claude and Codex sessions are handed off in the form Cursor's skill reads.
// This Cursor conversation is handed off in the form Codex and Claude read.
// The test skips when those files are not on the machine (upstream CI).
func TestRealHandoffDirections(t *testing.T) {
	if os.Getenv("BATON_TEST_REAL_SESSIONS") != "1" {
		t.Skip("set BATON_TEST_REAL_SESSIONS=1 to inspect local transcripts")
	}
	t.Setenv("TMPDIR", t.TempDir())
	t.Setenv("TMP", t.TempDir())
	t.Setenv("TEMP", t.TempDir())
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	const maxBytes = 2 << 20

	claudeFiles := recentSessionFiles(filepath.Join(claudeBaseDir(home), "projects"), maxBytes)
	if len(claudeFiles) == 0 {
		t.Skip("no Claude transcript under 2MB on this machine")
	}
	var claudePath string
	for _, path := range claudeFiles {
		ps, err := parseSessionFile("claude", path, time.Now())
		if err != nil || firstUserText(ps) == "" {
			continue
		}
		claudePath = path
		t.Logf("claude -> cursor: %s user=%q turns=%d cwd=%q", path, truncateInline(firstUserText(ps), 80), len(ps.AllTurns), ps.CWD)
		handoffHas(t, ps, "source [CLAUDE]")
		handoffHas(t, ps, userSnippet(firstUserText(ps)))
		break
	}
	if claudePath == "" {
		t.Fatal("Claude transcripts exist but none produced a user turn")
	}

	codexFiles := recentSessionFiles(filepath.Join(codexBaseDir(home), "sessions"), maxBytes)
	if len(codexFiles) == 0 {
		t.Skip("no Codex transcript under 2MB on this machine")
	}
	var codexPath string
	for _, path := range codexFiles {
		ps, err := parseSessionFile("codex", path, time.Now())
		if err != nil || firstUserText(ps) == "" {
			continue
		}
		codexPath = path
		t.Logf("codex -> cursor: %s user=%q turns=%d cwd=%q", path, truncateInline(firstUserText(ps), 80), len(ps.AllTurns), ps.CWD)
		handoffHas(t, ps, "source [CODEX]")
		handoffHas(t, ps, userSnippet(firstUserText(ps)))
		break
	}
	if codexPath == "" {
		t.Fatal("Codex transcripts exist but none produced a user turn")
	}

	id := "b416039b-a446-4d61-ac8e-4a21a42a07b6"
	matches, _ := filepath.Glob(filepath.Join(home, ".cursor", "projects", "*", "agent-transcripts", id, id+".jsonl"))
	if len(matches) == 0 {
		t.Skip("this Cursor conversation is not on this machine")
	}
	if len(matches) != 1 {
		t.Fatalf("this Cursor transcript: found %d files", len(matches))
	}
	// Meta lookup uses CURSOR_HOME. Keep the real ~/.cursor for this conversation.
	t.Setenv("CURSOR_HOME", filepath.Join(home, ".cursor"))
	var discovered SessionCandidate
	for _, c := range discoverSessions("cursor", "") {
		if strings.Contains(c.Path, id) {
			discovered = c
			break
		}
	}
	if discovered.Path == "" || discovered.Title != "Tool Upgrade Cursor" || discovered.CWD != "/data/sources/baton" {
		t.Fatalf("cursor discovery missed this session: %+v", discovered)
	}
	ps, err := parseSessionFile("cursor", matches[0], time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if ps.Title != "Tool Upgrade Cursor" || ps.CWD != "/data/sources/baton" {
		t.Fatalf("this session meta: title=%q cwd=%q", ps.Title, ps.CWD)
	}
	if !strings.Contains(firstUserText(ps), "support cursor") {
		t.Fatalf("this session user text does not include the original request: %q", truncateInline(firstUserText(ps), 200))
	}
	t.Logf("cursor -> codex and cursor -> claude: %s title=%q turns=%d post=%d", matches[0], ps.Title, len(ps.AllTurns), len(ps.PostCompact))
	for _, dest := range []string{"codex", "claude"} {
		dir := t.TempDir()
		report := formatHandoffSummary(ps, dir, nil)
		body, err := os.ReadFile(filepath.Join(dir, "active_context.md"))
		if err != nil {
			t.Fatal(err)
		}
		combined := report + "\n" + string(body)
		for _, want := range []string{"source [CURSOR]", "Tool Upgrade Cursor", "/data/sources/baton", "support cursor"} {
			if !strings.Contains(combined, want) {
				t.Fatalf("handoff for destination %s missing %q", dest, want)
			}
		}
		t.Logf("wrote %s handoff (%d bytes) for destination %s", filepath.Join(dir, "active_context.md"), len(body), dest)
	}
}
