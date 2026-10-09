package record

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// The parity gate in parity_test.go SKIPS when runlog.py is not reachable, which it will
// not be in this repo's CI. So the behaviours it compares need assertions of their own
// here, stating the expected answer directly. Parity then adds "and the Python agrees"
// on a machine that has both. A gate that can skip is not a gate on its own.

// mustConv and mustDropped assert the run decoded cleanly. A test that meant to exercise a
// well-formed fixture must never swallow a decode error with `_` — that is how a typed
// boundary turns back into a silent coercion.
func mustConv(t *testing.T, r *Run) string {
	t.Helper()
	got, err := r.Convergence()
	if err != nil {
		t.Fatalf("Convergence: unexpected decode error: %v", err)
	}
	return got
}

func mustDropped(t *testing.T, r *Run) map[string]string {
	t.Helper()
	got, err := r.DroppedGates()
	if err != nil {
		t.Fatalf("DroppedGates: unexpected decode error: %v", err)
	}
	return got
}

func store(t *testing.T, rows ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "runs.jsonl")
	body := ""
	for _, r := range rows {
		body += r + "\n"
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// rawStore writes the bytes verbatim. store() always ends the last row with a newline,
// so it cannot produce the shape this exists for: a store whose final line has none.
func rawStore(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "runs.jsonl")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func convergenceOf(t *testing.T, rows ...string) string {
	t.Helper()
	runs, err := Load(store(t, rows...), 0)
	if err != nil {
		t.Fatal(err)
	}
	r := runs["r"]
	if r == nil {
		t.Fatal("no run r in the fixture")
	}
	got, err := r.Convergence()
	if err != nil {
		t.Fatalf("Convergence: unexpected decode error: %v", err)
	}
	return got
}

const plan = `{"run_id":"r","phase":"plan","planned_at":"2026-01-01T00:00:00","repo":"x","agent_cap":40}`

func TestConvergenceRules(t *testing.T) {
	for _, c := range []struct {
		name string
		want string
		rows []string
	}{
		// A MISSING `applied` is not zero. The Python compares `last.get("applied") == 0`
		// and `None == 0` is False, so absence falls through to the cap/halted path.
		// Reading absence as zero derived Converged here — a clean result, with no
		// disclosure, bought by leaving a field out.
		{"a cycle with no applied field is not converged", Halted, []string{plan,
			`{"run_id":"r","phase":"cycle","n":1,"agents":5}`}},
		{"an explicitly null applied is not converged", Halted, []string{plan,
			`{"run_id":"r","phase":"cycle","n":1,"applied":null,"agents":5}`}},
		// Converged is checked BEFORE capped, and this is the only shape where both
		// predicates are true at once, so it is the only case that can fail if the two
		// branches are swapped. Without it the ordering was free to invert.
		{"a converged last cycle wins over a cap it also reached", Converged, []string{
			`{"run_id":"r","phase":"plan","planned_at":"2026-01-01T00:00:00","repo":"x","agent_cap":5}`,
			`{"run_id":"r","phase":"cycle","n":1,"applied":0,"agents":5}`}},
		// The ZERO direction of the `!= 0` cap guard, which nothing covered. Deleting
		// `*r.Plan.AgentCap != 0` from the cap test survived the whole suite, and a
		// differential over 18,041 synthetic stores run during review put the change at
		// 2,730 of them, halted -> capped (that count is the review's; what is reproduced
		// here is only that the mutation changes behaviour at all). With the guard gone a
		// recorded cap of 0 makes `spent >= 0` true for every run. The negative direction
		// already had `a negative cap is still a cap` below; "no recorded cap is never
		// capped" tested only ABSENCE, and absent takes a different branch (AgentCap is
		// nil) so it could never reach this comparison. Same reachability class as the
		// negative case — cmd_plan will not write a 0, and the store keeps what it has.
		{"a recorded cap of zero is not a cap", Halted, []string{
			`{"run_id":"r","phase":"plan","planned_at":"2026-01-01T00:00:00","repo":"x","agent_cap":0}`,
			`{"run_id":"r","phase":"cycle","n":1,"applied":3,"agents":5}`}},
		// The `c.Agents != nil` arm of the spend sum, which also survived. Absence must
		// contribute 0, matching the Python's `or 0`; a mutant contributing 1 instead needs
		// a cap small enough for one agent to cross it, which no case had. Not reachable
		// from the writer (`--agents` is required=True) and absent from all 59 real cycle
		// rows, so this pins a parity claim that would otherwise stand with nothing behind
		// it rather than guarding a live defect.
		{"a cycle with no agents field spends nothing", Halted, []string{
			`{"run_id":"r","phase":"plan","planned_at":"2026-01-01T00:00:00","repo":"x","agent_cap":1}`,
			`{"run_id":"r","phase":"cycle","n":1,"applied":3}`}},
		// The Python cap guard is a bare `if cap`, so a negative cap is truthy and any
		// spend clears it. cmd_plan will not write one; the store keeps what it has.
		{"a negative cap is still a cap", Capped, []string{
			`{"run_id":"r","phase":"plan","planned_at":"2026-01-01T00:00:00","repo":"x","agent_cap":-5}`,
			`{"run_id":"r","phase":"cycle","n":1,"applied":3,"agents":10}`}},
		{"a final zero-fix cycle converges", Converged, []string{plan,
			`{"run_id":"r","phase":"cycle","n":1,"applied":5,"agents":6}`,
			`{"run_id":"r","phase":"cycle","n":2,"applied":0,"agents":2}`}},
		{"fixes still landing with budget left is halted", Halted, []string{plan,
			`{"run_id":"r","phase":"cycle","n":1,"applied":5,"agents":6}`}},
		// No cycle rows must never read as converged, or omitting them buys a push.
		{"no cycle rows is unknown", Unknown, []string{plan}},
		// A cycle that routed every finding to the user resolved none of them.
		{"zero applied with asks is not converged", Halted, []string{plan,
			`{"run_id":"r","phase":"cycle","n":1,"applied":0,"agents":3,"asked":7}`}},
		// ...but zero asked is falsy, and that distinction is the whole point.
		{"zero asked still converges", Converged, []string{plan,
			`{"run_id":"r","phase":"cycle","n":1,"applied":0,"agents":2,"asked":0}`}},
		// The omission path: a clean-looking last cycle with the asks only at finish.
		// This derived converged with no disclosure while seven findings sat unresolved.
		{"asks recorded only at finish are still outstanding", Halted, []string{plan,
			`{"run_id":"r","phase":"cycle","n":1,"applied":0,"agents":3}`,
			`{"run_id":"r","phase":"finish","outcome":"clean","unresolved_asks":7}`}},
		// The deterministic pass changing files is unfinished work too.
		{"analysis changing files is not converged", Halted, []string{plan,
			`{"run_id":"r","phase":"cycle","n":1,"applied":0,"agents":3,"analysis_changed":true}`}},
		// One agent short of the cap is not capped. The other half of the boundary: with
		// only the at-cap case pinned, an off-by-one that fires early reads as correct.
		{"one agent short of the cap is not capped", Halted, []string{
			`{"run_id":"r","phase":"plan","planned_at":"2026-01-01T00:00:00","repo":"x","agent_cap":8}`,
			`{"run_id":"r","phase":"cycle","n":1,"applied":1,"agents":7}`}},
		// Spend exactly at the cap is capped, not halted. The boundary, stated.
		{"spend exactly at the cap is capped", Capped, []string{
			`{"run_id":"r","phase":"plan","planned_at":"2026-01-01T00:00:00","repo":"x","agent_cap":8}`,
			`{"run_id":"r","phase":"cycle","n":1,"applied":1,"agents":8}`}},
		// An absent cap must not make every run capped.
		{"no recorded cap is never capped", Halted, []string{
			`{"run_id":"r","phase":"plan","planned_at":"2026-01-01T00:00:00","repo":"x"}`,
			`{"run_id":"r","phase":"cycle","n":1,"applied":4,"agents":99}`}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := convergenceOf(t, c.rows...); got != c.want {
				t.Errorf("convergence = %q, want %q", got, c.want)
			}
		})
	}
}

// A restart resets the cycle counter, so two rows legitimately share n=1. Deduping by n
// deleted the first pass of every restart from every derived answer: with rows
// n=1/20 agents, n=2/15, n=1/5, the cap counted 20 of the 40 actually spent.
func TestRestartsRepeatedCycleNumberIsKept(t *testing.T) {
	rows := []string{plan,
		`{"run_id":"r","phase":"cycle","n":1,"applied":2,"agents":20}`,
		`{"run_id":"r","phase":"cycle","n":2,"applied":2,"agents":15}`,
		`{"run_id":"r","phase":"cycle","n":1,"applied":2,"agents":5}`}
	runs, err := Load(store(t, rows...), 0)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(runs["r"].Cycles); n != 3 {
		t.Errorf("kept %d cycle rows, want 3 — a repeated n was collapsed", n)
	}
	// And the spend counts all three, which is what makes the cap honest.
	if got := mustConv(t, runs["r"]); got != Capped {
		t.Errorf("convergence = %q, want %q — 40 of 40 agents were spent", got, Capped)
	}
}

// A torn write never invalidates the rest of the store, and a row with no run_id is
// skipped rather than fatal.
func TestUnreadableRowsDoNotInvalidateTheStore(t *testing.T) {
	runs, err := Load(store(t, plan,
		`{"run_id":"r","phase":"cycle","n":1,"applied":0,"agents":2}`,
		``,
		`{"phase":"cycle","n":9,"applied":9,"agents":9}`,
		`{"run_id":"broke`), 0)
	// The claim can fail two ways — an error back, or the good run missing — and both
	// are the same defect, so both say so. A bare t.Fatal(err) here reported the JSON
	// parse error instead, leaving the assertion below decorative.
	if err != nil {
		t.Fatalf("a torn line discarded the whole store: %v", err)
	}
	if runs["r"] == nil {
		t.Fatal("a torn line discarded the whole store")
	}
	if n := len(runs["r"].Cycles); n != 1 {
		t.Errorf("kept %d cycle rows, want 1 — a row with no run_id was attributed to r", n)
	}
	if got := mustConv(t, runs["r"]); got != Converged {
		t.Errorf("convergence = %q, want %q", got, Converged)
	}
}

// Reading the tail must take the LAST n lines. Taking the first would make a long store
// report on runs that have already scrolled away.
func TestTailLimitKeepsTheNewestRows(t *testing.T) {
	rows := []string{
		`{"run_id":"old","phase":"plan","planned_at":"2026-01-01T00:00:00","repo":"x"}`,
		`{"run_id":"new","phase":"plan","planned_at":"2026-01-02T00:00:00","repo":"x"}`,
	}
	runs, err := Load(store(t, rows...), 1)
	if err != nil {
		t.Fatal(err)
	}
	if runs["new"] == nil {
		t.Error("the tail dropped the newest run")
	}
	if runs["old"] != nil {
		t.Error("the tail kept a run it should have scrolled past")
	}
}

// A missing store is empty, not an error: nothing was ever planned.
func TestMissingStoreIsEmpty(t *testing.T) {
	runs, err := Load(filepath.Join(t.TempDir(), "absent.jsonl"), 0)
	if err != nil {
		t.Fatalf("a missing store should not error: %v", err)
	}
	if len(runs) != 0 {
		t.Errorf("got %d runs from a missing store", len(runs))
	}
}

// `n/a` is success, not a drop: a gate that cannot apply is not a gate that was dropped.
// With a `status == "done"` test instead, every n/a counted as dropped and the Step 0
// alarm fired permanently on gates nobody could fix.
// Every falsy status collapses to "unreported", not just an absent entry. The Python
// stores `st or "unreported"`, so "", 0 and false all land there too. Defaulting only on
// absence reported `""`, `"0"` and `"false"` — a parity break, and an alarm line naming
// nothing. The status vocabulary is open (cmd_finish never validates the string) and a
// non-string status reaching this path has already happened once.
// The falsy collapse, for the two falsy statuses a typed status can still hold: absent and
// the empty string. `false` and `0` used to be here too — they were the point of the
// original test, because the Python's `st or "unreported"` collapses every falsy value — but
// a non-string status is now a decode error, asserted from the other side in
// TestTypedDecodeRefusesTheShapesThePythonCoerced. The collapse itself is still a real rule
// and still has to be tested: defaulting only on ABSENCE reported "" for a gate that
// recorded an empty status, and an empty status names nothing about the gate.
func TestDroppedGatesCollapsesTheFalsyStatusesAStringCanHold(t *testing.T) {
	runs, err := Load(store(t,
		`{"run_id":"r","phase":"plan","planned_at":"2026-01-01T00:00:00","repo":"x","agent_cap":40,"gates":{"g_empty":{"planned":"run"},"g_null":{"planned":"run"},"g_absent":{"planned":"run"}}}`,
		`{"run_id":"r","phase":"finish","outcome":"clean","executed":{"g_empty":{"status":""},"g_null":{"status":null}}}`), 0)
	if err != nil {
		t.Fatal(err)
	}
	got := mustDropped(t, runs["r"])
	for _, g := range []string{"g_empty", "g_null", "g_absent"} {
		if got[g] != "unreported" {
			t.Errorf("gate %s reported %q, want \"unreported\" — a falsy status must not reach the caller verbatim", g, got[g])
		}
	}
}

func TestAnOverlongLineDoesNotInvalidateTheStore(t *testing.T) {
	// The over-long row is valid JSON, just large — so it must be READ and USED, not
	// merely survived. Making it the last cycle, with the fields that decide the answer,
	// is what proves that: if it were dropped the answer would come from cycle 1 instead.
	huge := `{"run_id":"r","phase":"cycle","n":2,"applied":0,"agents":2,"junk":"` +
		strings.Repeat("x", 17*1024*1024) + `"}`
	runs, err := Load(store(t, plan,
		`{"run_id":"r","phase":"cycle","n":1,"applied":5,"agents":6}`,
		huge), 0)
	if err != nil {
		t.Fatalf("an over-long line discarded the whole store: %v", err)
	}
	if runs["r"] == nil {
		t.Fatal("an over-long line discarded the whole store")
	}
	if n := len(runs["r"].Cycles); n != 2 {
		t.Errorf("kept %d cycle rows, want 2 — the over-long row was dropped", n)
	}
	// Cycle 1 alone would be Halted; only reading the over-long cycle 2 gives Converged.
	if got := mustConv(t, runs["r"]); got != Converged {
		t.Errorf("convergence = %q, want %q — the over-long row was not used", got, Converged)
	}
}

// A store whose last line has no terminating newline must still yield that line. The
// reader appends what ReadString returned BEFORE testing the error, because at EOF it
// returns the final partial line together with io.EOF — move the append below the break
// and the last row vanishes. That row is usually the finish row, so losing it silently
// turns a finished run into an unfinished one.
func TestALastLineWithNoNewlineIsStillRead(t *testing.T) {
	body := plan + "\n" +
		`{"run_id":"r","phase":"cycle","n":1,"applied":5,"agents":6}` + "\n" +
		`{"run_id":"r","phase":"cycle","n":2,"applied":0,"agents":2}` // no trailing newline
	runs, err := Load(rawStore(t, body), 0)
	if err != nil {
		t.Fatalf("a store with no trailing newline failed to load: %v", err)
	}
	if runs["r"] == nil {
		t.Fatal("a store with no trailing newline produced no run")
	}
	if n := len(runs["r"].Cycles); n != 2 {
		t.Errorf("kept %d cycle rows, want 2 — the unterminated last line was dropped", n)
	}
	// Cycle 1 alone is Halted; only reading the unterminated cycle 2 gives Converged.
	if got := mustConv(t, runs["r"]); got != Converged {
		t.Errorf("convergence = %q, want %q — the unterminated last line was dropped", got, Converged)
	}
}

// What replaced TestDroppedGatesPinsTheDegradedAnswers. That test pinned an ASYMMETRY the
// types have removed: a non-dict `executed` value denied silence (every gate read as
// unreported) while a non-dict `gates` value GRANTED it (nothing read as dropped at all, so
// a run whose gate record was corrupt reported a clean sweep). The bad direction was the
// second, and it was accepted knowingly because Go had no crash available where the Python
// raised AttributeError.
//
// Neither shape decodes now, so neither direction exists. This asserts that — and it is
// worth asserting rather than deleting, because "it cannot happen" is the claim, and an
// unasserted claim is how the asymmetry got in.
func TestACorruptGateRecordCannotReportACleanSweep(t *testing.T) {
	for _, tc := range []struct{ name, plan, finish string }{
		{
			name:   "a non-dict gates value",
			plan:   `{"run_id":"r","phase":"plan","planned_at":"2026-01-01T00:00:00","repo":"x","gates":["g"]}`,
			finish: `{"run_id":"r","phase":"finish","outcome":"clean","executed":{}}`,
		},
		{
			name:   "a non-dict executed value",
			plan:   `{"run_id":"r","phase":"plan","planned_at":"2026-01-01T00:00:00","repo":"x","gates":{"g":{"planned":"run"}}}`,
			finish: `{"run_id":"r","phase":"finish","outcome":"clean","executed":"nope"}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runs, err := Load(store(t, tc.plan, tc.finish), 0)
			if err != nil {
				t.Fatal(err)
			}
			got, err := runs["r"].DroppedGates()
			if err == nil {
				// The two subtests are OPPOSITE directions and the old message could not
				// tell them apart: `gates: ["g"]` returned {} — the clean sweep this test
				// is named for — while `executed: "nope"` returned {g: "unreported"}, the
				// safe direction. Printing the map is what makes the failure readable.
				t.Errorf("a corrupt gate record returned %d gate(s) %v instead of an error; "+
					"an empty map is the clean sweep this test is named for, a populated one is the safe direction",
					len(got), got)
			}
		})
	}
}

func TestDroppedGatesTreatsNAAsHandled(t *testing.T) {
	runs, err := Load(store(t,
		`{"run_id":"r","phase":"plan","planned_at":"2026-01-01T00:00:00","repo":"x","gates":{"a":{"planned":"run"},"b":{"planned":"run"},"c":{"planned":"run"},"d":{"planned":"skip"}}}`,
		`{"run_id":"r","phase":"finish","outcome":"clean","executed":{"a":{"status":"done"},"b":{"status":"n/a"},"c":{"status":"skipped"}}}`), 0)
	if err != nil {
		t.Fatal(err)
	}
	got := mustDropped(t, runs["r"])
	if _, ok := got["a"]; ok {
		t.Error("a `done` gate was reported dropped")
	}
	if _, ok := got["b"]; ok {
		t.Error("an `n/a` gate was reported dropped — it was handled, not dropped")
	}
	if got["c"] != "skipped" {
		t.Errorf("a skipped gate reported %q, want \"skipped\"", got["c"])
	}
	// A gate the plan already marked skip is not a drop either.
	if _, ok := got["d"]; ok {
		t.Error("a gate the plan did not ask for was reported dropped")
	}
	// And a planned gate with no entry at all is the unreported case.
	runs, _ = Load(store(t,
		`{"run_id":"r","phase":"plan","planned_at":"2026-01-01T00:00:00","repo":"x","gates":{"e":{"planned":"run"}}}`,
		`{"run_id":"r","phase":"finish","outcome":"clean","executed":{}}`), 0)
	if got := mustDropped(t, runs["r"])["e"]; got != "unreported" {
		t.Errorf("an absent entry reported %q, want \"unreported\"", got)
	}
}

// Three rules the mutation sweep found standing on nothing: the blank-run_id guard, the fact
// that an unknown phase still CREATES the run, and the convergence value returned alongside
// an error. Each mutation survived the whole suite before this test existed.
func TestLoadsStructuralRulesThatNothingElseAsserts(t *testing.T) {
	t.Run("rows with no run id do not collect under a phantom empty key", func(t *testing.T) {
		// Deleting `if env.RunID == "" { continue }` survives every other test: the rows
		// still decode, so no verdict changes — they just land in a run keyed "". Python's
		// `if not rid: continue` drops them, so the phantom run is a Go-only invention, and
		// nothing counted the runs map, which is the only place it is visible.
		runs, err := Load(store(t,
			`{"run_id":"r","phase":"plan","planned_at":"2026-01-01T00:00:00","repo":"x","agent_cap":40}`,
			`{"run_id":"r","phase":"cycle","n":1,"applied":0,"agents":2}`,
			`{"run_id":"","phase":"cycle","n":9,"applied":9,"agents":9}`,
			`{"run_id":null,"phase":"cycle","n":9,"applied":9,"agents":9}`,
			`{"phase":"cycle","n":9,"applied":9,"agents":9}`), 0)
		if err != nil {
			t.Fatalf("Load must not fail on a store with unidentified rows: %v", err)
		}
		if len(runs) != 1 {
			t.Errorf("got %d run(s) %v, want exactly 1: a row with no run_id belongs to no run", len(runs), keysOf(runs))
		}
		if _, ok := runs[""]; ok {
			t.Error(`a run keyed "" exists, so unidentified rows were collected instead of dropped`)
		}
	})

	t.Run("no row can forge a clean verdict by being filed under the wrong phase", func(t *testing.T) {
		// The worst hole this branch had, and it took three passes to get right. load()
		// merges every non-cycle row into the run dict FIELD-WISE and never looks at
		// `phase`, so ANY run-level field on ANY non-cycle row reaches the Python's
		// verdict. On a store whose only cycle applied nothing, each row below makes the
		// Python say halted and used to make this say converged — a clean push, no
		// disclosure, with seven asks outstanding.
		//
		// The first guard keyed on the phase WORD and closed only the first three. The last
		// three spell real phases, and one spells two: both parsers take the last duplicate
		// key, so Go filed the row under "nudge" and dropped it while Python merged the
		// body regardless. Keying on field PLACEMENT instead closes all six, because that
		// is the actual mechanism.
		for _, bad := range []string{
			`{"run_id":"r","phase":"finnish","unresolved_asks":7}`,
			`{"run_id":"r","phase":7,"unresolved_asks":7}`,
			`{"run_id":"r","unresolved_asks":7}`,
			`{"run_id":"r","phase":"nudge","unresolved_asks":7}`,
			`{"run_id":"r","phase":"plan","unresolved_asks":7}`,
			`{"run_id":"r","phase":"finish","unresolved_asks":7,"phase":"nudge"}`,
			`{"run_id":"r","phase":"finish","gates":{"g":{"planned":"run"}}}`,
			`{"run_id":"r","phase":"nudge","executed":{"g":{"status":"done"}}}`,
			// agent_cap was the one map entry nothing pinned: deleting it survived the
			// whole suite. The plan row above sets no cap, so a misplaced 1 is the cap the
			// Python reads and this one did not.
			`{"run_id":"r","phase":"nudge","agent_cap":1}`,
			// `cycles` is the sharpest form of the class and belongs to NO phase: load()
			// builds it with setdefault+append for cycle rows and merges it like any other
			// key for the rest, so a non-cycle row carrying it OVERWRITES the whole cycle
			// list, and cycles_of reads it back at runlog.py:180. The Python answers
			// "Review completeness UNKNOWN ... Treat as unreviewed"; this answered
			// converged and pushed clean.
			`{"run_id":"r","phase":"nudge","cycles":[]}`,
			`{"run_id":"r","phase":"finish","outcome":"clean","unresolved_asks":0,"cycles":[]}`,
			`{"run_id":"r","phase":"plan","cycles":[]}`,
			// A DUPLICATE run_id whose first occurrence is wrong-typed: the envelope decode
			// returns an error AND populates RunID from the last occurrence, so bailing on
			// the error alone skipped a row Go could already attribute, while the Python's
			// last-wins merged its body. Four bytes prepended to the row above.
			`{"run_id":7,"run_id":"r","phase":"nudge","unresolved_asks":7}`,
			`{"run_id":[],"run_id":"r","phase":"nudge","unresolved_asks":7}`,
		} {
			runs, err := Load(store(t, plan40,
				`{"run_id":"r","phase":"cycle","n":1,"applied":0,"asked":0,"agents":2}`,
				bad), 0)
			if err != nil {
				t.Fatalf("Load must not fail: %v", err)
			}
			got, cErr := runs["r"].Convergence()
			if cErr == nil {
				t.Errorf("row %s decoded without an error; the Python merges its fields into the verdict regardless of phase", bad)
			}
			if got == Converged {
				t.Errorf("row %s produced %q — a clean verdict forged by filing a run-level field under the wrong phase", bad, got)
			}
		}
	})

	t.Run("a key that only differs in case is unreadable, not a field", func(t *testing.T) {
		// encoding/json matches struct tags CASE-INSENSITIVELY and Go has no case-sensitive
		// mode, so every one of these decodes into a field here while the Python's exact
		// `rec.get(...)` never reads it. Found by CodeRabbit on the PR, after four cycles of
		// review had closed four other mechanisms for the same forgery and missed this one —
		// and `misplacedRunField` cannot see it, because these keys fold onto fields the
		// row's phase legitimately owns.
		//
		// Measured against the oracle on a store whose earlier cycle applied 5. The first
		// five forge a clean verdict; the last two go the safe way and are refused on the
		// same rule, because "Go reads it and the Python does not" is the defect, not the
		// direction it happens to point.
		for _, bad := range []string{
			`{"run_id":"r","phase":"cycle","n":2,"Applied":0,"asked":0,"agents":1}`,
			`{"run_id":"r","phase":"cycle","n":2,"applied":3,"APPLIED":0,"asked":0,"agents":1}`,
			`{"Run_ID":"r","phase":"cycle","n":2,"applied":0,"asked":0,"agents":1}`,
			`{"run_id":"r","PHASE":"cycle","n":2,"applied":0,"asked":0,"agents":1}`,
			`{"run_id":"r","phase":"cycle","n":2,"applied":0,"Asked":3,"agents":1}`,
			`{"run_id":"r","phase":"cycle","n":2,"applied":3,"asked":0,"Agents":99}`,
			`{"run_id":"r","phase":"cycle","n":2,"applied":0,"asked":0,"agents":1,"Analysis_Changed":true}`,
		} {
			runs, err := Load(store(t, plan40,
				`{"run_id":"r","phase":"cycle","n":1,"applied":5,"asked":0,"agents":2}`,
				bad), 0)
			if err != nil {
				t.Fatalf("Load must not fail: %v", err)
			}
			if _, cErr := runs["r"].Convergence(); cErr == nil {
				t.Errorf("row %s decoded; a key Go folds onto a field and the Python never reads is unreadable", bad)
			}
		}
	})

	t.Run("a misplaced or folded outcome cannot unblock a push", func(t *testing.T) {
		// `outcome` is the first run-level field anything in this REPO reads, and it is read
		// to BLOCK: push.Decide refuses a push on test-failure, blocked or abandoned. No
		// derivation in THIS package reads it — Convergence, Disclosure and DroppedGates
		// never touch Finish.Outcome — so the probe below is Convergence standing in for
		// "the row did not decode", which is what every derivation's Err guard then refuses.
		// An earlier version of this comment said "a derivation here reads", which the
		// runLevelFields comment in record.go gets right and this did not.
		//
		// Every mechanism this file closes points at the field at once. load() merges it off
		// any non-cycle row, so the Python's blocker sees all five of these; the typed decode
		// either files the row by phase and drops it, or folds the variant spelling onto the
		// field and reads the wrong one.
		//
		// The last two go opposite ways and are both refused, because the rule is "Go and
		// the Python read different values", not "Go reads the permissive one".
		for _, bad := range []string{
			`{"run_id":"r","phase":"plan","outcome":"test-failure"}`,
			`{"run_id":"r","phase":"nudge","outcome":"test-failure"}`,
			`{"run_id":"r","phase":"finnish","outcome":"test-failure"}`,
			`{"run_id":"r","phase":"finish","outcome":"test-failure","Outcome":"clean"}`,
			`{"run_id":"r","phase":"finish","OUTCOME":"test-failure"}`,
		} {
			runs, err := Load(store(t, plan40,
				`{"run_id":"r","phase":"cycle","n":1,"applied":0,"asked":0,"agents":2}`,
				bad), 0)
			if err != nil {
				t.Fatalf("Load must not fail: %v", err)
			}
			if _, cErr := runs["r"].Convergence(); cErr == nil {
				t.Errorf("row %s decoded without an error; the Python's push blocker reads its outcome and this would not", bad)
			}
		}
	})

	t.Run("a folded key inside a gate cannot buy a clean sweep", func(t *testing.T) {
		// The gate maps are the worse half: here a folded key makes this report NOTHING
		// dropped where the Python reports the gate. Measured — `status:"failed"` beside
		// `Status:"done"` gives python={g:failed} and used to give go={}; `planned:"run"`
		// beside `PLANNED:"skip"` gives python={g:unreported} and used to give go={}.
		for _, tc := range []struct{ plan, finish string }{
			{`{"run_id":"r","phase":"plan","planned_at":"2026-01-01T00:00:00","repo":"x","gates":{"g":{"planned":"run"}}}`,
				`{"run_id":"r","phase":"finish","executed":{"g":{"status":"failed","Status":"done"}}}`},
			{`{"run_id":"r","phase":"plan","planned_at":"2026-01-01T00:00:00","repo":"x","gates":{"g":{"planned":"run","PLANNED":"skip"}}}`,
				`{"run_id":"r","phase":"finish","executed":{}}`},
		} {
			runs, err := Load(store(t, tc.plan,
				`{"run_id":"r","phase":"cycle","n":1,"applied":0,"asked":0,"agents":2}`,
				tc.finish), 0)
			if err != nil {
				t.Fatalf("Load must not fail: %v", err)
			}
			got, dErr := runs["r"].DroppedGates()
			if dErr == nil {
				t.Errorf("plan %s finish %s returned %d gate(s) %v instead of an error; an empty map here is the clean sweep",
					tc.plan, tc.finish, len(got), got)
			}
		}
	})

	t.Run("a phase word this build does not model is ignored, not refused", func(t *testing.T) {
		// The word-based guard refused these, which would mean every Go build older than
		// runlog.py's next new phase refuses every run carrying one. The Python ignores
		// such a row for verdict purposes (it merges fields nothing reads), so ignoring it
		// is both parity and the forward-compatible choice. What makes that safe is the
		// field check above: a future phase that DOES carry a run-level field still errors.
		for _, ok := range []string{
			`{"run_id":"r","phase":"nudge","nudged_at":"2026-01-01T00:00:00"}`,
			`{"run_id":"r","phase":"rebase","rebased_at":"2026-01-01T00:00:00"}`,
			`{"run_id":"r","phase":"cycle","n":2,"applied":0,"asked":0,"agents":1,"unresolved_asks":7}`,
			// A cycle row's own `cycles` key is harmless: load() appends cycle rows and
			// never merges them, so setdefault ignores it on both sides.
			`{"run_id":"r","phase":"cycle","n":2,"applied":0,"asked":0,"agents":1,"cycles":[]}`,
			// The check reads the TOP level only. A key named like a run-level field, nested
			// under another, is not merged into the run by either implementation.
			`{"run_id":"r","phase":"nudge","inputs":{"gates":{"g":1},"unresolved_asks":7}}`,
			// The folded-key check is per phase, against the names that phase's struct
			// actually decodes. `Agents` on a PLAN row folds onto nothing Plan reads, and
			// the Python ignores it too, so refusing it would be over-firing.
			`{"run_id":"r","phase":"plan","planned_at":"2026-01-01T00:00:00","repo":"x","Agents":99}`,
			// Unmodelled keys that merely resemble record fields are not folded names. Both
			// of these are run-level keys runlog WRITES and nothing here decodes — see the
			// `runLevelFields` deferral, which lists the five still outstanding. This case
			// previously used `TIER_EXECUTED`, and it stopped being valid the moment the
			// report slice taught Finish to read `tier_executed`: the guard then refused it,
			// correctly, and the FIXTURE was what had gone stale. Any key chosen here is
			// one slice away from the same fate, which is the cost of asserting a negative
			// over a field list that grows.
			`{"run_id":"r","phase":"finish","outcome":"clean","unresolved_asks":0,"Finished_At":"x","SESSION_ID":"s1"}`,
		} {
			runs, err := Load(store(t, plan40,
				`{"run_id":"r","phase":"cycle","n":1,"applied":0,"asked":0,"agents":2}`,
				ok), 0)
			if err != nil {
				t.Fatalf("Load must not fail: %v", err)
			}
			if _, cErr := runs["r"].Convergence(); cErr != nil {
				t.Errorf("row %s was refused: %v — a row carrying nothing a derivation reads must be ignored, as the Python's merge effectively does", ok, cErr)
			}
		}
	})

	t.Run("the phase switch matches exactly, with no folding", func(t *testing.T) {
		// Python compares `rec.get("phase") == "cycle"` exactly and MERGES anything else, so
		// a "Cycle" row never joins `cycles`. Case-folding or trimming the phase here would
		// append it, and a zero-fix "Cycle" row appended after a cycle that applied five
		// fixes reads converged — another forgery, and one the field check cannot see
		// because `applied` is a cycle-owned field that load() never merges.
		for _, variant := range []string{"Cycle", "CYCLE", " cycle", "cycle "} {
			runs, err := Load(store(t, plan40,
				`{"run_id":"r","phase":"cycle","n":1,"applied":5,"asked":0,"agents":2}`,
				`{"run_id":"r","phase":"`+variant+`","n":2,"applied":0,"asked":0,"agents":2}`), 0)
			if err != nil {
				t.Fatalf("Load must not fail: %v", err)
			}
			if got := mustConv(t, runs["r"]); got == Converged {
				t.Errorf("phase %q was treated as a cycle row and appended; the Python merges it instead, so the last cycle still applied 5 and the answer is %q", variant, Halted)
			}
		}
	})

	t.Run("a run known only by a nudge exists and has no cycles", func(t *testing.T) {
		// The code comment asserts this parity and no fixture carried a nudge row, though
		// the real store has 17 of them. Inserting a `default: continue` before the run is
		// created survives the suite and makes the run vanish — which matters because
		// review-stats' abandonment accounting keys on a plan with no finish, so a run that
		// disappears changes a number rather than erroring.
		runs, err := Load(store(t,
			`{"run_id":"r","phase":"nudge","nudged_at":"2026-01-01T00:00:00"}`), 0)
		if err != nil {
			t.Fatalf("Load must not fail on a nudge-only store: %v", err)
		}
		run := runs["r"]
		if run == nil {
			t.Fatal("a run known only by a nudge must still exist, as runlog.load's setdefault does")
		}
		if len(run.Cycles) != 0 {
			t.Errorf("got %d cycle(s), want 0: a nudge row carries no cycle", len(run.Cycles))
		}
		if got := mustConv(t, run); got != Unknown {
			t.Errorf("got %q, want %q for a run with no cycle rows", got, Unknown)
		}
	})

	t.Run("an undecodable run reports Unknown alongside its error", func(t *testing.T) {
		// `return Unknown, r.Err` -> `return Converged, r.Err` survives, because both error
		// paths assert only that err != nil and never look at the value returned with it.
		// The file's own reasoning is the argument for pinning it: Unknown is the zero value
		// on purpose, so a caller that reads the string and drops the error still gets "did
		// not converge" rather than a clean result.
		runs, err := Load(store(t,
			`{"run_id":"r","phase":"plan","planned_at":"2026-01-01T00:00:00","repo":"x","agent_cap":40}`,
			`{"run_id":"r","phase":"cycle","n":1,"applied":"not a number","agents":2}`), 0)
		if err != nil {
			t.Fatalf("Load must not fail on a store containing one bad row: %v", err)
		}
		got, cErr := runs["r"].Convergence()
		if cErr == nil {
			t.Fatal("an undecodable run must not derive a verdict")
		}
		if got != Unknown {
			t.Errorf("got %q alongside the error, want %q: a caller that drops the error must still read 'did not converge'", got, Unknown)
		}
	})
}

// keysOf names the runs in a failure message, since the map's own order is random.
func keysOf(runs map[string]*Run) []string {
	out := make([]string, 0, len(runs))
	for k := range runs {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// The operator message is the product surface of the typed boundary: on an append-only store
// with no repair tooling, what it names IS the next action. Every part of it survived a
// mutation when it was first written — the line number could be replaced with 0, the phase
// word dropped, the accumulation reverted to keep-first, and nameBadGates could name every
// gate on the row instead of the bad one, all with the suite green. Each part gets an
// assertion here.
// plan40 is a minimal valid plan row with a cap no fixture reaches, so a store built on it
// exercises the cycle and finish rules without the cap rule interfering.
const plan40 = `{"run_id":"r","phase":"plan","planned_at":"2026-01-01T00:00:00","repo":"x","agent_cap":40}`

func TestTheDecodeErrorNamesWhatTheOperatorMustGoFix(t *testing.T) {
	t.Run("the file line number, so the row can be found", func(t *testing.T) {
		// `lineNo := 0` survived everything. The number has to be the line you can
		// `sed -n Np` out of the real file, which is why it is computed before the tail
		// slice rather than after.
		runs, err := Load(store(t, plan40,
			`{"run_id":"r","phase":"cycle","n":1,"applied":0,"agents":2}`,
			`{"run_id":"r","phase":"cycle","n":2,"applied":"not a number","agents":2}`), 0)
		if err != nil {
			t.Fatalf("Load must not fail on a store with one bad row: %v", err)
		}
		if got := runs["r"].Err.Error(); !strings.Contains(got, "line 3") {
			t.Errorf("the error must name line 3, the file line holding the bad row; got %v", got)
		}
	})

	t.Run("the line number survives the tail window", func(t *testing.T) {
		// The window case is the one that motivated computing `dropped` before the slice:
		// an index relative to the window names the wrong row in the file.
		runs, err := Load(store(t, plan40,
			`{"run_id":"r","phase":"cycle","n":1,"applied":0,"agents":2}`,
			`{"run_id":"r","phase":"cycle","n":2,"applied":0,"agents":2}`,
			`{"run_id":"r","phase":"cycle","n":3,"applied":"not a number","agents":2}`), 2)
		if err != nil {
			t.Fatalf("Load must not fail on a windowed store with one bad row: %v", err)
		}
		if got := runs["r"].Err.Error(); !strings.Contains(got, "line 4") {
			t.Errorf("the error must name line 4 of the FILE, not the window; got %v", got)
		}
	})

	t.Run("the phase, so the operator knows which row shape to read", func(t *testing.T) {
		runs, err := Load(store(t, plan40,
			`{"run_id":"r","phase":"cycle","n":1,"applied":0,"agents":2}`,
			`{"run_id":"r","phase":"finish","unresolved_asks":"7"}`), 0)
		if err != nil {
			t.Fatalf("Load must not fail: %v", err)
		}
		if got := runs["r"].Err.Error(); !strings.Contains(got, "(finish row)") {
			t.Errorf("the error must name the phase; got %v", got)
		}
	})

	t.Run("every bad row, not just the first", func(t *testing.T) {
		// Reverting setErr to keep-first scored green: no fixture had two bad rows. The
		// measured consequence was that `n` — which nothing derives from — appeared in the
		// message while `unresolved_asks` stayed hidden, costing a second hand-edit.
		runs, err := Load(store(t, plan40,
			`{"run_id":"r","phase":"cycle","n":1,"applied":"not a number","agents":2}`,
			`{"run_id":"r","phase":"finish","unresolved_asks":"7"}`), 0)
		if err != nil {
			t.Fatalf("Load must not fail: %v", err)
		}
		got := runs["r"].Err.Error()
		for _, want := range []string{"applied", "unresolved_asks"} {
			if !strings.Contains(got, want) {
				t.Errorf("the error must name %q: a hidden second bad field is a second round trip; got %v", want, got)
			}
		}
	})

	t.Run("only the gate that failed, out of many", func(t *testing.T) {
		// Naming EVERY gate on the row instead of the bad one scored green, because every
		// gate fixture in `refused` holds exactly one gate — so the function's own
		// motivating example (eight gates, one bad status) was unasserted.
		runs, err := Load(store(t,
			`{"run_id":"r","phase":"plan","planned_at":"2026-01-01T00:00:00","repo":"x","gates":{"g1":{"planned":"run"},"g2":{"planned":"run"},"g3":{"planned":"run"}}}`,
			`{"run_id":"r","phase":"finish","executed":{"g1":{"status":"done"},"g2":{"status":0},"g3":{"status":"done"}}}`), 0)
		if err != nil {
			t.Fatalf("Load must not fail: %v", err)
		}
		got := runs["r"].Err.Error()
		if !strings.Contains(got, `"g2"`) {
			t.Errorf("the error must name g2, the gate that failed; got %v", got)
		}
		for _, innocent := range []string{`"g1"`, `"g3"`} {
			if strings.Contains(got, innocent) {
				t.Errorf("the error names %s, which decoded fine — an innocent gate sends the operator to the wrong row; got %v", innocent, got)
			}
		}
	})

	t.Run("a gate name cannot forge a line of the message", func(t *testing.T) {
		// Writer-reachable: cmd_finish validates the container and demands a reason, and
		// checks neither the gate NAME nor the status type. A name holding ESC+CR erases the
		// line it prints on and reprints whatever follows; a name holding a newline forges a
		// whole second entry, because errors.Join already separates with \n.
		esc := "\x1b[2K\rGATE OK: converged"
		nl := "x\nrun \"other\": line 1 (finish row) has an unreadable field: forged"
		row := map[string]any{
			"run_id": "r", "phase": "finish",
			"executed": map[string]any{
				esc: map[string]any{"status": 0}, nl: map[string]any{"status": 0},
				"": map[string]any{"status": 0}, "comma, name": map[string]any{"status": 0},
			},
		}
		b, err := json.Marshal(row)
		if err != nil {
			t.Fatal(err)
		}
		runs, err := Load(store(t,
			`{"run_id":"r","phase":"plan","planned_at":"2026-01-01T00:00:00","repo":"x","gates":{"g":{"planned":"run"}}}`,
			string(b)), 0)
		if err != nil {
			t.Fatalf("Load must not fail: %v", err)
		}
		got := runs["r"].Err.Error()
		for _, b := range []struct {
			name string
			c    byte
		}{{"ESC", 0x1b}, {"CR", '\r'}} {
			if strings.IndexByte(got, b.c) >= 0 {
				t.Errorf("a raw %s byte reached the operator message, so a gate name can rewrite the line it prints on: %q", b.name, got)
			}
		}
		// One newline per joined error is this message's own structure; a gate name must not
		// be able to add one. Two bad rows would give two lines, so count against one row.
		if n := strings.Count(got, "\n"); n != 0 {
			t.Errorf("a gate name added %d newline(s), which forges an entry indistinguishable from a real one: %q", n, got)
		}
		if strings.Contains(got, "gate(s) :") || strings.Contains(got, "gate(s) ,") {
			t.Errorf("an empty gate name rendered as a bare name; it must render as %q: %v", `""`, got)
		}
	})

	t.Run("no gate is blamed for a failure elsewhere in the row", func(t *testing.T) {
		// nameBadGates wrapped unconditionally when written, so this row rendered as
		// `executed gate(s) "g": <an error about unresolved_asks>`. The "X: Y" form asserts
		// Y is why X, and it is not: the operator is told to fix a gate while the field they
		// must edit sits elsewhere in the same sentence. Both fields really are bad here,
		// which is why this is a false RELATION rather than over-firing, and why removing
		// the guard broke no other test.
		runs, err := Load(store(t,
			`{"run_id":"r","phase":"plan","planned_at":"2026-01-01T00:00:00","repo":"x","gates":{"g":{"planned":"run"}}}`,
			`{"run_id":"r","phase":"finish","unresolved_asks":"7","executed":{"g":{"status":0}}}`), 0)
		if err != nil {
			t.Fatalf("Load must not fail: %v", err)
		}
		got := runs["r"].Err.Error()
		if strings.Contains(got, "unresolved_asks") && strings.Contains(got, `gate(s) "g":`) {
			t.Errorf("the message names a gate as the cause of an unresolved_asks failure, which sends the operator to the wrong field; got %v", got)
		}
	})

	t.Run("one row's gate names are bounded too", func(t *testing.T) {
		// The row cap below bounds how many ROWS contribute; these bound what ONE row can
		// contribute. Removing either cap left the suite green: the 200-row case covers
		// errors.Join, not the names.
		executed := map[string]any{}
		for i := 0; i < 50; i++ {
			executed[fmt.Sprintf("%s-%02d", strings.Repeat("g", 200), i)] = map[string]any{"status": 0}
		}
		b, err := json.Marshal(map[string]any{"run_id": "r", "phase": "finish", "executed": executed})
		if err != nil {
			t.Fatal(err)
		}
		runs, err := Load(store(t,
			`{"run_id":"r","phase":"plan","planned_at":"2026-01-01T00:00:00","repo":"x","gates":{"g":{"planned":"run"}}}`,
			string(b)), 0)
		if err != nil {
			t.Fatalf("Load must not fail: %v", err)
		}
		got := runs["r"].Err.Error()
		if n := len(got); n > 2048 {
			t.Errorf("one row produced a %d-byte message; 50 gates of 200-char names must be capped, not copied", n)
		}
		if !strings.Contains(got, "and 42 more") {
			t.Errorf("the cap must say how many gates it elided, or it hides the scale; got %v", got)
		}
		if !strings.Contains(got, "...") {
			t.Errorf("an over-long gate name must be truncated visibly; got %v", got)
		}
	})

	t.Run("a misplaced field names its own owner, in a fixed order", func(t *testing.T) {
		// The message used to report runLevelFields[bad[0]] as the owner of the whole list,
		// so a nudge row carrying all four was told "only a plan row may set" — wrong for
		// two of them, and the singular phrasing read as covering all four. sort.Strings
		// made that deterministically the alphabetically-first field's owner, which is
		// arbitrary rather than informative. Three mutations in that half survived: dropping
		// the sort, joining only bad[0], and taking bad[len(bad)-1].
		runs, err := Load(store(t,
			`{"run_id":"r","phase":"plan","planned_at":"2026-01-01T00:00:00","repo":"x"}`,
			`{"run_id":"r","phase":"cycle","n":1,"applied":0,"asked":0,"agents":2}`,
			`{"run_id":"r","phase":"nudge","agent_cap":1,"executed":{},"gates":{},"unresolved_asks":7}`), 0)
		if err != nil {
			t.Fatalf("Load must not fail: %v", err)
		}
		got := runs["r"].Err.Error()
		// Each field with its own owner, and `cycles` would say no row may set it at all.
		for _, want := range []string{
			"agent_cap (only a plan row may set it)",
			"executed (only a finish row may set it)",
			"gates (only a plan row may set it)",
			"unresolved_asks (only a finish row may set it)",
		} {
			if !strings.Contains(got, want) {
				t.Errorf("the error must say %q; naming one owner for the whole list is wrong for half of them. got %v", want, got)
			}
		}
		// Sorted, because Go map order is random and an error message must not be. Asserted
		// over repeated loads and against the whole expected sequence, not two positions:
		// with four fields a random permutation is already sorted about 4% of the time, so
		// the single-comparison version passed in CI while failing locally — a guard that
		// fires 96% of the time reads as a flaky catalog entry rather than the unguarded
		// rule it is. Ten loads put a chance pass at (1/24)^10.
		want := "agent_cap (only a plan row may set it), executed (only a finish row may set it), " +
			"gates (only a plan row may set it), unresolved_asks (only a finish row may set it)"
		for i := 0; i < 10; i++ {
			runs, err := Load(store(t,
				`{"run_id":"r","phase":"plan","planned_at":"2026-01-01T00:00:00","repo":"x"}`,
				`{"run_id":"r","phase":"cycle","n":1,"applied":0,"asked":0,"agents":2}`,
				`{"run_id":"r","phase":"nudge","agent_cap":1,"executed":{},"gates":{},"unresolved_asks":7}`), 0)
			if err != nil {
				t.Fatalf("Load must not fail: %v", err)
			}
			if g := runs["r"].Err.Error(); !strings.Contains(g, want) {
				t.Fatalf("load %d: the fields are not in a fixed order, so the same bad row prints differently run to run.\nwant the sequence: %s\ngot: %v", i, want, g)
			}
		}
	})

	t.Run("a phase cannot forge a line of the message either", func(t *testing.T) {
		// The third place this class turned up: run_id, then gate names, now phase. Each is
		// writer-supplied and each reaches a message a human reads to decide whether a push
		// is safe, so each needs %q rather than %s. Checked here rather than assumed,
		// because the previous two were both found by review AFTER being written.
		//
		// The case and whitespace variants matter for a different reason: Python compares
		// `rec.get("phase") == "cycle"` exactly and merges anything else, so "Finish" and
		// " finish " are rows whose fields reach the Python's verdict. They must not be
		// silently dropped here.
		for _, ph := range []any{
			"\x1b[2K\rPHASE OK: converged",
			"x\nrun \"other\": line 1 (finish row) has an unreadable field: forged",
			7, nil, []any{1}, "", "Finish", " finish ",
		} {
			b, err := json.Marshal(map[string]any{"run_id": "r", "phase": ph, "unresolved_asks": 7})
			if err != nil {
				t.Fatal(err)
			}
			runs, err := Load(store(t, plan40,
				`{"run_id":"r","phase":"cycle","n":1,"applied":0,"asked":0,"agents":2}`,
				string(b)), 0)
			if err != nil {
				t.Fatalf("Load must not fail: %v", err)
			}
			got, cErr := runs["r"].Convergence()
			if cErr == nil || got == Converged {
				t.Errorf("phase %#v produced %q (err=%v); a row whose phase cannot be placed may carry outstanding work", ph, got, cErr)
				continue
			}
			msg := runs["r"].Err.Error()
			if strings.IndexByte(msg, 0x1b) >= 0 || strings.IndexByte(msg, '\r') >= 0 {
				t.Errorf("phase %#v put a raw control byte in the operator message: %q", ph, msg)
			}
			if n := strings.Count(msg, "\n"); n != 0 {
				t.Errorf("phase %#v added %d newline(s), forging an entry: %q", ph, n, msg)
			}
		}
		// And bounded, which the gate-name caps did not cover: a 1 MiB phase produced a
		// 4 MiB message, and twenty such rows produced 80 MiB. Third instance of the same
		// omission in this function's neighbourhood.
		b, err := json.Marshal(map[string]any{
			"run_id": "r", "phase": strings.Repeat("p", 1<<20), "unresolved_asks": 7})
		if err != nil {
			t.Fatal(err)
		}
		runs, err := Load(store(t, plan40,
			`{"run_id":"r","phase":"cycle","n":1,"applied":0,"asked":0,"agents":2}`,
			string(b)), 0)
		if err != nil {
			t.Fatalf("Load must not fail: %v", err)
		}
		if n := len(runs["r"].Err.Error()); n > 2048 {
			t.Errorf("a 1 MiB phase produced a %d-byte message; it must be capped, not copied", n)
		}
	})

	t.Run("a corrupt row cannot return a message too large to print", func(t *testing.T) {
		// Both dimensions were unbounded: errors.Join over rows, and gate names copied
		// verbatim. Measured at 1.3 MB for 10,000 bad rows and 13 MB for 200 rows of 64 KiB
		// names, on a channel whose whole purpose is to be read by a person.
		rows := []string{plan40}
		for i := 0; i < 2000; i++ {
			rows = append(rows, fmt.Sprintf(
				`{"run_id":"r","phase":"cycle","n":%d,"applied":"%s","agents":2}`, i, strings.Repeat("x", 4096)))
		}
		runs, err := Load(store(t, rows...), 0)
		if err != nil {
			t.Fatalf("Load must not fail: %v", err)
		}
		if n := len(runs["r"].Err.Error()); n > 64*1024 {
			t.Errorf("the error is %d bytes; a corrupt store must not produce a message nobody can print", n)
		}
		// The TOTAL, not the cap. An earlier version printed maxRunErrs, which made the
		// whole error byte-identical for 200, 2,000 and 10,000 bad rows while its comment
		// claimed the count could not be hidden.
		if got := runs["r"].Err.Error(); !strings.Contains(got, "2000 rows did not decode in total") {
			t.Errorf("the cap must report how many rows actually failed, not how many it listed; got %v", got)
		}
	})
}

func disclosureOf(t *testing.T, rows ...string) string {
	t.Helper()
	runs, err := Load(store(t, rows...), 0)
	if err != nil {
		t.Fatal(err)
	}
	r := runs["r"]
	if r == nil {
		r = &Run{ID: "r"}
	}
	got, err := r.Disclosure()
	if err != nil {
		t.Fatalf("Disclosure: unexpected decode error: %v", err)
	}
	return got
}

// The tail every non-converged disclosure ends with. A constant here and a literal in
// record.go on purpose: asserting the sentence against a reference to the code that builds
// it would pass whatever that code said.
const moreLikely = ". The loop had not stopped finding things — another cycle would likely find more."

func TestDisclosureRules(t *testing.T) {
	const cap8 = `{"run_id":"r","phase":"plan","planned_at":"2026-01-01T00:00:00","repo":"x","agent_cap":8}`
	for _, c := range []struct {
		name string
		want string
		rows []string
	}{
		// A converged run owes nothing, and the empty string is the whole signal: the
		// subcommand turns it into a JSON null, and Step 14 pushes with no disclosure.
		{"a converged run owes no disclosure", "", []string{plan40,
			`{"run_id":"r","phase":"cycle","n":1,"applied":0,"agents":3}`}},

		// Omitting the cycle rows is the cheapest state to produce, so it must buy the
		// LOUDEST line rather than silence. The push still happens; what it owes is this.
		{"a run with no cycles discloses that nothing is known",
			"Review completeness UNKNOWN: this run recorded no cycles, so nothing can say " +
				"whether the loop still had findings when it stopped. Treat as unreviewed.",
			[]string{plan40}},
		{"a run absent from the store discloses the same",
			"Review completeness UNKNOWN: this run recorded no cycles, so nothing can say " +
				"whether the loop still had findings when it stopped. Treat as unreviewed.",
			nil},

		// The cap head names the RECORDED cap, not the spend. Printing the spend twice
		// renders "CAPPED at 9 of 9 agents", which reads as a budget met exactly rather
		// than one overshot by a cycle — and the overshoot is the number that says the
		// fan-out width, not the cap, is what bound the run.
		{"the capped head names the spend and the recorded cap",
			"Review CAPPED at 9 of 8 agents: the last cycle applied 3 fix(es)" + moreLikely,
			[]string{cap8, `{"run_id":"r","phase":"cycle","n":1,"applied":3,"agents":9}`}},

		// The halted head counts CYCLES, and the count is of the whole run, not of the
		// last cycle's `n` — which nothing reads, and which a Step 13 restart resets to 1.
		{"the halted head counts every cycle and every agent",
			"Review HALTED after 3 cycle(s), 10 agents: the last cycle applied 2 fix(es)" + moreLikely,
			[]string{plan40,
				`{"run_id":"r","phase":"cycle","n":1,"applied":5,"agents":4}`,
				`{"run_id":"r","phase":"cycle","n":1,"applied":1,"agents":4}`,
				`{"run_id":"r","phase":"cycle","n":2,"applied":2,"agents":2}`}},

		// The asks clause, from the cycle row.
		{"asks on the last cycle are disclosed",
			"Review HALTED after 1 cycle(s), 4 agents: the last cycle applied 2 fix(es)" +
				" and left 3 finding(s) awaiting a decision" + moreLikely,
			[]string{plan40, `{"run_id":"r","phase":"cycle","n":1,"applied":2,"asked":3,"agents":4}`}},

		// And the fallback: asks recorded only at FINISH still reach the line. This is the
		// same channel the convergence check reads — a cycle that routed findings to the
		// user and recorded them nowhere but the finish row has not run out of findings.
		{"asks recorded only at finish are disclosed",
			"Review HALTED after 1 cycle(s), 4 agents: the last cycle applied 2 fix(es)" +
				" and left 7 finding(s) awaiting a decision" + moreLikely,
			[]string{plan40, `{"run_id":"r","phase":"cycle","n":1,"applied":2,"agents":4}`,
				`{"run_id":"r","phase":"finish","outcome":"clean","unresolved_asks":7}`}},
		// Zero FALLS THROUGH to the run-level total, which is the Python's `or`. Reading
		// the cycle's 0 as the answer hid seven outstanding findings behind a recorded
		// zero — and a cycle row carrying `asked: 0` is what every cycle writes.
		{"a zero cycle count falls through to the run total",
			"Review HALTED after 1 cycle(s), 4 agents: the last cycle applied 2 fix(es)" +
				" and left 7 finding(s) awaiting a decision" + moreLikely,
			[]string{plan40, `{"run_id":"r","phase":"cycle","n":1,"applied":2,"asked":0,"agents":4}`,
				`{"run_id":"r","phase":"finish","outcome":"clean","unresolved_asks":7}`}},
		// The cycle's count wins when it is non-zero, so a stale run-level total cannot
		// overwrite the live one.
		{"a non-zero cycle count is not overridden by the run total",
			"Review HALTED after 1 cycle(s), 4 agents: the last cycle applied 2 fix(es)" +
				" and left 3 finding(s) awaiting a decision" + moreLikely,
			[]string{plan40, `{"run_id":"r","phase":"cycle","n":1,"applied":2,"asked":3,"agents":4}`,
				`{"run_id":"r","phase":"finish","outcome":"clean","unresolved_asks":7}`}},
		// A negative count RENDERS rather than being hidden as falsy. Corrupt data the
		// reader can see beats a line that silently drops it.
		{"a negative ask count is disclosed, not swallowed",
			"Review HALTED after 1 cycle(s), 4 agents: the last cycle applied 2 fix(es)" +
				" and left -1 finding(s) awaiting a decision" + moreLikely,
			[]string{plan40, `{"run_id":"r","phase":"cycle","n":1,"applied":2,"asked":-1,"agents":4}`}},

		// The deterministic pass is unfinished work too, and says so in its own clause.
		{"a changed analysis pass is disclosed",
			"Review HALTED after 1 cycle(s), 4 agents: the last cycle applied 2 fix(es)" +
				" and the deterministic pass still had unresolved findings" + moreLikely,
			[]string{plan40,
				`{"run_id":"r","phase":"cycle","n":1,"applied":2,"agents":4,"analysis_changed":true}`}},
		{"both clauses render in order",
			"Review HALTED after 1 cycle(s), 4 agents: the last cycle applied 2 fix(es)" +
				" and left 3 finding(s) awaiting a decision" +
				" and the deterministic pass still had unresolved findings" + moreLikely,
			[]string{plan40,
				`{"run_id":"r","phase":"cycle","n":1,"applied":2,"asked":3,"agents":4,"analysis_changed":true}`}},
		{"a false analysis flag renders no clause",
			"Review HALTED after 1 cycle(s), 4 agents: the last cycle applied 2 fix(es)" + moreLikely,
			[]string{plan40,
				`{"run_id":"r","phase":"cycle","n":1,"applied":2,"agents":4,"analysis_changed":false}`}},

		// An absent `applied` renders "?" and must not render 0: a zero there says the
		// cycle found nothing left to apply, which is the opposite of what absence means
		// and is the exact claim convergence refuses to read out of it.
		{"an absent applied count renders as unknown, not zero",
			"Review HALTED after 1 cycle(s), 4 agents: the last cycle applied ? fix(es)" + moreLikely,
			[]string{plan40, `{"run_id":"r","phase":"cycle","n":1,"agents":4}`}},
		// A cycle with no `agents` spends nothing, matching the Python's `or 0`.
		{"a cycle with no agents field contributes no spend",
			"Review HALTED after 2 cycle(s), 4 agents: the last cycle applied 1 fix(es)" + moreLikely,
			[]string{plan40, `{"run_id":"r","phase":"cycle","n":1,"applied":2,"agents":4}`,
				`{"run_id":"r","phase":"cycle","n":2,"applied":1}`}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := disclosureOf(t, c.rows...); got != c.want {
				t.Errorf("Disclosure:\n got %q\nwant %q", got, c.want)
			}
		})
	}
}

// The three answers must agree about which run they describe, because a report rendered
// from one and gated on another is the shape that put a `converged` summary on a halted run.
func TestDisclosureAgreesWithConvergence(t *testing.T) {
	for _, c := range []struct {
		name string
		conv string
		rows []string
	}{
		{"converged", Converged, []string{plan40, `{"run_id":"r","phase":"cycle","n":1,"applied":0,"agents":3}`}},
		{"unknown", Unknown, []string{plan40}},
		{"capped", Capped, []string{
			`{"run_id":"r","phase":"plan","planned_at":"2026-01-01T00:00:00","repo":"x","agent_cap":8}`,
			`{"run_id":"r","phase":"cycle","n":1,"applied":3,"agents":9}`}},
		{"halted", Halted, []string{plan40, `{"run_id":"r","phase":"cycle","n":1,"applied":3,"agents":2}`}},
	} {
		t.Run(c.name, func(t *testing.T) {
			runs, err := Load(store(t, c.rows...), 0)
			if err != nil {
				t.Fatal(err)
			}
			r := runs["r"]
			if r == nil {
				r = &Run{ID: "r"}
			}
			if got := mustConv(t, r); got != c.conv {
				t.Fatalf("Convergence = %q, want %q", got, c.conv)
			}
			disc, err := r.Disclosure()
			if err != nil {
				t.Fatalf("Disclosure: %v", err)
			}
			// Exactly one of the two states, keyed on convergence and nothing else:
			// converged owes silence, everything else owes a line.
			if (c.conv == Converged) != (disc == "") {
				t.Errorf("convergence %q with disclosure %q — converged must owe nothing and "+
					"every other value must owe a line", c.conv, disc)
			}
		})
	}
}

// A decode error must reach the caller instead of a disclosure, the same way it does for
// the other two derivations. A run whose record is unreadable is not a run whose review can
// be summarised, and an empty disclosure here would read as `converged` to Step 14.
func TestDisclosureRefusesAnUnreadableRun(t *testing.T) {
	runs, err := Load(store(t, plan40,
		`{"run_id":"r","phase":"cycle","n":1,"applied":"3","agents":4}`), 0)
	if err != nil {
		t.Fatal(err)
	}
	got, err := runs["r"].Disclosure()
	if err == nil {
		t.Fatalf("Disclosure returned %q for a run with an unreadable cycle row", got)
	}
	if got != "" {
		t.Errorf("Disclosure returned both %q and an error; the text would be rendered", got)
	}
}

// derivations is every method on *Run that answers a question about the run: signature
// `func() (T, error)`, where the error means "this run could not be read, so there is no
// answer". The list is LITERAL and the set is checked against reflection below, in both
// directions — a test that ranges only over what reflection finds would delete its own case
// the moment a derivation was dropped, and one that ranges only over the literal list would
// miss a derivation added without a line here.
//
// This replaces a hand-enumerated pair that sat in two and a half places (the paired check
// in the refusal loop, plus one mutation entry per derivation). At two derivations that was
// adequate and reflection would have been scaffolding ahead of its need; the deferral said
// to write it in the commit that added the third, and Disclosure is the third.
var derivations = []string{"Convergence", "Disclosure", "DroppedGates"}

// refusingDerivations finds them by shape rather than by name, so a derivation added without
// a line in `derivations` is reported rather than silently unguarded.
func refusingDerivations(r *Run) map[string]func() error {
	out := map[string]func() error{}
	rt := reflect.TypeOf(r)
	errType := reflect.TypeOf((*error)(nil)).Elem()
	for i := 0; i < rt.NumMethod(); i++ {
		m := rt.Method(i)
		ft := m.Func.Type()
		// NumIn 1 is the receiver and no arguments; NumOut 2 with an error second.
		if ft.NumIn() != 1 || ft.NumOut() != 2 || ft.Out(1) != errType {
			continue
		}
		fn := m.Func
		out[m.Name] = func() error {
			res := fn.Call([]reflect.Value{reflect.ValueOf(r)})
			if res[1].IsNil() {
				return nil
			}
			return res[1].Interface().(error)
		}
	}
	return out
}

func TestEveryDerivationRefusesAPoisonedRun(t *testing.T) {
	poisoned, err := Load(store(t, plan40,
		`{"run_id":"r","phase":"cycle","n":1,"applied":"3","agents":4}`), 0)
	if err != nil {
		t.Fatal(err)
	}
	bad := poisoned["r"]
	if bad.Err == nil {
		t.Fatal("the fixture decoded cleanly; it is meant to be the unreadable one")
	}

	found := refusingDerivations(bad)
	// Set equality, both directions.
	for _, name := range derivations {
		if _, ok := found[name]; !ok {
			t.Errorf("derivations names %q, which is no longer a func() (T, error) method on *Run", name)
		}
	}
	for name := range found {
		if !contains(derivations, name) {
			t.Errorf("*Run has a derivation %q with no line in `derivations` — it is unguarded "+
				"by this test and probably by the catalog too", name)
		}
	}

	// RED side: every one must refuse rather than answer.
	for name, call := range found {
		if call() == nil {
			t.Errorf("%s returned an answer for a run it could not decode", name)
		}
	}

	// GREEN side, on the same channel: a healthy run must get an answer out of every one of
	// them. Without this, a derivation that ALWAYS errored would pass the loop above, and
	// so would deleting the decode and returning a bare error.
	healthy, err := Load(store(t, plan40,
		`{"run_id":"r","phase":"cycle","n":1,"applied":0,"asked":0,"agents":4}`,
		`{"run_id":"r","phase":"finish","outcome":"clean","executed":{}}`), 0)
	if err != nil {
		t.Fatal(err)
	}
	for name, call := range refusingDerivations(healthy["r"]) {
		if gotErr := call(); gotErr != nil {
			t.Errorf("%s refused a healthy run: %v", name, gotErr)
		}
	}
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

// storePathCases are every shape of a TILDE-EXPANDED `$HOME` and `$REVIEW_LOOP_RUNS` the two
// implementations could answer differently about. All TEN are compared against the oracle by
// TestStorePathMatchesThePython in this package's parity_test.go, which imports runlog under
// each combination and reads its STORE; the expectations here state the answer directly as
// well, because that parity test skips when the Python is unreachable and this must not.
//
// The `~user` arm is deliberately OUT of scope, and this table is therefore not exhaustive
// over every tilde form. expanduser has one (`i = path.find(sep, 1)`; `i != 1` →
// `pwd.getpwnam`) and StorePath short-circuits anything that is not exactly `~` or `~/`-
// prefixed, so they diverge: measured, `REVIEW_LOOP_RUNS=~root/x` reads `~root/x` here —
// relative, resolved against the working directory, never exists — and `/var/root/x` there.
// The bare `~root` and trailing-slash `~root/` forms diverge the same way. Swept 952
// HOME x REVIEW_LOOP_RUNS pairs: 102 diverge and every one of them is a `~` followed by an
// EXISTING user name, which is getpwnam's success arm; a nonexistent user agrees, because
// expanduser returns the path unchanged on KeyError, which is what this function always does.
//
// "Nothing configures a `~user` path" is a JUDGEMENT, not a measurement, and the most plausible
// human spelling is the one it is easiest to overlook: `~narthur/.claude/review-loop/runs.jsonl`
// names the default store exactly, and diverges — Go reads it relative, the Python resolves it.
// A shell expands `~narthur` before the program sees it, so the shape needs an env set without
// one: a quoted export, a JSON `env` block, a plist, a container spec. Weighed and left: the
// consequence is a gate that derives `unknown` for every run off a path that cannot exist, and
// the cheap half of the fix (expanding only `~` and `~/`) is what is already here.
// StorePath's own comment records the exception; nothing configures a `~user` path, and
// implementing getpwnam lookup would be real code for a shape nobody writes. An earlier
// version of this comment claimed the ten were "every shape ... the two implementations could
// answer differently about", which this measurement contradicts.
//
// An earlier version also said "all nine" and named internal/push/parity_test.go, where no
// such test existed — the hand measurement had been done once, in a shell, and written up as
// though it were wired into CI. The parity test above was added to make the claim true rather
// than to soften it.
//
// The reason this matters more than it looks: a gate reading a different store than the oracle
// answers `unknown` for EVERY run, with no error anywhere. `Finish` is nil, so a recorded
// `test-failure` never reaches the blocker — and for a run with NO cycle rows, which is 22 of
// the live store's 41, nothing at all is left standing: the empty record fingerprints as
// `0 cycle(s) · 0 agent(s)`, which is exactly what pr-report.py renders for such a run, so a
// genuine posted report satisfies the gate and the push is GRANTED. Measured end to end.
// unsetHOME removes $HOME for the duration of t and restores it afterwards. Three copies of
// this existed, two of them added in the same cycle, and the thing being hand-rolled is the
// distinction the tests exist to pin: a copy that restores UNCONDITIONALLY sets HOME="", and
// blank-HOME is a different input from absent-HOME in homeDirOrTilde — expanduser branches on
// presence, not value. Both failure modes land in a later test, because the environment is
// process-global.
//
// t.Cleanup rather than defer, and the restore error is checked rather than discarded:
// cleanups run after t.Setenv's own, in reverse order of registration, and a silently failed
// restore leaves every later test in the package running without HOME.
func unsetHOME(t *testing.T) {
	t.Helper()
	prev, had := os.LookupEnv("HOME")
	if err := os.Unsetenv("HOME"); err != nil {
		t.Fatalf("unset HOME: %v", err)
	}
	t.Cleanup(func() {
		if !had {
			return
		}
		if err := os.Setenv("HOME", prev); err != nil {
			t.Errorf("restore HOME to %q: %v", prev, err)
		}
	})
}

var storePathCases = []struct {
	name, home, env, want string
	unsetHome             bool
}{
	{name: "a plain home", home: "/x", want: "/x/.claude/review-loop/runs.jsonl"},
	// expanduser does `userhome.rstrip('/')`, so a trailing slash must not double.
	{name: "a trailing slash on home", home: "/x/", want: "/x/.claude/review-loop/runs.jsonl"},
	// HOME set but BLANK. expanduser branches on `'HOME' not in os.environ`, so a blank value
	// is used as-is and yields an absolute /.claude/... — os.UserHomeDir cannot express this,
	// it errors identically for blank and unset.
	{name: "a blank home is used, not rejected", home: "", want: "/.claude/review-loop/runs.jsonl"},
	{name: "root as home", home: "/", want: "/.claude/review-loop/runs.jsonl"},
	// HOME ABSENT. expanduser falls back to the passwd entry; os/user.Current does the same.
	// Before this was fixed both this case and the blank one returned the literal
	// "~/.claude/review-loop/runs.jsonl", which has no leading slash and so resolved against
	// the working directory.
	{name: "an absent home falls back to the passwd entry", unsetHome: true, want: "$PWDHOME/.claude/review-loop/runs.jsonl"},
	// The env value is expanded too, not just the default.
	{name: "a bare tilde in the env value", home: "/x", env: "~", want: "/x"},
	// expanduser ends with `(userhome + path[i:]) or '/'`, so a bare tilde under a blank HOME
	// falls back to "/" rather than to the empty string. The only input that reaches that
	// fallback, and it was missing from this table until a validation pass went looking for a
	// mutation this table could not catch.
	{name: "a bare tilde with a blank home falls back to root", home: "", env: "~", want: "/"},
	{name: "a tilde path in the env value", home: "/x", env: "~/alt/runs.jsonl", want: "/x/alt/runs.jsonl"},
	{name: "an absolute env value is untouched", home: "/x", env: "/abs/runs.jsonl", want: "/abs/runs.jsonl"},
	{name: "a relative env value is untouched", home: "/x", env: "rel/runs.jsonl", want: "rel/runs.jsonl"},
	// Whitespace, which is the shape the parity harness's TrimSuffix fix is FOR. Reverting
	// that fix to TrimSpace left the whole suite green, because no case carried whitespace —
	// a guard added to make a comment's claim true, with nothing exercising it. StorePath
	// returns the env value verbatim, so the Go answer is the space; TrimSpace on the oracle
	// side alone turned that into a mismatch the code had not caused.
	{name: "a whitespace env value is untouched", home: "/x", env: " ", want: " "},
}

func TestStorePathShapes(t *testing.T) {
	real, err := user.Current()
	if err != nil {
		t.Skip("no passwd entry to resolve an absent HOME against")
	}
	for _, c := range storePathCases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("REVIEW_LOOP_RUNS", c.env)
			// t.Setenv cannot UNSET, and unset is a distinct input here — it is the whole
			// difference between expanduser's two branches — so this case unsets by hand and
			// restores in a defer rather than being skipped for being awkward.
			if c.unsetHome {
				unsetHOME(t)
			} else {
				t.Setenv("HOME", c.home)
			}
			want := strings.ReplaceAll(c.want, "$PWDHOME", real.HomeDir)
			if got := StorePath(); got != want {
				t.Errorf("StorePath = %q, want %q", got, want)
			}
		})
	}
}

// What makes Disclosure's `default:` arm and its capped nil-cap guard unreachable, stated as an
// assertion rather than as a comment. Both were 0% covered and claimed unreachable by prose
// only — the one thing this repo treats as worse than a missing guard, and the file's other
// unreachable branch (misplacedRunField) already records how it was verified.
//
// Two invariants over a generated matrix of plan, cycle and finish shapes:
//   - Convergence answers one of the four declared constants and nothing else, which is what
//     makes the `default:` arm dead.
//   - Capped implies a recorded agent_cap, which is what makes the nil-cap guard dead.
//
// Both are properties of Convergence, so a mutation to Convergence's cap check trips this as
// well as its own entry — which is the point: these two guards in Disclosure are dead only for
// as long as Convergence keeps its end of the contract.
func TestConvergenceAnswersOnlyTheFourWords(t *testing.T) {
	plans := []string{
		`{"run_id":"r","phase":"plan","planned_at":"2026-01-01T00:00:00","repo":"x"}`,
		`{"run_id":"r","phase":"plan","planned_at":"2026-01-01T00:00:00","repo":"x","agent_cap":0}`,
		`{"run_id":"r","phase":"plan","planned_at":"2026-01-01T00:00:00","repo":"x","agent_cap":1}`,
		`{"run_id":"r","phase":"plan","planned_at":"2026-01-01T00:00:00","repo":"x","agent_cap":40}`,
		`{"run_id":"r","phase":"plan","planned_at":"2026-01-01T00:00:00","repo":"x","agent_cap":-1}`,
	}
	cycleSets := [][]string{
		nil,
		{`{"run_id":"r","phase":"cycle","n":1,"applied":0,"asked":0,"agents":2}`},
		{`{"run_id":"r","phase":"cycle","n":1,"applied":3,"agents":9}`},
		{`{"run_id":"r","phase":"cycle","n":1,"applied":0,"asked":1,"agents":2}`},
		{`{"run_id":"r","phase":"cycle","n":1,"agents":4}`},
		{`{"run_id":"r","phase":"cycle","n":1,"applied":0,"asked":0,"agents":2,"analysis_changed":true}`},
		{`{"run_id":"r","phase":"cycle","n":1,"applied":5,"agents":4}`,
			`{"run_id":"r","phase":"cycle","n":2,"applied":0,"asked":0}`},
	}
	finishes := []string{
		"",
		`{"run_id":"r","phase":"finish","outcome":"clean","executed":{}}`,
		`{"run_id":"r","phase":"finish","outcome":"clean","unresolved_asks":0,"executed":{}}`,
		`{"run_id":"r","phase":"finish","outcome":"clean","unresolved_asks":7,"executed":{}}`,
	}
	known := map[string]bool{Unknown: true, Converged: true, Capped: true, Halted: true}
	seen := map[string]int{}
	for _, pl := range plans {
		for _, cy := range cycleSets {
			for _, fi := range finishes {
				rows := append([]string{pl}, cy...)
				if fi != "" {
					rows = append(rows, fi)
				}
				runs, err := Load(store(t, rows...), 0)
				if err != nil {
					t.Fatalf("Load: %v", err)
				}
				r := runs["r"]
				got := mustConv(t, r)
				if !known[got] {
					t.Fatalf("Convergence answered %q, which is not one of the four constants — "+
						"Disclosure's default arm is now reachable and its error is what ships", got)
				}
				seen[got]++
				if got == Capped && (r.Plan == nil || r.Plan.AgentCap == nil) {
					t.Fatalf("Convergence answered capped with no recorded agent_cap (rows %v) — "+
						"Disclosure's nil-cap guard is now reachable", rows)
				}
				// And the pair must agree, which is the consequence both guards exist for.
				if _, dErr := r.Disclosure(); dErr != nil {
					t.Fatalf("Disclosure refused a healthy run (%q, rows %v): %v", got, rows, dErr)
				}
			}
		}
	}
	// The matrix has to actually reach all four, or the invariants above are satisfied
	// vacuously by a matrix that only ever produces one answer.
	for _, w := range []string{Unknown, Converged, Capped, Halted} {
		if seen[w] == 0 {
			t.Errorf("the matrix never produced %q, so this test does not cover it", w)
		}
	}
}

// The passwd-failure branch of homeDirOrTilde, which is otherwise unreachable: a machine with
// no passwd entry for its own uid. It was 0% covered and `return ""` in its place left the
// whole suite green, while its comment claimed the caller's concatenation "rebuilds the
// original string" — a checkable claim with nothing behind it.
//
// expanduser's own behaviour here is to return the path UNEXPANDED when the pwd lookup raises
// KeyError, and that is what the "~" return reproduces.
func TestAnUnresolvableHomeLeavesTheTildeInPlace(t *testing.T) {
	t.Setenv("REVIEW_LOOP_RUNS", "")
	unsetHOME(t)
	saved := userCurrent
	userCurrent = func() (*user.User, error) { return nil, errors.New("no passwd entry") }
	defer func() { userCurrent = saved }()

	// Unexpanded, exactly as written — not "" and not a path rooted anywhere.
	if got, want := StorePath(), "~/.claude/review-loop/runs.jsonl"; got != want {
		t.Errorf("StorePath = %q, want %q — the tilde must survive an unresolvable home, "+
			"because that is what expanduser does with a KeyError from pwd", got, want)
	}
	// And for a bare tilde, where the concatenation is empty before the root fallback.
	t.Setenv("REVIEW_LOOP_RUNS", "~")
	if got := StorePath(); got != "~" {
		t.Errorf("StorePath = %q for a bare tilde with no resolvable home, want %q", got, "~")
	}
}
