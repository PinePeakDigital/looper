package docs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fixture(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

const script = `
import argparse
p = argparse.ArgumentParser()
p.add_argument("--run-id")
sub = p.add_subparsers()
c = sub.add_parser("cycle")
c.add_argument("--agent-cap", type=int)
`

// The defect this exists for: prose documenting a flag that was deleted. SKILL.md
// told agents to pass --max-cycles for a full day after runlog.py stopped accepting it.
func TestFlagNotDeclaredIsReported(t *testing.T) {
	dir := fixture(t, map[string]string{
		"runlog.py": script,
		"SKILL.md":  "Run `python3 runlog.py cycle --max-cycles 3` to bound the loop.\n",
	})
	refs, err := Check(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 {
		t.Fatalf("got %d references, want 1: %v", len(refs), refs)
	}
	if refs[0].Flag != "--max-cycles" || refs[0].Script != "runlog.py" || refs[0].Line != 1 {
		t.Errorf("wrong reference: %+v", refs[0])
	}
	if !strings.Contains(refs[0].String(), "runlog.py has no --max-cycles") {
		t.Errorf("message does not say what is wrong: %s", refs[0])
	}
}

// Flags that DO exist must stay silent, or the check gets ignored.
func TestDeclaredFlagsAreSilent(t *testing.T) {
	dir := fixture(t, map[string]string{
		"runlog.py": script,
		"SKILL.md":  "`python3 runlog.py cycle --run-id X --agent-cap 8 --help`\n",
	})
	refs, err := Check(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 0 {
		t.Errorf("reported flags that exist: %v", refs)
	}
}

// A flag belonging to a DIFFERENT script on the same line is still that script's.
// Without the per-line pairing this reported every flag against every script.
func TestEachFlagGoesToItsNearestScript(t *testing.T) {
	dir := fixture(t, map[string]string{
		"runlog.py": script,
		"plan.py":   "p.add_argument(\"--context\")\n",
		"SKILL.md":  "plan.py --context f | runlog.py --run-id X\n",
	})
	refs, err := Check(dir)
	if err != nil {
		t.Fatal(err)
	}
	// Both flags are correct for their own script. This used to report two
	// cross-attributions and called them the point; they are false positives, and a
	// check that fires on correct prose is one readers learn to re-check instead of
	// act on. The identical line in a real SKILL.md is what surfaced it.
	if len(refs) != 0 {
		t.Fatalf("correct prose naming two scripts must be silent, got %v", refs)
	}
}

// The false positive has to stay dead in the shape that produced it: one sentence
// naming a second tool for an unrelated reason, beside a correctly-attributed flag.
func TestASecondScriptNamedNearbyIsNotBlamed(t *testing.T) {
	dir := fixture(t, map[string]string{
		"runlog.py":      script,
		"batch-files.py": "p.add_argument(\"--target\")\n",
		"SKILL.md":       "Measure with `batch-files.py` output, then `runlog.py --run-id X`.\n",
	})
	refs, err := Check(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 0 {
		t.Fatalf("want silence, got %v", refs)
	}
}

// Narrowing attribution must not stop it catching the thing it exists for.
func TestAStaleFlagIsStillCaughtBesideAnotherScript(t *testing.T) {
	dir := fixture(t, map[string]string{
		"runlog.py":      script,
		"batch-files.py": "p.add_argument(\"--target\")\n",
		"SKILL.md":       "Read `batch-files.py` first, then `runlog.py --max-cycles 3`.\n",
	})
	refs, err := Check(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 || refs[0].Script != "runlog.py" || refs[0].Flag != "--max-cycles" {
		t.Fatalf("want runlog.py/--max-cycles, got %v", refs)
	}
}

// "--flag of script.py" is ordinary prose, so the nearer edge decides rather than
// position alone. Attributing only to a PRECEDING mention would miss this entirely.
func TestAFlagBeforeItsScriptStillResolves(t *testing.T) {
	dir := fixture(t, map[string]string{
		"runlog.py": script,
		"plan.py":   "p.add_argument(\"--context\")\n",
		"SKILL.md":  "Pass `--max-cycles` to `runlog.py`, not to `plan.py`.\n",
	})
	refs, err := Check(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 || refs[0].Script != "runlog.py" || refs[0].Flag != "--max-cycles" {
		t.Fatalf("want runlog.py/--max-cycles, got %v", refs)
	}
}

// The metric is distance to the nearer EDGE of the script's name, which nearest-START
// and nearest-END both left green until these fixtures existed. Reachable by ordinary
// prose whenever a long script name sits beside a short one.
func TestDistanceIsToTheNearerEdge(t *testing.T) {
	files := map[string]string{
		"runlog.py":         script,
		"plan.py":           "p.add_argument(\"--context\")\n",
		"upstream-check.py": "if a == \"--since\":\n",
	}
	for name, line := range map[string]string{
		// nearest-START would pick plan.py: "upstream-check.py" starts far left.
		"edge not start": "`upstream-check.py --since` or `plan.py`\n",
		// nearest-END would pick plan.py: "upstream-check.py" ends far right.
		"edge not end": "`plan.py` is unrelated; the `--since` of `upstream-check.py`.\n",
		// Measuring a FOLLOWING script from the flag's start rather than its end adds
		// len(flag) to its distance. Here the owner follows at a gap of 5 while a
		// non-owner precedes at 10: the gap metric picks the owner and stays silent,
		// the biased one adds 7 for "--since", picks plan.py, and reports it.
		"gap not flag-start": "plan.py" + strings.Repeat(" ", 10) + "--since" +
			strings.Repeat(" ", 5) + "upstream-check.py\n",
	} {
		f := map[string]string{}
		for k, v := range files {
			f[k] = v
		}
		f["SKILL.md"] = line
		refs, err := Check(fixture(t, f))
		if err != nil {
			t.Fatal(err)
		}
		if len(refs) != 0 {
			t.Errorf("%s: want silence, got %v", name, refs)
		}
	}
}

// Equidistant candidates go to the PRECEDING mention, which the comment claims and a
// strict `<` is what delivers. Contrived spacing, but `<=` left the suite green.
func TestATieGoesToThePrecedingMention(t *testing.T) {
	dir := fixture(t, map[string]string{
		"runlog.py":         script,
		"upstream-check.py": "if a == \"--since\":\n",
		// Equal GAPS: 4 characters of space on each side of the flag.
		"SKILL.md": "runlog.py    --run-id    upstream-check.py\n",
	})
	refs, err := Check(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 0 {
		t.Fatalf("runlog.py precedes and has --run-id, so want silence, got %v", refs)
	}
}

// Nearest among the scripts THIS directory has. A nearer mention of someone else's
// tool would otherwise swallow the attribution and skip the real check silently.
func TestAnUnknownScriptDoesNotSwallowTheAttribution(t *testing.T) {
	dir := fixture(t, map[string]string{
		"runlog.py": script,
		// The unknown script must be NEARER than the known one, or nearest-overall and
		// nearest-known agree and the guard this test names is pinned by nothing. The
		// first version of this fixture had them the other way round.
		"SKILL.md": "Unlike `setup.py --max-cycles`, `runlog.py` is ours.\n",
	})
	refs, err := Check(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 || refs[0].Script != "runlog.py" {
		t.Fatalf("want runlog.py, got %v", refs)
	}
}

// references/*.md is where half the procedure lives; skipping it would make the
// check pass by not looking.
func TestReferencesSubdirectoryIsChecked(t *testing.T) {
	dir := fixture(t, map[string]string{
		"runlog.py":            script,
		"SKILL.md":             "see references\n",
		"references/finish.md": "`runlog.py finish --clean-exit`\n",
	})
	refs, err := Check(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 || refs[0].Doc != filepath.Join("references", "finish.md") {
		t.Fatalf("did not check references/: %v", refs)
	}
}

// A directory with nothing to compare must error, not report a clean bill of health.
func TestRefusesToCheckNothing(t *testing.T) {
	for _, tc := range []struct {
		name  string
		files map[string]string
		want  string
	}{
		{"no scripts", map[string]string{"SKILL.md": "x --flag\n"}, "no .py files"},
		{"no docs", map[string]string{"runlog.py": script}, "no .md files"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Check(fixture(t, tc.files))
			if err == nil {
				t.Fatal("reported success over nothing")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error does not name the cause: %v", err)
			}
		})
	}
}

// --help is argparse's, never written with add_argument.
func TestHelpIsNotReported(t *testing.T) {
	dir := fixture(t, map[string]string{
		"runlog.py": script,
		"SKILL.md":  "`runlog.py --help`\n",
	})
	refs, err := Check(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 0 {
		t.Errorf("reported --help: %v", refs)
	}
}
