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
	// Stale: the anchor no longer matches the target, so nothing was tested. A hole
	// in the catalog rather than in the suite, and reported as a failure either way —
	// a catalog that quietly stops applying flatters the score it produces.
	Stale Outcome = "stale"
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
func (r *Runner) one(m Mutation) (Result, error) {
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
	// Preserve the mode explicitly. Rewriting a file through a fresh create dropped
	// runlog.py's executable bit once, and the Stop hook that requires it then failed
	// three times for reasons that looked unrelated to the edit.
	perm := info.Mode().Perm()

	restore := func() error { return os.WriteFile(target, original, perm) }
	stop := onSignal(func() {
		_ = restore()
		os.Exit(130)
	})
	defer stop()

	if err := os.WriteFile(target, mutated, perm); err != nil {
		return Result{}, fmt.Errorf("writing %s: %w", target, err)
	}
	defer func() { _ = restore() }()

	for _, cmd := range m.Verify {
		ok, detail := r.verify(cmd)
		if !ok {
			return Result{Mutation: m, Outcome: Caught, Detail: detail}, nil
		}
	}
	return Result{Mutation: m, Outcome: Survived,
		Detail: "every verify command passed with the defect present"}, nil
}

// verify runs one command in the repo root. It reports ok=true when the command
// SUCCEEDED, which for a mutation run is the bad news.
//
// `sh -c` is deliberate, not an injection hole: `verify:` IS a shell command line,
// often a pipeline, and it comes from a committed .mut file in this repo — the same
// trust level as a Makefile or a CI config, reviewed the same way. Passing argv
// directly would make the common case (`./runlog.test.sh 2>&1 | grep -q FAIL`)
// unexpressible. If a catalog ever comes from somewhere untrusted, this is the line
// that has to change first.
func (r *Runner) verify(command string) (ok bool, detail string) {
	cmd := exec.Command("sh", "-c", command)
	cmd.Dir = r.Root
	// No stdin. A suite that reads stdin would otherwise block forever inheriting
	// this process's — which is exactly the hang filed against pr-report.py.
	cmd.Stdin = nil
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out

	done := make(chan error, 1)
	if err := cmd.Start(); err != nil {
		return false, fmt.Sprintf("%s: could not start: %v", command, err)
	}
	go func() { done <- cmd.Wait() }()

	select {
	case err := <-done:
		if err != nil {
			return false, fmt.Sprintf("%s: %v", command, err)
		}
		return true, ""
	case <-time.After(verifyTimeout):
		_ = cmd.Process.Kill()
		<-done
		return false, fmt.Sprintf("%s: timed out after %s", command, verifyTimeout)
	}
}

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
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
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

// Score counts outcomes. Survivors and stale entries are both failures, because
// both mean a defect could ship with the suite green.
func Score(results []Result) (caught, survived, stale int) {
	for _, res := range results {
		switch res.Outcome {
		case Caught:
			caught++
		case Survived:
			survived++
		case Stale:
			stale++
		}
	}
	return caught, survived, stale
}

// ErrHoles is returned when any mutation survived or went stale.
var ErrHoles = errors.New("the suite did not catch every reintroduced defect")
