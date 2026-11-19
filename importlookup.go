package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
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
	aliases := loadAliases()
	workers := make([]importAgentSearch, 0, len(agents))
	var wg sync.WaitGroup
	for _, agent := range agents {
		w := importAgentSearch{since: make(chan time.Time), reply: make(chan importAgentReply, 1)}
		workers = append(workers, w)
		wg.Add(1)
		go func(agent string, w importAgentSearch) {
			defer wg.Done()
			searchImportAgent(agent, w, aliases, query)
		}(agent, w)
	}
	return workers, func() {
		for _, w := range workers {
			close(w.since)
		}
		wg.Wait()
	}
}

func searchImportAgent(agent string, w importAgentSearch, aliases map[string]string, query string) {
	candidates := collectSessions(agent, "", aliases, true)
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
			if strings.HasSuffix(c.Path, ".baton.json") || c.Agent == "claude" || c.Agent == "pi" {
				c.NativeTitle = readImportTitle(c)
				if c.Title == "" {
					c.Title = c.NativeTitle
				}
			}
			if strings.EqualFold(c.Title, query) || strings.EqualFold(c.NativeTitle, query) {
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
	ps, err := parseCandidate(c)
	if err == nil && (isHandoffStubSession(ps) || len(ps.AllTurns) == 0 && strings.TrimSpace(ps.CompactSummary) == "") {
		return nil, nil
	}
	return ps, err
}

func readImportTitle(c SessionCandidate) string {
	f, err := os.Open(c.Path)
	if err != nil {
		return ""
	}
	defer f.Close()
	if strings.HasSuffix(c.Path, ".baton.json") {
		return readSnapshotTitle(io.LimitReader(f, importTitleWindow))
	}

	info, err := f.Stat()
	if err != nil {
		return ""
	}
	marker := []byte(`"customTitle"`)
	if c.Agent == "pi" {
		marker = []byte(`"session_info"`)
	}
	titleIn := func(start, length int64) string {
		data, _ := io.ReadAll(io.NewSectionReader(f, start, length))
		if start > 0 {
			// Start one byte before the window, then discard the partial row.
			_, data, _ = bytes.Cut(data, []byte("\n"))
		}
		var title string
		for len(data) > 0 {
			var line []byte
			line, data, _ = bytes.Cut(data, []byte("\n"))
			if !bytes.Contains(line, marker) {
				continue
			}
			var row struct {
				Type        string `json:"type"`
				Name        string `json:"name"`
				CustomTitle string `json:"customTitle"`
			}
			if json.Unmarshal(line, &row) != nil {
				continue
			}
			value := row.CustomTitle
			if c.Agent == "pi" {
				if row.Type != "session_info" {
					continue
				}
				value = row.Name
			}
			if value = strings.TrimSpace(value); value != "" {
				title = value
			}
		}
		return title
	}
	if size := info.Size(); size > 2*importTitleWindow {
		if title := titleIn(size-importTitleWindow-1, importTitleWindow+1); title != "" {
			return title
		}
		return titleIn(0, importTitleWindow)
	}
	return titleIn(0, info.Size())
}

// Baton writes the snapshot title before the turn arrays. Stop at the arrays
// even for old snapshots without a title.
func readSnapshotTitle(r io.Reader) string {
	d := json.NewDecoder(r)
	if token, err := d.Token(); err != nil || token != json.Delim('{') {
		return ""
	}
	for d.More() {
		key, err := d.Token()
		if err != nil || key == "all_turns" || key == "post_compact" {
			break
		}
		if key == "title" {
			var title string
			if d.Decode(&title) == nil {
				return strings.TrimSpace(title)
			}
			break
		}
		var value json.RawMessage
		if d.Decode(&value) != nil {
			break
		}
	}
	return ""
}
