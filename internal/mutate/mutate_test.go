package mutate

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// guard builds a verify script that PASSES while `want` is still in app.py and fails
// once it is gone — i.e. a real test of that line, rather than `exit 1`.
//
// Every fixture needs this now: Run establishes a baseline by running each verify command
// against the unmutated file and requires it to pass, so a script that fails
// unconditionally is reported as already-red and the mutation is never evaluated. Several
// fixtures here used `exit 1` and were, in exactly the sense this tool exists to measure,
// not testing anything.
func guard(want, file string) string {
	return "#!/bin/sh\ngrep -q '" + want + "' " + file + " && exit 0\necho '--- FAIL: TestGuard'\nexit 1\n"
}

// repo builds a throwaway git repo with the given files, committed, so the
// clean-tree check passes. Every test gets its own.
func repo(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		mode := os.FileMode(0o644)
		if strings.HasSuffix(name, ".sh") {
			mode = 0o755
		}
		if err := os.WriteFile(path, []byte(body), mode); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{
		{"init", "-q", "."},
		{"config", "user.email", "t@t"},
		{"config", "user.name", "t"},
		{"config", "commit.gpgsign", "false"},
		// This machine has a global hooks path whose pre-commit scans for secrets over
		// the network; in a throwaway repo it fails slowly and the commit never lands,
		// leaving a dirty tree and a test that fails for the wrong reason.
		{"config", "core.hooksPath", "/dev/null"},
		{"add", "-A"},
		{"commit", "-qm", "seed"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return dir
}

func run(t *testing.T, dir string, muts ...Mutation) []Result {
	t.Helper()
	r := &Runner{Root: dir}
	results, err := r.Run(muts)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return results
}

// A suite that notices the defect is the baseline everything else is measured
// against, and the file must come back byte-identical afterwards.
func TestCaughtAndRestored(t *testing.T) {
	const src = "def f(x):\n    return x > 0\n"
	dir := repo(t, map[string]string{
		"app.py": src,
		"t.sh":   "#!/bin/sh\npython3 -c 'import app; assert app.f(1) and not app.f(0)'\n",
	})
	res := run(t, dir, Mutation{
		Source: "flip.mut", Target: "app.py", Verify: []string{"./t.sh"},
		Why: "comparison inverted", Old: "x > 0", New: "x < 0",
	})
	if res[0].Outcome != Caught {
		t.Errorf("outcome = %q, want caught (detail: %s)", res[0].Outcome, res[0].Detail)
	}
	got, err := os.ReadFile(filepath.Join(dir, "app.py"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != src {
		t.Errorf("target not restored:\n got %q\nwant %q", got, src)
	}
}

// The case this tool exists for: the suite passes with the defect present.
func TestSurvived(t *testing.T) {
	dir := repo(t, map[string]string{
		"app.py": "def f(x):\n    return x > 0\n",
		// Imports the module and asserts nothing — the shape of a real hollow test.
		"t.sh": "#!/bin/sh\npython3 -c 'import app' && echo all checks passed\n",
	})
	res := run(t, dir, Mutation{
		Source: "flip.mut", Target: "app.py", Verify: []string{"./t.sh"},
		Why: "comparison inverted", Old: "x > 0", New: "x < 0",
	})
	if res[0].Outcome != Survived {
		t.Errorf("outcome = %q, want survived", res[0].Outcome)
	}
}

// A stale anchor must never read as caught. Yesterday's hand-rolled version
// reported "0 failures" for a mutation that had silently matched nothing, which is
// indistinguishable from a working guard unless the count is checked.
func TestStaleAnchor(t *testing.T) {
	dir := repo(t, map[string]string{
		"app.py": "def f(x):\n    return x > 0\n",
		"t.sh":   "#!/bin/sh\nexit 0\n",
	})
	for _, tc := range []struct{ name, old string }{
		{"absent", "this text is not in the file"},
		{"ambiguous", "return"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := dir
			if tc.name == "ambiguous" {
				dir = repo(t, map[string]string{
					"app.py": "def f():\n    return 1\ndef g():\n    return 2\n",
					"t.sh":   "#!/bin/sh\nexit 0\n",
				})
			}
			res := run(t, dir, Mutation{
				Source: "x.mut", Target: "app.py", Verify: []string{"./t.sh"},
				Why: "whatever", Old: tc.old, New: "return 99",
			})
			if res[0].Outcome != Stale {
				t.Errorf("outcome = %q, want stale", res[0].Outcome)
			}
			if !strings.Contains(res[0].Detail, "want exactly 1") {
				t.Errorf("detail does not say why: %q", res[0].Detail)
			}
		})
	}
}

// Refusing a dirty tree is what makes a restore unambiguous. Without it, this tool
// reproduces the exact accident it was built after: a restore that also reverts
// uncommitted work, and a conclusion drawn from the result.
func TestRefusesDirtyTree(t *testing.T) {
	dir := repo(t, map[string]string{
		"app.py": "x = 1\n",
		// Leaves a trace if it is ever run. The refusal has to come BEFORE anything
		// executes: there is now a second clean-tree check after the baseline, so merely
		// asserting that Run returns an error no longer distinguishes the two — the
		// baseline would catch it too, having already run every command in the catalog
		// against the dirty tree first.
		"t.sh":       "#!/bin/sh\ntouch verify-ran\nexit 0\n",
		".gitignore": "verify-ran\n",
	})
	if err := os.WriteFile(filepath.Join(dir, "app.py"), []byte("x = 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := &Runner{Root: dir}
	_, err := r.Run([]Mutation{{Target: "app.py", Verify: []string{"./t.sh"}, Why: "w", Old: "x = 2", New: "x = 3"}})
	if err == nil {
		t.Fatal("ran against a dirty tree")
	}
	if !strings.Contains(err.Error(), "uncommitted") {
		t.Errorf("error does not name the cause: %v", err)
	}
	if _, serr := os.Stat(filepath.Join(dir, "verify-ran")); serr == nil {
		t.Error("a verify command ran before the dirty tree was refused")
	}
	// And the edit in progress is still there, untouched.
	got, _ := os.ReadFile(filepath.Join(dir, "app.py"))
	if string(got) != "x = 2\n" {
		t.Errorf("clobbered work in progress: %q", got)
	}
}

// Any one failing command is enough; the rest are not run.
func TestFirstFailingVerifyWins(t *testing.T) {
	dir := repo(t, map[string]string{
		"app.py": "x = 1\n",
		"a.sh":   guard("x = 1", "app.py"),
		// Counts into an IGNORED path. A verify command that writes a tracked file would
		// now (correctly) be refused: the baseline runs it against the real tree, so the
		// bytes a later mutation reads as "original" would no longer be the committed ones.
		"mark.sh":    "#!/bin/sh\necho ran >> ran-second\n",
		".gitignore": "ran-second\n",
	})
	res := run(t, dir, Mutation{
		Source: "x.mut", Target: "app.py", Verify: []string{"./a.sh", "./mark.sh"},
		Why: "w", Old: "x = 1", New: "x = 2",
	})
	if res[0].Outcome != Caught {
		t.Fatalf("outcome = %q, want caught", res[0].Outcome)
	}
	// Once, for the baseline. A second line means the mutated run kept going after
	// a.sh had already failed.
	ran, err := os.ReadFile(filepath.Join(dir, "ran-second"))
	if err != nil {
		t.Fatalf("the baseline never ran the second command: %v", err)
	}
	if n := strings.Count(string(ran), "ran"); n != 1 {
		t.Errorf("second verify command ran %d time(s), want 1 (baseline only) — the loop "+
			"kept going after the first command failed", n)
	}
}

// The executable bit must survive a restore that has to RECREATE the file. A verify
// command deleting its own target is outside the contract, but it happens — a test
// harness cleaning up, a build step rewriting a generated file — and os.WriteFile applies
// its perm argument only on create, so this is the one path where the stashed mode does
// any work at all.
//
// The previous version of this test mutated the file and compared the mode before and
// after, which could not fail: rewriting an existing file never changes its mode, so the
// assertion held whether or not the code preserved anything. Mutation testing found it.
func TestRestoreRecreatesWithTheOriginalMode(t *testing.T) {
	dir := repo(t, map[string]string{
		"tool.sh": "#!/bin/sh\necho one\n",
		// Deletes its target, but only once the mutation has landed: at baseline it must
		// pass and leave the tree alone.
		"t.sh": "#!/bin/sh\ngrep -q 'echo one' tool.sh && exit 0\nrm -f tool.sh\necho '--- FAIL: TestMode'\nexit 1\n",
	})
	before, err := os.Stat(filepath.Join(dir, "tool.sh"))
	if err != nil {
		t.Fatal(err)
	}
	run(t, dir, Mutation{
		Source: "x.mut", Target: "tool.sh", Verify: []string{"./t.sh"},
		Why: "w", Old: "echo one", New: "echo two",
	})
	after, err := os.Stat(filepath.Join(dir, "tool.sh"))
	if err != nil {
		t.Fatalf("restore did not put the target back after a verify command deleted it: %v", err)
	}
	if before.Mode() != after.Mode() {
		t.Errorf("mode changed: %v -> %v", before.Mode(), after.Mode())
	}
}

// A verify command that reads stdin must not inherit ours and block forever. This
// is a live hazard, not a hypothetical: pr-report.py hangs when run without stdin
// redirected, and a hung verify would stall the whole run with no result.
//
// The test replaces this process's stdin with a pipe nobody writes to, because
// `go test` otherwise supplies /dev/null — under which inheriting stdin reads EOF
// immediately and the assertion cannot fail. That is the exact shape of hollow
// assertion this tool exists to find, and the first run of it found this one.
func TestVerifyGetsNoStdin(t *testing.T) {
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	// Closing the write end on the way out releases anything left blocked on it.
	t.Cleanup(func() { pw.Close(); pr.Close() })
	saved := os.Stdin
	os.Stdin = pr
	t.Cleanup(func() { os.Stdin = saved })

	dir := repo(t, map[string]string{
		"app.py": "x = 1\n",
		"t.sh":   "#!/bin/sh\ncat > /dev/null\nexit 0\n",
	})
	done := make(chan Outcome, 1)
	go func() {
		r := &Runner{Root: dir}
		res, err := r.Run([]Mutation{{
			Source: "x.mut", Target: "app.py", Verify: []string{"./t.sh"},
			Why: "w", Old: "x = 1", New: "x = 2",
		}})
		if err != nil {
			t.Error(err)
			done <- ""
			return
		}
		done <- res[0].Outcome
	}()
	select {
	case got := <-done:
		if got != Survived {
			t.Errorf("outcome = %q, want survived", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a verify command reading stdin blocked the run")
	}
}

// A restore that fails must abort the run with a loud error. Reverting this to the old
// `_ = restore()` left every test green while the tool could print a clean score, exit 0,
// and leave a mutated file on disk.
func TestFailedRestoreIsFatal(t *testing.T) {
	dir := repo(t, map[string]string{
		"sub/app.py": "x = 1\n",
		// Passes at baseline; once the mutation lands it makes the target read-only, so
		// the restore cannot put the original bytes back. (Removing write permission from
		// the DIRECTORY is not enough: rewriting an existing file does not need it.)
		"t.sh": "#!/bin/sh\ngrep -q 'x = 1' sub/app.py && exit 0\nchmod 400 sub/app.py\necho '--- FAIL: TestX'\nexit 1\n",
	})
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(dir, "sub/app.py"), 0o644) })

	r := &Runner{Root: dir}
	_, err := r.Run([]Mutation{{
		Source: "x.mut", Target: "sub/app.py", Verify: []string{"./t.sh"},
		Why: "w", Old: "x = 1", New: "x = 2", Expect: "--- FAIL:",
	}})
	if err == nil {
		t.Fatal("a failed restore did not stop the run")
	}
	if !strings.Contains(err.Error(), "could not restore") ||
		!strings.Contains(err.Error(), "NOT CLEAN") {
		t.Errorf("error does not say the tree is dirty: %v", err)
	}
}

// A suite already failing before any mutation is applied cannot be said to have caught
// anything. Without the baseline, such a command fails identically with the mutation in
// place and every entry it guards scores as caught.
func TestAlreadyRedSuiteIsBroken(t *testing.T) {
	dir := repo(t, map[string]string{
		"app.py": "x = 1\n",
		// Red regardless of the mutation — a flaky test, a broken environment, someone
		// else's regression.
		"t.sh": "#!/bin/sh\necho '--- FAIL: TestUnrelated'\nexit 1\n",
	})
	res := run(t, dir, Mutation{
		Source: "x.mut", Target: "app.py", Verify: []string{"./t.sh"},
		Why: "w", Old: "x = 1", New: "x = 2", Expect: "--- FAIL:",
	})
	if res[0].Outcome != Broken {
		t.Errorf("outcome = %q, want broken (detail: %s)", res[0].Outcome, res[0].Detail)
	}
	if !strings.Contains(res[0].Detail, "already failing") {
		t.Errorf("detail does not say why: %q", res[0].Detail)
	}
}

// A target git does not track has no committed copy to recover from, and does not show up
// in the clean-tree check either, so it is refused before anything is written.
func TestRefusesUntrackedTarget(t *testing.T) {
	dir := repo(t, map[string]string{"app.py": "x = 1\n", "t.sh": guard("x = 1", "app.py")})
	if err := os.WriteFile(filepath.Join(dir, "generated.py"), []byte("y = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Ignored, so the clean-tree check cannot see it — which is the whole hazard.
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("generated.py\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", ".gitignore")
	gitCmd(t, dir, "commit", "-qm", "ignore")

	r := &Runner{Root: dir}
	_, err := r.Run([]Mutation{{
		Source: "x.mut", Target: "generated.py", Verify: []string{"./t.sh"},
		Why: "w", Old: "y = 1", New: "y = 2",
	}})
	if err == nil {
		t.Fatal("ran against an untracked target")
	}
	if !strings.Contains(err.Error(), "does not track") {
		t.Errorf("error does not name the cause: %v", err)
	}
	// And it must say so BEFORE writing anything.
	got, _ := os.ReadFile(filepath.Join(dir, "generated.py"))
	if string(got) != "y = 1\n" {
		t.Errorf("wrote to the untracked target anyway: %q", got)
	}
}

// A command that never started is not a pass. Reporting it as one made the mutation read
// as "every verify command passed with the defect present", which is simply untrue.
func TestCommandThatCannotStartIsBroken(t *testing.T) {
	r := &Runner{Root: "/no/such/directory"}
	c := r.verify("echo hello")
	if c.ran {
		t.Fatalf("a command in a nonexistent directory reported as having run: %+v", c)
	}
	if !strings.Contains(c.detail, "could not start") {
		t.Errorf("detail does not say why: %q", c.detail)
	}
}

func TestScoreCountsHolesAsFailures(t *testing.T) {
	caught, survived, stale, broken := Score([]Result{
		{Outcome: Caught}, {Outcome: Caught}, {Outcome: Survived}, {Outcome: Stale},
		{Outcome: Broken},
	})
	if caught != 2 || survived != 1 || stale != 1 || broken != 1 {
		t.Errorf("got %d/%d/%d/%d, want 2/1/1/1", caught, survived, stale, broken)
	}
}

// The defect that made this whole field necessary: a mutation that does not compile
// makes the verify command fail, and so scored as caught. Two of this repo's own
// nineteen entries were in that state, which the 19/19 score did not reveal.
func TestFailureWithoutTheExpectedMarkerIsBroken(t *testing.T) {
	dir := repo(t, map[string]string{
		"app.py": "def f(x):\n    return x > 0\n",
		// Passes at baseline, then fails for a reason that has nothing to do with the
		// assertion. Failing unconditionally would make the baseline answer this test
		// instead of the marker check — which is how it briefly passed for the wrong reason.
		"t.sh": "#!/bin/sh\ngrep -q 'x > 0' app.py && exit 0\necho 'SyntaxError: unexpected EOF' >&2\nexit 2\n",
	})
	res := run(t, dir, Mutation{
		Source: "flip.mut", Target: "app.py", Verify: []string{"./t.sh"},
		Why: "comparison inverted", Old: "x > 0", New: "x < 0",
		Expect: "--- FAIL:",
	})
	if res[0].Outcome != Broken {
		t.Errorf("outcome = %q, want broken (detail: %s)", res[0].Outcome, res[0].Detail)
	}
	// And the detail must carry the output, or the operator cannot tell which it was.
	if !strings.Contains(res[0].Detail, "SyntaxError") {
		t.Errorf("detail does not include the command output: %q", res[0].Detail)
	}
}

// The same command, now failing the way the mutation said it would.
func TestFailureWithTheExpectedMarkerIsCaught(t *testing.T) {
	dir := repo(t, map[string]string{
		"app.py": "def f(x):\n    return x > 0\n",
		"t.sh":   guard("x > 0", "app.py"),
	})
	res := run(t, dir, Mutation{
		Source: "flip.mut", Target: "app.py", Verify: []string{"./t.sh"},
		Why: "comparison inverted", Old: "x > 0", New: "x < 0",
		Expect: "--- FAIL:",
	})
	if res[0].Outcome != Caught {
		t.Errorf("outcome = %q, want caught (detail: %s)", res[0].Outcome, res[0].Detail)
	}
}

// With no Expect, any failure still counts — the field is opt-in, so an existing
// catalog keeps working.
func TestNoExpectMeansAnyFailureCounts(t *testing.T) {
	dir := repo(t, map[string]string{
		"app.py": "x = 1\n",
		// Fails once the line is gone, but says nothing a marker could match — which is
		// fine, because this mutation sets no Expect.
		"t.sh": "#!/bin/sh\ngrep -q 'x = 1' app.py && exit 0\necho 'something unrelated broke' >&2\nexit 3\n",
	})
	res := run(t, dir, Mutation{
		Source: "x.mut", Target: "app.py", Verify: []string{"./t.sh"},
		Why: "w", Old: "x = 1", New: "x = 2",
	})
	if res[0].Outcome != Caught {
		t.Errorf("outcome = %q, want caught", res[0].Outcome)
	}
}

// A target that cannot be read is Stale, not Survived. Reporting it as Survived would
// claim the suite missed a defect that was never introduced.
func TestUnreadableTargetIsStale(t *testing.T) {
	dir := repo(t, map[string]string{
		"locked.go": "package p\n\nvar A = 1\n",
		"t.sh":      guard("A = 1", "locked.go"),
	})
	path := filepath.Join(dir, "locked.go")
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o644) })
	// Through Run() the clean-tree check fires first, because chmod makes git report the
	// file as changed. one() is the unit that has to get this right.
	r := &Runner{Root: dir}
	res, err := r.one(Mutation{
		Source: "x.mut", Target: "locked.go", Verify: []string{"./t.sh"},
		Why: "w", Old: "A = 1", New: "A = 2",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != Stale {
		t.Fatalf("outcome = %q, want stale", res.Outcome)
	}
	if !strings.Contains(res.Detail, "unreadable") {
		t.Errorf("detail does not say why: %q", res.Detail)
	}
}

// onSignal has to run its function on every signal that would otherwise terminate the
// process without running deferred calls. SIGHUP is the one that was missing, and it is
// the likely one: a dropped SSH session or a supervisor HUPping the process group.
func TestOnSignalRunsOnHangup(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGHUP} {
		t.Run(sig.String(), func(t *testing.T) {
			// Ignore it first: with the SIGHUP registration removed, the default
			// disposition would kill the test binary, so the test would "fail" by dying
			// rather than by this assertion — a failure the catalog cannot tell apart
			// from the mutation breaking the build.
			signal.Ignore(sig)
			defer signal.Reset(sig)
			fired := make(chan struct{})
			stop := onSignal(func() { close(fired) })
			defer stop()
			if err := syscall.Kill(os.Getpid(), sig); err != nil {
				t.Fatal(err)
			}
			select {
			case <-fired:
			case <-time.After(5 * time.Second):
				t.Fatalf("%s did not run the restore function", sig)
			}
		})
	}
}

// After stop(), a signal must not run the function.
//
// This proves less than it looks like it proves, and the gap is the point. Deleting
// `signal.Stop(ch)` from stop() leaves this test passing — twenty runs out of twenty —
// because stop() closes `done` first and the goroutine always leaves through that branch.
// Chasing a test that could tell the two apart showed why none exists: even with the
// subscription left open, nothing reads `ch` once the goroutine is gone, and the restore a
// leaked handler would perform writes the same original bytes that were already written
// back. So signal.Stop and the `defer stop()` beside it are hygiene against a goroutine
// leak, not guards against a reachable defect — which is why neither has a catalog entry.
// Recorded here rather than papered over with a mutation that would be a no-op.
func TestStopDoesNotRunTheFunctionAfterwards(t *testing.T) {
	var ran int32
	stop := onSignal(func() { atomic.AddInt32(&ran, 1) })
	stop()
	// Default disposition for SIGHUP would kill the test binary, so re-ignore it first.
	signal.Ignore(syscall.SIGHUP)
	defer signal.Reset(syscall.SIGHUP)
	if err := syscall.Kill(os.Getpid(), syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	if n := atomic.LoadInt32(&ran); n != 0 {
		t.Errorf("the function ran %d time(s) after stop()", n)
	}
}

func TestProgressLineNamesEachMutation(t *testing.T) {
	dir := repo(t, map[string]string{
		"app.py": "x = 1\n",
		"t.sh":   guard("x = 1", "app.py"),
	})
	var log bytes.Buffer
	r := &Runner{Root: dir, Log: &log}
	if _, err := r.Run([]Mutation{{
		Source: "catalog/flip-compare.mut", Target: "app.py",
		Verify: []string{"./t.sh"}, Why: "w", Old: "x = 1", New: "x = 2",
	}}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(log.String(), "flip-compare") {
		t.Errorf("progress line does not name the mutation: %q", log.String())
	}
}

// gitCmd runs one git command in a fixture repo, failing the test if it does not land.
func gitCmd(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "core.hooksPath=/dev/null"}, args...)...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// A shell suite prints its one failure early and then dozens of passing checks, so the
// last few lines are the wrong few. Measured once (2026-10-02) against the review-loop
// skill's then-13-entry catalog in github.com/narthur/skills: 7 of the 13 details carried
// no failure line at all, while the output containing it had been captured the whole time.
func TestDetailShowsTheFailureNotJustTheEnd(t *testing.T) {
	var sh strings.Builder
	sh.WriteString("#!/bin/sh\ngrep -q 'x = 1' app.py && exit 0\n")
	sh.WriteString("echo 'FAIL  the comparison is inverted'\n")
	for i := 0; i < tailLines*3; i++ {
		sh.WriteString("echo 'ok    some later check'\n")
	}
	sh.WriteString("exit 1\n")

	dir := repo(t, map[string]string{"app.py": "x = 1\n", "t.sh": sh.String()})
	res := run(t, dir, Mutation{
		Target: "app.py", Verify: []string{"./t.sh"},
		Expect: "FAIL", Why: "w", Old: "x = 1", New: "x = 2",
	})
	if res[0].Outcome != Caught {
		t.Fatalf("outcome = %q, want caught (detail: %s)", res[0].Outcome, res[0].Detail)
	}
	if !strings.Contains(res[0].Detail, "the comparison is inverted") {
		t.Errorf("detail buried the failure under later passes: %q", res[0].Detail)
	}
}

// ...and when nothing in the output looks like a failure, the end is still the best
// guess: that is where an interpreter's last words land.
func TestDetailFallsBackToTheEndWhenNothingLooksLikeAFailure(t *testing.T) {
	var sh strings.Builder
	sh.WriteString("#!/bin/sh\ngrep -q 'x = 1' app.py && exit 0\n")
	for i := 0; i < tailLines*3; i++ {
		sh.WriteString("echo 'some unremarkable line'\n")
	}
	sh.WriteString("echo 'the last thing it said'\nexit 1\n")

	dir := repo(t, map[string]string{"app.py": "x = 1\n", "t.sh": sh.String()})
	res := run(t, dir, Mutation{
		Target: "app.py", Verify: []string{"./t.sh"},
		Expect: "unremarkable", Why: "w", Old: "x = 1", New: "x = 2",
	})
	if !strings.Contains(res[0].Detail, "the last thing it said") {
		t.Errorf("detail dropped the end of the output: %q", res[0].Detail)
	}
}

// The tests above drive tail() through the whole Runner, which proves the wiring. These
// drive it directly, because the cases that matter are about which lines survive and are
// unreasonable to stage as git fixtures.

// goTestOutput is what a real `go test` prints for one failing assertion. Note that the
// marker and the line explaining it are SEPARATE lines, and only the marker line contains
// "FAIL". Captured from an actual run, not written from memory.
var goTestOutput = []string{
	"--- FAIL: TestAssertFails (0.00s)",
	`    p_test.go:6: outcome = "survived", want caught`,
	"FAIL",
	"FAIL\ttailprobe\t0.233s",
	"FAIL",
}

// The first version of this selected only the lines carrying a marker, which threw away
// the one line with the diagnosis on it — every time, for the output shape this tool's own
// catalog produces on nearly every entry.
func TestTailKeepsTheMessageUnderTheMarker(t *testing.T) {
	lines := append(noise(tailLines*2), goTestOutput...)
	got := tail(strings.Join(lines, "\n"))

	if !strings.Contains(got, "--- FAIL: TestAssertFails") {
		t.Errorf("detail lost the marker line:\n%s", got)
	}
	if !strings.Contains(got, `p_test.go:6: outcome = "survived", want caught`) {
		t.Errorf("detail kept the marker and dropped the line that explains it:\n%s", got)
	}
}

// ...and a build failure carries no marker at all on the line that names the error, so
// filtering would have left two bare "FAIL" lines to explain a mutation that did not
// compile — the one case where the operator most needs to be told it was not the test.
func TestTailKeepsTheCompilerErrorOnABuildFailure(t *testing.T) {
	lines := append(noise(tailLines*2), []string{
		"# tailprobe [tailprobe.test]",
		"./p_test.go:6:7: undefined: undefinedSymbol",
		"FAIL\ttailprobe [build failed]",
	}...)
	got := tail(strings.Join(lines, "\n"))

	if !strings.Contains(got, "undefined: undefinedSymbol") {
		t.Errorf("detail dropped the compiler error:\n%s", got)
	}
}

// Nothing needs cutting, so nothing is cut. Filtering to marker lines narrowed output
// that already fitted, which is strictly worse than the behaviour it replaced.
func TestTailDoesNotNarrowOutputThatAlreadyFits(t *testing.T) {
	got := tail("building the fixture\nFAIL  the guard did not fire\ndone in 0.4s\n")
	for _, want := range []string{"building the fixture", "FAIL  the guard did not fire", "done in 0.4s"} {
		if !strings.Contains(got, want) {
			t.Errorf("short output lost %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "…") {
		t.Errorf("elided output that already fitted:\n%s", got)
	}
}

// More failures than the window holds. The earliest is the cause and the rest are usually
// consequences, so the window starts there and says so when it has cut something off.
func TestTailAnchorsOnTheEarliestFailure(t *testing.T) {
	var lines []string
	lines = append(lines, noise(3)...)
	for i := 1; i <= tailLines*2; i++ {
		lines = append(lines, fmt.Sprintf("FAIL  case %d", i))
	}
	got := tail(strings.Join(lines, "\n"))

	if !strings.Contains(got, "FAIL  case 1\n") {
		t.Errorf("window did not start at the earliest failure:\n%s", got)
	}
	if strings.Contains(got, fmt.Sprintf("FAIL  case %d", tailLines*2)) {
		t.Errorf("window ran past its budget to the last failure:\n%s", got)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("cut the output without saying so at the end:\n%s", got)
	}
	// And the same at the front. A window that drops the lines before it without marking
	// the cut reads as the whole output, so an operator stops looking — the identical
	// failure mode, and it went untested while its mirror image was covered.
	if !strings.HasPrefix(strings.TrimLeft(got, " "), "…") {
		t.Errorf("cut the output without saying so at the front:\n%s", got)
	}
}

// ...and the converse, or the assertion above is satisfied by marking every window
// whether or not anything was cut.
func TestTailDoesNotMarkACutItDidNotMake(t *testing.T) {
	lines := append([]string{"FAIL  the very first line"}, noise(tailLines*2)...)
	got := tail(strings.Join(lines, "\n"))
	// Asserted first, because the check below is an ABSENCE and an absence is satisfied by
	// returning nothing at all: `return ""` as tail()'s first statement passed this test.
	if !strings.Contains(got, "FAIL  the very first line") {
		// Errorf, not Fatalf: the check below is a total function on `got`, so letting it
		// run reports a compound defect — content dropped AND a cut claimed that never
		// happened — in one go instead of one symptom at a time.
		t.Errorf("window lost the failure it is supposed to start at:\n%s", got)
	}
	if strings.HasPrefix(strings.TrimLeft(got, " "), "…") {
		t.Errorf("window starts at line 0 but claims it cut something:\n%s", got)
	}
}

// Each marker, individually. The list had five entries no test touched, so deleting any of
// them left the suite green — and a silently narrowed marker list sends every window for
// that runner back to the end of the output.
func TestTailRecognisesEveryFailureMarker(t *testing.T) {
	// Spelled out rather than ranging over failureMarkers: ranging over the list under
	// test means deleting an entry deletes the case that would have caught it, and the
	// suite stays green. Add the marker here too when adding one there.
	want := []string{"FAIL", "Traceback", "panic:", "not ok", "AssertionError", "Error:"}
	if len(want) != len(failureMarkers) {
		t.Fatalf("failureMarkers has %d entries, this test knows %d — add the new one here",
			len(failureMarkers), len(want))
	}
	for _, marker := range want {
		t.Run(marker, func(t *testing.T) {
			lines := append([]string{"a line carrying " + marker + " and nothing else of note"},
				noise(tailLines*2)...)
			got := tail(strings.Join(lines, "\n"))
			if !strings.Contains(got, "a line carrying "+marker) {
				t.Errorf("%q is in failureMarkers but did not anchor the window:\n%s", marker, got)
			}
		})
	}
}

// noise builds n lines that no marker matches, each distinct so a test can tell which end
// of the output it is looking at.
func noise(n int) []string {
	lines := make([]string, n)
	for i := range lines {
		lines[i] = fmt.Sprintf("unremarkable line %d", i)
	}
	return lines
}

// Tracked is not the same as inside the repo: git tracks a symlink as a symlink, so a
// committed link satisfies `ls-files --error-unmatch` while os.ReadFile and os.WriteFile
// both follow it. Reproduced before the guard existed — a link to a file outside the repo
// passed the tracked-target check and the mutation was written through it.
func TestSymlinkTargetIsRefused(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("x = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dir := repo(t, map[string]string{"t.sh": guard("x = 1", "linked.txt")})
	if err := os.Symlink(outside, filepath.Join(dir, "linked.txt")); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-qm", "link")

	r := &Runner{Root: dir}
	_, err := r.Run([]Mutation{{
		Source: "catalog/x.mut", Target: "linked.txt",
		Verify: []string{"./t.sh"}, Why: "w", Old: "x = 1", New: "x = 2",
	}})
	if err == nil {
		t.Fatal("a tracked symlink target was accepted; writes follow it out of the repo")
	}
	if !strings.Contains(err.Error(), "symlink") {
		t.Errorf("error does not say why: %v", err)
	}
	// And the file it points at must be untouched.
	if b, _ := os.ReadFile(outside); string(b) != "x = 1\n" {
		t.Errorf("wrote through the link: %q", b)
	}
}

// The symlink guard reads `err == nil && <is a symlink>`, so an Lstat error falls through
// to the tracked-files check rather than deciding anything. Nothing pinned that down:
// inverting it to `err != nil || <is a symlink>` — every Lstat failure now reported as a
// symlink — left the whole suite green. The distinction matters because the fall-through is
// the only reason a plainly-missing target gets the error that names the actual problem.
func TestLstatFailureFallsThroughToTheTrackedCheck(t *testing.T) {
	dir := repo(t, map[string]string{"app.py": "x = 1\n", "t.sh": guard("x = 1", "app.py")})

	r := &Runner{Root: dir}
	_, err := r.Run([]Mutation{{
		Source: "catalog/x.mut", Target: "nothing-here.py",
		Verify: []string{"./t.sh"}, Why: "w", Old: "x = 1", New: "x = 2",
	}})
	if err == nil {
		t.Fatal("a target that does not exist at all was accepted")
	}
	if strings.Contains(err.Error(), "symlink") {
		t.Errorf("an Lstat failure was reported as a symlink, which is not what went wrong: %v", err)
	}
	if !strings.Contains(err.Error(), "does not track") {
		t.Errorf("error does not name the actual problem: %v", err)
	}
}

// indent is what separates the captured output from the summary line above it in the CLI
// report, and nothing checked it: stripping the prefix entirely left the whole suite green.
// Cosmetic, but the detail line is the one thing an operator actually reads.
func TestEveryDetailLineIsIndented(t *testing.T) {
	// A blank line in the middle, because that is the one a prefix loop is most likely to
	// skip and the one that would visually break the block.
	got := tail("alpha\n\nbeta\n")
	for _, l := range strings.Split(got, "\n") {
		if !strings.HasPrefix(l, "      ") {
			t.Errorf("detail line runs into the summary line above it: %q", l)
		}
	}
}
