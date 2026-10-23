package push

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/pinepeakdigital/looper/internal/golden"
	"github.com/pinepeakdigital/looper/internal/record"
)

// The parity gate for the push decision. Each case compares the whole JSON verdict against
// what push-check.py printed for the same store, git repo and flags before it was retired —
// frozen under testdata/oracle; see internal/golden for what those files are and are not.
//
// The oracle covers more than the verdict: `disclose` is prose an operator reads, and a
// reworded disclosure is a silent behaviour change that no verdict comparison catches.
func pyPushCheck(t *testing.T) Result {
	t.Helper()
	out := golden.Read(t, "push-check.json")
	var got Result
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("frozen oracle output %q: %v", out, err)
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
	// The same failing gh the goldens were captured under, so this side cannot satisfy the
	// report gate from a PR either and falls through to the pending file. Shimmed rather than
	// left to the real
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
		// The three flag values and the one switch, defaulted below — the inputs each
		// case's golden was captured with.
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
		// A run absent from the store used to live here as an agreement case. It is now an
		// enumerated DIVERGENCE — this side refuses, the Python permits with a disclosure —
		// so it is asserted by TestTheEmptyRecordDivergence below instead. Leaving it here
		// would have failed the table, which is exactly what it did when the block landed.
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

			dir, _ := repo(t)
			store := writeStore(t, c.rows...)
			if c.land {
				// Rendered from the Go side's reading of the record. The goldens were captured
				// with a report rendered the same way, which the Python found, so a Fingerprint
				// that drifted from len(cycles) and the agent sum shows up as a `push` that no
				// longer matches the golden rather than as silent agreement.
				runs, err := record.Load(store, 0)
				if err != nil {
					t.Fatal(err)
				}
				r := runs[id]
				if r == nil {
					t.Fatalf("fixture has no run %q to render a report for", id)
				}
				landReport(t, id, Fingerprint(r))
			}

			py := pyPushCheck(t)

			got, err := Check(CheckParams{Store: store, RunID: id, GateState: gateState,
				UnresolvedSkip: c.unresolvedSkip, Branch: branch, DefaultBranch: defaultBranch,
				Repo: dir})
			if err != nil {
				t.Fatalf("Check: %v", err)
			}
			sameResult(t, c.name, py, got)
		})
	}
}

// parity02 runs Check on the store TestTheDisclosureWordingDivergence's golden was captured
// from — a plan and one cycle with a null applied, report landed — optionally plus a clean
// finish row. One builder, because TestTheUnfinishedRunDivergence reuses that golden and is
// only valid while the two stores differ by the finish row alone.
func parity02(t *testing.T, finished bool) Result {
	t.Helper()
	shimGh(t, "exit 1")
	const id = "parity02"
	dir, _ := repo(t)
	rows := []string{
		fmt.Sprintf(`{"run_id":%q,"phase":"plan","planned_at":"2026-01-01T00:00:00","repo":"x","agent_cap":40}`, id),
		fmt.Sprintf(`{"run_id":%q,"phase":"cycle","n":1,"applied":null,"agents":4}`, id),
	}
	if finished {
		rows = append(rows, fmt.Sprintf(`{"run_id":%q,"phase":"finish","outcome":"clean"}`, id))
	}
	store := writeStore(t, rows...)
	runs, err := record.Load(store, 0)
	if err != nil {
		t.Fatal(err)
	}
	landReport(t, id, Fingerprint(runs[id]))
	got, err := Check(CheckParams{Store: store, RunID: id, GateState: "passed",
		Branch: "feat/x", DefaultBranch: "main", Repo: dir})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	return got
}

// The one place the two disagree on purpose, and the whole of it. A *int is nil for an
// absent key and for an explicit null alike, so `applied: null` renders "?" here where the
// Python renders "None". Enumerated rather than compared, with the grounding that would
// expire: no row in the real store holds a null `applied`, both spellings tell the reader
// the same thing, and the alternative is three-state decoding on the field every verdict
// turns on.
//
// Stated as a TEST, not a comment, so the day someone makes the pointer three-state this
// says which half moved. The Python half is frozen; it changes only by editing its golden.
func TestTheDisclosureWordingDivergence(t *testing.T) {
	// The finish row is not in the captured store. A clean finish cannot move the Python's
	// answer — it reads the finish only for a broken outcome — and without one this side
	// refuses for the unfinished-run divergence, before the wording is reached.
	got := parity02(t, true)
	py := pyPushCheck(t)
	// Everything but the one word must still agree, which is what makes this a divergence
	// rather than a disagreement: same verdict, same reason, same convergence.
	if py.Push != got.Push || py.Reason != got.Reason || py.Convergence != got.Convergence {
		t.Errorf("a null applied changed more than the disclosure wording:\n py %+v\n go %+v", py, got)
	}
	if py.Disclose == nil || got.Disclose == nil {
		t.Fatalf("both sides owe a disclosure here: py=%v go=%v", deref(py.Disclose), deref(got.Disclose))
	}
	if !strings.Contains(*py.Disclose, "applied None fix(es)") {
		t.Errorf("the frozen golden no longer records the Python rendering a null applied as "+
			"None: %q — it was edited, and this enumerated divergence needs restating", *py.Disclose)
	}
	if !strings.Contains(*got.Disclose, "applied ? fix(es)") {
		t.Errorf("this side no longer renders a null applied as ?: %q", *got.Disclose)
	}
	// And the rest of the line is identical, so the divergence is exactly one token wide.
	if strings.Replace(*py.Disclose, "applied None", "applied ?", 1) != *got.Disclose {
		t.Errorf("the divergence is wider than the one word:\n py %q\n go %q", *py.Disclose, *got.Disclose)
	}
}

// The second enumerated divergence, and the first of two that change a VERDICT rather than a
// word: a run id that resolves to no readable record. The Python permitted with an `unknown`
// disclosure, because the design's split says "we stopped looking" discloses and pushes. This
// side refuses, because an empty record is not a run that stopped looking — it is the absence
// of any evidence that a run happened, and every input the gate reads comes off it.
//
// Deliberate, approved, and asserted here rather than left to drift. Both halves are pinned:
// that the frozen golden records the Python permitting, and that this side refuses for the
// empty-record reason specifically rather than incidentally via the report or convergence
// checks.
//
// The report is LANDED and genuinely satisfying, which is the whole point: Fingerprint of an
// empty record is the two lines pr-report.py renders for a run with no cycle rows, so no
// forgery is needed to reach the state the Python permitted.
func TestTheEmptyRecordDivergence(t *testing.T) {
	shimGh(t, "exit 1")
	const id = "parity03"
	dir, _ := repo(t)
	// A store that exists and holds a DIFFERENT run, so the file is readable and only this
	// run id is absent — the wrong-run-id case rather than the missing-file case.
	store := writeStore(t,
		`{"run_id":"other1","phase":"plan","planned_at":"2026-01-01T00:00:00","repo":"x","agent_cap":8}`,
		`{"run_id":"other1","phase":"cycle","n":1,"applied":1,"agents":2}`)
	landReport(t, id, Fingerprint(&record.Run{ID: id}))

	py := pyPushCheck(t)
	got, err := Check(CheckParams{Store: store, RunID: id, GateState: "passed",
		Branch: "feat/x", DefaultBranch: "main", Repo: dir})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}

	if !py.Push {
		t.Errorf("the frozen golden no longer records the Python permitting an empty record "+
			"(%q) — it was edited, and this enumerated divergence needs restating", py.Reason)
	}
	if got.Push {
		t.Fatalf("this side permitted a push off an empty record with a matching report: %+v", got)
	}
	if !strings.Contains(got.Reason, "no readable record") {
		t.Errorf("reason = %q, want the empty-record refusal — a refusal for some other "+
			"reason would close this hole only by accident", got.Reason)
	}
	// Convergence still agrees: the divergence is the VERDICT, not the derivation. Both read
	// the same empty record the same way.
	if py.Convergence != got.Convergence {
		t.Errorf("convergence diverged too (py %q, go %q) — the derivation is meant to be "+
			"identical and only the decision different", py.Convergence, got.Convergence)
	}
}

// The third enumerated divergence, and the second that changes a VERDICT: a run with a
// record but no finish row. The Python never looked, so it permitted. This side refuses,
// because Step 14 closes the record before it posts the report or asks the gate, so reaching
// here unfinished means a step was skipped — and the report already posted rendered no
// outcome and no tier.
//
// The golden is TestTheDisclosureWordingDivergence's capture, copied: that store had no
// finish row, and push-check.py has since been deleted, so no new capture is possible. Its
// verdict is the evidence the Python permitted exactly this state.
func TestTheUnfinishedRunDivergence(t *testing.T) {
	got := parity02(t, false)
	py := pyPushCheck(t)
	if !py.Push {
		t.Errorf("the frozen golden no longer records the Python permitting an unfinished run "+
			"(%q) — it was edited, and this enumerated divergence needs restating", py.Reason)
	}
	if got.Push {
		t.Fatalf("this side permitted a push for a run with no finish row: %+v", got)
	}
	if !strings.Contains(got.Reason, "never finished") {
		t.Errorf("reason = %q, want the unfinished-run refusal — a refusal for some other "+
			"reason would close this hole only by accident", got.Reason)
	}
	if py.Convergence != got.Convergence {
		t.Errorf("convergence diverged too (py %q, go %q)", py.Convergence, got.Convergence)
	}
}
