package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// snapshotFormat is the format of saved snapshots (*.baton.json) this baton
// writes and understands. New optional fields do not change it (older batons
// ignore unknown JSON fields); it goes up only when an existing field changes
// meaning, so an older baton refuses such a snapshot instead of misreading it.
const snapshotFormat = 1

// keepVersions is how many older versions of one saved name baton keeps.
const keepVersions = 3

func checkSnapshotFormat(ps *ParsedSession) error {
	if ps == nil || ps.Format <= snapshotFormat {
		return nil
	}
	by := ps.SavedBy
	if by == "" {
		by = "a newer baton"
	} else {
		by = "baton " + by
	}
	return fmt.Errorf("session %q was saved by %s in snapshot format %d, but this baton %s reads formats up to %d: run `baton update`",
		ps.Title, by, ps.Format, version, snapshotFormat)
}

// --- Older versions of a saved name ---

// keepOlderVersion moves the snapshot saved under name to <name>.<date> (the
// time it was saved) so that a newer session can take the name.
func keepOlderVersion(path, name string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("cannot keep the previous %s as an older version: %w", filepath.Base(path), err)
	}
	dir := filepath.Dir(path)
	stamp := info.ModTime().Format("2006-01-02_15-04")
	vname := name + "." + stamp
	vpath := filepath.Join(dir, sanitizeSessionFileName(vname)+".baton.json")
	for n := 2; ; n++ {
		if _, err := os.Stat(vpath); os.IsNotExist(err) {
			break
		}
		vname = fmt.Sprintf("%s.%s-%d", name, stamp, n)
		vpath = filepath.Join(dir, sanitizeSessionFileName(vname)+".baton.json")
	}
	raw["title"], _ = json.Marshal(vname)
	raw["version_of"], _ = json.Marshal(name)
	out, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(vpath, out, 0600); err != nil {
		return err
	}
	// Keep the original save time: lists sort by it.
	_ = os.Chtimes(vpath, info.ModTime(), info.ModTime())
	if err := os.Remove(path); err != nil {
		return err
	}
	aliases := loadAliases()
	delete(aliases, path)
	aliases[vpath] = vname
	_ = saveAliases(aliases)
	pruneOlderVersions(name)
	return nil
}

// pruneOlderVersions deletes all but the keepVersions newest older versions of name.
func pruneOlderVersions(name string) {
	var versions []SessionCandidate
	for _, c := range savedSessions("all", "", true) {
		if strings.EqualFold(c.VersionOf, name) {
			versions = append(versions, c)
		}
	}
	if len(versions) <= keepVersions {
		return
	}
	aliases := loadAliases()
	for _, c := range versions[keepVersions:] {
		_ = os.Remove(c.Path)
		delete(aliases, c.Path)
	}
	_ = saveAliases(aliases)
}

// savedVersionNames returns the names of the older versions of name.
func savedVersionNames(name string) map[string]bool {
	out := make(map[string]bool)
	if strings.TrimSpace(name) == "" {
		return out
	}
	for _, c := range savedSessions("all", "", true) {
		if strings.EqualFold(c.VersionOf, strings.TrimSpace(name)) {
			out[c.Title] = true
		}
	}
	return out
}

// newVersionName returns the older version of name that appeared since before
// was taken, or "" if saving did not replace another session.
func newVersionName(name string, before map[string]bool) string {
	for v := range savedVersionNames(name) {
		if !before[v] {
			return v
		}
	}
	return ""
}

// --- Links: which agent sessions continue which saved name ---

// sessionLink records that an agent session holds a point of a saved name: it
// was saved under it or a saved session was loaded into it. Prompts is the
// number of typed user prompts at that moment: when it changes, the work went
// on in that session after baton last saw it.
type sessionLink struct {
	Name    string    `json:"name"`
	Prompts int       `json:"prompts"`
	At      time.Time `json:"at"`
}

func linksFilePath() string {
	return filepath.Join(batonConfigDir(), "links.json")
}

func loadLinks() map[string]sessionLink {
	out := make(map[string]sessionLink)
	if data, err := os.ReadFile(linksFilePath()); err == nil {
		_ = json.Unmarshal(data, &out)
	}
	return out
}

func saveLinks(links map[string]sessionLink) {
	path := linksFilePath()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return
	}
	data, err := json.MarshalIndent(links, "", "  ")
	if err != nil {
		return
	}
	tmp := path + ".tmp"
	if os.WriteFile(tmp, data, 0600) == nil {
		_ = os.Rename(tmp, path)
	}
}

// countTypedPrompts counts the prompts the user typed (not injected text, not
// bare baton commands).
func countTypedPrompts(turns []SessionTurn) int {
	n := 0
	for _, t := range turns {
		if t.Role == "user" && !t.Meta && cleanUserPrompt(t.Text) != "" {
			n++
		}
	}
	return n
}

// linkSavedOrigin links the agent session a snapshot was saved from to its name.
func linkSavedOrigin(ps *ParsedSession, name string) {
	if ps == nil || ps.SourceFile == "" || strings.HasSuffix(ps.SourceFile, ".baton.json") {
		return
	}
	if _, err := os.Stat(ps.SourceFile); err != nil {
		return // a session pulled from another computer
	}
	links := loadLinks()
	links[ps.SessionID] = sessionLink{Name: name, Prompts: countTypedPrompts(ps.AllTurns), At: time.Now()}
	saveLinks(links)
}

// chainName is the saved name a loaded snapshot continues: an older version
// continues the name it was a version of.
func chainName(ps *ParsedSession) string {
	if ps == nil {
		return ""
	}
	if ps.VersionOf != "" {
		return ps.VersionOf
	}
	return ps.Title
}

// measureSessions parses sessions (or takes them from the import cache) and
// keeps the cache's other entries.
func measureSessions(cands []SessionCandidate) []importMeta {
	cache := loadImportCache()
	fresh := make(map[string]importMeta, len(cache)+len(cands))
	for k, v := range cache {
		fresh[k] = v
	}
	metas := measureCandidates(cands, cache, fresh)
	saveImportCache(fresh)
	return metas
}

// currentSessionCandidate finds the agent session that is running baton: by the
// session ID the agent exports, otherwise the newest session of the calling
// agent in the current folder (it is being written right now).
func currentSessionCandidate(cands []SessionCandidate) (SessionCandidate, bool) {
	exported := false
	for _, env := range []string{"CLAUDE_CODE_SESSION_ID", "CODEX_THREAD_ID", "PI_SESSION_ID", "CURSOR_CONVERSATION_ID"} {
		id := strings.ToLower(strings.TrimSpace(os.Getenv(env)))
		if id == "" {
			continue
		}
		exported = true
		for _, c := range cands {
			if strings.Contains(strings.ToLower(c.Path), id) {
				return c, true
			}
		}
	}
	if exported {
		// The agent named its session but it is not on disk: don't guess.
		return SessionCandidate{}, false
	}
	caller := detectCallerAgent()
	if caller == "" {
		return SessionCandidate{}, false
	}
	cwd, _ := os.Getwd()
	found := false
	var best SessionCandidate
	for _, c := range cands {
		if c.Agent == caller && cwdMatches(c.CWD, cwd) && (!found || c.ModTime.After(best.ModTime)) {
			best, found = c, true
		}
	}
	return best, found
}

// attachCurrentSession runs after a saved session was loaded into the agent
// session running baton: it links that session to name (so `baton save` without
// a name and `baton resume` know it continues name) and names it so in the agent
// where baton can do that safely. Returns a note for the report.
func attachCurrentSession(name string) string {
	name = strings.TrimSpace(name)
	if name == "" || os.Getenv("BATON_NO_ATTACH") != "" {
		return ""
	}
	cur, ok := currentSessionCandidate(discoverSessions("all", ""))
	if !ok {
		return ""
	}
	key := sessionIDForPath(cur.Agent, cur.Path)
	m := measureSessions([]SessionCandidate{cur})[0]
	links := loadLinks()
	prev := links[key]
	links[key] = sessionLink{Name: name, Prompts: m.Prompts, At: time.Now()}
	saveLinks(links)
	if note := renameAgentSession(cur, name, prev.Name); note != "" {
		return "\n" + note + "\n"
	}
	return ""
}

// lastJSONLField returns the last non-empty string field of rows of type typ
// in a JSONL log.
func lastJSONLField(path, typ, field string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	marker := []byte(`"` + typ + `"`)
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 64*1024*1024)
	out := ""
	for scanner.Scan() {
		line := scanner.Bytes()
		if !strings.Contains(string(line), string(marker)) {
			continue
		}
		var row map[string]interface{}
		if json.Unmarshal(line, &row) != nil {
			continue
		}
		if t, _ := row["type"].(string); t != typ {
			continue
		}
		if v, _ := row[field].(string); strings.TrimSpace(v) != "" {
			out = strings.TrimSpace(v)
		}
	}
	return out
}

// userSessionName is the name the user gave a session in the agent itself, for
// agents that keep it apart from automatic titles ("" otherwise).
func userSessionName(agent, path string) string {
	switch agent {
	case "claude":
		return lastJSONLField(path, "custom-title", "customTitle")
	case "pi":
		return lastJSONLField(path, "session_info", "name")
	case "kimi":
		return readKimiSessionState(kimiSessionDir(path)).name()
	}
	return ""
}

// appendJSONLine appends one JSON row to a JSONL file; create allows a missing file.
func appendJSONLine(path string, row interface{}, create bool) error {
	data, err := json.Marshal(row)
	if err != nil {
		return err
	}
	flags := os.O_APPEND | os.O_WRONLY
	if create {
		flags |= os.O_CREATE
	}
	f, err := os.OpenFile(path, flags, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(data, '\n'))
	return err
}

// renameAgentSession gives the agent session the saved name, so the agent's own
// resume menu shows it instead of an automatic title. It writes the same record
// the agent's own rename writes, only for agents whose logs are append-only
// (Claude Code, Codex). A name the user set in Claude Code is kept; prevName is
// the name baton gave this session earlier, which may be replaced.
func renameAgentSession(c SessionCandidate, name, prevName string) string {
	switch c.Agent {
	case "claude":
		current := userSessionName("claude", c.Path)
		if strings.EqualFold(current, name) {
			return ""
		}
		if current != "" && current != prevName && current != loadAliases()[c.Path] {
			return ""
		}
		id := strings.TrimSuffix(filepath.Base(c.Path), filepath.Ext(c.Path))
		if appendJSONLine(c.Path, map[string]string{"type": "custom-title", "customTitle": name, "sessionId": id}, false) != nil {
			return ""
		}
		return fmt.Sprintf("This Claude Code session is now named **%s** (as in `/rename`).", name)
	case "codex":
		if strings.EqualFold(c.Title, name) {
			return ""
		}
		home, _ := os.UserHomeDir()
		idx := filepath.Join(codexBaseDir(home), "session_index.jsonl")
		row := map[string]string{"id": agentSessionID(c.Path), "thread_name": name, "updated_at": time.Now().UTC().Format(time.RFC3339Nano)}
		if appendJSONLine(idx, row, true) != nil {
			return ""
		}
		return fmt.Sprintf("This Codex session is now named **%s** in `/resume`.", name)
	}
	return ""
}

// defaultSaveName is the name `baton save` uses when none is given: the saved
// name this session continues, a name the user gave it in the agent, the folder.
func defaultSaveName(ps *ParsedSession) string {
	if l, ok := loadLinks()[ps.SessionID]; ok && strings.TrimSpace(l.Name) != "" {
		return l.Name
	}
	if ps.SourceFile != "" {
		if n := userSessionName(ps.Agent, ps.SourceFile); n != "" {
			return n
		}
	}
	cwd := ps.CWD
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	if base := filepath.Base(cwd); cwd != "" && base != "." && base != string(filepath.Separator) {
		return base
	}
	return "saved-session"
}

// --- baton resume ---

// latestAgentPoint returns the most recently active agent session that
// continues name: one linked to it whose typed prompts changed since baton linked
// it, or one the agent itself names so. The session running baton is skipped.
func latestAgentPoint(name string, notBefore time.Time) (SessionCandidate, bool) {
	cands := discoverSessions("all", "")
	curKey := ""
	if cur, ok := currentSessionCandidate(cands); ok {
		curKey = sessionIDForPath(cur.Agent, cur.Path)
	}
	links := loadLinks()
	var pick []SessionCandidate
	var linked []*sessionLink
	for _, c := range cands {
		key := sessionIDForPath(c.Agent, c.Path)
		if key == curKey || isCallerOwnSession(c.Path) {
			continue
		}
		l, ok := links[key]
		isLinked := ok && strings.EqualFold(l.Name, name)
		if !isLinked && !strings.EqualFold(c.Title, name) {
			continue
		}
		pick = append(pick, c)
		if isLinked {
			l := l
			linked = append(linked, &l)
		} else {
			linked = append(linked, nil)
		}
	}
	if len(pick) == 0 {
		return SessionCandidate{}, false
	}
	metas := measureSessions(pick)
	found := false
	var best SessionCandidate
	for i, c := range pick {
		if l := linked[i]; l != nil {
			if metas[i].Prompts == l.Prompts {
				continue // nothing typed there since it was saved or loaded
			}
		} else if metas[i].Stub || metas[i].Prompts == 0 || !c.ModTime.After(notBefore.Add(5*time.Minute)) {
			// Named so only by title: the session a snapshot was saved from is
			// written for a moment after the save, so demand a clear lead.
			continue
		}
		if !c.ModTime.After(notBefore) {
			continue
		}
		if !found || c.ModTime.After(best.ModTime) {
			best, found = c, true
		}
	}
	return best, found
}

// nativeResumeCommand is the terminal command that resumes a session in its
// own agent by ID ("" if baton does not know it).
func nativeResumeCommand(c SessionCandidate) string {
	id := c.SessionID()
	switch c.Agent {
	case "claude":
		return "claude --resume " + id
	case "codex":
		return "codex resume " + id
	case "agy":
		return "agy --conversation " + id
	case "kimi":
		// Kimi's own IDs keep the "session_" prefix of the session directory.
		return "kimi --session " + filepath.Base(kimiSessionDir(c.Path))
	case "opencode":
		if _, full, ok := strings.Cut(c.Path, "::"); ok {
			id = full
		}
		return "opencode --session " + id
	case "cursor":
		return "agent --resume " + id
	case "pi":
		return "pi --session " + id
	}
	return ""
}

// nativeResumeHint tells how to continue a session in its own agent: a
// terminal command (in another folder it starts there, since agents resume
// sessions of the folder they run in) and, in Claude Code, `/resume <id>`.
func nativeResumeHint(c SessionCandidate) string {
	elsewhere := otherExistingFolder(c.CWD)
	cmd := nativeResumeCommand(c)
	var sb strings.Builder
	if cmd == "" {
		sb.WriteString(fmt.Sprintf("- start %s", c.Agent))
		if elsewhere {
			sb.WriteString(" in `" + shortPath(c.CWD) + "`")
		}
		sb.WriteString(fmt.Sprintf(" and resume session `%s` with its resume command\n", c.SessionID()))
		return sb.String()
	}
	if elsewhere {
		sb.WriteString(fmt.Sprintf("- exit this agent and run: `cd %s && %s`\n", shellShortPath(c.CWD), cmd))
		return sb.String()
	}
	if c.Agent == "claude" {
		sb.WriteString(fmt.Sprintf("- here: `/resume %s`\n", c.SessionID()))
	}
	sb.WriteString(fmt.Sprintf("- in a terminal: `%s`\n", cmd))
	return sb.String()
}

// shortTime shows a time of today as 15:04, others with the date.
func shortTime(t time.Time) string {
	if t.Format("2006-01-02") == time.Now().Format("2006-01-02") {
		return t.Format("15:04")
	}
	return t.Format("2006-01-02 15:04")
}

// shortPath shows a path inside the home folder as ~/...
func shortPath(p string) string {
	return displayPath(p)
}

// shellShortPath is shortPath ready for a POSIX shell (~ stays unquoted).
func shellShortPath(p string) string {
	sp := shortPath(p)
	if rest, ok := strings.CutPrefix(sp, "~"+string(filepath.Separator)); ok {
		return "~/" + shellPath(rest)
	}
	return shellPath(sp)
}

// handoffFromSavedFile loads a saved snapshot and links the current session to name.
func handoffFromSavedFile(agent, path, name string) (string, error) {
	parsed, err := parseSessionFile(agent, path, time.Now())
	if err != nil {
		return "", err
	}
	handoffDir, err := writeHandoffDirectory(parsed, nil)
	if err != nil {
		return "", err
	}
	if name == "" {
		name = chainName(parsed)
	}
	return formatHandoffSummary(parsed, handoffDir, nil) + attachCurrentSession(name), nil
}

// resumeSession implements `baton resume <name|#|id> [--load]`: it continues
// name from its latest point wherever that is. A saved copy is loaded unless an
// agent session went on after it; such a session is resumed natively when it
// belongs to the calling agent (or baton runs in a terminal), otherwise it is
// imported under name (the old copy becomes an older version) and loaded.
// --load always loads into the current conversation.
func resumeSession(args []string) (string, error) {
	cleanupOldHandoffs()
	forceLoad := false
	var q []string
	for _, a := range args {
		if a == "--load" || a == "--here" {
			forceLoad = true
			continue
		}
		q = append(q, a)
	}
	query := strings.TrimSpace(strings.Join(q, " "))
	saved := discoverSavedBatonSessions("all", "")
	if query == "" {
		return formatCandidateListReport("all", "", saved) + "\nResume one with `baton resume <name|#>`.\n", nil
	}

	name := query
	if idx, ok := parseCandidateIndex(query, len(saved)); ok {
		name = saved[idx].Title
	}
	var snap *SessionCandidate
	for _, c := range savedSessions("all", "", true) {
		if strings.EqualFold(c.Title, name) {
			c := c
			snap = &c
			name = c.Title
			break
		}
	}
	retry := "baton resume " + name
	if snap != nil && snap.VersionOf != "" {
		// An explicit older version: load exactly it.
		if err := checkSessionFolder(snap.CWD, retry, nil); err != nil {
			return "", err
		}
		return handoffFromSavedFile(snap.Agent, snap.Path, snap.VersionOf)
	}

	var notBefore time.Time
	if snap != nil {
		notBefore = snap.ModTime
	}
	if point, ok := latestAgentPoint(name, notBefore); ok {
		where := fmt.Sprintf("**%s** went on in [%s] session %s at %s", name, point.Agent, shortSessionID(point.SessionID()), shortTime(point.ModTime))
		if snap != nil {
			where += ", after the copy saved at " + shortTime(snap.ModTime)
		}
		caller := detectCallerAgent()
		if !forceLoad && (caller == "" || caller == point.Agent) {
			return fmt.Sprintf("%s. Resume it natively, without loss:\n%sTo load it here through baton instead: `baton resume %s --load`\n",
				where, nativeResumeHint(point), name), nil
		}
		ps, err := parseCandidate(point)
		if err != nil {
			return "", err
		}
		if err := checkSessionFolder(ps.CWD, retry+" --load", nil); err != nil {
			return "", err
		}
		before := savedVersionNames(name)
		savePath, savedName, err := saveSessionToBatonStore(ps, name)
		if err != nil {
			return "", err
		}
		head := where + "; imported it as **" + savedName + "**"
		if older := newVersionName(name, before); older != "" {
			head += fmt.Sprintf(" (previous copy kept as **%s**)", older)
		}
		head += "."
		body, err := handoffFromSavedFile(ps.Agent, savePath, savedName)
		if err != nil {
			return "", err
		}
		return head + "\n\n" + body, nil
	}
	if snap != nil {
		if !forceLoad {
			// Saved in this agent and nothing typed there since: the agent's own
			// resume reaches the same point with the whole conversation.
			if ps, err := parseSessionFile(snap.Agent, snap.Path, snap.ModTime); err == nil {
				if origin, unchanged, ok := snapshotOrigin(ps, snap.ModTime); ok && unchanged {
					caller := detectCallerAgent()
					cur, curOK := currentSessionCandidate(discoverSessions("all", ""))
					if curOK && sessionIDForPath(cur.Agent, cur.Path) == ps.SessionID {
						return fmt.Sprintf("This conversation already is **%s**: nothing to load.\n", name), nil
					}
					if caller == "" || caller == origin.Agent {
						return fmt.Sprintf("**%s** was saved from [%s] session %s at %s, and nothing was typed there since. Resume it natively, without loss:\n%sTo load the saved copy here through baton instead: `baton resume %s --load`\n",
							name, origin.Agent, shortSessionID(origin.SessionID()), shortTime(snap.ModTime), nativeResumeHint(origin), name), nil
					}
				}
			}
		}
		if err := checkSessionFolder(snap.CWD, retry, nil); err != nil {
			return "", err
		}
		return handoffFromSavedFile(snap.Agent, snap.Path, name)
	}

	// No exact name: a saved session by ID or name fragment, then an agent session.
	if parsed, err := findAndParseSession("all", query); err == nil {
		if err := checkSessionFolder(parsed.CWD, "baton resume "+chainName(parsed), nil); err != nil {
			return "", err
		}
		handoffDir, err := writeHandoffDirectory(parsed, nil)
		if err != nil {
			return "", err
		}
		return formatHandoffSummary(parsed, handoffDir, nil) + attachCurrentSession(chainName(parsed)), nil
	}
	if _, ok := findImportCandidate(discoverImportCandidates(), query); ok {
		return importSession([]string{query})
	}
	report := formatCandidateListReport("all", query, saved)
	return report + fmt.Sprintf("\nNo saved or agent session is named `%s`. `baton import` lists the agents' own sessions.\n", query), nil
}

// promoteNewestVersion gives name back to its newest older version after the
// session saved under name was removed, so removing the latest acts as undo.
func promoteNewestVersion(name string) string {
	for _, c := range savedSessions("all", "", true) {
		if !strings.EqualFold(c.VersionOf, name) {
			continue
		}
		info, err := os.Stat(c.Path)
		if err != nil {
			return ""
		}
		data, err := os.ReadFile(c.Path)
		if err != nil {
			return ""
		}
		var raw map[string]json.RawMessage
		if json.Unmarshal(data, &raw) != nil {
			return ""
		}
		raw["title"], _ = json.Marshal(name)
		delete(raw, "version_of")
		out, err := json.MarshalIndent(raw, "", "  ")
		if err != nil {
			return ""
		}
		target := filepath.Join(filepath.Dir(c.Path), sanitizeSessionFileName(name)+".baton.json")
		if _, err := os.Stat(target); err == nil {
			return ""
		}
		if os.WriteFile(target, out, 0600) != nil {
			return ""
		}
		_ = os.Chtimes(target, info.ModTime(), info.ModTime())
		_ = os.Remove(c.Path)
		aliases := loadAliases()
		delete(aliases, c.Path)
		aliases[target] = name
		_ = saveAliases(aliases)
		return c.Title
	}
	return ""
}

// --- The session's folder ---

// allowOtherFolder is set by --force: load a session even though it worked in
// another existing folder.
var allowOtherFolder bool

// resolvedFolder is an absolute, symlink-free form of a folder for comparison.
func resolvedFolder(p string) string {
	p = filepath.Clean(strings.TrimSpace(p))
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	if real, err := filepath.EvalSymlinks(p); err == nil {
		p = real
	}
	return p
}

func sameFolder(a, b string) bool {
	ra, rb := resolvedFolder(a), resolvedFolder(b)
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		return strings.EqualFold(ra, rb)
	}
	return ra == rb
}

// otherExistingFolder reports whether a session's folder exists on this machine
// and is not the folder baton runs in. A missing folder (another computer, a
// removed worktree) is not a reason to stop.
func otherExistingFolder(sessionCWD string) bool {
	if strings.TrimSpace(sessionCWD) == "" {
		return false
	}
	info, err := os.Stat(sessionCWD)
	if err != nil || !info.IsDir() {
		return false
	}
	cwd, err := os.Getwd()
	if err != nil {
		return false
	}
	return !sameFolder(sessionCWD, cwd)
}

// shellPath quotes a path for a POSIX shell command when it needs quoting.
func shellPath(p string) string {
	if p != "" && !strings.ContainsAny(p, " \t'\"$`\\!*?&;|<>()[]{}#~") {
		return p
	}
	return "'" + strings.ReplaceAll(p, "'", `'\''`) + "'"
}

// agentLaunchCommand is the command that starts an agent in a terminal.
func agentLaunchCommand(agent string) string {
	switch agent {
	case "claude", "codex", "agy", "pi", "opencode", "kimi":
		return agent
	case "cursor":
		return "agent"
	}
	return "<your agent>"
}

// checkSessionFolder refuses to load a session that worked in another existing
// folder: its relative paths, commands, and project rules belong there, and an
// agent cannot change its own working folder. retry is the baton command to run
// again from the right folder; native, if it returns a command, resumes the
// same point natively there instead. --force loads here anyway.
func checkSessionFolder(sessionCWD, retry string, native func() string) error {
	if allowOtherFolder || !otherExistingFolder(sessionCWD) {
		return nil
	}
	dir := shellShortPath(sessionCWD)
	how := fmt.Sprintf("Restart there: `cd %s && %s`, then `%s`.", dir, agentLaunchCommand(detectCallerAgent()), retry)
	if native != nil {
		if cmd := native(); cmd != "" {
			how = fmt.Sprintf("Resume it there natively, without loss: `cd %s && %s`.", dir, cmd)
		}
	}
	return fmt.Errorf("the session worked in `%s`, not here, so it was not loaded.\n%s Or load here anyway: `%s --force`",
		shortPath(sessionCWD), how, retry)
}

// sameAgentNative returns the native resume command for an agent session when
// it belongs to the agent running baton.
func sameAgentNative(c SessionCandidate) func() string {
	return func() string {
		if detectCallerAgent() != c.Agent {
			return ""
		}
		return nativeResumeCommand(c)
	}
}

// snapshotOrigin finds the agent session a saved snapshot was made from, if it
// is still on disk. unchanged reports that nothing was typed there since the
// save, so resuming it natively reaches the same point as the snapshot.
func snapshotOrigin(ps *ParsedSession, savedAt time.Time) (origin SessionCandidate, unchanged, ok bool) {
	if ps == nil || ps.SourceFile == "" || strings.HasSuffix(ps.SourceFile, ".baton.json") {
		return SessionCandidate{}, false, false
	}
	if _, err := os.Stat(ps.SourceFile); err != nil {
		return SessionCandidate{}, false, false
	}
	for _, c := range discoverSessions(ps.Agent, "") {
		if sessionIDForPath(c.Agent, c.Path) != ps.SessionID {
			continue
		}
		if l, linked := loadLinks()[ps.SessionID]; linked {
			unchanged = measureSessions([]SessionCandidate{c})[0].Prompts == l.Prompts
		} else {
			unchanged = !c.ModTime.After(savedAt.Add(5 * time.Minute))
		}
		return c, unchanged, true
	}
	return SessionCandidate{}, false, false
}

// snapshotNative is the native resume command for a snapshot's origin session
// when it belongs to the agent running baton and nothing was typed there since.
func snapshotNative(ps *ParsedSession, savedAt time.Time) func() string {
	return func() string {
		origin, unchanged, ok := snapshotOrigin(ps, savedAt)
		if !ok || !unchanged || detectCallerAgent() != origin.Agent {
			return ""
		}
		return nativeResumeCommand(origin)
	}
}
