// Command looper runs the review-loop's own tooling.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/pinepeakdigital/looper/internal/docs"
	"github.com/pinepeakdigital/looper/internal/mutate"
)

const usage = `usage: looper mutate [-catalog dir] [-root dir]
       looper docs <dir>`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run is where the work lives, so it can be tested. main() is only the os.Exit
// wrapper around it: everything this binary decides — which subcommand, what to
// print, which exit code — used to sit behind an os.Exit call and had no test at
// all, while being the exact surface CI gates on.
func run(args []string, out, errOut io.Writer) int {
	if len(args) < 1 {
		fmt.Fprintln(errOut, usage)
		return 2
	}
	var err error
	switch args[0] {
	case "mutate":
		err = runMutate(args[1:], out)
	case "docs":
		err = runDocs(args[1:], out)
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

func runMutate(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("mutate", flag.ContinueOnError)
	fs.SetOutput(out)
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
	report(out, results, len(muts))

	if runErr != nil {
		return fmt.Errorf("aborted after %d of %d mutation(s): %w", len(results), len(muts), runErr)
	}
	_, survived, stale, broken := mutate.Score(results)
	if survived+stale+broken > 0 {
		return mutate.ErrHoles
	}
	return nil
}

// report prints the score and names every result that is not a catch. A score alone
// says the suite has holes; the names say which defect could ship through one.
func report(out io.Writer, results []mutate.Result, planned int) {
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
