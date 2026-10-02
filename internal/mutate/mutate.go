package mutate

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// Outcome is what happened to one mutation.
type Outcome string

const (
	// Caught: at least one verify command failed. The suite does its job.
	Caught Outcome = "caught"
	// Survived: every verify command passed with the defect reintroduced. A hole.
	Survived Outcome = "survived"
	// Stale: the target could not be read, or the anchor does not match it exactly
	// once, so nothing was tested. A hole in the catalog rather than in the suite, and
	// reported as a failure either way — a catalog that quietly stops applying flatters
	// the score it produces.
	Stale Outcome = "stale"
	// Broken: a verify command failed, but not with the marker the mutation said to
	// expect, so something other than the named assertion broke — most often the
	// mutation itself not compiling. Also a failure: it means that mutation tested
	// nothing, while reading exactly like a catch.
	Broken Outcome = "broken"
)

// Result records one mutation and what it proved.
type Result struct {
	Mutation Mutation
	Outcome  Outcome
	Detail   string // why it was stale, or which command caught it
}

// verifyTimeout bounds one verify command. A mutation can easily produce a hang
// rather than a failure — an infinite loop, a process waiting on stdin — and an
// unbounded wait would stall the whole run with no result. A timeout counts as
// caught: the suite did notice, just not politely.
const verifyTimeout = 5 * time.Minute

// Runner applies mutations one at a time and reports what the suites noticed.
type Runner struct {
	Root string    // repo root; targets and commands resolve against it
	Log  io.Writer // progress, one line per mutation
}

// Run works through muts in order, restoring the target after each.
//
// It refuses to start on a dirty tree. That is not fastidiousness: restoring a
// mutation by checking the file out of git silently reverts uncommitted work
// alongside it, which happened six times in one session of doing this by hand and
// twice produced a *wrong conclusion* — a guard looked untested when the restore
// had already deleted the assertion testing it. This restores from bytes held in
// memory instead, and the clean-tree check means a crash mid-run leaves nothing
// ambiguous to recover.
func (r *Runner) Run(muts []Mutation) ([]Result, error) {
	if err := r.requireCleanTree(); err != nil {
		return nil, err
	}

	results := make([]Result, 0, len(muts))
	for _, m := range muts {
		res, err := r.one(m)
		if err != nil {
			return results, err
		}
		results = append(results, res)
		r.logf("  %-8s %-34s %s\n", res.Outcome, m.Name(), res.Detail)
	}
	return results, nil
}

// one applies a single mutation, runs its verify commands, and restores the file
// before returning — on every path, including a signal.
func (r *Runner) one(m Mutation) (res Result, err error) {
	target := filepath.Join(r.Root, m.Target)
	original, err := os.ReadFile(target)
	if err != nil {
		return Result{Mutation: m, Outcome: Stale,
			Detail: fmt.Sprintf("target unreadable: %v", err)}, nil
	}

	// Exactly one occurrence. Zero means the catalog has rotted; more than one means
	// the edit is ambiguous and would change code the mutation does not name. Both
	// have produced vacuous passes: a replace that silently matched nothing left the
	// suite green and the mutation looked "caught by nothing".
	if n := bytes.Count(original, []byte(m.Old)); n != 1 {
		return Result{Mutation: m, Outcome: Stale,
			Detail: fmt.Sprintf("anchor occurs %d times in %s, want exactly 1", n, m.Target)}, nil
	}

	mutated := bytes.Replace(original, []byte(m.Old), []byte(m.New), 1)
	info, err := os.Stat(target)
	if err != nil {
		return Result{}, fmt.Errorf("stat %s: %w", target, err)
	}
	// The mode, for the one case where it is load-bearing. os.WriteFile applies perm
	// ONLY when it creates the file, so rewriting an existing target cannot change its
	// mode — which is why the test guarding this could not fail until it was rewritten to
	// cover the real path: a verify command that DELETES the target, after which restore
	// recreates it and perm is the mode it comes back with. (The incident behind this,
	// a dropped executable bit breaking a hook three times, came from a write that
	// created the file fresh every time; this one does not.)
	perm := info.Mode().Perm()

	restore := func() error { return os.WriteFile(target, original, perm) }
	stop := onSignal(func() {
		if rerr := restore(); rerr != nil {
			fmt.Fprintf(os.Stderr, "\nlooper: could not restore %s: %v\n"+
				"THE WORKING TREE IS NOT CLEAN — %s still holds the mutation.\n", target, rerr, m.Target)
		}
		os.Exit(130)
	})
	defer stop()

	// Registered BEFORE the write, not after it. os.WriteFile truncates and then writes,
	// so a write that failed partway used to return here with the target holding neither
	// the original nor the mutation and no restore even attempted — while the function's
	// own doc comment promised a restore "on every path".
	//
	// A failed restore is fatal rather than ignored. Both call sites used to be
	// `_ = restore()`, so the tool could print "score N/N caught", exit 0, and leave a
	// mutated file on disk: the one outcome the clean-tree refusal exists to make
	// impossible.
	defer func() {
		if rerr := restore(); rerr != nil && err == nil {
			err = fmt.Errorf("could not restore %s after %s: %w\n"+
				"THE WORKING TREE IS NOT CLEAN — check `git status` before trusting anything above",
				m.Target, m.Name(), rerr)
		}
	}()

	if werr := os.WriteFile(target, mutated, perm); werr != nil {
		return Result{}, fmt.Errorf("writing %s: %w", target, werr)
	}

	// First failing command decides. Whether that failure counts as a catch depends on
	// the output, not merely on the exit code — see Mutation.Expect.
	for _, cmd := range m.Verify {
		failed, output, detail := r.verify(cmd)
		if !failed {
			continue
		}
		if m.Expect != "" && !strings.Contains(output, m.Expect) {
			return Result{Mutation: m, Outcome: Broken, Detail: fmt.Sprintf(
				"%s failed, but its output never contains %q, so the named assertion did "+
					"not report the defect — something else broke. %s", cmd, m.Expect, detail)}, nil
		}
		return Result{Mutation: m, Outcome: Caught, Detail: detail}, nil
	}
	return Result{Mutation: m, Outcome: Survived,
		Detail: "every verify command passed with the defect present"}, nil
}

// verify runs one command in the repo root. It reports failed=true when the command
// exited non-zero, which for a mutation run is the good news, plus the command's
// combined output so the caller can tell WHY it failed. That output used to be captured
// into a buffer nothing ever read, which is how a mutation that did not compile scored
// as caught.
//
// `sh -c` is deliberate, not an injection hole: `verify:` IS a shell command line,
// often a pipeline, and it comes from a committed .mut file in this repo — the same
// trust level as a Makefile or a CI config, reviewed the same way. Passing argv
// directly would make the common case (`./runlog.test.sh 2>&1 | grep -q FAIL`)
// unexpressible. If a catalog ever comes from somewhere untrusted, this is the line
// that has to change first.
func (r *Runner) verify(command string) (failed bool, output, detail string) {
	cmd := exec.Command("sh", "-c", command)
	cmd.Dir = r.Root
	// Own process group, so a timeout can kill the whole tree. Killing only the `sh`
	// child left its grandchild — usually the actual test runner — alive and consuming
	// CPU while the Runner restored the file and moved on to the next target, so the
	// timeout bounded the reported wait but not the cost. Unix-only, which this tool is.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// No stdin. A suite that reads stdin would otherwise block forever inheriting
	// this process's — which is exactly the hang filed against pr-report.py.
	cmd.Stdin = nil
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out

	done := make(chan error, 1)
	if err := cmd.Start(); err != nil {
		// Could not start is not a catch: nothing ran. Reported as a pass so the
		// mutation reads as surviving rather than silently scoring in the suite's favour.
		return false, "", fmt.Sprintf("%s: could not start: %v", command, err)
	}
	go func() { done <- cmd.Wait() }()

	select {
	case err := <-done:
		if err != nil {
			return true, out.String(), fmt.Sprintf("%s: %v\n%s", command, err, tail(out.String()))
		}
		return false, out.String(), ""
	case <-time.After(verifyTimeout):
		// Negative pid: the whole process group, not just the shell.
		if kerr := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); kerr != nil {
			_ = cmd.Process.Kill()
		}
		<-done
		return true, out.String(), fmt.Sprintf("%s: timed out after %s\n%s",
			command, verifyTimeout, tail(out.String()))
	}
}

// tail returns the last few lines of a command's output, indented, for a detail line.
// The whole point is that the operator can tell a failing assertion from a build error
// without re-running anything by hand, so it has to show enough to read.
func tail(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > tailLines {
		lines = append([]string{"…"}, lines[len(lines)-tailLines:]...)
	}
	for i, l := range lines {
		lines[i] = "      " + l
	}
	return strings.Join(lines, "\n")
}

const tailLines = 12

// requireCleanTree refuses to run when the working tree has changes, so that a
// restore can never be confused with a revert of someone's work in progress.
func (r *Runner) requireCleanTree() error {
	out, err := exec.Command("git", "-C", r.Root, "status", "--porcelain").Output()
	if err != nil {
		return fmt.Errorf("checking the tree in %s: %w (is it a git repo?)", r.Root, err)
	}
	if dirty := strings.TrimSpace(string(out)); dirty != "" {
		n := len(strings.Split(dirty, "\n"))
		return fmt.Errorf("refusing to run: %d uncommitted change(s) in %s.\n"+
			"Mutation testing restores files, and a restore is indistinguishable from\n"+
			"reverting work in progress. Commit or stash first", n, r.Root)
	}
	return nil
}

// onSignal arranges for fn to run on interrupt, and returns a stop function.
func onSignal(fn func()) (stop func()) {
	ch := make(chan os.Signal, 1)
	// SIGHUP and SIGQUIT too. Their default disposition terminates the process without
	// running a single deferred function, so a dropped SSH session or a supervisor
	// HUPping the process group left the target mutated with no restore at all — the
	// one thing the README promises cannot happen.
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT)
	done := make(chan struct{})
	go func() {
		select {
		case <-ch:
			fn()
		case <-done:
		}
	}()
	return func() {
		signal.Stop(ch)
		close(done)
	}
}

func (r *Runner) logf(format string, args ...any) {
	if r.Log != nil {
		fmt.Fprintf(r.Log, format, args...)
	}
}

// Score counts outcomes. Everything that is not Caught is a failure: a survivor means
// the suite missed the defect, a stale entry means the catalog never applied it, and a
// broken one means something other than the named assertion failed — in all three cases
// that defect can ship with the suite green.
func Score(results []Result) (caught, survived, stale, broken int) {
	for _, res := range results {
		switch res.Outcome {
		case Caught:
			caught++
		case Survived:
			survived++
		case Stale:
			stale++
		case Broken:
			broken++
		}
	}
	return caught, survived, stale, broken
}

// ErrHoles is returned when any mutation survived, went stale, or broke.
var ErrHoles = errors.New("the suite did not catch every reintroduced defect")
