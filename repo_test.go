package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRepoURLs(t *testing.T) {
	same := []string{"git@github.com:u/r.git", "ssh://git@github.com/u/r", "https://github.com/u/r/", "https://GitHub.com/u/r.git"}
	for _, u := range same {
		if got := normalizeRepoURL(u); got != "github.com/u/r" {
			t.Errorf("%s -> %s", u, got)
		}
	}
	if got := stripURLCredentials("https://user:tok@example.com/r.git"); got != "https://example.com/r.git" {
		t.Errorf("credentials must be removed: %s", got)
	}
	if got := stripURLCredentials("git@github.com:u/r.git"); got != "git@github.com:u/r.git" {
		t.Errorf("scp-like URLs stay: %s", got)
	}
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestRepoChecks(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	tmp := t.TempDir()
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	origin := filepath.Join(tmp, "origin.git")
	src := filepath.Join(tmp, "src")
	git(t, tmp, "init", "-q", "--bare", "-b", "main", origin)
	git(t, tmp, "clone", "-q", origin, src)
	git(t, src, "checkout", "-q", "-b", "main")
	git(t, src, "commit", "-q", "--allow-empty", "-m", "one")
	git(t, src, "push", "-q", "-u", "origin", "main")

	r := detectRepo(src)
	if r == nil || r.VCS != "git" || r.Branch != "main" || r.URL != origin || r.Subdir != "" {
		t.Fatalf("unexpected repo info: %+v", r)
	}
	if w := unpushedRepoWarnings(src, r); len(w) != 0 {
		t.Fatalf("everything is pushed: %v", w)
	}
	git(t, src, "commit", "-q", "--allow-empty", "-m", "two")
	r = detectRepo(src)
	if w := unpushedRepoWarnings(src, r); len(w) != 1 || !strings.Contains(w[0], "1 commit(s)") {
		t.Fatalf("expected an unpushed-commit warning: %v", w)
	}

	// The receiver: not a checkout at all.
	plain := filepath.Join(tmp, "plain")
	_ = os.MkdirAll(plain, 0755)
	if w := repoMismatchWarnings(plain, r); len(w) != 1 || !strings.Contains(w[0], "git clone "+origin) {
		t.Fatalf("expected a clone hint: %v", w)
	}
	// A clone without the unpushed commit.
	dst := filepath.Join(tmp, "dst")
	git(t, tmp, "clone", "-q", origin, dst)
	if w := repoMismatchWarnings(dst, r); len(w) != 1 || !strings.Contains(w[0], "is not here") {
		t.Fatalf("expected a missing-revision hint: %v", w)
	}
	git(t, src, "push", "-q")
	git(t, dst, "fetch", "-q")
	if w := repoMismatchWarnings(dst, r); len(w) != 1 || !strings.Contains(w[0], "git checkout main") {
		t.Fatalf("expected an update hint: %v", w)
	}
	git(t, dst, "merge", "-q", "--ff-only", "origin/main")
	if w := repoMismatchWarnings(dst, r); len(w) != 0 {
		t.Fatalf("same code, no warnings: %v", w)
	}
	// Another branch on the sending side.
	git(t, src, "checkout", "-q", "-b", "feature")
	r = detectRepo(src)
	if w := unpushedRepoWarnings(src, r); len(w) != 1 || !strings.Contains(w[0], "git push -u origin feature") {
		t.Fatalf("expected a no-upstream warning: %v", w)
	}
	if w := repoMismatchWarnings(dst, r); len(w) == 0 || !strings.Contains(strings.Join(w, " "), "Branch `feature` is not here") {
		t.Fatalf("expected a missing-branch hint: %v", w)
	}
	// A different repository.
	other := filepath.Join(tmp, "other")
	git(t, tmp, "init", "-q", other)
	git(t, other, "remote", "add", "origin", "https://example.com/x.git")
	if w := repoMismatchWarnings(other, r); len(w) != 1 || !strings.Contains(w[0], "but the session worked in") {
		t.Fatalf("expected a different-repository warning: %v", w)
	}
}
