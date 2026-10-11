package report

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// shBudget bounds every subprocess this package starts.
//
// NOT the Python's — pr-report.py's `sh()` passes no timeout at all and can hang forever on a
// stuck gh. (push-check.py does pass `timeout=20`, which is where this line's wording came
// from; it was copied with the number and without the fact.) A bound this port adds, with the
// same value its sibling gate uses so an operator sees one figure.
//
// Threaded through shTimeout rather than read inline, so the kill branch can be exercised in
// milliseconds. With the constant inline that claim was false by construction: a const cannot
// be lowered by a test, so the one branch deciding whether a hung gh fails open or closed had
// no test and could not have one.
const shBudget = 20 * time.Second

// result is one subprocess outcome, INCLUDING its stderr. The Python throws stderr away on
// the probe — `rc, num, _ = sh(...)` — which is the whole defect this file exists to fix: the
// discarded stream is the only thing that says why gh failed.
type result struct {
	// Runnable is false when the program could not be started at all: not installed, not
	// executable, not on PATH. Distinct from "ran and failed", which has an exit code.
	Runnable bool
	Code     int
	Out, Err string
	// Drained is false when the output pipe could not be read to EOF within the drain bound,
	// which happens when a DESCENDANT of the child inherited it and outlived it. The child's
	// exit status is still trustworthy in that case; `Out` is NOT, because the descendant
	// writes into the same fd and therefore into the same buffer. Measured: a `gh pr list`
	// that exited 0 printing nothing — meaning there is no PR — alongside a descendant that
	// printed "42" yielded Out="42", and FindPR answered "PR #42" for a PR that does not
	// exist, after which the disclosure was commented onto it. Any caller that READS Out must
	// test this; a caller that only needs the exit status must not, because rejecting those
	// is what made a `gh pr comment` that had already landed read as failed.
	Drained bool
}

func sh(repo, prog string, args ...string) result {
	return shTimeout(repo, shBudget, prog, args...)
}

func shTimeout(repo string, budget time.Duration, prog string, args ...string) result {
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	cmd := exec.CommandContext(ctx, prog, args...)
	cmd.Dir = repo
	// WaitDelay, or the budget above bounds NOTHING. CommandContext kills the direct child
	// only, and because these sinks are strings.Builders os/exec drains them through pipes a
	// GRANDCHILD inherits — a credential helper gh starts, a git subprocess, a shell wrapper's
	// background job. With WaitDelay zero, Wait blocks until every holder closes its end.
	// Measured against a shim whose last line is `sleep 10 &`: a 300ms budget returned after
	// 10.15s, and returned code 0 — a success verdict for a call that blew its deadline by 34x.
	// The hung-subprocess subtest cannot see it: it asserts the error text and not the elapsed
	// time, and at its budget the kill usually wins the race against the shell's fork. The
	// subtest after it asserts the bound deterministically, by backgrounding the sleep so a
	// grandchild exists for certain.
	//
	// After the delay fires Wait reports exec.ErrWaitDelay when the child itself exited
	// cleanly — measured, NOT an ExitError, so it lands in the default arm and reads as
	// ran-and-failed, which is the honest verdict for a call that could not be drained. When
	// the child was also killed, the ExitError wins and nothing changes. One second rather
	// than something tighter: it bounds the DRAIN, not the deadline, and a healthy gh never
	// reaches it.
	cmd.WaitDelay = time.Second
	var out, errOut strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err := cmd.Run()
	r := result{Runnable: true, Drained: true,
		Out: strings.TrimSpace(out.String()), Err: strings.TrimSpace(errOut.String())}
	switch {
	case err == nil:
		return r
	// The child exited CLEANLY and only the DRAIN timed out, because a descendant inherited
	// the output pipe and outlived it. The EXIT STATUS is a real success; `Out` is not
	// trustworthy, so this sets Drained false rather than claiming the output is whole.
	//
	// Both halves are load-bearing and each was measured after getting it wrong. Rejecting the
	// whole call — which is what happened before this arm existed — made a `gh pr comment`
	// that had ALREADY LANDED read as failed, so the body was deferred and Step 0c posted a
	// SECOND public disclosure; under the production 20s budget, with 19 of them unused.
	// Accepting it as a success WITH its output — the first version of this arm — let the
	// descendant's bytes become the answer: measured, a `gh pr list` that exited 0 printing
	// NOTHING, meaning there is no PR, alongside a descendant printing "42", gave Out="42",
	// FindPR said "PR #42", and the disclosure was commented onto a PR that does not exist for
	// this branch. The descendant is not a bystander; it writes into the same fd.
	//
	// NOT a kill-versus-drain test. `ctx.Err()` is read AFTER the up-to-WaitDelay drain, so for
	// any budget at or below WaitDelay the deadline has always passed by the time this runs and
	// the arm cannot fire — measured, the same shim gives code 0 at a 20s budget and code 1 at
	// 300ms. An earlier comment here claimed this distinguished the kill, which it does not; a
	// kill is an ExitError and the default arm takes it either way. What the condition actually
	// buys is that a call whose own deadline expired is never reported as a success.
	case ctx.Err() == nil && errors.Is(err, exec.ErrWaitDelay) &&
		!errors.As(err, new(*exec.ExitError)):
		r.Drained = false
		drain := "a descendant held the output pipe past the drain bound (" + err.Error() +
			"), so the output cannot be trusted; the subprocess itself exited cleanly"
		// APPENDED, not assigned: gh's stderr is the only stream that says what gh thought,
		// and the arm one level down was fixed for dropping it. This one dropped it too.
		if r.Err != "" {
			drain += "; its stderr: " + r.Err
		}
		r.Err = drain
		return r
	// OUR OWN budget cancelled the probe before the fork. `Start` checks the context after
	// LookPath and returns `ctx.Err()`, which is not an ExitError, so without this arm it fell
	// to the "never started" test below and answered "install gh" for a gh the caller's own
	// deadline cancelled — the advice the default arm's comment says must never be given for a
	// gh that was killed. Measured: budget 1ns gives `context deadline exceeded`; budget 1ms
	// gives `signal: killed`, an ExitError, which the default arm already handles.
	//
	// The test is on the ERROR's identity and NOT on `ctx.Err() != nil`, which was the first
	// attempt and asked the clock instead. `Start` returns a missing binary's lookPathErr
	// BEFORE it consults the context, so with both true at once that arm claimed an uninstalled
	// gh and reported "gh ran and failed" — measured, `Runnable=true Code=1` with "executable
	// file not found in $PATH" in the very same string. Trading one wrong assertion for another:
	// an expired clock is not evidence about what went wrong.
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		r.Code = 1
		if r.Err == "" {
			r.Err = err.Error()
		}
		return r
	// Never STARTED. `Cmd.Process` is the underlying process "once started", so a nil one
	// after Run is exactly that and nothing else. Enumerating sentinels — ErrNotFound,
	// ErrNotExist, ErrPermission — missed the binary that exists, is executable, and is the
	// wrong format: measured, a chmod +x text file gives a *fs.PathError ("exec format
	// error") matching none of the three, so it fell to the default and reported "gh ran and
	// failed" for a gh that never ran. That is the wrong remedy twice over — it tells the
	// operator to wait for GitHub when the fix is to reinstall. The inverse test
	// `!errors.As(err, new(*exec.ExitError))` replaced the enumeration and was wrong in the
	// other direction: os/exec documents "other error types may be returned for I/O
	// problems", and these sinks are strings.Builders, so os/exec allocates pipes and copy
	// goroutines and Wait reports a copy failure — for a process that DID run. ErrWaitDelay is
	// the second such error and now the one that actually fires: setting WaitDelay above made
	// it reachable, and it is what a grandchild holding the pipes produces. (An earlier wording
	// here said it "cannot arise because cmd.WaitDelay is never set" — true when written, false
	// one commit later, which is what a claim resting on the absence of a line is worth.)
	// Measured: it is NOT an ExitError, so it falls to the default arm and reads as ran-and-
	// failed, which is the honest verdict for a call that could not be drained. Asking the Cmd
	// whether it started answers the question this branch is actually about.
	case cmd.Process == nil:
		r.Runnable, r.Code = false, 127
		if r.Err == "" {
			r.Err = err.Error()
		}
		return r
	default:
		// Everything else RAN. Including a signalled process killed by something other than
		// this function's own budget, which is deliberately not folded in with "not
		// installed": the two call for opposite actions, and "install gh" is the wrong
		// advice for a gh that was killed mid-call. Go reports -1 for a signal-terminated
		// process, which is not a value any caller here could read as "cannot tell", so it
		// becomes 1 with the signal named in Err. Also the I/O copy error the inverse test used to
		// misfile as never-started, which names a process that did run.
		r.Code = 1
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			if code := ee.ExitCode(); code >= 0 {
				r.Code = code
			}
		}
		// The DRAIN cause has to be stated even when gh wrote to stderr, or the guard below
		// lets gh's own words stand as the cause of a failure they had nothing to do with.
		// Adding cmd.WaitDelay made this arm reachable for a call that otherwise SUCCEEDED, so
		// a deprecation warning on a working `gh pr comment` rendered as "gh ran and failed
		// (warning: ...) — Step 0c can flush it once gh works": a cause nothing checked, the
		// very class this file exists to remove, introduced by the fix for the budget. The
		// drain reason goes first and gh's stderr is labelled as possibly unrelated.
		if errors.Is(err, exec.ErrWaitDelay) {
			drain := "the subprocess outlived the drain bound (" + err.Error() + ")"
			if r.Err != "" {
				drain += "; gh's own stderr, which may be unrelated: " + r.Err
			}
			r.Err = drain
		}
		if r.Err == "" {
			r.Err = err.Error()
		}
		return r
	}
}

// PRState is what the probe actually established. The Python collapses the first three into
// one — `if rc != 0 or not num:` prints "no PR yet" — and that assertion is wrong in two of
// the three cases, on a machine where this repo has already watched `gh` fail with a keychain
// TLS error while `git push` worked. An instrument that asserts a cause it never checked is
// the defect class this whole port keeps finding; here it tells an operator to create a PR
// when the actual problem is that gh cannot reach GitHub.
type PRState int

const (
	// PRFound: gh answered and named a PR.
	PRFound PRState = iota
	// NoPR: gh answered and said there are none — meaning it printed an EMPTY JSON ARRAY,
	// `[]`. Not empty stdout: `gh pr list --json number` is what the probe runs, and empty
	// output from it reaches json.Unmarshal("") and comes back GhFailed, because a descendant
	// sharing the fd can produce empty output just as easily as gh can. This comment said the
	// opposite for one commit — "it exits 0 with empty output for no PRs" — which was true of
	// the pre-`--json` command and was left standing when the probe changed. Verified against
	// the live repo: `--head` on a branch with no PR gives rc 0 and `[]`; on this branch it
	// gives `[{"number":5}]`. The bare `gh pr list` without `--json` does print nothing for no
	// PR, which is where the stale sentence came from, and is not the command used here.
	// `gh pr view` is the other contrast: it exits 1 for no PR, the same code it uses for
	// every other failure, which is why the probe does not use it.
	NoPR
	// GhUnrunnable: gh never started — not installed, not executable, or the wrong binary
	// format. NOT a kill: this said "or was killed" and no kill has reached it since shTimeout
	// grew an arm for the deadline, because a kill before the fork is a context error and one
	// after it is an ExitError, so both land on GhFailed. A state nothing can produce is worse
	// than an undocumented one — it tells a reader a cause has been accounted for.
	GhUnrunnable
	// GhFailed: gh ran and failed, or this function's own budget cancelled it. Auth, rate
	// limit, TLS, a network error, a repo it cannot see, a hang. Its stderr is the only thing
	// that says which, so it is carried, not dropped.
	GhFailed
	// NoBranch: there was no branch to look a PR up by — a detached HEAD, an unborn branch, or
	// a `git branch --show-current` that could not run. GH WAS NEVER ASKED. This state exists
	// because folding it into GhFailed made Note() say "gh ran and failed … Step 0c can flush
	// it once gh works" for a gh that was never invoked and a condition no amount of gh
	// working will fix — the operator has to check out a branch or pass -branch. That is this
	// file's own defect class, inside the file written to remove it, and the test for the path
	// asserted GhFailed, so it pinned the wrong answer.
	NoBranch
)

// Probe is the answer plus what it rests on.
type Probe struct {
	State  PRState
	Number string
	// Detail is gh's own stderr, or the exec error. Empty only when gh succeeded.
	Detail string
}

// Note says what an operator should do, naming the cause rather than guessing at it.
func (p Probe) Note() string {
	switch p.State {
	case PRFound:
		return fmt.Sprintf("PR #%s", p.Number)
	case NoPR:
		return "no PR for this branch yet — the normal case for a fresh branch, since the " +
			"push gate forces loop-then-push-then-PR"
	case GhUnrunnable:
		return "gh could not be run (" + p.Detail + ") — whether a PR exists is UNKNOWN, " +
			"not answered; install gh or post the report by hand"
	case NoBranch:
		return "no branch to look a PR up by (" + p.Detail + ") — gh was NOT asked, so " +
			"whether a PR exists is unknown; check out a branch or pass -branch. The report " +
			"is kept"
	default:
		return "gh ran and failed (" + p.Detail + ") — whether a PR exists is UNKNOWN, not " +
			"answered; the report is kept, and Step 0c can flush it once gh works"
	}
}

// FindPR asks whether this branch has a PR, in a way whose failure modes are distinguishable.
//
// `--state all` rather than open-only: a report belongs on the PR whose branch this is even
// after it merges, which is what `gh pr view` does and what Step 0c's backfill needs when the
// PR was merged between the loop finishing and the flush.
func FindPR(repo, branch string) Probe {
	// Why there is no branch, in git's own words when git is what failed. Defaults to the
	// answer for the case where git worked and simply reported no branch.
	gitDetail := "detached HEAD or an unborn branch"
	if branch == "" {
		// `git branch --show-current`, not `rev-parse --abbrev-ref HEAD`. Measured: on an
		// UNBORN branch — a repo with no commits yet — rev-parse exits 128 and prints the
		// literal "HEAD" on stdout, and on a detached HEAD it exits 0 and prints "HEAD",
		// so the caller needs a magic-string check to tell a branch named HEAD from no
		// branch at all. --show-current answers the question asked: the branch name, or
		// empty when there is none, exit 0 either way.
		switch r := sh(repo, "git", "branch", "--show-current"); {
		case !r.Runnable:
			gitDetail = "git could not be run: " + firstLine(r.Err)
		case r.Code != 0:
			gitDetail = "git branch --show-current failed: " + firstLine(r.Err)
		case !r.Drained:
			// A descendant co-wrote this buffer, so the "branch name" may be its bytes.
			gitDetail = "git's output could not be trusted: " + firstLine(r.Err)
		default:
			branch = r.Out
		}
	}
	if branch == "" {
		// No branch to ask about. Not an error: a detached HEAD has no PR by this route, and
		// the report still has to survive. NoBranch rather than GhFailed because gh has not
		// been invoked at this point, and `detail` carries git's own words when git is what
		// failed — the old single detail guessed "detached HEAD?" for an unrunnable git too.
		return Probe{State: NoBranch, Detail: gitDetail}
	}
	// The FULL JSON, not `-q .[0].number`. Asking gh to reduce its answer to a bare number
	// makes any byte on stdout a valid answer, and stdout is shared with every descendant that
	// inherited the fd: measured, a `gh pr list` that exited 0 printing NOTHING — meaning there
	// is no PR for this branch — next to a descendant that printed "42" yielded "42", and the
	// public disclosure was commented onto a PR that does not exist. `Drained` does not catch
	// it, because a descendant that writes and then EXITS leaves Wait with no error at all; it
	// only catches the one that outlives the drain bound. The oracle has to be the SHAPE of the
	// answer rather than the liveness of a pipe. A stray "42" makes `[{"number":9}]42` and
	// `[]42`, neither of which decodes, so contamination is reported as unknown instead of
	// being believed.
	r := sh(repo, "gh", "pr", "list", "--head", branch, "--state", "all", "--json", "number")
	switch {
	case !r.Runnable:
		return Probe{State: GhUnrunnable, Detail: r.Err}
	case r.Code != 0:
		return Probe{State: GhFailed, Detail: firstLine(r.Err)}
	case !r.Drained:
		return Probe{State: GhFailed, Detail: firstLine(r.Err)}
	}
	var prs []struct {
		Number int `json:"number"`
	}
	if err := json.Unmarshal([]byte(r.Out), &prs); err != nil {
		// Unknown, NOT "no PR": the one thing this probe must never do is assert a cause it
		// has not established, and an answer it cannot read establishes nothing.
		return Probe{State: GhFailed, Detail: fmt.Sprintf(
			"gh's answer could not be read as JSON (%v) — output was %q", err, firstLine(r.Out))}
	}
	if len(prs) == 0 {
		return Probe{State: NoPR}
	}
	return Probe{State: PRFound, Number: strconv.Itoa(prs[0].Number)}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return s
}

// PostParams is one publish. Body is rendered by Render and passed in, so the thing posted and
// the thing deferred are the same bytes by construction.
type PostParams struct {
	Repo, RunID, Branch string
	// Convergence is the DERIVED word, used for the label and written into the pending file's
	// "label still owed" note so Step 0c applies the same one.
	Convergence string
	Body        string
	Label       bool
	// Diag takes the operator-facing notes. Nothing here writes to stdout.
	Diag io.Writer
}

// Post publishes the report, or keeps it. The invariant, which the Python states and this
// keeps: FAIL ON A MISSING REPORT, NEVER ON A FAILED POST. Every path that cannot comment
// writes the pending file, so a transient GitHub error cannot discard the body and leave the
// push gate refusing on advice that could not succeed.
//
// Returns the pending path when one was written, and an error only when the report could not
// be preserved anywhere — which is the one outcome worth failing on.
func Post(p PostParams) (pending string, err error) {
	diag := func(format string, a ...any) {
		if p.Diag != nil {
			fmt.Fprintf(p.Diag, "pr-report: "+format+"\n", a...)
		}
	}

	probe := FindPR(p.Repo, p.Branch)
	if probe.State != PRFound {
		path, werr := WritePending(p.Repo, p.RunID, p.Convergence, p.Body)
		if werr != nil {
			return "", fmt.Errorf("%s, and the report could not be preserved: %w", probe.Note(), werr)
		}
		diag("%s — deferred to %s (Step 0c flushes it)", probe.Note(), path)
		return path, nil
	}

	c := sh(p.Repo, "gh", "pr", "comment", probe.Number, "--body", p.Body)
	if !c.Runnable || c.Code != 0 {
		// A transient GitHub error — 502, a rate limit, expired auth, the keychain TLS
		// failure this repo has already hit while `git push` worked — used to discard the
		// body and leave push-check refusing the push on advice that could not succeed.
		diag("comment failed on PR #%s: %s", probe.Number, firstLine(c.Err))
		path, werr := WritePending(p.Repo, p.RunID, p.Convergence, p.Body)
		if werr != nil {
			return "", fmt.Errorf("the comment failed on PR #%s and the report could not be "+
				"preserved: %w", probe.Number, werr)
		}
		diag("kept the report at %s for Step 0c", path)
		// The label still goes on. The Python's `if a.label:` sits AFTER both the posted and
		// the comment-failed branches, so it runs whenever a PR number is known, and nothing
		// about labelling depends on the comment having landed: it is a separate `gh pr edit`
		// against a PR this probe already found. Returning here instead dropped the
		// at-a-glance signal for exactly the runs whose report is hardest to find — the ones
		// whose comment failed.
		if p.Label {
			applyLabel(p, probe.Number, diag)
		}
		return path, nil
	}
	diag("posted to PR #%s", probe.Number)

	// Also locally, so the push gate rests on an artifact that does not depend on a SECOND
	// successful remote read. Without this, a post that succeeded while the read-back failed
	// refused the push forever and posted a duplicate on every retry.
	path, werr := WritePending(p.Repo, p.RunID, p.Convergence, p.Body)
	if werr != nil {
		// The report is POSTED; the local copy is belt-and-braces. Say so and carry on.
		diag("posted, but the local copy could not be written: %v", werr)
	} else {
		pending = path
	}

	if p.Label {
		applyLabel(p, probe.Number, diag)
	}
	return pending, nil
}

// applyLabel moves the run's at-a-glance signal, removing the other three so two cannot stand
// at once. Every call is best-effort: a label is a convenience, and failing the publish over
// one would mean losing a posted report to a missing `labels` scope.
func applyLabel(p PostParams, num string, diag func(string, ...any)) {
	conv := p.Convergence
	if conv == "" {
		conv = "unknown"
	}
	want, ok := Labels[conv]
	if !ok {
		// record.Convergence answers only the four words, so this is unreachable from the
		// record — but Convergence is a plain string on the way in, and a caller that
		// mistypes it would otherwise take a nil-map read.
		diag("no label for convergence %q — none applied", conv)
		return
	}
	// Every one of these is checked, which four of the five were not. `// Every call is
	// best-effort` described the add below and nothing else: a removal that failed left the
	// PREVIOUS run's label standing beside the new one, the add still succeeded, and the diag
	// still said the label had moved — a PR advertising two convergences at once with nothing
	// anywhere saying so. Discarding the stream that says why a gh call failed is the exact
	// defect this whole slice exists to stop doing.
	if r := sh(p.Repo, "gh", "label", "create", want.Name, "--color", want.Colour,
		"--description", want.Description); !r.Runnable || r.Code != 0 {
		// Ordinary: the label already exists from a previous run. Noted, not treated as a
		// failure, because the add below is what actually has to work.
		diag("label %s not created (it may already exist): %s", want.Name, firstLine(r.Err))
	}
	for _, l := range Labels {
		if l.Name == want.Name {
			continue
		}
		if r := sh(p.Repo, "gh", "pr", "edit", num, "--remove-label", l.Name); !r.Runnable || r.Code != 0 {
			diag("label %s not removed, so two may now stand at once: %s", l.Name, firstLine(r.Err))
		}
	}
	if r := sh(p.Repo, "gh", "pr", "edit", num, "--add-label", want.Name); !r.Runnable || r.Code != 0 {
		diag("label %s failed: %s", want.Name, firstLine(r.Err))
		return
	}
	diag("label %s", want.Name)
}

// WritePending writes the report where Step 0c will find it: the fallback channel for
// "nowhere to post it right now". push-check looks for this file when no PR comment carries
// the run's marker, so writing it is also what keeps a transient GitHub error from blocking
// the push permanently.
func WritePending(repo, runID, conv, body string) (string, error) {
	r := sh(repo, "git", "rev-parse", "--path-format=absolute", "--git-common-dir")
	if !r.Runnable {
		return "", fmt.Errorf("git could not be run (%s) — nowhere to defer the report to", r.Err)
	}
	// !Drained too: this Out becomes a filesystem PATH, so a descendant's bytes in it would
	// write the report somewhere nobody looks.
	if r.Code != 0 || r.Out == "" || !r.Drained {
		return "", fmt.Errorf("no git dir in %s — nowhere to defer the report to", repo)
	}
	// The run id reaches a PATH here, and it arrives from the caller's -run-id. A traversing
	// id would write outside the repo: filepath.Join(gitdir, "info/...-report.../../../x.md")
	// resolves wherever the dots lead. The record's own validator is the gate; this is the
	// guard travelling with the interpolation, the way push.reportProbe's does.
	if !ValidRunID(runID) {
		return "", fmt.Errorf("run id %q is not a plain id — refusing to build a path from it", runID)
	}
	path := filepath.Join(r.Out, fmt.Sprintf(PendingName, runID))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	if conv == "" {
		conv = "unknown"
	}
	// The label block only runs when a PR exists, and on a fresh branch there is none — the
	// common case, since the push gate forces loop-then-push-then-PR. Say the label is still
	// owed so Step 0c applies it.
	note := fmt.Sprintf("\n<!-- review-loop: label review:%s still owed; Step 0c applies it "+
		"with `looper pr-report --run-id %s --label` -->\n", conv, runID)
	if err := os.WriteFile(path, []byte(body+note), 0o644); err != nil {
		return "", err
	}
	return path, nil
}

// ValidRunID mirrors push.ValidRunID. Duplicated rather than imported: internal/push imports
// nothing from here and must not start, or the two halves of the gate become one package that
// can agree with itself by accident.
//
// Exported because the COMMAND needs it too: the run id reaches a public PR comment raw, in
// the marker and the summary line, and `record.Load` places no constraint on `run_id`. Checked
// at the command rather than in Render so Render stays byte-comparable with the oracle, and
// because cell()ing the id would rewrite the marker push.Fingerprint matches exactly.
func ValidRunID(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for _, c := range s {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}
