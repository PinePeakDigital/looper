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

// check is what one verify command reported. `ran` is separate from `failed` because a
// command that never started is neither a pass nor a catch: treating it as a pass made
// the mutation report "every verify command passed with the defect present", which is
// false, and threw away the only line saying what had actually gone wrong.
type check struct {
	ran    bool
	failed bool
	output string
	detail string
}

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
	if err := r.requireTrackedTargets(muts); err != nil {
		return nil, err
	}
	alreadyBad, err := r.baseline(muts)
	if err != nil {
		return nil, err
	}

	results := make([]Result, 0, len(muts))
	for _, m := range muts {
		if why := firstBad(m, alreadyBad); why != "" {
			res := Result{Mutation: m, Outcome: Broken, Detail: why}
			results = append(results, res)
			r.logf("  %-8s %-34s %s\n", res.Outcome, m.Name(), res.Detail)
			continue
		}
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
	// The mutated file gets a LATER modification time, and the restore puts the original
	// one back. Both matter because of the baseline run: anything that caches on
	// (mtime, size) — Python's .pyc, make, many watchers — will happily reuse what the
	// baseline built if the mutation happens to be the same length and lands in the same
	// second. Measured: a `x > 0` -> `x < 0` mutation re-imported the bytecode the
	// baseline had just compiled and reported the suite as missing the defect.
	// (`go test` hashes content, so it was never affected; most things are not Go.)
	modTime := info.ModTime()
	// A failed Chtimes is logged, not ignored: silently leaving the original mtime in
	// place re-opens the stale-cache window this exists to close, and a mutation could
	// then read as surviving because of a cached build rather than a gap in the suite.
	stamp := func(t time.Time) {
		if err := os.Chtimes(target, t, t); err != nil {
			r.logf("  warning: could not set the modification time on %s: %v\n", m.Target, err)
		}
	}

	restore := func() error {
		if err := os.WriteFile(target, original, perm); err != nil {
			return err
		}
		stamp(modTime)
		return nil
	}
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
		rerr := restore()
		if rerr == nil {
			return
		}
		// Reported whether or not something else already failed. Guarding this on
		// `err == nil` meant the compound case — the write fails, and the restore
		// responding to it fails too — was the one case that stayed silent, while being
		// the one most likely to leave a half-written file behind.
		if err != nil {
			err = fmt.Errorf("%w\nand the restore failed too: %v\n"+
				"THE WORKING TREE IS NOT CLEAN — check `git status` before trusting anything above", err, rerr)
			return
		}
		err = fmt.Errorf("could not restore %s after %s: %w\n"+
			"THE WORKING TREE IS NOT CLEAN — check `git status` before trusting anything above",
			m.Target, m.Name(), rerr)
	}()

	if werr := os.WriteFile(target, mutated, perm); werr != nil {
		return Result{}, fmt.Errorf("writing %s: %w", target, werr)
	}
	stamp(modTime.Add(time.Second))

	// First failing command decides. Whether that failure counts as a catch depends on
	// the output, not merely on the exit code — see Mutation.Expect.
	for _, cmd := range m.Verify {
		c := r.verify(cmd)
		switch {
		case !c.ran:
			return Result{Mutation: m, Outcome: Broken, Detail: c.detail}, nil
		case !c.failed:
			continue
		case m.Expect != "" && !strings.Contains(c.output, m.Expect):
			return Result{Mutation: m, Outcome: Broken, Detail: fmt.Sprintf(
				"%s failed, but its output never contains %q, so the named assertion did "+
					"not report the defect — something else broke. %s", cmd, m.Expect, c.detail)}, nil
		default:
			return Result{Mutation: m, Outcome: Caught, Detail: c.detail}, nil
		}
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
func (r *Runner) verify(command string) check {
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
		return check{detail: fmt.Sprintf("%s: could not start: %v", command, err)}
	}
	go func() { done <- cmd.Wait() }()

	select {
	case err := <-done:
		if err != nil {
			return check{ran: true, failed: true, output: out.String(),
				detail: fmt.Sprintf("%s: %v\n%s", command, err, tail(out.String()))}
		}
		return check{ran: true, output: out.String()}
	case <-time.After(verifyTimeout):
		// Negative pid: the whole process group, not just the shell.
		if kerr := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); kerr != nil {
			_ = cmd.Process.Kill()
		}
		<-done
		return check{ran: true, failed: true, output: out.String(),
			detail: fmt.Sprintf("%s: timed out after %s\n%s", command, verifyTimeout, tail(out.String()))}
	}
}

// tail returns the part of a command's output an operator actually needs, indented, for a
// detail line. The point is to tell a failing assertion from a build error without
// re-running anything by hand.
//
// Output that already fits comes back whole. Output that must be cut is cut to a WINDOW,
// and the window starts at the first line that looks like a failure rather than at the
// end, because the end is often the wrong end: a shell test suite prints its one failure
// early and then dozens of passing checks, so a plain tail scrolls the failure out.
// (Measured once, 2026-10-02, against the review-loop skill's then-13-entry catalog in
// github.com/narthur/skills: 7 of the 13 details carried no failure line at all, while
// the output containing it had been captured the whole time.)
//
// A window and not a filter-to-matching-lines, which is what this did first: `go test`
// puts the marker and the line that explains it on SEPARATE lines, and only the marker
// line contains "FAIL" —
//
//	--- FAIL: TestX (0.00s)
//	    x_test.go:6: outcome = "survived", want caught
//	FAIL
//
// so keeping only marker-bearing lines throws away the one line with the diagnosis in it.
// A build failure is worse: the compiler error carries no marker and would vanish
// entirely, leaving two bare "FAIL" lines to explain a mutation that did not compile.
func tail(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) <= tailLines {
		return indent(lines)
	}

	start := len(lines) - tailLines // the end, where a crash message lands
	if i := firstFailure(lines); i >= 0 && i < start {
		start = i // ...unless something earlier looks like the failure itself
	}
	end := min(start+tailLines, len(lines))

	window := make([]string, 0, tailLines+2)
	if start > 0 {
		window = append(window, "…")
	}
	window = append(window, lines[start:end]...)
	if end < len(lines) {
		window = append(window, "…")
	}
	return indent(window)
}

func indent(lines []string) string {
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = "      " + l
	}
	return strings.Join(out, "\n")
}

// failureMarkers are what test runners and interpreters print when something went wrong.
// Deliberately a short list of unambiguous ones rather than anything matching "error":
// a false positive here anchors the window in the wrong place, which is worse than the
// end-of-output default it replaces.
var failureMarkers = []string{"FAIL", "Traceback", "panic:", "not ok", "AssertionError", "Error:"}

// firstFailure returns the index of the earliest line carrying a failure marker, or -1.
// Earliest and not latest: the first failure is the cause, the rest are usually
// consequences of it.
func firstFailure(lines []string) int {
	for i, l := range lines {
		for _, m := range failureMarkers {
			if strings.Contains(l, m) {
				return i
			}
		}
	}
	return -1
}

const tailLines = 12

// firstBad reports why a mutation cannot be evaluated, given the commands the baseline
// found unusable, or "" when all of its commands are sound.
func firstBad(m Mutation, alreadyBad map[string]string) string {
	for _, c := range m.Verify {
		if why, ok := alreadyBad[c]; ok {
			return why
		}
	}
	return ""
}

// requireTrackedTargets refuses a target git does not know about. An untracked or ignored
// file passes requireCleanTree — `git status --porcelain` does not list ignored files —
// but has no committed baseline, so if the process dies before the restore there is
// nothing to recover it from. In-repo damage is recoverable; this would not be.
func (r *Runner) requireTrackedTargets(muts []Mutation) error {
	seen := map[string]bool{}
	for _, m := range muts {
		if seen[m.Target] {
			continue
		}
		seen[m.Target] = true

		// Tracked is not the same as inside the repo. git tracks a symlink as a symlink,
		// so a committed `linked.txt -> /etc/hosts` satisfies ls-files while os.ReadFile
		// and os.WriteFile both follow it — and the catalog in CI is editable by whoever
		// opens the pull request. Lstat, so the link itself is what gets inspected.
		if info, err := os.Lstat(filepath.Join(r.Root, m.Target)); err == nil && info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%s names target %q, which is a symlink.\n"+
				"Writes follow it, so a tracked link is a write to wherever it points — "+
				"outside this repo, if that is where it points. Name the file itself",
				m.Source, m.Target)
		}

		out, err := exec.Command("git", "-C", r.Root, "ls-files", "--error-unmatch", "--", m.Target).Output()
		if err != nil || strings.TrimSpace(string(out)) == "" {
			return fmt.Errorf("%s names target %q, which git does not track.\n"+
				"An untracked target has no committed copy to recover from if this process is "+
				"killed before the restore, and it does not show up in the clean-tree check either",
				m.Source, m.Target)
		}
	}
	return nil
}

// porcelain runs `git status --porcelain` in the repo root. Both callers go through it so
// that they cannot drift apart on how a git failure is handled — they already had, one
// failing closed and the other open on the identical error.
func (r *Runner) porcelain() (string, error) {
	out, err := exec.Command("git", "-C", r.Root, "status", "--porcelain").Output()
	if err != nil {
		return "", fmt.Errorf("checking the tree in %s: %w (is it a git repo?)", r.Root, err)
	}
	return string(out), nil
}

// requireCleanTree refuses to run when the working tree has changes, so that a
// restore can never be confused with a revert of someone's work in progress.
func (r *Runner) requireCleanTree() error {
	out, err := r.porcelain()
	if err != nil {
		return err
	}
	if dirty := strings.TrimSpace(out); dirty != "" {
		n := len(strings.Split(dirty, "\n"))
		return fmt.Errorf("refusing to run: %d uncommitted change(s) in %s.\n"+
			"Mutation testing restores files, and a restore is indistinguishable from\n"+
			"reverting work in progress. Commit or stash first", n, r.Root)
	}
	return nil
}

// dirtyTracked reports any tracked-file change — modified, deleted, renamed, staged or
// conflicted — one per line, or "" when there are none. Untracked additions are
// deliberately excluded; see baseline's use of it.
//
// It returns the git failure rather than swallowing it. Reporting "clean" when the status
// command itself failed would fail OPEN on the one check whose purpose is to notice that
// the tree can no longer be trusted — and a verify command has just run arbitrary shell,
// so git breaking is exactly the case to worry about.
func (r *Runner) dirtyTracked() (string, error) {
	out, err := r.porcelain()
	if err != nil {
		return "", err
	}
	var lines []string
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if line == "" || strings.HasPrefix(line, "??") {
			continue
		}
		lines = append(lines, "  "+line)
	}
	return strings.Join(lines, "\n"), nil
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
