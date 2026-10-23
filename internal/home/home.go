// Package home is where review-loop keeps state that is not the run record: one store per
// repo for the ledgers, and one dir per run for its inputs and its pending report.
//
// Both live under $REVIEW_LOOP_HOME (default ~/.claude/review-loop), not under the repo's .git: Claude Code's worktree
// isolation refuses any path under .git, so a worktree-isolated builder could not write a
// ledger kept in <git-common-dir>/info and every such run ended owing the main session a
// hand-applied update (narthur/skills#16). One store per repo, not per branch: two branches
// that diverge on a ledger are reconciled by the skill's rules (pinned threat-model claims are
// re-verified; a deferral is resolved by sha), not by keeping two copies.
package home

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pinepeakdigital/looper/internal/run"
)

// Ledger file names in a repo's store, and the name each had under <git-common-dir>/info.
const (
	ThreatModel = "threat-model.md"
	Deferred    = "deferred.md"
	Learnings   = "learnings.md"
	legacyPfx   = "review-loop-"
)

// Base is $REVIEW_LOOP_HOME, else ~/.claude/review-loop. No HOME is an error rather than a
// relative path, which would put the store inside the working directory — the repo itself.
func Base() (string, error) {
	if p := os.Getenv("REVIEW_LOOP_HOME"); p != "" {
		return p, nil
	}
	h, err := os.UserHomeDir()
	if err != nil || h == "" {
		return "", errors.New("no home directory, so there is no review-loop store to use")
	}
	return filepath.Join(h, ".claude", "review-loop"), nil
}

// errGit is a git that timed out, or exited while a process it started (a hook, a credential
// helper) held its output open. It is not "outside a repo": falling back to the working
// directory would quietly name another store and split the ledgers.
var errGit = errors.New("git timed out or a process it started held its output open, so the repo is unknown")

// gitBudget is git's time limit; a var so a test can shorten it.
var gitBudget = 10 * time.Second

// git is git's trimmed stdout, or "" when it fails or is missing, which reads as "outside a
// repo". Bounded like every other looper git call: a git whose descendant holds stdout open (a
// hook, a credential helper) once hung `looper home` for as long as the descendant lived. A
// git that ran out of time or left its pipe undrained is errGit.
func git(dir string, args ...string) (string, error) {
	r := run.Run(dir, gitBudget, "git", args...)
	if r.Expired || !r.Drained {
		return "", errGit
	}
	if r.Error != nil || r.Code != 0 {
		return "", nil
	}
	return strings.TrimSpace(string(r.Out)), nil
}

// commonDir is dir's absolute git common dir, or "" outside a repo. --git-common-dir, never
// --git-dir: a linked worktree's git dir is per worktree, so it would name another repo.
func commonDir(dir string) (string, error) {
	return git(dir, "rev-parse", "--path-format=absolute", "--git-common-dir")
}

// RepoID is a repo's identity across worktrees: its common dir's parent ("" means the working
// directory). Outside a repo, or when git could not answer, it is the directory itself. The
// run record's `repo` is this value.
func RepoID(dir string) string {
	id, _ := repoID(dir)
	return id
}

func repoID(dir string) (string, error) {
	common, err := commonDir(dir)
	if common == "" {
		if dir == "" {
			dir, _ = os.Getwd()
		}
		abs, _ := filepath.Abs(dir)
		return abs, err
	}
	if filepath.Base(common) == ".git" {
		return filepath.Dir(common), nil
	}
	return common, nil
}

// RepoDir is the repo's store: <base>/repos/<name>-<hash of RepoID>. The name is for people;
// the hash keeps two repos with the same basename apart.
// ponytail: keyed by path, so moving or re-cloning a repo starts an empty store; key on the
// remote URL if that starts to bite.
func RepoDir(dir string) (string, error) {
	base, err := Base()
	if err != nil {
		return "", err
	}
	id, err := repoID(dir)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(id))
	return filepath.Join(base, "repos", name(filepath.Base(id))+"-"+hex.EncodeToString(sum[:4])), nil
}

// name is s with every byte but [A-Za-z0-9._-] replaced by _, so a repo path that is not
// UTF-8, or holds anything a filesystem refuses, still names a store; the hash keeps it unique.
func name(s string) string {
	b := []byte(s)
	for i, c := range b {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-') {
			b[i] = '_'
		}
	}
	return string(b)
}

// Ledger is the path of ledger name in the repo's store. A ledger that exists only at its old
// place, <git-common-dir>/info/review-loop-<name>, is copied in first, then removed there when
// that is allowed: a worktree-isolated session can read the old file but not delete it, and the
// copy is what matters. Once the new file exists the old one is never read again.
func Ledger(dir, name string) (string, error) {
	rd, err := RepoDir(dir)
	if err != nil {
		return "", err
	}
	p := filepath.Join(rd, name)
	if _, err := os.Lstat(p); err == nil {
		return p, nil
	}
	common, err := commonDir(dir)
	if err != nil {
		return "", err
	}
	if common != "" {
		old := filepath.Join(common, "info", legacyPfx+name)
		if err := move(old, p); err != nil {
			return "", err
		}
	}
	return p, nil
}

// RunDir is a run's dir, <base>/run/<id>. The caller has validated id.
func RunDir(id string) (string, error) {
	base, err := Base()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "run", id), nil
}

// MigratePending moves every pending report left at the old place,
// <git-common-dir>/info/review-loop-pending-report.<id>.md, into <run dir>/pending-report.md.
// valid says which ids can name a dir; a name it refuses is left where it is. A report that
// fails to move stays where it was and the rest are still tried; the error names every one.
func MigratePending(dir string, valid func(string) bool) error {
	common, err := commonDir(dir)
	if common == "" {
		return err
	}
	// Listed, not globbed: a common dir holding `[` or `*` is a pattern to Glob.
	des, err := os.ReadDir(filepath.Join(common, "info"))
	if err != nil {
		return nil
	}
	var errs []error
	for _, de := range des {
		pfx, n := legacyPfx+"pending-report.", de.Name()
		if !strings.HasPrefix(n, pfx) || !strings.HasSuffix(n, ".md") {
			continue
		}
		old := filepath.Join(common, "info", n)
		id := strings.TrimSuffix(strings.TrimPrefix(n, pfx), ".md")
		if !valid(id) {
			continue
		}
		rd, err := RunDir(id)
		if err != nil {
			return err
		}
		p := filepath.Join(rd, "pending-report.md")
		if _, err := os.Lstat(p); err == nil {
			continue
		}
		if err := move(old, p); err != nil {
			errs = append(errs, fmt.Errorf("%s left in place: %w", old, err))
		}
	}
	return errors.Join(errs...)
}

// move copies a regular file src to dst (creating dst's dir) and then tries to remove src. A
// missing or non-regular src is nothing to move. The copy is written to a temp file and renamed
// into place, so dst exists only once complete: callers treat an existing dst as migrated and
// never read src again, so a half-written dst would shadow the whole ledger for good.
func move(src, dst string) error {
	fi, err := os.Lstat(src)
	if err != nil || !fi.Mode().IsRegular() {
		return nil
	}
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(dst), ".migrate-*")
	if err != nil {
		return err
	}
	_, werr := f.Write(b)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Chmod(f.Name(), fi.Mode().Perm()) // CreateTemp makes 0600; keep the source's mode
	}
	if werr == nil {
		werr = os.Rename(f.Name(), dst)
	}
	if werr != nil {
		_ = os.Remove(f.Name())
		return werr
	}
	_ = os.Remove(src) // refused in a worktree-isolated session; the copy already stands
	return nil
}
