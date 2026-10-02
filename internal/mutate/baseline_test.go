package mutate

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Two mutations naming the same verify command must baseline it once, not twice. Without
// the dedup the baseline cost scales with the catalog rather than with the number of
// distinct commands, which on a real catalog is the difference between one extra suite run
// and thirty.
func TestBaselineRunsEachCommandOnce(t *testing.T) {
	dir := repo(t, map[string]string{
		"app.py":     "x = 1\ny = 1\n",
		"t.sh":       "#!/bin/sh\necho ran >> counter\ngrep -q 'x = 1' app.py && exit 0\necho '--- FAIL: TestX'\nexit 1\n",
		".gitignore": "counter\n",
	})
	same := []string{"./t.sh"}
	run(t, dir,
		Mutation{Source: "a.mut", Target: "app.py", Verify: same, Why: "w", Old: "x = 1", New: "x = 2"},
		Mutation{Source: "b.mut", Target: "app.py", Verify: same, Why: "w", Old: "y = 1", New: "y = 2"},
	)
	counter, err := os.ReadFile(filepath.Join(dir, "counter"))
	if err != nil {
		t.Fatal(err)
	}
	// One baseline run, plus one per mutation.
	if n := strings.Count(string(counter), "ran"); n != 3 {
		t.Errorf("the command ran %d time(s), want 3 (one baseline + two mutations) — the "+
			"baseline is not deduplicating", n)
	}
}

func TestDistinctCommandsDedupesAndSorts(t *testing.T) {
	got := distinctCommands([]Mutation{
		{Verify: []string{"./zebra.sh", "./alpha.sh"}},
		{Verify: []string{"./alpha.sh", "./middle.sh"}},
	})
	if strings.Join(got, ",") != "./alpha.sh,./middle.sh,./zebra.sh" {
		t.Errorf("got %v, want the three sorted and deduplicated", got)
	}
}

// A command that passes while running no tests can never fail either, so it cannot be
// evidence of anything. `go test -run` exits 0 when its pattern matches nothing, which is
// what a renamed test leaves behind.
func TestCommandThatRunsNoTestsIsVacuous(t *testing.T) {
	dir := repo(t, map[string]string{
		"app.py": "x = 1\n",
		// What `go test -run NoSuchTest` prints, on a command that exits 0.
		"t.sh": "#!/bin/sh\necho 'testing: warning: no tests to run'\necho 'ok  \tpkg\t0.001s [no tests to run]'\nexit 0\n",
	})
	res := run(t, dir, Mutation{
		Source: "x.mut", Target: "app.py", Verify: []string{"./t.sh"},
		Why: "w", Old: "x = 1", New: "x = 2",
	})
	if res[0].Outcome != Broken {
		t.Fatalf("outcome = %q, want broken — a command that runs nothing read as a hole "+
			"in the suite (detail: %s)", res[0].Outcome, res[0].Detail)
	}
	if !strings.Contains(res[0].Detail, "without running any test") {
		t.Errorf("detail does not say why: %q", res[0].Detail)
	}
}

// A real `go test` invocation naming a test that does not exist must be caught the same
// way. Built on its own tiny module rather than on this repo, so the test does not depend
// on the live working tree being clean.
func TestRealGoTestWithNoMatchIsVacuous(t *testing.T) {
	dir := repo(t, map[string]string{
		"go.mod":     "module probe\n\ngo 1.24\n",
		"p.go":       "package p\n\nfunc A() int { return 1 }\n",
		"p_test.go":  "package p\n\nimport \"testing\"\n\nfunc TestA(t *testing.T) {\n\tif A() != 1 {\n\t\tt.Fatal(\"no\")\n\t}\n}\n",
		".gitignore": "",
	})
	r := &Runner{Root: dir}

	// The pattern that exists: sound.
	bad, err := r.baseline([]Mutation{{
		Source: "ok.mut", Target: "p.go", Verify: []string{"go test . -count=1 -run 'TestA'"},
		Why: "w", Old: "return 1", New: "return 2",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(bad) != 0 {
		t.Fatalf("rejected a command that does run its test: %v", bad)
	}

	// The pattern that does not: `go test` exits 0, so only the output gives it away.
	bad, err = r.baseline([]Mutation{{
		Source: "x.mut", Target: "p.go", Verify: []string{"go test . -count=1 -run 'TestNoSuchNameAnywhere'"},
		Why: "w", Old: "return 1", New: "return 2",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(bad) != 1 {
		t.Fatalf("a -run pattern matching no test was accepted as sound: %v", bad)
	}
	for _, why := range bad {
		if !strings.Contains(why, "without running any test") {
			t.Errorf("wrong reason: %q", why)
		}
	}
}

// The baseline names what it found, or an operator reading CI output cannot tell a
// mutation that was never evaluated from one the suite genuinely missed.
func TestBaselineLogsWhatItRejected(t *testing.T) {
	dir := repo(t, map[string]string{
		"app.py": "x = 1\n",
		"t.sh":   "#!/bin/sh\necho '--- FAIL: TestUnrelated'\nexit 1\n",
	})
	var log bytes.Buffer
	r := &Runner{Root: dir, Log: &log}
	if _, err := r.Run([]Mutation{{
		Source: "x.mut", Target: "app.py", Verify: []string{"./t.sh"},
		Why: "w", Old: "x = 1", New: "x = 2", Expect: "--- FAIL:",
	}}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"baseline: 1 distinct", "ALREADY RED", "./t.sh"} {
		if !strings.Contains(log.String(), want) {
			t.Errorf("log does not mention %q:\n%s", want, log.String())
		}
	}
}

// A verify command that writes a TRACKED file during the baseline makes every later
// "original" suspect, so the run stops rather than restoring post-baseline bytes.
func TestBaselineRefusesIfACommandDirtiesTheTree(t *testing.T) {
	dir := repo(t, map[string]string{
		"app.py":      "x = 1\n",
		"tracked.txt": "before\n",
		"t.sh":        "#!/bin/sh\necho after > tracked.txt\nexit 0\n",
	})
	r := &Runner{Root: dir}
	_, err := r.Run([]Mutation{{
		Source: "x.mut", Target: "app.py", Verify: []string{"./t.sh"},
		Why: "w", Old: "x = 1", New: "x = 2",
	}})
	if err == nil {
		t.Fatal("ran on after a verify command changed a tracked file")
	}
	if !strings.Contains(err.Error(), "changed the working tree during the baseline") {
		t.Errorf("error does not name the cause: %v", err)
	}
}
