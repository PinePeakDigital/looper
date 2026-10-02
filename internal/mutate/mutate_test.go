package mutate

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// repo builds a throwaway git repo with the given files, committed, so the
// clean-tree check passes. Every test gets its own.
func repo(t *testing.T, files map[string]string) string {
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
	for _, args := range [][]string{
		{"init", "-q", "."},
		{"config", "user.email", "t@t"},
		{"config", "user.name", "t"},
		{"config", "commit.gpgsign", "false"},
		// This machine has a global hooks path whose pre-commit scans for secrets over
		// the network; in a throwaway repo it fails slowly and the commit never lands,
		// leaving a dirty tree and a test that fails for the wrong reason.
		{"config", "core.hooksPath", "/dev/null"},
		{"add", "-A"},
		{"commit", "-qm", "seed"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return dir
}

func run(t *testing.T, dir string, muts ...Mutation) []Result {
	t.Helper()
	r := &Runner{Root: dir}
	results, err := r.Run(muts)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return results
}

// A suite that notices the defect is the baseline everything else is measured
// against, and the file must come back byte-identical afterwards.
func TestCaughtAndRestored(t *testing.T) {
	const src = "def f(x):\n    return x > 0\n"
	dir := repo(t, map[string]string{
		"app.py": src,
		"t.sh":   "#!/bin/sh\npython3 -c 'import app; assert app.f(1) and not app.f(0)'\n",
	})
	res := run(t, dir, Mutation{
		Source: "flip.mut", Target: "app.py", Verify: []string{"./t.sh"},
		Why: "comparison inverted", Old: "x > 0", New: "x < 0",
	})
	if res[0].Outcome != Caught {
		t.Errorf("outcome = %q, want caught (detail: %s)", res[0].Outcome, res[0].Detail)
	}
	got, err := os.ReadFile(filepath.Join(dir, "app.py"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != src {
		t.Errorf("target not restored:\n got %q\nwant %q", got, src)
	}
}

// The case this tool exists for: the suite passes with the defect present.
func TestSurvived(t *testing.T) {
	dir := repo(t, map[string]string{
		"app.py": "def f(x):\n    return x > 0\n",
		// Imports the module and asserts nothing — the shape of a real hollow test.
		"t.sh": "#!/bin/sh\npython3 -c 'import app' && echo all checks passed\n",
	})
	res := run(t, dir, Mutation{
		Source: "flip.mut", Target: "app.py", Verify: []string{"./t.sh"},
		Why: "comparison inverted", Old: "x > 0", New: "x < 0",
	})
	if res[0].Outcome != Survived {
		t.Errorf("outcome = %q, want survived", res[0].Outcome)
	}
}

// A stale anchor must never read as caught. Yesterday's hand-rolled version
// reported "0 failures" for a mutation that had silently matched nothing, which is
// indistinguishable from a working guard unless the count is checked.
func TestStaleAnchor(t *testing.T) {
	dir := repo(t, map[string]string{
		"app.py": "def f(x):\n    return x > 0\n",
		"t.sh":   "#!/bin/sh\nexit 0\n",
	})
	for _, tc := range []struct{ name, old string }{
		{"absent", "this text is not in the file"},
		{"ambiguous", "return"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := dir
			if tc.name == "ambiguous" {
				dir = repo(t, map[string]string{
					"app.py": "def f():\n    return 1\ndef g():\n    return 2\n",
					"t.sh":   "#!/bin/sh\nexit 0\n",
				})
			}
			res := run(t, dir, Mutation{
				Source: "x.mut", Target: "app.py", Verify: []string{"./t.sh"},
				Why: "whatever", Old: tc.old, New: "return 99",
			})
			if res[0].Outcome != Stale {
				t.Errorf("outcome = %q, want stale", res[0].Outcome)
			}
			if !strings.Contains(res[0].Detail, "want exactly 1") {
				t.Errorf("detail does not say why: %q", res[0].Detail)
			}
		})
	}
}

// Refusing a dirty tree is what makes a restore unambiguous. Without it, this tool
// reproduces the exact accident it was built after: a restore that also reverts
// uncommitted work, and a conclusion drawn from the result.
func TestRefusesDirtyTree(t *testing.T) {
	dir := repo(t, map[string]string{
		"app.py": "x = 1\n",
		"t.sh":   "#!/bin/sh\nexit 1\n",
	})
	if err := os.WriteFile(filepath.Join(dir, "app.py"), []byte("x = 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := &Runner{Root: dir}
	_, err := r.Run([]Mutation{{Target: "app.py", Verify: []string{"./t.sh"}, Why: "w", Old: "x = 2", New: "x = 3"}})
	if err == nil {
		t.Fatal("ran against a dirty tree")
	}
	if !strings.Contains(err.Error(), "uncommitted") {
		t.Errorf("error does not name the cause: %v", err)
	}
	// And the edit in progress is still there, untouched.
	got, _ := os.ReadFile(filepath.Join(dir, "app.py"))
	if string(got) != "x = 2\n" {
		t.Errorf("clobbered work in progress: %q", got)
	}
}

// Any one failing command is enough; the rest are not run.
func TestFirstFailingVerifyWins(t *testing.T) {
	dir := repo(t, map[string]string{
		"app.py":  "x = 1\n",
		"a.sh":    "#!/bin/sh\nexit 1\n",
		"mark.sh": "#!/bin/sh\ntouch ran-second\n",
	})
	res := run(t, dir, Mutation{
		Source: "x.mut", Target: "app.py", Verify: []string{"./a.sh", "./mark.sh"},
		Why: "w", Old: "x = 1", New: "x = 2",
	})
	if res[0].Outcome != Caught {
		t.Fatalf("outcome = %q, want caught", res[0].Outcome)
	}
	if _, err := os.Stat(filepath.Join(dir, "ran-second")); err == nil {
		t.Error("kept running verify commands after one failed")
	}
}

// The executable bit must survive. Rewriting a file through a fresh create dropped
// runlog.py's, and the Stop hook requiring it then failed three times for reasons
// that looked unrelated to the edit.
func TestPreservesMode(t *testing.T) {
	dir := repo(t, map[string]string{
		"tool.sh": "#!/bin/sh\necho one\n",
		"t.sh":    "#!/bin/sh\nexit 1\n",
	})
	before, err := os.Stat(filepath.Join(dir, "tool.sh"))
	if err != nil {
		t.Fatal(err)
	}
	run(t, dir, Mutation{
		Source: "x.mut", Target: "tool.sh", Verify: []string{"./t.sh"},
		Why: "w", Old: "echo one", New: "echo two",
	})
	after, err := os.Stat(filepath.Join(dir, "tool.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if before.Mode() != after.Mode() {
		t.Errorf("mode changed: %v -> %v", before.Mode(), after.Mode())
	}
}

// A verify command that reads stdin must not inherit ours and block forever. This
// is a live hazard, not a hypothetical: pr-report.py hangs when run without stdin
// redirected, and a hung verify would stall the whole run with no result.
//
// The test replaces this process's stdin with a pipe nobody writes to, because
// `go test` otherwise supplies /dev/null — under which inheriting stdin reads EOF
// immediately and the assertion cannot fail. That is the exact shape of hollow
// assertion this tool exists to find, and the first run of it found this one.
func TestVerifyGetsNoStdin(t *testing.T) {
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	// Closing the write end on the way out releases anything left blocked on it.
	t.Cleanup(func() { pw.Close(); pr.Close() })
	saved := os.Stdin
	os.Stdin = pr
	t.Cleanup(func() { os.Stdin = saved })

	dir := repo(t, map[string]string{
		"app.py": "x = 1\n",
		"t.sh":   "#!/bin/sh\ncat > /dev/null\nexit 0\n",
	})
	done := make(chan Outcome, 1)
	go func() {
		r := &Runner{Root: dir}
		res, err := r.Run([]Mutation{{
			Source: "x.mut", Target: "app.py", Verify: []string{"./t.sh"},
			Why: "w", Old: "x = 1", New: "x = 2",
		}})
		if err != nil {
			t.Error(err)
			done <- ""
			return
		}
		done <- res[0].Outcome
	}()
	select {
	case got := <-done:
		if got != Survived {
			t.Errorf("outcome = %q, want survived", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a verify command reading stdin blocked the run")
	}
}

func TestScoreCountsHolesAsFailures(t *testing.T) {
	caught, survived, stale := Score([]Result{
		{Outcome: Caught}, {Outcome: Caught}, {Outcome: Survived}, {Outcome: Stale},
	})
	if caught != 2 || survived != 1 || stale != 1 {
		t.Errorf("got %d/%d/%d, want 2/1/1", caught, survived, stale)
	}
}

func TestProgressLineNamesEachMutation(t *testing.T) {
	dir := repo(t, map[string]string{
		"app.py": "x = 1\n",
		"t.sh":   "#!/bin/sh\nexit 1\n",
	})
	var log bytes.Buffer
	r := &Runner{Root: dir, Log: &log}
	if _, err := r.Run([]Mutation{{
		Source: "catalog/flip-compare.mut", Target: "app.py",
		Verify: []string{"./t.sh"}, Why: "w", Old: "x = 1", New: "x = 2",
	}}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(log.String(), "flip-compare") {
		t.Errorf("progress line does not name the mutation: %q", log.String())
	}
}
