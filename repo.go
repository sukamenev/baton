package main

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// RepoInfo is the version-control checkout a session worked in, recorded by
// `baton push` so that the receiving computer can check it has the same code.
type RepoInfo struct {
	VCS    string `json:"vcs"`
	URL    string `json:"url,omitempty"`    // remote, without credentials
	Branch string `json:"branch,omitempty"` // empty when detached
	Rev    string `json:"rev,omitempty"`
	Subdir string `json:"subdir,omitempty"` // session folder inside the checkout ("" = root)
}

// vcsOutput runs a version-control command in dir and returns its trimmed
// output ("" on any error or after a few seconds).
func vcsOutput(dir, name string, args ...string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "LC_ALL=C", "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func vcsOK(dir, name string, args ...string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "LC_ALL=C", "GIT_TERMINAL_PROMPT=0")
	return cmd.Run() == nil
}

// subdirOf is dir relative to root in slash form ("" for the root itself).
func subdirOf(root, dir string) string {
	rel, err := filepath.Rel(resolvedFolder(root), resolvedFolder(dir))
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return ""
	}
	return filepath.ToSlash(rel)
}

// detectRepo describes the checkout dir belongs to (git, Mercurial, Fossil, or
// Subversion), or returns nil.
func detectRepo(dir string) *RepoInfo {
	if dir == "" {
		return nil
	}
	if _, err := exec.LookPath("git"); err == nil {
		if root := vcsOutput(dir, "git", "rev-parse", "--show-toplevel"); root != "" {
			r := &RepoInfo{VCS: "git", Subdir: subdirOf(root, dir)}
			r.URL = vcsOutput(dir, "git", "remote", "get-url", "origin")
			if r.URL == "" {
				if remotes := strings.Fields(vcsOutput(dir, "git", "remote")); len(remotes) > 0 {
					r.URL = vcsOutput(dir, "git", "remote", "get-url", remotes[0])
				}
			}
			if b := vcsOutput(dir, "git", "rev-parse", "--abbrev-ref", "HEAD"); b != "HEAD" {
				r.Branch = b
			}
			r.Rev = vcsOutput(dir, "git", "rev-parse", "HEAD")
			r.URL = stripURLCredentials(r.URL)
			return r
		}
	}
	if _, err := exec.LookPath("hg"); err == nil {
		if root := vcsOutput(dir, "hg", "root"); root != "" {
			return &RepoInfo{
				VCS:    "hg",
				URL:    stripURLCredentials(vcsOutput(dir, "hg", "paths", "default")),
				Branch: vcsOutput(dir, "hg", "branch"),
				Rev:    vcsOutput(dir, "hg", "log", "-r", ".", "-T", "{node}"),
				Subdir: subdirOf(root, dir),
			}
		}
	}
	if _, err := exec.LookPath("fossil"); err == nil {
		if info := vcsOutput(dir, "fossil", "info"); info != "" {
			r := &RepoInfo{VCS: "fossil"}
			for _, line := range strings.Split(info, "\n") {
				key, val, ok := strings.Cut(line, ":")
				if !ok {
					continue
				}
				val = strings.TrimSpace(val)
				switch strings.TrimSpace(key) {
				case "local-root":
					r.Subdir = subdirOf(val, dir)
				case "checkout":
					if f := strings.Fields(val); len(f) > 0 {
						r.Rev = f[0]
					}
				}
			}
			if r.Rev != "" {
				if remote := vcsOutput(dir, "fossil", "remote"); remote != "off" {
					r.URL = stripURLCredentials(remote)
				}
				r.Branch = vcsOutput(dir, "fossil", "branch", "current")
				return r
			}
		}
	}
	if _, err := exec.LookPath("svn"); err == nil {
		// An unversioned folder inside a working copy is not one for `svn info`:
		// look at its parents too.
		for d, i := dir, 0; i < 32; i++ {
			if wc := vcsOutput(d, "svn", "info", "--show-item", "wc-root"); wc != "" {
				u := vcsOutput(wc, "svn", "info", "--show-item", "url")
				r := &RepoInfo{VCS: "svn", URL: stripURLCredentials(u), Subdir: subdirOf(wc, dir)}
				r.Rev = vcsOutput(wc, "svn", "info", "--show-item", "revision")
				// In Subversion the branch is part of the URL.
				if root := vcsOutput(wc, "svn", "info", "--show-item", "repos-root-url"); root != "" {
					r.Branch = strings.TrimPrefix(strings.TrimPrefix(u, root), "/")
				}
				return r
			}
			parent := filepath.Dir(d)
			if parent == d {
				break
			}
			d = parent
		}
	}
	return nil
}

// stripURLCredentials removes a password or token from a remote URL
// (https://user:token@host/... → https://host/...).
func stripURLCredentials(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.User == nil {
		return raw
	}
	u.User = nil
	return u.String()
}

// normalizeRepoURL lets different spellings of one remote compare equal:
// git@github.com:u/r.git, ssh://git@github.com/u/r and https://github.com/u/r/.
func normalizeRepoURL(raw string) string {
	s := strings.TrimSpace(raw)
	if u, err := url.Parse(s); err == nil && u.Scheme != "" && u.Host != "" {
		s = strings.ToLower(u.Hostname()) + "/" + strings.Trim(u.Path, "/")
	} else if at, rest, ok := strings.Cut(s, "@"); ok && !strings.Contains(at, "/") {
		// scp-like user@host:path
		if host, p, ok := strings.Cut(rest, ":"); ok {
			s = strings.ToLower(host) + "/" + strings.Trim(p, "/")
		}
	}
	s = strings.TrimSuffix(strings.TrimSuffix(s, "/"), ".git")
	return s
}

// repoFolderName is the folder a clone of u would get.
func repoFolderName(u string) string {
	base := path.Base(strings.TrimSuffix(strings.TrimSuffix(normalizeRepoURL(u), "/"), ".fossil"))
	if base == "" || base == "." || base == "/" {
		return "repo"
	}
	return base
}

func shortRev(rev string) string {
	if len(rev) > 12 {
		return rev[:12]
	}
	return rev
}

// unpushedRepoWarnings tells the sending side about commits the other computer
// cannot get (git only: Mercurial and Fossil need the network to know).
func unpushedRepoWarnings(dir string, r *RepoInfo) []string {
	if r == nil || r.VCS != "git" {
		return nil
	}
	if r.URL == "" {
		return []string{"The git repository has no remote: the other computer cannot get its commits."}
	}
	if r.Branch == "" {
		if !vcsOK(dir, "git", "branch", "-r", "--contains", "HEAD") || vcsOutput(dir, "git", "branch", "-r", "--contains", "HEAD") == "" {
			return []string{fmt.Sprintf("Commit %s is not on the server: push it, or the other computer cannot get it.", shortRev(r.Rev))}
		}
		return nil
	}
	if vcsOutput(dir, "git", "rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{upstream}") == "" {
		return []string{fmt.Sprintf("Branch `%s` is not on the server: `git push -u origin %s`.", r.Branch, r.Branch)}
	}
	if n, _ := strconv.Atoi(vcsOutput(dir, "git", "rev-list", "--count", "@{upstream}..HEAD")); n > 0 {
		return []string{fmt.Sprintf("%d commit(s) of branch `%s` are not on the server: `git push`.", n, r.Branch)}
	}
	return nil
}

// repoMismatchWarnings compares the checkout the session worked in (from a
// pushed bundle) with the one in dir and says how to get the same code.
func repoMismatchWarnings(dir string, want *RepoInfo) []string {
	if want == nil {
		return nil
	}
	have := detectRepo(dir)
	rev := shortRev(want.Rev)
	if have == nil || have.VCS != want.VCS {
		if want.URL == "" {
			return []string{fmt.Sprintf("The session worked in a %s repository without a remote (revision %s); this folder is not a checkout of it.", want.VCS, rev)}
		}
		folder := repoFolderName(want.URL)
		var get string
		switch want.VCS {
		case "git":
			get = fmt.Sprintf("git clone %s %s && cd %s && git checkout %s", want.URL, folder, folder, firstNonEmpty(want.Branch, want.Rev))
		case "hg":
			get = fmt.Sprintf("hg clone %s %s && cd %s && hg update %s", want.URL, folder, folder, want.Rev)
		case "fossil":
			get = fmt.Sprintf("mkdir %s && cd %s && fossil open %s %s", folder, folder, want.URL, firstNonEmpty(want.Branch, want.Rev))
		case "svn":
			get = fmt.Sprintf("svn checkout -r %s %s %s", want.Rev, want.URL, folder)
		}
		target := folder
		if want.Subdir != "" {
			target += "/" + want.Subdir
		}
		return []string{fmt.Sprintf("This folder is not a checkout of %s (%s, branch `%s`, revision %s). Get it: `%s`, then load the session in `%s`.",
			want.URL, want.VCS, want.Branch, rev, get, target)}
	}
	var out []string
	if want.URL != "" && have.URL != "" && normalizeRepoURL(want.URL) != normalizeRepoURL(have.URL) {
		return []string{fmt.Sprintf("This folder is a checkout of %s, but the session worked in %s.", have.URL, want.URL)}
	}
	if want.Subdir != have.Subdir {
		out = append(out, fmt.Sprintf("The session worked in `%s` of the repository, this folder is `%s`.", "/"+want.Subdir, "/"+have.Subdir))
	}
	switch want.VCS {
	case "git":
		if want.Branch != "" && want.Branch != have.Branch {
			if vcsOK(dir, "git", "rev-parse", "--verify", "--quiet", "refs/heads/"+want.Branch) ||
				vcsOK(dir, "git", "rev-parse", "--verify", "--quiet", "refs/remotes/origin/"+want.Branch) {
				out = append(out, fmt.Sprintf("The session worked on branch `%s`, this checkout is on `%s`: `git switch %s`.", want.Branch, firstNonEmpty(have.Branch, "a detached HEAD"), want.Branch))
			} else {
				out = append(out, fmt.Sprintf("Branch `%s` is not here: `git fetch`; if it is still missing, run `git push -u origin %s` on the source computer.", want.Branch, want.Branch))
			}
		}
		if want.Rev != "" && want.Rev != have.Rev {
			if !vcsOK(dir, "git", "cat-file", "-e", want.Rev+"^{commit}") {
				out = append(out, fmt.Sprintf("Revision %s is not here: `git fetch`; if it is still missing, run `git push` on the source computer.", rev))
			} else {
				out = append(out, fmt.Sprintf("This checkout is at %s, the session at %s: `git checkout %s` (or update the branch).", shortRev(have.Rev), rev, firstNonEmpty(want.Branch, rev)))
			}
		}
	default:
		if want.Branch != "" && want.Branch != have.Branch {
			out = append(out, fmt.Sprintf("The session worked on branch `%s`, this checkout is on `%s`.", want.Branch, have.Branch))
		}
		if want.Rev != "" && want.Rev != have.Rev {
			update := map[string]string{"hg": "hg pull && hg update ", "fossil": "fossil update ", "svn": "svn update -r "}[want.VCS]
			out = append(out, fmt.Sprintf("This checkout is at %s, the session at %s: `%s%s`.", shortRev(have.Rev), rev, update, want.Rev))
		}
	}
	return out
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
