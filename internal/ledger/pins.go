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
	"os/exec"
	"regexp"
	"sort"
	"unicode/utf8"

	"github.com/pinepeakdigital/looper/internal/home"
)

// citation is pins.CITATION — [path:line @ sha], [path:start-end @ sha] or [path @ sha] — with
// \s and \d widened to what they match in a Python str pattern, and one divergence: the line
// part may be a comma-separated list, [path:206,225-231 @ sha]. pins.py accepted one line or
// one range, so a list was no citation at all, and the claim it pinned looked pinned while
// nothing watched it.
var citation = regexp.MustCompile(`\[([^\]` + spaceClass + `:]+)(?::(` + lineSpec + `(?:,` + lineSpec +
	`)*))?[` + spaceClass + `]*@[` + spaceClass + `]*([0-9a-f]{7,40})\]`)

// lineSpec is one line or one range of a citation.
const lineSpec = `[` + digitClass + `]+(?:-[` + digitClass + `]+)?`

// pinShape is text a writer meant as a pin: brackets around something, an @, and a hex-looking
// token. Where it is not also a citation it is malformed — a stray space, an upper-case or
// six-character sha, a range written 1- — and is reported in broken_pins rather than skipped,
// because skipped it reads exactly like a pin that is fine. The token must look like hex so a
// placeholder such as [file:line @ sha] in prose is not reported. Hex alone is not enough:
// [price @ 100] and [note: see @ 100] are prose, so the left side must look like a path (a `.`
// or `/`; a colon alone is prose punctuation) or the token must be six or more hex digits.
// [user.name@fade] still reads as a pin shape: the cost of catching [x.go:1 @ abc12].
var pinShape = regexp.MustCompile(`\[(?:[^\[\]@]*[./][^\[\]@]*@[` + spaceClass + `]*[0-9A-Fa-f]+|[^\[\]@]+@[` +
	spaceClass + `]*[0-9A-Fa-f]{6,})[` + spaceClass + `]*\]`)

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

// ledgerPath is the ledger's path in the repo's store (home.Ledger), or "" outside a repo. The
// store is outside .git so a worktree-isolated builder can write it (narthur/skills#16); a
// ledger still at its old place under <git-common-dir>/info is moved in on first use.
func ledgerPath(name string) (string, error) {
	if strip(decode(gitRaw("rev-parse", "--git-common-dir"))) == "" {
		return "", nil
	}
	return home.Ledger("", name)
}

// Malformed is pin-shaped text that is not a citation: on Line, as written.
type Malformed struct {
	Line int
	Text string
}

func (m Malformed) value() value {
	return obj{{"line", m.Line}, {"malformed", m.Text}}
}

// Citations returns every pin in text and every malformed pin, each numbered by its line.
// Every pin on a line counts: pins.py took the first, so a second pin on the same line was
// never checked for staleness or a broken sha.
func Citations(text string) ([]Pin, []Malformed) {
	var pins []Pin
	var bad []Malformed
	for i, raw := range splitlines(text) {
		ms := citation.FindAllStringSubmatchIndex(raw, -1)
		for _, m := range ms {
			p := Pin{Line: i + 1, Path: raw[m[2]:m[3]], Sha: raw[m[6]:m[7]]}
			if m[4] >= 0 {
				l := raw[m[4]:m[5]]
				p.Lines = &l
			}
			pins = append(pins, p)
		}
		for _, s := range pinShape.FindAllStringIndex(raw, -1) {
			if !overlaps(s, ms) {
				bad = append(bad, Malformed{Line: i + 1, Text: raw[s[0]:s[1]]})
			}
		}
	}
	return pins, bad
}

// overlaps says whether span s shares any byte with one of the citation matches ms. A
// pin-shaped span inside or around a real citation — [[src/a.ts @ abc1234], say — is that
// citation, not a second, broken one.
func overlaps(s []int, ms [][]int) bool {
	for _, m := range ms {
		if s[0] < m[1] && m[0] < s[1] {
			return true
		}
	}
	return false
}

// Claims counts the claims in text — each line holding marker opens one — and how many of
// them carry no pin. A claim is its marker line plus the continuation lines after it: indented,
// not blank, and not a bullet of their own. So a pin wrapped onto a later line of the claim
// pins it, where pins.py read each line alone and counted such a claim as uncited, and a pin on
// the next bullet, an unindented footer or past a blank line does not. An uncited claim is the
// dangerous one: nothing can ever mark it stale.
func Claims(text, marker string) (claims, uncited int) {
	open, pinned := false, false
	closeClaim := func() {
		if open && !pinned {
			uncited++
		}
		open, pinned = false, false
	}
	for _, raw := range splitlines(text) {
		switch {
		case contains(raw, marker):
			closeClaim()
			claims++
			open = true
		case open && continues(raw):
		default:
			closeClaim()
			continue
		}
		if citation.MatchString(raw) {
			pinned = true
		}
	}
	closeClaim()
	return claims, uncited
}

// continues says whether raw continues the claim above it: indented, not blank, and not a
// bullet — `-`, `*` or `+`, or a number with `.` or `)` — which starts a claim or note of its own.
func continues(raw string) bool {
	s := strip(raw)
	return s != "" && startsWithSpace(raw) && !isBullet(s)
}

func isBullet(s string) bool {
	if s == "" {
		return false
	}
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	switch {
	case i == 0 && (s[0] == '-' || s[0] == '*' || s[0] == '+'):
		i = 1
	case i > 0 && i < len(s) && (s[i] == '.' || s[i] == ')'):
		i++
	default:
		return false
	}
	r, _ := utf8.DecodeRuneInString(s[i:])
	return i == len(s) || isSpace(r)
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

// brokenList is broken_pins: the pins Broken returns and every malformed pin, in line order.
// A malformed pin is broken for the same reason an unresolvable sha is — it looks pinned and
// nothing can ever mark it stale — so it is reported in the same list, as {line, malformed}.
func brokenList(pins []Pin, bad []Malformed) []value {
	type at struct {
		line int
		v    value
	}
	var all []at
	for _, p := range Broken(pins) {
		all = append(all, at{p.Line, p.value()})
	}
	for _, m := range bad {
		all = append(all, at{m.Line, m.value()})
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].line < all[j].line })
	out := make([]value, len(all))
	for i, a := range all {
		out[i] = a.v
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
