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
func TestFlagsAreAttributedPerScript(t *testing.T) {
	dir := fixture(t, map[string]string{
		"runlog.py": script,
		"plan.py":   "p.add_argument(\"--context\")\n",
		"SKILL.md":  "plan.py --context f | runlog.py --run-id X\n",
	})
	refs, err := Check(dir)
	if err != nil {
		t.Fatal(err)
	}
	// Both flags are correct for their own script, but each appears alongside the
	// OTHER script's name too, so exactly the two cross-attributions are reported —
	// and each must name the script it was actually attributed to. Asserting only
	// the count let a reference that named the wrong script pass.
	if len(refs) != 2 {
		t.Fatalf("got %d, want 2 cross-attributions: %v", len(refs), refs)
	}
	got := map[string]string{}
	for _, r := range refs {
		got[r.Script] = r.Flag
	}
	if got["plan.py"] != "--run-id" || got["runlog.py"] != "--context" {
		t.Errorf("wrong attribution: %v", got)
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
