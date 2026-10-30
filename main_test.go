package main

import (
	"encoding/binary"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

func TestParsePiTreeAndHermesSQLite(t *testing.T) {
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

	// 2. Test Hermes SQLite v32 schema if sqlite3 CLI is available
	if _, err := exec.LookPath("sqlite3"); err == nil {
		dbPath := filepath.Join(tmp, "state.db")
		schemaSQL := `
		CREATE TABLE sessions (
			id TEXT PRIMARY KEY,
			parent_session_id TEXT,
			title TEXT,
			display_name TEXT,
			cwd TEXT,
			started_at REAL,
			last_activity_at REAL,
			end_reason TEXT
		);
		CREATE TABLE messages (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			session_id TEXT,
			role TEXT,
			content TEXT,
			reasoning TEXT,
			reasoning_content TEXT,
			tool_name TEXT,
			tool_calls TEXT,
			_compressed_summary INTEGER DEFAULT 0,
			compacted INTEGER DEFAULT 0,
			active INTEGER DEFAULT 1
		);
		INSERT INTO sessions (id, parent_session_id, title, cwd, started_at, last_activity_at, end_reason)
		VALUES ('s-parent', NULL, 'hermes-migration', '/srv/app', 1700000000, 1700000100, 'compression');
		INSERT INTO sessions (id, parent_session_id, title, cwd, started_at, last_activity_at, end_reason)
		VALUES ('s-child', 's-parent', 'hermes-migration', '/srv/app', 1700000101, 1700000200, NULL);
		INSERT INTO messages (session_id, role, content, _compressed_summary) VALUES ('s-parent', 'user', 'Initial request in parent session', 0);
		INSERT INTO messages (session_id, role, content, _compressed_summary) VALUES ('s-child', 'system', 'Compressed summary of s-parent', 1);
		INSERT INTO messages (session_id, role, content, _compressed_summary) VALUES ('s-child', 'user', 'Follow-up after compression in Hermes', 0);
		`
		if out, err := exec.Command("sqlite3", dbPath, schemaSQL).CombinedOutput(); err != nil {
			t.Fatalf("sqlite3 init failed: %v (%s)", err, string(out))
		}
		psHermesDB, err := parseSessionFile("hermes", dbPath+"::s-child", time.Now())
		if err != nil {
			t.Fatalf("parseSessionFile hermes sqlite: %v", err)
		}
		if psHermesDB.Title != "hermes-migration" || psHermesDB.CompactSummary != "Compressed summary of s-parent" {
			t.Fatalf("unexpected Hermes SQLite parse: %+v", psHermesDB)
		}
		if len(psHermesDB.PostCompact) != 1 || !strings.Contains(psHermesDB.PostCompact[0].Text, "Follow-up after compression") {
			t.Fatalf("unexpected Hermes post-compact turns: %+v", psHermesDB.PostCompact)
		}
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

	// Verify Named Sessions & Candidate List fallback using isolated PI_CODING_AGENT_DIR
	tmp := t.TempDir()
	t.Setenv("PI_CODING_AGENT_DIR", tmp)
	sessDir := filepath.Join(tmp, "sessions", "--tmp--")
	if err := os.MkdirAll(sessDir, 0755); err != nil {
		t.Fatal(err)
	}
	sFile := filepath.Join(sessDir, "2026-10-30_s1.jsonl")
	content := `{"type":"session","version":3,"id":"s1","cwd":"/tmp"}` + "\n" +
		`{"type":"session_info","id":"i1","name":"billing-fix"}` + "\n" +
		`{"type":"message","id":"m1","parentId":"i1","message":{"role":"user","content":[{"type":"text","text":"Fix invoice rounding"}]}}` + "\n"
	if err := os.WriteFile(sFile, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	// 1. Load by session name ("billing-fix")
	reportNamed, err := loadSessionContext("pi", "billing-fix")
	if err != nil || !strings.Contains(reportNamed, "Fix invoice rounding") {
		t.Fatalf("expected named session load to succeed, err=%v, report=%s", err, reportNamed)
	}

	// 2. When user forgot the session name or gave an unknown name, return numbered candidate list
	reportCandidates, err := loadSessionContext("pi", "unknown-session-name")
	if err != nil || !strings.Contains(reportCandidates, "**#1**") || !strings.Contains(reportCandidates, "billing-fix") {
		t.Fatalf("expected numbered candidate list fallback, err=%v, report=%s", err, reportCandidates)
	}

	// 3. Load by candidate number ("1")
	reportByNum, err := loadSessionContext("pi", "1")
	if err != nil || !strings.Contains(reportByNum, "Fix invoice rounding") {
		t.Fatalf("expected candidate #1 load to succeed, err=%v, report=%s", err, reportByNum)
	}
}
