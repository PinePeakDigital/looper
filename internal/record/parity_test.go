package record

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The parity gate. Until runlog.py is retired, the two implementations must not be able to
// disagree: this runs the Python against the same fixture and requires the same answer.
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

			cmd := exec.Command("python3", script, "convergence", "--run-id", "r")
			cmd.Env = append(os.Environ(), "REVIEW_LOOP_RUNS="+path)
			out, _ := cmd.CombinedOutput() // exits 1 for anything but converged, by design
			want := strings.TrimSpace(string(out))

			if got != want {
				t.Errorf("convergence disagrees: Go %q, Python %q\nfixture:\n%s",
					got, want, strings.Join(rows, "\n"))
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
			got := 0
			if runs["r"] != nil {
				got = len(runs["r"].Cycles)
			}

			cmd := exec.Command("python3", "-c",
				"import json,sys,runpy,os;"+
					"m=runpy.run_path(sys.argv[1]);"+
					"print(len(m['cycles_of'](m['load'](limit=None).get('r') or {})))",
				script)
			cmd.Env = append(os.Environ(), "REVIEW_LOOP_RUNS="+path)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("python side failed: %v\n%s", err, out)
			}
			want := strings.TrimSpace(string(out))
			if gotS := strconv.Itoa(got); gotS != want {
				t.Errorf("cycle count disagrees: Go %s, Python %s\nfixture:\n%s",
					gotS, want, strings.Join(rows, "\n"))
			}
		})
	}
}
