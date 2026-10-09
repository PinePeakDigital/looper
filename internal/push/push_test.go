package push

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

	// A marker-only file must NOT satisfy the gate. At 37 bytes for a real run id — 25 static
	// plus 12 hex — that was cheaper to forge than the `disclosed --where "trust me"` row this
	// replaced.
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
	res, err := Check(CheckParams{Store: store, RunID: runID, GateState: "passed",
		Branch: "feat/x", DefaultBranch: "main", Repo: repoDir})
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
	res, err := Check(CheckParams{Store: st, RunID: "run1", GateState: "passed",
		Branch: "feat/x", DefaultBranch: "main", Repo: dir})
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
		if _, err := Check(CheckParams{Store: writeStore(t, cappedRun("run1")...), RunID: bad,
			GateState: "passed", Branch: "feat/x", DefaultBranch: "main", Repo: dir}); err == nil {
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

// sh must fail CLOSED when it cannot run the command at all, and nothing asserted that:
// flipping `return 1, ""` to `return 0, ""` left this whole package green, which would make a
// missing `gh`/`git` or a fired timeout read as success in the one function whose answer gates
// a push. Found by reproducing the mutation, not by reading.
//
// Three distinct causes, because they arrive through three different branches of sh: a binary
// that does not resolve at all (exec.Error, not ExitError), one that resolves but is not
// executable, and a process the context kills (an ExitError whose ExitCode() is -1).
func TestShFailsClosedWhenItCannotRun(t *testing.T) {
	dir := t.TempDir()

	t.Run("a binary that does not resolve", func(t *testing.T) {
		if rc, out := sh(dir, "looper-no-such-binary-anywhere"); rc != 1 || out != "" {
			t.Errorf("sh = (%d, %q), want (1, \"\") — an unresolvable binary must read as the blocking answer", rc, out)
		}
	})

	t.Run("a file that is not executable", func(t *testing.T) {
		notExec := filepath.Join(dir, "notexec")
		if err := os.WriteFile(notExec, []byte("#!/bin/sh\nexit 0\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if rc, _ := sh(dir, notExec); rc != 1 {
			t.Errorf("sh = %d for a non-executable file, want 1", rc)
		}
	})

	t.Run("a process the timeout kills", func(t *testing.T) {
		// Through shTimeout rather than the 20-second constant, so the test does not take 20
		// seconds. The branch under test is the same one.
		rc, out := shTimeout(dir, time.Millisecond, "sleep", "5")
		if rc != 1 {
			t.Errorf("sh = %d for a killed process, want 1 — Go reports -1 for a signalled "+
				"process and the Python's sh returns 1 for TimeoutExpired", rc)
		}
		if out != "" {
			t.Errorf("sh returned %q from a killed process", out)
		}
	})

	// And the GREEN side on the same channel: a command that runs and succeeds must not be
	// swept up by any of the above. Without this, `return 1` unconditionally would pass.
	t.Run("a command that works still reports zero", func(t *testing.T) {
		rc, out := sh(dir, "git", "rev-parse", "--is-inside-work-tree")
		if rc == 0 {
			t.Errorf("sh succeeded in a non-repo: (%d, %q)", rc, out)
		}
		gitRepo, _ := repo(t)
		rc, out = sh(gitRepo, "git", "rev-parse", "--is-inside-work-tree")
		if rc != 0 || out != "true" {
			t.Errorf("sh = (%d, %q) in a real repo, want (0, \"true\")", rc, out)
		}
	})
}

// upstreamExists's TRUE path was never reached through git: every fixture is a bare `git init`
// with no remote, so hardcoding the function to `return false` left the package green and the
// "feature branch with upstream" wording was only ever produced from a hand-built State.
func TestUpstreamExistsBothWays(t *testing.T) {
	dir, _ := repo(t)
	if upstreamExists(dir) {
		t.Error("a fresh repo with no remote must have no upstream")
	}

	// A tracking ref pointed at this same repo, which is enough for `rev-parse @{upstream}`
	// and needs no network. Commit first: @{upstream} resolves against a branch that exists.
	run := func(args ...string) {
		t.Helper()
		// core.hooksPath emptied: a global pre-commit hook has no business running inside a
		// test fixture, and the one on this machine needs the network.
		cmd := exec.Command("git", append([]string{"-c", "core.hooksPath="}, args...)...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@x",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@x")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("commit", "-q", "--allow-empty", "-m", "base")
	branch := "feat/x"
	run("checkout", "-q", "-b", branch)
	run("remote", "add", "origin", dir)
	run("update-ref", "refs/remotes/origin/"+branch, "HEAD")
	run("config", "branch."+branch+".remote", "origin")
	run("config", "branch."+branch+".merge", "refs/heads/"+branch)

	if !upstreamExists(dir) {
		t.Fatal("a branch with a configured tracking ref must have an upstream")
	}

	// And the wording it drives, end to end rather than from a hand-built State — which is
	// the half that was missing. "push with -u" must NOT appear once an upstream exists.
	shimGh(t, "exit 1")
	st := writeStore(t, cappedRun("run1")...)
	runs, err := record.Load(st, 0)
	if err != nil {
		t.Fatal(err)
	}
	gitdir := filepath.Join(dir, ".git")
	landReport(t, gitdir, "run1", Fingerprint(runs["run1"]))
	res, err := Check(CheckParams{Store: st, RunID: "run1", GateState: "passed",
		Branch: branch, DefaultBranch: "main", Repo: dir})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Push {
		t.Fatalf("expected a permitted push: %+v", res)
	}
	if !strings.Contains(res.Reason, "feature branch with upstream") {
		t.Errorf("reason = %q, want it to say the branch has an upstream", res.Reason)
	}
	if strings.Contains(res.Reason, "-u") {
		t.Errorf("reason = %q still asks for -u on a branch that has an upstream", res.Reason)
	}
}

// ReportLanded's git-dir lookup failing must read as NOT landed. Every other test uses a real
// repo, so this branch was never taken.
//
// The `gitdir == ""` half is what makes the guard load-bearing rather than defensive, and that
// needs the second case to show: `git rev-parse` writes its error to stderr, so sh returns an
// EMPTY stdout, and filepath.Join("", "info/review-loop-pending-report.X.md") is a RELATIVE
// path. Without the guard the gate would read that path out of the working directory — so a
// stray pending file beside wherever the tool happens to be invoked would satisfy it. Measured
// by planting exactly that file and chdir-ing to it.
func TestReportLandedWithoutAGitDir(t *testing.T) {
	shimGh(t, "exit 1")
	r := &record.Run{ID: "run1", Cycles: []record.Cycle{cyc(9)}}
	notARepo := t.TempDir()
	if ReportLanded("run1", r, notARepo) {
		t.Error("a directory that is not a git repo must read as not landed")
	}

	// The relative-read case. The planted file is a REAL, satisfying report, so the only
	// thing standing between it and a false "landed" is the guard.
	cwd := t.TempDir()
	body := fmt.Sprintf(reportMarker, "run1") + "\n\n" + strings.Join(Fingerprint(r), "\n") + "\n"
	rel := fmt.Sprintf(pendingFmt, "run1")
	if err := os.MkdirAll(filepath.Join(cwd, filepath.Dir(rel)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cwd, rel), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(cwd)
	if ReportLanded("run1", r, notARepo) {
		t.Error("a pending file in the working directory satisfied the gate — the git-dir " +
			"lookup's empty result was joined into a relative path and read")
	}
}

// The path guard belongs to ReportLanded, not to whoever remembers to call Check first — and
// this package's own tests call ReportLanded directly, so until it was moved the guard was a
// property of one caller. Asserted on the function that builds the path.
func TestReportLandedRefusesATraversingRunID(t *testing.T) {
	shimGh(t, "exit 1")
	dir, gitdir := repo(t)
	r := &record.Run{ID: "x", Cycles: []record.Cycle{cyc(9)}}

	// A real, satisfying report planted where a traversing id would reach it. The id below
	// resolves to <parent-of-repo>/escaped.md, so if the guard is gone the gate is satisfied
	// by a file outside the repo entirely.
	// Enough `..` to clear info/, the filename segment it is spliced into, and .git itself.
	// The count is CHECKED below rather than reasoned about: the first version used three and
	// landed back inside .git, because the id is spliced mid-filename so the leading `..`
	// pairs with that segment rather than with a directory.
	esc := strings.Repeat("../", 6) + "escaped"
	// The marker is keyed on the id BEING TESTED, not on r.ID. The first version of this
	// fixture wrote the marker for "x" while probing with `esc`, so the needle could never
	// match and the test would have passed with the guard deleted — vacuous for the one
	// thing it exists to check.
	body := fmt.Sprintf(reportMarker, esc) + "\n\n" + strings.Join(Fingerprint(r), "\n") + "\n"
	target := filepath.Join(gitdir, fmt.Sprintf(pendingFmt, esc))
	if rel, err := filepath.Rel(gitdir, target); err != nil || !strings.HasPrefix(rel, "..") {
		t.Fatalf("the fixture does not escape the git dir: %q resolves inside %q (rel %q)", target, gitdir, rel)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, bad := range []string{esc, "a/b", "", strings.Repeat("x", 65)} {
		if ReportLanded(bad, r, dir) {
			t.Errorf("ReportLanded accepted run id %q", bad)
		}
	}
}

// The diagnostic channel. A genuinely unposted report, a missing `gh`/`git`, and a fired
// timeout all refuse the push identically and correctly — the gate fails closed — but the
// refusal says "run pr-report.py --post first", which is the wrong instruction for two of the
// three. The reason string cannot say more, because parity_test.go compares it for exact
// equality against the Python's, so the distinction goes to Diag.
func TestCheckDiagnosesBeingUnableToAsk(t *testing.T) {
	shimGh(t, "exit 1")
	st := writeStore(t, cappedRun("run1")...)

	t.Run("neither probe could run", func(t *testing.T) {
		// Not a git repo, so `git rev-parse` fails too — the only case where nothing could be
		// asked at all.
		var diag strings.Builder
		res, err := Check(CheckParams{Store: st, RunID: "run1", GateState: "passed",
			Branch: "feat/x", DefaultBranch: "main", Repo: t.TempDir(), Diag: &diag})
		if err != nil {
			t.Fatal(err)
		}
		if res.Push {
			t.Fatalf("expected a refusal: %+v", res)
		}
		// The refusal itself is unchanged, which is what keeps parity.
		if !strings.Contains(res.Reason, "pr-report.py --post") {
			t.Errorf("reason = %q, want the unchanged owed-report refusal", res.Reason)
		}
		if !strings.Contains(diag.String(), "could not be determined") {
			t.Errorf("Diag = %q, want it to say the report check could not be run", diag.String())
		}
	})

	t.Run("git could be asked and said no", func(t *testing.T) {
		// A real repo with no pending file: the report genuinely is not there, which is the
		// case the refusal's instruction IS right for. Nothing may be written to Diag, or the
		// note becomes noise on every ordinary refusal and stops meaning anything.
		dir, _ := repo(t)
		var diag strings.Builder
		res, err := Check(CheckParams{Store: st, RunID: "run1", GateState: "passed",
			Branch: "feat/x", DefaultBranch: "main", Repo: dir, Diag: &diag})
		if err != nil {
			t.Fatal(err)
		}
		if res.Push {
			t.Fatalf("expected a refusal: %+v", res)
		}
		if diag.String() != "" {
			t.Errorf("Diag = %q for an ordinary missing report; it must stay silent", diag.String())
		}
	})

	t.Run("gh could be asked and said no", func(t *testing.T) {
		// gh ANSWERS — a PR exists and its comments were fetched — but the report is not
		// among them, and git is unavailable. `attributable` must be true on the strength of
		// gh alone, so no note is owed. TWO places observe the gh branch's
		// `attributable = true` — this one and TestReportProbeReportsBothAnswers' seventh
		// subtest, added by the same commit that claimed this was the only one and that
		// "no other subtest moves" when the assignment is deleted. Both move.
		// mutations/push-gh-answer-not-counted-as-asked verifies THIS test because its
		// `verify:` names it, not because the coverage is unique to it.
		//
		// An earlier version of these lines said the assignment was dead to the suite because
		// "every other test here shims gh to fail", which is false —
		// TestReportLandedFromAPRComment shims a SUCCEEDING gh at three call sites. It was
		// unobserved because ReportLanded discards the second value and Check reads it only
		// when `landed` is false.
		shimGh(t, "printf 'some unrelated comment\\n'")
		var diag strings.Builder
		res, err := Check(CheckParams{Store: st, RunID: "run1", GateState: "passed",
			Branch: "feat/x", DefaultBranch: "main", Repo: t.TempDir(), Diag: &diag})
		if err != nil {
			t.Fatal(err)
		}
		if res.Push {
			t.Fatalf("expected a refusal: %+v", res)
		}
		if diag.String() != "" {
			t.Errorf("Diag = %q, but gh answered — nothing could not be asked", diag.String())
		}
	})

	t.Run("a nil Diag is not a crash", func(t *testing.T) {
		if _, err := Check(CheckParams{Store: st, RunID: "run1", GateState: "passed",
			Branch: "feat/x", DefaultBranch: "main", Repo: t.TempDir()}); err != nil {
			t.Fatal(err)
		}
	})
}

// reportProbe's two return values on every path it can return them from. The accounting, by
// instrumenting every return site rather than counting them by eye: five syntactic returns,
// SEVEN reachable (site, value) states, of which three were already asserted at Check level
// before this test existed — the two git-unavailable states and the unreadable-pending-file
// one, each via a subtest of TestCheckDiagnosesBeingUnableToAsk. The other four were not, the
// second value being observable through Check only when the first is false and discarded
// outright by ReportLanded. Reproduced before this existed: flipping the refused-id return and
// the shared final return both left the whole package green.
//
// An earlier version of this comment said "four of its six", and the commit message said the
// test covered "all six paths". Both were wrong, and the second was wrong in the direction
// that invites deleting the Check-level case still holding a state this test misses — which is
// why the gh-answered-and-git-unavailable subtest below exists rather than the claim being
// narrowed to five of six.
//
// Called directly, which needs no export: this file is `package push`.
func TestReportProbeReportsBothAnswers(t *testing.T) {
	r := &record.Run{ID: "run1", Cycles: []record.Cycle{cyc(9)}}
	good := Fingerprint(r)

	t.Run("a refused run id is attributable without asking anything", func(t *testing.T) {
		// Nothing runs, and `attributable` is still true: the refusal has a cause the caller
		// can state. This is the path whose name the rename was about.
		//
		// This is NOT the traversal test, despite the `../` — with the ValidRunID guard
		// disabled this subtest still passes, because the traversed path does not exist, so
		// os.ReadFile fails and the function returns the same (false, true) by another route.
		// The bound is held by TestReportLandedRefusesATraversingRunID, which plants a
		// satisfying report at the destination and so can tell the two routes apart.
		shimGh(t, "exit 1")
		dir, _ := repo(t)
		landed, attributable := reportProbe("../escape", r, dir)
		if landed || !attributable {
			t.Errorf("reportProbe = (%v, %v), want (false, true)", landed, attributable)
		}
	})

	t.Run("gh matches", func(t *testing.T) {
		body := fmt.Sprintf(reportMarker, "run1") + "\\n" + strings.Join(good, "\\n")
		shimGh(t, "printf '"+body+"\\n'")
		dir, _ := repo(t)
		if landed, attributable := reportProbe("run1", r, dir); !landed || !attributable {
			t.Errorf("reportProbe = (%v, %v), want (true, true)", landed, attributable)
		}
	})

	t.Run("gh answers without a match and the pending file is unreadable", func(t *testing.T) {
		// A REAL git repo, so git answers and the return comes from the os.ReadFile error
		// branch, whose second value is a literal `true`. Named for that, because an earlier
		// version called this the gh path and asserted on a hardcoded constant: deleting
		// `attributable = true` from the gh branch left this subtest green.
		shimGh(t, "printf 'unrelated\\n'")
		dir, _ := repo(t)
		if landed, attributable := reportProbe("run1", r, dir); landed || !attributable {
			t.Errorf("reportProbe = (%v, %v), want (false, true) — git answered and the "+
				"pending file was absent", landed, attributable)
		}
	})

	t.Run("gh answers without a match and git cannot be asked", func(t *testing.T) {
		// The seventh state, and the only one where the gh branch's `attributable = true`
		// reaches a return: gh answered, so the refusal is attributable, while git cannot run
		// at all. A non-git dir rather than repo(t) is the whole difference.
		shimGh(t, "printf 'unrelated\\n'")
		if landed, attributable := reportProbe("run1", r, t.TempDir()); landed || !attributable {
			t.Errorf("reportProbe = (%v, %v), want (false, true) — gh answered on its own",
				landed, attributable)
		}
	})

	t.Run("the pending file matches", func(t *testing.T) {
		shimGh(t, "exit 1")
		dir, gitdir := repo(t)
		landReport(t, gitdir, "run1", good)
		if landed, attributable := reportProbe("run1", r, dir); !landed || !attributable {
			t.Errorf("reportProbe = (%v, %v), want (true, true)", landed, attributable)
		}
	})

	t.Run("the pending file contradicts the record", func(t *testing.T) {
		shimGh(t, "exit 1")
		dir, gitdir := repo(t)
		landReport(t, gitdir, "run1", []string{"## review-loop", "1 cycle(s) · 1 agent(s)"})
		if landed, attributable := reportProbe("run1", r, dir); landed || !attributable {
			t.Errorf("reportProbe = (%v, %v), want (false, true) — git answered", landed, attributable)
		}
	})

	t.Run("neither probe can run", func(t *testing.T) {
		shimGh(t, "exit 1")
		if landed, attributable := reportProbe("run1", r, t.TempDir()); landed || attributable {
			t.Errorf("reportProbe = (%v, %v), want (false, false) — this is the ONLY path "+
				"where the refusal has no stateable cause", landed, attributable)
		}
	})
}

// The second refusal whose stated reason is the wrong instruction: the run is not in the
// store. Measured before this existed — a wrong -store produced exit 0, an EMPTY stderr, and
// "run pr-report.py --post first", which re-renders the same empty record and cannot fix it.
//
// Kept separate from TestCheckDiagnosesBeingUnableToAsk because the two notes answer different
// questions and only this one can name the store path.
func TestCheckDiagnosesARunMissingFromTheStore(t *testing.T) {
	st := writeStore(t, cappedRun("run1")...)

	t.Run("a run id absent from the store is named, with the store that was read", func(t *testing.T) {
		shimGh(t, "exit 1")
		var diag strings.Builder
		dir, _ := repo(t)
		res, err := Check(CheckParams{Store: st, RunID: "absent1", GateState: "passed",
			Branch: "feat/x", DefaultBranch: "main", Repo: dir, Diag: &diag})
		if err != nil {
			t.Fatal(err)
		}
		if res.Push {
			t.Errorf("push permitted for a run that is not in the store: %+v", res)
		}
		for _, want := range []string{`"absent1"`, st, "empty record", "no readable row"} {
			if !strings.Contains(diag.String(), want) {
				t.Errorf("Diag = %q, want it to contain %q", diag.String(), want)
			}
		}
	})

	t.Run("a missing store file is the same state and says so", func(t *testing.T) {
		// record.Load returns an empty map and NO error for a path that does not exist, which
		// is why this is indistinguishable from a typo without the note.
		shimGh(t, "exit 1")
		var diag strings.Builder
		dir, _ := repo(t)
		missing := filepath.Join(t.TempDir(), "nosuchstore.jsonl")
		if _, err := Check(CheckParams{Store: missing, RunID: "run1", GateState: "passed",
			Branch: "feat/x", DefaultBranch: "main", Repo: dir, Diag: &diag}); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(diag.String(), missing) {
			t.Errorf("Diag = %q, want the store path %q that was actually read", diag.String(), missing)
		}
	})

	t.Run("a run that IS in the store is silent", func(t *testing.T) {
		// The note must not fire on the ordinary path, or it stops being a signal. gh is
		// shimmed to fail and git answers, so this still refuses — for a reason that IS
		// about the report.
		shimGh(t, "exit 1")
		var diag strings.Builder
		dir, _ := repo(t)
		if _, err := Check(CheckParams{Store: st, RunID: "run1", GateState: "passed",
			Branch: "feat/x", DefaultBranch: "main", Repo: dir, Diag: &diag}); err != nil {
			t.Fatal(err)
		}
		// Keyed on the note's CURRENT wording. It said "not found in" when this was written
		// and the note was reworded to "no readable row" in the same cycle, which left this
		// needle unable to match anything — the over-firing mutation then survived a 177-entry
		// catalog. Assert on a substring the note actually contains, and let the catalog prove
		// it: push-missing-run-diagnosed-on-every-check reddens here.
		if strings.Contains(diag.String(), "no readable row") {
			t.Errorf("Diag = %q for a run that is in the store; it must stay silent", diag.String())
		}
	})

	t.Run("a nil Diag is not a crash", func(t *testing.T) {
		shimGh(t, "exit 1")
		dir, _ := repo(t)
		if _, err := Check(CheckParams{Store: st, RunID: "absent2", GateState: "passed",
			Branch: "feat/x", DefaultBranch: "main", Repo: dir}); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("the note fires on a GRANTED push, which is the case worth saying something about", func(t *testing.T) {
		// The empty record derives `unknown`, which is a DISCLOSURE and not a block, and its
		// fingerprint is the two lines pr-report.py renders for a run with no cycles — so a
		// genuine report satisfies the gate and the push is permitted off a record that said
		// nothing. Every other subtest here is a refusal, which is how the note shipped
		// saying "the refusal below" while firing on grants.
		shimGh(t, "exit 1")
		var diag strings.Builder
		dir, gitdir := repo(t)
		landReport(t, gitdir, "absent3", Fingerprint(&record.Run{ID: "absent3"}))
		res, err := Check(CheckParams{Store: st, RunID: "absent3", GateState: "passed",
			Branch: "feat/x", DefaultBranch: "main", Repo: dir, Diag: &diag})
		if err != nil {
			t.Fatal(err)
		}
		if !res.Push {
			t.Fatalf("expected the empty record to GRANT here; got %+v", res)
		}
		if !strings.Contains(diag.String(), "no readable row") {
			t.Errorf("Diag = %q on a granted push; the note must fire here most of all", diag.String())
		}
		if strings.Contains(diag.String(), "refusal") {
			t.Errorf("Diag = %q — it says \"refusal\" on a push it just permitted", diag.String())
		}
	})

	t.Run("both notes fire when both are true, missing-run first", func(t *testing.T) {
		// A run absent from the store AND neither probe runnable. Nothing reached this state,
		// so a plausible "do not double-report" tightening passed the whole suite and the
		// catalog. Order matters: the store is the outer cause.
		t.Setenv("PATH", t.TempDir())
		var diag strings.Builder
		if _, err := Check(CheckParams{Store: st, RunID: "absent4", GateState: "passed",
			Branch: "feat/x", DefaultBranch: "main", Repo: t.TempDir(), Diag: &diag}); err != nil {
			t.Fatal(err)
		}
		got := diag.String()
		iStore := strings.Index(got, "no readable row")
		iProbe := strings.Index(got, "could not be determined")
		if iStore < 0 || iProbe < 0 {
			t.Fatalf("Diag = %q, want BOTH notes", got)
		}
		if iStore > iProbe {
			t.Errorf("Diag = %q, want the store note first — it is the outer cause", got)
		}
	})
}
