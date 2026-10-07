// Package docs checks that the prose describing a tool matches the tool.
//
// This is the cheapest half of a finding class that keeps recurring: SKILL.md
// documented a `--max-cycles` flag for a full day after it was deleted, and told
// agents to run steps in an order the scripts refuse. The order can't be checked
// mechanically. A flag that does not exist can.
package docs

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Reference is one flag a document attributes to one script.
type Reference struct {
	Doc    string // the markdown file
	Line   int    // 1-indexed
	Script string // base name, e.g. "runlog.py"
	Flag   string // e.g. "--agent-cap"
}

func (r Reference) String() string {
	return fmt.Sprintf("%s:%d: %s has no %s", r.Doc, r.Line, r.Script, r.Flag)
}

var (
	// Flags a script declares: any flag spelled as a string literal anywhere in its
	// source. Narrower rules were wrong in both directions — matching only
	// `add_argument(` missed upstream-check.py, which parses sys.argv by hand, and
	// reading `--help` output would make a script that cannot start look flagless.
	// A flag mentioned only in the script's own docstring counts, which is fine: the
	// failure being caught is a flag that exists in NO form, having been deleted.
	declared = regexp.MustCompile(`["'](--[a-z0-9][a-z0-9-]*)["']`)
	// Scripts and flags as prose mentions them.
	scriptRef = regexp.MustCompile(`([a-z0-9_-]+\.py)`)
	flagRef   = regexp.MustCompile(`(--[a-z0-9][a-z0-9-]*)`)
)

// helpFlag is argparse's own, never written with add_argument. A map modelled a
// plurality that does not exist: argparse auto-adds exactly one.
const helpFlag = "--help"

// Check reports each flag a document attributes to a script that does not declare it.
// It reads every .py file directly under dir, and every .md file both directly under dir
// and under dir/references/ — that subdirectory is where half a skill's procedure tends
// to live, so skipping it would let the check pass by not looking.
//
// A flag counts as attributed only when the same LINE also names the script. Prose
// spanning lines is skipped rather than guessed at: the point is a signal that is
// worth acting on every time it fires, not a complete one.
//
// Each flag is attributed to ONE script — the nearest one named on the line that this
// directory actually contains. It used to be attributed to every script on the line,
// which made the check fire on correct prose: a sentence naming two tools reported the
// first as missing the second's flag. That cost the promise one line above, because a
// signal that fires on correct prose is one readers learn to re-check rather than act
// on. The original rationale for line-scoped attribution — do not guess across lines —
// is untouched by this; it never argued for attributing one flag to every script.
//
// The cost, recorded here because the commit message is not where a later session will
// look: a line that states one flag ONCE for several scripts is now checked only against
// the nearest, so a flag that exists on one of them but not the others goes unreported.
// "Pass --run-id to both runlog.py and plan.py" and the table row
// "| --target | batch-files.py, runlog.py |" are the realistic shapes. A flag that exists
// on NO script in the directory — the failure this package was built for — is unaffected:
// a 300k-line differential run against the old cross product lost none of those.
func Check(dir string) ([]Reference, error) {
	flags, err := scriptFlags(dir)
	if err != nil {
		return nil, err
	}
	if len(flags) == 0 {
		return nil, fmt.Errorf("no .py files under %s, so nothing could be checked", dir)
	}

	docs, err := filepath.Glob(filepath.Join(dir, "*.md"))
	if err != nil {
		return nil, err
	}
	more, err := filepath.Glob(filepath.Join(dir, "references", "*.md"))
	if err != nil {
		return nil, err
	}
	docs = append(docs, more...)
	if len(docs) == 0 {
		return nil, fmt.Errorf("no .md files under %s, so nothing could be checked", dir)
	}
	sort.Strings(docs)

	var out []Reference
	for _, doc := range docs {
		body, err := os.ReadFile(doc)
		if err != nil {
			return nil, err
		}
		rel, _ := filepath.Rel(dir, doc)
		for i, line := range strings.Split(string(body), "\n") {
			named := scriptRef.FindAllStringSubmatchIndex(line, -1)
			if len(named) == 0 {
				continue
			}
			for _, flag := range flagRef.FindAllStringSubmatchIndex(line, -1) {
				name := line[flag[2]:flag[3]]
				if name == helpFlag {
					continue
				}
				script := nearestKnown(line, named, flag[2], flag[3], flags)
				if script == "" || flags[script][name] {
					continue
				}
				out = append(out, Reference{Doc: rel, Line: i + 1, Script: script, Flag: name})
			}
		}
	}
	return out, nil
}

// nearestKnown returns the script named closest to pos on the line, among those this
// directory contains. "" when the line names none of them — prose about some other
// repo's tool is not this check's business.
//
// Known-only, not simply nearest: a line mentioning `setup.py` beside `runlog.py` would
// otherwise attribute to the unknown one and silently skip the real check.
func nearestKnown(line string, named [][]int, pos, flagEnd int, flags map[string]map[string]bool) string {
	best, bestDist := "", -1
	for _, m := range named {
		start, end := m[2], m[3]
		if _, ok := flags[line[start:end]]; !ok {
			continue
		}
		// The GAP between the two spans, so "script.py --flag" and "--flag of script.py"
		// are measured the same way. Measuring a following script from the flag's START
		// instead silently added the flag's own length to its distance, biasing every
		// comparison toward preceding mentions by len(flag) — enough to decide real
		// cases, and it made a genuine tie almost unconstructible. Ties go to the
		// preceding mention, which is the overwhelmingly common prose order.
		dist := pos - end
		if dist < 0 {
			if dist = start - flagEnd; dist < 0 {
				dist = 0
			}
		}
		if bestDist < 0 || dist < bestDist {
			best, bestDist = line[start:end], dist
		}
	}
	return best
}

// scriptFlags maps each .py file's base name to the flags it declares.
func scriptFlags(dir string) (map[string]map[string]bool, error) {
	flags := map[string]map[string]bool{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".py") {
			continue
		}
		body, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		set := map[string]bool{}
		for _, m := range declared.FindAllStringSubmatch(string(body), -1) {
			set[m[1]] = true
		}
		flags[e.Name()] = set
	}
	return flags, nil
}
