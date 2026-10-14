// Package ledger is the review-loop's two pinned ledgers — the threat model (Step 2b) and the
// deferred findings (Step 2a) — ported from threat-model.py, deferred.py and the pins.py they
// share.
//
// Both make the same kind of statement: something true about the code at a commit, which
// expires when a cited file moves. So staleness is a `git log` away rather than a judgment
// call, and the reader gets an exact worklist. One definition of the citation shape and the
// staleness rule serves both, as pins.py did, because two copies would drift.
package ledger

import (
	"bytes"
	"fmt"
	"os/exec"
	"regexp"
	"unicode/utf8"
)

// citation is pins.CITATION: [path:line @ sha], [path:start-end @ sha] or [path @ sha], with
// \s and \d widened to what they match in a Python str pattern.
var citation = regexp.MustCompile(`\[([^\]` + spaceClass + `:]+)(?::([` + digitClass + `]+(?:-[` +
	digitClass + `]+)?))?[` + spaceClass + `]*@[` + spaceClass + `]*([0-9a-f]{7,40})\]`)

// Pin is one citation. Lines is nil for a whole-file pin. Commits is set only on a stale pin:
// the `git log --oneline` lines that moved its file since Sha.
type Pin struct {
	Line    int
	Path    string
	Lines   *string
	Sha     string
	Commits []string
}

func (p Pin) value() value {
	o := obj{{"line", p.Line}, {"path", p.Path}, {"lines", p.Lines}, {"sha", p.Sha}}
	if p.Commits != nil {
		cs := make([]value, len(p.Commits))
		for i, c := range p.Commits {
			cs[i] = c
		}
		o = append(o, field{"commits", cs})
	}
	return o
}

func pinList(ps []Pin) []value {
	out := make([]value, len(ps))
	for i, p := range ps {
		out[i] = p.value()
	}
	return out
}

// git runs git in the working directory and returns its stdout, stripped, or "" when it
// fails or cannot start — pins.git, which callers rely on: "" is both "no output" and "no
// answer", and the staleness rule below is built on that.
func git(args ...string) string {
	return strip(decode(gitRaw(args...)))
}

// gitRaw is git's stdout as bytes, or nil when it fails or cannot start.
func gitRaw(args ...string) []byte {
	cmd := exec.Command("git", args...)
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return nil
	}
	return out.Bytes()
}

// infoPath is `<git-common-dir>/info/<name>`, or "" outside a repo. --git-common-dir, never
// --git-dir: in a linked worktree the latter has no info/, so the ledger would be written
// where the next run cannot find it. A common dir that is not UTF-8 is an error: decoded, it
// names no real file, so an existing ledger would read as missing. The Python crashed on it.
func infoPath(name string) (string, error) {
	raw := gitRaw("rev-parse", "--git-common-dir")
	if !utf8.Valid(raw) {
		return "", fmt.Errorf("git's common dir %q is not UTF-8, so the ledger under it cannot "+
			"be named", bytes.TrimRight(raw, "\n"))
	}
	common := strip(decode(raw))
	if common == "" {
		return "", nil
	}
	return common + "/info/" + name, nil
}

// Citations returns the pins in text, one per line at most (the first on it), and, when
// marker is not "", how many lines carry the marker but no pin. An unpinned claim is the
// dangerous one: nothing can ever mark it stale.
func Citations(text, marker string) ([]Pin, int) {
	var pinned []Pin
	unpinned := 0
	for i, raw := range splitlines(text) {
		m := citation.FindStringSubmatchIndex(raw)
		if m != nil {
			p := Pin{Line: i + 1, Path: raw[m[2]:m[3]], Sha: raw[m[6]:m[7]]}
			if m[4] >= 0 {
				l := raw[m[4]:m[5]]
				p.Lines = &l
			}
			pinned = append(pinned, p)
		} else if marker != "" && contains(raw, marker) {
			unpinned++
		}
	}
	return pinned, unpinned
}

// Stale returns the pins whose cited file moved since the pin, each with the commits that
// moved it. A pin whose sha does not resolve falls out here on the failed `git log`, which
// reads exactly like "nothing touched it" — so it looks checked and is unwatched. Broken is
// what surfaces those, and every caller reports it.
func Stale(pins []Pin) []Pin {
	out := []Pin{}
	for _, p := range pins {
		touched := git("log", "--oneline", p.Sha+"..HEAD", "--", p.Path)
		if touched != "" {
			p.Commits = splitlines(touched)
			out = append(out, p)
		}
	}
	return out
}

// Broken returns the pins whose sha names no commit here, so staleness can never be computed.
func Broken(pins []Pin) []Pin {
	out := []Pin{}
	for _, p := range pins {
		if exec.Command("git", "cat-file", "-e", p.Sha+"^{commit}").Run() != nil {
			out = append(out, p)
		}
	}
	return out
}
