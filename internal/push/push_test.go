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

// ok is the three git facts that permit a push, in the order State names them. The Python's
// selftest spreads the same triple as `*ok` into a positional signature; here it is a
// partial State, so a transposition is a compile error rather than an inverted guard.
var ok = State{Branch: "feat/x", DefaultBranch: "main", UpstreamExists: true}

func with(s State, f func(*State)) State { f(&s); return s }

func TestDecide(t *testing.T) {
	for _, c := range []struct {
		name   string
		state  State
		push   bool
		reason string // substring; "" asserts nothing about the text
	}{
		// Converged pushes with no disclosure owed, whatever the gate did.
		{"converged with a passed gate", with(ok, func(s *State) {
			s.Convergence, s.GateState = record.Converged, "passed"
		}), true, "converged, evidence gate ok"},
		{"converged with a skipped gate", with(ok, func(s *State) {
			s.Convergence, s.GateState = record.Converged, "skipped"
		}), true, "converged, evidence gate ok"},

		// Not converging no longer BLOCKS — it obliges a disclosure. This is the behaviour
		// the whole split exists for: a cap that strands commits just hands the decision
		// back to a human every time.
		{"capped pushes and says so", with(ok, func(s *State) {
			s.Convergence, s.GateState = record.Capped, "passed"
		}), true, "capped (review not finished)"},
		{"halted pushes and says so", with(ok, func(s *State) {
			s.Convergence, s.GateState = record.Halted, "passed"
		}), true, "halted (review not finished)"},
		{"unknown pushes and says so", with(ok, func(s *State) {
			s.Convergence, s.GateState = record.Unknown, "passed"
		}), true, "unknown (review not finished)"},
		// An unset Convergence is the zero value of a string, not a convergence word. It
		// must read as unknown rather than printing " (review not finished)" with a blank
		// where the verdict goes.
		{"an unset convergence reads as unknown", with(ok, func(s *State) {
			s.GateState = "passed"
		}), true, "unknown (review not finished)"},

		// What genuinely blocks is a different question: broken, not unfinished.
		{"a blocked gate blocks", with(ok, func(s *State) {
			s.Convergence, s.GateState = record.Converged, "blocked"
		}), false, "evidence gate blocked"},
		// And it outranks convergence either way, so an unconverged run cannot push past a
		// real blocker by virtue of being merely unfinished.
		{"a blocked gate outranks an unfinished review", with(ok, func(s *State) {
			s.Convergence, s.GateState = record.Capped, "blocked"
		}), false, "evidence gate blocked"},
		{"an unresolved skip blocks", with(ok, func(s *State) {
			s.Convergence, s.GateState, s.UnresolvedSkip = record.Converged, "passed", true
		}), false, "skipped without 'remember as dismissal'"},

		// The default-branch guard, and the two unknown-name cases that would skip it.
		{"the default branch is never auto-pushed to", with(ok, func(s *State) {
			s.Convergence, s.GateState = record.Converged, "passed"
			s.Branch, s.DefaultBranch = "main", "main"
		}), false, "never auto-push to it"},
		{"an unknown default branch blocks", with(ok, func(s *State) {
			s.Convergence, s.GateState = record.Converged, "passed"
			s.Branch, s.DefaultBranch, s.UpstreamExists = "main", "", false
		}), false, "branch unknown"},
		{"an unknown current branch blocks", with(ok, func(s *State) {
			s.Convergence, s.GateState = record.Converged, "passed"
			s.Branch = ""
		}), false, "branch unknown"},
		// Both names known and equal, with no upstream: still the default branch. Without
		// this the no-upstream branch below could carry the default-branch case.
		{"the default branch blocks even with no upstream", with(ok, func(s *State) {
			s.Convergence, s.GateState = record.Converged, "passed"
			s.Branch, s.DefaultBranch, s.UpstreamExists = "main", "main", false
		}), false, "never auto-push to it"},

		// First push of a new feature branch is the normal case, not a block.
		{"a fresh feature branch pushes with -u", with(ok, func(s *State) {
			s.Convergence, s.GateState, s.UpstreamExists = record.Converged, "passed", false
		}), true, "push with -u"},

		// A recorded broken outcome blocks, and outranks a converged review: the tree is
		// the problem, not the review's completeness.
		{"a recorded test-failure blocks a converged run", with(ok, func(s *State) {
			s.Convergence, s.GateState, s.Outcome = record.Converged, "passed", "test-failure"
		}), false, "test-failure — broken"},
		{"a recorded blocked outcome blocks", with(ok, func(s *State) {
			s.Convergence, s.GateState, s.Outcome = record.Converged, "passed", "blocked"
		}), false, "blocked — broken"},
		{"a recorded abandoned outcome blocks", with(ok, func(s *State) {
			s.Convergence, s.GateState, s.Outcome = record.Converged, "passed", "abandoned"
		}), false, "abandoned — broken"},
		{"a clean outcome does not block", with(ok, func(s *State) {
			s.Convergence, s.GateState, s.Outcome = record.Converged, "passed", "clean"
		}), true, "converged, evidence gate ok"},
		// An outcome nothing recognises is not a blocker. The list is a denylist, so a
		// future outcome word pushes rather than deadlocking every run until this is
		// updated — the convergence derivation is what carries the completeness question.
		{"an unrecognised outcome does not block", with(ok, func(s *State) {
			s.Convergence, s.GateState, s.Outcome = record.Converged, "passed", "cycle-limit"
		}), true, "converged, evidence gate ok"},

		// An owed-but-missing report blocks, and the reason it carries IS the message.
		{"an owed report blocks an unfinished run", with(ok, func(s *State) {
			s.Convergence, s.GateState = record.Capped, "passed"
			s.Unreported = "report has not reached the PR"
		}), false, "report has not reached the PR"},
		// And it blocks a CONVERGED run too: the incident that motivated this was a clean
		// exit on a fresh branch whose summary never reached the PR, so gating only the
		// non-converged runs misses exactly the case that happened.
		{"an owed report blocks a converged run", with(ok, func(s *State) {
			s.Convergence, s.GateState = record.Converged, "passed"
			s.Unreported = "report has not reached the PR"
		}), false, "report has not reached the PR"},
		// A recorded broken outcome outranks the report check, so the operator is told the
		// tree is broken rather than being sent to post a report about it first.
		{"a broken outcome outranks an owed report", with(ok, func(s *State) {
			s.Convergence, s.GateState, s.Outcome = record.Converged, "passed", "test-failure"
			s.Unreported = "report has not reached the PR"
		}), false, "test-failure — broken"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, reason := Decide(c.state)
			if got != c.push {
				t.Errorf("Decide = %v, want %v (reason %q)", got, c.push, reason)
			}
			if c.reason != "" && !strings.Contains(reason, c.reason) {
				t.Errorf("reason %q does not contain %q", reason, c.reason)
			}
		})
	}
}

// Every blocking reason must be distinguishable from every other, or the operator is told
// the push was refused without being told which of six things to go fix.
func TestEveryBlockingReasonIsDistinct(t *testing.T) {
	seen := map[string]string{}
	for _, s := range []State{
		with(ok, func(s *State) { s.Outcome = "test-failure" }),
		with(ok, func(s *State) { s.Unreported = "no report" }),
		with(ok, func(s *State) { s.GateState = "blocked" }),
		with(ok, func(s *State) { s.UnresolvedSkip = true }),
		with(ok, func(s *State) { s.Branch = "" }),
		with(ok, func(s *State) { s.Branch, s.DefaultBranch = "main", "main" }),
	} {
		push, reason := Decide(s)
		if push {
			t.Fatalf("state %+v was permitted; this table is meant to be the blockers", s)
		}
		if prev, dup := seen[reason]; dup {
			t.Errorf("two different blockers share the reason %q (%s)", reason, prev)
		}
		seen[reason] = fmt.Sprintf("%+v", s)
	}
}

func cyc(agents int) record.Cycle { return record.Cycle{Agents: &agents} }

func TestFingerprint(t *testing.T) {
	r := &record.Run{ID: "r", Cycles: []record.Cycle{cyc(4), cyc(3), cyc(2)}}
	got := Fingerprint(r)
	want := []string{"## review-loop", "3 cycle(s) · 9 agent(s)"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("Fingerprint = %q, want %q", got, want)
	}
	// U+00B7, not an ASCII dot or a bullet. pr-report.py writes that byte, so an
	// ASCII-looking substitute makes every real report read as missing.
	if !strings.Contains(got[1], "·") {
		t.Errorf("the separator in %q is not U+00B7 MIDDLE DOT", got[1])
	}
	// A run with no cycles still has a fingerprint, and it is not the empty string: a
	// needle of "" is in every document, which would make the gate unfalsifiable.
	empty := Fingerprint(&record.Run{ID: "r"})
	if empty[1] != "0 cycle(s) · 0 agent(s)" {
		t.Errorf("a cycle-less run fingerprints as %q", empty[1])
	}
}

// shimGh puts a fake `gh` ahead of the real PATH. Tests that do not want a PR comment shim
// one that exits non-zero, which is also what a machine with no gh produces; the real
// binary is never invoked, so no test depends on a login or the network.
func shimGh(t *testing.T, body string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// repo is a git repo with no remote, so upstreamExists is false there and the push reason
// says "-u". Returns the repo root and its git dir.
func repo(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	if out, err := exec.Command("git", "init", "-q", dir).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	return dir, filepath.Join(dir, ".git")
}

func writeStore(t *testing.T, rows ...string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "runs.jsonl")
	if err := os.WriteFile(p, []byte(strings.Join(rows, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// A capped run with its report landed: the shape Step 14 is meant to permit.
func cappedRun(id string) []string {
	return []string{
		fmt.Sprintf(`{"run_id":%q,"phase":"plan","planned_at":"2026-01-01T00:00:00","repo":"x","agent_cap":8,"gates":{"t":{"planned":"run"}}}`, id),
		fmt.Sprintf(`{"run_id":%q,"phase":"cycle","n":1,"applied":3,"agents":9}`, id),
		fmt.Sprintf(`{"run_id":%q,"phase":"finish","outcome":"cycle-limit","executed":{"t":{"status":"done"}}}`, id),
	}
}

func landReport(t *testing.T, gitdir, runID string, lines []string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(gitdir, "info"), 0o755); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(reportMarker, runID) + "\n\n" + strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(gitdir, fmt.Sprintf(pendingFmt, runID)), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestReportLanded(t *testing.T) {
	shimGh(t, "exit 1")
	dir, gitdir := repo(t)
	r := &record.Run{ID: "run1", Cycles: []record.Cycle{cyc(9)}}

	if ReportLanded("run1", r, dir) {
		t.Error("with no report anywhere the gate must read NOT landed; it fails closed")
	}

	// A marker-only file must NOT satisfy the gate. At 38 bytes that was cheaper to forge
	// than the `disclosed --where "trust me"` row this replaced.
	landReport(t, gitdir, "run1", []string{"the report"})
	if ReportLanded("run1", r, dir) {
		t.Error("a marker alone satisfied the gate")
	}

	// The real thing: marker plus the run line, whose numbers come from the cycle rows and
	// so cannot be produced without rendering from the record.
	landReport(t, gitdir, "run1", Fingerprint(r))
	if !ReportLanded("run1", r, dir) {
		t.Error("marker plus fingerprint must satisfy the gate")
	}

	// And numbers that disagree with the record do not satisfy it either — the point of
	// the fingerprint is that it is a property of the artifact, not a claim about it.
	landReport(t, gitdir, "run1", []string{"## review-loop", "1 cycle(s) · 1 agent(s)"})
	if ReportLanded("run1", r, dir) {
		t.Error("a report whose counts contradict the cycle rows satisfied the gate")
	}
}

func TestReportLandedFromAPRComment(t *testing.T) {
	r := &record.Run{ID: "run1", Cycles: []record.Cycle{cyc(9)}}
	body := fmt.Sprintf(reportMarker, "run1") + "\\n" + strings.Join(Fingerprint(r), "\\n")
	shimGh(t, "printf '"+body+"\\n'")
	dir, _ := repo(t)
	if !ReportLanded("run1", r, dir) {
		t.Error("a PR comment carrying the marker and the fingerprint must satisfy the gate")
	}
	// gh succeeding with a body that is missing the fingerprint must fall through to the
	// pending file rather than being read as an answer: a PR with comments but no report
	// is exactly the case this gate exists for.
	shimGh(t, "printf 'some unrelated review comment\\n'")
	if ReportLanded("run1", r, dir) {
		t.Error("an unrelated PR comment satisfied the gate")
	}

	// A report rendered for a DIFFERENT run does not satisfy this one's gate, even with an
	// identical fingerprint: the MARKER names the run. Checked on the PR-comment path,
	// because one PR carries every run's comments and the pending file is keyed by run id
	// in its own filename — so the pending path cannot exercise the marker at all, and a
	// version of this case written there passed with the marker needle removed.
	shimGh(t, "printf '"+body+"\\n'")
	if ReportLanded("run2", r, dir) {
		t.Error("run1's report comment satisfied run2's gate")
	}
}

func check(t *testing.T, store, runID, repoDir string) Result {
	t.Helper()
	res, err := Check(store, runID, "passed", false, "feat/x", "main", repoDir)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	return res
}

func TestCheckReadsTheRecordRatherThanBeingTold(t *testing.T) {
	shimGh(t, "exit 1")
	dir, gitdir := repo(t)

	// A run id absent from the store is UNKNOWN, never converged, and owes a disclosure.
	// Defaulting it to converged used to pass every suite while emitting a silent
	// unconverged push.
	st := writeStore(t, cappedRun("run1")...)
	got := check(t, st, "nosuchrun", dir)
	if got.Convergence != record.Unknown {
		t.Errorf("an absent run derived %q", got.Convergence)
	}
	if got.Disclose == nil || !strings.Contains(*got.Disclose, "UNKNOWN") {
		t.Errorf("an absent run owes the unknown-completeness disclosure, got %v", got.Disclose)
	}

	// The record's outcome reaches Decide. Replacing that wiring with a constant used to
	// pass every suite, which is the same "the channel was never wired" shape that removing
	// --clean-exit left behind — so this asserts main's wiring, not Decide's handling.
	broken := append(cappedRun("run2"),
		`{"run_id":"run2","phase":"finish","outcome":"test-failure","executed":{"t":{"status":"done"}}}`)
	got = check(t, writeStore(t, broken...), "run2", dir)
	if got.Push || !strings.Contains(got.Reason, "test-failure") {
		t.Errorf("a recorded test-failure did not reach the decision: %+v", got)
	}

	// And the report check: no report anywhere -> refused, naming what to run.
	got = check(t, st, "run1", dir)
	if got.Push || !strings.Contains(got.Reason, "pr-report") {
		t.Errorf("an owed report did not reach the decision: %+v", got)
	}

	// A CONVERGED run's result carries no disclosure. Asserted through Check rather than on
	// a hand-built Result: the nil is set here, and a version of this that only encoded a
	// literal Result passed with the condition removed.
	conv := append(cappedRun("run3"),
		`{"run_id":"run3","phase":"cycle","n":2,"applied":0,"asked":0,"agents":1}`)
	convStore := writeStore(t, conv...)
	convRuns, err := record.Load(convStore, 0)
	if err != nil {
		t.Fatal(err)
	}
	landReport(t, gitdir, "run3", Fingerprint(convRuns["run3"]))
	if res := check(t, convStore, "run3", dir); res.Convergence != record.Converged || res.Disclose != nil {
		t.Errorf("a converged run must owe no disclosure: convergence=%q disclose=%v",
			res.Convergence, res.Disclose)
	}

	// With the report landed, the capped run is permitted AND still carries its line.
	runs, err := record.Load(st, 0)
	if err != nil {
		t.Fatal(err)
	}
	landReport(t, gitdir, "run1", Fingerprint(runs["run1"]))
	got = check(t, st, "run1", dir)
	if !got.Push {
		t.Fatalf("a capped run with its report landed must push: %+v", got)
	}
	if got.Convergence != record.Capped {
		t.Errorf("convergence = %q, want capped", got.Convergence)
	}
	if got.Disclose == nil || !strings.Contains(*got.Disclose, "CAPPED at 9 of 8 agents") {
		t.Errorf("a permitted capped push must carry its disclosure, got %v", got.Disclose)
	}
	if !strings.Contains(got.Reason, "push with -u") {
		t.Errorf("a repo with no upstream must say -u: %q", got.Reason)
	}
}

// The tier does not reach this gate. Five places in the Python's docs said it did, and all
// five survived because none was stated as an assertion: prose about what some other module
// reads has no failing test when it rots. Re-finishing the same run as `partial` must change
// nothing. Only tier_executed differs between the two rows — writing a different `outcome`
// too would change two fields Decide is given and prove nothing about the tier.
func TestTheTierDoesNotReachTheGate(t *testing.T) {
	shimGh(t, "exit 1")
	dir, gitdir := repo(t)
	rows := cappedRun("run1")
	st := writeStore(t, rows...)
	runs, err := record.Load(st, 0)
	if err != nil {
		t.Fatal(err)
	}
	landReport(t, gitdir, "run1", Fingerprint(runs["run1"]))
	before := check(t, st, "run1", dir)

	withTier := writeStore(t, append(rows,
		`{"run_id":"run1","phase":"finish","outcome":"cycle-limit","tier_executed":"partial","executed":{"t":{"status":"done"}}}`)...)
	after := check(t, withTier, "run1", dir)
	// Compared as the ENCODED result, not with %+v: Disclose is a pointer, so %+v prints
	// an address and two identical decisions never compare equal — the first version of
	// this test failed for that reason while the tier was already being ignored.
	beforeJSON, _ := before.Encode()
	afterJSON, _ := after.Encode()
	if string(beforeJSON) != string(afterJSON) {
		t.Errorf("tier_executed changed the push decision:\n before %s after  %s", beforeJSON, afterJSON)
	}
}

// An unreadable record is not evidence that a review finished. The Python cannot reach this
// state — it coerces instead of refusing — so the port's own rule decides it: Check errors,
// the subcommand exits non-zero, and nothing prints a verdict derived from a field it could
// not read.
func TestCheckRefusesAnUnreadableRecord(t *testing.T) {
	dir, _ := repo(t)
	st := writeStore(t,
		`{"run_id":"run1","phase":"plan","planned_at":"2026-01-01T00:00:00","repo":"x","agent_cap":8}`,
		`{"run_id":"run1","phase":"cycle","n":1,"applied":"3","agents":9}`)
	// The gh shim records that it ran. Nothing may consult the outside world for a run whose
	// record did not decode: Fingerprint reads Cycles, which is SHORT by one row for every
	// cycle row that failed, so a report gate run on this record would be checking the wrong
	// numbers. That is what the explicit Err check at the top of Check buys over the one
	// inside Convergence, and without this assertion reordering the two is invisible.
	ran := filepath.Join(t.TempDir(), "gh-ran")
	shimGh(t, "touch "+ran+"; exit 1")
	res, err := Check(st, "run1", "passed", false, "feat/x", "main", dir)
	if err == nil {
		t.Fatalf("Check returned %+v for a run with an unreadable cycle row", res)
	}
	if res.Push {
		t.Error("Check returned push=true alongside the error")
	}
	if _, statErr := os.Stat(ran); statErr == nil {
		t.Error("Check consulted gh for a run whose record did not decode")
	}
}

// The run id is a path segment in the pending-report filename, so a traversing one would
// send the gate looking for its evidence outside the git dir.
func TestCheckRefusesATraversingRunID(t *testing.T) {
	dir, _ := repo(t)
	for _, bad := range []string{"../../etc/passwd", "a/b", "", strings.Repeat("x", 65)} {
		if _, err := Check(writeStore(t, cappedRun("run1")...), bad, "passed", false, "feat/x", "main", dir); err == nil {
			t.Errorf("run id %q was accepted", bad)
		}
	}
}

// A converged run emits `disclose: null`, not `""` and not a missing key. Step 14 reads
// this, and "nothing to disclose" and "the empty disclosure" are not the same answer.
func TestEncodeEmitsNullForAConvergedRun(t *testing.T) {
	b, err := Result{Push: true, Reason: "converged, evidence gate ok", Convergence: record.Converged}.Encode()
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("%v: %s", err, b)
	}
	if v, present := m["disclose"]; !present || string(v) != "null" {
		t.Errorf("disclose = %s (present %v), want null", v, present)
	}
	if !strings.HasSuffix(string(b), "\n") {
		t.Errorf("the encoding must end in a newline: %q", b)
	}
	// HTML escaping off: encoding/json escapes <, > and & by default and json.dumps does
	// not, so a disclosure carrying one would reach the operator as <.
	line := "halted <1 of 2> & counting"
	b, err = Result{Reason: line, Disclose: &line}.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), `\u003c`) {
		t.Errorf("HTML escaping is on: %s", b)
	}
	if !strings.Contains(string(b), line) {
		t.Errorf("the line did not survive encoding verbatim: %s", b)
	}
}
