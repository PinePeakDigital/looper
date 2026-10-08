package record

import (
	"encoding/json"
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
		`{"run_id":"r","phase":"finish","outcome":"converged","executed":{"g":{"status":"waived"}}}`,
	},
	// done and n/a are the whole of GATE_OK: nothing is dropped.
	"gate done and n/a are accounted": {
		`{"run_id":"r","phase":"plan","planned_at":"2026-01-01T00:00:00","repo":"x","agent_cap":40,"gates":{"a":{"planned":"run"},"b":{"planned":"run"}}}`,
		`{"run_id":"r","phase":"cycle","n":1,"applied":0,"agents":2}`,
		`{"run_id":"r","phase":"finish","outcome":"converged","executed":{"a":{"status":"done"},"b":{"status":"n/a"}}}`,
	},
	// No executed entry at all: the gate was planned and nobody said anything.
	"gate with no executed entry": {
		`{"run_id":"r","phase":"plan","planned_at":"2026-01-01T00:00:00","repo":"x","agent_cap":40,"gates":{"g":{"planned":"run"}}}`,
		`{"run_id":"r","phase":"cycle","n":1,"applied":0,"agents":2}`,
		`{"run_id":"r","phase":"finish","outcome":"converged","executed":{}}`,
	},
	// Every FALSY status collapses to "unreported", not just an absent entry. The Python
	// writes `st or "unreported"`, so "", 0 and false all take the default; defaulting
	// only on absence reports `""`, `"0"` and `"false"`, which is a parity break and a
	// worse alarm line, since the status is what names the gate's problem.
	"gate empty-string status is unreported": {
		`{"run_id":"r","phase":"plan","planned_at":"2026-01-01T00:00:00","repo":"x","agent_cap":40,"gates":{"g":{"planned":"run"}}}`,
		`{"run_id":"r","phase":"cycle","n":1,"applied":0,"agents":2}`,
		`{"run_id":"r","phase":"finish","outcome":"converged","executed":{"g":{"status":""}}}`,
	},
	"gate false status is unreported": {
		`{"run_id":"r","phase":"plan","planned_at":"2026-01-01T00:00:00","repo":"x","agent_cap":40,"gates":{"g":{"planned":"run"}}}`,
		`{"run_id":"r","phase":"cycle","n":1,"applied":0,"agents":2}`,
		`{"run_id":"r","phase":"finish","outcome":"converged","executed":{"g":{"status":false}}}`,
	},
	"gate zero status is unreported": {
		`{"run_id":"r","phase":"plan","planned_at":"2026-01-01T00:00:00","repo":"x","agent_cap":40,"gates":{"g":{"planned":"run"}}}`,
		`{"run_id":"r","phase":"cycle","n":1,"applied":0,"agents":2}`,
		`{"run_id":"r","phase":"finish","outcome":"converged","executed":{"g":{"status":0}}}`,
	},
	"gate null status is unreported": {
		`{"run_id":"r","phase":"plan","planned_at":"2026-01-01T00:00:00","repo":"x","agent_cap":40,"gates":{"g":{"planned":"run"}}}`,
		`{"run_id":"r","phase":"cycle","n":1,"applied":0,"agents":2}`,
		`{"run_id":"r","phase":"finish","outcome":"converged","executed":{"g":{"status":null}}}`,
	},
	// A truthy non-string status. The vocabulary is open — cmd_finish never validates the
	// status string — and one of these has already reached this path once.
	"gate numeric status is reported as itself": {
		`{"run_id":"r","phase":"plan","planned_at":"2026-01-01T00:00:00","repo":"x","agent_cap":40,"gates":{"g":{"planned":"run"}}}`,
		`{"run_id":"r","phase":"cycle","n":1,"applied":0,"agents":2}`,
		`{"run_id":"r","phase":"finish","outcome":"converged","executed":{"g":{"status":5}}}`,
	},
	// `status: true` is the mirror of the false fixture above, and the one case where
	// Python's rendering and Go's are not the same word: str(True) is "True" and
	// fmt.Sprint(true) is "true". Reachable the same way false is — cmd_finish never
	// validates the status — and it is the alarm line's text that differs.
	"gate boolean-true status": {
		`{"run_id":"r","phase":"plan","planned_at":"2026-01-01T00:00:00","repo":"x","agent_cap":40,"gates":{"g":{"planned":"run"}}}`,
		`{"run_id":"r","phase":"cycle","n":1,"applied":0,"agents":2}`,
		`{"run_id":"r","phase":"finish","outcome":"converged","executed":{"g":{"status":true}}}`,
	},
	// Only `planned == "run"` is a planned gate. A gate the plan said to skip is not
	// dropped however badly it reports, which is why the Python tests the value rather
	// than mere presence in `gates`.
	"gate planned to skip is not dropped": {
		`{"run_id":"r","phase":"plan","planned_at":"2026-01-01T00:00:00","repo":"x","agent_cap":40,"gates":{"g":{"planned":"skip"}}}`,
		`{"run_id":"r","phase":"cycle","n":1,"applied":0,"agents":2}`,
		`{"run_id":"r","phase":"finish","outcome":"converged","executed":{"g":{"status":"failed"}}}`,
	},
	// A gate spec that is a bare string, not a dict. The Python guards with isinstance,
	// so this is not planned at all; reading `v["planned"]` would raise, and treating
	// the string itself as the plan would make it planned.
	"gate spec is not a dict": {
		`{"run_id":"r","phase":"plan","planned_at":"2026-01-01T00:00:00","repo":"x","agent_cap":40,"gates":{"g":"run"}}`,
		`{"run_id":"r","phase":"cycle","n":1,"applied":0,"agents":2}`,
		`{"run_id":"r","phase":"finish","outcome":"converged","executed":{}}`,
	},
	// The mirror case on the other side: an executed entry that is a bare string. The
	// isinstance guard means no status is read, so the gate is unreported — NOT "done".
	"executed entry is not a dict": {
		`{"run_id":"r","phase":"plan","planned_at":"2026-01-01T00:00:00","repo":"x","agent_cap":40,"gates":{"g":{"planned":"run"}}}`,
		`{"run_id":"r","phase":"cycle","n":1,"applied":0,"agents":2}`,
		`{"run_id":"r","phase":"finish","outcome":"converged","executed":{"g":"done"}}`,
	},
	// `executed` absent entirely, which is what a run that never reached finish looks
	// like once a later row merges in.
	"gates planned with no executed field": {
		`{"run_id":"r","phase":"plan","planned_at":"2026-01-01T00:00:00","repo":"x","agent_cap":40,"gates":{"g":{"planned":"run"}}}`,
		`{"run_id":"r","phase":"cycle","n":1,"applied":0,"agents":2}`,
	},
	// Several gates at once, each dropping for a different reason: the alarm line names
	// all of them, so the map must carry every key, not the first one found.
	"several gates drop for different reasons": {
		`{"run_id":"r","phase":"plan","planned_at":"2026-01-01T00:00:00","repo":"x","agent_cap":40,"gates":{"ok":{"planned":"run"},"waived":{"planned":"run"},"failed":{"planned":"run"},"silent":{"planned":"run"},"skipped":{"planned":"skip"}}}`,
		`{"run_id":"r","phase":"cycle","n":1,"applied":0,"agents":2}`,
		`{"run_id":"r","phase":"finish","outcome":"converged","executed":{"ok":{"status":"done"},"waived":{"status":"waived"},"failed":{"status":"failed"},"skipped":{"status":"failed"}}}`,
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
		if os.Getenv("REVIEW_LOOP_RUNLOG") != "" {
			// Explicitly pointed at a runlog.py with no sibling: a broken checkout, not
			// an absent one. Skipping here would report a green gate for half a gate.
			t.Fatalf("review-stats.py is not beside REVIEW_LOOP_RUNLOG (looked at %s)", p)
		}
		t.Skipf("review-stats.py not found at %s", p)
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
			dropped := map[string]string{}
			if runs["r"] != nil {
				dropped = runs["r"].DroppedGates()
			}
			gotJSON, err := json.Marshal(dropped) // encoding/json sorts map keys
			if err != nil {
				t.Fatal(err)
			}

			// runpy rather than an import: the file name has a hyphen in it, so it is not
			// importable, and its own `import runlog` needs the skill directory on the path.
			cmd := exec.Command("python3", "-c",
				"import json,os,sys,runpy;"+
					"sys.path.insert(0, os.path.dirname(os.path.abspath(sys.argv[1])));"+
					"m=runpy.run_path(sys.argv[1]);"+
					"run=next((r for r in m['load']() if r.get('run_id')=='r'), {});"+
					"print(json.dumps({k: str(v) for k, v in m['dropped_gates'](run).items()}, sort_keys=True))",
				stats)
			cmd.Env = append(os.Environ(), "REVIEW_LOOP_RUNS="+path)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("python side failed: %v\n%s", err, out)
			}

			// Re-marshal through Go so the two strings differ only where the maps do:
			// Python's json.dumps spaces its separators and Go's does not.
			var pyMap map[string]string
			if err := json.Unmarshal(out, &pyMap); err != nil {
				t.Fatalf("python side printed something that is not a status map: %v\n%s", err, out)
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
