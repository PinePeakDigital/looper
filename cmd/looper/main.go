// Command looper runs the review-loop's own tooling.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/pinepeakdigital/looper/internal/mutate"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: looper mutate [-catalog dir] [-root dir]")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "mutate":
		if err := runMutate(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", os.Args[1])
		os.Exit(2)
	}
}

func runMutate(args []string) error {
	fs := flag.NewFlagSet("mutate", flag.ExitOnError)
	catalog := fs.String("catalog", "mutations", "directory of .mut files")
	root := fs.String("root", ".", "repo root the mutations apply to")
	if err := fs.Parse(args); err != nil {
		return err
	}

	muts, err := mutate.ParseCatalog(*catalog)
	if err != nil {
		return err
	}
	fmt.Printf("%d mutation(s) from %s against %s\n\n", len(muts), *catalog, *root)

	r := &mutate.Runner{Root: *root, Log: os.Stdout}
	results, err := r.Run(muts)
	if err != nil {
		return err
	}

	caught, survived, stale := mutate.Score(results)
	fmt.Printf("\nscore %d/%d caught", caught, len(results))
	if survived > 0 || stale > 0 {
		fmt.Printf("  (%d survived, %d stale)", survived, stale)
	}
	fmt.Println()

	// Survivors and stale entries are both reported by name. A score alone says the
	// suite has holes; the names say which defect could ship through one.
	for _, res := range results {
		if res.Outcome == mutate.Caught {
			continue
		}
		fmt.Printf("\n%s: %s\n  %s — %s\n  %s\n",
			res.Outcome, res.Mutation.Name(), res.Mutation.Target, res.Mutation.Why, res.Detail)
	}
	if survived > 0 || stale > 0 {
		return mutate.ErrHoles
	}
	return nil
}
