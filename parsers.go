package main

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

type SessionTurn struct {
	TurnID    int    `json:"turn_id"`
	Role      string `json:"role"`
	Timestamp string `json:"timestamp,omitempty"`
	Text      string `json:"text"`
	ToolName  string `json:"tool_name,omitempty"`
	RawOutput string `json:"raw_output,omitempty"`
}

type ParsedSession struct {
	Agent           string        `json:"agent"`
	SessionID       string        `json:"session_id"`
	Title           string        `json:"title,omitempty"`
	CWD             string        `json:"cwd,omitempty"`
	SourceFile      string        `json:"source_file"`
	ModTime         time.Time     `json:"mod_time"`
	PreCompactTurns int           `json:"pre_compact_turns"`
	CompactSummary  string        `json:"compact_summary,omitempty"`
	AllTurns        []SessionTurn `json:"all_turns"`
	PostCompact     []SessionTurn `json:"post_compact"`
	ModifiedFiles   []string      `json:"modified_files"`
	Commands        []string      `json:"commands"`
}

type SessionCandidate struct {
	Agent   string
	Path    string
	Title   string
	CWD     string
	ModTime time.Time
	Preview string
	ID      string // agent's own session ID; derived from Path when empty
}

var uuidPattern = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)

// agentSessionID extracts the agent's own session ID from a raw session path:
// the Hermes id after "::", otherwise the last UUID in the path (Claude/Pi/Codex
// file name, agy brain directory), otherwise the file name.
func agentSessionID(path string) string {
	if _, id, ok := strings.Cut(path, "::"); ok {
		return id
	}
	if all := uuidPattern.FindAllString(path, -1); len(all) > 0 {
		return strings.ToLower(all[len(all)-1])
	}
	base := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	return strings.TrimPrefix(base, "session_")
}

func (c SessionCandidate) SessionID() string {
	if c.ID != "" {
		return c.ID
	}
	return agentSessionID(c.Path)
}

// shortSessionID is the ID shown in lists: the first 8 characters of a UUID,
// or the whole ID otherwise (Hermes IDs start with a date and are not unique by prefix).
func shortSessionID(id string) string {
	if uuidPattern.MatchString(id) && len(id) > 8 {
		return id[:8]
	}
	return id
}

// findCandidateByID returns the only candidate whose session ID starts with q
// (at least 4 characters, case-insensitive).
func findCandidateByID(candidates []SessionCandidate, q string) (SessionCandidate, bool) {
	q = strings.ToLower(strings.TrimSpace(q))
	if len(q) < 4 {
		return SessionCandidate{}, false
	}
	var found []SessionCandidate
	for _, c := range candidates {
		if strings.HasPrefix(strings.ToLower(c.SessionID()), q) {
			found = append(found, c)
		}
	}
	if len(found) != 1 {
		return SessionCandidate{}, false
	}
	return found[0], true
}

// --- Cross-Platform Agent Base Directories (Linux, macOS, Windows) ---

func claudeBaseDir(home string) string {
	if env := os.Getenv("CLAUDE_CONFIG_DIR"); env != "" {
		return env
	}
	return filepath.Join(home, ".claude")
}

func codexBaseDir(home string) string {
	if env := os.Getenv("CODEX_HOME"); env != "" {
		return env
	}
	return filepath.Join(home, ".codex")
}

func piBaseDir(home string) string {
	if env := os.Getenv("PI_CODING_AGENT_DIR"); env != "" {
		return env
	}
	return filepath.Join(home, ".pi", "agent")
}

func hermesBaseDir(home string) string {
	if env := os.Getenv("HERMES_HOME"); env != "" {
		return env
	}
	if runtime.GOOS == "windows" {
		if localApp := os.Getenv("LOCALAPPDATA"); localApp != "" {
			cand := filepath.Join(localApp, "hermes")
			if _, err := os.Stat(cand); err == nil {
				return cand
			}
		}
	}
	return filepath.Join(home, ".hermes")
}

func agyBaseDirs(home string) []string {
	dirs := []string{
		filepath.Join(home, ".gemini", "antigravity-cli"),
		filepath.Join(home, ".gemini", "antigravity"),
	}
	if runtime.GOOS == "windows" {
		for _, envName := range []string{"APPDATA", "LOCALAPPDATA"} {
			if base := os.Getenv(envName); base != "" {
				dirs = append(dirs,
					filepath.Join(base, "Gemini", "antigravity-cli"),
					filepath.Join(base, "gemini", "antigravity-cli"),
				)
			}
		}
	}
	return dirs
}

// claudeProjectSlugs returns possible Claude Code project directory names for cwd
// across Linux, macOS, and Windows (where C:\foo\bar becomes C--foo-bar or -C--foo-bar).
func claudeProjectSlugs(cwd string) []string {
	replacer := strings.NewReplacer("/", "-", "\\", "-", ":", "-")
	slug := replacer.Replace(cwd)
	slugs := []string{slug}
	if !strings.HasPrefix(slug, "-") {
		slugs = append(slugs, "-"+slug)
	}
	return slugs
}

// piSafeSessionDirName implements Pi's exact directory encoding from session-manager.ts:
// `--${cwd.replace(/^[/\\]/, "").replace(/[/\\:]/g, "-")}--`
func piSafeSessionDirName(cwd string) string {
	trimmed := strings.TrimLeft(cwd, `/\`)
	replacer := strings.NewReplacer("/", "-", "\\", "-", ":", "-")
	return "--" + replacer.Replace(trimmed) + "--"
}

func handoffBaseDir() string {
	return filepath.Join(os.TempDir(), "baton-handoff")
}

func cleanupOldHandoffs() {
	base := handoffBaseDir()
	entries, err := os.ReadDir(base)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-24 * time.Hour)
	for _, e := range entries {
		if info, err := e.Info(); err == nil && info.ModTime().Before(cutoff) {
			_ = os.RemoveAll(filepath.Join(base, e.Name()))
		}
	}
}

// --- Baton Saved Sessions Store & Session Aliases ---

func savedSessionsDir() string {
	return filepath.Join(batonConfigDir(), "saved")
}

func sanitizeSessionFileName(name string) string {
	name = strings.TrimSpace(name)
	repl := strings.NewReplacer("/", "_", "\\", "_", ":", "_", "*", "_", "?", "_", "\"", "_", "<", "_", ">", "_", "|", "_")
	safe := repl.Replace(name)
	if safe == "" || safe == "." || safe == ".." {
		safe = fmt.Sprintf("session-%d", time.Now().Unix())
	}
	return safe
}

// savedSessionPath picks the store file for a session saved under name. A file that
// already holds a different session is never overwritten: the name gets a numeric
// suffix instead (BookTrans-2, BookTrans-3, ...). Re-saving the same session
// (same session_id) updates its existing file in place.
func savedSessionPath(dir, name, sessionID string) (string, string) {
	for n := 1; ; n++ {
		candidate := name
		if n > 1 {
			candidate = fmt.Sprintf("%s-%d", name, n)
		}
		path := filepath.Join(dir, sanitizeSessionFileName(candidate)+".baton.json")
		data, err := os.ReadFile(path)
		if err != nil {
			return path, candidate
		}
		var existing struct {
			SessionID string `json:"session_id"`
		}
		if json.Unmarshal(data, &existing) != nil || existing.SessionID == "" || existing.SessionID == sessionID {
			return path, candidate
		}
	}
}

// saveSessionToBatonStore writes a session snapshot into Baton's saved store and
// returns the file path and the final session name (which may carry a -N suffix
// if another session already uses the requested name).
func saveSessionToBatonStore(ps *ParsedSession, name string) (string, string, error) {
	if ps == nil {
		return "", "", fmt.Errorf("parsed session is nil")
	}
	dir := savedSessionsDir()
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", "", err
	}
	name = strings.TrimSpace(name)
	if name == "" {
		name = strings.TrimSpace(ps.Title)
	}
	if name == "" && ps.CWD != "" {
		name = filepath.Base(ps.CWD)
	}
	if name == "" {
		name = ps.SessionID
	}

	savePath, name := savedSessionPath(dir, name, ps.SessionID)

	cp := *ps
	cp.Title = name
	cp.ModTime = time.Now()
	if cp.CWD == "" {
		if cwd, err := os.Getwd(); err == nil && cwd != "" {
			cp.CWD = cwd
		}
	}
	// Keep only post-compaction active context in the saved snapshot so loading is instant
	cp.AllTurns = cp.PostCompact

	data, err := json.MarshalIndent(&cp, "", "  ")
	if err != nil {
		return "", "", err
	}
	if err := os.WriteFile(savePath, data, 0600); err != nil {
		return "", "", err
	}
	aliases := loadAliases()
	aliases[savePath] = name
	if ps.SourceFile != "" {
		aliases[ps.SourceFile] = name
	}
	_ = saveAliases(aliases)
	return savePath, name, nil
}

func clearSavedSessions() (string, error) {
	var removed int
	for _, sub := range []string{"saved", "imported", "vault_cache"} {
		dir := filepath.Join(batonConfigDir(), sub)
		if entries, err := os.ReadDir(dir); err == nil {
			for _, e := range entries {
				if strings.HasSuffix(e.Name(), ".baton.json") {
					removed++
				}
			}
		}
		_ = os.RemoveAll(dir)
	}
	_ = os.Remove(aliasesFilePath())
	_ = os.RemoveAll(handoffBaseDir())
	return fmt.Sprintf("Cleared all locally saved Baton sessions (%d saved session(s) removed).", removed), nil
}

// removeSavedSession implements `baton rm <name|#>`: deletes one session from Baton's
// saved store. The number refers to `baton list`; names must match exactly (no
// substring matching, so a typo never deletes a different session).
func removeSavedSession(query string) (string, error) {
	query = strings.TrimSpace(query)
	saved := discoverSavedBatonSessions("all", "")
	if query == "" {
		return "", fmt.Errorf("usage: baton rm <name|#> (numbers as shown by `baton list`)")
	}
	var chosen *SessionCandidate
	if idx, ok := parseCandidateIndex(query, len(saved)); ok {
		chosen = &saved[idx]
	}
	for i := range saved {
		if chosen == nil && strings.EqualFold(saved[i].Title, query) {
			chosen = &saved[i]
		}
	}
	if chosen == nil {
		return formatCandidateListReport("all", query, saved), nil
	}
	if err := os.Remove(chosen.Path); err != nil {
		return "", err
	}
	aliases := loadAliases()
	for path, name := range aliases {
		if path == chosen.Path || name == chosen.Title {
			delete(aliases, path)
		}
	}
	_ = saveAliases(aliases)
	return fmt.Sprintf("Removed saved session **%s** ([%s] `%s`). The original agent session on disk is untouched.",
		chosen.Title, chosen.Agent, filepath.Base(chosen.Path)), nil
}

func discoverSavedBatonSessions(agent, query string) []SessionCandidate {
	agent = strings.ToLower(strings.TrimSpace(agent))
	if agent == "" || agent == "auto" {
		agent = "all"
	}
	aliases := loadAliases()
	var out []SessionCandidate
	seen := make(map[string]bool)

	for _, sub := range []string{"saved", "imported"} {
		dir := filepath.Join(batonConfigDir(), sub)
		files, err := filepath.Glob(filepath.Join(dir, "*.baton.json"))
		if err != nil {
			continue
		}
		for _, m := range files {
			if seen[m] {
				continue
			}
			info, err := os.Stat(m)
			if err != nil || info.IsDir() || info.Size() == 0 {
				continue
			}
			data, err := os.ReadFile(m)
			if err != nil {
				continue
			}
			var meta struct {
				Agent       string        `json:"agent"`
				SessionID   string        `json:"session_id"`
				Title       string        `json:"title"`
				CWD         string        `json:"cwd"`
				PostCompact []SessionTurn `json:"post_compact"`
			}
			if json.Unmarshal(data, &meta) != nil {
				continue
			}
			ag := strings.ToLower(strings.TrimSpace(meta.Agent))
			if ag == "" {
				ag = "saved"
			}
			if agent != "all" && agent != ag {
				continue
			}
			title := strings.TrimSpace(meta.Title)
			if title == "" {
				title = aliases[m]
			}
			if title == "" {
				title = strings.TrimSuffix(filepath.Base(m), ".baton.json")
			}
			preview := ""
			for i := len(meta.PostCompact) - 1; i >= 0; i-- {
				if meta.PostCompact[i].Role != "user" {
					continue
				}
				if txt := cleanUserPrompt(meta.PostCompact[i].Text); txt != "" {
					preview = truncateInline(txt, 140)
					break
				}
			}
			if query != "" && !candidateMatchesQuery(m, title, preview, query) {
				continue
			}
			seen[m] = true
			out = append(out, SessionCandidate{
				Agent:   ag,
				Path:    m,
				Title:   title,
				CWD:     meta.CWD,
				ModTime: info.ModTime(),
				Preview: preview,
				ID:      agentSessionID(strings.TrimPrefix(meta.SessionID, ag+"-")),
			})
		}
	}

	sort.Slice(out, func(i, j int) bool {
		return out[i].ModTime.After(out[j].ModTime)
	})
	return out
}

func aliasesFilePath() string {
	return filepath.Join(batonConfigDir(), "aliases.json")
}

func loadAliases() map[string]string {
	out := make(map[string]string)
	data, err := os.ReadFile(aliasesFilePath())
	if err == nil {
		_ = json.Unmarshal(data, &out)
	}
	return out
}

func saveAliases(aliases map[string]string) error {
	path := aliasesFilePath()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(aliases, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0644)
}

// detectCallerAgent identifies which CLI coding agent is currently executing baton
// by inspecting agent-specific environment variables and walking the parent process tree.
func detectCallerAgent() string {
	if os.Getenv("CLAUDECODE") != "" || os.Getenv("CLAUDE_CODE_ENTRYPOINT") != "" {
		return "claude"
	}
	if os.Getenv("CODEX_SANDBOX") != "" || os.Getenv("CODEX_THREAD_ID") != "" || os.Getenv("CODEX_CI") != "" {
		return "codex"
	}
	if os.Getenv("PI_CODING_AGENT_DIR") != "" || os.Getenv("PI_SESSION_ID") != "" {
		return "pi"
	}
	if os.Getenv("HERMES_SESSION_ID") != "" {
		return "hermes"
	}
	if os.Getenv("ANTIGRAVITY_CLI") != "" {
		return "agy"
	}
	// Walk parent process tree on Linux/macOS if available
	pid := os.Getppid()
	for depth := 0; depth < 12 && pid > 1; depth++ {
		if envBytes, err := os.ReadFile(fmt.Sprintf("/proc/%d/environ", pid)); err == nil && len(envBytes) > 0 {
			envStr := string(envBytes)
			switch {
			case strings.Contains(envStr, "CLAUDECODE=") || strings.Contains(envStr, "CLAUDE_CODE_ENTRYPOINT="):
				return "claude"
			case strings.Contains(envStr, "CODEX_SANDBOX=") || strings.Contains(envStr, "CODEX_THREAD_ID="):
				return "codex"
			case strings.Contains(envStr, "PI_SESSION_ID="):
				return "pi"
			case strings.Contains(envStr, "HERMES_SESSION_ID="):
				return "hermes"
			case strings.Contains(envStr, "ANTIGRAVITY_CLI="):
				return "agy"
			}
		}
		cmdline, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
		if err != nil {
			break
		}
		lower := strings.ToLower(string(bytes.ReplaceAll(cmdline, []byte{0}, []byte{' '})))
		switch {
		case strings.Contains(lower, "claude"):
			return "claude"
		case strings.Contains(lower, "codex"):
			return "codex"
		case strings.Contains(lower, "antigravity") || strings.Contains(lower, "/agy ") || strings.HasSuffix(strings.TrimSpace(lower), "/agy"):
			return "agy"
		case strings.Contains(lower, "hermes"):
			return "hermes"
		}
		statData, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if err != nil {
			break
		}
		rparen := bytes.LastIndexByte(statData, ')')
		if rparen < 0 || rparen+2 >= len(statData) {
			break
		}
		fields := strings.Fields(string(statData[rparen+2:]))
		if len(fields) < 2 {
			break
		}
		nextPID, err := strconv.Atoi(fields[1])
		if err != nil || nextPID <= 1 || nextPID == pid {
			break
		}
		pid = nextPID
	}
	return ""
}

func cwdMatches(sessionCWD, currentCWD string) bool {
	s := filepath.Clean(strings.TrimSpace(sessionCWD))
	c := filepath.Clean(strings.TrimSpace(currentCWD))
	if s == "" || s == "." || c == "" || c == "." {
		return false
	}
	if strings.EqualFold(s, c) {
		return true
	}
	return false
}

// isHandoffStubSession returns true if a session was started as a handoff-receiver
// session (its first user prompt asks to load/list a baton session) and has not
// modified any workspace files yet.
func isHandoffStubSession(ps *ParsedSession) bool {
	if ps == nil || ps.PreCompactTurns > 0 || len(ps.ModifiedFiles) > 0 || strings.TrimSpace(ps.CompactSummary) != "" {
		return false
	}
	if strings.HasSuffix(ps.SourceFile, ".baton.json") {
		return false
	}
	userTurns := 0
	firstIsBaton := false
	for _, t := range ps.PostCompact {
		if t.Role == "user" {
			userTurns++
			if userTurns > 6 {
				return false
			}
			if userTurns == 1 {
				low := strings.ToLower(strings.TrimSpace(t.Text))
				firstIsBaton = strings.Contains(low, "baton") ||
					strings.Contains(low, "загрузи") ||
					strings.Contains(low, "продолжи сессию") ||
					strings.Contains(low, "восстанови сессию") ||
					strings.Contains(low, "load session") ||
					strings.Contains(low, "resume session") ||
					strings.Contains(low, "continue session")
			}
		}
	}
	return userTurns > 0 && firstIsBaton
}

func selectCurrentSessionCandidate(agent string, candidates []SessionCandidate) SessionCandidate {
	cwd, _ := os.Getwd()
	if codexThread := strings.TrimSpace(os.Getenv("CODEX_THREAD_ID")); codexThread != "" {
		for _, c := range candidates {
			if strings.Contains(filepath.Base(c.Path), codexThread) {
				return c
			}
		}
	}
	caller := detectCallerAgent()
	if (agent == "" || agent == "all" || agent == "auto") && caller != "" {
		// First pass: non-stub session of caller agent in current working directory
		for _, c := range candidates {
			if c.Agent == caller && cwdMatches(c.CWD, cwd) {
				if ps, err := parseSessionFile(c.Agent, c.Path, c.ModTime); err == nil && !isHandoffStubSession(ps) {
					return c
				}
			}
		}
		// Second pass: any session of caller agent in current working directory
		for _, c := range candidates {
			if c.Agent == caller && cwdMatches(c.CWD, cwd) {
				return c
			}
		}
	}
	// Third pass: non-stub session of any agent in current working directory
	for _, c := range candidates {
		if cwdMatches(c.CWD, cwd) {
			if ps, err := parseSessionFile(c.Agent, c.Path, c.ModTime); err == nil && !isHandoffStubSession(ps) {
				return c
			}
		}
	}
	return candidates[0]
}

// parseCandidate parses a raw session and fills the title and workdir known from the listing.
func parseCandidate(c SessionCandidate) (*ParsedSession, error) {
	ps, err := parseSessionFile(c.Agent, c.Path, c.ModTime)
	if err != nil {
		return nil, fmt.Errorf("failed to parse session %s: %w", c.Path, err)
	}
	if ps.Title == "" {
		ps.Title = c.Title
	}
	if ps.CWD == "" {
		ps.CWD = c.CWD
	}
	return ps, nil
}

// parseCurrentSession parses the raw session of the agent that is running baton
// (`baton save` and `baton push` work only with it; other sessions go through `baton import`).
func parseCurrentSession() (*ParsedSession, error) {
	candidates := discoverSessions("all", "")
	if len(candidates) == 0 {
		return nil, fmt.Errorf("no local agent sessions found")
	}
	return parseCandidate(selectCurrentSessionCandidate("all", candidates))
}

func nameSession(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", fmt.Errorf("session name cannot be empty")
	}
	ps, err := parseCurrentSession()
	if err != nil {
		return "", err
	}
	savePath, savedName, err := saveSessionToBatonStore(ps, name)
	if err != nil {
		return "", err
	}
	note := ""
	if savedName != name {
		note = fmt.Sprintf("\n\n> The name **%s** is already used by another saved session, so this one was saved as **%s**.", name, savedName)
	}
	return fmt.Sprintf("Saved session name: **%s** ([%s] `%s` -> `%s`)%s\n\nTo continue in another agent, paste this prompt:\n  `Load session %s using baton (if baton is not installed, install it from https://github.com/sukamenev/baton following INSTALL.md)`\nor run:\n  `/baton load %s`",
		savedName, ps.Agent, filepath.Base(ps.SourceFile), filepath.Base(savePath), note, savedName, savedName), nil
}

func parseCandidateIndex(q string, total int) (int, bool) {
	q = strings.TrimPrefix(strings.TrimSpace(q), "#")
	idx, err := strconv.Atoi(q)
	if err != nil || idx < 1 || idx > total {
		return 0, false
	}
	return idx - 1, true
}

func formatCandidateListReport(agent, unmatchedQuery string, candidates []SessionCandidate) string {
	if len(candidates) == 0 {
		return "No saved Baton sessions found. Save a session first with `baton save [name]` or `baton push [name]`, or import one with `baton load <URL>`."
	}
	limit := 10
	if len(candidates) < limit {
		limit = len(candidates)
	}

	var sb strings.Builder
	if unmatchedQuery != "" && !strings.EqualFold(unmatchedQuery, "candidates") && !strings.EqualFold(unmatchedQuery, "list") && unmatchedQuery != "?" {
		sb.WriteString(fmt.Sprintf("> No saved session matched `%s`. Showing the %d most recent saved Baton sessions below:\n\n", unmatchedQuery, limit))
	} else {
		sb.WriteString(fmt.Sprintf("# Saved Baton Sessions (showing top %d of %d)\n\n", limit, len(candidates)))
	}

	writeCandidateRows(&sb, candidates[:limit])
	sb.WriteString("\n**How to load a saved session:**\n")
	sb.WriteString("- Latest saved session: `baton load`\n")
	sb.WriteString("- By number: `baton load 1`\n")
	sb.WriteString("- By session name: `baton load <name>`\n")
	sb.WriteString("- By session ID: `baton load <id>`\n")
	sb.WriteString("\nThe session you need is not in this list (for example, the agent hit its limit before `baton save`)? `baton import` lists the agents' own sessions that can be imported into baton.\n")
	return sb.String()
}

// writeCandidateRows prints the list inside a code block: Markdown renderers in
// the agents would otherwise strip the indentation that sets rows apart.
func writeCandidateRows(sb *strings.Builder, candidates []SessionCandidate) {
	sb.WriteString("```text\n")
	for i, c := range candidates {
		title := c.Title
		if title == "" {
			title = "(unnamed)"
		}
		sb.WriteString(fmt.Sprintf("#%d  %s  [%s] %s  · id %s\n",
			i+1, c.ModTime.Format("2006-01-02 15:04"), c.Agent, title, shortSessionID(c.SessionID())))
		if c.CWD != "" {
			sb.WriteString(fmt.Sprintf("    %s\n", c.CWD))
		}
		if c.Preview != "" {
			sb.WriteString(fmt.Sprintf("    %s\n", c.Preview))
		}
	}
	sb.WriteString("```\n")
}

// --- `baton import`: pick a raw agent session that was never saved with `baton save` ---

const importListLimit = 15

// isCallerOwnSession reports whether a raw session file belongs to the agent
// session that is running baton right now (it is a fresh handoff receiver,
// not something to import).
func isCallerOwnSession(path string) bool {
	base := filepath.Base(path)
	for _, env := range []string{"CLAUDE_CODE_SESSION_ID", "CODEX_THREAD_ID", "PI_SESSION_ID", "HERMES_SESSION_ID"} {
		if id := strings.TrimSpace(os.Getenv(env)); id != "" && strings.Contains(base, id) {
			return true
		}
	}
	return false
}

// discoverImportCandidates returns raw on-disk sessions of all agents for `baton import`,
// skipping the caller's own running session and small "baton load" stub sessions
// so that the numbering shown by `baton import` matches `baton import <#>`.
func discoverImportCandidates() []SessionCandidate {
	var out []SessionCandidate
	for _, c := range discoverSessions("all", "") {
		if len(out) >= importListLimit {
			break
		}
		if isCallerOwnSession(c.Path) {
			continue
		}
		realPath := strings.SplitN(c.Path, "::", 2)[0]
		if info, err := os.Stat(realPath); err == nil && info.Size() < 4*1024*1024 && !strings.Contains(c.Path, "::") {
			if ps, err := parseSessionFile(c.Agent, c.Path, c.ModTime); err == nil && isHandoffStubSession(ps) {
				continue
			}
		}
		out = append(out, c)
	}
	return out
}

func formatImportListReport(unmatchedQuery string, candidates []SessionCandidate) string {
	if len(candidates) == 0 {
		return "No raw agent sessions found on disk."
	}
	var sb strings.Builder
	if unmatchedQuery != "" {
		sb.WriteString(fmt.Sprintf("> No agent session matched `%s`. Showing the %d most recent agent sessions below:\n\n", unmatchedQuery, len(candidates)))
	} else {
		sb.WriteString(fmt.Sprintf("# Agent Sessions Available for Import (top %d, current folder first)\n\n", len(candidates)))
	}
	writeCandidateRows(&sb, candidates)
	sb.WriteString("\n**How to import a session into baton and load it:**\n")
	sb.WriteString("- By number: `baton import 1`\n")
	sb.WriteString("- By session name: `baton import <name>`\n")
	sb.WriteString("- By session ID: `baton import <id>`\n")
	sb.WriteString("- Under another name: `baton import 1 as <name>`; without loading: add `--no-load`\n")
	return sb.String()
}

// parseImportArgs splits `baton import` arguments into the session query,
// the optional `as <name>` and the `--no-load` flag.
func parseImportArgs(args []string) (query, asName string, noLoad bool) {
	var q, as []string
	inAs := false
	for _, a := range args {
		switch {
		case a == "--no-load":
			noLoad = true
		case strings.EqualFold(a, "as") && !inAs && len(q) > 0:
			inAs = true
		case inAs:
			as = append(as, a)
		default:
			q = append(q, a)
		}
	}
	return strings.Join(q, " "), strings.Join(as, " "), noLoad
}

// findImportCandidate picks a session from the `baton import` list by number,
// exact name, ID prefix, or a name fragment that matches only one session.
func findImportCandidate(candidates []SessionCandidate, query string) (SessionCandidate, bool) {
	if idx, ok := parseCandidateIndex(query, len(candidates)); ok {
		return candidates[idx], true
	}
	for _, c := range candidates {
		if c.Title != "" && strings.EqualFold(c.Title, query) {
			return c, true
		}
	}
	if c, ok := findCandidateByID(candidates, query); ok {
		return c, true
	}
	qLower := strings.ToLower(query)
	var found []SessionCandidate
	for _, c := range candidates {
		if c.Title != "" && strings.Contains(strings.ToLower(c.Title), qLower) {
			found = append(found, c)
		}
	}
	if len(found) == 1 {
		return found[0], true
	}
	return SessionCandidate{}, false
}

// importSession implements `baton import [#|name|id] [as <name>] [--no-load]`:
// with no query it lists raw sessions of all agents; with a number, name, or ID
// from that list it saves the session into Baton's saved store and loads it
// (unless --no-load is given). Unknown queries return the list, never another session.
func importSession(args []string) (string, error) {
	cleanupOldHandoffs()
	query, asName, noLoad := parseImportArgs(args)
	candidates := discoverImportCandidates()
	if query == "" || strings.EqualFold(query, "list") || query == "?" {
		return formatImportListReport("", candidates), nil
	}

	chosen, ok := findImportCandidate(candidates, query)
	if !ok {
		return formatImportListReport(query, candidates), nil
	}

	ps, err := parseCandidate(chosen)
	if err != nil {
		return "", err
	}
	savePath, savedName, err := saveSessionToBatonStore(ps, asName)
	if err != nil {
		return "", err
	}
	imported := fmt.Sprintf("Imported [%s] session %s into baton as **%s** (`%s`).",
		chosen.Agent, shortSessionID(chosen.SessionID()), savedName, filepath.Base(savePath))
	if asName != "" && savedName != asName {
		imported += fmt.Sprintf(" The name **%s** is already used by another saved session.", asName)
	}
	if noLoad {
		return imported + fmt.Sprintf(" Load it with `baton load %s`.", savedName), nil
	}
	saved, err := parseSessionFile(ps.Agent, savePath, time.Now())
	if err != nil {
		return "", err
	}
	handoffDir, err := writeHandoffDirectory(saved, nil)
	if err != nil {
		return "", err
	}
	return imported + "\n\n" + formatHandoffSummary(saved, handoffDir, nil), nil
}

func listSessionsReport(agent, query string) string {
	candidates := discoverSavedBatonSessions(agent, query)
	return formatCandidateListReport(agent, "", candidates)
}

func loadSessionContext(agent, query string) (string, error) {
	cleanupOldHandoffs()
	qTrim := strings.TrimSpace(query)
	if strings.EqualFold(qTrim, "candidates") || strings.EqualFold(qTrim, "list") || qTrim == "?" {
		all := discoverSavedBatonSessions(agent, "")
		return formatCandidateListReport(agent, qTrim, all), nil
	}
	if qTrim == "" {
		qTrim = "latest"
	}

	parsed, err := findAndParseSession(agent, qTrim)
	if err != nil {
		// Fallback: if the user gave a name that doesn't match, return the numbered
		// list of saved session candidates so they can see available saved sessions.
		all := discoverSavedBatonSessions(agent, "")
		if len(all) > 0 {
			return formatCandidateListReport(agent, qTrim, all), nil
		}
		return "", err
	}
	handoffDir, err := writeHandoffDirectory(parsed, nil)
	if err != nil {
		return "", err
	}
	return formatHandoffSummary(parsed, handoffDir, nil), nil
}

func getSessionTurnDetails(sessionID string, turnID int) (string, error) {
	if sessionID == "" {
		return "", fmt.Errorf("session_id is required")
	}
	turnsPath := filepath.Join(handoffBaseDir(), filepath.Base(sessionID), "turns.json")
	data, err := os.ReadFile(turnsPath)
	if err != nil {
		return "", fmt.Errorf("failed to open %s: %w", turnsPath, err)
	}
	var turns []SessionTurn
	if err := json.Unmarshal(data, &turns); err != nil {
		return "", err
	}
	for _, t := range turns {
		if t.TurnID == turnID {
			var sb strings.Builder
			sb.WriteString(fmt.Sprintf("# Turn #%d (%s)\n", t.TurnID, t.Role))
			if t.ToolName != "" {
				sb.WriteString(fmt.Sprintf("Tool: `%s`\n", t.ToolName))
			}
			sb.WriteString("\n" + t.Text + "\n")
			if t.RawOutput != "" {
				sb.WriteString("\n## Full Tool Output:\n```\n" + t.RawOutput + "\n```\n")
			}
			return sb.String(), nil
		}
	}
	return "", fmt.Errorf("turn #%d not found in session %s", turnID, sessionID)
}

// findAndParseSession finds a session in Baton's saved store by number, name, or ID.
func findAndParseSession(agent, query string) (*ParsedSession, error) {
	if query == "" {
		query = "latest"
	}
	savedCandidates := discoverSavedBatonSessions(agent, "")
	if len(savedCandidates) == 0 {
		return nil, fmt.Errorf("no saved Baton sessions found (run 'baton save [name]' or 'baton push [name]' in the source agent first)")
	}
	if idx, ok := parseCandidateIndex(query, len(savedCandidates)); ok {
		chosen := savedCandidates[idx]
		return parseSessionFile(chosen.Agent, chosen.Path, chosen.ModTime)
	}
	if strings.EqualFold(query, "latest") {
		chosen := savedCandidates[0]
		return parseSessionFile(chosen.Agent, chosen.Path, chosen.ModTime)
	}
	qLower := strings.ToLower(query)
	for _, c := range savedCandidates {
		if c.Title != "" && strings.EqualFold(c.Title, query) {
			return parseSessionFile(c.Agent, c.Path, c.ModTime)
		}
	}
	if c, ok := findCandidateByID(savedCandidates, query); ok {
		return parseSessionFile(c.Agent, c.Path, c.ModTime)
	}
	for _, c := range savedCandidates {
		if c.Title != "" && strings.Contains(strings.ToLower(c.Title), qLower) {
			return parseSessionFile(c.Agent, c.Path, c.ModTime)
		}
	}
	filteredSaved := discoverSavedBatonSessions(agent, query)
	if len(filteredSaved) > 0 {
		chosen := filteredSaved[0]
		return parseSessionFile(chosen.Agent, chosen.Path, chosen.ModTime)
	}
	return nil, fmt.Errorf("no saved Baton session matching '%s'", query)
}

func loadAgyWorkspaces(agyRoot string) map[string]string {
	wsMap := make(map[string]string)
	histPath := filepath.Join(agyRoot, "history.jsonl")
	f, err := os.Open(histPath)
	if err != nil {
		return wsMap
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 2*1024*1024)
	for scanner.Scan() {
		var row map[string]interface{}
		if json.Unmarshal(scanner.Bytes(), &row) == nil {
			cid, _ := row["conversationId"].(string)
			ws, _ := row["workspace"].(string)
			if cid != "" && ws != "" {
				wsMap[cid] = ws
			}
		}
	}
	return wsMap
}

func loadCodexThreadNames(codexRoot string) map[string]string {
	titles := make(map[string]string)
	idxPath := filepath.Join(codexRoot, "session_index.jsonl")
	f, err := os.Open(idxPath)
	if err != nil {
		return titles
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var row map[string]interface{}
		if json.Unmarshal(scanner.Bytes(), &row) == nil {
			id, _ := row["id"].(string)
			name, _ := row["thread_name"].(string)
			if name == "" {
				name, _ = row["title"].(string)
			}
			if id != "" && name != "" {
				titles[id] = name
			}
		}
	}
	return titles
}

func discoverSessions(agent, query string) []SessionCandidate {
	home, _ := os.UserHomeDir()
	cwd, _ := os.Getwd()
	agent = strings.ToLower(strings.TrimSpace(agent))
	if agent == "" || agent == "auto" {
		agent = "all"
	}

	aliases := loadAliases()
	var codexTitles map[string]string
	if agent == "all" || agent == "codex" {
		codexTitles = loadCodexThreadNames(codexBaseDir(home))
	}

	var all []SessionCandidate
	seen := make(map[string]bool)

	addFile := func(ag, path, defaultTitle, defaultCWD string) {
		if seen[path] {
			return
		}
		info, err := os.Stat(path)
		if err != nil || info.IsDir() || info.Size() == 0 {
			return
		}
		title := aliases[path]
		if title == "" {
			title = defaultTitle
		}
		if query != "" {
			metaTitle, metaCWD, preview := inspectSessionQuickMeta(ag, path)
			if title == "" {
				title = metaTitle
			}
			if metaCWD != "" {
				defaultCWD = metaCWD
			}
			if !candidateMatchesQuery(path, title, preview, query) {
				return
			}
			seen[path] = true
			all = append(all, SessionCandidate{
				Agent:   ag,
				Path:    path,
				Title:   title,
				CWD:     defaultCWD,
				ModTime: info.ModTime(),
				Preview: preview,
			})
			return
		}
		seen[path] = true
		all = append(all, SessionCandidate{
			Agent:   ag,
			Path:    path,
			Title:   title,
			CWD:     defaultCWD,
			ModTime: info.ModTime(),
		})
	}

	if agent == "all" || agent == "claude" {
		cBase := claudeBaseDir(home)
		for _, slug := range claudeProjectSlugs(cwd) {
			projDir := filepath.Join(cBase, "projects", slug)
			matches, _ := filepath.Glob(filepath.Join(projDir, "*.jsonl"))
			for _, m := range matches {
				addFile("claude", m, "", cwd)
			}
		}
		allMatches, _ := filepath.Glob(filepath.Join(cBase, "projects", "*", "*.jsonl"))
		for _, m := range allMatches {
			addFile("claude", m, "", "")
		}
	}

	if agent == "all" || agent == "codex" {
		codexRoot := filepath.Join(codexBaseDir(home), "sessions")
		_ = filepath.WalkDir(codexRoot, func(path string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() && strings.HasSuffix(path, ".jsonl") {
				t := ""
				for id, name := range codexTitles {
					if strings.Contains(filepath.Base(path), id) {
						t = name
						break
					}
				}
				addFile("codex", path, t, "")
			}
			return nil
		})
	}

	if agent == "all" || agent == "agy" {
		for _, agyRoot := range agyBaseDirs(home) {
			agyWS := loadAgyWorkspaces(agyRoot)
			brainLogs, _ := filepath.Glob(filepath.Join(agyRoot, "brain", "*", ".system_generated", "logs", "transcript_full.jsonl"))
			if len(brainLogs) == 0 {
				brainLogs, _ = filepath.Glob(filepath.Join(agyRoot, "brain", "*", ".system_generated", "logs", "transcript.jsonl"))
			}
			for _, m := range brainLogs {
				ws := ""
				parts := strings.Split(filepath.ToSlash(m), "/")
				for idx, p := range parts {
					if p == "brain" && idx+1 < len(parts) {
						ws = agyWS[parts[idx+1]]
						break
					}
				}
				addFile("agy", m, "", ws)
			}
			pbFiles, _ := filepath.Glob(filepath.Join(agyRoot, "conversations", "*.pb"))
			for _, m := range pbFiles {
				cid := strings.TrimSuffix(filepath.Base(m), ".pb")
				addFile("agy", m, "", agyWS[cid])
			}
		}
	}

	if agent == "all" || agent == "pi" {
		piRoot := filepath.Join(piBaseDir(home), "sessions")
		// Prioritize current working directory's safe session folder first
		cwdSafeDir := filepath.Join(piRoot, piSafeSessionDirName(cwd))
		if matches, err := filepath.Glob(filepath.Join(cwdSafeDir, "*.jsonl")); err == nil {
			for _, m := range matches {
				addFile("pi", m, "", cwd)
			}
		}
		piDirs := []string{
			piRoot,
			filepath.Join(cwd, ".pi", "sessions"),
		}
		for _, dir := range piDirs {
			_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
				if err == nil && !d.IsDir() && strings.HasSuffix(path, ".jsonl") {
					addFile("pi", path, "", "")
				}
				return nil
			})
		}
	}

	if agent == "all" || agent == "hermes" {
		hBase := hermesBaseDir(home)
		for _, sub := range []string{"sessions", "trajectories"} {
			hRoot := filepath.Join(hBase, sub)
			_ = filepath.WalkDir(hRoot, func(path string, d fs.DirEntry, err error) error {
				if err == nil && !d.IsDir() && (strings.HasSuffix(path, ".jsonl") || strings.HasSuffix(path, ".json")) {
					addFile("hermes", path, "", "")
				}
				return nil
			})
		}
		for _, dbPath := range discoverHermesDBPaths(home, hBase) {
			dbCandidates := discoverHermesSQLiteSessions(dbPath, aliases, query)
			all = append(all, dbCandidates...)
		}
	}

	// Include sessions imported from remote URLs via `baton load <URL>`
	importDir := filepath.Join(filepath.Dir(aliasesFilePath()), "imported")
	if importFiles, err := filepath.Glob(filepath.Join(importDir, "*.baton.json")); err == nil {
		for _, m := range importFiles {
			ag := "imported"
			base := filepath.Base(m)
			for _, known := range []string{"claude", "codex", "agy", "pi", "hermes"} {
				if strings.HasPrefix(base, known+"-") {
					ag = known
					break
				}
			}
			if agent == "all" || agent == ag {
				addFile(ag, m, aliases[m], "")
			}
		}
	}

	sort.Slice(all, func(i, j int) bool {
		return all[i].ModTime.After(all[j].ModTime)
	})
	if query == "" {
		limit := 30
		if len(all) < limit {
			limit = len(all)
		}
		for i := 0; i < limit; i++ {
			if !strings.Contains(all[i].Path, "::") {
				metaTitle, metaCWD, preview := inspectSessionQuickMeta(all[i].Agent, all[i].Path)
				if all[i].Title == "" {
					all[i].Title = metaTitle
				}
				if all[i].CWD == "" {
					all[i].CWD = metaCWD
				}
				if all[i].Preview == "" {
					all[i].Preview = preview
				}
			}
		}
		// Prioritize sessions belonging to the current working directory first,
		// ordered by ModTime descending, followed by sessions from other directories.
		sort.SliceStable(all, func(i, j int) bool {
			matchI := cwdMatches(all[i].CWD, cwd)
			matchJ := cwdMatches(all[j].CWD, cwd)
			if matchI != matchJ {
				return matchI
			}
			return all[i].ModTime.After(all[j].ModTime)
		})
	}
	return all
}

func discoverHermesDBPaths(home, hBase string) []string {
	var paths []string
	seen := make(map[string]bool)
	addDB := func(p string) {
		if !seen[p] {
			if info, err := os.Stat(p); err == nil && !info.IsDir() && info.Size() > 0 {
				seen[p] = true
				paths = append(paths, p)
			}
		}
	}
	for _, name := range []string{"state.db", "history.db"} {
		addDB(filepath.Join(hBase, name))
		addDB(filepath.Join(home, ".hermes", name))
	}
	if profMatches, err := filepath.Glob(filepath.Join(hBase, "profiles", "*", "state.db")); err == nil {
		for _, m := range profMatches {
			addDB(m)
		}
	}
	if runtime.GOOS == "windows" {
		for _, env := range []string{"LOCALAPPDATA", "APPDATA"} {
			if v := os.Getenv(env); v != "" {
				addDB(filepath.Join(v, "hermes", "state.db"))
			}
		}
	}
	return paths
}

func discoverHermesSQLiteSessions(dbPath string, aliases map[string]string, query string) []SessionCandidate {
	var out []SessionCandidate
	info, err := os.Stat(dbPath)
	if err != nil {
		return out
	}

	sql := `SELECT s.id, COALESCE(s.title, s.display_name, ''), COALESCE(s.cwd, ''), COALESCE(s.last_activity_at, s.started_at, 0),
	        COALESCE((SELECT content FROM messages m WHERE m.session_id = s.id AND m.role = 'user' ORDER BY m.id ASC LIMIT 1), '')
	        FROM sessions s ORDER BY COALESCE(s.last_activity_at, s.started_at, 0) DESC LIMIT 25;`
	raw, err := exec.Command("sqlite3", "-json", dbPath, sql).Output()
	if err == nil && len(raw) > 0 {
		var rows []map[string]interface{}
		if json.Unmarshal(raw, &rows) == nil && len(rows) > 0 {
			for _, r := range rows {
				var id, title, cwd, preview string
				var ts float64
				for k, v := range r {
					kl := strings.ToLower(k)
					switch {
					case kl == "id":
						id, _ = v.(string)
					case strings.Contains(kl, "title"):
						title, _ = v.(string)
					case strings.Contains(kl, "cwd"):
						cwd, _ = v.(string)
					case strings.Contains(kl, "last_activity") || strings.Contains(kl, "started_at"):
						ts, _ = v.(float64)
					default:
						if s, ok := v.(string); ok && preview == "" {
							preview = truncateInline(s, 140)
						}
					}
				}
				if id == "" {
					continue
				}
				virtualPath := dbPath + "::" + id
				if a := aliases[virtualPath]; a != "" {
					title = a
				}
				modTime := info.ModTime()
				if ts > 1e9 {
					modTime = time.Unix(int64(ts), 0)
				}
				if query != "" && !candidateMatchesQuery(virtualPath, title, preview, query) {
					continue
				}
				out = append(out, SessionCandidate{
					Agent:   "hermes",
					Path:    virtualPath,
					Title:   title,
					CWD:     cwd,
					ModTime: modTime,
					Preview: preview,
				})
			}
			return out
		}
	}

	// Fallback if sessions table is absent or sqlite3 CLI is unavailable
	title := aliases[dbPath]
	if query == "" || candidateMatchesQuery(dbPath, title, "Hermes SQLite database", query) {
		out = append(out, SessionCandidate{
			Agent:   "hermes",
			Path:    dbPath,
			Title:   title,
			ModTime: info.ModTime(),
			Preview: "Hermes SQLite session database (" + filepath.Base(dbPath) + ")",
		})
	}
	return out
}

func candidateMatchesQuery(path, title, preview, query string) bool {
	q := strings.ToLower(query)
	if title != "" && strings.Contains(strings.ToLower(title), q) {
		return true
	}
	if strings.Contains(strings.ToLower(filepath.Base(path)), q) {
		return true
	}
	if preview != "" && strings.Contains(strings.ToLower(preview), q) {
		return true
	}
	realPath := strings.Split(path, "::")[0]
	data, err := os.ReadFile(realPath)
	if err != nil {
		return false
	}
	return bytes.Contains(bytes.ToLower(data), []byte(q))
}

func inspectSessionQuickMeta(agent, path string) (title, cwd, preview string) {
	if strings.HasSuffix(path, ".pb") {
		return "", "", "Protobuf binary snapshot"
	}
	if strings.HasSuffix(path, ".baton.json") {
		if data, err := os.ReadFile(path); err == nil {
			var imported struct {
				Title string `json:"title"`
				CWD   string `json:"cwd"`
			}
			if json.Unmarshal(data, &imported) == nil {
				return imported.Title, imported.CWD, "Imported remote baton session"
			}
		}
		return "", "", "Imported remote baton session"
	}
	f, err := os.Open(path)
	if err != nil {
		return "", "", ""
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 2*1024*1024)
	linesChecked := 0
	for scanner.Scan() && linesChecked < 50 {
		linesChecked++
		line := scanner.Text()
		var row map[string]interface{}
		if json.Unmarshal([]byte(line), &row) == nil {
			typ, _ := row["type"].(string)
			if typ == "session" || typ == "session_meta" {
				if c, ok := row["cwd"].(string); ok && cwd == "" {
					cwd = c
				}
				if p, ok := row["payload"].(map[string]interface{}); ok {
					if c, ok := p["cwd"].(string); ok && cwd == "" {
						cwd = c
					}
				}
			}
			if typ == "session_info" {
				if n, ok := row["name"].(string); ok && strings.TrimSpace(n) != "" {
					title = strings.TrimSpace(n)
				}
			}
			if typ == "custom-title" {
				if ct, ok := row["customTitle"].(string); ok && strings.TrimSpace(ct) != "" {
					title = strings.TrimSpace(ct)
				}
			}
			if ct, ok := row["customTitle"].(string); ok && strings.TrimSpace(ct) != "" && title == "" {
				title = strings.TrimSpace(ct)
			}
			if c, ok := row["cwd"].(string); ok && cwd == "" {
				cwd = c
			}
			if preview == "" {
				preview = userPromptFromRow(row)
			}
		}
	}
	// Prefer the last user prompt (where the session stopped) over the first one.
	if last := lastUserPromptInTail(f, 256*1024); last != "" {
		preview = last
	}
	return title, cwd, truncateInline(preview, 140)
}

// lastUserPromptInTail scans the last tailBytes of a JSONL session file and
// returns the most recent user prompt found there.
func lastUserPromptInTail(f *os.File, tailBytes int64) string {
	info, err := f.Stat()
	if err != nil {
		return ""
	}
	offset := info.Size() - tailBytes
	if offset < 0 {
		offset = 0
	}
	if _, err := f.Seek(offset, 0); err != nil {
		return ""
	}
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
	if offset > 0 {
		scanner.Scan() // skip the partial first line
	}
	last := ""
	for scanner.Scan() {
		var row map[string]interface{}
		if json.Unmarshal(scanner.Bytes(), &row) == nil {
			if p := userPromptFromRow(row); p != "" {
				last = p
			}
		}
	}
	return last
}

// userPromptFromRow extracts the text typed by the user from one JSONL row of any
// supported agent (Claude, Codex, AGY, Pi), skipping tool results and injected
// meta/system messages.
func userPromptFromRow(row map[string]interface{}) string {
	if isMeta, _ := row["isMeta"].(bool); isMeta {
		return ""
	}
	if typ, _ := row["type"].(string); typ == "USER_INPUT" {
		s, _ := row["content"].(string)
		if i := strings.Index(s, "<USER_REQUEST>"); i >= 0 {
			s = s[i+len("<USER_REQUEST>"):]
			if j := strings.Index(s, "</USER_REQUEST>"); j >= 0 {
				s = s[:j]
			}
		}
		return cleanUserPrompt(s)
	}
	msg, _ := row["message"].(map[string]interface{})
	if msg == nil {
		msg, _ = row["payload"].(map[string]interface{})
	}
	if msg == nil || msg["role"] != "user" {
		return ""
	}
	switch content := msg["content"].(type) {
	case string:
		return cleanUserPrompt(content)
	case []interface{}:
		for _, part := range content {
			p, _ := part.(map[string]interface{})
			if t, _ := p["type"].(string); t == "text" || t == "input_text" {
				text, _ := p["text"].(string)
				if s := cleanUserPrompt(text); s != "" {
					return s
				}
			}
		}
	}
	return ""
}

func cleanUserPrompt(s string) string {
	s = strings.TrimSpace(s)
	// Agent-injected context: <environment_context>, <command-name>, <system-reminder>,
	// <local-command-caveat>, AGENTS.md instructions, etc.
	if s == "" || strings.HasPrefix(s, "<") || strings.HasPrefix(s, "# AGENTS.md instructions") ||
		strings.HasPrefix(s, "Base directory for this skill:") ||
		(strings.HasPrefix(s, "[") && strings.HasSuffix(s, "]") && !strings.Contains(s, "\n")) {
		return ""
	}
	// Bare baton commands say nothing about what the session was about.
	low := strings.ToLower(s)
	if low == "baton" || strings.HasPrefix(low, "baton ") || strings.HasPrefix(low, "/baton") {
		return ""
	}
	return s
}

func parseSessionFile(agent, path string, modTime time.Time) (*ParsedSession, error) {
	realPath := path
	explicitSessionID := ""
	if parts := strings.SplitN(path, "::", 2); len(parts) == 2 {
		realPath = parts[0]
		explicitSessionID = parts[1]
	}

	base := strings.TrimSuffix(filepath.Base(realPath), filepath.Ext(realPath))
	if explicitSessionID != "" {
		base = explicitSessionID
	} else if base == "transcript_full" || base == "transcript" {
		parts := strings.Split(filepath.ToSlash(realPath), "/")
		for i, p := range parts {
			if p == "brain" && i+1 < len(parts) {
				base = parts[i+1]
				break
			}
		}
	}

	ps := &ParsedSession{
		Agent:      agent,
		SessionID:  fmt.Sprintf("%s-%s", agent, base),
		SourceFile: realPath,
		ModTime:    modTime,
	}

	if strings.HasSuffix(realPath, ".baton.json") {
		data, err := os.ReadFile(realPath)
		if err != nil {
			return nil, err
		}
		var imported ParsedSession
		if err := json.Unmarshal(data, &imported); err != nil {
			return nil, err
		}
		imported.ModTime = modTime
		return &imported, nil
	}

	if strings.HasSuffix(realPath, ".pb") {
		if err := parseProtobufSession(realPath, ps); err != nil {
			return nil, err
		}
		return ps, nil
	}

	if strings.HasSuffix(realPath, ".db") || strings.HasSuffix(realPath, ".sqlite") {
		if err := parseHermesSQLiteSession(realPath, explicitSessionID, ps); err != nil {
			return nil, err
		}
		return ps, nil
	}

	if strings.HasSuffix(realPath, ".json") && agent == "hermes" {
		if err := parseHermesJSONFile(realPath, ps); err != nil {
			return nil, err
		}
		return ps, nil
	}

	switch agent {
	case "claude":
		err := parseClaudeJSONL(realPath, ps)
		return ps, err
	case "agy":
		err := parseAgyTranscriptJSONL(realPath, ps)
		return ps, err
	case "pi":
		err := parsePiJSONL(realPath, ps)
		return ps, err
	default:
		err := parseCodexOrGenericJSONL(realPath, ps)
		return ps, err
	}
}

func parseClaudeJSONL(path string, ps *ParsedSession) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	modSet := make(map[string]bool)
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024*1024), 64*1024*1024)
	turnID := 0

	addTurn := func(t SessionTurn) {
		turnID++
		t.TurnID = turnID
		ps.AllTurns = append(ps.AllTurns, t)
		ps.PostCompact = append(ps.PostCompact, t)
	}

	markCompact := func(summary string) {
		ps.PreCompactTurns = turnID
		ps.CompactSummary = strings.TrimSpace(summary)
		ps.PostCompact = nil
		if len(ps.Commands) > 50 {
			ps.Commands = ps.Commands[len(ps.Commands)-50:]
		}
	}

	for scanner.Scan() {
		var row map[string]interface{}
		if err := json.Unmarshal(scanner.Bytes(), &row); err != nil {
			continue
		}
		typ, _ := row["type"].(string)
		if ct, _ := row["customTitle"].(string); strings.TrimSpace(ct) != "" {
			ps.Title = strings.TrimSpace(ct)
		}
		if c, _ := row["cwd"].(string); c != "" && ps.CWD == "" {
			ps.CWD = c
		}

		if typ == "summary" {
			if sumText, ok := row["summary"].(string); ok && strings.TrimSpace(sumText) != "" {
				markCompact(sumText)
				continue
			}
		}

		if isCompact, ok := row["isCompactSummary"].(bool); ok && isCompact {
			if msg, ok := row["message"].(map[string]interface{}); ok {
				if c, ok := msg["content"].(string); ok && strings.TrimSpace(c) != "" {
					markCompact(c)
					continue
				}
			}
		}

		msg, _ := row["message"].(map[string]interface{})
		if msg == nil {
			continue
		}

		switch typ {
		case "user":
			switch content := msg["content"].(type) {
			case string:
				if strings.HasPrefix(strings.TrimSpace(content), "<compact_summary>") {
					markCompact(content)
					continue
				}
				if strings.TrimSpace(content) != "" {
					addTurn(SessionTurn{Role: "user", Text: content})
				}
			case []interface{}:
				for _, item := range content {
					block, _ := item.(map[string]interface{})
					bType, _ := block["type"].(string)
					switch bType {
					case "text":
						txt, _ := block["text"].(string)
						if strings.TrimSpace(txt) != "" {
							addTurn(SessionTurn{Role: "user", Text: txt})
						}
					case "tool_result":
						raw := extractContentText(block["content"])
						preview := summarizeToolOutput(raw)
						addTurn(SessionTurn{
							Role:      "tool_result",
							Text:      preview,
							RawOutput: raw,
						})
					}
				}
			}

		case "assistant":
			contents, ok := msg["content"].([]interface{})
			if !ok {
				if str, ok := msg["content"].(string); ok && strings.TrimSpace(str) != "" {
					addTurn(SessionTurn{Role: "assistant", Text: str})
				}
				continue
			}
			for _, item := range contents {
				block, _ := item.(map[string]interface{})
				bType, _ := block["type"].(string)
				switch bType {
				case "thinking":
					think, _ := block["thinking"].(string)
					if strings.TrimSpace(think) != "" {
						addTurn(SessionTurn{Role: "assistant_thinking", Text: think})
					}
				case "text":
					txt, _ := block["text"].(string)
					if strings.TrimSpace(txt) != "" {
						addTurn(SessionTurn{Role: "assistant", Text: txt})
					}
				case "tool_use":
					name, _ := block["name"].(string)
					input, _ := block["input"].(map[string]interface{})
					recordToolSideEffects(name, input, modSet, &ps.Commands)
					argBytes, _ := json.Marshal(input)
					addTurn(SessionTurn{
						Role:      "tool_use",
						ToolName:  name,
						Text:      fmt.Sprintf("Call `%s`: %s", name, truncateInline(string(argBytes), 300)),
						RawOutput: fmt.Sprintf("Call `%s`: %s", name, string(argBytes)),
					})
				}
			}
		}
	}

	ps.ModifiedFiles = sortedKeys(modSet)
	return nil
}

// parsePiJSONL implements exact parsing for Pi coding agent's tree-based JSONL
// format (version 3, badlogic/pi-mono/packages/coding-agent/src/core/session-manager.ts).
func parsePiJSONL(path string, ps *ParsedSession) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	type piEntry struct {
		id       string
		parentID string
		typ      string
		raw      map[string]interface{}
	}

	var entries []piEntry
	byID := make(map[string]piEntry)
	var leafID string

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024*1024), 64*1024*1024)

	for scanner.Scan() {
		var row map[string]interface{}
		if err := json.Unmarshal(scanner.Bytes(), &row); err != nil {
			continue
		}
		typ, _ := row["type"].(string)
		if typ == "session" {
			if c, _ := row["cwd"].(string); c != "" {
				ps.CWD = c
			}
			continue
		}
		if typ == "session_info" {
			if n, _ := row["name"].(string); strings.TrimSpace(n) != "" {
				ps.Title = strings.TrimSpace(n)
			}
		}
		id, _ := row["id"].(string)
		parentID, _ := row["parentId"].(string)
		e := piEntry{id: id, parentID: parentID, typ: typ, raw: row}
		entries = append(entries, e)
		if id != "" {
			byID[id] = e
			leafID = id
		}
	}

	// Reconstruct active tree path from leafId to root if IDs and parentIds are used
	activePath := entries
	if leafID != "" && len(byID) > 0 {
		var reversed []piEntry
		seen := make(map[string]bool)
		currID := leafID
		for currID != "" && !seen[currID] {
			seen[currID] = true
			e, ok := byID[currID]
			if !ok {
				break
			}
			reversed = append(reversed, e)
			currID = e.parentID
		}
		// Only use parentId walk if it linked all the way or if entries explicitly form a tree
		if len(reversed) > 1 || len(entries) == 1 {
			activePath = make([]piEntry, len(reversed))
			for i := range reversed {
				activePath[i] = reversed[len(reversed)-1-i]
			}
		}
	}

	modSet := make(map[string]bool)
	turnID := 0
	entryFirstTurn := make(map[string]int)
	var latestFirstKeptID string

	addTurn := func(entryID string, t SessionTurn) {
		turnID++
		t.TurnID = turnID
		if entryID != "" && entryFirstTurn[entryID] == 0 {
			entryFirstTurn[entryID] = turnID
		}
		ps.AllTurns = append(ps.AllTurns, t)
		ps.PostCompact = append(ps.PostCompact, t)
	}

	for _, e := range activePath {
		row := e.raw
		switch e.typ {
		case "compaction":
			summary, _ := row["summary"].(string)
			firstKept, _ := row["firstKeptEntryId"].(string)
			if details, ok := row["details"].(map[string]interface{}); ok {
				if mFiles, ok := details["modifiedFiles"].([]interface{}); ok {
					for _, mf := range mFiles {
						if s, ok := mf.(string); ok && s != "" {
							modSet[s] = true
						}
					}
				}
			}
			ps.PreCompactTurns = turnID
			ps.CompactSummary = strings.TrimSpace(summary)
			latestFirstKeptID = firstKept
			ps.PostCompact = nil

		case "message":
			msgObj, _ := row["message"].(map[string]interface{})
			if msgObj == nil {
				continue
			}
			mRole, _ := msgObj["role"].(string)
			switch mRole {
			case "user":
				txt := extractContentText(msgObj["content"])
				if strings.TrimSpace(txt) != "" {
					addTurn(e.id, SessionTurn{Role: "user", Text: txt})
				}
			case "assistant":
				if blocks, ok := msgObj["content"].([]interface{}); ok {
					for _, item := range blocks {
						b, _ := item.(map[string]interface{})
						bType, _ := b["type"].(string)
						switch bType {
						case "thinking":
							th, _ := b["thinking"].(string)
							if strings.TrimSpace(th) != "" {
								addTurn(e.id, SessionTurn{Role: "assistant_thinking", Text: th})
							}
						case "text":
							tx, _ := b["text"].(string)
							if strings.TrimSpace(tx) != "" {
								addTurn(e.id, SessionTurn{Role: "assistant", Text: tx})
							}
						case "toolCall", "tool_use":
							name, _ := b["name"].(string)
							args, _ := b["arguments"].(map[string]interface{})
							if args == nil {
								args, _ = b["input"].(map[string]interface{})
							}
							recordToolSideEffects(name, args, modSet, &ps.Commands)
							rawArgs, _ := json.Marshal(args)
							addTurn(e.id, SessionTurn{
								Role:      "tool_use",
								ToolName:  name,
								Text:      fmt.Sprintf("Call `%s`: %s", name, truncateInline(string(rawArgs), 300)),
								RawOutput: fmt.Sprintf("Call `%s`: %s", name, string(rawArgs)),
							})
						}
					}
				} else if txt := extractContentText(msgObj["content"]); strings.TrimSpace(txt) != "" {
					addTurn(e.id, SessionTurn{Role: "assistant", Text: txt})
				}
			case "toolResult", "tool":
				raw := extractContentText(msgObj["content"])
				toolName, _ := msgObj["toolName"].(string)
				if strings.TrimSpace(raw) != "" {
					addTurn(e.id, SessionTurn{
						Role:      "tool_result",
						ToolName:  toolName,
						Text:      summarizeToolOutput(raw),
						RawOutput: raw,
					})
				}
			case "bashExecution":
				cmd, _ := msgObj["command"].(string)
				out, _ := msgObj["output"].(string)
				if strings.TrimSpace(cmd) != "" {
					ps.Commands = append(ps.Commands, strings.TrimSpace(cmd))
					addTurn(e.id, SessionTurn{
						Role:      "tool_result",
						ToolName:  "bash",
						Text:      fmt.Sprintf("$ %s\n%s", cmd, summarizeToolOutput(out)),
						RawOutput: out,
					})
				}
			}
		}
	}

	// If Pi's compaction entry specified a firstKeptEntryId that precedes the compaction entry,
	// include kept turns starting from that entry in PostCompact.
	if latestFirstKeptID != "" {
		if firstTurnIdx := entryFirstTurn[latestFirstKeptID]; firstTurnIdx > 0 && firstTurnIdx-1 < len(ps.AllTurns) {
			ps.PreCompactTurns = firstTurnIdx - 1
			ps.PostCompact = append([]SessionTurn(nil), ps.AllTurns[firstTurnIdx-1:]...)
		}
	}

	ps.ModifiedFiles = sortedKeys(modSet)
	return nil
}

func parseAgyTranscriptJSONL(path string, ps *ParsedSession) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	modSet := make(map[string]bool)
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024*1024), 64*1024*1024)
	turnID := 0

	addTurn := func(t SessionTurn) {
		turnID++
		t.TurnID = turnID
		ps.AllTurns = append(ps.AllTurns, t)
		ps.PostCompact = append(ps.PostCompact, t)
	}

	for scanner.Scan() {
		var row map[string]interface{}
		if err := json.Unmarshal(scanner.Bytes(), &row); err != nil {
			continue
		}
		stepType, _ := row["type"].(string)
		content, _ := row["content"].(string)
		thinking, _ := row["thinking"].(string)

		if stepType == "CHECKPOINT" || stepType == "COMPACTION" {
			ps.PreCompactTurns = turnID
			ps.CompactSummary = content
			ps.PostCompact = nil
			continue
		}

		switch stepType {
		case "USER_INPUT":
			if strings.TrimSpace(content) != "" {
				addTurn(SessionTurn{Role: "user", Text: content})
			}
		case "PLANNER_RESPONSE":
			if strings.TrimSpace(thinking) != "" {
				addTurn(SessionTurn{Role: "assistant_thinking", Text: thinking})
			}
			if strings.TrimSpace(content) != "" {
				addTurn(SessionTurn{Role: "assistant", Text: content})
			}
			if tcs, ok := row["tool_calls"].([]interface{}); ok {
				for _, tc := range tcs {
					tcMap, _ := tc.(map[string]interface{})
					name, _ := tcMap["name"].(string)
					args, _ := tcMap["args"].(map[string]interface{})
					if args == nil {
						args, _ = tcMap["arguments"].(map[string]interface{})
					}
					recordToolSideEffects(name, args, modSet, &ps.Commands)
					rawArgs, _ := json.Marshal(args)
					addTurn(SessionTurn{
						Role:      "tool_use",
						ToolName:  name,
						Text:      fmt.Sprintf("Call `%s`: %s", name, truncateInline(string(rawArgs), 300)),
						RawOutput: fmt.Sprintf("Call `%s`: %s", name, string(rawArgs)),
					})
				}
			}
		default:
			if strings.TrimSpace(content) != "" && (stepType == "GENERIC" || strings.Contains(stepType, "TOOL") || strings.Contains(stepType, "OBSERVATION")) {
				addTurn(SessionTurn{
					Role:      "tool_result",
					Text:      summarizeToolOutput(content),
					RawOutput: content,
				})
			}
		}
	}

	ps.ModifiedFiles = sortedKeys(modSet)
	return nil
}

func parseCodexOrGenericJSONL(path string, ps *ParsedSession) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	modSet := make(map[string]bool)
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024*1024), 64*1024*1024)
	turnID := 0

	addTurn := func(t SessionTurn) {
		turnID++
		t.TurnID = turnID
		ps.AllTurns = append(ps.AllTurns, t)
		ps.PostCompact = append(ps.PostCompact, t)
	}

	for scanner.Scan() {
		var row map[string]interface{}
		if err := json.Unmarshal(scanner.Bytes(), &row); err != nil {
			continue
		}

		typ, _ := row["type"].(string)
		role, _ := row["role"].(string)
		content := extractContentText(row["content"])

		if typ == "session_meta" {
			if p, ok := row["payload"].(map[string]interface{}); ok {
				if c, _ := p["cwd"].(string); c != "" && ps.CWD == "" {
					ps.CWD = c
				}
			}
		}

		if typ == "compaction" || typ == "context_compacted" || (role != "tool" && (strings.HasPrefix(strings.TrimSpace(content), "[Context Compacted]") || strings.HasPrefix(strings.TrimSpace(content), "<context_summary>"))) {
			summary := content
			if summary == "" {
				summary = extractContentText(row["summary"])
			}
			ps.PreCompactTurns = turnID
			ps.CompactSummary = summary
			ps.PostCompact = nil
			continue
		}

		if payload, ok := row["payload"].(map[string]interface{}); ok {
			pType, _ := payload["type"].(string)
			pRole, _ := payload["role"].(string)
			pContent := extractContentText(payload["content"])
			if pType == "message" && pContent != "" {
				if pRole == "developer" || pRole == "system" {
					continue
				}
				trimmed := strings.TrimSpace(pContent)
				if pRole == "user" && (strings.HasPrefix(trimmed, "<environment_context>") || strings.HasPrefix(trimmed, "<permissions instructions>") || strings.HasPrefix(trimmed, "<collaboration_mode>")) {
					continue
				}
				if pRole == "" {
					pRole = "assistant"
				}
				addTurn(SessionTurn{Role: pRole, Text: pContent})
				continue
			}
			if pType == "function_call" || pType == "local_shell_call" || pType == "custom_tool_call" {
				name, _ := payload["name"].(string)
				if name == "" {
					name = pType
				}
				argsStr, _ := payload["arguments"].(string)
				if argsStr == "" {
					argsStr, _ = payload["input"].(string)
				}
				var argsMap map[string]interface{}
				_ = json.Unmarshal([]byte(argsStr), &argsMap)
				recordToolSideEffects(name, argsMap, modSet, &ps.Commands)
				addTurn(SessionTurn{
					Role:      "tool_use",
					ToolName:  name,
					Text:      fmt.Sprintf("Call `%s`: %s", name, truncateInline(argsStr, 300)),
					RawOutput: fmt.Sprintf("Call `%s`: %s", name, argsStr),
				})
				continue
			}
			if pType == "function_call_output" || pType == "custom_tool_call_output" {
				raw := extractContentText(payload["output"])
				if strings.TrimSpace(raw) != "" {
					addTurn(SessionTurn{
						Role:      "tool_result",
						Text:      summarizeToolOutput(raw),
						RawOutput: raw,
					})
				}
				continue
			}
		}

		if role != "" && strings.TrimSpace(content) != "" {
			if role == "tool" {
				addTurn(SessionTurn{
					Role:      "tool_result",
					Text:      summarizeToolOutput(content),
					RawOutput: content,
				})
			} else {
				addTurn(SessionTurn{Role: role, Text: content})
			}
			continue
		}

		if msgObj, ok := row["message"].(map[string]interface{}); ok {
			mRole, _ := msgObj["role"].(string)
			mContent := extractContentText(msgObj["content"])
			if mRole != "" && strings.TrimSpace(mContent) != "" {
				addTurn(SessionTurn{Role: mRole, Text: mContent})
			}
		}
	}

	ps.ModifiedFiles = sortedKeys(modSet)
	return nil
}

func parseHermesJSONFile(path string, ps *ParsedSession) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var root map[string]interface{}
	if err := json.Unmarshal(data, &root); err != nil {
		return parseCodexOrGenericJSONL(path, ps)
	}
	if t, _ := root["title"].(string); t != "" {
		ps.Title = t
	}
	if c, _ := root["cwd"].(string); c != "" {
		ps.CWD = c
	}
	msgs, ok := root["messages"].([]interface{})
	if !ok {
		return nil
	}
	modSet := make(map[string]bool)
	turnID := 0
	for _, m := range msgs {
		obj, _ := m.(map[string]interface{})
		role, _ := obj["role"].(string)
		content := extractContentText(obj["content"])
		if isComp, _ := obj["_compressed_summary"].(float64); isComp == 1 {
			ps.PreCompactTurns = turnID
			ps.CompactSummary = content
			ps.PostCompact = nil
			continue
		}
		if tcs, ok := obj["tool_calls"].([]interface{}); ok {
			for _, tc := range tcs {
				tcMap, _ := tc.(map[string]interface{})
				fn, _ := tcMap["function"].(map[string]interface{})
				name, _ := fn["name"].(string)
				argsStr, _ := fn["arguments"].(string)
				var argsMap map[string]interface{}
				_ = json.Unmarshal([]byte(argsStr), &argsMap)
				recordToolSideEffects(name, argsMap, modSet, &ps.Commands)
			}
		}
		if strings.TrimSpace(content) == "" {
			continue
		}
		turnID++
		t := SessionTurn{TurnID: turnID, Role: role, Text: content}
		ps.AllTurns = append(ps.AllTurns, t)
		ps.PostCompact = append(ps.PostCompact, t)
	}
	ps.ModifiedFiles = sortedKeys(modSet)
	return nil
}

// parseHermesSQLiteSession queries Hermes Agent's SQLite schema (v32, NousResearch/hermes-agent).
// It walks parent_session_id compression lineage and extracts _compressed_summary checkpoints,
// reasoning, tool_calls, and active messages.
func parseHermesSQLiteSession(path, targetSessionID string, ps *ParsedSession) error {
	if targetSessionID == "" {
		qLatest := "SELECT id FROM sessions ORDER BY COALESCE(last_activity_at, started_at, 0) DESC LIMIT 1;"
		out, err := exec.Command("sqlite3", path, qLatest).Output()
		if err == nil && strings.TrimSpace(string(out)) != "" {
			targetSessionID = strings.TrimSpace(string(out))
		}
	}

	// Collect session lineage (oldest ancestor -> targetSessionID) if compressed/split
	var sessionChain []string
	if targetSessionID != "" {
		curr := targetSessionID
		seen := make(map[string]bool)
		for curr != "" && !seen[curr] {
			seen[curr] = true
			sessionChain = append([]string{curr}, sessionChain...)
			safeCurr := strings.ReplaceAll(curr, "'", "''")
			metaSQL := fmt.Sprintf("SELECT COALESCE(parent_session_id, ''), COALESCE(title, display_name, ''), COALESCE(cwd, '') FROM sessions WHERE id = '%s' LIMIT 1;", safeCurr)
			out, err := exec.Command("sqlite3", "-json", path, metaSQL).Output()
			if err != nil || len(out) == 0 {
				break
			}
			var metaRows []map[string]interface{}
			if json.Unmarshal(out, &metaRows) != nil || len(metaRows) == 0 {
				break
			}
			parent := ""
			for k, v := range metaRows[0] {
				kl := strings.ToLower(k)
				s, _ := v.(string)
				switch {
				case strings.Contains(kl, "parent"):
					parent = s
				case strings.Contains(kl, "title") && ps.Title == "" && s != "":
					ps.Title = s
				case strings.Contains(kl, "cwd") && ps.CWD == "" && s != "":
					ps.CWD = s
				}
			}
			curr = parent
		}
	}

	var whereClause string
	if len(sessionChain) > 0 {
		quoted := make([]string, len(sessionChain))
		for i, id := range sessionChain {
			quoted[i] = "'" + strings.ReplaceAll(id, "'", "''") + "'"
		}
		whereClause = "WHERE session_id IN (" + strings.Join(quoted, ",") + ")"
	} else {
		whereClause = "WHERE session_id = (SELECT session_id FROM messages ORDER BY id DESC LIMIT 1)"
	}

	query := fmt.Sprintf(`SELECT role, COALESCE(content, '') AS content,
	        COALESCE(reasoning, reasoning_content, '') AS reasoning,
	        COALESCE(tool_name, '') AS tool_name,
	        COALESCE(tool_calls, '') AS tool_calls,
	        COALESCE(_compressed_summary, 0) AS is_summary
	        FROM messages %s ORDER BY id ASC;`, whereClause)

	out, err := exec.Command("sqlite3", "-json", path, query).Output()
	if err == nil && len(out) > 0 {
		var rows []struct {
			Role      string `json:"role"`
			Content   string `json:"content"`
			Reasoning string `json:"reasoning"`
			ToolName  string `json:"tool_name"`
			ToolCalls string `json:"tool_calls"`
			IsSummary int    `json:"is_summary"`
		}
		if json.Unmarshal(out, &rows) == nil && len(rows) > 0 {
			modSet := make(map[string]bool)
			turnID := 0
			addTurn := func(t SessionTurn) {
				turnID++
				t.TurnID = turnID
				ps.AllTurns = append(ps.AllTurns, t)
				ps.PostCompact = append(ps.PostCompact, t)
			}
			for _, r := range rows {
				if r.IsSummary == 1 || strings.Contains(r.Content, "<compact_summary>") {
					ps.PreCompactTurns = turnID
					ps.CompactSummary = strings.TrimSpace(r.Content)
					ps.PostCompact = nil
					continue
				}
				if strings.TrimSpace(r.Reasoning) != "" {
					addTurn(SessionTurn{Role: "assistant_thinking", Text: r.Reasoning})
				}
				if strings.TrimSpace(r.ToolCalls) != "" {
					var tcs []map[string]interface{}
					if json.Unmarshal([]byte(r.ToolCalls), &tcs) == nil {
						for _, tc := range tcs {
							fn, _ := tc["function"].(map[string]interface{})
							name, _ := fn["name"].(string)
							if name == "" {
								name, _ = tc["name"].(string)
							}
							argsStr, _ := fn["arguments"].(string)
							var argsMap map[string]interface{}
							_ = json.Unmarshal([]byte(argsStr), &argsMap)
							recordToolSideEffects(name, argsMap, modSet, &ps.Commands)
							addTurn(SessionTurn{
								Role:      "tool_use",
								ToolName:  name,
								Text:      fmt.Sprintf("Call `%s`: %s", name, truncateInline(argsStr, 300)),
								RawOutput: fmt.Sprintf("Call `%s`: %s", name, argsStr),
							})
						}
					}
				}
				if strings.TrimSpace(r.Content) != "" {
					if r.Role == "tool" {
						addTurn(SessionTurn{
							Role:      "tool_result",
							ToolName:  r.ToolName,
							Text:      summarizeToolOutput(r.Content),
							RawOutput: r.Content,
						})
					} else {
						addTurn(SessionTurn{Role: r.Role, Text: r.Content})
					}
				}
			}
			ps.ModifiedFiles = sortedKeys(modSet)
			return nil
		}
	}

	// Fallback if older schema without v32 columns
	simpleQuery := "SELECT role, content FROM messages WHERE session_id = (SELECT session_id FROM messages ORDER BY id DESC LIMIT 1) ORDER BY id ASC;"
	outSimple, err := exec.Command("sqlite3", "-json", path, simpleQuery).Output()
	if err == nil && len(outSimple) > 0 {
		var rows []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		}
		if json.Unmarshal(outSimple, &rows) == nil && len(rows) > 0 {
			for i, r := range rows {
				t := SessionTurn{TurnID: i + 1, Role: r.Role, Text: r.Content}
				ps.AllTurns = append(ps.AllTurns, t)
				ps.PostCompact = append(ps.PostCompact, t)
			}
			return nil
		}
	}
	return parseProtobufSession(path, ps)
}

func parseProtobufSession(path string, ps *ParsedSession) error {
	buf, err := os.ReadFile(path)
	if err != nil {
		return err
	}

	chunks := extractUTF8StringsFromPB(buf)
	turnID := 0
	for _, s := range chunks {
		if strings.Contains(s, "## Summary") || strings.Contains(s, "### Current State") || strings.Contains(s, "<compact_summary>") {
			ps.PreCompactTurns = turnID
			ps.CompactSummary = s
			ps.PostCompact = nil
			continue
		}
		turnID++
		t := SessionTurn{
			TurnID: turnID,
			Role:   "extracted",
			Text:   s,
		}
		ps.AllTurns = append(ps.AllTurns, t)
		ps.PostCompact = append(ps.PostCompact, t)
	}
	return nil
}

func extractUTF8StringsFromPB(buf []byte) []string {
	var texts []string
	idx := 0
	for idx < len(buf) {
		tag, n := binary.Uvarint(buf[idx:])
		if n <= 0 {
			break
		}
		idx += n

		wireType := tag & 0x7
		switch wireType {
		case 0:
			_, vn := binary.Uvarint(buf[idx:])
			if vn <= 0 {
				return texts
			}
			idx += vn
		case 2:
			length, ln := binary.Uvarint(buf[idx:])
			if ln <= 0 || idx+ln+int(length) > len(buf) || int(length) < 0 {
				return texts
			}
			idx += ln
			chunk := buf[idx : idx+int(length)]
			idx += int(length)

			if utf8.Valid(chunk) && len(chunk) > 15 && bytes.ContainsAny(chunk, " \n\t") {
				str := strings.TrimSpace(string(chunk))
				if !strings.HasPrefix(str, "{\"type\":") && !strings.Contains(str, "base64,") {
					texts = append(texts, str)
				}
			}
		case 1:
			idx += 8
		case 5:
			idx += 4
		default:
			return texts
		}
	}
	return texts
}

func recordToolSideEffects(name string, input map[string]interface{}, modSet map[string]bool, commands *[]string) {
	if input == nil {
		return
	}
	lower := strings.ToLower(name)
	if lower == "bash" || lower == "run_command" || lower == "execute_command" || lower == "shell" || lower == "local_shell_call" || lower == "terminal" {
		for _, k := range []string{"command", "CommandLine", "cmd"} {
			if cmd, ok := input[k].(string); ok && strings.TrimSpace(cmd) != "" {
				*commands = append(*commands, strings.TrimSpace(cmd))
			}
		}
	}
	for _, k := range []string{"file_path", "path", "Path", "target_file", "TargetFile"} {
		if p, ok := input[k].(string); ok && strings.TrimSpace(p) != "" {
			if strings.Contains(lower, "edit") || strings.Contains(lower, "write") || strings.Contains(lower, "replace") || strings.Contains(lower, "create") || strings.Contains(lower, "patch") {
				modSet[p] = true
			}
		}
	}
}

func extractContentText(v interface{}) string {
	switch val := v.(type) {
	case string:
		return val
	case []interface{}:
		var parts []string
		for _, item := range val {
			if m, ok := item.(map[string]interface{}); ok {
				if t, ok := m["text"].(string); ok && t != "" {
					parts = append(parts, t)
				} else if c, ok := m["content"].(string); ok && c != "" {
					parts = append(parts, c)
				}
			}
		}
		return strings.Join(parts, "\n")
	default:
		return ""
	}
}

func summarizeToolOutput(raw string) string {
	lines := strings.Split(strings.TrimSpace(raw), "\n")
	if len(raw) <= 600 && len(lines) <= 12 {
		return raw
	}
	headCount := 5
	if len(lines) < headCount {
		headCount = len(lines)
	}
	tailCount := 5
	if len(lines)-headCount < tailCount {
		tailCount = len(lines) - headCount
	}
	var sb strings.Builder
	sb.WriteString(strings.Join(lines[:headCount], "\n"))
	sb.WriteString(fmt.Sprintf("\n... [collapsed %d bytes / %d lines; full output in turns.json or via get_session_turn] ...\n", len(raw), len(lines)))
	if tailCount > 0 {
		sb.WriteString(strings.Join(lines[len(lines)-tailCount:], "\n"))
	}
	return sb.String()
}

func truncateInline(s string, max int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "..."
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func writeHandoffDirectory(ps *ParsedSession, globalRules map[string]string) (string, error) {
	handoffDir := filepath.Join(handoffBaseDir(), filepath.Base(ps.SessionID))
	if err := os.MkdirAll(handoffDir, 0700); err != nil {
		return "", err
	}

	turnsBytes, err := json.MarshalIndent(ps.AllTurns, "", "  ")
	if err == nil {
		_ = os.WriteFile(filepath.Join(handoffDir, "turns.json"), turnsBytes, 0600)
	}

	var full strings.Builder
	full.WriteString(fmt.Sprintf("# Full Session Transcript: %s (%s)\n", ps.SessionID, ps.Agent))
	if ps.Title != "" {
		full.WriteString(fmt.Sprintf("Session Name: **%s**\n", ps.Title))
	}
	full.WriteString(fmt.Sprintf("Source file: `%s`\n\n", ps.SourceFile))
	if ps.CompactSummary != "" {
		full.WriteString(fmt.Sprintf("## Latest Compaction Summary (after turn #%d)\n\n%s\n\n", ps.PreCompactTurns, ps.CompactSummary))
	}
	full.WriteString("## Complete Turn History\n\n")
	for _, t := range ps.AllTurns {
		full.WriteString(fmt.Sprintf("### [Turn #%d] %s\n%s\n\n", t.TurnID, t.Role, t.Text))
		if t.RawOutput != "" && t.RawOutput != t.Text {
			full.WriteString("```\n" + t.RawOutput + "\n```\n\n")
		}
	}
	_ = os.WriteFile(filepath.Join(handoffDir, "full_dialogue.md"), []byte(full.String()), 0600)

	if len(globalRules) > 0 {
		rulesDir := filepath.Join(handoffDir, "global_rules")
		_ = os.MkdirAll(rulesDir, 0700)
		for name, content := range globalRules {
			_ = os.WriteFile(filepath.Join(rulesDir, filepath.Base(name)), []byte(content), 0600)
		}
	}

	_ = formatHandoffSummary(ps, handoffDir, globalRules)
	return handoffDir, nil
}

func formatHandoffSummary(ps *ParsedSession, handoffDir string, globalRules map[string]string) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("# BATON SESSION HANDOFF: source [%s]\n\n", strings.ToUpper(ps.Agent)))
	sb.WriteString(fmt.Sprintf("- **Session ID**: `%s`\n", ps.SessionID))
	if ps.Title != "" {
		sb.WriteString(fmt.Sprintf("- **Session Name**: **%s**\n", ps.Title))
	}
	if ps.CWD != "" {
		sb.WriteString(fmt.Sprintf("- **Working Directory**: `%s`\n", ps.CWD))
	}
	sb.WriteString(fmt.Sprintf("- **Source log**: `%s`\n", ps.SourceFile))
	activeContextPath := filepath.Join(handoffDir, "active_context.md")
	sb.WriteString(fmt.Sprintf("- **Active context file**: `%s`\n", activeContextPath))
	sb.WriteString(fmt.Sprintf("- **Full lossless transcript**: `%s`\n", filepath.Join(handoffDir, "full_dialogue.md")))
	sb.WriteString(fmt.Sprintf("- **Turn index (`get_session_turn`)**: `%s`\n\n", filepath.Join(handoffDir, "turns.json")))

	if vcsStatus := detectVCSStatus(); vcsStatus != "" {
		sb.WriteString("## Current Workspace Status:\n```\n")
		sb.WriteString(vcsStatus)
		sb.WriteString("```\n\n")
	}

	if len(ps.ModifiedFiles) > 0 {
		sb.WriteString("## Files Modified During Session:\n")
		for _, f := range ps.ModifiedFiles {
			sb.WriteString(fmt.Sprintf("- `%s`\n", f))
		}
		sb.WriteString("\n")
	}

	if len(ps.Commands) > 0 {
		sb.WriteString("## Executed Commands:\n```bash\n")
		start := 0
		if len(ps.Commands) > 25 {
			start = len(ps.Commands) - 25
		}
		for _, c := range ps.Commands[start:] {
			sb.WriteString(c + "\n")
		}
		sb.WriteString("```\n\n")
	}

	if len(globalRules) > 0 {
		sb.WriteString("## Imported Global Rules from Source Machine:\n")
		for name := range globalRules {
			sb.WriteString(fmt.Sprintf("- `%s`\n", filepath.Join(handoffDir, "global_rules", filepath.Base(name))))
		}
		sb.WriteString("\n")
	}

	if ps.CompactSummary != "" {
		sb.WriteString(fmt.Sprintf("## Latest Compaction Checkpoint (after %d turns):\n%s\n\n", ps.PreCompactTurns, ps.CompactSummary))
	}

	sb.WriteString("## Post-Compaction Dialogue (Active Context Turns):\n\n")
	turns := ps.PostCompact
	for _, t := range turns {
		sb.WriteString(fmt.Sprintf("### [Turn #%d] %s", t.TurnID, t.Role))
		if t.ToolName != "" {
			sb.WriteString(fmt.Sprintf(" (`%s`)", t.ToolName))
		}
		body := t.Text
		if t.RawOutput != "" {
			body = t.RawOutput
		}
		sb.WriteString("\n" + body + "\n\n")
	}

	fullActive := sb.String()
	_ = os.WriteFile(activeContextPath, []byte(fullActive), 0600)

	const maxStdoutBytes = 7000
	if len(fullActive) <= maxStdoutBytes {
		return fullActive + fmt.Sprintf("---\nHandoff Status: COMPLETE (%d/%d post-compaction turns loaded)\n", len(turns), len(turns))
	}

	lines := strings.Split(fullActive, "\n")
	totalLines := len(lines)
	var out strings.Builder
	shownLines := 0
	for i, line := range lines {
		if out.Len()+len(line)+1 > maxStdoutBytes && shownLines > 0 {
			break
		}
		out.WriteString(line)
		out.WriteByte('\n')
		shownLines = i + 1
	}

	// Split remaining lines into self-contained part files (<= 550 lines and <= 28,000 bytes each)
	// so every agent (including Codex CLI's ~10k-token per-command buffer and AGY's 45KB read_file)
	// can read each part file in full via a separate tool call without any truncation.
	const maxPartLines = 550
	const maxPartBytes = 28000
	var partFiles []string
	remLines := lines[shownLines:]
	partIdx := 1
	for len(remLines) > 0 {
		var partBuf strings.Builder
		taken := 0
		for i, line := range remLines {
			if taken > 0 && (taken >= maxPartLines || partBuf.Len()+len(line)+1 > maxPartBytes) {
				break
			}
			partBuf.WriteString(line)
			partBuf.WriteByte('\n')
			taken = i + 1
		}
		partPath := filepath.Join(handoffDir, fmt.Sprintf("active_context_part%d.md", partIdx))
		_ = os.WriteFile(partPath, []byte(partBuf.String()), 0600)
		partFiles = append(partFiles, partPath)
		remLines = remLines[taken:]
		partIdx++
	}

	out.WriteString(fmt.Sprintf("---\nHandoff Status: PARTIAL (lines 1-%d of %d shown above; %d remaining part files)\nRemaining Active Context Files:\n",
		shownLines, totalLines, len(partFiles)))
	for _, pf := range partFiles {
		out.WriteString(fmt.Sprintf("- %s\n", pf))
	}
	return out.String()
}

func detectVCSStatus() string {
	if _, err := os.Stat(".git"); err == nil {
		if out, err := exec.Command("git", "status", "--short").Output(); err == nil && len(out) > 0 {
			return string(out)
		}
	}
	if _, err := os.Stat(".fslckout"); err == nil {
		if out, err := exec.Command("fossil", "changes").Output(); err == nil && len(out) > 0 {
			return string(out)
		}
	}
	if _, err := os.Stat(".svn"); err == nil {
		if out, err := exec.Command("svn", "status").Output(); err == nil && len(out) > 0 {
			return string(out)
		}
	}
	return ""
}
