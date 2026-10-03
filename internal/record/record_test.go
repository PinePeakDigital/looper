package record

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The parity gate in parity_test.go SKIPS when runlog.py is not reachable, which it will
// not be in this repo's CI. So the behaviours it compares need assertions of their own
// here, stating the expected answer directly. Parity then adds "and the Python agrees"
// on a machine that has both. A gate that can skip is not a gate on its own.

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
	return r.Convergence()
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
		// An empty list is falsy in Python; treating a non-nil empty slice as truthy
		// blocked the converged branch.
		{"an empty asked list is falsy", Converged, []string{plan,
			`{"run_id":"r","phase":"cycle","n":1,"applied":0,"asked":[],"agents":2}`}},
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
	if got := runs["r"].Convergence(); got != Capped {
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
	if got := runs["r"].Convergence(); got != Converged {
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
func TestDroppedGatesCollapsesEveryFalsyStatus(t *testing.T) {
	runs, err := Load(store(t,
		`{"run_id":"r","phase":"plan","planned_at":"2026-01-01T00:00:00","repo":"x","agent_cap":40,"gates":{"g_empty":{"planned":"run"},"g_false":{"planned":"run"},"g_zero":{"planned":"run"},"g_absent":{"planned":"run"}}}`,
		`{"run_id":"r","phase":"finish","outcome":"clean","executed":{"g_empty":{"status":""},"g_false":{"status":false},"g_zero":{"status":0}}}`), 0)
	if err != nil {
		t.Fatalf("store did not load: %v", err)
	}
	got := runs["r"].DroppedGates()
	for _, g := range []string{"g_empty", "g_false", "g_zero", "g_absent"} {
		if got[g] != "unreported" {
			t.Errorf("gate %s reported %q, want \"unreported\" — a falsy status must not reach the caller verbatim", g, got[g])
		}
	}
}

// An over-long line must not invalidate the store either. A bufio.Scanner caps the line
// length and then reports ErrTooLong, which discarded every run already parsed — and it
// fires on the same input the torn-line tolerance exists for, a half-written line from a
// concurrent writer. runlog.py has no ceiling on record size.
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
	if got := runs["r"].Convergence(); got != Converged {
		t.Errorf("convergence = %q, want %q — the over-long row was not used", got, Converged)
	}
}

func TestDroppedGatesTreatsNAAsHandled(t *testing.T) {
	runs, err := Load(store(t,
		`{"run_id":"r","phase":"plan","planned_at":"2026-01-01T00:00:00","repo":"x","gates":{"a":{"planned":"run"},"b":{"planned":"run"},"c":{"planned":"run"},"d":{"planned":"skip"}}}`,
		`{"run_id":"r","phase":"finish","outcome":"clean","executed":{"a":{"status":"done"},"b":{"status":"n/a"},"c":{"status":"skipped"}}}`), 0)
	if err != nil {
		t.Fatal(err)
	}
	got := runs["r"].DroppedGates()
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
	if got := runs["r"].DroppedGates()["e"]; got != "unreported" {
		t.Errorf("an absent entry reported %q, want \"unreported\"", got)
	}
}
