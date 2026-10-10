// Command looper runs the review-loop's own tooling.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/pinepeakdigital/looper/internal/docs"
	"github.com/pinepeakdigital/looper/internal/mutate"
	"github.com/pinepeakdigital/looper/internal/push"
	"github.com/pinepeakdigital/looper/internal/record"
	"github.com/pinepeakdigital/looper/internal/report"
)

const usage = `usage: looper mutate [-catalog dir] [-root dir]
       looper docs <dir>
       looper push-check -run-id <id> [-gate-state passed|skipped|blocked]
                         [-unresolved-skip] [-branch b] [-default-branch b]
                         [-repo dir] [-store path]
       looper pr-report -run-id <id> [-post] [-label] [-findings-file f]
                        [-repo dir] [-branch b] [-store path]

push-check writes the decision as JSON to stdout and nothing else. stderr carries a
note when the report check could not be run at all, or when no row for the run was
readable in the store — two cases where the decision's own stated reason is the wrong
instruction.

pr-report renders the Step 14 disclosure from the record. Without -post it writes the
body to stdout. With -post it comments on this branch's PR, and otherwise keeps the
body in .git/info for Step 0c to flush — stderr says which, and names the cause when
gh could not answer rather than asserting there is no PR. See README.md.`

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
	case "mutate":
		err = runMutate(args[1:], out, errOut)
	case "docs":
		err = runDocs(args[1:], out)
	case "push-check":
		err = runPushCheck(args[1:], out, errOut)
	case "pr-report":
		err = runPrReport(args[1:], in, out, errOut)
	default:
		fmt.Fprintf(errOut, "unknown command %q\n%s\n", args[0], usage)
		return 2
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
	if err := fs.Parse(args); err != nil {
		return err
	}

	muts, err := mutate.ParseCatalog(*catalog)
	if err != nil {
		return err
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

// runPushCheck answers Step 14's auto-push question. It prints JSON and exits 0 whether or
// not the push is permitted: the ANSWER is the output, and a non-zero exit would make a
// refusal indistinguishable from the tool failing to produce one.
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
	branch := fs.String("branch", "", "the current branch")
	defaultBranch := fs.String("default-branch", "", "the repo's default branch")
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
	_, err = out.Write(b)
	return err
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
	findingsFile := fs.String("findings-file", "", "the orchestrator's narrative; \"-\" or omitted reads stdin")
	post := fs.Bool("post", false, "comment on this branch's PR, or keep the report for Step 0c")
	label := fs.Bool("label", false, "also apply the review:<convergence> label")
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
	_, err = report.Post(report.PostParams{
		Repo: *repo, RunID: *runID, Branch: *branch, Convergence: conv,
		Body: body, Label: *label, Diag: errOut,
	})
	return err
}

// readNarrative resolves the narrative the same way the Python does: a named file, else stdin
// when stdin is not a terminal.
func readNarrative(path string, in io.Reader) (string, error) {
	if path != "" && path != "-" {
		b, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		return string(b), nil
	}
	if !shouldReadStdin(in) {
		return "", nil
	}
	b, err := io.ReadAll(in)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// shouldReadStdin is the tty test, split out as its own function so it can be ASSERTED.
// Folded into readNarrative it could not be: the only observable difference between taking the
// branch and not taking it is whether a read happens, and the obvious fixture — /dev/null —
// returns the empty string either way, so a test of readNarrative's RESULT passed identically
// with the whole guard deleted. Reproduce it by deleting the guard and running the package.
//
// What the guard is for: an interactive `looper pr-report` must not block on a read nobody is
// going to feed. A character device is a terminal, or /dev/zero, which never EOFs at all;
// /dev/null is one too and EOFs immediately, which is exactly why it cannot serve as the
// fixture. The `!ok` arm is for a reader that is not an *os.File at all — a strings.Reader —
// and NOT for a pipe: os.Pipe returns *os.File, so a pipe goes through Stat and is read
// because its mode is ModeNamedPipe. In production `in` is always os.Stdin, so that arm is
// reached only by this package's tests; it exists because run() takes an io.Reader, and a
// caller that hands one over has already decided to supply a narrative.
func shouldReadStdin(in io.Reader) bool {
	f, ok := in.(*os.File)
	if !ok {
		return true
	}
	st, err := f.Stat()
	if err != nil {
		return false // cannot tell, so do not risk hanging
	}
	return st.Mode()&os.ModeCharDevice == 0
}
