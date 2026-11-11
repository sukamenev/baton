package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// batonTempRoot is the per-user root for baton's temporary files:
// $TMPDIR/baton-<uid>/ on Unix and %TEMP%\baton\ on Windows (where %TEMP% is
// already private). It holds handoff/ (files written by `baton load`) and
// cache/ (`baton import` metadata). The root is created with 0700 and is used
// only if it is a real directory owned by the current user: a shared /tmp could
// otherwise hold a directory or symlink planted by someone else.
// The checks run on every call (a mkdir and an lstat), so the result always
// follows the current TMPDIR.
func batonTempRoot() (string, bool) {
	root := filepath.Join(os.TempDir(), tempRootName())
	_ = os.Mkdir(root, 0700)
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || !ownedByCurrentUser(info) {
		return root, false
	}
	if info.Mode().Perm()&0077 != 0 && os.Chmod(root, 0700) != nil {
		return root, false
	}
	return root, true
}

var (
	fallbackHandoffOnce sync.Once
	fallbackHandoffDir  string
)

// handoffBaseDir is where `baton load` writes active_context*.md and the global
// rule files. If the per-user root cannot be trusted, this run uses a fresh
// private directory with a random name instead.
func handoffBaseDir() string {
	if root, ok := batonTempRoot(); ok {
		return filepath.Join(root, "handoff")
	}
	fallbackHandoffOnce.Do(func() {
		dir, err := os.MkdirTemp("", "baton-handoff-")
		if err != nil {
			dir = filepath.Join(os.TempDir(), fmt.Sprintf("baton-handoff-%d", os.Getpid()))
		}
		fallbackHandoffDir = dir
		root, _ := batonTempRoot()
		fmt.Fprintf(os.Stderr, "baton: warning: %s is not a private directory owned by you; using %s for this run\n", root, dir)
	})
	return fallbackHandoffDir
}

// batonCacheDir returns the cache directory, or "" when the per-user root cannot
// be trusted (the cache is then simply not used).
func batonCacheDir() string {
	if root, ok := batonTempRoot(); ok {
		return filepath.Join(root, "cache")
	}
	return ""
}
