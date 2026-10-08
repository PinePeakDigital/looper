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
// except where a divergence is recorded as deliberate at the code that causes it. Today that
// is four places, not one: pyStr's number, container, nil and false gaps; num() and
// asksOutstanding() degrading where the Python raises TypeError and dies; and DroppedGates'
// two type assertions degrading where it raises AttributeError. A gap with a written reason
// is a different thing from one nobody noticed, and only the second kind is what this forbids.
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
		// A SHORT wrong token. Every other wrong-last-line case here is long and wordy, so
		// three length-based guards (len == len("unknown"), len <=, HasPrefix with the
		// operands swapped) passed the whole table.
		{"a short wrong last line is not the word", exitWith(2), "boom", false},
		// Exit 2 with NOTHING on stderr. argparse always says something and cmd_convergence
		// always prints the word, so silence is some third failure — and it is the case that
		// separates `lastLine == "unknown"` from `HasPrefix("unknown", lastLine)`, which is
		// true of every prefix including the empty string.
		{"exit 2 saying nothing is not a missing run", exitWith(2), "", false},
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
		// And these two keep lastLine honest where it CAN be wrong: a weakening of the outer
		// trim to newlines only breaks the second. The inner trim is a different story —
		// see its own note at lastLine; only its left half is reachable.
		// Padding on a line that is NOT the whole buffer: with only one line the outer trim
		// already strips it, so the inner one is never reached and dropping it survives.
		{"padding around the word is still the word", exitWith(2), "warn\n  unknown  \n", true},
		{"a whitespace-only last line does not hide the word", exitWith(2),
			"unknown\n   \n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := saysNoSuchRun(tc.err, tc.stderr); got != tc.want {
				t.Errorf("saysNoSuchRun(%v, %q) = %v, want %v", tc.err, tc.stderr, got, tc.want)
			}
		})
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
	// An empty list is falsy in Python, so `asked: []` does not block the converged branch.
	"empty-asked-list-is-falsy": {
		`{"run_id":"r","phase":"plan","planned_at":"2026-01-01T00:00:00","repo":"x","agent_cap":40}`,
		`{"run_id":"r","phase":"cycle","n":1,"applied":0,"asked":[],"agents":2}`,
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
	// An empty `unresolved_asks` is none, exactly as absent is — Python's `"" or 0`. The
	// non-empty-string case cannot live here: the Python raises TypeError on it, so there
	// is no answer to compare. record_test.go states the Go answer directly instead.
	"empty-asks-value-is-not-outstanding": {
		`{"run_id":"r","phase":"plan","planned_at":"2026-01-01T00:00:00","repo":"x","agent_cap":40}`,
		`{"run_id":"r","phase":"cycle","n":1,"applied":0,"agents":3}`,
		`{"run_id":"r","phase":"finish","outcome":"clean","unresolved_asks":""}`,
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
	// `false` is the same collapse spelled as a boolean, and the direct mirror of the
	// `true` fixture below, which does NOT collapse and so prints its rendering.
	"gate false status is unreported": {
		`{"run_id":"r","phase":"plan","planned_at":"2026-01-01T00:00:00","repo":"x","agent_cap":40,"gates":{"g":{"planned":"run"}}}`,
		`{"run_id":"r","phase":"cycle","n":1,"applied":0,"agents":2}`,
		`{"run_id":"r","phase":"finish","outcome":"clean","executed":{"g":{"status":false,"reason":"r"}}}`,
	},
	// `0` is the same collapse spelled as a number. Defaulting only on absence reported
	// "0" here, which names nothing about the gate.
	"gate zero status is unreported": {
		`{"run_id":"r","phase":"plan","planned_at":"2026-01-01T00:00:00","repo":"x","agent_cap":40,"gates":{"g":{"planned":"run"}}}`,
		`{"run_id":"r","phase":"cycle","n":1,"applied":0,"agents":2}`,
		`{"run_id":"r","phase":"finish","outcome":"clean","executed":{"g":{"status":0,"reason":"r"}}}`,
	},
	// An explicit JSON null, as distinct from the key being absent above: four different
	// Python routes to the same answer, which is the thing worth pinning.
	"gate null status is unreported": {
		`{"run_id":"r","phase":"plan","planned_at":"2026-01-01T00:00:00","repo":"x","agent_cap":40,"gates":{"g":{"planned":"run"}}}`,
		`{"run_id":"r","phase":"cycle","n":1,"applied":0,"agents":2}`,
		`{"run_id":"r","phase":"finish","outcome":"clean","executed":{"g":{"status":null,"reason":"r"}}}`,
	},
	// A SMALL INTEGER status, which is the only numeric class where the two renderings agree.
	// Named narrowly on purpose: this fixture passing says nothing about numbers in general,
	// and when it was called "reported as itself" it read as proof of a claim that is false
	// for 5.0, for 1000000, and for any JSON integer past 2^53. Those gaps are recorded at
	// pyStr and carry no fixture, because a fixture for them would fail.
	//
	// Reachability: runlog.py's cmd_finish does not validate the status VOCABULARY, though it
	// does refuse a non-dict executed value and require a non-empty reason for any status but
	// done — hence the reason on every row here.
	"gate small-integer status renders as itself": {
		`{"run_id":"r","phase":"plan","planned_at":"2026-01-01T00:00:00","repo":"x","agent_cap":40,"gates":{"g":{"planned":"run"}}}`,
		`{"run_id":"r","phase":"cycle","n":1,"applied":0,"agents":2}`,
		`{"run_id":"r","phase":"finish","outcome":"clean","executed":{"g":{"status":5,"reason":"r"}}}`,
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
	// A gate spec that is a bare string, not a dict. The Python guards with isinstance,
	// so this is not planned at all; reading `v["planned"]` would raise, and treating
	// the string itself as the plan would make it planned.
	"gate spec is not a dict": {
		`{"run_id":"r","phase":"plan","planned_at":"2026-01-01T00:00:00","repo":"x","agent_cap":40,"gates":{"g":"run"}}`,
		`{"run_id":"r","phase":"cycle","n":1,"applied":0,"agents":2}`,
		`{"run_id":"r","phase":"finish","outcome":"clean","executed":{}}`,
	},
	// The mirror case on the other side: an executed entry that is a bare string. The
	// isinstance guard means no status is read, so the gate is unreported — NOT "done".
	"executed entry is not a dict": {
		`{"run_id":"r","phase":"plan","planned_at":"2026-01-01T00:00:00","repo":"x","agent_cap":40,"gates":{"g":{"planned":"run"}}}`,
		`{"run_id":"r","phase":"cycle","n":1,"applied":0,"agents":2}`,
		`{"run_id":"r","phase":"finish","outcome":"clean","executed":{"g":"done"}}`,
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
			got := run.Convergence()

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
			gotJSON, err := json.Marshal(run.DroppedGates()) // encoding/json sorts map keys
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
