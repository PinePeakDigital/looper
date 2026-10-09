package push

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pinepeakdigital/looper/internal/record"
)

// The parity gate for the push decision. Until push-check.py is retired, the two must not
// be able to disagree: this runs the Python against the same store, the same git repo and
// the same flags, and requires the same JSON.
//
// A port is the one refactor with a free oracle — the thing being replaced still runs — and
// the previous slice's worst defects were all found by running it rather than reading it.
// Here the oracle covers more than the verdict: `disclose` is prose an operator reads, and
// a reworded disclosure is a silent behaviour change that no verdict comparison catches.
//
// Skipped, not failed, when the Python is unreachable: this repo must test in a checkout
// with no skills repo beside it. REVIEW_LOOP_PUSH_CHECK names the script; otherwise the
// default sibling path is tried.
func pushCheckPath(t *testing.T) string {
	t.Helper()
	if p := os.Getenv("REVIEW_LOOP_PUSH_CHECK"); p != "" {
		if _, err := os.Stat(p); err == nil {
			return p
		}
		t.Fatalf("REVIEW_LOOP_PUSH_CHECK=%s does not exist — set it correctly or unset it", p)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory to look for push-check.py in")
	}
	p := filepath.Join(home, ".claude", "skills", "review-loop", "push-check.py")
	if _, err := os.Stat(p); err != nil {
		t.Skip("push-check.py not found; set REVIEW_LOOP_PUSH_CHECK to run the parity gate")
	}
	return p
}

// pyPushCheck runs the Python and returns its parsed JSON.
func pyPushCheck(t *testing.T, script, store string, args ...string) Result {
	t.Helper()
	cmd := exec.Command("python3", append([]string{script}, args...)...)
	cmd.Env = append(os.Environ(), "REVIEW_LOOP_RUNS="+store)
	var errOut strings.Builder
	cmd.Stderr = &errOut
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("python side failed: %v\nstderr:\n%s", err, errOut.String())
	}
	var got Result
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("python printed %q: %v\nstderr:\n%s", out, err, errOut.String())
	}
	return got
}

// The Python reads `convergence` as `conv or "unknown"` and `disclose` as None for a
// converged run, which is exactly Result's shape — so the comparison is field-wise on the
// decoded struct rather than on bytes. json.dumps and encoding/json differ in separators
// and in HTML escaping, and a byte comparison would be asserting those rather than the
// decision.
func sameResult(t *testing.T, label string, py, got Result) {
	t.Helper()
	if py.Push != got.Push {
		t.Errorf("%s: push py=%v go=%v (py reason %q; go reason %q)", label, py.Push, got.Push, py.Reason, got.Reason)
	}
	if py.Reason != got.Reason {
		t.Errorf("%s: reason\n py %q\n go %q", label, py.Reason, got.Reason)
	}
	if py.Convergence != got.Convergence {
		t.Errorf("%s: convergence py=%q go=%q", label, py.Convergence, got.Convergence)
	}
	switch {
	case (py.Disclose == nil) != (got.Disclose == nil):
		t.Errorf("%s: disclose py=%v go=%v — one owes a line and the other does not",
			label, deref(py.Disclose), deref(got.Disclose))
	case py.Disclose != nil && *py.Disclose != *got.Disclose:
		t.Errorf("%s: disclose\n py %q\n go %q", label, *py.Disclose, *got.Disclose)
	}
}

func deref(s *string) string {
	if s == nil {
		return "<null>"
	}
	return *s
}

func TestParityWithPushCheck(t *testing.T) {
	script := pushCheckPath(t)
	// Both sides see the same failing gh, so neither can satisfy the report gate from a PR
	// and both fall through to the pending file. Shimmed rather than left to the real
	// binary: gh's outcome would otherwise depend on a login, the network, and which
	// directory the test happened to run in.
	shimGh(t, "exit 1")

	const id = "parity01"
	plan8 := fmt.Sprintf(`{"run_id":%q,"phase":"plan","planned_at":"2026-01-01T00:00:00","repo":"x","agent_cap":8,"gates":{"t":{"planned":"run"}}}`, id)
	plan40 := fmt.Sprintf(`{"run_id":%q,"phase":"plan","planned_at":"2026-01-01T00:00:00","repo":"x","agent_cap":40,"gates":{"t":{"planned":"run"}}}`, id)
	finish := func(extra string) string {
		return fmt.Sprintf(`{"run_id":%q,"phase":"finish","outcome":"cycle-limit","finished_at":"2026-01-01T01:00:00","executed":{"t":{"status":"done"}}%s}`, id, extra)
	}

	for _, c := range []struct {
		name string
		rows []string
		// The three flag values and the one switch, defaulted below. Named fields rather
		// than a flag slice both sides have to parse: the Go side would otherwise be
		// reading the Python's argv to decide what to pass itself, which is a second
		// implementation of the thing under comparison.
		gateState, branch, defaultBranch string
		unresolvedSkip                   bool
		// land says whether this run's report is written to the pending file first.
		land bool
	}{
		{name: "converged", rows: []string{plan40,
			fmt.Sprintf(`{"run_id":%q,"phase":"cycle","n":1,"applied":0,"asked":0,"agents":3}`, id),
			finish("")}, land: true},
		{name: "capped carries its disclosure", rows: []string{plan8,
			fmt.Sprintf(`{"run_id":%q,"phase":"cycle","n":1,"applied":3,"agents":9}`, id),
			finish("")}, land: true},
		{name: "halted with asks on the cycle", rows: []string{plan40,
			fmt.Sprintf(`{"run_id":%q,"phase":"cycle","n":1,"applied":2,"asked":3,"agents":4}`, id),
			finish("")}, land: true},
		{name: "halted with asks only at finish", rows: []string{plan40,
			fmt.Sprintf(`{"run_id":%q,"phase":"cycle","n":1,"applied":2,"asked":0,"agents":4}`, id),
			finish(`,"unresolved_asks":7`)}, land: true},
		{name: "halted with a changed analysis pass", rows: []string{plan40,
			fmt.Sprintf(`{"run_id":%q,"phase":"cycle","n":1,"applied":2,"agents":4,"analysis_changed":true}`, id),
			finish("")}, land: true},
		{name: "halted over three cycles", rows: []string{plan40,
			fmt.Sprintf(`{"run_id":%q,"phase":"cycle","n":1,"applied":5,"agents":4}`, id),
			fmt.Sprintf(`{"run_id":%q,"phase":"cycle","n":2,"applied":1,"agents":4}`, id),
			fmt.Sprintf(`{"run_id":%q,"phase":"cycle","n":3,"applied":2,"asked":1,"agents":2,"analysis_changed":true}`, id),
			finish("")}, land: true},
		// A cycle with no agents field, which both read as spending nothing.
		{name: "a cycle with no agents field", rows: []string{plan40,
			fmt.Sprintf(`{"run_id":%q,"phase":"cycle","n":1,"applied":2,"agents":4}`, id),
			fmt.Sprintf(`{"run_id":%q,"phase":"cycle","n":2,"applied":1}`, id),
			finish("")}, land: true},
		// No cycle rows at all: the UNKNOWN disclosure, which both must word identically.
		{name: "no cycles", rows: []string{plan40, finish("")}, land: true},
		// A run absent from the store. Nothing to land a report for, so this also exercises
		// the report gate's blocking path on an unknown run.
		{name: "absent from the store"},
		// The report gate itself: same rows as the capped case, nothing landed.
		{name: "no report landed", rows: []string{plan8,
			fmt.Sprintf(`{"run_id":%q,"phase":"cycle","n":1,"applied":3,"agents":9}`, id),
			finish("")}},
		// The blockers, each outranking a converged review.
		{name: "a recorded broken outcome", rows: []string{plan40,
			fmt.Sprintf(`{"run_id":%q,"phase":"cycle","n":1,"applied":0,"asked":0,"agents":3}`, id),
			fmt.Sprintf(`{"run_id":%q,"phase":"finish","outcome":"test-failure","executed":{"t":{"status":"done"}}}`, id)},
			land: true},
		{name: "a blocked evidence gate", rows: []string{plan40,
			fmt.Sprintf(`{"run_id":%q,"phase":"cycle","n":1,"applied":0,"asked":0,"agents":3}`, id),
			finish("")}, gateState: "blocked", land: true},
		{name: "an unresolved skip", rows: []string{plan40,
			fmt.Sprintf(`{"run_id":%q,"phase":"cycle","n":1,"applied":0,"asked":0,"agents":3}`, id),
			finish("")}, unresolvedSkip: true, land: true},
		{name: "the default branch", rows: []string{plan40,
			fmt.Sprintf(`{"run_id":%q,"phase":"cycle","n":1,"applied":0,"asked":0,"agents":3}`, id),
			finish("")}, branch: "main", land: true},
		{name: "an unknown default branch", rows: []string{plan40,
			fmt.Sprintf(`{"run_id":%q,"phase":"cycle","n":1,"applied":0,"asked":0,"agents":3}`, id),
			finish("")}, defaultBranch: "\x00", land: true},
		// An ABSENT applied count renders "?" on both sides — the Python's
		// `last.get("applied", "?")` default, which agrees with a nil *int. Only an
		// explicit null diverges, and that case is enumerated below rather than compared.
		{name: "an absent applied count", rows: []string{plan40,
			fmt.Sprintf(`{"run_id":%q,"phase":"cycle","n":1,"agents":4}`, id),
			finish("")}, land: true},
		// A negative ask count renders rather than being hidden as falsy — the one place
		// Python's truthiness and Go's `!= 0` have to be checked to agree.
		{name: "a negative ask count", rows: []string{plan40,
			fmt.Sprintf(`{"run_id":%q,"phase":"cycle","n":1,"applied":2,"asked":-1,"agents":4}`, id),
			finish("")}, land: true},
		// A zero cap is not a cap on either side: the Python's guard is a bare `if cap`.
		{name: "a recorded cap of zero", rows: []string{
			fmt.Sprintf(`{"run_id":%q,"phase":"plan","planned_at":"2026-01-01T00:00:00","repo":"x","agent_cap":0,"gates":{"t":{"planned":"run"}}}`, id),
			fmt.Sprintf(`{"run_id":%q,"phase":"cycle","n":1,"applied":3,"agents":9}`, id),
			finish("")}, land: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			gateState, branch, defaultBranch := c.gateState, c.branch, c.defaultBranch
			if gateState == "" {
				gateState = "passed"
			}
			if branch == "" {
				branch = "feat/x"
			}
			// "\x00" is how a case asks for an EMPTY default branch, which is a real input
			// (git could not resolve one) and is therefore not available as the zero value.
			switch defaultBranch {
			case "":
				defaultBranch = "main"
			case "\x00":
				defaultBranch = ""
			}

			dir, gitdir := repo(t)
			store := writeStore(t, c.rows...)
			if c.land {
				// Rendered from the Go side's reading of the record, which is also the
				// Python's: the fingerprint is len(cycles) and the agent sum, so if the two
				// disagreed about either, the Python would not find its own report and the
				// case would fail on `push` rather than silently agreeing.
				runs, err := record.Load(store, 0)
				if err != nil {
					t.Fatal(err)
				}
				r := runs[id]
				if r == nil {
					t.Fatalf("fixture has no run %q to render a report for", id)
				}
				landReport(t, gitdir, id, Fingerprint(r))
			}

			args := []string{"--run-id", id, "--repo", dir, "--gate-state", gateState,
				"--branch", branch, "--default-branch", defaultBranch}
			if c.unresolvedSkip {
				args = append(args, "--unresolved-skip")
			}
			py := pyPushCheck(t, script, store, args...)

			got, err := Check(store, id, gateState, c.unresolvedSkip, branch, defaultBranch, dir)
			if err != nil {
				t.Fatalf("Check: %v", err)
			}
			sameResult(t, c.name, py, got)
		})
	}
}

// The one place the two disagree on purpose, and the whole of it. A *int is nil for an
// absent key and for an explicit null alike, so `applied: null` renders "?" here where the
// Python renders "None". Enumerated rather than compared, with the grounding that would
// expire: no row in the real store holds a null `applied`, both spellings tell the reader
// the same thing, and the alternative is three-state decoding on the field every verdict
// turns on.
//
// Stated as a TEST, not a comment, so the day the Python's wording is what matters, or the
// day someone makes the pointer three-state, this says which half moved.
func TestTheOneDisclosureDivergence(t *testing.T) {
	script := pushCheckPath(t)
	shimGh(t, "exit 1")
	const id = "parity02"
	dir, gitdir := repo(t)
	store := writeStore(t,
		fmt.Sprintf(`{"run_id":%q,"phase":"plan","planned_at":"2026-01-01T00:00:00","repo":"x","agent_cap":40}`, id),
		fmt.Sprintf(`{"run_id":%q,"phase":"cycle","n":1,"applied":null,"agents":4}`, id))
	runs, err := record.Load(store, 0)
	if err != nil {
		t.Fatal(err)
	}
	landReport(t, gitdir, id, Fingerprint(runs[id]))

	py := pyPushCheck(t, script, store, "--run-id", id, "--repo", dir,
		"--gate-state", "passed", "--branch", "feat/x", "--default-branch", "main")
	got, err := Check(store, id, "passed", false, "feat/x", "main", dir)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	// Everything but the one word must still agree, which is what makes this a divergence
	// rather than a disagreement: same verdict, same reason, same convergence.
	if py.Push != got.Push || py.Reason != got.Reason || py.Convergence != got.Convergence {
		t.Errorf("a null applied changed more than the disclosure wording:\n py %+v\n go %+v", py, got)
	}
	if py.Disclose == nil || got.Disclose == nil {
		t.Fatalf("both sides owe a disclosure here: py=%v go=%v", deref(py.Disclose), deref(got.Disclose))
	}
	if !strings.Contains(*py.Disclose, "applied None fix(es)") {
		t.Errorf("the Python no longer renders a null applied as None: %q — if it now renders "+
			"something else, this divergence has moved and the comparison above should cover it",
			*py.Disclose)
	}
	if !strings.Contains(*got.Disclose, "applied ? fix(es)") {
		t.Errorf("this side no longer renders a null applied as ?: %q", *got.Disclose)
	}
	// And the rest of the line is identical, so the divergence is exactly one token wide.
	if strings.Replace(*py.Disclose, "applied None", "applied ?", 1) != *got.Disclose {
		t.Errorf("the divergence is wider than the one word:\n py %q\n go %q", *py.Disclose, *got.Disclose)
	}
}
