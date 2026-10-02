package mutate

import (
	"fmt"
	"os/exec"
	"regexp"
	"sort"
	"strings"
)

// A verify command has to be shown to PASS on the unmutated code before its failure on
// the mutated code means anything. Without that, a command already failing for an
// unrelated reason — a flaky test, a broken environment, a regression someone else
// introduced — fails identically with the mutation applied, and every mutation it guards
// scores as caught while the suite has proven nothing.
//
// Commands are deduplicated: a catalog of 29 entries here resolves to about 8 distinct
// commands, so this costs far less than one baseline run per mutation.
func (r *Runner) baseline(muts []Mutation) (map[string]string, error) {
	seen := map[string]bool{}
	var commands []string
	for _, m := range muts {
		for _, c := range m.Verify {
			if !seen[c] {
				seen[c] = true
				commands = append(commands, c)
			}
		}
	}
	sort.Strings(commands)

	bad := map[string]string{}
	r.logf("baseline: %d distinct verify command(s) on unmutated code\n", len(commands))
	for _, command := range commands {
		if why := r.vacuous(command); why != "" {
			bad[command] = why
			r.logf("  VACUOUS %s\n    %s\n", command, why)
			continue
		}
		c := r.verify(command)
		switch {
		case !c.ran:
			bad[command] = c.detail
		case c.failed:
			bad[command] = fmt.Sprintf("already failing before any mutation was applied: %s", c.detail)
		default:
			continue
		}
		r.logf("  ALREADY RED %s\n", command)
	}
	if len(bad) > 0 {
		r.logf("\n")
	}
	return bad, nil
}

// runPattern pulls the -run argument out of a `go test` command line. Deliberately
// narrow: this is the one place the tool knows anything about a specific test runner,
// and it is here because `go test -run` with a pattern that matches NOTHING exits 0.
// A catalog entry naming a test that has since been renamed therefore reads as a hole in
// the suite rather than as a stale entry — the failure is loud, but it is the wrong story.
var runPattern = regexp.MustCompile(`(?:^|\s)go\s+test\s+(.*?)-run\s+'([^']+)'`)

// vacuous reports why a verify command could not possibly catch anything, or "" if it
// could. Only `go test -run` is recognised; anything else is assumed to be fine, because
// guessing at an unknown runner would reject working catalogs.
func (r *Runner) vacuous(command string) string {
	m := runPattern.FindStringSubmatch(command)
	if m == nil {
		return ""
	}
	pkg := ""
	for _, f := range strings.Fields(m[1]) {
		if strings.HasPrefix(f, "./") || strings.HasPrefix(f, "github.com/") {
			pkg = f
			break
		}
	}
	if pkg == "" {
		return ""
	}
	// -list only enumerates; it runs no tests, so this is cheap.
	out, err := exec.Command("go", "test", pkg, "-list", m[2]).Output()
	if err != nil {
		// Cannot tell — do not invent a failure. The baseline run still has to pass.
		return ""
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "Test") || strings.HasPrefix(line, "Example") ||
			strings.HasPrefix(line, "Benchmark") || strings.HasPrefix(line, "Fuzz") {
			return ""
		}
	}
	return fmt.Sprintf("-run %q matches no test in %s, and `go test` exits 0 when it "+
		"matches nothing — so this command can never catch anything", m[2], pkg)
}
