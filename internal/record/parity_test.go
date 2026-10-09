package record

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The parity gate. Until runlog.py is retired, the two implementations must not be able to
// disagree: this runs the Python against the same fixture and requires the same answer —
// except where a divergence is recorded as deliberate at the code that causes it.
//
// The typed decode moved every divergence into one place. It used to be four: pyStr's number,
// container, nil and false gaps; num() and asksOutstanding() degrading where the Python raises
// TypeError and dies; and DroppedGates' two type assertions degrading where it raises
// AttributeError. All four helpers are gone, and with them the per-helper reasoning about what
// a wrong-typed value should coerce to. What replaced them is `refused` below: a shape the
// Python coerced into a verdict now fails to decode, and Run.Err carries that to the caller
// instead of an answer. A gap with a written reason is a different thing from one nobody
// noticed, and only the second kind is what this forbids — so the enumeration IS the gate, and
// `refused` has to stay complete or this comment is the lie it used to be.
//
// A port is the one refactor with a free oracle — the thing being replaced still runs — and
// not using it is how a port ships a behaviour change nobody intended. Every fixture below
// is a shape with a reason: most encode an incident recorded in runlog.py's comments.
//
// Skipped, not failed, when the Python is not reachable: this package must build and test
// in a checkout that does not have the skills repo beside it. REVIEW_LOOP_RUNLOG names the
// script; otherwise the default sibling path is tried.
func runlogPath(t *testing.T) string {
	t.Helper()
	if p := os.Getenv("REVIEW_LOOP_RUNLOG"); p != "" {
		if _, err := os.Stat(p); err == nil {
			return p
		}
		t.Fatalf("REVIEW_LOOP_RUNLOG=%s does not exist — set it correctly or unset it", p)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory to look for runlog.py in")
	}
	p := filepath.Join(home, ".claude", "skills", "review-loop", "runlog.py")
	if _, err := os.Stat(p); err != nil {
		t.Skip("runlog.py not found; set REVIEW_LOOP_RUNLOG to run the parity gate")
	}
	return p
}

// lastLine is the last non-blank line of a captured stderr. The verdict word is what the
// Python prints LAST; anything above it is noise from the interpreter, and an equality
// against the whole buffer fails the moment a DeprecationWarning appears at import. That
// brittleness is the same thing separating stderr from stdout was meant to prevent — a
// healthy oracle must not turn the gate red — so the comparisons below read the last line,
// not the buffer.
func lastLine(s string) string {
	fields := strings.Split(strings.TrimSpace(s), "\n")
	// Only the LEFT half of this trim is reachable, and no test can cover the right half:
	// the outer TrimSpace has already removed trailing whitespace from the buffer, so the
	// final field cannot end in any. Narrowing it to TrimLeft therefore survives every
	// mutation and always will. Left as TrimSpace because the symmetry is what a reader
	// expects; labelled because an unreachable half that scores as covered is worse than
	// one that is named.
	return strings.TrimSpace(fields[len(fields)-1])
}

// saysNoSuchRun reports whether cmd_convergence's failure means "that run is not in this
// store", as opposed to any other way it can fail. It takes BOTH signals because each alone
// has been wrong once: the word "unknown" also arrives on STDOUT as the legitimate verdict
// for a run with no cycles, and exit 2 is also argparse's usage-error code, so a renamed
// subcommand reported "no such run" for every fixture.
//
// A function rather than an inline condition so the cases can be asserted directly — see
// TestSaysNoSuchRunReadsBothSignals, which is the only thing that exercises the tolerance
// for noise on stderr, because no fixture produces any.
func saysNoSuchRun(err error, stderr string) bool {
	ee, ok := err.(*exec.ExitError)
	if !ok || ee.ExitCode() != 2 {
		return false
	}
	// Equality, not Contains or HasSuffix. A real argparse failure ends with the word:
	// `convergence --run-id r --unknown` exits 2 with "runlog.py: error: unrecognized
	// arguments: --unknown" as its last line, and a suffix test reports that as a missing
	// run — the wrong-cause failure this guard has already shipped three times.
	return lastLine(stderr) == "unknown"
}

// noRun is what an oracle prints when the fixture holds no run "r". Without it the Python
// half of every comparison degrades silently: `next(..., {})` and `.get('r') or {}` both
// render a missing run as "nothing", so a typo'd run_id or rows that fail to parse compared
// an empty Go answer against an empty Python one and reported PASS. Guarding only the Go
// side — which is all the first fix did — left 23 of 36 DroppedGates subtests able to pass
// having compared nothing, and still counting toward the CI floor.
const noRun = "NO-SUCH-RUN"

// pyOracle runs one Python statement block against a script loaded with runpy and returns
// what it printed. `m` is the script's GLOBALS DICT, not a module — run_path returns the
// executed globals, which is why every call site writes m['load'] and not m.load — and
// `json`, `os`, `sys` are imported. The block must end in a print.
//
// It centralises DETECTION of the noRun sentinel, not its EMISSION: the branch that prints
// the sentinel lives in each caller's body string, so a fourth oracle added through here
// without one reintroduces exactly the vacuity this helper was extracted to fix, with
// nothing red. Copy the shape from a caller below rather than assuming the helper covers it.
//
// runpy rather than an import: review-stats.py's name has a hyphen and is therefore not
// importable, and its own `import runlog` needs the skill directory on sys.path — run_path
// does not put the script's directory there. runlog.py needs neither, but both oracles go
// through here so a third cannot invent a third mechanism.
//
// Output, not CombinedOutput: stderr merged into stdout is parsed as part of the answer, so
// one DeprecationWarning at import turned 72 subtests red across the two tests that had it
// and blamed the comparison for it. Captured separately and reported on failure instead.
func pyOracle(t *testing.T, script, store, body string) string {
	t.Helper()
	cmd := exec.Command("python3", "-c",
		"import json,os,sys,runpy;"+
			"sys.path.insert(0, os.path.dirname(os.path.abspath(sys.argv[1])));"+
			"m=runpy.run_path(sys.argv[1]);"+body, script)
	cmd.Env = append(os.Environ(), "REVIEW_LOOP_RUNS="+store)
	var errOut strings.Builder
	cmd.Stderr = &errOut
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("python side failed: %v\nstderr:\n%s", err, errOut.String())
	}
	got := strings.TrimSpace(string(out))
	if got == noRun {
		t.Fatalf("Python side found no run %q in the fixture", "r")
	}
	return got
}

// refused are the fixtures the TYPED port will not decode, and that is the deliberate
// divergence this port buys. The Python coerces almost every one of them into a verdict: a
// bool or a number where a status string belongs, a bare string where a gate spec or an
// executed entry belongs, a bool, a list or an empty string where a count belongs.
//
// WRITER-REACHABILITY IS SPLIT, and the earlier claim here — "each is a shape no writer
// produces" — was false for four of them. Checked by running the writer, not by reading it:
//
//	runlog.py finish --executed '{"g":{"status":5,"reason":"measured"}}'  -> exits 0, row written
//
// cmd_finish validates the CONTAINER and demands a reason for any status but `done`; it never
// checks the status's type. So `gate false/zero/true/small-integer status` are all rows the
// live writer creates, and the typed port refuses to decode a run containing one. That is a
// real tightening, not just a prose error, and the supporting measurement cited for it was a
// non-sequitur: "every numeric field is a number in every row" says nothing about gate specs,
// executed entries or statuses. What IS true today is that all 302 statuses in the store are
// strings, so nothing is broken yet — and that the remaining ten shapes are unreachable,
// each by its own mechanism rather than by one blanket rule: --applied/--asked/--asks are
// argparse type=int; --analysis-changed is store_true and cmd_cycle writes bool(), so a
// number cannot land there; a bare-string executed entry is refused by cmd_finish's guard
// and a bare-string gate spec by cmd_plan's. The direction is safe for the four reachable
// ones: Python reports the gate dropped (with the raw 5 or True, not a stringified one) and
// Go refuses the run, so neither buys a clean sweep. That is NOT true of all fourteen —
// for a whole-map `gates: {"g":"run"}` the Python returns {} and a clean sweep is exactly
// what it buys, which is why TestACorruptGateRecordCannotReportACleanSweep exists.
//
// `an unreadable asks count` is also not a shape the Python "derived an answer anyway" from:
// `("7" or 0) > 0` raises TypeError and the Python dies. Refusing beats reproducing a crash.
//
// They are out of the parity map because parity is no longer the claim for them. The claim
// is below: the decode fails, and the error names the run and the field so the operator can
// go fix the row instead of acting on a verdict read from it.
var refused = map[string][]string{
	// `asked: {}` and `unresolved_asks: "7"`. Both had unit tests of their own asserting
	// the Python's coercion: an empty dict is falsy so it converged, and an unreadable
	// count was treated as outstanding BECAUSE it could not be read — the Go's OWN policy,
	// not the Python's coercion: on that input the Python raises TypeError and dies. That
	// second rule was
	// the most careful thing in the old port — it is the one guard where a falsy reading
	// waves a run through, so it had to answer "outstanding" whenever it could not tell.
	// Typing retires the rule rather than restating it: there is no unreadable value left
	// to have a policy about.
	"an empty asked dict": {
		`{"run_id":"r","phase":"plan","planned_at":"2026-01-01T00:00:00","repo":"x","agent_cap":40}`,
		`{"run_id":"r","phase":"cycle","n":1,"applied":0,"asked":{},"agents":2}`,
	},
	"an unreadable asks count": {
		`{"run_id":"r","phase":"plan","planned_at":"2026-01-01T00:00:00","repo":"x","agent_cap":40}`,
		`{"run_id":"r","phase":"cycle","n":1,"applied":0,"agents":2}`,
		`{"run_id":"r","phase":"finish","outcome":"clean","unresolved_asks":"7"}`,
	},
	// Python's bool and float arithmetic in the converged test, which `Convergence`'s own
	// comment used to claim exactness over. `False == 0` is True and `0.0 == 0` is True, so
	// each of these derives `converged` on the Python — quiet, push with no disclosure —
	// while the typed port returns a decode error. The retired isZero handled them and said
	// so; this is where that documented behaviour went. None is writer-reachable: --applied,
	// --asked and --analysis-changed are all argparse type=int or a yes/no choice.
	"a bool where a count belongs": {
		`{"run_id":"r","phase":"plan","planned_at":"2026-01-01T00:00:00","repo":"x","agent_cap":40}`,
		`{"run_id":"r","phase":"cycle","n":1,"applied":false,"asked":0,"agents":2}`,
	},
	"a bool where the ask count belongs": {
		`{"run_id":"r","phase":"plan","planned_at":"2026-01-01T00:00:00","repo":"x","agent_cap":40}`,
		`{"run_id":"r","phase":"cycle","n":1,"applied":0,"asked":false,"agents":2}`,
	},
	"a number where the analysis flag belongs": {
		`{"run_id":"r","phase":"plan","planned_at":"2026-01-01T00:00:00","repo":"x","agent_cap":40}`,
		`{"run_id":"r","phase":"cycle","n":1,"applied":0,"asked":0,"analysis_changed":0,"agents":2}`,
	},
	// A JSON number that is not an integer literal. The whole class — `0.0`, `2e1`, `2.5`,
	// and any integer past int64 — and `applied` is only the cheapest place to stand it up;
	// the same holds for every *int field and for agent_cap. This was the refusal class
	// `refused` did not name, which matters precisely because the comment above claims to be
	// the enumeration: a class missing from it is indistinguishable from one nobody noticed.
	//
	// It belongs here rather than in the parity map because `0.0` is the one member where the
	// Python is QUIET and the Go is loud. Python's `0.0 == 0` is True, so it reports converged
	// and push-check pushes with no disclosure at all; `2e1` and `2.5` only reach halted, which
	// discloses anyway. So the entry is pinned to the member with a real cost, not a lateral one.
	//
	// Reachability: zero of the 154 rows in the live store hold a non-integer numeric literal —
	// every numeric field in every row is an integer — and runlog.py's cmd_cycle types
	// `--applied` as int, so argparse rejects `0.0` before it can be written.
	"a numeric field that is not an integer literal": {
		`{"run_id":"r","phase":"plan","planned_at":"2026-01-01T00:00:00","repo":"x","agent_cap":40}`,
		`{"run_id":"r","phase":"cycle","n":1,"applied":0.0,"asked":0,"agents":2}`,
	},
	// An empty list is falsy in Python, so `asked: []` does not block the converged branch.
	"empty-asked-list-is-falsy": {
		`{"run_id":"r","phase":"plan","planned_at":"2026-01-01T00:00:00","repo":"x","agent_cap":40}`,
		`{"run_id":"r","phase":"cycle","n":1,"applied":0,"asked":[],"agents":2}`,
	},
	// An empty `unresolved_asks` is none, exactly as absent is — Python's `"" or 0`. The
	// non-empty-string case cannot live here: the Python raises TypeError on it, so there
	// is no answer to compare. record_test.go states the Go answer directly instead.
	"empty-asks-value-is-not-outstanding": {
		`{"run_id":"r","phase":"plan","planned_at":"2026-01-01T00:00:00","repo":"x","agent_cap":40}`,
		`{"run_id":"r","phase":"cycle","n":1,"applied":0,"agents":3}`,
		`{"run_id":"r","phase":"finish","outcome":"clean","unresolved_asks":""}`,
	},
	// The mirror case on the other side: an executed entry that is a bare string. The
	// isinstance guard means no status is read, so the gate is unreported — NOT "done".
	"executed entry is not a dict": {
		`{"run_id":"r","phase":"plan","planned_at":"2026-01-01T00:00:00","repo":"x","agent_cap":40,"gates":{"g":{"planned":"run"}}}`,
		`{"run_id":"r","phase":"cycle","n":1,"applied":0,"agents":2}`,
		`{"run_id":"r","phase":"finish","outcome":"clean","executed":{"g":"done"}}`,
	},
	// `false` is the same collapse spelled as a boolean, and the direct mirror of the
	// `true` fixture below, which does NOT collapse and so prints its rendering.
	"gate false status is unreported": {
		`{"run_id":"r","phase":"plan","planned_at":"2026-01-01T00:00:00","repo":"x","agent_cap":40,"gates":{"g":{"planned":"run"}}}`,
		`{"run_id":"r","phase":"cycle","n":1,"applied":0,"agents":2}`,
		`{"run_id":"r","phase":"finish","outcome":"clean","executed":{"g":{"status":false,"reason":"r"}}}`,
	},
	// A SMALL INTEGER status. This was the one numeric class where the Go's rendering agreed
	// with the Python's, which is why it alone had a parity fixture while 5.0, 1000000 and any
	// JSON integer past 2^53 had only a prose note at pyStr saying a fixture for them would
	// fail. The typed decode makes that distinction moot: a status is a *string or the row does
	// not decode, so every numeric class including this one is refused on the same ground and
	// none of them needs a rendering to agree about. The narrow name is kept because it still
	// says what the row holds.
	//
	// Reachability: runlog.py's cmd_finish does not validate the status VOCABULARY, though it
	// does refuse a non-dict executed value and require a non-empty reason for any status but
	// done — hence the reason on every row here.
	"gate small-integer status renders as itself": {
		`{"run_id":"r","phase":"plan","planned_at":"2026-01-01T00:00:00","repo":"x","agent_cap":40,"gates":{"g":{"planned":"run"}}}`,
		`{"run_id":"r","phase":"cycle","n":1,"applied":0,"agents":2}`,
		`{"run_id":"r","phase":"finish","outcome":"clean","executed":{"g":{"status":5,"reason":"r"}}}`,
	},
	// A gate spec that is a bare string, not a dict. The Python guards with isinstance,
	// so this is not planned at all; reading `v["planned"]` would raise, and treating
	// the string itself as the plan would make it planned.
	"gate spec is not a dict": {
		`{"run_id":"r","phase":"plan","planned_at":"2026-01-01T00:00:00","repo":"x","agent_cap":40,"gates":{"g":"run"}}`,
		`{"run_id":"r","phase":"cycle","n":1,"applied":0,"agents":2}`,
		`{"run_id":"r","phase":"finish","outcome":"clean","executed":{}}`,
	},
	// `status: true` is the mirror of the false fixture above, and the one case where
	// Python's rendering and Go's are not the same word: str(True) is "True" and
	// fmt.Sprint(true) is "true". Reachable the same way false is — cmd_finish never
	// validates the status — and it is the alarm line's text that differs.
	"gate true status renders as True": {
		`{"run_id":"r","phase":"plan","planned_at":"2026-01-01T00:00:00","repo":"x","agent_cap":40,"gates":{"g":{"planned":"run"}}}`,
		`{"run_id":"r","phase":"cycle","n":1,"applied":0,"agents":2}`,
		`{"run_id":"r","phase":"finish","outcome":"clean","executed":{"g":{"status":true,"reason":"r"}}}`,
	},
	// `0` is the same collapse spelled as a number. Defaulting only on absence reported
	// "0" here, which names nothing about the gate.
	"gate zero status is unreported": {
		`{"run_id":"r","phase":"plan","planned_at":"2026-01-01T00:00:00","repo":"x","agent_cap":40,"gates":{"g":{"planned":"run"}}}`,
		`{"run_id":"r","phase":"cycle","n":1,"applied":0,"agents":2}`,
		`{"run_id":"r","phase":"finish","outcome":"clean","executed":{"g":{"status":0,"reason":"r"}}}`,
	},
}

// The vacuity guards above are tripwires no fixture can trip: every fixture holds run "r",
// so `got == noRun` and the exit-2 check are unreachable, and mutation confirms both survive
// the whole suite. Labelled rather than left to read as coverage.
//
// What IS testable is the half that can change underneath us — whether the Python still
// SIGNALS a missing run at all. A refactor that made review-stats' load() return a default
// run, or dropped cmd_convergence's exit 2, would silence the tripwires without touching Go,
// and nothing else would notice. So this pins the oracles' side of the contract directly.
func TestTheOraclesSayWhenTheyFindNoRun(t *testing.T) {
	script := runlogPath(t)
	stats := reviewStatsPath(t)
	// A store with a run, just not the one anything asks about.
	path := store(t, `{"run_id":"other","phase":"plan","planned_at":"2026-01-01T00:00:00","repo":"x"}`)

	t.Run("review-stats load", func(t *testing.T) {
		got := pyOracle(t, stats, path,
			"run=next((x for x in m['load']() if x.get('run_id')=='r'), None);"+
				"print('ABSENT' if run is None else 'FOUND')")
		if got != "ABSENT" {
			t.Errorf("the oracle no longer distinguishes a missing run: got %q", got)
		}
	})

	t.Run("runlog load", func(t *testing.T) {
		got := pyOracle(t, script, path,
			"print('ABSENT' if m['load'](limit=None).get('r') is None else 'FOUND')")
		if got != "ABSENT" {
			t.Errorf("the oracle no longer distinguishes a missing run: got %q", got)
		}
	})

	t.Run("convergence exits 2", func(t *testing.T) {
		cmd := exec.Command("python3", script, "convergence", "--run-id", "r")
		cmd.Env = append(os.Environ(), "REVIEW_LOOP_RUNS="+path)
		var errOut strings.Builder
		cmd.Stderr = &errOut
		out, err := cmd.Output()
		if !saysNoSuchRun(err, errOut.String()) {
			t.Errorf("cmd_convergence no longer signals a missing run: err=%v, stdout=%q, stderr=%q",
				err, out, errOut.String())
		}
		// And it must not put the word on stdout, where it would be read as a verdict —
		// "unknown" on STDOUT with exit 1 is the legitimate answer for a cycle-less run.
		if strings.TrimSpace(string(out)) != "" {
			t.Errorf("a missing run printed %q to stdout; it belongs on stderr", out)
		}
	})
}

// The healthy-with-noise case, which four review cycles never ran. Every guard in this file
// was validated by simulating the break it names and confirming red; not one was validated
// by perturbing a CORRECT system and confirming green, and that is where each of them failed
// in turn. No fixture puts noise on stderr, so without this nothing exercises the tolerance
// and a return to comparing the whole buffer would score as caught by nothing.
func TestSaysNoSuchRunReadsBothSignals(t *testing.T) {
	// A real *exec.ExitError, since its ExitCode is what the guard reads.
	exitWith := func(code int) error {
		err := exec.Command("sh", "-c", "exit "+strconv.Itoa(code)).Run()
		// sh always honours an explicit exit, so the mismatch branch is a guard against the
		// environment, not a case under test — it fires only if sh is missing or broken.
		if (err == nil) != (code == 0) {
			t.Fatalf("sh exit %d gave err %v", code, err)
		}
		return err
	}
	argparse := "usage: runlog.py [-h] {plan,finish} ...\n" +
		"runlog.py: error: argument cmd: invalid choice: 'convergence'"

	for _, tc := range []struct {
		name   string
		err    error
		stderr string
		want   bool
	}{
		{"exit 2 and the word", exitWith(2), "unknown\n", true},
		{"a warning above the word is still the word", exitWith(2),
			"DeprecationWarning: noise\n\nunknown\n", true},
		{"argparse shares the exit code but not the word", exitWith(2), argparse, false},
		{"the word without the code is a cycle-less run", exitWith(1), "unknown\n", false},
		{"a process that never ran is not a missing run", errors.New("exec: not found"), "", false},
		{"success is not a missing run", nil, "", false},
		{"the word must be the LAST line, not merely present", exitWith(2),
			"unknown\nruntime shutdown error\n", false},
		// The cases below exist because five wrong versions of this guard passed the seven
		// above. Each is a healthy-OTHER-failure: the process failed for a reason that is
		// not a missing run, on the same channel, in a shape a loose test accepts.
		{"a last line that ENDS with the word is not the word", exitWith(2),
			"usage: runlog.py [-h] ...\nrunlog.py: error: unrecognized arguments: --unknown", false},
		{"a last line that STARTS with the word is not the word", exitWith(2),
			"usage: runlog.py [-h] ...\nunknown option --x", false},
		{"nor is any other non-2 code", exitWith(4), "unknown\n", false},
		// A strict PREFIX of the word, which is what separates equality from
		// HasPrefix("unknown", lastLine) — true of every prefix, the empty string included.
		// This case strictly dominates an empty-stderr one: no mutation is killed by that
		// and not by this, so there is only this.
		{"a strict prefix of the word is not the word", exitWith(2), "unk", false},
		// Case, and punctuation. EqualFold and TrimSuffix(".") both passed the table.
		{"the word is case-sensitive", exitWith(2), "UNKNOWN", false},
		{"the word with a full stop is not the word", exitWith(2), "unknown.", false},
		// The WORD-level form of the suffix bug: splitting on spaces and taking the last
		// field. The argparse fixture above ends in "--unknown", whose last FIELD is
		// "--unknown", so it did not catch this — but argparse also produces lines ending in
		// the bare word, and that is a missing-run report for a subcommand typo.
		{"a last field that IS the word is still not the word", exitWith(2),
			"runlog.py: error: argument cmd: invalid choice: unknown", false},
		// TWO consecutive warnings with no blank line between them. The noise case above has
		// a blank middle line, which hides a SplitN(..., 2) weakening of lastLine — and two
		// import warnings on a healthy oracle is the likeliest noise there is.
		{"two warnings above the word are still the word", exitWith(2),
			"DeprecationWarning: a\nUserWarning: b\nunknown\n", true},
		// CRLF, and a non-breaking space. Both trims are TrimSpace, which is unicode-aware;
		// narrowing either to a " \t\n" cutset survives without these.
		{"a CRLF line ending does not hide the word", exitWith(2), "warn\r\nunknown\r\n", true},
		{"unicode whitespace around the word does not hide it", exitWith(2),
			"warn\n\u00a0unknown\u00a0\n", true},
		// And these two keep lastLine honest where it CAN be wrong: a weakening of the outer
		// trim to newlines only breaks the second. The inner trim is a different story —
		// see its own note at lastLine; only its left half is reachable.
		// Padding on a line that is NOT the whole buffer: with only one line the outer trim
		// already strips it, so the inner one is never reached and dropping it survives.
		{"padding around the word is still the word", exitWith(2), "warn\n  unknown  \n", true},
		{"a whitespace-only last line does not hide the word", exitWith(2),
			"unknown\n   \n", true},
		// The same, but whitespace no ASCII cutset covers. A trailing JUNK LINE is the only
		// thing that exercises the OUTER trim: when the junk is on the verdict line itself
		// the inner trim absorbs it, so narrowing the outer one to " \t\n" or " \t\n\r"
		// survives every other case here.
		{"a trailing line of unicode whitespace does not hide the word", exitWith(2),
			"unknown\n\u00a0\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := saysNoSuchRun(tc.err, tc.stderr); got != tc.want {
				t.Errorf("saysNoSuchRun(%v, %q) = %v, want %v", tc.err, tc.stderr, got, tc.want)
			}
		})
	}
}

// refusedNames is what each refused fixture's error must actually NAME. It is a separate map
// keyed identically to `refused`, and the test asserts the two key sets are equal, so adding a
// refusal without saying what its error names fails rather than passing silently.
//
// This exists because the assertions it replaces were vacuous. They were:
//
//	strings.Contains(run.Err.Error(), "run r:")
//	strings.Contains(run.Err.Error(), "unreadable field")
//
// Both are literal substrings of setErr's own format string, so both held for ANY error — the
// %w could be dropped entirely and the test stayed green. The one guard on the product surface
// this commit elevates ("the error must name the run and the field") asserted only that the
// wrapper we wrote was the wrapper we wrote. Naming the field per fixture is the whole point:
// the operator's next action is to go edit that field on that row.
var refusedNames = map[string][]string{
	"an empty asked dict":                            {"asked"},
	"an unreadable asks count":                       {"unresolved_asks"},
	"a bool where a count belongs":                   {"applied"},
	"a bool where the ask count belongs":             {"asked"},
	"a number where the analysis flag belongs":       {"analysis_changed"},
	"a numeric field that is not an integer literal": {"applied"},
	"empty-asked-list-is-falsy":                      {"asked"},
	"empty-asks-value-is-not-outstanding":            {"unresolved_asks"},
	// The gate cases must name the GATE as well as the field. encoding/json never names a map
	// key, so without nameBadGates these said only `executed.status` on a row that can hold
	// eight gates.
	//
	// Two of them want `Finish.executed` / `Plan.gates` rather than the bare field name, and
	// that is not cosmetic. nameBadGates' message BEGINS with the field — `executed gate(s)
	// "g": …` — so for these two fixtures the field name is a substring of our own wrapper,
	// and wanting it asserted nothing at all. Both subtests stayed GREEN when the json error
	// was stripped of every field name, which is the same vacuity this map was added to fix,
	// one wrapper further down. The struct-qualified path is text only encoding/json emits.
	"executed entry is not a dict":                {"Finish.executed", `gate(s) "g":`},
	"gate false status is unreported":             {"status", `gate(s) "g":`},
	"gate small-integer status renders as itself": {"status", `gate(s) "g":`},
	"gate spec is not a dict":                     {"Plan.gates", `gate(s) "g":`},
	"gate true status renders as True":            {"status", `gate(s) "g":`},
	"gate zero status is unreported":              {"status", `gate(s) "g":`},
}

// The typed boundary, asserted from the other side. Every fixture in `refused` must fail to
// decode, and the error must name the run and the offending field — an error that says only
// "bad input" leaves the operator with a 153-row append-only store and nowhere to look.
//
// This is the test the parity gate cannot be: the oracle's answer for these inputs is a
// coerced verdict, so agreeing with it is the defect. Enumerated here rather than left as an
// absence, because a fixture quietly dropped from the parity map looks identical to one that
// was never written.
func TestTypedDecodeRefusesTheShapesThePythonCoerced(t *testing.T) {
	for name, rows := range refused {
		t.Run(name, func(t *testing.T) {
			runs, err := Load(store(t, rows...), 0)
			if err != nil {
				t.Fatalf("Load should not fail on a decodable store: %v", err)
			}
			run := runs["r"]
			if run == nil {
				t.Fatal("the run must still exist: a bad field is not a missing run")
			}
			if run.Err == nil {
				t.Fatalf("this shape must not decode, but it did\nfixture:\n%s",
					strings.Join(rows, "\n"))
			}
			// The run id, quoted, so an operator with one store and many repos knows which
			// run — and so a crafted id cannot forge a line with control bytes.
			if !strings.Contains(run.Err.Error(), `run "r":`) {
				t.Errorf("the error must name the run as %s; got %v", `run "r":`, run.Err)
			}
			// And what they actually have to go edit. Asserted against the json error's own
			// text, not against our wrapper, which is the part the old assertion could not do.
			wants, ok := refusedNames[name]
			if !ok {
				t.Fatalf("no entry in refusedNames for %q: say what this refusal's error names", name)
			}
			for _, want := range wants {
				if !strings.Contains(run.Err.Error(), want) {
					t.Errorf("the error must name %q, so the operator knows what to fix; got %v", want, run.Err)
				}
			}
			// Both derivations must refuse rather than answer.
			if _, err := run.Convergence(); err == nil {
				t.Error("Convergence returned a verdict for a run it could not decode")
			}
			if _, err := run.DroppedGates(); err == nil {
				t.Error("DroppedGates returned a map for a run it could not decode")
			}
		})
	}
}

// The two maps must describe the same set. A refusal with no expectation would otherwise skip
// the only assertion that is not tautological, and an expectation with no refusal would sit
// there asserting nothing — the shape the repo's own learning warns about, where a test that
// ranges over the list under test deletes its own case when the list shrinks.
func TestEveryRefusalSaysWhatItsErrorNames(t *testing.T) {
	for name := range refused {
		if _, ok := refusedNames[name]; !ok {
			t.Errorf("refused[%q] has no refusedNames entry", name)
		}
	}
	for name := range refusedNames {
		if _, ok := refused[name]; !ok {
			t.Errorf("refusedNames[%q] names a refusal that no longer exists", name)
		}
	}
	if len(refused) < 14 {
		t.Errorf("refused has %d entries, expected at least 14 — was one deleted?", len(refused))
	}
}

// One bad run must not block the others. The store is append-only and shared by every repo
// on the machine, so a row written months ago for some other run has to stay harmless —
// which is the whole reason the error lives on the Run and not on Load.
func TestABadRowDoesNotPoisonOtherRuns(t *testing.T) {
	path := store(t,
		`{"run_id":"bad","phase":"plan","planned_at":"2026-01-01T00:00:00","repo":"x","agent_cap":40}`,
		`{"run_id":"bad","phase":"cycle","n":1,"applied":"not a number","agents":2}`,
		`{"run_id":"good","phase":"plan","planned_at":"2026-01-01T00:00:00","repo":"x","agent_cap":40}`,
		`{"run_id":"good","phase":"cycle","n":1,"applied":0,"agents":2}`,
	)
	runs, err := Load(path, 0)
	if err != nil {
		// Not a bare t.Fatal(err): this is the regression the catalog's
		// record-decode-error-blocks-the-whole-store entry exists to catch, and a raw json
		// error with no statement of the expectation leaves a reader unable to tell whether
		// the error was expected.
		t.Fatalf("Load must not fail on a store that contains one bad row: %v", err)
	}
	// Two distinct regressions, which one message used to collapse: the run being dropped
	// outright, and the run existing with its error swallowed. The old sentence printed
	// neither, so a failure did not say which had happened.
	if runs["bad"] == nil {
		t.Fatalf("the bad run was dropped entirely; runs present: %v", keysOf(runs))
	}
	if runs["bad"].Err == nil {
		t.Fatal("the bad run exists but its error was swallowed, so a verdict could be derived from a row that did not decode")
	}
	if runs["good"] == nil || runs["good"].Err != nil {
		t.Fatalf("the good run must be unaffected; got err %v", runs["good"].Err)
	}
	got, err := runs["good"].Convergence()
	if err != nil || got != Converged {
		t.Errorf("good run: got %q, %v; want %q, nil", got, err, Converged)
	}
}

// fixtures are stores to compare on. Each is a list of JSONL rows.
var fixtures = map[string][]string{
	// A cycle row with NO `applied` field. The Python compares `last.get("applied") == 0`,
	// and `None == 0` is False, so absence does not satisfy the converged branch — this
	// derives halted. Routing it through a helper that maps absent to 0 derived
	// `converged` instead, which is a clean result bought by omitting a field.
	"applied-absent-is-not-zero": {
		`{"run_id":"r","phase":"plan","planned_at":"2026-01-01T00:00:00","repo":"x","agent_cap":40}`,
		`{"run_id":"r","phase":"cycle","n":1,"agents":5}`,
	},
	// `applied: null` is the same case spelled explicitly.
	"applied-null-is-not-zero": {
		`{"run_id":"r","phase":"plan","planned_at":"2026-01-01T00:00:00","repo":"x","agent_cap":40}`,
		`{"run_id":"r","phase":"cycle","n":1,"applied":null,"agents":5}`,
	},
	// Converged wins over capped when BOTH predicates hold: a last cycle that applied
	// nothing, at a spend that also reaches the cap. Every other fixture has only one of
	// the two true, so swapping those two branches passed the whole suite and this gate.
	"converged-beats-capped-at-the-boundary": {
		`{"run_id":"r","phase":"plan","planned_at":"2026-01-01T00:00:00","repo":"x","agent_cap":5}`,
		`{"run_id":"r","phase":"cycle","n":1,"applied":0,"agents":5}`,
	},
	// A negative cap is truthy in Python's bare `if cap`, so any spend clears it.
	// cmd_plan refuses to write one, but the store is append-only and never rewritten.
	"negative-cap-is-still-a-cap": {
		`{"run_id":"r","phase":"plan","planned_at":"2026-01-01T00:00:00","repo":"x","agent_cap":-5}`,
		`{"run_id":"r","phase":"cycle","n":1,"applied":3,"agents":10}`,
	},
	// One agent short of the cap: the other half of the boundary.
	"one-short-of-the-cap": {
		`{"run_id":"r","phase":"plan","planned_at":"2026-01-01T00:00:00","repo":"x","agent_cap":8}`,
		`{"run_id":"r","phase":"cycle","n":1,"applied":1,"agents":7}`,
	},
	// A final zero-fix cycle: the only shape that converges.
	"converged": {
		`{"run_id":"r","phase":"plan","planned_at":"2026-01-01T00:00:00","repo":"x","agent_cap":40}`,
		`{"run_id":"r","phase":"cycle","n":1,"applied":5,"agents":6}`,
		`{"run_id":"r","phase":"cycle","n":2,"applied":0,"agents":2}`,
	},
	// Fixes still landing with budget left.
	"halted": {
		`{"run_id":"r","phase":"plan","planned_at":"2026-01-01T00:00:00","repo":"x","agent_cap":40}`,
		`{"run_id":"r","phase":"cycle","n":1,"applied":5,"agents":6}`,
	},
	// Over the recorded cap with work outstanding.
	"capped": {
		`{"run_id":"r","phase":"plan","planned_at":"2026-01-01T00:00:00","repo":"x","agent_cap":8}`,
		`{"run_id":"r","phase":"cycle","n":1,"applied":3,"agents":9}`,
	},
	// No cycle rows at all: unknown, and every consumer must read it as not converged, so
	// omitting the rows cannot buy a silent clean push.
	"no cycles": {
		`{"run_id":"r","phase":"plan","planned_at":"2026-01-01T00:00:00","repo":"x"}`,
	},
	// applied=0 but the ask bucket is non-empty: not converged. A cycle that routed every
	// finding to the user and resolved none has not run out of findings.
	"zero applied with asks": {
		`{"run_id":"r","phase":"plan","planned_at":"2026-01-01T00:00:00","repo":"x","agent_cap":40}`,
		`{"run_id":"r","phase":"cycle","n":1,"applied":0,"agents":3,"asked":7}`,
	},
	// The omission path: a clean-looking last cycle, with the asks recorded only at finish.
	// This derived `converged` with no disclosure while seven findings sat unresolved.
	"asks only at finish": {
		`{"run_id":"r","phase":"plan","planned_at":"2026-01-01T00:00:00","repo":"x","agent_cap":40}`,
		`{"run_id":"r","phase":"cycle","n":1,"applied":0,"agents":3}`,
		`{"run_id":"r","phase":"finish","outcome":"clean","unresolved_asks":7}`,
	},
	// The deterministic pass changing files is as much unfinished work as a fix bucket.
	"analysis changed files": {
		`{"run_id":"r","phase":"plan","planned_at":"2026-01-01T00:00:00","repo":"x","agent_cap":40}`,
		`{"run_id":"r","phase":"cycle","n":1,"applied":0,"agents":3,"analysis_changed":true}`,
	},
	// A restart resets the counter, so two rows share n=1. Deduping by n deleted the first
	// pass of every restart from the spend: this counted 20 of the 40 actually spent.
	"restart repeats a cycle number": {
		`{"run_id":"r","phase":"plan","planned_at":"2026-01-01T00:00:00","repo":"x","agent_cap":40}`,
		`{"run_id":"r","phase":"cycle","n":1,"applied":2,"agents":20}`,
		`{"run_id":"r","phase":"cycle","n":2,"applied":2,"agents":15}`,
		`{"run_id":"r","phase":"cycle","n":1,"applied":2,"agents":5}`,
	},
	// A torn write never invalidates the rest of the store.
	"torn line": {
		`{"run_id":"r","phase":"plan","planned_at":"2026-01-01T00:00:00","repo":"x","agent_cap":40}`,
		`{"run_id":"r","phase":"cycle","n":1,"applied":0,"agents":2}`,
		`{"run_id":"broke`,
	},
	// A blank line, and a row with no run_id, are both skipped rather than fatal.
	"blank and unidentified rows": {
		`{"run_id":"r","phase":"plan","planned_at":"2026-01-01T00:00:00","repo":"x","agent_cap":40}`,
		``,
		`{"phase":"cycle","n":9,"applied":9,"agents":9}`,
		`{"run_id":"r","phase":"cycle","n":1,"applied":0,"agents":2}`,
	},
	// A cap of 0 or absent must not make every run capped.
	"no cap recorded": {
		`{"run_id":"r","phase":"plan","planned_at":"2026-01-01T00:00:00","repo":"x"}`,
		`{"run_id":"r","phase":"cycle","n":1,"applied":4,"agents":99}`,
	},
	// Spend exactly at the cap is capped, not halted — the boundary.
	"spend exactly at the cap": {
		`{"run_id":"r","phase":"plan","planned_at":"2026-01-01T00:00:00","repo":"x","agent_cap":8}`,
		`{"run_id":"r","phase":"cycle","n":1,"applied":1,"agents":8}`,
	},
	// asked:0 is falsy, so this still converges — distinct from asked:7 above.
	"zero asked is not an ask": {
		`{"run_id":"r","phase":"plan","planned_at":"2026-01-01T00:00:00","repo":"x","agent_cap":40}`,
		`{"run_id":"r","phase":"cycle","n":1,"applied":0,"agents":2,"asked":0}`,
	},
	// --- gate fixtures, for DroppedGates ---------------------------------------
	//
	// `gates` lands on the plan row and `executed` on the finish row, so every one of
	// these needs both. The convergence and cycle-count tests run over them too, which
	// costs nothing and widens their coverage to runs that actually finished.

	// `waived` is NOT in GATE_OK, so a waived gate is still dropped — it reports its
	// status rather than disappearing. runlog.py has a second tuple, GATE_ACCOUNTED,
	// which is GATE_OK plus "waived"; it exists for derive_tier and reading it here
	// would silence the alarm for exactly the gates an operator chose not to run.
	"gate waived is still dropped": {
		`{"run_id":"r","phase":"plan","planned_at":"2026-01-01T00:00:00","repo":"x","agent_cap":40,"gates":{"g":{"planned":"run"}}}`,
		`{"run_id":"r","phase":"cycle","n":1,"applied":0,"agents":2}`,
		`{"run_id":"r","phase":"finish","outcome":"clean","executed":{"g":{"status":"waived","reason":"r"}}}`,
	},
	// done and n/a are the whole of GATE_OK: nothing is dropped.
	"gate done and na are accounted": {
		`{"run_id":"r","phase":"plan","planned_at":"2026-01-01T00:00:00","repo":"x","agent_cap":40,"gates":{"a":{"planned":"run"},"b":{"planned":"run"}}}`,
		`{"run_id":"r","phase":"cycle","n":1,"applied":0,"agents":2}`,
		`{"run_id":"r","phase":"finish","outcome":"clean","executed":{"a":{"status":"done","reason":"r"},"b":{"status":"n/a","reason":"r"}}}`,
	},
	// No executed entry at all: the gate was planned and nobody said anything.
	"gate missing from a present executed map": {
		`{"run_id":"r","phase":"plan","planned_at":"2026-01-01T00:00:00","repo":"x","agent_cap":40,"gates":{"g":{"planned":"run"}}}`,
		`{"run_id":"r","phase":"cycle","n":1,"applied":0,"agents":2}`,
		`{"run_id":"r","phase":"finish","outcome":"clean","executed":{}}`,
	},
	// Every FALSY status collapses to "unreported", not just an absent entry. The Python
	// writes `st or "unreported"`, so "", 0 and false all take the default; defaulting
	// only on absence reports `""`, `"0"` and `"false"`, which is a parity break and a
	// worse alarm line, since the status is what names the gate's problem.
	"gate empty-string status is unreported": {
		`{"run_id":"r","phase":"plan","planned_at":"2026-01-01T00:00:00","repo":"x","agent_cap":40,"gates":{"g":{"planned":"run"}}}`,
		`{"run_id":"r","phase":"cycle","n":1,"applied":0,"agents":2}`,
		`{"run_id":"r","phase":"finish","outcome":"clean","executed":{"g":{"status":"","reason":"r"}}}`,
	},
	// An explicit JSON null, as distinct from the key being absent above: four different
	// Python routes to the same answer, which is the thing worth pinning.
	"gate null status is unreported": {
		`{"run_id":"r","phase":"plan","planned_at":"2026-01-01T00:00:00","repo":"x","agent_cap":40,"gates":{"g":{"planned":"run"}}}`,
		`{"run_id":"r","phase":"cycle","n":1,"applied":0,"agents":2}`,
		`{"run_id":"r","phase":"finish","outcome":"clean","executed":{"g":{"status":null,"reason":"r"}}}`,
	},
	// `passed` and `blocked` are the two statuses most likely to be quietly accounted, and
	// both are deliberately absent from the accounted vocabularies — but for different
	// reasons, and not the same tuple. `passed` was added to GATE_ACCOUNTED and taken back
	// out (runlog.py:466-470, review-stats.py:104-112): it is report-line PROSE for a gate
	// line rather than a status word, and accounting it made the tier read `full` while the
	// alarm read `did not complete (passed)` — two instruments contradicting each other about
	// one record. `blocked` has no add-and-revert; its absence is argued from the docs' own
	// asymmetry, that a waived gate pushes and a blocked one does not
	// (review-stats.py:95-102). Each must surface under its own name.
	"gate passed is still dropped": {
		`{"run_id":"r","phase":"plan","planned_at":"2026-01-01T00:00:00","repo":"x","agent_cap":40,"gates":{"g":{"planned":"run"}}}`,
		`{"run_id":"r","phase":"cycle","n":1,"applied":0,"agents":2}`,
		`{"run_id":"r","phase":"finish","outcome":"clean","executed":{"g":{"status":"passed","reason":"r"}}}`,
	},
	"gate blocked is still dropped": {
		`{"run_id":"r","phase":"plan","planned_at":"2026-01-01T00:00:00","repo":"x","agent_cap":40,"gates":{"g":{"planned":"run"}}}`,
		`{"run_id":"r","phase":"cycle","n":1,"applied":0,"agents":2}`,
		`{"run_id":"r","phase":"finish","outcome":"clean","executed":{"g":{"status":"blocked","reason":"r"}}}`,
	},
	// Only `planned == "run"` is a planned gate. A gate the plan said to skip is not
	// dropped however badly it reports, which is why the Python tests the value rather
	// than mere presence in `gates`.
	"gate planned to skip is not dropped": {
		`{"run_id":"r","phase":"plan","planned_at":"2026-01-01T00:00:00","repo":"x","agent_cap":40,"gates":{"g":{"planned":"skip"}}}`,
		`{"run_id":"r","phase":"cycle","n":1,"applied":0,"agents":2}`,
		`{"run_id":"r","phase":"finish","outcome":"clean","executed":{"g":{"status":"failed","reason":"r"}}}`,
	},
	// `executed` absent entirely, which is what a run that never reached finish looks
	// like once a later row merges in.
	"no executed map at all": {
		`{"run_id":"r","phase":"plan","planned_at":"2026-01-01T00:00:00","repo":"x","agent_cap":40,"gates":{"g":{"planned":"run"}}}`,
		`{"run_id":"r","phase":"cycle","n":1,"applied":0,"agents":2}`,
	},
	// Several gates at once, each dropping for a different reason: the alarm line names
	// all of them, so the map must carry every key, not the first one found.
	"several gates drop for different reasons": {
		`{"run_id":"r","phase":"plan","planned_at":"2026-01-01T00:00:00","repo":"x","agent_cap":40,"gates":{"ok":{"planned":"run"},"waived":{"planned":"run"},"failed":{"planned":"run"},"silent":{"planned":"run"},"skipped":{"planned":"skip"}}}`,
		`{"run_id":"r","phase":"cycle","n":1,"applied":0,"agents":2}`,
		`{"run_id":"r","phase":"finish","outcome":"clean","executed":{"ok":{"status":"done","reason":"r"},"waived":{"status":"waived","reason":"r"},"failed":{"status":"failed","reason":"r"},"skipped":{"status":"failed","reason":"r"}}}`,
	},
}

func TestConvergenceMatchesThePython(t *testing.T) {
	script := runlogPath(t)
	for name, rows := range fixtures {
		t.Run(name, func(t *testing.T) {
			path := store(t, rows...)

			runs, err := Load(path, 0)
			if err != nil {
				t.Fatal(err)
			}
			run := runs["r"]
			if run == nil {
				t.Fatalf("Go side found no run %q in the fixture", "r")
			}
			got, err := run.Convergence()
			if err != nil {
				// A decode error here means the fixture belongs in the Go-only set, not in
				// the parity map: the typed port refuses shapes the Python coerced.
				t.Fatalf("Go side could not decode the fixture: %v", err)
			}

			// The CLI, not pyOracle: convergence has a subcommand, and its exit code is
			// non-zero for anything but converged by design, so the error is not a signal.
			// Stderr still has to come off the answer — merged in, one warning at import
			// would be compared as the verdict.
			cmd := exec.Command("python3", script, "convergence", "--run-id", "r")
			cmd.Env = append(os.Environ(), "REVIEW_LOOP_RUNS="+path)
			var errOut strings.Builder
			cmd.Stderr = &errOut
			out, runErr := cmd.Output()
			want := strings.TrimSpace(string(out))
			// "No such run" is exit 2 AND the word on stderr, and it takes both. For a
			// missing run cmd_convergence prints "unknown" to stderr and returns 2; for a
			// run that exists with no cycles it prints the same word to STDOUT with exit 1
			// (runlog.py:576-584). Keying on the text alone broke the `no cycles` fixture,
			// whose correct verdict IS "unknown"; keying on the code alone then swallowed
			// argparse, which also exits 2 — so a renamed or mistyped subcommand reported
			// "found no run" for all 36 fixtures while the real cause was `invalid choice`.
			// Two wrong guards in a row, each asserting a cause it did not check.
			if saysNoSuchRun(runErr, errOut.String()) {
				t.Fatalf("Python side found no run %q in the fixture", "r")
			}

			if got != want {
				// runErr is in the message because a non-ExitError failure — python3 absent,
				// say — leaves stdout and stderr both empty, and without it the report
				// blames the comparison for a process that never ran.
				t.Errorf("convergence disagrees: Go %q, Python %q\nrun error: %v\nstderr:\n%s\nfixture:\n%s",
					got, want, runErr, errOut.String(), strings.Join(rows, "\n"))
			}
		})
	}
}

// Load's merge is the other half of the port with a free oracle: if the two disagree about
// how many cycle rows a run has, every derivation downstream disagrees too.
func TestCycleCountMatchesThePython(t *testing.T) {
	script := runlogPath(t)
	for name, rows := range fixtures {
		t.Run(name, func(t *testing.T) {
			path := store(t, rows...)

			runs, err := Load(path, 0)
			if err != nil {
				t.Fatal(err)
			}
			if runs["r"] == nil {
				// Same vacuity as the DroppedGates test had: both sides render a missing run
				// as 0, so the subtest would pass having compared nothing.
				t.Fatalf("Go side found no run %q in the fixture", "r")
			}
			got := len(runs["r"].Cycles)

			// limit=None, not the default tail: a correctness check that silently sees no
			// plan because the record scrolled past 4000 lines is worse than a slow one.
			want := pyOracle(t, script, path,
				"run=m['load'](limit=None).get('r');"+
					"print("+strconv.Quote(noRun)+" if run is None else len(m['cycles_of'](run)))")
			if gotS := strconv.Itoa(got); gotS != want {
				t.Errorf("cycle count disagrees: Go %s, Python %s\nfixture:\n%s",
					gotS, want, strings.Join(rows, "\n"))
			}
		})
	}
}

// review-stats.py is the SECOND oracle, and it exists because the gate was narrower than
// its name. `dropped_gates` does not live in runlog.py — it lives in review-stats.py, which
// has no CLI entry point for it — so when the parity harness only knew how to shell out to
// `runlog.py <subcommand>`, DroppedGates had no oracle at all. It had unit tests, but those
// assert against my reading of the Python, which is the thing a port most needs checked.
//
// Resolved as a sibling of runlog.py rather than from its own variable: the two files ship
// together in the skill directory, and a second env var is a second thing to get wrong.
func reviewStatsPath(t *testing.T) string {
	t.Helper()
	p := filepath.Join(filepath.Dir(runlogPath(t)), "review-stats.py")
	if _, err := os.Stat(p); err != nil {
		// Fatal, never Skip, and not conditional on how the path was found. runlogPath has
		// already established that runlog.py is there — it skips or fatals otherwise — so by
		// here a missing sibling always means a runlog.py with no review-stats.py beside it,
		// which is a broken checkout rather than an absent one. Keying the polarity on
		// whether REVIEW_LOOP_RUNLOG happened to be set sent half those cases to Skip, and a
		// skip reports a green gate for half a gate, which is the thing this guard is for.
		t.Fatalf("review-stats.py is not beside runlog.py (looked at %s) — point "+
			"REVIEW_LOOP_RUNLOG at a runlog.py that has review-stats.py next to it", p)
	}
	return p
}

// DroppedGates feeds the alarm, and its answer is a map rather than a word, so it is the
// method with the most room to diverge quietly: a wrong default, a wrong GATE_OK, or a
// dropped isinstance guard each change one entry and nothing else.
//
// Parity is on the RENDERED status. The Python's map can hold a non-string status, because
// the status vocabulary is open and cmd_finish never validates it, while the port's type is
// map[string]string. str() is the same rendering the alarm line's f-string performs, so
// comparing there compares what a reader of the report would actually see — the keys and
// the defaults, which is where the logic is, still compare exactly.
func TestDroppedGatesMatchesThePython(t *testing.T) {
	stats := reviewStatsPath(t)
	for name, rows := range fixtures {
		t.Run(name, func(t *testing.T) {
			path := store(t, rows...)

			runs, err := Load(path, 0)
			if err != nil {
				t.Fatal(err)
			}
			run := runs["r"]
			if run == nil {
				// Without this the subtest is vacuous: the Python side also degrades a
				// missing run to {}, so a typo'd run_id or rows that fail to parse compare
				// {} against {} and report PASS while counting toward the CI floor. The
				// sibling test above has always had this guard; this one shipped without it.
				t.Fatalf("Go side found no run %q in the fixture", "r")
			}
			dropped, err := run.DroppedGates()
			if err != nil {
				t.Fatalf("Go side could not decode the fixture: %v", err)
			}
			gotJSON, err := json.Marshal(dropped) // encoding/json sorts map keys
			if err != nil {
				t.Fatal(err)
			}

			// review-stats' own load() takes no limit and so reads at runlog's DEFAULT
			// 4000-line tail, where the CycleCount site above passes limit=None and the Go
			// side reads the whole store. Bare is the more faithful oracle — it is the entry
			// point review-stats actually uses — and fixtures are a handful of rows, so the
			// tail never bites; a fixture past 4000 rows would need runlog.load directly.
			want := pyOracle(t, stats, path,
				"run=next((x for x in m['load']() if x.get('run_id')=='r'), None);"+
					"print("+strconv.Quote(noRun)+" if run is None else "+
					"json.dumps({k: str(v) for k, v in m['dropped_gates'](run).items()}, sort_keys=True))")

			// Re-marshal through Go so the two strings differ only where the maps do:
			// Python's json.dumps spaces its separators and Go's does not.
			var pyMap map[string]string
			if err := json.Unmarshal([]byte(want), &pyMap); err != nil {
				// %q, and the byte count: "printed something that is not a status map" sent
				// the reader hunting for malformed output when the real answer was zero bytes
				// and exit 0, which %s renders as nothing at all.
				t.Fatalf("python side's %d bytes of output are not a status map: %v\n"+
					"stdout: %q\nfixture:\n%s",
					len(want), err, want, strings.Join(rows, "\n"))
			}
			wantJSON, err := json.Marshal(pyMap)
			if err != nil {
				t.Fatal(err)
			}

			if string(gotJSON) != string(wantJSON) {
				t.Errorf("dropped gates disagree: Go %s, Python %s\nfixture:\n%s",
					gotJSON, wantJSON, strings.Join(rows, "\n"))
			}
		})
	}
}
