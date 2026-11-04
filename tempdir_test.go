package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// resetTempRoot forgets the cached temp root so a test can point TMPDIR elsewhere.
func resetTempRoot(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	t.Setenv("TMP", tmp)
	t.Setenv("TEMP", tmp)
	fallbackHandoffOnce = sync.Once{}
	fallbackHandoffDir = ""
	t.Cleanup(func() {
		fallbackHandoffOnce = sync.Once{}
		fallbackHandoffDir = ""
	})
	return tmp
}

func TestPrivateTempRoot(t *testing.T) {
	tmp := resetTempRoot(t)
	root, ok := batonTempRoot()
	if !ok || root != filepath.Join(tmp, tempRootName()) {
		t.Fatalf("root %q ok=%v", root, ok)
	}
	if got := handoffBaseDir(); got != filepath.Join(root, "handoff") {
		t.Fatalf("handoff dir %q", got)
	}
	if got := batonCacheDir(); got != filepath.Join(root, "cache") {
		t.Fatalf("cache dir %q", got)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(root)
		if err != nil || info.Mode().Perm() != 0700 {
			t.Fatalf("root mode: %v %v", info.Mode(), err)
		}
	}
}

func TestPlantedTempRootIsNotUsed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink planting targets the shared Unix /tmp")
	}
	tmp := resetTempRoot(t)
	target := filepath.Join(tmp, "attacker")
	if err := os.Mkdir(target, 0777); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(tmp, tempRootName())); err != nil {
		t.Fatal(err)
	}
	if _, ok := batonTempRoot(); ok {
		t.Fatal("a symlinked root must not be trusted")
	}
	if batonCacheDir() != "" {
		t.Fatal("cache must be disabled without a trusted root")
	}
	dir := handoffBaseDir()
	if strings.HasPrefix(dir, target) || strings.HasPrefix(dir, filepath.Join(tmp, tempRootName())) {
		t.Fatalf("handoff dir %q goes through the planted link", dir)
	}
	_ = os.RemoveAll(dir)
}

func TestImportCacheReusesUnchangedSessions(t *testing.T) {
	resetTempRoot(t)
	log := filepath.Join(t.TempDir(), "11111111-2222-3333-4444-555555555555.jsonl")
	write := func(body string, at time.Time) SessionCandidate {
		if err := os.WriteFile(log, []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(log, at, at); err != nil {
			t.Fatal(err)
		}
		return SessionCandidate{Agent: "claude", Path: log, ModTime: at}
	}
	line := func(text string) string {
		return `{"type":"user","message":{"content":"` + text + `"}}` + "\n"
	}
	at := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)

	c := write(line("first request with some text"), at)
	fresh := map[string]importMeta{}
	first := measureCandidates([]SessionCandidate{c}, loadImportCache(), fresh)[0]
	if first.Context == 0 {
		t.Fatal("context size not measured")
	}
	saveImportCache(fresh)

	// Same size and time: the cached value is reused even though the content differs.
	c = write(line("FIRST REQUEST WITH SOME TEXT"), at)
	again := measureCandidates([]SessionCandidate{c}, loadImportCache(), map[string]importMeta{})[0]
	if again != first {
		t.Fatalf("cache not used: %+v vs %+v", again, first)
	}

	// The session grew: it is parsed again.
	c = write(line("first request with some text")+line("and a longer follow-up request"), at.Add(time.Minute))
	grown := measureCandidates([]SessionCandidate{c}, loadImportCache(), map[string]importMeta{})[0]
	if grown.Context <= first.Context {
		t.Fatalf("changed session not re-measured: %+v", grown)
	}
}
