package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// importMeta is what `baton import` needs from a raw session besides its listing
// fields: the active context size and whether it is a "baton load" stub. Both
// require parsing the whole log, so they are cached per session and reused while
// the log's modification time and size stay the same.
type importMeta struct {
	ModTime int64 `json:"mtime"`
	Bytes   int64 `json:"bytes"`
	Context int   `json:"context_size"`
	// Stub hides the session from the list: a "baton load" stub, or a session
	// with no messages at all (the agent was opened and closed).
	Stub    bool `json:"stub"`
	Version int  `json:"v,omitempty"`
}

// importMetaVersion invalidates cached entries computed by older rules.
const importMetaVersion = 2

// importParseWorkers bounds parallel parsing: big logs take hundreds of MB
// of memory while parsed.
const importParseWorkers = 4

func importCachePath() string {
	dir := batonCacheDir()
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, "import.json")
}

func loadImportCache() map[string]importMeta {
	out := make(map[string]importMeta)
	if path := importCachePath(); path != "" {
		if data, err := os.ReadFile(path); err == nil {
			_ = json.Unmarshal(data, &out)
		}
	}
	return out
}

// saveImportCache writes the cache atomically; errors are ignored because the
// cache only saves time.
func saveImportCache(entries map[string]importMeta) {
	path := importCachePath()
	if path == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return
	}
	data, err := json.Marshal(entries)
	if err != nil {
		return
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "import-*.json")
	if err != nil {
		return
	}
	_, werr := tmp.Write(data)
	cerr := tmp.Close()
	if werr != nil || cerr != nil || os.Rename(tmp.Name(), path) != nil {
		_ = os.Remove(tmp.Name())
	}
}

// candidateFingerprint identifies the state of a raw session: the log file's
// modification time and size (for OpenCode, whose sessions share one database,
// the session's own update time).
func candidateFingerprint(c SessionCandidate) (int64, int64) {
	if strings.Contains(c.Path, "::") {
		return c.ModTime.UnixNano(), 0
	}
	info, err := os.Stat(c.Path)
	if err != nil {
		return c.ModTime.UnixNano(), -1
	}
	return info.ModTime().UnixNano(), info.Size()
}

// measureCandidates returns importMeta for each candidate, taking unchanged
// sessions from cache and parsing the rest in parallel. Every result is
// recorded in fresh, which becomes the next cache.
func measureCandidates(cands []SessionCandidate, cache, fresh map[string]importMeta) []importMeta {
	metas := make([]importMeta, len(cands))
	var todo []int
	for i, c := range cands {
		mt, sz := candidateFingerprint(c)
		if m, ok := cache[c.Path]; ok && m.ModTime == mt && m.Bytes == sz && m.Version == importMetaVersion {
			metas[i] = m
		} else {
			metas[i] = importMeta{ModTime: mt, Bytes: sz, Version: importMetaVersion}
			todo = append(todo, i)
		}
	}
	var wg sync.WaitGroup
	sem := make(chan struct{}, importParseWorkers)
	for _, i := range todo {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			c := cands[i]
			if ps, err := parseSessionFile(c.Agent, c.Path, c.ModTime); err == nil {
				metas[i].Context = activeContextSize(ps.CompactSummary, ps.PostCompact)
				empty := len(ps.AllTurns) == 0 && strings.TrimSpace(ps.CompactSummary) == ""
				metas[i].Stub = empty || isHandoffStubSession(ps)
			}
		}(i)
	}
	wg.Wait()
	for i, c := range cands {
		fresh[c.Path] = metas[i]
	}
	return metas
}
