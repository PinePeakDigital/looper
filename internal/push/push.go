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
// The marker alone was not enough: `printf '<!-- review-loop:run=X -->' > <pending>` is 38
// bytes and satisfied the gate, making it CHEAPER to forge than the `disclosed --where
// "trust me"` row it replaced. Requiring the run line too means the numbers have to agree
// with the cycle rows, which cannot be produced without rendering from the record — the
// point being that this is a property of the artifact, not a claim about it.
//
// The separator is U+00B7 MIDDLE DOT, as pr-report.py writes it. An ASCII `·`-alike here
// would make every real report read as missing.
func Fingerprint(r *record.Run) []string {
	var spent int
	for _, c := range r.Cycles {
		if c.Agents != nil {
			spent += *c.Agents
		}
	}
	return []string{"## review-loop", fmt.Sprintf("%d cycle(s) · %d agent(s)", len(r.Cycles), spent)}
}

// ReportLanded reports whether this run's rendered report has reached somewhere a reader
// will see it.
//
// No "cannot tell" result: no gh and no git both read as NOT landed, which fails closed, and
// pr-report guarantees the body lands in one of the two places checked — a PR comment or
// this run's pending file — including when the post fails. So "neither" means pr-report did
// not run, which is the one thing this gate exists to catch.
func ReportLanded(runID string, r *record.Run, repo string) bool {
	needles := append([]string{fmt.Sprintf(reportMarker, runID)}, Fingerprint(r)...)
	if rc, out := sh(repo, "gh", "pr", "view", "--json", "comments", "-q", ".comments[].body"); rc == 0 {
		if containsAll(out, needles) {
			return true
		}
	}
	rc, gitdir := sh(repo, "git", "rev-parse", "--path-format=absolute", "--git-common-dir")
	if rc != 0 || gitdir == "" {
		return false
	}
	body, err := os.ReadFile(filepath.Join(gitdir, fmt.Sprintf(pendingFmt, runID)))
	if err != nil {
		return false
	}
	return containsAll(string(body), needles)
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
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, prog, args...)
	cmd.Dir = repo
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return ee.ExitCode(), strings.TrimSpace(string(out))
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

// Check is the whole decision: read the run, derive what the record says, look for the
// report, and weigh it. gateState is "passed", "skipped" or "blocked".
func Check(store, runID, gateState string, unresolvedSkip bool, branch, defaultBranch, repo string) (Result, error) {
	if !runIDRe.MatchString(runID) {
		return Result{}, fmt.Errorf("run id %q is not a plain identifier", runID)
	}
	runs, err := record.Load(store, 0)
	if err != nil {
		return Result{}, err
	}
	// A run id absent from the store is UNKNOWN, never converged. Absence is the cheapest
	// thing to produce, so it must buy the same disclosure a recorded unfinished run owes.
	r := runs[runID]
	if r == nil {
		r = &record.Run{ID: runID}
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
	disclose, err := r.Disclosure()
	if err != nil {
		return Result{}, err
	}
	var outcome string
	if r.Finish != nil {
		outcome = r.Finish.Outcome
	}
	var unreported string
	if !ReportLanded(runID, r, repo) {
		unreported = "this run's report has not reached the PR or the pending-report file — " +
			"run pr-report.py --post first"
	}
	push, reason := Decide(State{
		Convergence:    conv,
		GateState:      gateState,
		UnresolvedSkip: unresolvedSkip,
		Branch:         branch,
		DefaultBranch:  defaultBranch,
		UpstreamExists: upstreamExists(repo),
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
