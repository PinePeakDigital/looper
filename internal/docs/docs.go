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

// universal flags are argparse's own, never written with add_argument.
var universal = map[string]bool{"--help": true}

// Check reads every .py and .md file directly under dir and reports each flag a
// document attributes to a script that does not declare it.
//
// A flag counts as attributed only when the same LINE also names the script. Prose
// spanning lines is skipped rather than guessed at: the point is a signal that is
// worth acting on every time it fires, not a complete one.
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
			named := scriptRef.FindAllStringSubmatch(line, -1)
			if len(named) == 0 {
				continue
			}
			for _, flag := range flagRef.FindAllStringSubmatch(line, -1) {
				if universal[flag[1]] {
					continue
				}
				for _, script := range named {
					have, known := flags[script[1]]
					if !known || have[flag[1]] {
						continue
					}
					out = append(out, Reference{Doc: rel, Line: i + 1, Script: script[1], Flag: flag[1]})
				}
			}
		}
	}
	return out, nil
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
