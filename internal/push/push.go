// Package push decides whether review-loop Step 14 may auto-push, and why. It is a port of
// push-check.py.
//
// The decision is split in two on purpose. "We stopped looking" is a DISCLOSURE: a capped or
// halted run pushes and owes the line record.Disclosure derives, because a cap that strands
// commits just moves the decision back to a human every time. "It is broken" is a BLOCK: a
// recorded broken outcome, a blocked evidence gate, an unresolved finding, the default
// branch. So is "there is no evidence": no readable record, or a record never finished.
// Conflating the two is what made the old `--clean-exit` flag the whole gate.
package push

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/pinepeakdigital/looper/internal/evidence"
	"github.com/pinepeakdigital/looper/internal/record"
	"github.com/pinepeakdigital/looper/internal/run"
)

// BrokenOutcomes are the recorded outcomes that mean the tree is the problem, not the
// review's completeness. They block, and they outrank a converged review.
//
// This channel exists because removing `--clean-exit` deleted the only way a Step 9 test
// failure reached the decision, and nothing replaced it: a run that recorded test-failure
// with a failed gate was permitted, with the reason "converged, evidence gate ok".
var BrokenOutcomes = []string{"test-failure", "blocked", "abandoned"}

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
	// NoRecord is true when the run id resolved to no readable row at all. Derived from the
	// loaded record, never asserted by the caller — same rule as Convergence and Outcome.
	//
	// It blocks, and that is a DELIBERATE DIVERGENCE from push-check.py, which permits here
	// with a disclosure. See the enumerated divergence in parity_test.go. The Python's
	// behaviour follows from the design's own split — `unknown` means "we stopped looking",
	// which discloses and pushes (once the run has finished; see Unfinished) — but an empty
	// record is not a run that stopped looking, it
	// is the absence of any evidence that a run happened. Everything the gate reads comes off
	// it: convergence derives `unknown`, no recorded outcome can block, and the required
	// report fingerprint collapses to the two lines pr-report.py renders for a run with no
	// cycle rows, which is about HALF the runs in the author's live store (22 of 45 when last counted, 2026-10-10 19:50 — the store gains runs several times a day, so re-derive rather than trust the figure: group its rows by run_id and count those with no `cycle` row). Measured: changing only $HOME
	// turned a recorded `test-failure` refusal into a granted push. That makes misresolving
	// the store the cheapest way to make this gate say yes, which is the one thing it exists
	// to prevent.
	NoRecord bool
	// Unfinished is the refusal for a run that has a record but no finish row, "" when it
	// has one. A reason rather than a bool, like Unreported, so it can name the run. Step 14
	// closes the record before it posts the report or asks this, so an unfinished run here
	// skipped a step — and the report it posted rendered no outcome and no tier. Blocks; a
	// DELIBERATE DIVERGENCE from push-check.py, which never looked.
	//
	// A session that died mid-loop lands here too, but its way out is NOT a finish: a
	// `cycle-limit` finish would push, and stamp as reviewed, a tip its cycles only partly
	// covered. It is `looper record abandon` on the dead run, then a fresh run. `abandoned`
	// is a broken outcome, so the dead run's id stays blocked, but the fresh run has an id of
	// its own and the commits are not stranded.
	Unfinished string
}

// Decide answers push/no-push with the reason. First failing check wins, mirroring SKILL.md
// Step 14's "When NOT to auto-push".
func Decide(s State) (bool, string) {
	// First, because every check below it reads something off the record, and if there is no
	// record their answers are vacuous rather than reassuring. Ordered ahead of the outcome
	// loop for that reason, not because absence is worse than a recorded failure.
	if s.NoRecord {
		return false, "no readable record for this run — convergence, the outcome and the " +
			"report fingerprint would all come off an empty record; check -store and -run-id"
	}
	if s.Unfinished != "" {
		return false, s.Unfinished
	}
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
// No "cannot tell" result: no gh and no home directory both read as NOT landed, which fails closed, and
// pr-report guarantees the body lands in one of the two places checked — a PR comment or
// this run's pending file — including when the post fails, unless there is no home
// directory to hold the pending file, which reads as not landed too. So "neither" means pr-report did
// not run, which is the one thing this gate exists to catch.
func ReportLanded(runID string, r *record.Run, repo string) bool {
	landed, _ := reportProbe(runID, r, repo, currentBranch(repo))
	return landed
}

// reportProbe is ReportLanded plus the one thing the bool cannot carry: whether either probe
// could RUN. A genuinely unposted report, a missing or non-executable `gh`, no home directory
// and a fired timeout all mean "not landed" — correctly, the gate fails closed — but they call for
// different actions, and the refusal text cannot distinguish them, because parity_test.go
// pins it exactly against push-check.py's frozen output. So the distinction goes to stderr,
// where neither implementation writes anything and nothing parses.
//
// attributable is whether the not-landed answer has a cause the refusal text already covers:
// a probe ran and said no, or the run id was refused outright. It is FALSE only when gh could
// not answer and there is no home directory to hold the pending file.
//
// That is NOT the only state an operator cannot act on from the refusal alone, and an earlier
// version of this comment said it was. Measured: with a store that does not hold the run — a
// wrong -store, a missing file, a mistyped -run-id — record.Load returns an empty map and NO
// error, so Fingerprint asks for `0 cycle(s) · 0 agent(s)`, the pending file is read, `attributable` is
// true, and the refusal's stated remedy cannot fix it: posting the report again renders the
// same two lines off the same empty record. Check now writes a second Diag note for that
// state, naming the run and the store it read — see the `r == nil` block below. An earlier
// version of these lines said "nothing is written to Diag ... covering that is a separate
// change", which stopped being true one cycle later and one function down.
//
// Named for the question it answers rather than for "could a probe be asked", which is what
// the earlier name `couldAsk` claimed while the refused-run-id path asks nothing.
//
// branch is the branch whose PR's comments are read; "" skips the PR half, as a gh that could
// not run does. The two differ for the operator: with no branch known (a detached HEAD, an
// unborn branch, a git that could not say) gh was never asked, so a Diag note must not blame
// gh for not running. `gh pr view` used to find the PR from gh's own reading of the checkout.
func reportProbe(runID string, r *record.Run, repo, branch string) (landed, attributable bool) {
	landed, attributable, _ = probeReport(runID, r, repo, branch)
	return landed, attributable
}

// probeReport is reportProbe plus ghWhy: one clause saying which gh call failed, or "" when
// gh answered or was never asked (no branch). attributable cannot carry it, because a
// readable pending file makes the refusal attributable whether or not the PR half answered,
// and a report already posted to the PR is then invisible to the gate with no sign of why.
func probeReport(runID string, r *record.Run, repo, branch string) (landed, attributable bool, ghWhy string) {
	// The guard travels with the interpolation: evidence.Marker and evidence.PendingPath
	// refuse an id that is not plain, so no caller can forget it. It once sat in Check only,
	// and a run id of six `../` segments then resolved the pending path outside the repo.
	//
	// Fails closed rather than erroring, because every other way this function cannot find
	// the report already does.
	marker, ok := evidence.Marker(runID)
	if !ok {
		// Attributable: the question was well-formed enough to answer and the answer is no.
		// Nothing is asked here, which is why `couldAsk` needed this comment as an exception
		// and `attributable` does not.
		return false, true, ""
	}
	needles := append([]string{marker}, Fingerprint(r)...)
	out, ok, ghWhy := prComments(repo, branch)
	if ok {
		attributable = true
		if containsAll(out, needles) {
			return true, true, ghWhy
		}
	}
	// The run dir is keyed by the run id alone, so no git is needed to find it.
	pending, ok := evidence.PendingPath(runID) // the id passed evidence.Marker above
	if !ok {
		return false, attributable, ghWhy // no home directory: nowhere to look
	}
	// A report deferred by a looper older than the run dirs is not moved here: that runs git
	// with no budget on the gate's path. `record pending` moves it (Step 0c runs it before any
	// push), and until then it reads as not landed, which fails closed.
	// No attributable assignment here: the path resolved, so both paths below return true outright.
	body, err := os.ReadFile(pending)
	if err != nil {
		return false, true, ghWhy
	}
	return containsAll(string(body), needles), true, ghWhy
}

// prComments is the bodies of the comments on branch's PR, and whether gh ANSWERED. Two REST
// calls — find the PR, then read its comments — where `gh pr view --json comments` was one
// GraphQL call: the friction log records a sandbox in which gh's GraphQL failed with a
// keychain TLS error while REST worked, and the gate then refused on advice that could not
// help. The PR is evidence.PRsForBranch's newest, the one report.FindPR posts to.
//
// "No PR for this branch" is an answer (ok, no comments): gh was asked and said so, so the
// refusal that follows — post the report — has its cause. `gh pr view` exited 1 there, the
// same code as every failure, so that state used to read as "could not ask".
//
// An answer that does not decode as a PR list is no answer, for the reason report.FindPR
// gives: stdout is shared with any descendant that inherited it.
//
// why names the gh call that failed ("" when gh answered, or was never asked for want of a
// branch), for the Diag note in Check.
func prComments(repo, branch string) (body string, ok bool, why string) {
	if branch == "" {
		return "", false, ""
	}
	rc, out := sh(repo, "gh", "api", evidence.PRsForBranch(branch))
	if rc != 0 {
		return "", false, fmt.Sprintf("listing the PR failed (gh status %d — a status sh also reports when gh never started, was killed or left its output open)", rc)
	}
	var prs []struct {
		Number int `json:"number"`
	}
	if json.Unmarshal([]byte(out), &prs) != nil {
		return "", false, "listing the PR returned something that is not a PR list"
	}
	if len(prs) == 0 {
		return "", true, ""
	}
	rc, out = sh(repo, "gh", "api", "--paginate", evidence.PRComments(prs[0].Number), "--jq", ".[].body")
	if rc != 0 {
		return out, false, fmt.Sprintf("reading the PR's comments failed (gh status %d — a status sh also reports when gh never started, was killed or left its output open)", rc)
	}
	return out, true, ""
}

// currentBranch is git's current branch in repo, or "" when there is none (a detached HEAD)
// or git cannot say. `git branch --show-current`, the spelling report.FindPR measured: it
// answers the name or nothing, exit 0 either way, where `rev-parse --abbrev-ref HEAD` prints
// the literal "HEAD" for a detached one.
func currentBranch(repo string) string {
	if rc, out := sh(repo, "git", "branch", "--show-current"); rc == 0 {
		return out
	}
	return ""
}

// defaultBranch is the branch origin's HEAD points at, or "" when git cannot say: no origin,
// or an origin/HEAD never set (a clone sets it; `git remote add` + fetch does not, and
// `git remote set-head origin -a` does). Deliberately local: asking the remote would make
// the gate's answer depend on the network, and "" already fails closed in Decide.
func defaultBranch(repo string) string {
	rc, out := sh(repo, "git", "symbolic-ref", "--quiet", "--short", "refs/remotes/origin/HEAD")
	if rc != 0 {
		return ""
	}
	name, ok := strings.CutPrefix(out, "origin/")
	if !ok {
		return ""
	}
	return name
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
	r := run.Run(repo, budget, prog, args...)
	out := strings.TrimSpace(string(r.Out))
	switch {
	// Never started, or an I/O failure reading a process that did: the Python's sh returns 1
	// with no output for either, as for any other SubprocessError.
	case !r.Started, r.Error != nil && r.Code == 0 && r.Drained:
		return 1, ""
	// The child exited cleanly while a descendant (a credential helper) held the pipes past
	// run's drain bound. Its output may be the descendant's bytes, not gh's answer, so this
	// cannot tell. Before the shared runner push had no drain bound at all, and such a
	// descendant held the gate until it exited, whatever the budget said.
	case !r.Drained:
		return 1, ""
	// SIGNALLED, which is what the budget's own kill looks like. -signal is not a value any
	// caller here could read as "cannot tell"; the Python's sh returns 1 for TimeoutExpired
	// like any other SubprocessError. Checked on the code rather than on Expired, which
	// would be narrower: a signal from outside the budget satisfies only this.
	case r.Code < 0:
		return 1, out
	}
	return r.Code, out
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
	// Branch and DefaultBranch are read from git in Repo when "" — the current branch, and
	// the branch origin/HEAD names. Whatever git cannot name stays "" and refuses.
	Branch        string
	DefaultBranch string
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
	if !evidence.ValidRunID(p.RunID) {
		return Result{}, fmt.Errorf("run id %q is not a plain identifier", p.RunID)
	}
	runs, err := record.Load(p.Store, 0)
	if err != nil {
		return Result{}, err
	}
	// A run id absent from the store derives UNKNOWN, never converged — absence is the
	// cheapest thing to produce, so it cannot buy a better answer than a recorded run. The
	// empty Run is still built, because Convergence and Disclosure must answer for
	// it, but `noRecord` blocks the push outright: see State.NoRecord for why disclosing was
	// not enough, and parity_test.go for the enumerated divergence this creates.
	r := runs[p.RunID]
	// The DERIVED emptiness, not the map lookup. `r == nil` misses a run that is present but
	// says nothing — rows carrying the id under a phase the typed decode does not own, for
	// instance — and the question the block asks is whether anything was readable, not
	// whether a key existed. Measured against the live store: ALL BUT ONE run carries a plan
	// row, and the holdout — run id `x`, a single `finish` row from 2026-09-30 — still has a
	// Finish, so `Empty()` is false for it too. So this cannot fire on a legitimately
	// zero-cycle run, and those are about HALF the runs in the author's live store (22 of 45 when last counted, 2026-10-10 19:50 — the store gains runs several times a day, so re-derive rather than trust the figure: group its rows by run_id and count those with no `cycle` row). Stated as
	// "all but one" rather than as a fraction because the fraction has now been wrong twice:
	// "all 41 real runs carry a plan row" was widened to "every one of the 44" in the pass
	// that fixed the denominator, without re-measuring, and "every one" is false.
	noRecord := r.Empty()
	if r == nil {
		r = &record.Run{ID: p.RunID}
		if p.Diag != nil {
			// Every derivation below now comes off an EMPTY record, and each one reads as a
			// legitimate answer: convergence is `unknown`, Finish is nil so no recorded
			// outcome can block (the unfinished-run block is skipped for an empty record,
			// which NoRecord refuses first), and the required fingerprint collapses to the two lines
			// pr-report.py renders for a run with no cycles. On this store that collides
			// with a real report for about HALF the runs in the author's live store (22 of 45 when last counted, 2026-10-10 19:50 — the store gains runs several times a day, so re-derive rather than trust the figure: group its rows by run_id and count those with no `cycle` row),
			// so this can GRANT as easily as refuse.
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
	// An omitted -branch or -default-branch is read from git in the repo, as the upstream
	// already is: they are git facts, and push-check.py's own docstring filed them as such.
	// Both flags defaulting to "" with nothing behind them made every omission a refusal
	// ("current or default branch unknown") on an ordinary feature branch, and the friction
	// log records that refusal twice. When git cannot say either — a detached HEAD, no
	// origin/HEAD — the name stays "" and Decide refuses exactly as before.
	branch, defBranch := p.Branch, p.DefaultBranch
	if branch == "" {
		branch = currentBranch(p.Repo)
	}
	if defBranch == "" {
		defBranch = defaultBranch(p.Repo)
	}
	var unreported string
	if landed, attributable, ghWhy := probeReport(p.RunID, r, p.Repo, branch); !landed {
		if ghWhy != "" && p.Diag != nil {
			// Said even when a pending file was readable (so the refusal is attributable): a
			// report already on the PR cannot be seen while gh fails, and the refusal alone
			// reads as an unposted one. The exit code is all sh carries; gh's stderr is not.
			fmt.Fprintf(p.Diag, "push-check: the PR's comments could not be read — %s — so a report "+
				"already posted there was not seen; the refusal below may not mean it is unposted\n", ghWhy)
		}
		unreported = "this run's report has not reached the PR or the pending-report file — " +
			"run looper pr-report --run-id " + p.RunID + " --post first"
		if !attributable && p.Diag != nil {
			// The refusal below is correct either way — fail closed — but "go post the
			// report" is the wrong instruction when the real problem is that neither probe
			// ran. Said here rather than in the reason, which parity pins exactly.
			ghNote := "gh (the PR's comments)"
			if branch == "" {
				ghNote = "gh (the PR's comments; never asked, no branch is known, as on a detached HEAD)"
			}
			fmt.Fprintf(p.Diag, "push-check: "+ghNote+" could not be run in %s, or answered "+
				"for certain (missing binary, not executable, killed at the %s budget, or a "+
				"descendant held its output past the drain bound), and there is no home directory "+
				"to hold the pending-report file, so "+
				"whether the report landed could not be determined; the refusal below fails "+
				"closed and may not be about the report at all\n", p.Repo, shBudget)
		}
	}
	var unfinished string
	if !noRecord && r.Finish == nil {
		unfinished = "run " + p.RunID + " was never finished — close it with `looper record " +
			"finish --run-id " + p.RunID + "`, re-post its report with `looper pr-report " +
			"--run-id " + p.RunID + " --post`, then ask again; if the session that planned it " +
			"is gone, run `looper record abandon --run-id " + p.RunID + " --missing '<gates>'`, " +
			"with '<gates>' replaced by the planned gates not run and why, and start a fresh " +
			"run instead"
		if d := p.Diag; d != nil {
			// record.Load drops a torn line without an error, so a finish row that was
			// written and then torn reads exactly like one never written.
			fmt.Fprintf(d, "push-check: run %q has no readable finish row in %q; if a "+
				"finish was recorded, its line may be torn\n", p.RunID, p.Store)
		}
	}
	push, reason := Decide(State{
		Convergence:    conv,
		GateState:      p.GateState,
		UnresolvedSkip: p.UnresolvedSkip,
		Branch:         branch,
		DefaultBranch:  defBranch,
		UpstreamExists: upstreamExists(p.Repo),
		Outcome:        outcome,
		Unreported:     unreported,
		NoRecord:       noRecord,
		Unfinished:     unfinished,
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
