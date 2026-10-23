// Package home is where review-loop keeps state that is not the run record: one store per
// repo for the ledgers, and one dir per run for its inputs and its pending report.
//
// Both live under ~/.claude/review-loop, not under the repo's .git: Claude Code's worktree
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
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

func git(dir string, args ...string) string {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// commonDir is dir's absolute git common dir, or "" outside a repo. --git-common-dir, never
// --git-dir: a linked worktree's git dir is per worktree, so it would name another repo.
func commonDir(dir string) string {
	return git(dir, "rev-parse", "--path-format=absolute", "--git-common-dir")
}

// RepoID is a repo's identity across worktrees: its common dir's parent ("" means the working
// directory). Outside a repo it is the directory itself. The run record's `repo` is this value.
func RepoID(dir string) string {
	common := commonDir(dir)
	if common == "" {
		if dir == "" {
			dir, _ = os.Getwd()
		}
		abs, _ := filepath.Abs(dir)
		return abs
	}
	if filepath.Base(common) == ".git" {
		return filepath.Dir(common)
	}
	return common
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
	id := RepoID(dir)
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
	if common := commonDir(dir); common != "" {
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
// valid says which ids can name a dir; a name it refuses is left where it is.
func MigratePending(dir string, valid func(string) bool) error {
	common := commonDir(dir)
	if common == "" {
		return nil
	}
	olds, _ := filepath.Glob(filepath.Join(common, "info", legacyPfx+"pending-report.*.md"))
	for _, old := range olds {
		id := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(old), legacyPfx+"pending-report."), ".md")
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
			return err
		}
	}
	return nil
}

// move copies a regular file old to new (creating new's dir) and then tries to remove old. A
// missing or non-regular old is nothing to move.
func move(old, new string) error {
	fi, err := os.Lstat(old)
	if err != nil || !fi.Mode().IsRegular() {
		return nil
	}
	b, err := os.ReadFile(old)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(new), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(new, b, 0o644); err != nil {
		return err
	}
	_ = os.Remove(old) // refused in a worktree-isolated session; the copy already stands
	return nil
}
