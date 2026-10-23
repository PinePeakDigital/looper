// Command looper runs the review-loop's own tooling.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime/debug"

	"github.com/pinepeakdigital/looper/internal/docs"
	"github.com/pinepeakdigital/looper/internal/evidence"
	"github.com/pinepeakdigital/looper/internal/gather"
	"github.com/pinepeakdigital/looper/internal/home"
	"github.com/pinepeakdigital/looper/internal/learn"
	"github.com/pinepeakdigital/looper/internal/ledger"
	"github.com/pinepeakdigital/looper/internal/mutate"
	"github.com/pinepeakdigital/looper/internal/plan"
	"github.com/pinepeakdigital/looper/internal/push"
	"github.com/pinepeakdigital/looper/internal/record"
	"github.com/pinepeakdigital/looper/internal/report"
	"github.com/pinepeakdigital/looper/internal/stats"
)

const usage = `usage: looper version
       looper mutate [-catalog dir] [-root dir] [-check]
       looper docs <dir>
       looper push-check -run-id <id> [-gate-state passed|skipped|blocked]
                         [-unresolved-skip] [-branch b] [-default-branch b]
                         [-repo dir] [-store path]
       looper pr-report -run-id <id> [-post] [-label] [-findings-file f|-]
                        [-repo dir] [-branch b] [-store path]
       looper record {plan,cycle,finish,convergence,carried,skipped,nudge,abandon,
                      check,show,pending} [flags]
       looper stats [--alarm] [--repo <substring>]
       looper threat-model
       looper deferred
       looper home
       looper context
       looper learn {add,bump,prune} <file> [text] [--section dismissed|accepted] [--cap N]
       looper plan --model m --logic yes|no --behavioral-goal yes|no
                   --runtime-change yes|no --attacker-reachable yes|no
                   [--spec-artifact yes|no] [--delta-logic yes|no] [--context f] [--dry-run]

push-check writes the decision as JSON to stdout and nothing else, and exits 0 when the
push is permitted, 3 when it is refused (the JSON says why), and 1 when it could not
decide (stdout empty; 2 is reserved for usage errors of the looper command itself). An omitted -branch or -default-branch is read from git in -repo:
the current branch, and the branch origin/HEAD names. stderr carries a
note when the report check could not be run at all, or when no row for the run was
readable in the store — two cases where the decision's own stated reason is the wrong
instruction.

pr-report renders the Step 14 disclosure from the record. Without -post it writes the
body to stdout. With -post it comments on this branch's PR, and otherwise keeps the
body in the run dir for Step 0c to flush. Without -findings-file the narrative is the run
dir's narrative.md when there is one — stderr says which, and names the cause when
gh could not answer rather than asserting there is no PR.

stats reads the run record: the cadence report, or with --alarm the repeat-offender
gates plan.py prints at Step 0 (silent when there are none). See README.md.

home prints this repo's store, ~/.claude/review-loop/repos/<name>-<hash>, where the
threat model, the deferred list and the learnings live (moved in from .git/info on first
use). It is outside .git so a worktree-isolated builder can write it.

threat-model and deferred report the two pinned ledgers in the repo's store as
JSON, from the working directory: which pinned files moved since their pin (stale), which
pins name no commit here or do not parse (broken_pins), and what owes a pin or a Guard
line. Exit 0 for a report, 1 for a ledger that exists and cannot be read.

context is Step 0: the base branch (fetched), the learnings, the test and lint commands and
the diff's size, as JSON, from the working directory. plan reads it.

plan is Step 0b: which gates run and the tier floor, as JSON, from looper context's JSON
(the --context file, or stdin), four yes/no answers and the optional --delta-logic, which
floors a fast-path re-entry on the delta since the last review. Unless --dry-run it records
the plan and prints the run_id on the last line. The Step 0 alarm goes to stderr.`

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

// run is where the work lives, so it can be tested. main() is only the os.Exit
// wrapper around it: everything this binary decides — which subcommand, what to
// print, which exit code — used to sit behind an os.Exit call and had no test at
// all, while being the exact surface CI gates on.
func run(args []string, in io.Reader, out, errOut io.Writer) int {
	if len(args) < 1 {
		fmt.Fprintln(errOut, usage)
		return 2
	}
	var err error
	switch args[0] {
	case "version":
		// The skill's Step 0 runs this to tell a looper that has push-check and pr-report
		// from an older one that has only mutate and docs: both pass `command -v`, and the
		// older one then fails at Step 14 with a usage message and empty stdout.
		_, err = fmt.Fprintln(out, version())
	case "mutate":
		err = runMutate(args[1:], out, errOut)
	case "docs":
		err = runDocs(args[1:], out)
	case "push-check":
		err = runPushCheck(args[1:], out, errOut)
	case "pr-report":
		err = runPrReport(args[1:], in, out, errOut)
	case "record":
		// The run record's writer, and runlog.py's exit statuses with it: check's 1 means an
		// open run, convergence's 1 means not converged. Returned as is, not mapped to 1/2.
		return record.Command(args[1:], out, errOut)
	case "stats":
		// review-stats.py's statuses: 0 for either report, 2 for a usage error, 1 for a store
		// that exists and cannot be read. plan.py prints the alarm only on 0.
		return stats.Command(args[1:], out, errOut)
	case "threat-model":
		// threat-model.py's report, which plan.py parses. Its own exit status: 0 for a report,
		// 1 for a ledger that exists and cannot be read, 2 for --selftest.
		return ledger.ThreatModel(args[1:], out, errOut)
	case "deferred":
		return ledger.Deferred(args[1:], out, errOut)
	case "home":
		// The repo's store, for the skill's shell snippets: `$(looper home)/learnings.md`. The
		// ledgers are moved in first: a `looper learn add` on that path before anything had read
		// the ledger would create a new file, and the old one would then never be moved. So a
		// ledger that cannot move fails the command and prints no store: a caller that ignores
		// stderr (Step 0 does) must not go on to write a fresh ledger over the orphaned one. The
		// rest are still tried, and each failure is named.
		var d string
		if d, err = home.RepoDir(""); err == nil {
			var errs []error
			for _, n := range []string{home.ThreatModel, home.Deferred, home.Learnings} {
				if _, lerr := home.Ledger("", n); lerr != nil {
					errs = append(errs, fmt.Errorf("%s not moved in: %w", n, lerr))
				}
			}
			if err = errors.Join(errs...); err == nil {
				_, err = fmt.Fprintln(out, d)
			}
		}
	case "plan":
		return plan.Command(args[1:], in, out, errOut)
	case "context":
		return gather.Command(args[1:], out, errOut)
	case "learn":
		return learn.Command(args[1:], out, errOut)
	default:
		fmt.Fprintf(errOut, "unknown command %q\n%s\n", args[0], usage)
		return 2
	}
	if errors.Is(err, errPushRefused) {
		return exitPushRefused
	}
	if err != nil {
		fmt.Fprintln(errOut, err)
		return 1
	}
	return 0
}

func runMutate(args []string, out, errOut io.Writer) error {
	fs := flag.NewFlagSet("mutate", flag.ContinueOnError)
	// Flag usage and parse errors go to STDERR, not to the stream this command's results go
	// to. Harmless here, where stdout is a human report; load-bearing for push-check, whose
	// stdout is a machine-readable contract. Kept consistent so the next subcommand inherits
	// the right default rather than the one that happened to be harmless.
	fs.SetOutput(errOut)
	catalog := fs.String("catalog", "mutations", "directory of .mut files")
	root := fs.String("root", ".", "repo root the mutations apply to")
	check := fs.Bool("check", false,
		"parse the catalog, check every target and anchor, then stop — seconds, not the full run")
	if err := fs.Parse(args); err != nil {
		// `-h` is a request that was SERVED, not a failure — same handling the two
		// sibling subcommands have had. It mattered less when this command had no flag
		// worth discovering; `-h` is now the only place -check is documented besides the
		// usage banner, and it exited 1 printing `flag: help requested`.
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}

	muts, err := mutate.ParseCatalog(*catalog)
	if err != nil {
		return err
	}

	// -check runs the real run's preconditions and nothing else. It replaced a Python
	// script that shadowed this parser and diverged from it in four consecutive review
	// rounds — every time in the direction where the script passed and the catalog then
	// failed to load, taking every entry's guard with it. Reusing ParseCatalog and
	// Runner.Check makes a divergence impossible rather than merely reviewed.
	if *check {
		problems := (&mutate.Runner{Root: *root}).Check(muts)
		for _, p := range problems {
			fmt.Fprintln(errOut, p)
		}
		if len(problems) > 0 {
			// "%d problem(s) across %d entries", NOT "%d of %d entries would not run". The
			// earlier wording was false whenever a precondition failed: a symlinked or
			// untracked target aborts Run WHOLESALE, so "1 of 4 would not run" described a
			// catalog where the true answer is none of the four. A problem count is also not
			// an entry count — one entry can contribute a target problem and an anchor
			// problem — and the thing the operator needs is that the catalog will not run.
			return fmt.Errorf("%d problem(s) across %d catalog entries; this catalog would not run",
				len(problems), len(muts))
		}
		// Deliberately narrow, and the one claim here that is exactly true. It does NOT say
		// the catalog will run: -check never executes a `verify:` command, so an entry whose
		// verify is already failing (ALREADY RED) or matches no test (VACUOUS) scores Broken
		// in a full run and is invisible here. Measured: a `-run` pattern naming a renamed
		// test — the same rename the anchor half exists for — passes this gate and scores
		// 0/2 in the run.
		fmt.Fprintf(out, "ok: %d entries parse, every target is usable, and every anchor "+
			"matches exactly once\n", len(muts))
		return nil
	}

	fmt.Fprintf(out, "%d mutation(s) from %s against %s\n\n", len(muts), *catalog, *root)

	r := &mutate.Runner{Root: *root, Log: out}
	results, runErr := r.Run(muts)

	// Report whatever came back even when the run aborted. Discarding the partial
	// results meant a run that died on mutation 17 of 19 printed no score and named no
	// survivors — the one thing the tool exists to produce — because the error path
	// returned before the summary.
	printScore(out, results, len(muts))

	if runErr != nil {
		return fmt.Errorf("aborted after %d of %d mutation(s): %w", len(results), len(muts), runErr)
	}
	_, survived, stale, broken := mutate.Score(results)
	if survived+stale+broken > 0 {
		return mutate.ErrHoles
	}
	return nil
}

// printScore prints the score and names every result that is not a catch. A score alone
// says the suite has holes; the names say which defect could ship through one.
//
// Named printScore rather than report since internal/report arrived: a local identifier that
// shadows an imported package name makes every later use of the package a compile error at
// the USE site, which reads as a problem with the new code rather than with the old name.
func printScore(out io.Writer, results []mutate.Result, planned int) {
	if len(results) == 0 {
		return
	}
	caught, survived, stale, broken := mutate.Score(results)
	fmt.Fprintf(out, "\nscore %d/%d caught", caught, len(results))
	if len(results) != planned {
		fmt.Fprintf(out, " (of %d attempted; the run did not finish)", planned)
	}
	if n := survived + stale + broken; n > 0 {
		fmt.Fprintf(out, "  (%d survived, %d stale, %d broken)", survived, stale, broken)
	}
	fmt.Fprintln(out)

	for _, res := range results {
		if res.Outcome == mutate.Caught {
			continue
		}
		fmt.Fprintf(out, "\n%s: %s\n  %s — %s\n  %s\n",
			res.Outcome, res.Mutation.Name(), res.Mutation.Target, res.Mutation.Why, res.Detail)
	}
}

func runDocs(args []string, out io.Writer) error {
	dir := "."
	if len(args) > 0 {
		dir = args[0]
	}
	refs, err := docs.Check(dir)
	if err != nil {
		return err
	}
	for _, ref := range refs {
		fmt.Fprintln(out, ref)
	}
	if n := len(refs); n > 0 {
		return fmt.Errorf("%d documented flag(s) do not exist", n)
	}
	fmt.Fprintf(out, "every flag the docs under %s name exists\n", dir)
	return nil
}

// errPushRefused is push-check's refusal: the JSON answer is already on stdout, and run maps
// this to exitPushRefused without printing anything, so stderr carries no error for it.
var errPushRefused = errors.New("push refused")

// exitPushRefused is push-check's exit status for a refusal. Distinct from 1 (the tool could
// not decide) and 2 (reserved for usage errors of the looper command itself).
const exitPushRefused = 3

// runPushCheck answers Step 14's auto-push question. It prints the decision as JSON either
// way, and exits 0 when the push is permitted, 3 when it is refused, 1 when it could not
// decide (nothing on stdout then; every failure maps to 1, only an unknown top-level looper
// command exits 2).
//
// Two reasons fix that contract, and each rules out the simpler one. The answer is the
// OUTPUT: a refusal is a successful answer with a reason to act on, so it must not look like
// the tool failing — that was why this exited 0 for both, and why a refusal still prints its
// JSON and does not share 1 with a failure. But exit 0 for a refusal made
// `looper push-check … && git push` push anyway on push:false. A non-zero refusal stops that
// plain chain. The two logged incidents (friction log, 2026-10-16 and 2026-10-20) were
// pipe-masked — `push-check … | tail … && git push` takes tail's status — so exit 3 alone
// would not have stopped them, and the JSON must still be read. Nathan chose the distinct
// code on 2026-10-21, over keeping 0 and over a plain non-zero exit.
func runPushCheck(args []string, out, errOut io.Writer) error {
	fs := flag.NewFlagSet("push-check", flag.ContinueOnError)
	// STDERR, not out. This command documents stdout as JSON and nothing else, and
	// flag.ContinueOnError writes both `-h` text and parse-error text to fs.Output(): with
	// that pointed at stdout, `looper push-check -h` and a mistyped flag each emitted the
	// usage banner where Step 14 reads a decision. Measured before the fix — both landed on
	// stdout and both exited 1.
	fs.SetOutput(errOut)
	// Required, not optional. In the Python both record-derived blockers were computed only
	// when it was present, so omitting it turned off the owed-report check AND the
	// broken-outcome check while a BOGUS id was correctly caught — the cheapest wrong
	// spelling was the one that passed.
	runID := fs.String("run-id", "", "the run to read convergence, outcome and the owed report from (required)")
	gateState := fs.String("gate-state", "skipped", "passed, skipped or blocked")
	unresolvedSkip := fs.Bool("unresolved-skip", false, "a 50-79 finding was skipped without a recorded dismissal")
	branch := fs.String("branch", "", "the current branch (default: git's current branch in -repo)")
	defaultBranch := fs.String("default-branch", "", "the repo's default branch (default: the branch origin/HEAD names in -repo)")
	repo := fs.String("repo", ".", "the repo to check git and gh facts in")
	store := fs.String("store", "", "run record path (default: $REVIEW_LOOP_RUNS, else ~/.claude/review-loop/runs.jsonl)")
	if err := fs.Parse(args); err != nil {
		// `-h` is a request that was SERVED, not a failure. flag reports it as ErrHelp and an
		// undifferentiated `return err` turned it into exit 1, indistinguishable from a
		// mistyped flag — where the Python exits 0 for -h and 2 for a parse error. Exit 0
		// here and let the caller tell them apart by the empty stdout.
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if *runID == "" {
		return errors.New("push-check: -run-id is required — without it the record's outcome and " +
			"the report check are both invisible, and the run pushes on nothing")
	}
	switch *gateState {
	case "passed", "skipped", "blocked":
	default:
		return fmt.Errorf("push-check: -gate-state %q is not passed, skipped or blocked", *gateState)
	}
	if *store == "" {
		*store = record.StorePath()
	}
	res, err := push.Check(push.CheckParams{
		Store:          *store,
		RunID:          *runID,
		GateState:      *gateState,
		UnresolvedSkip: *unresolvedSkip,
		Branch:         *branch,
		DefaultBranch:  *defaultBranch,
		Repo:           *repo,
		Diag:           errOut,
	})
	if err != nil {
		return err
	}
	b, err := res.Encode()
	if err != nil {
		return err
	}
	if _, err := out.Write(b); err != nil {
		return err
	}
	// After the write: a refusal carries its answer.
	if !res.Push {
		return errPushRefused
	}
	return nil
}

// runPrReport renders the Step 14 disclosure and, with -post, publishes it.
//
// The summary is the single most-dropped step in this skill — it is in the friction log, and
// one run's report had to be volunteered by its author because nothing produced it. A script
// that reads the record cannot skip a gate, cannot misstate convergence, and cannot quietly
// leave out the fourth cycle.
func runPrReport(args []string, in io.Reader, out, errOut io.Writer) error {
	fs := flag.NewFlagSet("pr-report", flag.ContinueOnError)
	// STDERR for usage and parse errors: with -post off, stdout is the report BODY, and a
	// usage banner mixed into it would be posted as the review disclosure on the next run.
	fs.SetOutput(errOut)
	runID := fs.String("run-id", "", "the run to render from the record (required)")
	findingsFile := fs.String("findings-file", "", "the orchestrator's narrative: a path, or \"-\" for stdin; stdin is read only then")
	post := fs.Bool("post", false, "comment on this branch's PR, or keep the report for Step 0c")
	label := fs.Bool("label", false, "also apply the review:<convergence> label, and the risk:<level> label from the plan's inputs (removing any risk label when they cannot say)")
	repo := fs.String("repo", ".", "the repo to run git and gh in")
	branch := fs.String("branch", "", "the branch to find the PR for (default: git's current branch)")
	store := fs.String("store", "", "run record path (default: $REVIEW_LOOP_RUNS, else ~/.claude/review-loop/runs.jsonl)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if *runID == "" {
		return errors.New("pr-report: -run-id is required — the report's facts all come from " +
			"that row, and there is no sensible default run to render")
	}
	// A DELIBERATE DIVERGENCE, in the hardening direction: the Python interpolates the run id
	// raw into both the marker and the summary line of a PUBLIC PR comment, and `record.Load`
	// does not constrain `run_id`, so a store row can carry anything and this flag can name it.
	// The forgery: a `-->` in the id closes the marker early and renders
	// whatever follows as VISIBLE markdown. Measured against the oracle with `run_id` =
	// "a-->\n\n> **Review converged — nothing outstanding**": the comment ends after `a` and a
	// forged blockquote saying the review converged renders directly above the real disclosure,
	// which says UNKNOWN. A plain newline is milder than first recorded — measured, `a\nb`
	// splits the marker across two lines but swallows NOTHING, and the marker needle the push gate
	// looks for is built by Sprintf from the same raw id, so it still matches. (That needle is
	// push.reportProbe's, not push.Fingerprint's, which returns only the heading and the
	// cycle/agent line — the earlier wording named the wrong function.) Both earlier claims here
	// ("swallowing the first line of the body", "the needle is gone") were consequences nobody
	// measured; the `-->` case is the one that justifies the guard.
	//
	// Refused HERE first, so the refusal can explain itself; evidence.Marker refuses the same
	// ids inside Render as a backstop. Refused rather than cell()ed, because cell()ing the id
	// would rewrite the marker the push gate matches exactly — hardening one gate by breaking
	// the other. The push gate uses the same evidence.ValidRunID, so a run id this rejects
	// could never have cleared it anyway.
	if !evidence.ValidRunID(*runID) {
		return fmt.Errorf("pr-report: %q is not a usable run id — the id is interpolated raw "+
			"into a public PR comment, so it is limited to letters, digits, - and _ (max 64). "+
			"push-check refuses the same ids, so this one could not have cleared the gate",
			*runID)
	}
	if *store == "" {
		*store = record.StorePath()
	}

	runs, err := record.Load(*store, 0)
	if err != nil {
		return err
	}
	r := runs[*runID]
	// record.Load returns an empty map and NO error for a store that does not hold the run —
	// a wrong -store, a missing file, a mistyped id — and a report rendered off nothing is
	// not an empty report: it is a GENUINE one reading `0 cycle(s) · 0 agent(s)`, which is
	// exactly what the push gate's required fingerprint collapses to. So the two halves
	// refuse together. The store is named because the id is usually not the wrong half.
	// record.Run.Empty, not `r == nil`: the narrower test shipped here and misses a run that
	// is PRESENT and says nothing — a row filed under a phase the typed decode does not own,
	// such as `{"run_id":"x","phase":"nudge"}`. Measured: that store rendered a complete,
	// plausible report reading `0 cycle(s) · 0 agent(s)` and exited 0, which is exactly the
	// fingerprint push-check accepts. One definition, in internal/record, so the two halves of
	// the gate cannot drift — which is the same mistake PR #4 caught in internal/push.
	if r.Empty() {
		return fmt.Errorf("pr-report: no readable record for run %q in %s — check -store and "+
			"-run-id; a report rendered from no record would read as a reviewed run that "+
			"spawned no agents", *runID, *store)
	}
	conv, err := r.Convergence()
	if err != nil {
		return err
	}

	// No -findings-file: the run dir's narrative.md, which the skill always writes. A builder
	// once could not pass the flag at all (narthur/skills#16), and the report went out bare.
	if *findingsFile == "" {
		if rd, err := home.RunDir(*runID); err == nil {
			if p := filepath.Join(rd, "narrative.md"); fileExists(p) {
				*findingsFile = p
				if *post {
					fmt.Fprintf(errOut, "pr-report: narrative from %s\n", p)
				}
			}
		}
	}
	narrative, err := readNarrative(*findingsFile, in)
	if err != nil {
		return err
	}
	body, err := report.Render(r, *runID, conv, narrative)
	if err != nil {
		return err
	}
	if !*post {
		_, err = io.WriteString(out, body)
		return err
	}
	if *findingsFile == "" && stdinOffered(in) {
		// Stdin is not read without the flag, and a narrative piped in without it is otherwise
		// lost without a trace: the report posts, exit 0, and reads like a run with nothing to say.
		// Advice for both readings, because the mode cannot tell them apart: "-findings-file -"
		// on an idle inherited pipe is exactly the hang opt-in removed.
		fmt.Fprintln(errOut, "pr-report: stdin is piped but not read, so the report carries no "+
			"narrative. If one was piped, re-run with -findings-file -; if not, run with "+
			"</dev/null to silence this")
	}
	_, err = report.Post(report.PostParams{
		Repo: *repo, RunID: *runID, Branch: *branch, Convergence: conv,
		Risk: report.Risk(r.Plan), Body: body, Label: *label, Diag: errOut,
	})
	return err
}

func fileExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.Mode().IsRegular()
}

// readNarrative takes the narrative from -findings-file: a path, or "-" for stdin. Stdin is
// opt-in. It used to be read whenever it was not a terminal, the Python's rule, and an
// inherited pipe that nobody writes to is not a terminal either: on 2026-10-11 an orchestrator
// flushing a report by hand ran `looper pr-report --post` with its harness's stdin and it sat
// silent for minutes until killed. No test can tell an idle pipe from a slow writer, so the
// caller says which it means.
func readNarrative(path string, in io.Reader) (string, error) {
	if path == "" {
		return "", nil
	}
	if path == "-" {
		b, err := io.ReadAll(in)
		return string(b), err
	}
	b, err := os.ReadFile(path)
	return string(b), err
}

// stdinOffered reports whether stdin LOOKS like it carries a narrative — a pipe or a file —
// from its mode alone. Stat never blocks, which is the difference from reading it: an idle
// inherited pipe is indistinguishable from a real one by mode, so this can only warn, never
// read. A terminal or </dev/null is a character device and stays quiet. A reader that is not
// an *os.File is something a caller handed over on purpose, so it counts as offered.
func stdinOffered(in io.Reader) bool {
	f, ok := in.(*os.File)
	if !ok {
		return true
	}
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice == 0
}

// version is the module version `go install ...@vX.Y.Z` stamped into the binary. Other
// builds print a VCS pseudo-version (`go build`) or "(devel)" (`go run`, -buildvcs=false).
func version() string {
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" {
		return bi.Main.Version
	}
	return "(devel)"
}
