// Package learn is learn.py: the deterministic edits to the review-loop learnings file. The
// judgment — novel or a re-match, which section — stays with the caller.
//
//	looper learn add   <file> "<entry text, no date>" --section dismissed|accepted
//	looper learn bump  <file> "<substring of the matched entry>"
//	looper learn prune <file> [--cap 50]
//
// Options may come anywhere among the arguments, as argparse allowed on 3.13+.
package learn

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/pinepeakdigital/looper/internal/py"
	"github.com/pinepeakdigital/looper/internal/record"
)

// ponytail: ASCII digits where re's \d is Unicode Nd. A date in another script's digits is
// not something the writer here produces; see README.
var entryRe = regexp.MustCompile(`^- (\d{4}-\d{2}-\d{2}): (.*)$`)

// Skeleton is the file add creates when there is none.
var Skeleton = []string{
	"# Review Loop Learnings", "",
	"## Dismissed",
	"<!-- Patterns the user has chosen to skip. Skill won't auto-flag matches. -->", "",
	"## Accepted patterns",
	"<!-- Patterns the user has explicitly confirmed matter here. Skill weights matches higher. -->",
}

// leadingDate is one ISO date opening the entry text, with the colon or spaces after it. Add
// stamps its own date, so a caller passing "2026-10-20: text" had the date written twice.
var leadingDate = regexp.MustCompile(`^[ \t]*\d{4}-\d{2}-\d{2}(?::[ \t]*|[ \t]+|$)`)

// minEntry is the fewest characters, after trimming, that add writes as an entry. The shortest
// entry in looper's own learnings file runs past a hundred, and the shortest realistic fixture
// text here ("a new pattern") is 13. Under 10 is a probe ("x", "test") or an empty argument,
// and a probe `x` was once written to a real file as a learning.
const minEntry = 10

// Today is the date stamped on an entry. A variable so tests can pin it.
var Today = func() string { return time.Now().Format("2006-01-02") }

// Durable reports whether an entry is a PATTERN, which prune never evicts. A DIVERGENCE:
// learn.py tests for "PATTERN:" only, while the entries it is meant to protect are written
// "PATTERN — …" (41 of 43 in looper's own file), so it counted none of them durable and would
// have evicted them first.
func Durable(text string) bool {
	return strings.Contains(text, "PATTERN:") || strings.Contains(text, "PATTERN —")
}

// Bump re-dates the first entry whose text contains match.
func Bump(lines []string, match, today string) bool {
	for i, ln := range lines {
		if m := entryRe.FindStringSubmatch(ln); m != nil && strings.Contains(m[2], match) {
			lines[i] = "- " + today + ": " + m[2]
			return true
		}
	}
	return false
}

// Add inserts a dated entry at the top of its section, after the section's comment lines,
// appending the section if it is missing.
func Add(lines []string, section, text, today string) []string {
	header := "## Accepted patterns"
	if section == "dismissed" {
		header = "## Dismissed"
	}
	entry := "- " + today + ": " + text
	for i, ln := range lines {
		if py.Strip(ln) == header {
			j := i + 1
			for j < len(lines) && strings.HasPrefix(py.Strip(lines[j]), "<!--") {
				j++
			}
			return append(lines[:j], append([]string{entry}, lines[j:]...)...)
		}
	}
	return append(lines, "", header, entry)
}

// Prune evicts entries down to limit: never a PATTERN, dismissed before accepted, oldest first.
func Prune(lines []string, limit int) (kept, dropped []string) {
	type entry struct {
		i         int
		date      string
		durable   bool
		dismissed bool
	}
	section := ""
	var es []entry
	for i, ln := range lines {
		if strings.HasPrefix(ln, "## ") {
			section = strings.ToLower(py.Strip(ln[3:]))
		}
		if m := entryRe.FindStringSubmatch(ln); m != nil {
			es = append(es, entry{i, m[1], Durable(m[2]), strings.Contains(section, "dismiss")})
		}
	}
	if len(es) <= limit {
		return lines, nil
	}
	var cand []entry
	for _, e := range es {
		if !e.durable {
			cand = append(cand, e)
		}
	}
	sort.SliceStable(cand, func(a, b int) bool {
		if cand[a].dismissed != cand[b].dismissed {
			return cand[a].dismissed
		}
		return cand[a].date < cand[b].date
	})
	n := len(es) - limit
	if n > len(cand) {
		n = len(cand)
	}
	drop := map[int]bool{}
	for _, e := range cand[:n] {
		drop[e.i] = true
	}
	for i, ln := range lines {
		if drop[i] {
			dropped = append(dropped, ln)
		} else {
			kept = append(kept, ln)
		}
	}
	return kept, dropped
}

// looksLikeOption is argparse's test (3.13+): a leading dash, unless the argument is "-",
// starts like a negative number, or contains a space — so an entry like "--no-verify was used"
// is text. Option matches come first, as there: an --option=value is an option whatever its
// value holds (an ambiguous one is an error, not text), and anything starting -h is help.
func looksLikeOption(a string) bool {
	if !strings.HasPrefix(a, "-") || a == "-" {
		return false
	}
	if shortHelp(a) {
		return true
	}
	if name, _, ok := strings.Cut(a, "="); ok {
		if _, err := expand(name); err == "" || strings.HasPrefix(err, "ambiguous") {
			return true
		}
	}
	return !strings.Contains(a, " ") && !negNum.MatchString(a)
}

// shortHelp is argparse reading -h followed by anything as -h: "-hx" and "-h x" print help.
func shortHelp(a string) bool { return strings.HasPrefix(a, "-h") }

var negNum = regexp.MustCompile(`^-\.?\p{Nd}`)

// expand resolves an abbreviation the way argparse's allow_abbrev does, over learn.py's own
// options so that an abbreviation ambiguous there is ambiguous here.
func expand(name string) (string, string) {
	if shortHelp(name) {
		return "--help", ""
	}
	var hits []string
	// A bare "-" or "--" before an `=` is a prefix of every option, as in argparse.
	for _, o := range []string{"-h", "--help", "--section", "--cap", "--selftest"} {
		if o == name {
			return o, ""
		}
		if strings.HasPrefix(o, name) {
			hits = append(hits, o)
		}
	}
	switch len(hits) {
	case 1:
		return hits[0], ""
	case 0:
		return "", "unrecognized arguments: " + name
	}
	return "", "ambiguous option: " + name + " could match " + strings.Join(hits, ", ")
}

const usage = `usage: looper learn {add,bump,prune} <file> [text] [--section dismissed|accepted] [--cap N]`

// Command runs learn.py's command line: 0 on success, 1 for a refusal, 2 for a usage error.
func Command(args []string, out, errOut io.Writer) int {
	usageErr := func(msg string) int {
		fmt.Fprintf(errOut, "%s\nlooper learn: error: %s\n", usage, msg)
		return 2
	}
	var pos []string
	section, capN := "", 50
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			pos = append(pos, args[i+1:]...)
			break
		}
		if !looksLikeOption(a) {
			pos = append(pos, a)
			continue
		}
		name, val, hasVal := strings.Cut(a, "=")
		long, err := expand(name)
		if err != "" {
			// argparse names the whole argument, value and all, in its ambiguity error.
			return usageErr(strings.Replace(err, name, a, 1))
		}
		switch long {
		case "--help":
			fmt.Fprintln(out, usage)
			return 0
		case "--selftest":
			return usageErr("--selftest is learn.py's; the Go tests replace it")
		}
		if !hasVal {
			if i+1 >= len(args) || looksLikeOption(args[i+1]) {
				return usageErr("argument " + long + ": expected one argument")
			}
			i++
			val = args[i]
		}
		if long == "--section" {
			if val != "dismissed" && val != "accepted" {
				return usageErr("argument --section: invalid choice: " + record.PyRepr(val) + " (choose from 'dismissed', 'accepted')")
			}
			section = val
		} else {
			n, err := strconv.Atoi(val)
			if err != nil {
				return usageErr("argument --cap: invalid int value: " + record.PyRepr(val))
			}
			capN = n
		}
	}
	if len(pos) > 3 {
		return usageErr("unrecognized arguments: " + strings.Join(pos[3:], " "))
	}
	if len(pos) < 2 || pos[0] == "" || pos[1] == "" {
		return usageErr("cmd and file are required")
	}
	cmd, file, text := pos[0], pos[1], ""
	if len(pos) == 3 {
		text = pos[2]
	}
	if cmd != "add" && cmd != "bump" && cmd != "prune" {
		return usageErr("argument cmd: invalid choice: " + record.PyRepr(cmd) + " (choose from 'add', 'bump', 'prune')")
	}
	refuse := func(msg string) int { fmt.Fprintln(errOut, msg); return 1 }
	if !utf8.ValidString(text) {
		// learn.py truncated the file and then died encoding the entry.
		return refuse("the entry text is not UTF-8 — nothing written")
	}

	var lines []string
	b, err := os.ReadFile(file)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if cmd != "add" {
			return refuse("no learnings file: " + file)
		}
		lines = append([]string(nil), Skeleton...)
	case err != nil:
		// learn.py dies with a traceback here; this names the cause.
		return refuse("can't read " + file + ": " + err.Error())
	case !utf8.Valid(b):
		// learn.py dies on the decode and writes nothing; py.Decode would substitute U+FFFD
		// and the write below would then rewrite every bad byte in the file.
		return refuse(file + " is not UTF-8 — left as it is")
	default:
		lines = py.Splitlines(py.Decode(b))
	}

	switch cmd {
	case "bump":
		// strings.Contains(x, "") is true, so an empty match re-dated the first entry in the
		// file, whichever it was. learn.py did the same; measured: `looper learn bump f ""`
		// re-dated the first entry and exited 0.
		if py.Strip(text) == "" {
			return refuse("the match text is empty: it would match the first entry — nothing written")
		}
		if !Bump(lines, text, Today()) {
			return refuse("no entry matched: " + record.PyRepr(text))
		}
	case "add":
		text = leadingDate.ReplaceAllString(text, "")
		if section == "dismissed" {
			if err := record.RejectBanned("this dismissal", text); err != nil {
				return refuse(err.Error())
			}
		}
		if section == "" {
			return usageErr("add requires --section")
		}
		if n := utf8.RuneCountInString(py.Strip(text)); n < minEntry {
			return refuse(fmt.Sprintf("the entry text is %d characters, under %d: too short to be a learning — nothing written", n, minEntry))
		}
		lines = Add(lines, section, text, Today())
	case "prune":
		var dropped []string
		lines, dropped = Prune(lines, capN)
		noun := "entries"
		if len(dropped) == 1 {
			noun = "entry"
		}
		fmt.Fprintf(errOut, "pruned %d %s\n", len(dropped), noun)
	}
	if err := os.WriteFile(file, []byte(strings.Join(lines, "\n")+"\n"), 0o666); err != nil {
		return refuse(fmt.Sprintf("can't write %s: %v", file, err))
	}
	return 0
}
