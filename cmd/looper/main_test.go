package main

import (
	"bytes"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fixture builds a committed throwaway repo with a catalog, and returns its path.
// Every test gets its own, because the runner refuses a dirty tree.
func fixture(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		mode := os.FileMode(0o644)
		if strings.HasSuffix(name, ".sh") {
			mode = 0o755
		}
		if err := os.WriteFile(path, []byte(body), mode); err != nil {
			t.Fatal(err)
		}
	}
	git(t, dir, "init", "-q", ".")
	git(t, dir, "config", "user.email", "t@t")
	git(t, dir, "config", "user.name", "t")
	git(t, dir, "config", "commit.gpgsign", "false")
	// This machine's global pre-commit hook makes a network call that fails slowly, so
	// without this the commit never lands and every check below runs against a dirty tree.
	git(t, dir, "config", "core.hooksPath", "/dev/null")
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-qm", "seed")
	return dir
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := osexec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// doRun calls run() with its output captured, and returns the exit code plus both streams.
func doRun(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := run(args, &out, &errOut)
	return code, out.String(), errOut.String()
}

// The dispatch is the whole contract of the binary, and CI reads only its exit code.
func TestExitCodes(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want int
		says string
	}{
		{"no arguments", nil, 2, "usage"},
		{"unknown command", []string{"frobnicate"}, 2, "unknown command"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, _, errOut := doRun(t, tc.args...)
			if code != tc.want {
				t.Errorf("exit = %d, want %d", code, tc.want)
			}
			if !strings.Contains(errOut, tc.says) {
				t.Errorf("stderr does not mention %q: %q", tc.says, errOut)
			}
		})
	}
}

const caughtCatalog = `target: app.py
verify: ./t.sh
why:    the comparison was inverted
--- old
x > 0
--- new
x < 0
`

// A clean run exits 0 and says what it measured.
func TestMutateAllCaughtExitsZero(t *testing.T) {
	dir := fixture(t, map[string]string{
		"app.py":             "def f(x):\n    return x > 0\n",
		"t.sh":               "#!/bin/sh\npython3 -c 'import app; assert app.f(1) and not app.f(0)'\n",
		"mutations/flip.mut": caughtCatalog,
	})
	code, out, errOut := doRun(t, "mutate", "-root", dir, "-catalog", filepath.Join(dir, "mutations"))
	if code != 0 {
		t.Fatalf("exit = %d, want 0\nstdout: %s\nstderr: %s", code, out, errOut)
	}
	if !strings.Contains(out, "score 1/1 caught") {
		t.Errorf("stdout does not report the score: %q", out)
	}
}

// A survivor must exit non-zero — this is the gate, and nothing else asserted it.
func TestMutateSurvivorExitsNonZero(t *testing.T) {
	dir := fixture(t, map[string]string{
		"app.py": "def f(x):\n    return x > 0\n",
		// Asserts nothing: the shape of a real hollow test.
		"t.sh":               "#!/bin/sh\npython3 -c 'import app'\n",
		"mutations/flip.mut": caughtCatalog,
	})
	code, out, _ := doRun(t, "mutate", "-root", dir, "-catalog", filepath.Join(dir, "mutations"))
	if code != 1 {
		t.Fatalf("exit = %d, want 1\n%s", code, out)
	}
	// And it must name the mutation, or CI output says a hole exists without saying which.
	if !strings.Contains(out, "survived: flip") {
		t.Errorf("stdout does not name the survivor: %q", out)
	}
	if !strings.Contains(out, "the comparison was inverted") {
		t.Errorf("stdout does not carry the why line: %q", out)
	}
}

// A failure that is not the named assertion is reported as broken, not as a catch, and
// still fails the gate. This is the defect that scored two of this repo's own mutations
// as caught when they did not even compile.
func TestMutateBrokenExitsNonZero(t *testing.T) {
	dir := fixture(t, map[string]string{
		"app.py": "def f(x):\n    return x > 0\n",
		"t.sh":   "#!/bin/sh\necho 'SyntaxError' >&2\nexit 2\n",
		// expect: goes in the header, before the markers — appended after them it is
		// silently swallowed into the new block instead, which is how this test first
		// reported the mutation as caught.
		"mutations/flip.mut": "target: app.py\nverify: ./t.sh\nexpect: --- FAIL:\nwhy: the comparison was inverted\n--- old\nx > 0\n--- new\nx < 0\n",
	})
	code, out, _ := doRun(t, "mutate", "-root", dir, "-catalog", filepath.Join(dir, "mutations"))
	if code != 1 {
		t.Fatalf("exit = %d, want 1\n%s", code, out)
	}
	if !strings.Contains(out, "broken: flip") {
		t.Errorf("stdout does not report it as broken: %q", out)
	}
	if !strings.Contains(out, "0 survived, 0 stale, 1 broken") {
		t.Errorf("the score line does not count it: %q", out)
	}
}

// An empty or missing catalog must fail loudly, not report a clean bill of health.
func TestMutateRefusesEmptyCatalog(t *testing.T) {
	dir := fixture(t, map[string]string{"app.py": "x = 1\n", "mutations/notes.md": "nothing"})
	code, out, errOut := doRun(t, "mutate", "-root", dir, "-catalog", filepath.Join(dir, "mutations"))
	if code != 1 {
		t.Fatalf("exit = %d, want 1\n%s", code, out)
	}
	if !strings.Contains(errOut, "no .mut files") {
		t.Errorf("stderr does not say why: %q", errOut)
	}
}

func TestDocsCleanAndDirty(t *testing.T) {
	script := "p.add_argument(\"--run-id\")\n"
	t.Run("clean", func(t *testing.T) {
		dir := t.TempDir()
		write(t, dir, "runlog.py", script)
		write(t, dir, "SKILL.md", "`runlog.py --run-id X`\n")
		code, out, _ := doRun(t, "docs", dir)
		if code != 0 {
			t.Fatalf("exit = %d, want 0\n%s", code, out)
		}
		if !strings.Contains(out, "every flag") {
			t.Errorf("stdout does not confirm: %q", out)
		}
	})
	t.Run("a flag that does not exist", func(t *testing.T) {
		dir := t.TempDir()
		write(t, dir, "runlog.py", script)
		write(t, dir, "SKILL.md", "`runlog.py --max-cycles 3`\n")
		code, out, errOut := doRun(t, "docs", dir)
		if code != 1 {
			t.Fatalf("exit = %d, want 1", code)
		}
		if !strings.Contains(out, "--max-cycles") {
			t.Errorf("stdout does not name the flag: %q", out)
		}
		if !strings.Contains(errOut, "1 documented flag") {
			t.Errorf("stderr does not count them: %q", errOut)
		}
	})
}

func write(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
