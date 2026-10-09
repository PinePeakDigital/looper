package record

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
		// from the writer (`--agents` is required=True) and absent from all 57 real cycle
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

	t.Run("a misspelled phase cannot forge a clean verdict", func(t *testing.T) {
		// The worst hole this branch had, and it was reported once and dismissed as parity.
		// load() merges every non-cycle row field-wise and never checks the word, so a
		// finish row carrying 7 asks outstanding reaches the Python's verdict whether its
		// phase says "finish", "finnish", 7, or nothing at all — halted every time. The Go
		// switch had no arm for the last three, dropped the row, and read the earlier
		// zero-fix cycle as CONVERGED: a clean push, no disclosure, bought by one typo.
		//
		// The dismissal was measured on a CYCLE row, where both sides do read converged
		// because such a row only contributes to `cycles`. Checking the cheaper case and
		// generalising is how a guard ends up asserting a cause nobody verified.
		for _, bad := range []string{
			`{"run_id":"r","phase":"finnish","outcome":"clean","unresolved_asks":7}`,
			`{"run_id":"r","phase":7,"outcome":"clean","unresolved_asks":7}`,
			`{"run_id":"r","outcome":"clean","unresolved_asks":7}`,
		} {
			runs, err := Load(store(t,
				`{"run_id":"r","phase":"plan","planned_at":"2026-01-01T00:00:00","repo":"x","agent_cap":40}`,
				`{"run_id":"r","phase":"cycle","n":1,"applied":0,"asked":0,"agents":2}`,
				bad), 0)
			if err != nil {
				t.Fatalf("Load must not fail: %v", err)
			}
			got, cErr := runs["r"].Convergence()
			if cErr == nil {
				t.Errorf("row %s decoded without an error; a row whose phase cannot be placed may carry outstanding work", bad)
			}
			if got == Converged {
				t.Errorf("row %s produced %q — a clean verdict forged by an unplaceable phase, with 7 asks outstanding", bad, got)
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
func TestTheDecodeErrorNamesWhatTheOperatorMustGoFix(t *testing.T) {
	const plan40 = `{"run_id":"r","phase":"plan","planned_at":"2026-01-01T00:00:00","repo":"x","agent_cap":40}`

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
		if got := runs["r"].Err.Error(); !strings.Contains(got, "more rows did not decode") {
			t.Errorf("the cap must say it elided rows, or it hides the scale of the problem; got %v", got)
		}
	})
}
