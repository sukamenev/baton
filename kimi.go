package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type kimiSessionState struct {
	ID         string `json:"id"`
	CWD        string `json:"cwd"`
	WorkDir    string `json:"workDir"`
	Title      string `json:"title"`
	LastPrompt string `json:"lastPrompt"`
}

func kimiBaseDir(home string) string {
	if env := strings.TrimSpace(os.Getenv("KIMI_CODE_HOME")); env != "" {
		return env
	}
	return filepath.Join(home, ".kimi-code")
}

func kimiSessionDir(wirePath string) string {
	return filepath.Dir(filepath.Dir(filepath.Dir(wirePath)))
}

func readKimiSessionState(sessionDir string) kimiSessionState {
	var state kimiSessionState
	for _, name := range []string{"state.json", filepath.Join("session-meta", "state.json")} {
		data, err := os.ReadFile(filepath.Join(sessionDir, name))
		if err == nil && json.Unmarshal(data, &state) == nil {
			return state
		}
	}
	return state
}

func loadKimiWorkDirs(root string) map[string]string {
	out := make(map[string]string)
	f, err := os.Open(filepath.Join(root, "session_index.jsonl"))
	if err != nil {
		return out
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 2*1024*1024)
	for scanner.Scan() {
		var row struct {
			SessionID string `json:"sessionId"`
			WorkDir   string `json:"workDir"`
		}
		if json.Unmarshal(scanner.Bytes(), &row) == nil && row.SessionID != "" && row.WorkDir != "" {
			out[row.SessionID] = row.WorkDir
		}
	}
	return out
}

// discoverKimiSessions reads only the current v2 layout shared by Kimi Code CLI
// and Desktop. The index supplies metadata; sessionDir is deliberately not trusted.
func discoverKimiSessions(root string, aliases map[string]string, query string) []SessionCandidate {
	workDirs := loadKimiWorkDirs(root)
	wires, _ := filepath.Glob(filepath.Join(root, "sessions", "*", "*", "agents", "main", "wire.jsonl"))
	var out []SessionCandidate
	for _, path := range wires {
		sessionDir := kimiSessionDir(path)
		externalID := filepath.Base(sessionDir)
		// Kimi names new sessions session_<uuid>, but a session id can also be
		// given explicitly, so the prefix is optional.
		if strings.TrimPrefix(externalID, "session_") == "" {
			continue
		}
		info, err := os.Stat(path)
		if err != nil || info.IsDir() || info.Size() == 0 {
			continue
		}
		state := readKimiSessionState(sessionDir)
		cwd := workDirs[externalID]
		if cwd == "" {
			cwd = state.CWD
		}
		if cwd == "" {
			cwd = state.WorkDir
		}
		title := aliases[path]
		if title == "" {
			title = strings.TrimSpace(state.Title)
		}
		preview := truncateInline(cleanUserPrompt(state.LastPrompt), 140)
		if query != "" && !candidateMatchesQuery(path, title, preview, query) {
			continue
		}
		out = append(out, SessionCandidate{
			Agent:   "kimi",
			Path:    path,
			Title:   title,
			CWD:     cwd,
			ModTime: info.ModTime(),
			Preview: preview,
			ID:      strings.TrimPrefix(externalID, "session_"),
		})
	}
	return out
}

func kimiTimestamp(ms int64) string {
	if ms <= 0 {
		return ""
	}
	return time.UnixMilli(ms).UTC().Format(time.RFC3339Nano)
}

func kimiToolCall(call map[string]interface{}) (id, name, rawArgs string, args map[string]interface{}) {
	id, _ = call["id"].(string)
	name, _ = call["name"].(string)
	rawValue := call["arguments"]
	if function, ok := call["function"].(map[string]interface{}); ok {
		if name == "" {
			name, _ = function["name"].(string)
		}
		if rawValue == nil {
			rawValue = function["arguments"]
		}
	}
	switch value := rawValue.(type) {
	case string:
		rawArgs = value
		_ = json.Unmarshal([]byte(value), &args)
	case nil:
		rawArgs = "{}"
	default:
		data, _ := json.Marshal(value)
		rawArgs = string(data)
		args, _ = value.(map[string]interface{})
	}
	return id, name, rawArgs, args
}

// kimiHiddenOrigins are prompt origins Kimi Code itself never shows in the
// conversation (kap-server conversationFold.ts): injected reminders, retries,
// system triggers, and the compaction summary, which baton reads separately.
var kimiHiddenOrigins = map[string]bool{
	"injection":          true,
	"retry":              true,
	"system_trigger":     true,
	"compaction_summary": true,
}

// kimiOriginKind returns the prompt origin of a history entry. Kimi keeps it in
// the entry's meta ({"message": {...}, "meta": {"origin": {"kind": ...}}});
// message.origin, used by context records, is accepted as well.
func kimiOriginKind(entry, msg map[string]interface{}) string {
	for _, holder := range []interface{}{entry["meta"], msg} {
		h, _ := holder.(map[string]interface{})
		if origin, ok := h["origin"].(map[string]interface{}); ok {
			kind, _ := origin["kind"].(string)
			return kind
		}
	}
	return ""
}

// kimiInjectedPart reports a text part Kimi added to a user message (a skill
// activation body or UserPromptSubmit hook output); such parts carry meta.source.
func kimiInjectedPart(part map[string]interface{}) bool {
	meta, _ := part["meta"].(map[string]interface{})
	source, _ := meta["source"].(string)
	return source != ""
}

func kimiCompactionSummary(row map[string]interface{}) string {
	switch summary := row["summary"].(type) {
	case string:
		return strings.TrimSpace(summary)
	case map[string]interface{}:
		return strings.TrimSpace(extractContentText(summary["content"]))
	}
	summary, _ := row["contextSummary"].(string)
	return strings.TrimSpace(summary)
}

// parseKimiSession consumes completed UI messages rather than the equivalent
// streaming context.append_loop_event records, avoiding duplicate turns.
func parseKimiSession(path string, ps *ParsedSession) error {
	state := readKimiSessionState(kimiSessionDir(path))
	if ps.Title == "" {
		ps.Title = strings.TrimSpace(state.Title)
	}
	if ps.CWD == "" {
		ps.CWD = state.CWD
		if ps.CWD == "" {
			ps.CWD = state.WorkDir
		}
	}

	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	modSet := make(map[string]bool)
	toolNames := make(map[string]string)
	turnID := 0
	addTurn := func(t SessionTurn, ms int64) {
		turnID++
		t.TurnID = turnID
		t.Timestamp = kimiTimestamp(ms)
		ps.AllTurns = append(ps.AllTurns, t)
		ps.PostCompact = append(ps.PostCompact, t)
	}

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024*1024), 64*1024*1024)
	for scanner.Scan() {
		var row map[string]interface{}
		if json.Unmarshal(scanner.Bytes(), &row) != nil {
			continue
		}
		typ, _ := row["type"].(string)
		ms, _ := row["time"].(float64)
		if typ == "context.apply_compaction" {
			if summary := kimiCompactionSummary(row); summary != "" {
				ps.PreCompactTurns = turnID
				ps.CompactSummary = summary
				ps.PostCompact = nil
			}
			continue
		}
		if typ == "context.clear" {
			// /clear: the model starts from an empty context, like a new session.
			ps.PreCompactTurns = turnID
			ps.CompactSummary = ""
			ps.PostCompact = nil
			continue
		}
		if typ != "agent.message.appended" {
			continue
		}
		wrapper, _ := row["message"].(map[string]interface{})
		msg, _ := wrapper["message"].(map[string]interface{})
		if msg == nil {
			continue
		}
		role, _ := msg["role"].(string)
		kind := kimiOriginKind(wrapper, msg)
		if kimiHiddenOrigins[kind] {
			continue
		}
		// Task notifications, shell commands, skill or plugin activations stay in
		// the transcript but are never taken for what the user typed.
		meta := role == "user" && kind != "" && kind != "user"
		if role == "tool" {
			raw := extractContentText(msg["content"])
			callID, _ := msg["toolCallId"].(string)
			if strings.TrimSpace(raw) != "" {
				addTurn(SessionTurn{
					Role:      "tool_result",
					ToolName:  toolNames[callID],
					Text:      summarizeToolOutput(raw),
					RawOutput: raw,
				}, int64(ms))
			}
			continue
		}
		if role != "user" && role != "assistant" {
			continue
		}
		switch content := msg["content"].(type) {
		case string:
			if strings.TrimSpace(content) != "" {
				addTurn(SessionTurn{Role: role, Text: content, Meta: meta}, int64(ms))
			}
		case []interface{}:
			for _, item := range content {
				part, _ := item.(map[string]interface{})
				partType, _ := part["type"].(string)
				switch partType {
				case "text":
					text, _ := part["text"].(string)
					if strings.TrimSpace(text) != "" {
						injected := role == "user" && kimiInjectedPart(part)
						addTurn(SessionTurn{Role: role, Text: text, Meta: meta || injected}, int64(ms))
					}
				case "think", "thinking":
					text, _ := part["think"].(string)
					if text == "" {
						text, _ = part["thinking"].(string)
					}
					if role == "assistant" && strings.TrimSpace(text) != "" {
						addTurn(SessionTurn{Role: "assistant_thinking", Text: text}, int64(ms))
					}
				}
			}
		}
		calls, _ := msg["toolCalls"].([]interface{})
		for _, item := range calls {
			call, _ := item.(map[string]interface{})
			id, name, rawArgs, args := kimiToolCall(call)
			if name == "" {
				continue
			}
			toolNames[id] = name
			recordToolSideEffects(name, args, modSet, &ps.Commands)
			addTurn(SessionTurn{
				Role:      "tool_use",
				ToolName:  name,
				Text:      fmt.Sprintf("Call `%s`: %s", name, truncateInline(rawArgs, 300)),
				RawOutput: fmt.Sprintf("Call `%s`: %s", name, rawArgs),
			}, int64(ms))
		}
	}
	ps.ModifiedFiles = sortedKeys(modSet)
	return scanner.Err()
}
