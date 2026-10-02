package mutate

import (
	"bytes"
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
		"t.sh":   "#!/bin/sh\nexit 1\n",
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
	// And the edit in progress is still there, untouched.
	got, _ := os.ReadFile(filepath.Join(dir, "app.py"))
	if string(got) != "x = 2\n" {
		t.Errorf("clobbered work in progress: %q", got)
	}
}

// Any one failing command is enough; the rest are not run.
func TestFirstFailingVerifyWins(t *testing.T) {
	dir := repo(t, map[string]string{
		"app.py":  "x = 1\n",
		"a.sh":    guard("x = 1", "app.py"),
		"mark.sh": "#!/bin/sh\necho ran >> ran-second\n",
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
