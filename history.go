package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// `baton history` shows a session's turns on request instead of writing the whole
// transcript next to every handoff. It reads the agent's own log while it exists
// on this machine (all turns, also those before the latest compaction), else the
// snapshot saved in baton (the turns after the latest compaction).

const historyGrepLimit = 50

// resolveHistorySession finds a session by the ID printed in a handoff
// (`claude-<uuid>`, `codex-rollout-...`), an ID prefix, or a saved name.
func resolveHistorySession(q string) (*ParsedSession, error) {
	q = strings.TrimSpace(q)
	if q == "" {
		return nil, fmt.Errorf("give a session ID or a saved session name")
	}
	low := strings.ToLower(q)
	// The agent's own log: all turns.
	cands := discoverSessions("all", "")
	for _, c := range cands {
		if strings.EqualFold(sessionIDForPath(c.Agent, c.Path), q) {
			return parseCandidate(c)
		}
	}
	// A saved snapshot, by name or by the session ID it was saved from.
	for _, c := range savedSessions("all", "", true) {
		id, _ := savedSnapshotSessionID(c.Path)
		if !strings.EqualFold(c.Title, q) && !strings.EqualFold(id, q) {
			continue
		}
		snap, err := parseSessionFile(c.Agent, c.Path, c.ModTime)
		if err != nil {
			return nil, err
		}
		if ps, ok := sourceSession(snap, cands); ok {
			return ps, nil
		}
		return snap, nil
	}
	// An ID or its prefix as shown in lists (8 characters), with or without the
	// agent in front.
	for _, agent := range []string{"claude", "codex", "agy", "pi", "opencode", "cursor", "kimi"} {
		low = strings.TrimPrefix(low, agent+"-")
	}
	if c, ok := findCandidateByID(cands, low); ok {
		return parseCandidate(c)
	}
	return nil, fmt.Errorf("no session %q: give the Session ID from the handoff or a saved session name", q)
}

// sourceSession parses the agent log a snapshot was saved from, if it is on this
// machine: it still has the turns before the latest compaction.
func sourceSession(snap *ParsedSession, cands []SessionCandidate) (*ParsedSession, bool) {
	for _, c := range cands {
		if sessionIDForPath(c.Agent, c.Path) == snap.SessionID {
			ps, err := parseCandidate(c)
			return ps, err == nil
		}
	}
	return nil, false
}

// historyReport implements `baton history <session> [N | from to | --grep text]`.
func historyReport(args []string) (string, error) {
	if len(args) == 0 {
		return "", fmt.Errorf("usage: baton history <session_id|name> [turn | from to | --grep text]")
	}
	ps, err := resolveHistorySession(args[0])
	if err != nil {
		return "", err
	}
	rest := args[1:]
	switch {
	case len(rest) == 0:
		return historyIndex(ps), nil
	case rest[0] == "--grep" || rest[0] == "-g":
		if len(rest) < 2 {
			return "", fmt.Errorf("usage: baton history <session> --grep <text>")
		}
		return historyGrep(ps, strings.Join(rest[1:], " ")), nil
	}
	from, err := strconv.Atoi(strings.TrimPrefix(rest[0], "#"))
	if err != nil {
		return "", fmt.Errorf("turn number expected, got %q", rest[0])
	}
	if len(rest) == 1 {
		for _, t := range ps.AllTurns {
			if t.TurnID == from {
				return renderTurn(t, true), nil
			}
		}
		return "", fmt.Errorf("turn #%d is not in %s%s", from, ps.SessionID, historyCoverage(ps))
	}
	to, err := strconv.Atoi(strings.TrimPrefix(rest[1], "#"))
	if err != nil || to < from {
		return "", fmt.Errorf("turn range expected, got %q %q", rest[0], rest[1])
	}
	var sb strings.Builder
	for _, t := range ps.AllTurns {
		if t.TurnID >= from && t.TurnID <= to {
			sb.WriteString(renderTurn(t, false))
		}
	}
	if sb.Len() == 0 {
		return "", fmt.Errorf("no turns #%d-#%d in %s%s", from, to, ps.SessionID, historyCoverage(ps))
	}
	sb.WriteString("\nLong outputs are shortened; `baton history " + ps.SessionID + " <N>` prints turn N in full.\n")
	return sb.String(), nil
}

// renderTurn prints one turn; full adds the untruncated tool output.
func renderTurn(t SessionTurn, full bool) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("### [Turn #%d] %s\n", t.TurnID, t.Role))
	if full && t.RawOutput != "" {
		sb.WriteString(t.RawOutput + "\n\n")
		return sb.String()
	}
	sb.WriteString(t.Text + "\n\n")
	return sb.String()
}

// historyCoverage says which turns the source holds, for "not found" errors.
func historyCoverage(ps *ParsedSession) string {
	if len(ps.AllTurns) == 0 {
		return " (it has no turns)"
	}
	s := fmt.Sprintf(" (it holds turns #%d-#%d", ps.AllTurns[0].TurnID, ps.AllTurns[len(ps.AllTurns)-1].TurnID)
	if ps.AllTurns[0].TurnID > 1 {
		// A snapshot keeps the turns after the latest compaction only.
		s += fmt.Sprintf("; earlier turns are only in the agent's own log `%s`, which is not on this machine", displayPath(ps.SourceFile))
	}
	return s + ")"
}

// historyIndex lists the user's prompts with turn numbers and marks the latest
// compaction, so the agent can pick the turns to read.
func historyIndex(ps *ParsedSession) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("# History of %s [%s]\n\n", ps.SessionID, ps.Agent))
	if ps.Title != "" {
		sb.WriteString(fmt.Sprintf("Session name: **%s**  \n", ps.Title))
	}
	sb.WriteString(fmt.Sprintf("Source: `%s`%s\n\n", displayPath(ps.SourceFile), historyCoverage(ps)))
	for _, t := range ps.AllTurns {
		if ps.PreCompactTurns > 0 && t.TurnID == ps.PreCompactTurns+1 {
			sb.WriteString(fmt.Sprintf("--- latest compaction after turn #%d ---\n", ps.PreCompactTurns))
		}
		if t.Role != "user" || t.Meta {
			continue
		}
		if p := cleanUserPrompt(t.Text); p != "" {
			sb.WriteString(fmt.Sprintf("#%d %s\n", t.TurnID, truncateInline(p, 160)))
		}
	}
	sb.WriteString(fmt.Sprintf("\nRead turns: `baton history %s <from> <to>`; one turn in full: `baton history %s <N>`; search: `baton history %s --grep <text>`.\n",
		ps.SessionID, ps.SessionID, ps.SessionID))
	return sb.String()
}

// historyGrep lists the turns whose text or full output contains the text.
func historyGrep(ps *ParsedSession, text string) string {
	needle := strings.ToLower(text)
	var sb strings.Builder
	n := 0
	for _, t := range ps.AllTurns {
		body := t.RawOutput
		if body == "" {
			body = t.Text
		}
		low := strings.ToLower(body)
		i := strings.Index(low, needle)
		if i < 0 {
			continue
		}
		n++
		if n > historyGrepLimit {
			sb.WriteString(fmt.Sprintf("... more matches; narrow the search.\n"))
			break
		}
		start := i - 60
		if start < 0 {
			start = 0
		}
		end := i + len(needle) + 60
		if end > len(body) {
			end = len(body)
		}
		// Keep the snippet on UTF-8 boundaries.
		for start > 0 && !utf8Start(body[start]) {
			start--
		}
		for end < len(body) && !utf8Start(body[end]) {
			end++
		}
		sb.WriteString(fmt.Sprintf("#%d %s: %s\n", t.TurnID, t.Role, truncateInline(strings.TrimSpace(body[start:end]), 200)))
	}
	if n == 0 {
		return fmt.Sprintf("No turn of %s contains %q%s.\n", ps.SessionID, text, historyCoverage(ps))
	}
	return sb.String()
}

func utf8Start(b byte) bool { return b&0xC0 != 0x80 }

// runHistory prints a history report or exits with an error.
func runHistory(args []string) {
	text, err := historyReport(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	fmt.Print(text)
}
