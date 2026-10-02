// Package mutate answers one question about a test suite: if the code it covers
// broke, would it say so?
//
// A green suite is not evidence. Reviewing the review-loop skill's own suites turned
// up twelve assertions that could not fail — greps matching a hard-coded table
// header, an inverted test satisfied by a crash printing nothing, a row count
// identical under both behaviours, a fixture whose setup was silently refused. All of
// them passed, continuously, for days.
//
// So each mutation here names a defect that actually shipped, the edit that
// reintroduces it, and the suites that must go red. A mutation that survives is a
// hole in the suite; a mutation whose anchor no longer matches is a hole in this
// catalog, and both are failures.
package mutate

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Mutation is one reintroduced defect, parsed from a .mut file.
type Mutation struct {
	Source string   // the .mut file, for error messages
	Target string   // file to edit, relative to the repo root
	Verify []string // commands that must FAIL once the edit is applied
	Why    string   // the defect this reproduces, in one line
	Old    string   // exact text to replace; must occur exactly once
	New    string   // replacement
	// Expect is a substring the failing verify output must contain for the failure to
	// count as the suite catching the defect. Optional, and the single most load-bearing
	// field in the format, because without it ANY non-zero exit reads as a catch:
	//
	//   - A mutation that merely fails to COMPILE makes the command fail, and scored as
	//     caught. Measured: two of this repo's own 19 entries did not compile, so their
	//     named tests had never been shown to catch anything, and the score said 19/19.
	//   - A `go test -run` pattern naming a test that no longer exists exits 0 ("no tests
	//     to run"), so a renamed test reads as a hole rather than a stale catalog entry.
	//   - A suite already red for an unrelated reason fails identically to one that
	//     noticed.
	//
	// With it, "caught" means the named assertion reported failure. `--- FAIL:` for Go,
	// whatever a given suite prints for anything else.
	Expect string
}

// Name identifies a mutation in output, by its catalog filename.
func (m Mutation) Name() string {
	return strings.TrimSuffix(filepath.Base(m.Source), ".mut")
}

const (
	oldMarker = "--- old"
	newMarker = "--- new"
)

// ParseCatalog reads every .mut file under dir, sorted by path so runs are
// reproducible. An unreadable or malformed file is an error, never a skip: a
// catalog that silently shrinks reports a better score than it earned.
func ParseCatalog(dir string) ([]Mutation, error) {
	var found []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(path, ".mut") {
			found = append(found, path)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("reading catalog %s: %w", dir, err)
	}
	if len(found) == 0 {
		return nil, fmt.Errorf("no .mut files under %s", dir)
	}
	muts := make([]Mutation, 0, len(found))
	for _, path := range found {
		m, err := parseFile(path)
		if err != nil {
			return nil, err
		}
		muts = append(muts, m)
	}
	return muts, nil
}

// parseFile reads one .mut file:
//
//	target: path/to/file.py
//	verify: ./its.test.sh          (repeatable)
//	why:    one line on the defect
//	expect: --- FAIL: TestThing    (optional; see Mutation.Expect)
//	--- old
//	<exact text>
//	--- new
//	<replacement>
//
// Old and New are taken verbatim between the markers, minus the single newline
// that ends each block, so a mutation can span lines and carry its own
// indentation — which matters because leading whitespace is syntax in some of the
// languages a catalog can target, Python and shell among them, and an anchor that
// lost its indentation would match nothing.
func parseFile(path string) (Mutation, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Mutation{}, fmt.Errorf("reading %s: %w", path, err)
	}
	text := string(raw)
	m := Mutation{Source: path}

	oldAt := strings.Index(text, oldMarker+"\n")
	newAt := strings.Index(text, newMarker+"\n")
	switch {
	case oldAt < 0:
		return m, fmt.Errorf("%s: no %q section", path, oldMarker)
	case newAt < 0:
		return m, fmt.Errorf("%s: no %q section", path, newMarker)
	case newAt < oldAt:
		return m, fmt.Errorf("%s: %q must come before %q", path, oldMarker, newMarker)
	}

	for _, line := range strings.Split(text[:oldAt], "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			return m, fmt.Errorf("%s: header line is not `key: value`: %q", path, line)
		}
		val = strings.TrimSpace(val)
		switch strings.TrimSpace(key) {
		case "target":
			m.Target = val
		case "verify":
			m.Verify = append(m.Verify, val)
		case "why":
			m.Why = val
		case "expect":
			m.Expect = val
		default:
			return m, fmt.Errorf("%s: unknown header %q", path, key)
		}
	}

	m.Old = trimBlock(text[oldAt+len(oldMarker)+1 : newAt])
	m.New = trimBlock(text[newAt+len(newMarker)+1:])

	switch {
	case m.Target == "":
		return m, fmt.Errorf("%s: no `target:`", path)
	case len(m.Verify) == 0:
		// Without this, a mutation would be scored against nothing and always
		// read as surviving — or worse, as caught by an unrelated suite.
		return m, fmt.Errorf("%s: no `verify:` command, so nothing could catch it", path)
	case m.Why == "":
		// The defect being reproduced is the only thing that makes a surviving
		// mutation actionable later, and it is what a port has to carry over.
		return m, fmt.Errorf("%s: no `why:`, so a survivor would not say what it means", path)
	case m.Old == "":
		return m, fmt.Errorf("%s: empty `%s` block", path, oldMarker)
	case m.Old == m.New:
		return m, fmt.Errorf("%s: old and new are identical, so it mutates nothing", path)
	}
	return m, nil
}

// trimBlock drops the one trailing newline that separates a block from what
// follows, while leaving every other byte — including indentation and any blank
// lines inside the block — alone.
func trimBlock(s string) string {
	return strings.TrimSuffix(s, "\n")
}
