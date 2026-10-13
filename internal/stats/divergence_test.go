package stats

import (
	"bytes"
	"strings"
	"testing"

	"github.com/pinepeakdigital/looper/internal/golden"
)

// The enumerated divergences. Each one reads what review-stats.py did from its frozen golden,
// asserts the golden still records the Python diverging (so an edited golden cannot quietly
// retire a divergence), and asserts what this side does instead. README.md lists them.

// oracleCrashed reports a frozen transcript in which review-stats.py died with a traceback.
// The capture kept only the traceback's last line, the exception, with the store's path
// replaced: the frames name the capturing machine's checkout.
func oracleCrashed(tr string) bool {
	return strings.HasPrefix(tr, "exit=1\n") && strings.Contains(tr, "Error")
}

// base is three finished runs that each left gate g unreported: enough for the alarm to fire,
// so a shape that silences it is visible as silence and not as an empty store.
var base = runs("d", 0, 3, g1)

type shape struct {
	name string
	rows []string
	args []string // added to each mode's own
	// what this side does with it: "unreadable" names the run in both modes; "unattributed"
	// counts the row in the report (it has no run to put in the alarm's window); "torn" skips
	// the row as a torn line is skipped.
	kind string
}

var shapes = []shape{
	// review-stats.py dies on these (TypeError or AttributeError, exit 1) in at least one mode.
	{name: "a planned_at that is a number", kind: "unreadable", rows: []string{
		`{"run_id":"x","phase":"plan","planned_at":5,"session_id":"s1"}`}},
	{name: "gates that are a list", kind: "unreadable", rows: []string{
		`{"run_id":"x","phase":"plan","planned_at":"2026-03-01T00:00:00","session_id":"s1","gates":["g"]}`}},
	{name: "executed that is a list", kind: "unreadable", rows: []string{
		plan("x", "2026-03-01T00:00:00", "s1", "g"), `{"run_id":"x","phase":"finish","outcome":"clean","executed":["g"]}`}},
	{name: "unresolved_asks that is a string", kind: "unreadable", rows: []string{
		plan("x", "2026-03-01T00:00:00", "s1"), `{"run_id":"x","phase":"cycle","n":1,"applied":0,"agents":1}`,
		`{"run_id":"x","phase":"finish","outcome":"clean","unresolved_asks":"7"}`}},
	{name: "cycle agents that is a string", kind: "unreadable", rows: []string{
		plan("x", "2026-03-01T00:00:00", "s1"), `{"run_id":"x","phase":"cycle","n":1,"applied":1,"agents":"4"}`, fin("x", "clean")}},
	{name: "an outcome that is a list", kind: "unreadable", rows: []string{
		plan("x", "2026-03-01T00:00:00", "s1"), `{"run_id":"x","phase":"finish","outcome":["clean"]}`}},
	{name: "an escalation gate that is a list", kind: "unreadable", rows: []string{
		plan("x", "2026-03-01T00:00:00", "s1"), `{"run_id":"x","phase":"finish","outcome":"clean","escalations":[{"gate":["g"]}]}`}},
	{name: "a repo that is a number, filtered", kind: "unreadable", args: []string{"--repo", "one"}, rows: []string{
		`{"run_id":"x","phase":"plan","planned_at":"2026-03-01T00:00:00","session_id":"s1","repo":5}`}},
	{name: "a run_id that is a list", kind: "unattributed", rows: []string{
		`{"run_id":["x"],"phase":"plan","planned_at":"2026-03-01T00:00:00"}`}},
	{name: "a row that is a JSON array", kind: "torn", rows: []string{`["not","a","row"]`}},

	// review-stats.py reads these, coercing the value, where the typed boundary refuses it.
	{name: "a gate status that is a number", kind: "unreadable", rows: []string{
		plan("x", "2026-03-01T00:00:00", "s1", "g"), `{"run_id":"x","phase":"finish","outcome":"clean","executed":{"g":{"status":123,"reason":"r"}}}`}},
	{name: "an abandon reason that is a list", kind: "unreadable", rows: []string{
		plan("x", "2026-03-01T00:00:00", "s1"), `{"run_id":"x","phase":"finish","outcome":"abandoned","abandoned_missing":["g"]}`}},
	{name: "an escalation that is a string", kind: "unreadable", rows: []string{
		plan("x", "2026-03-01T00:00:00", "s1"), `{"run_id":"x","phase":"finish","outcome":"clean","escalations":["security"]}`}},
	{name: "applied that is a string", kind: "unreadable", rows: []string{
		plan("x", "2026-03-01T00:00:00", "s1"), `{"run_id":"x","phase":"cycle","n":1,"applied":"0","agents":1}`, fin("x", "clean")}},
	{name: "a session_kind that is a number", kind: "unreadable", rows: []string{
		`{"run_id":"x","phase":"plan","planned_at":"2026-03-01T00:00:00","session_id":"s1","session_kind":5}`}},
	{name: "an outcome on a plan row", kind: "unreadable", rows: []string{
		`{"run_id":"x","phase":"plan","planned_at":"2026-03-01T00:00:00","session_id":"s1","outcome":"abandoned"}`}},
	{name: "a run_id that is a number", kind: "unattributed", rows: []string{
		`{"run_id":7,"phase":"plan","planned_at":"2026-03-01T00:00:00","session_id":"other"}`}},
}

func TestShapesThisBuildCannotReadAreNamed(t *testing.T) {
	for _, sh := range shapes {
		for _, mode := range []string{"alarm", "report"} {
			args := append([]string{}, sh.args...)
			if mode == "alarm" {
				args = append(args, "--alarm")
			}
			t.Run(sh.name+", "+mode, func(t *testing.T) {
				py := golden.Read(t, "transcript")
				got := runGo(t, scenario{rows: cat(base, sh.rows), args: args, env: s1})
				clean := runGo(t, scenario{rows: base, args: args, env: s1})
				if canon(py) == got {
					t.Fatalf("the frozen oracle now agrees with this side — the golden was edited, and this "+
						"enumerated divergence needs restating or retiring:\n%s", got)
				}
				if !strings.HasPrefix(got, "exit=0\n") {
					t.Errorf("this side did not answer:\n%s", got)
				}
				switch {
				case sh.kind == "torn":
					if got != clean {
						t.Errorf("a row that is not an object changed the answer; it should read as a torn "+
							"line\n--- with it ---\n%s\n--- without it ---\n%s", got, clean)
					}
				case sh.kind == "unattributed" && mode == "alarm":
					// No run to place in the window, so the alarm is what it is without the row.
					if got != clean {
						t.Errorf("--- with it ---\n%s\n--- without it ---\n%s", got, clean)
					}
				case sh.kind == "unattributed":
					if !strings.Contains(got, "1 row(s) carry a run_id that is not a string") {
						t.Errorf("the report does not count the row:\n%s", got)
					}
				case mode == "alarm":
					if !strings.Contains(got, "1 run(s) here could not be read") ||
						!strings.Contains(got, "g did not complete 3x") {
						t.Errorf("the alarm did not name the unreadable run beside the real signal:\n%s", got)
					}
				default:
					if !strings.Contains(got, "unreadable (listed at the end, counted nowhere else): 1") ||
						!strings.Contains(got, "runs this build could not read") || !strings.Contains(got, `run "x"`) {
						t.Errorf("the report did not name the unreadable run:\n%s", got)
					}
				}
			})
		}
	}
}

// A run this build cannot read inside the window is said; one outside it is not, because the
// alarm is about the window and an old unreadable row would otherwise speak at every Step 0.
func TestTheAlarmNamesOnlyUnreadableRunsInItsWindow(t *testing.T) {
	bad := `{"run_id":"bad","phase":"plan","planned_at":"2026-01-01T00:00:00","session_id":"s1","gates":["g"]}`
	inside := runGo(t, scenario{rows: cat([]string{bad}, runs("ok", 0, 49, g1, "g=done")), args: alarmArgs, env: s1})
	want := "exit=0\n--- stdout ---\nreview-loop alarm — over the last 50 runs:\n" +
		"  1 run(s) here could not be read, so no gate above counts them — `looper stats` names them\n" +
		"--- stderr ---\n"
	if inside != want {
		t.Errorf("inside the window:\n%s\nwant:\n%s", inside, want)
	}
	outside := runGo(t, scenario{rows: cat([]string{bad}, runs("ok", 0, 50, g1, "g=done")), args: alarmArgs, env: s1})
	if outside != "exit=0\n--- stdout ---\n--- stderr ---\n" {
		t.Errorf("an unreadable run outside the window spoke:\n%s", outside)
	}
}

// review-stats.py opens the store as strict UTF-8 and dies on one bad byte anywhere in the tail,
// taking the Step 0 alarm down with it. This side reads the byte as U+FFFD, as every looper
// reader does, and answers.
func TestInvalidUTF8IsReadNotFatal(t *testing.T) {
	rows := cat(base, []string{`{"run_id":"u","phase":"plan","planned_at":"2026-03-01T00:00:00","session_id":"s1","tier_reason":"a` + "\xff" + `b"}`})
	for _, mode := range []string{"alarm", "report"} {
		t.Run(mode, func(t *testing.T) {
			py := golden.Read(t, "transcript")
			if !oracleCrashed(py) || !strings.Contains(py, "UnicodeDecodeError") {
				t.Fatalf("the frozen oracle no longer records the crash:\n%s", py)
			}
			var args []string
			if mode == "alarm" {
				args = alarmArgs
			}
			got := runGo(t, scenario{rows: rows, args: args, env: s1})
			want := "g did not complete 3x"
			if mode == "report" {
				want = "runs: 4  finished: 3"
			}
			if !strings.HasPrefix(got, "exit=0\n") || !strings.Contains(got, want) {
				t.Errorf("invalid UTF-8 in the store:\n%s", got)
			}
		})
	}
}

// Only a missing store reads as empty. review-stats.py dies on any other read error with a
// traceback and exit 1; this side exits 1 too, with a sentence. Same status, so plan.py, which
// prints the alarm only on 0, behaves identically — and an unreadable store is never "no runs".
func TestAnUnreadableStoreIsAnError(t *testing.T) {
	for _, mode := range []string{"alarm", "report"} {
		t.Run(mode, func(t *testing.T) {
			py := golden.Read(t, "transcript")
			if !oracleCrashed(py) || !strings.Contains(py, "IsADirectoryError") {
				t.Fatalf("the frozen oracle no longer records the crash:\n%s", py)
			}
			var args []string
			if mode == "alarm" {
				args = alarmArgs
			}
			setEnv(t, s1)
			t.Setenv("REVIEW_LOOP_RUNS", t.TempDir()) // a directory: EISDIR, not ENOENT
			var out, errb bytes.Buffer
			code := Command(args, &out, &errb)
			if code != 1 || out.Len() != 0 || !strings.Contains(errb.String(), "looper stats: cannot read the run store") {
				t.Errorf("exit %d, stdout %q, stderr %q", code, out.String(), errb.String())
			}
		})
	}
}

// review-stats.py iterates a SET of gate names when it collects one run's dropped gates, and a
// set of strings iterates in an order seeded per process. So when two gates are first dropped
// by the same run and tie on their count, which prints first in the report changes from one
// invocation to the next (the parity scenarios are captured under six hash seeds to keep clear
// of it). Nothing can freeze a random order, so this pins the decision instead: by name.
func TestGatesFirstDroppedByOneRunTieByName(t *testing.T) {
	got := runGo(t, scenario{rows: []string{plan("t1", at(1), "s1", "zeta", "alpha", "mid"), fin("t1", "clean")}, env: s1})
	for _, want := range []string{
		"gates planned but not completed:\n    1  alpha\n    1  mid\n    1  zeta\n",
		"    opus: alpha×1, mid×1, zeta×1\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("want %q in:\n%s", want, got)
		}
	}
}

// The alarm's overflow line names the command that lists every reason. The Python names itself,
// which after the switch would send a reader to a script that no longer runs; this names looper.
// The parity gate normalises exactly this string, so this is the test that it is still here.
func TestTheOverflowLinePointsAtLooper(t *testing.T) {
	var rows []string
	for i, why := range []string{"one", "two", "three", "four"} {
		id := "o" + why
		rows = append(rows, plan(id, at(i), "s1", "g"), abandon(id, why))
	}
	got := runGo(t, scenario{rows: rows, args: alarmArgs, env: s1})
	if !strings.Contains(got, "      ...and 1 other reason(s) — `looper stats` has them all\n") {
		t.Errorf("the overflow line:\n%s", got)
	}
}

// Nothing matched the --repo filter: not a headline of zeros and five bare "outcome: " lines,
// which is what a store holding one row with a non-string run_id produced. With such rows in
// the store, "no runs recorded" would be false, so the answer says what is true and names them.
func TestARepoFilterMatchingNothingSaysNoRuns(t *testing.T) {
	rows := []string{`{"run_id":"a","phase":"plan","planned_at":"1","repo":"/r/one","gates":{}}`, `{"run_id":7,"phase":"plan","planned_at":"2","repo":"/r/two"}`}
	got := runGo(t, scenario{rows: rows, args: []string{"--repo", "zzz"}, env: s1})
	if !strings.Contains(got, "\nno runs this build can read\n  1 row(s) carry a run_id that is not a string") || strings.Contains(got, "outcome:") {
		t.Errorf("a filter matching nothing:\n%s", got)
	}
	// And with no filter at all, when those rows are all the store holds: still not "no runs
	// recorded", which would be false.
	got = runGo(t, scenario{rows: rows[1:], env: s1})
	if !strings.Contains(got, "\nno runs this build can read\n  1 row(s)") || strings.Contains(got, "no runs recorded") {
		t.Errorf("a store of unattributable rows only:\n%s", got)
	}
}

// Every run unreadable: the tallies have nothing to count, and an empty "outcome: " line says
// nothing the list of unreadable runs below it does not.
func TestNoEmptyTallyWhenEveryRunIsUnreadable(t *testing.T) {
	rows := []string{`{"run_id":"a","phase":"plan","planned_at":"1","gates":{"g":{"planned":"run","reason":"x"}}}`, `{"run_id":"a","phase":"finish","outcome":"clean","executed":{"g":{"status":"done","reason":5}}}`}
	got := runGo(t, scenario{rows: rows, env: s1})
	if !strings.Contains(got, "unreadable (listed at the end, counted nowhere else): 1") || strings.Contains(got, "outcome:") {
		t.Errorf("every run unreadable:\n%s", got)
	}
}
