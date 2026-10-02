package mutate

import (
	"fmt"
	"sort"
	"strings"
)

// noTestsRun is what `go test` prints when its -run pattern matches nothing. It exits 0 in
// that case, so a catalog entry naming a test that has since been renamed would otherwise
// turn a stale entry into what looks like a hole in the suite.
//
// Checked against the baseline run's own output rather than by parsing the command line.
// An earlier version pulled the -run pattern out with a regex and cross-checked it with
// `go test -list`, and review found three defects in that approach: it forgot to set the
// subprocess's directory, so the check silently never fired whenever -root differed from
// the working directory; the regex missed `go test -run 'X' ./pkg/`, where the flag
// precedes the package; and it matched a decoy `go test ... -run '...'` inside an earlier
// `echo` in a pipeline. This has none of those failure modes, costs no extra process, and
// is one string.
const noTestsRun = "no tests to run"

// A verify command has to be shown to PASS on the unmutated code before its failure on the
// mutated code means anything. Without that, a command already failing for an unrelated
// reason — a flaky test, a broken environment, a regression someone else introduced —
// fails identically with the mutation applied, and every mutation it guards scores as
// caught while the suite has proven nothing.
//
// Commands are deduplicated, though the saving is modest in practice: this repo's 40
// entries resolve to 32 distinct commands, so the baseline roughly doubles the number of
// suite invocations rather than adding a negligible fraction. That cost buys the only
// thing that makes "caught" mean anything, which is why it is paid.
func (r *Runner) baseline(muts []Mutation) (map[string]string, error) {
	commands := distinctCommands(muts)
	bad := map[string]string{}
	r.logf("baseline: %d distinct verify command(s) on unmutated code\n", len(commands))

	for _, command := range commands {
		c := r.verify(command)
		switch {
		case !c.ran:
			bad[command] = c.detail
			r.logf("  CANNOT RUN   %s\n", command)
		case c.failed:
			bad[command] = fmt.Sprintf("already failing before any mutation was applied: %s", c.detail)
			r.logf("  ALREADY RED  %s\n", command)
		case strings.Contains(c.output, noTestsRun):
			// Passed, but ran nothing — so it could never fail either.
			bad[command] = fmt.Sprintf("passes without running any test (%q), so it can "+
				"never catch anything — the named test has probably been renamed: %s", noTestsRun, command)
			r.logf("  VACUOUS      %s\n", command)
		}
	}
	if len(bad) > 0 {
		r.logf("\n")
	}

	// The baseline just ran every command in the catalog against the real tree, and a
	// verify command is an arbitrary shell line that may write files. If one of them left
	// the tree dirty, the bytes a later mutation reads as "original" are no longer the
	// committed ones, and restoring them would quietly leave that change behind — defeating
	// the clean-tree guarantee checked at the top of Run. This hazard did not exist before
	// the baseline, because nothing ever ran against unmutated code.
	if err := r.requireCleanTree(); err != nil {
		return nil, fmt.Errorf("a verify command changed the working tree during the "+
			"baseline run, so the original bytes can no longer be trusted:\n%w", err)
	}
	return bad, nil
}

// distinctCommands lists every verify command in the catalog once, sorted so two runs of
// the same catalog report in the same order.
func distinctCommands(muts []Mutation) []string {
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
	return commands
}
