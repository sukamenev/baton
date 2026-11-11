package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// A Codex session compacted on OpenAI's servers keeps its summary encrypted:
// only OpenAI's model can read it, and it does, as part of its context. baton
// knows when that is the case, so it asks the Codex agent running it to retell
// its context into a file and to rerun with --summary; the recap becomes the
// summary. `baton save` saves the readable stand-in first (the user's kept
// messages and the conversation before the compaction) and asks in its output;
// `baton push` sends nothing until it has the recap, so only one link goes out.
// A session imported from another agent cannot be asked.

// recapFile is the recap the agent wrote (`--summary <file>`).
var recapFile string

// takeRecapFlags removes --summary <file> from args.
func takeRecapFlags(args []string) []string {
	var out []string
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "--summary" && i+1 < len(args):
			recapFile = args[i+1]
			i++
		case strings.HasPrefix(a, "--summary="):
			recapFile = strings.TrimPrefix(a, "--summary=")
		default:
			out = append(out, a)
		}
	}
	return out
}

// recapPath is where baton suggests writing a recap it asks for.
func recapPath(ps *ParsedSession) string {
	return filepath.Join(os.TempDir(), "baton-recap-"+shortSessionID(agentSessionID(ps.SourceFile))+".md")
}

// recapRequest is what the agent is asked to write.
const recapRequest = `a detailed recap of this whole session:
- the goals and the user's instructions and preferences;
- what was done, the decisions made and why;
- files changed, commands and checks run and their results;
- commits, builds, deployments, with exact names, paths, versions, and hashes;
- what is unconfirmed or still to do, and the last state of the work.
Include details from before the compaction. If your context holds an earlier recap, carry its facts (paths, hashes, decisions) over in full. Write in the language of the conversation; leave out secrets.`

// applyRecap puts the agent's recap in place of an unreadable compaction
// summary. It returns a note for the output: that the recap was not needed, or,
// when the Codex session running baton has an encrypted summary and no recap,
// the request to write one and rerun (retry is the command). With mustHave
// (push: a link already sent cannot be improved) that request is an error and
// nothing is done.
func applyRecap(ps *ParsedSession, retry string, mustHave bool) (string, error) {
	if recapFile != "" {
		if !ps.SummaryEncrypted {
			return "\n\nThe recap was not used: this session has no encrypted compaction summary, so its conversation is handed over as it is.", nil
		}
		data, err := os.ReadFile(recapFile)
		if err != nil {
			return "", fmt.Errorf("cannot read the recap: %w", err)
		}
		text := strings.TrimSpace(string(data))
		if text == "" {
			return "", fmt.Errorf("the recap %s is empty", recapFile)
		}
		ps.CompactSummary = fmt.Sprintf("[Recap written by %s when the session was saved: its compaction summary is encrypted, so the agent retold its own context.]\n\n%s", ps.Agent, text)
		ps.SummaryEncrypted = false
		return "", nil
	}
	if !ps.SummaryEncrypted || ps.Agent != "codex" || detectCallerAgent() != "codex" {
		return "", nil
	}
	path := recapPath(ps)
	if mustHave {
		return "", fmt.Errorf("nothing was sent yet. This Codex session was compacted on OpenAI's servers: its summary is encrypted and baton cannot read it, but it is in your context. Write %s\nSave it to `%s`, then run `%s --summary %s`.",
			recapRequest, path, retry, path)
	}
	return fmt.Sprintf("\n\nThis Codex session was compacted on OpenAI's servers: its summary is encrypted, so baton saved the user's kept messages and the conversation before the compaction instead. The summary is in your context: write %s\nSave it to `%s`, then run `%s --summary %s` to hand over the full context.",
		recapRequest, path, retry, path), nil
}
