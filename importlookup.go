package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	importDefaultDays = 60
	importWeekDays    = 7
	importWeekSeconds = importWeekDays * 24 * 60 * 60
	importTitleWindow = 256 << 10
)

type importAgentReply struct {
	matches []SessionCandidate
	next    time.Time // newest unchecked session, zero when the worker is exhausted
}

type importAgentSearch struct {
	since chan time.Time
	reply chan importAgentReply
}

// Each worker enumerates and sorts its sessions once, then retains its place
// between requests. Closing the request channels stops all workers.
func startImportSearch(query string) ([]importAgentSearch, func()) {
	agents := []string{"claude", "codex", "agy", "pi", "opencode", "cursor", "kimi", "kiro", "imported"}
	workers := make([]importAgentSearch, 0, len(agents))
	var wg sync.WaitGroup
	for _, agent := range agents {
		w := importAgentSearch{since: make(chan time.Time), reply: make(chan importAgentReply, 1)}
		workers = append(workers, w)
		wg.Add(1)
		go func(agent string, w importAgentSearch) {
			defer wg.Done()
			searchImportAgent(agent, w, query)
		}(agent, w)
	}
	return workers, func() {
		for _, w := range workers {
			close(w.since)
		}
		wg.Wait()
	}
}

func searchImportAgent(agent string, w importAgentSearch, query string) {
	candidates := collectSessions(agent, "", true)
	pending := candidates[:0]
	for _, c := range candidates {
		if !isCallerOwnSession(c.Path) {
			pending = append(pending, c)
		}
	}
	sortImportMatches(pending)

	q := strings.ToLower(query)
	var ids []SessionCandidate
	if len(q) >= 4 {
		ids = candidatesByIDPrefix(pending, q)
	}
	report := func(matches []SessionCandidate) {
		var next time.Time
		if len(pending) > 0 {
			next = pending[0].ModTime
		}
		w.reply <- importAgentReply{matches: matches, next: next}
	}
	report(ids)
	for since := range w.since {
		var matches []SessionCandidate
		for len(pending) > 0 && !pending[0].ModTime.Before(since) {
			c := pending[0]
			pending = pending[1:]
			if strings.HasSuffix(c.Path, ".baton.json") {
				continue // saved titles are addressed through baton load
			}
			c.Title = currentSessionTitle(c)
			if strings.EqualFold(c.Title, query) {
				matches = append(matches, c)
			}
		}
		report(matches)
	}
}

func collectImportReplies(workers []importAgentSearch) ([]SessionCandidate, time.Time) {
	var matches []SessionCandidate
	var next time.Time
	for _, w := range workers {
		reply := <-w.reply
		matches = append(matches, reply.matches...)
		if reply.next.After(next) {
			next = reply.next
		}
	}
	sortImportMatches(matches)
	return matches, next
}

func sortImportMatches(candidates []SessionCandidate) {
	sort.Slice(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]
		if a.ModTime.Equal(b.ModTime) {
			return a.Path < b.Path
		}
		return a.ModTime.After(b.ModTime)
	})
}

func lookupImport(query string, days int, now time.Time) (SessionCandidate, *ParsedSession, error) {
	workers, stop := startImportSearch(query)
	defer stop()

	ids, next := collectImportReplies(workers)
	if len(ids) != 0 {
		if len(ids) > 1 {
			return SessionCandidate{}, nil, fmt.Errorf("session ID %q matches %d sessions; use a longer ID", query, len(ids))
		}
		ps, err := parseImportMatch(ids[0])
		return ids[0], ps, err
	}

	now = now.UTC()
	cutoff := now.AddDate(0, 0, -days)
	for !next.IsZero() && !next.Before(cutoff) {
		// Jump straight to the next populated week if there is a gap.
		seconds := now.Unix() - next.Unix()
		if now.Nanosecond() < next.Nanosecond() {
			seconds--
		}
		weeks := max(0, seconds/importWeekSeconds)
		since := now.AddDate(0, 0, -int((weeks+1)*importWeekDays))
		if since.Before(cutoff) {
			since = cutoff
		}
		for _, w := range workers {
			w.since <- since
		}
		var batch []SessionCandidate
		batch, next = collectImportReplies(workers)
		for _, c := range batch {
			ps, err := parseImportMatch(c)
			if ps != nil || err != nil {
				return c, ps, err
			}
		}
	}
	return SessionCandidate{}, nil, nil
}

func parseImportMatch(c SessionCandidate) (*ParsedSession, error) {
	if !strings.HasSuffix(c.Path, ".baton.json") && c.Title == "" {
		c.Title = currentSessionTitle(c)
	}
	ps, err := parseCandidate(c)
	if err == nil && (isHandoffStubSession(ps) || len(ps.AllTurns) == 0 && strings.TrimSpace(ps.CompactSummary) == "") {
		return nil, nil
	}
	return ps, err
}

func currentSessionTitle(c SessionCandidate) string {
	switch c.Agent {
	case "claude":
		return lastJSONLField(c.Path, "custom-title", "customTitle")
	case "pi":
		return lastJSONLField(c.Path, "session_info", "name")
	default:
		return strings.TrimSpace(c.Title)
	}
}

// Read backwards so a recently appended rename wins, even if it is buried
// within a long transcript. Keep at most one chunk of a split row and skip
// large message/tool rows without buffering them.
func lastJSONLField(path, kind, field string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return ""
	}

	chunk := int(min(info.Size(), importTitleWindow))
	buffer := make([]byte, 2*chunk)
	marker := []byte(`"` + kind + `"`)
	titleInRow := func(line []byte) string {
		if !bytes.Contains(line, marker) {
			return ""
		}
		var row map[string]json.RawMessage
		if json.Unmarshal(line, &row) != nil {
			return ""
		}
		var typ, title string
		if json.Unmarshal(row["type"], &typ) != nil || typ != kind || json.Unmarshal(row[field], &title) != nil {
			return ""
		}
		return strings.TrimSpace(title)
	}

	carry := 0
	discard := false
	for end := info.Size(); end > 0; {
		n := int(min(end, int64(chunk)))
		start := end - int64(n)
		if _, err := f.ReadAt(buffer[chunk-n:chunk], start); err != nil {
			return ""
		}
		data := buffer[chunk-n : chunk+carry]
		for {
			i := bytes.LastIndexByte(data, '\n')
			if i < 0 {
				break
			}
			if !discard {
				if title := titleInRow(data[i+1:]); title != "" {
					return title
				}
			}
			discard = false
			data = data[:i]
		}
		if start == 0 && !discard {
			return titleInRow(data)
		}
		discard = discard || len(data) > chunk
		carry = 0
		if !discard {
			carry = copy(buffer[chunk:], data)
		}
		end = start
	}
	return ""
}
