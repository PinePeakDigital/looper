// Package push decides whether review-loop Step 14 may auto-push, and why. It is a port of
// push-check.py.
//
// The decision is split in two on purpose. "We stopped looking" is a DISCLOSURE: a capped or
// halted run pushes and owes the line record.Disclosure derives, because a cap that strands
// commits just moves the decision back to a human every time. "It is broken" is a BLOCK: a
// recorded broken outcome, a blocked evidence gate, an unresolved finding, the default
// branch. Conflating the two is what made the old `--clean-exit` flag the whole gate.
package push

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/pinepeakdigital/looper/internal/record"
)

// BrokenOutcomes are the recorded outcomes that mean the tree is the problem, not the
// review's completeness. They block, and they outrank a converged review.
//
// This channel exists because removing `--clean-exit` deleted the only way a Step 9 test
// failure reached the decision, and nothing replaced it: a run that recorded test-failure
// with a failed gate was permitted, with the reason "converged, evidence gate ok".
var BrokenOutcomes = []string{"test-failure", "blocked", "abandoned"}

// reportMarker is the comment pr-report.py writes at the top of every report body. Matching
// it is how this checks the report actually reached a reader.
const reportMarker = "<!-- review-loop:run=%s -->"

// pendingFmt is relative to the git common dir. The push gate forces the order
// loop -> push -> create PR on a fresh branch, so the report usually has no PR to land on
// yet and pr-report.py writes it here instead.
const pendingFmt = "info/review-loop-pending-report.%s.md"

// State is everything Decide weighs. A struct rather than the Python's eight positional
// parameters, three of which are bools in a row: `decide(conv, gate, skip, branch, default,
// upstream, outcome, unreported)` is a signature where transposing two arguments silently
// inverts the default-branch guard, and its own selftest passes three of them as `*ok`.
type State struct {
	// Convergence is derived from the record, never asserted — see record.Convergence.
	Convergence string
	// GateState is "passed", "skipped" or "blocked".
	GateState      string
	UnresolvedSkip bool
	Branch         string
	DefaultBranch  string
	UpstreamExists bool
	// Outcome is the run's recorded self-report. It may only block, never permit.
	Outcome string
	// Unreported is the reason this run's report has not reached a reader, or "" when it
	// has. A reason rather than a bool because it is the message the operator acts on.
	Unreported string
}

// Decide answers push/no-push with the reason. First failing check wins, mirroring SKILL.md
// Step 14's "When NOT to auto-push".
func Decide(s State) (bool, string) {
	for _, bad := range BrokenOutcomes {
		if s.Outcome == bad {
			return false, fmt.Sprintf("run recorded %s — broken, not merely unfinished", bad)
		}
	}
	// The report is the entire consideration for which a non-converged run is allowed to
	// push at all, and prose instructions to post it are the ones that get skipped — which
	// is why pr-report.py is a script. Required on EVERY terminal exit, not only the
	// non-converged ones: the incident that motivated this redesign was a CLEAN exit on a
	// fresh branch whose summary never reached the PR.
	if s.Unreported != "" {
		return false, s.Unreported
	}
	if s.GateState == "blocked" {
		return false, "evidence gate blocked or hit its restart cap"
	}
	if s.UnresolvedSkip {
		return false, "a 50-79 finding was skipped without 'remember as dismissal' — unresolved"
	}
	if s.Branch == "" || s.DefaultBranch == "" {
		// Fail closed: an unknown name would skip the default-branch guard below.
		return false, "current or default branch unknown — refusing to push"
	}
	if s.Branch == s.DefaultBranch {
		return false, fmt.Sprintf("branch is the default branch (%s) — never auto-push to it", s.Branch)
	}
	// First push of a new feature branch is the normal case, not a block: Step 14 pushes it
	// with `-u origin`. The old "don't infer an upstream" rule blocked every fresh branch.
	where := "feature branch with upstream"
	if !s.UpstreamExists {
		where = "feature branch has no upstream yet — push with -u"
	}
	if s.Convergence == record.Converged {
		return true, fmt.Sprintf("converged, evidence gate ok, %s", where)
	}
	conv := s.Convergence
	if conv == "" {
		conv = record.Unknown
	}
	return true, fmt.Sprintf("%s (review not finished), evidence gate ok, %s", conv, where)
}

// Fingerprint is the set of strings a real rendered report must contain for THIS run.
//
// The marker alone was not enough: `printf '<!-- review-loop:run=<id> -->' > <pending>` is 37
// bytes for a real run id — 25 static plus the 12 hex characters runlog.py generates — and it
// satisfied the gate, making it CHEAPER to forge than the `disclosed --where "trust me"` row
// it replaced. (push-check.py's docstring says 38, which is the marker plus the newline
// pr-report.py writes after it; the printf shown here writes no newline.) Requiring the run line too means the numbers have to agree
// with the cycle rows, which cannot be produced without rendering from the record — the
// point being that this is a property of the artifact, not a claim about it.
//
// The separator is U+00B7 MIDDLE DOT, as pr-report.py writes it. An ASCII `·`-alike here
// would make every real report read as missing.
func Fingerprint(r *record.Run) []string {
	// record.AgentsSpent, not a local sum. The figure has to equal the one pr-report.py
	// rendered — `sum(c.get("agents") or 0 for c in cycles)` — and equal what Disclosure
	// says, or a report disagrees with the verdict printed beside it. `cycles_of`'s docstring
	// in runlog.py records that divergence, and records in the same breath that it was
	// REPRODUCED IN A FIXTURE and never seen on a real PR. An earlier version of this comment
	// called it "a real incident", which is the blurring that docstring exists to warn about;
	// record.AgentsSpent carries the corrected framing and the argument that needs no incident.
	return []string{"## review-loop",
		fmt.Sprintf("%d cycle(s) · %d agent(s)", len(r.Cycles), r.AgentsSpent())}
}

// ReportLanded reports whether this run's rendered report has reached somewhere a reader
// will see it.
//
// No "cannot tell" result: no gh and no git both read as NOT landed, which fails closed, and
// pr-report guarantees the body lands in one of the two places checked — a PR comment or
// this run's pending file — including when the post fails. So "neither" means pr-report did
// not run, which is the one thing this gate exists to catch.
func ReportLanded(runID string, r *record.Run, repo string) bool {
	landed, _ := reportProbe(runID, r, repo)
	return landed
}

// reportProbe is ReportLanded plus the one thing the bool cannot carry: whether either probe
// could RUN. A genuinely unposted report, a missing or non-executable `gh`/`git`, and a fired
// timeout all mean "not landed" — correctly, the gate fails closed — but they call for
// different actions, and the refusal text cannot distinguish them, because parity_test.go
// compares it for exact equality against push-check.py's. So the distinction goes to stderr,
// where neither implementation writes anything and nothing parses.
//
// attributable is whether the not-landed answer has a cause the refusal text already covers:
// a probe ran and said no, or the run id was refused outright. It is FALSE only when neither
// probe could run.
//
// That is NOT the only state an operator cannot act on from the refusal alone, and an earlier
// version of this comment said it was. Measured: with a store that does not hold the run — a
// wrong -store, a missing file, a mistyped -run-id — record.Load returns an empty map and NO
// error, so Fingerprint asks for `0 cycle(s) · 0 agent(s)`, git answers, `attributable` is
// true, and the refusal's stated remedy cannot fix it: posting the report again renders the
// same two lines off the same empty record. Check now writes a second Diag note for that
// state, naming the run and the store it read — see the `r == nil` block below. An earlier
// version of these lines said "nothing is written to Diag ... covering that is a separate
// change", which stopped being true one cycle later and one function down.
//
// Named for the question it answers rather than for "could a probe be asked", which is what
// the earlier name `couldAsk` claimed while the refused-run-id path asks nothing.
func reportProbe(runID string, r *record.Run, repo string) (landed, attributable bool) {
	// The guard travels with the path interpolation, not with one caller. It was in Check
	// only, which made it a property of whoever remembered to call Check first — and the
	// tests in this package already call ReportLanded directly, bypassing it. Measured:
	// filepath.Join(gitdir, fmt.Sprintf(pendingFmt, "../../../../../../tmp/evil")) resolves
	// to /Users/<user>/tmp/evil.md, outside the repo. Same lesson as misplacedRunField in
	// internal/record: guard the mechanism, not the spelling of its one current caller.
	//
	// Fails closed rather than erroring, because every other way this function cannot find
	// the report already does.
	if !ValidRunID(runID) {
		// Attributable: the question was well-formed enough to answer and the answer is no.
		// Nothing is asked here, which is why `couldAsk` needed this comment as an exception
		// and `attributable` does not.
		return false, true
	}
	needles := append([]string{fmt.Sprintf(reportMarker, runID)}, Fingerprint(r)...)
	if rc, out := sh(repo, "gh", "pr", "view", "--json", "comments", "-q", ".comments[].body"); rc == 0 {
		attributable = true
		if containsAll(out, needles) {
			return true, true
		}
	}
	rc, gitdir := sh(repo, "git", "rev-parse", "--path-format=absolute", "--git-common-dir")
	if rc != 0 || gitdir == "" {
		return false, attributable
	}
	// No attributable assignment here: git answered, so both paths below return true outright.
	body, err := os.ReadFile(filepath.Join(gitdir, fmt.Sprintf(pendingFmt, runID)))
	if err != nil {
		return false, true
	}
	return containsAll(string(body), needles), true
}

func containsAll(hay string, needles []string) bool {
	for _, n := range needles {
		if !strings.Contains(hay, n) {
			return false
		}
	}
	return true
}

// upstreamExists is routed through sh, which bounds the call and swallows a missing git. As
// a raw subprocess call this was the only crash path in push-check.py: it is evaluated as an
// argument to decide(), so an empty PATH produced a traceback and an empty stdout where
// Step 14 expects JSON.
func upstreamExists(repo string) bool {
	rc, _ := sh(repo, "git", "rev-parse", "--abbrev-ref", "@{upstream}")
	return rc == 0
}

// sh runs prog in repo and returns its exit code and trimmed stdout. A command that cannot
// run at all — missing binary, timeout, not executable — reads as exit 1, i.e. "cannot
// tell", which every caller here treats as the blocking answer.
//
// prog is a separate parameter rather than args[0] for two reasons. It makes "the program
// name is a literal at every call site" visible in the signature, which is what makes
// semgrep's dangerous-exec-command finding here a false positive rather than a judgement
// call — nothing reachable passes a variable. And args[0] panicked on an empty slice, which
// the variadic signature made it possible to write.
func sh(repo, prog string, args ...string) (int, string) {
	return shTimeout(repo, shBudget, prog, args...)
}

// shBudget is the Python's `timeout=20`. Named and threaded through shTimeout so the
// timeout-kill branch can be exercised in milliseconds: with the constant inline, the one
// branch that decides whether a hung `gh` fails open or closed could only be tested by a
// twenty-second test, which is why it had no test at all.
const shBudget = 20 * time.Second

func shTimeout(repo string, budget time.Duration, prog string, args ...string) (int, string) {
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	cmd := exec.CommandContext(ctx, prog, args...)
	cmd.Dir = repo
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			if code := ee.ExitCode(); code >= 0 {
				return code, strings.TrimSpace(string(out))
			}
			// SIGNALLED, which is what the context's own kill looks like. Go reports -1 for
			// a signal-terminated process; the Python's sh returns 1 for TimeoutExpired like
			// any other SubprocessError, and -1 is not a value any caller here could read
			// as "cannot tell". Measured: `sleep 5` under a one-millisecond context gives
			// err "signal: killed", errors.As true, ExitCode() -1.
			//
			// Checked here rather than on ctx.Err(), which would be narrower AND redundant:
			// a context kill satisfies both, and a signal from outside the context satisfies
			// only this. An earlier version tested ctx.Err() first and the two together
			// masked each other — removing either left the suite green, which is how a
			// guard with no test behind it looks.
			return 1, strings.TrimSpace(string(out))
		}
		return 1, ""
	}
	return 0, strings.TrimSpace(string(out))
}

// Result is what the subcommand prints, field for field as push-check.py's JSON. Disclose is
// a pointer so a converged run emits `null` rather than `""` or no key at all; SKILL.md
// Step 14 reads this, and the two are not the same answer to "what must the PR say".
type Result struct {
	Push        bool    `json:"push"`
	Reason      string  `json:"reason"`
	Convergence string  `json:"convergence"`
	Disclose    *string `json:"disclose"`
}

// runIDRe bounds what may be interpolated into pendingFmt. The id is a path segment there,
// so `../../` in one would send the gate looking for its evidence outside the git dir. The
// Python has the same hole and the ids it writes are hex, so nothing has ever exercised it;
// refusing the shape costs a line and keeps the evidence path inside the repo.
var runIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// ValidRunID reports whether a run id may be interpolated into a path. Exported so
// ReportLanded and Check share ONE definition of the bound rather than each carrying a copy
// that can drift — the shape of defect this repo has already paid for twice.
func ValidRunID(id string) bool { return runIDRe.MatchString(id) }

// CheckParams is what Check needs. A struct for the same reason State is one, and the comment
// on State applies verbatim: the previous signature was seven positionals with Branch and
// DefaultBranch adjacent and both plain strings, so transposing them silently inverted the
// default-branch guard — in the function that receives them straight from flag parsing, where
// nothing in the type system would have caught it.
type CheckParams struct {
	// Store is the run record's path; see record.StorePath for the default.
	Store string
	RunID string
	// GateState is "passed", "skipped" or "blocked".
	GateState      string
	UnresolvedSkip bool
	Branch         string
	DefaultBranch  string
	// Repo is where the git and gh facts are read from.
	Repo string
	// Diag receives a line for each DECISION whose stated reason is the wrong instruction for
	// the actual cause: the report check could not be RUN at all, or no row for the run was
	// readable in the store. Neither can be said in the reason itself, which parity pins
	// exactly. "Decision", not "refusal": the empty-record note fires before Decide and
	// the empty record can GRANT, which is the case most worth saying something about.
	// Optional: nil writes nothing. It must NOT be the stream the Result is encoded to —
	// stdout is a machine-readable contract.
	Diag io.Writer
}

// Check is the whole decision: read the run, derive what the record says, look for the
// report, and weigh it.
func Check(p CheckParams) (Result, error) {
	// Checked here as well as in ReportLanded, and deliberately not only there: this is the
	// boundary where a bad id can still be reported as an ERROR naming it, where
	// ReportLanded can only fail closed and say nothing.
	if !ValidRunID(p.RunID) {
		return Result{}, fmt.Errorf("run id %q is not a plain identifier", p.RunID)
	}
	runs, err := record.Load(p.Store, 0)
	if err != nil {
		return Result{}, err
	}
	// A run id absent from the store is UNKNOWN, never converged. Absence is the cheapest
	// thing to produce, so it must buy the same disclosure a recorded unfinished run owes.
	r := runs[p.RunID]
	if r == nil {
		r = &record.Run{ID: p.RunID}
		if p.Diag != nil {
			// Every derivation below now comes off an EMPTY record, and each one reads as a
			// legitimate answer: convergence is `unknown`, Finish is nil so no recorded
			// outcome can block, and the required fingerprint collapses to the two lines
			// pr-report.py renders for a run with no cycles. On this store that collides
			// with a real report for 22 of 41 runs, so this can GRANT as easily as refuse.
			//
			// "No readable row", not "not found": the two causes this reaches are a run that
			// was never written (a wrong -store, a mistyped -run-id) and a run all of whose
			// rows were torn — record.Load drops a syntactically torn line with no error and
			// no map entry, so `grep` can find the id in a store this note names. Saying
			// "was not found in <store>" sent an operator to check a store that does contain
			// it. The note states what it has actually checked: nothing readable, and which
			// file it read.
			//
			// "the decision below", not "the refusal below": this fires before Decide.
			fmt.Fprintf(p.Diag, "push-check: no readable row for run %q in %q, so convergence, "+
				"the outcome and the required report fingerprint are all derived from an empty "+
				"record; the decision below rests on nothing the record said\n",
				p.RunID, p.Store)
		}
	}
	// The derivations are read FIRST, and each refuses a run whose record did not decode.
	// That ordering is load-bearing rather than tidy: Fingerprint reads Cycles, which is
	// SHORT by one row for every cycle row that failed, so a report gate run on such a
	// record would be checking numbers no report could ever match. An explicit Err check
	// here as well would be defence with no test behind it — Convergence already returns
	// the error, so removing one changed nothing — and the assertion in push_test.go pins
	// the order instead, by failing if anything consults gh before the derivations.
	//
	// The Python cannot reach this state at all; it coerces instead of refusing. So there
	// is no parity answer to match and the port's own rule decides it: an unreadable record
	// is not evidence that a review finished.
	conv, err := r.Convergence()
	if err != nil {
		return Result{}, err
	}
	// Dead today, deliberately kept. Disclosure can only error from its own Convergence call
	// — already handled above, and the two cannot diverge — or from its two arms that
	// TestConvergenceAnswersOnlyTheFourWords proves unreachable. It stops being dead in the
	// commit that adds a fifth convergence value, which is exactly when a missing check here
	// would render a headless disclosure instead of refusing. Noted rather than deleted
	// because the proof of deadness lives in another package's test, so a reader here has no
	// way to see it.
	disclose, err := r.Disclosure()
	if err != nil {
		return Result{}, err
	}
	var outcome string
	if r.Finish != nil {
		outcome = r.Finish.Outcome
	}
	var unreported string
	if landed, attributable := reportProbe(p.RunID, r, p.Repo); !landed {
		unreported = "this run's report has not reached the PR or the pending-report file — " +
			"run pr-report.py --post first"
		if !attributable && p.Diag != nil {
			// The refusal below is correct either way — fail closed — but "go post the
			// report" is the wrong instruction when the real problem is that neither probe
			// ran. Said here rather than in the reason, which parity pins exactly.
			fmt.Fprintf(p.Diag, "push-check: neither `gh pr view` nor `git rev-parse` could be "+
				"run in %s (missing binary, not executable, or killed at the %s budget), so "+
				"whether the report landed could not be determined; the refusal below fails "+
				"closed and may not be about the report at all\n", p.Repo, shBudget)
		}
	}
	push, reason := Decide(State{
		Convergence:    conv,
		GateState:      p.GateState,
		UnresolvedSkip: p.UnresolvedSkip,
		Branch:         p.Branch,
		DefaultBranch:  p.DefaultBranch,
		UpstreamExists: upstreamExists(p.Repo),
		Outcome:        outcome,
		Unreported:     unreported,
	})
	res := Result{Push: push, Reason: reason, Convergence: conv}
	if disclose != "" {
		res.Disclose = &disclose
	}
	return res, nil
}

// Encode renders the result as the subcommand prints it. HTML escaping off, because
// encoding/json escapes <, > and & by default and json.dumps does not: a disclosure or a
// branch name carrying one would read as \u003c in a line an operator is meant to act on.
func (res Result) Encode() ([]byte, error) {
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(res); err != nil {
		return nil, err
	}
	return []byte(b.String()), nil // Encode already appends the newline
}
