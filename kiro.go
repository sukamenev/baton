package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Kiro CLI V3 uses the shared Kiro session store. V2 JSONL and SQLite stores
// deliberately aren't scanned: their wire formats differ from V3.
func kiroBaseDir(home string) string {
	if root := strings.TrimSpace(os.Getenv("KIRO_HOME")); root != "" {
		return root
	}
	return filepath.Join(home, ".kiro")
}

type kiroSessionMeta struct {
	ID             string   `json:"id"`
	Title          string   `json:"title"`
	WorkspacePaths []string `json:"workspacePaths"`
	RootPaths      []string `json:"rootPaths"`
}

func readKiroSessionMeta(path string) (kiroSessionMeta, error) {
	var meta kiroSessionMeta
	data, err := os.ReadFile(filepath.Join(filepath.Dir(path), "session.json"))
	if err != nil {
		return meta, err
	}
	err = json.Unmarshal(data, &meta)
	return meta, err
}

func (m kiroSessionMeta) cwd() string {
	for _, paths := range [][]string{m.WorkspacePaths, m.RootPaths} {
		for _, path := range paths {
			if strings.TrimSpace(path) != "" {
				return path
			}
		}
	}
	return ""
}

// The dashboard sidecar distinguishes the user's title from an automatic title
// in session.json. Keep it relative to the source path, including custom roots.
func kiroDashboardPath(path string) string {
	return filepath.Join(filepath.Dir(filepath.Dir(filepath.Dir(path))), "dashboard-meta.json")
}

type kiroDashboard map[string]struct {
	Title string `json:"title"`
}

func readKiroDashboard(path string) kiroDashboard {
	var entries kiroDashboard
	if data, err := os.ReadFile(path); err == nil && json.Unmarshal(data, &entries) == nil {
		return entries
	}
	return nil
}

func (entries kiroDashboard) title(sessionID string) string {
	id := strings.TrimPrefix(sessionID, "sess_")
	for _, key := range []string{"sess_" + id, id} {
		if title := strings.TrimSpace(entries[key].Title); title != "" {
			return title
		}
	}
	return ""
}

func kiroUserTitle(path string) string {
	return readKiroDashboard(kiroDashboardPath(path)).title(filepath.Base(filepath.Dir(path)))
}

func kiroTitle(path string, meta kiroSessionMeta) string {
	if title := kiroUserTitle(path); title != "" {
		return title
	}
	return strings.TrimSpace(meta.Title)
}

func discoverKiroSessions(root string, aliases map[string]string, query string) []SessionCandidate {
	paths, _ := filepath.Glob(filepath.Join(root, "sessions", "*", "sess_*", "messages.jsonl"))
	dashboard := readKiroDashboard(filepath.Join(root, "sessions", "dashboard-meta.json"))
	var out []SessionCandidate
	for _, path := range paths {
		info, err := os.Stat(path)
		if err != nil || info.IsDir() || info.Size() == 0 {
			continue
		}
		meta, err := readKiroSessionMeta(path)
		if err != nil || meta.ID != filepath.Base(filepath.Dir(path)) {
			continue
		}
		nativeTitle := dashboard.title(meta.ID)
		if nativeTitle == "" {
			nativeTitle = strings.TrimSpace(meta.Title)
		}
		title := aliases[path]
		if title == "" {
			title = nativeTitle
		}
		if query != "" && !strings.Contains(strings.ToLower(meta.ID), strings.ToLower(query)) && !candidateMatchesQuery(path, title, "", query) {
			continue
		}
		out = append(out, SessionCandidate{
			Agent: "kiro", Path: path, Title: title, NativeTitle: nativeTitle, CWD: meta.cwd(),
			ModTime: info.ModTime(), ID: strings.TrimPrefix(meta.ID, "sess_"),
		})
	}
	return out
}

type kiroMessage struct {
	ID        string `json:"id"`
	Timestamp string `json:"timestamp"`
	Payload   struct {
		Type                   string         `json:"type"`
		Content                string         `json:"content"`
		OperationType          string         `json:"operationType"`
		ToolCallID             string         `json:"toolCallId"`
		ToolName               string         `json:"toolName"`
		Args                   map[string]any `json:"args"`
		Kind                   string         `json:"kind"`
		EffectiveFromMessageID string         `json:"effectiveFromMessageId"`
		SubSessionID           string         `json:"subSessionId"`
		SubAgentName           string         `json:"subAgentName"`
		Prompt                 string         `json:"prompt"`
		Response               string         `json:"response"`
	} `json:"payload"`
}

// parseKiroSession reads the append-only main transcript. A Summary replaces
// the active context while earlier turns remain available to baton hist.
// Subagent requests and returned results are in the main transcript; their
// private sub-executions are not added to the main agent's context.
func parseKiroSession(path string, ps *ParsedSession) error {
	meta, err := readKiroSessionMeta(path)
	if err != nil {
		return fmt.Errorf("read Kiro V3 session metadata: %w", err)
	}
	ps.Title = kiroTitle(path, meta)
	ps.CWD = meta.cwd()
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	type contextState struct {
		summary string
		pre     int
		post    []SessionTurn
	}
	// Capture slice headers, not copies of the transcript. Append-only slices
	// remain valid; a rewind copies its restored slice before appending to it.
	states := make(map[string]contextState)
	toolNames := make(map[string]string)
	modified := make(map[string]bool)
	add := func(row kiroMessage, turn SessionTurn) {
		turn.TurnID = len(ps.AllTurns) + 1
		turn.Timestamp = row.Timestamp
		ps.AllTurns = append(ps.AllTurns, turn)
		ps.PostCompact = append(ps.PostCompact, turn)
	}
	call := func(row kiroMessage, id, name string, args map[string]any) {
		toolNames[id] = name
		raw, _ := json.Marshal(args)
		add(row, SessionTurn{Role: "tool_use", ToolName: name,
			Text:      fmt.Sprintf("Call `%s`: %s", name, truncateInline(string(raw), 300)),
			RawOutput: fmt.Sprintf("Call `%s`: %s", name, raw)})
	}
	result := func(row kiroMessage, id, text string) {
		add(row, SessionTurn{Role: "tool_result", ToolName: toolNames[id],
			Text: summarizeToolOutput(text), RawOutput: text})
	}
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 64*1024*1024)
	for scanner.Scan() {
		var row kiroMessage
		if json.Unmarshal(scanner.Bytes(), &row) != nil {
			continue
		}
		if row.ID != "" {
			states[row.ID] = contextState{ps.CompactSummary, ps.PreCompactTurns, ps.PostCompact}
		}
		p := row.Payload
		switch p.Type {
		case "user":
			if strings.TrimSpace(p.Content) != "" {
				add(row, SessionTurn{Role: "user", Text: p.Content})
			}
		case "assistant":
			if strings.TrimSpace(p.Content) == "" {
				continue
			}
			switch p.OperationType {
			case "Summary":
				ps.CompactSummary = p.Content
				ps.PreCompactTurns = len(ps.AllTurns)
				ps.PostCompact = nil
			case "Reasoning":
				if strings.TrimSpace(p.Content) != "..." {
					add(row, SessionTurn{Role: "assistant_thinking", Text: p.Content})
				}
			default:
				add(row, SessionTurn{Role: "assistant", Text: p.Content})
			}
		case "tool_call":
			if p.ToolName == "" {
				continue
			}
			call(row, p.ToolCallID, p.ToolName, p.Args)
			recordToolSideEffects(p.ToolName, p.Args, modified, &ps.Commands)
		case "tool_result":
			result(row, p.ToolCallID, p.Content)
		case "sub_agent_start":
			call(row, p.SubSessionID, "sub_agent:"+p.SubAgentName, map[string]any{"prompt": p.Prompt})
		case "sub_agent_complete":
			result(row, p.SubSessionID, p.Response)
		case "agent_note":
			if !strings.HasPrefix(row.ID, "context_snapshot_") && strings.TrimSpace(p.Content) != "" {
				add(row, SessionTurn{Role: "system", Text: p.Content})
			}
		case "tombstone":
			// A summarization tombstone alone is not a completed compaction:
			// keep the old context until the actual Summary arrives.
			if p.Kind == "checkpoint_revert" {
				if state, ok := states[p.EffectiveFromMessageID]; ok {
					ps.CompactSummary, ps.PreCompactTurns = state.summary, state.pre
					ps.PostCompact = append([]SessionTurn(nil), state.post...)
				}
			}
		}
	}
	ps.ModifiedFiles = sortedKeys(modified)
	return scanner.Err()
}

// renameKiroSession writes only the dashboard's user-title override, never the
// live session.json. Use Kiro's directory lock and owner format so its client
// and baton cannot overwrite each other's bookmarks, tags, or titles.
func renameKiroSession(path, name, prevName string) bool {
	dashboard := kiroDashboardPath(path)
	root := filepath.Dir(dashboard)
	tmpLock, err := os.MkdirTemp(root, ".baton-title-lock-")
	if err != nil {
		return false
	}
	owner := filepath.Join(tmpLock, "owner.json")
	defer func() { _ = os.Remove(owner); _ = os.Remove(tmpLock) }()
	data, _ := json.Marshal(map[string]any{"pid": os.Getpid(), "token": filepath.Base(tmpLock)})
	if os.WriteFile(owner, data, 0600) != nil {
		return false
	}
	lock := dashboard + ".lock"
	if os.Rename(tmpLock, lock) != nil {
		return false
	}
	defer func() { _ = os.Remove(filepath.Join(lock, "owner.json")); _ = os.Remove(lock) }()

	entries := make(map[string]map[string]json.RawMessage)
	data, err = os.ReadFile(dashboard)
	if err != nil && !os.IsNotExist(err) {
		return false
	}
	if err == nil && (json.Unmarshal(data, &entries) != nil || entries == nil) {
		return false
	}
	id := strings.TrimPrefix(filepath.Base(filepath.Dir(path)), "sess_")
	for _, key := range []string{"sess_" + id, id} {
		var current string
		_ = json.Unmarshal(entries[key]["title"], &current)
		if current != "" && current != prevName && !strings.EqualFold(current, name) {
			return false // keep the user's own rename
		}
	}
	for _, key := range []string{"sess_" + id, id} {
		delete(entries[key], "title")
	}
	key := "sess_" + id
	if entries[key] == nil {
		entries[key] = map[string]json.RawMessage{"bookmarked": json.RawMessage("false"), "tags": json.RawMessage("[]")}
	}
	entries[key]["title"], _ = json.Marshal(name)
	data, err = json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return false
	}
	tmp, err := os.CreateTemp(root, ".baton-title-*.json")
	if err != nil {
		return false
	}
	defer os.Remove(tmp.Name())
	_, writeErr := tmp.Write(data)
	closeErr := tmp.Close()
	return writeErr == nil && closeErr == nil && os.Rename(tmp.Name(), dashboard) == nil
}

// The current V3 client exports KIRO_SESSION_ID; the process fallback also
// recognizes the versioned chat-cli binary without matching arbitrary paths.
func isKiroCmdline(lower string) bool {
	fields := strings.Fields(lower)
	if len(fields) == 0 {
		return false
	}
	base := filepath.Base(fields[0])
	if base == "kiro-cli" || base == "kiro-cli.exe" || strings.HasPrefix(base, "chat-cli-") {
		return true
	}
	return len(fields) > 1 && (base == "bun" || base == "node") &&
		strings.Contains(filepath.ToSlash(fields[1]), "/kiro-cli/tui.js")
}
