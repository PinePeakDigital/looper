package ledger

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"syscall"

	"github.com/pinepeakdigital/looper/internal/home"
)

// The two ledgers' file names in the repo's store.
const (
	ThreatModelFile = home.ThreatModel
	DeferredFile    = home.Deferred
)

func contains(s, sub string) bool { return strings.Contains(s, sub) }

// read returns the ledger's text, or ok=false when there is none. Only a file that does not
// exist is "none": the Python read every OSError — a directory, a permission refusal — as
// exists:false, which for the deferred list says nothing is deferred and for the threat model
// sends a bootstrap to overwrite a file it could not read. Here those are an error.
func read(path string) (text string, ok bool, err error) {
	if path == "" {
		return "", false, nil
	}
	b, err := os.ReadFile(path)
	// ENOTDIR is a path component being a file (a $REVIEW_LOOP_HOME that is a file): no ledger can
	// be under it, so there is none, as the Python said.
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return decode(b), true, nil
}

// selftest refuses the Python's --selftest rather than reporting as if it were not there.
// The Python's checks are this package's go tests; printing a report would answer a question
// nobody asked, and printing "ok" would claim a check that did not run.
func selftest(cmd string, args []string, errOut io.Writer) bool {
	if len(args) > 0 && args[0] == "--selftest" {
		fmt.Fprintf(errOut, "looper %s: --selftest was the Python's own check; looper's is "+
			"`go test ./internal/ledger/`\n", cmd)
		return true
	}
	return false
}

func emit(out io.Writer, v value) int {
	if _, err := io.WriteString(out, encode(v)); err != nil {
		return 1
	}
	return 0
}

// nulPath refuses a pin whose path holds a NUL byte. No file can be named that, and git cannot
// be asked about it: the exec fails to start, which git() reads as "nothing moved", so the pin
// would look fresh forever. The Python died on it with a ValueError; this says which pin.
func nulPath(cmd string, pins []Pin, errOut io.Writer) bool {
	for _, p := range pins {
		if strings.ContainsRune(p.Path, 0) {
			fmt.Fprintf(errOut, "looper %s: the pin on line %d names a path with a NUL byte, "+
				"which git cannot be asked about — fix the pin\n", cmd, p.Line)
			return true
		}
	}
	return false
}

func unreadable(cmd, path string, err error, errOut io.Writer) int {
	fmt.Fprintf(errOut, "looper %s: cannot read %s: %v — it exists, so it is not reported "+
		"as missing\n", cmd, path, err)
	return 1
}

// ThreatModel is `looper threat-model`: threat-model.py's report on the repo's store
// threat-model.md (home.Ledger), from the working directory. Every OBSERVED
// claim owes a pin; `uncited` counts those without one. Arguments are ignored, as the Python
// ignored them, except --selftest. Exit 0 for a report, whatever it says.
func ThreatModel(args []string, out, errOut io.Writer) int {
	if selftest("threat-model", args, errOut) {
		return 2
	}
	path, err := ledgerPath(ThreatModelFile)
	if err != nil {
		fmt.Fprintf(errOut, "looper threat-model: %v\n", err)
		return 1
	}
	text, ok, err := read(path)
	if err != nil {
		return unreadable("threat-model", path, err, errOut)
	}
	if !ok {
		return emit(out, obj{{"path", path}, {"exists", false}, {"claims", 0},
			{"stale", []value{}}, {"broken_pins", []value{}}, {"uncited", 0}})
	}
	pins, bad := Citations(text)
	if nulPath("threat-model", pins, errOut) {
		return 1
	}
	claims, uncited := Claims(text, "OBSERVED:")
	return emit(out, obj{
		{"path", path},
		{"exists", true},
		// OBSERVED claims, not pins: pins.py reported how many lines held a pin here.
		{"claims", claims},
		// Every pin in the model is watched, the claims' and the rest's — an INFERRED note, a
		// Watch this spot or Not an issue here entry — as pins.py watched them.
		{"stale", pinList(Stale(pins))},
		// A claim pinned to an unresolvable sha returns nothing from Stale, so Step 2b's "skip
		// the agent when stale is empty" would drop it — while it still looks pinned and stays
		// out of uncited. For a `Not an issue here` dismissal that is a suppression nothing
		// ever re-examines. A malformed pin is the same hazard, and is listed with them.
		{"broken_pins", brokenList(pins, bad)},
		{"uncited", uncited},
	})
}

// entry is one DEFERRED finding, read as a block: its pin lives on the Grounding line, not
// on the DEFERRED line.
type entry struct {
	line int
	text string
	pins []Pin
	bad  []Malformed
}

// first is the entry as the report names it: its line and the first line of its text.
func (e entry) first() value {
	return obj{{"line", e.line}, {"text", splitlines(e.text)[0]}}
}

// entries is deferred.entries. A block is the DEFERRED line plus every following line that is
// indented and not blank; a blank or unindented line is skipped, not a terminator, so a later
// indented line still joins the entry. Pin line numbers are the block-relative index plus the
// entry's line, so a skipped line in between shifts them — what the Python reports.
func entries(text string) []entry {
	type block struct {
		line  int
		lines []string
	}
	var bs []block
	for i, raw := range splitlines(text) {
		if contains(raw, "DEFERRED") {
			bs = append(bs, block{line: i + 1, lines: []string{raw}})
		} else if len(bs) > 0 && strip(raw) != "" && startsWithSpace(raw) {
			bs[len(bs)-1].lines = append(bs[len(bs)-1].lines, raw)
		}
	}
	out := make([]entry, len(bs))
	for i, b := range bs {
		joined := strings.Join(b.lines, "\n")
		pins, bad := Citations(joined)
		for j := range pins {
			pins[j].Line += b.line - 1
		}
		for j := range bad {
			bad[j].Line += b.line - 1
		}
		out[i] = entry{line: b.line, text: strip(joined), pins: pins, bad: bad}
	}
	return out
}

func startsWithSpace(s string) bool {
	for _, r := range s {
		return isSpace(r)
	}
	return false
}

// Deferred is `looper deferred`: deferred.py's report on the repo's store deferred.md
// (home.Ledger), from the working directory. Arguments are
// ignored, as the Python ignored them, except --selftest. Exit 0 for a report.
func Deferred(args []string, out, errOut io.Writer) int {
	if selftest("deferred", args, errOut) {
		return 2
	}
	path, err := ledgerPath(DeferredFile)
	if err != nil {
		fmt.Fprintf(errOut, "looper deferred: %v\n", err)
		return 1
	}
	text, ok, err := read(path)
	if err != nil {
		return unreadable("deferred", path, err, errOut)
	}
	if !ok {
		// No broken_pins or unguarded here: the Python's missing-file report had neither key.
		return emit(out, obj{{"path", path}, {"exists", false}, {"entries", 0},
			{"stale", []value{}}, {"unpinned", []value{}}})
	}
	es := entries(text)
	var all []Pin
	var bad []Malformed
	unpinned, unguarded := []value{}, []value{}
	for _, e := range es {
		all = append(all, e.pins...)
		bad = append(bad, e.bad...)
		if len(e.pins) == 0 {
			// Named, not counted: an entry with no pin can never go stale, and the fix is to pin it.
			unpinned = append(unpinned, e.first())
		}
		// "Guard: none, because ..." is an answer; no Guard at all means nobody asked.
		if !contains(e.text, "Guard") {
			unguarded = append(unguarded, e.first())
		}
	}
	if nulPath("deferred", all, errOut) {
		return 1
	}
	return emit(out, obj{
		{"path", path},
		{"exists", true},
		{"entries", len(es)},
		{"stale", pinList(Stale(all))},
		{"unpinned", unpinned},
		{"broken_pins", brokenList(all, bad)},
		{"unguarded", unguarded},
		{"merged", merged(es)},
	})
}

// resolvedRe is the mark a run leaves on an entry it fixed: `RESOLVED @ <sha>` opening a line
// (after indentation or a bullet). Anchored, so a Guard or note that merely quotes the marker
// does not count: a match there would drop a deferral that was never resolved.
var resolvedRe = regexp.MustCompile(`(?m)^[ \t]*(?:- )?RESOLVED @ ([0-9a-fA-F]{7,40})\b`)

// merged names the entries marked `RESOLVED @ <sha>` whose sha is on the default branch
// (origin/HEAD), which the skill then deletes. An entry resolved only on an unmerged branch
// stays, so a dropped branch does not take its deferral with it: the ledger is one per repo,
// not per branch (narthur/skills#16). nil (JSON null) when origin/HEAD is unknown, which is not
// the same answer as "none merged".
// ponytail: ancestry, so a squash merge never counts as merged; match on the PR's merge state if
// squash merges start leaving RESOLVED entries behind.
func merged(es []entry) value {
	def := strip(decode(gitRaw("rev-parse", "--abbrev-ref", "origin/HEAD")))
	if def == "" || def == "origin/HEAD" {
		return nil
	}
	out := []value{}
	for _, e := range es {
		m := resolvedRe.FindStringSubmatch(e.text)
		if m != nil && exec.Command("git", "merge-base", "--is-ancestor", m[1], def).Run() == nil {
			out = append(out, e.first())
		}
	}
	return out
}
