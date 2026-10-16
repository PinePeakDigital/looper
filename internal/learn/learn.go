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

// Prune evicts entries down to cap: never a PATTERN, dismissed before accepted, oldest first.
func Prune(lines []string, cap int) (kept, dropped []string) {
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
	if len(es) <= cap {
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
	n := len(es) - cap
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
		name, val, hasVal := strings.Cut(a, "=")
		switch name {
		case "--section", "--cap":
			if !hasVal {
				if i+1 >= len(args) {
					return usageErr("argument " + name + ": expected one argument")
				}
				i++
				val = args[i]
			}
			if name == "--section" {
				if val != "dismissed" && val != "accepted" {
					return usageErr("argument --section: invalid choice: " + record.PyRepr(val))
				}
				section = val
			} else {
				n, err := strconv.Atoi(val)
				if err != nil {
					return usageErr("argument --cap: invalid int value: " + record.PyRepr(val))
				}
				capN = n
			}
		case "-h", "--help":
			fmt.Fprintln(out, usage)
			return 0
		default:
			if strings.HasPrefix(a, "-") && a != "-" {
				return usageErr("unrecognized arguments: " + a)
			}
			pos = append(pos, a)
		}
	}
	if len(pos) > 3 {
		return usageErr("unrecognized arguments: " + strings.Join(pos[3:], " "))
	}
	if len(pos) < 2 {
		return usageErr("cmd and file are required")
	}
	cmd, file, text := pos[0], pos[1], ""
	if len(pos) == 3 {
		text = pos[2]
	}
	if cmd != "add" && cmd != "bump" && cmd != "prune" {
		return usageErr("argument cmd: invalid choice: " + record.PyRepr(cmd) + " (choose from add, bump, prune)")
	}
	refuse := func(msg string) int { fmt.Fprintln(errOut, msg); return 1 }

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
		if !Bump(lines, text, Today()) {
			return refuse("no entry matched: " + record.PyRepr(text))
		}
	case "add":
		if section == "dismissed" {
			if err := record.RejectBanned("this dismissal", text); err != nil {
				return refuse(err.Error())
			}
		}
		if section == "" {
			return usageErr("add requires --section")
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
	if err := os.WriteFile(file, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		return refuse(fmt.Sprintf("can't write %s: %v", file, err))
	}
	return 0
}
