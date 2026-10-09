package main

import (
	"bytes"
	"encoding/json"
	"io"
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
	return doRunIn(t, strings.NewReader(""), args...)
}

// doRunIn is doRun with a stdin to feed: pr-report reads its narrative from there when no
// -findings-file is named, which is how the skill pipes an orchestrator's findings in.
func doRunIn(t *testing.T, in io.Reader, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := run(args, in, &out, &errOut)
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

// The push-check subcommand's own surface: the flags it refuses, and the fact that it
// prints its answer on stdout and exits 0 whether or not the push is permitted. A refusal
// that exits non-zero is indistinguishable from the tool failing to produce one, and Step 14
// reads the JSON either way.
func TestPushCheck(t *testing.T) {
	dir := t.TempDir()
	store := filepath.Join(dir, "runs.jsonl")
	rows := []string{
		`{"run_id":"pc1","phase":"plan","planned_at":"2026-01-01T00:00:00","repo":"x","agent_cap":40}`,
		`{"run_id":"pc1","phase":"cycle","n":1,"applied":0,"asked":0,"agents":3}`,
	}
	if err := os.WriteFile(store, []byte(strings.Join(rows, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// -run-id is REQUIRED. In the Python it was optional, and both record-derived blockers
	// were computed only when it was present — so omitting it turned off the owed-report
	// check AND the broken-outcome check, while a bogus id was correctly caught. The
	// cheapest wrong spelling was the one that passed.
	code, _, errOut := doRun(t, "push-check")
	if code == 0 || !strings.Contains(errOut, "-run-id is required") {
		t.Errorf("a missing -run-id gave exit %d and stderr %q", code, errOut)
	}

	// An unrecognised gate state is refused rather than read as "not blocked". The Python
	// gets this from argparse `choices`; here it is an explicit switch, so it needs a test.
	code, _, errOut = doRun(t, "push-check", "-run-id", "pc1", "-gate-state", "pased", "-store", store)
	if code == 0 || !strings.Contains(errOut, "not passed, skipped or blocked") {
		t.Errorf("a misspelled -gate-state gave exit %d and stderr %q", code, errOut)
	}

	// A refusal is still a successful answer: exit 0, JSON on stdout, push false. This run
	// has no report landed anywhere, which is what refuses it.
	code, out, errOut := doRun(t, "push-check", "-run-id", "pc1", "-store", store,
		"-gate-state", "passed", "-branch", "feat/x", "-default-branch", "main", "-repo", dir)
	if code != 0 {
		t.Fatalf("a refusal exited %d: stdout %q stderr %q", code, out, errOut)
	}
	var got struct {
		Push        bool    `json:"push"`
		Reason      string  `json:"reason"`
		Convergence string  `json:"convergence"`
		Disclose    *string `json:"disclose"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("stdout is not the JSON Step 14 reads: %v\n%q", err, out)
	}
	if got.Push {
		t.Errorf("a run with no report landed was permitted: %q", out)
	}
	if got.Convergence != "converged" {
		t.Errorf("convergence = %q, want converged", got.Convergence)
	}
	if got.Disclose != nil {
		t.Errorf("a converged run owes no disclosure, got %q", *got.Disclose)
	}
}

// push-check's stdout is a machine-readable contract — Step 14 parses it as JSON — so NOTHING
// but the decision may reach it. `flag.ContinueOnError` writes both `-h` text and parse-error
// text to the flag set's output, and that was pointed at stdout: measured before the fix,
// `looper push-check -h` and `looper push-check -bogus xyz` each put the usage banner on
// stdout, and both exited 1.
//
// `-h` is a request that was SERVED. Exiting non-zero made it indistinguishable from a
// mistyped flag, where the Python exits 0 for -h and 2 for a parse error. The exit code for a
// parse error stays 1 here rather than 2, because `run` returns 1 for every error and 2 only
// for an unknown subcommand; that is an enumerated divergence, not an oversight.
func TestPushCheckFlagOutputNeverTouchesStdout(t *testing.T) {
	for _, c := range []struct {
		name     string
		args     []string
		wantCode int
		saysOn   string // substring required on stderr
	}{
		{"help is served, not failed", []string{"push-check", "-h"}, 0, "-run-id"},
		{"an unknown flag is a parse error", []string{"push-check", "-bogus-flag", "xyz"}, 1, "not defined"},
		{"a missing run id", []string{"push-check"}, 1, "-run-id is required"},
		{"a misspelled gate state", []string{"push-check", "-run-id", "pc1", "-gate-state", "pased"}, 1, "not passed, skipped or blocked"},
	} {
		t.Run(c.name, func(t *testing.T) {
			code, out, errOut := doRun(t, c.args...)
			if code != c.wantCode {
				t.Errorf("exit = %d, want %d (stderr %q)", code, c.wantCode, errOut)
			}
			if out != "" {
				t.Errorf("stdout must carry the decision and nothing else; got %q", out)
			}
			if !strings.Contains(errOut, c.saysOn) {
				t.Errorf("stderr does not mention %q: %q", c.saysOn, errOut)
			}
		})
	}
}

// The `-store` default is the one wiring between the subcommand and record.StorePath, and
// every other test passes `-store` explicitly, so deleting these two lines changed nothing.
// Exercised through $REVIEW_LOOP_RUNS rather than the real default, which would read the
// operator's own record.
func TestPushCheckDefaultsTheStoreFromTheEnvironment(t *testing.T) {
	dir := t.TempDir()
	store := filepath.Join(dir, "runs.jsonl")
	rows := []string{
		`{"run_id":"pc2","phase":"plan","planned_at":"2026-01-01T00:00:00","repo":"x","agent_cap":8}`,
		`{"run_id":"pc2","phase":"cycle","n":1,"applied":3,"agents":9}`,
	}
	if err := os.WriteFile(store, []byte(strings.Join(rows, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("REVIEW_LOOP_RUNS", store)

	// No -store. If the default wiring is gone this reads some other file, finds no run, and
	// derives `unknown` instead of the `capped` the fixture records — which is exactly the
	// silent failure a wrong store path produces in production.
	code, out, errOut := doRun(t, "push-check", "-run-id", "pc2",
		"-gate-state", "passed", "-branch", "feat/x", "-default-branch", "main", "-repo", dir)
	if code != 0 {
		t.Fatalf("exit = %d: stdout %q stderr %q", code, out, errOut)
	}
	var got struct {
		Convergence string  `json:"convergence"`
		Disclose    *string `json:"disclose"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%q", err, out)
	}
	if got.Convergence != "capped" {
		t.Errorf("convergence = %q, want capped — the store named by $REVIEW_LOOP_RUNS was not read", got.Convergence)
	}
	if got.Disclose == nil || !strings.Contains(*got.Disclose, "CAPPED at 9 of 8 agents") {
		t.Errorf("disclosure = %v, want the capped line derived from the fixture's cycle row", got.Disclose)
	}
}

// The one line wiring the diagnostic to the real binary: `Diag: errOut` in runPushCheck's
// CheckParams literal. Deleting it left `go test ./...` fully green, including all eight subtests
// of TestCheckDiagnosesBeingUnableToAsk and TestCheckDiagnosesARunMissingFromTheStore — the
// two library-level tests written for the feature — none of which goes through the CLI.
// (This said "all four ... the one test" for one cycle, which was the same undercount it
// had just replaced: the second Diag test landed in the very commit that wrote it.) Reproduced before this existed.
//
// stdout must still carry only the decision: the note is a second stream, not a prefix.
func TestPushCheckWritesTheDiagnosisToStderr(t *testing.T) {
	// gh shimmed to fail AND a repo that is not a git dir, so neither probe can run — the one
	// state that owes the note.
	shim := t.TempDir()
	if err := os.WriteFile(filepath.Join(shim, "gh"), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", shim+string(os.PathListSeparator)+os.Getenv("PATH"))

	dir := t.TempDir()
	store := filepath.Join(dir, "runs.jsonl")
	rows := []string{
		`{"run_id":"pc3","phase":"plan","planned_at":"2026-01-01T00:00:00","repo":"x","agent_cap":8}`,
		`{"run_id":"pc3","phase":"cycle","n":1,"applied":3,"agents":9}`,
	}
	if err := os.WriteFile(store, []byte(strings.Join(rows, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	code, out, errOut := doRun(t, "push-check", "-run-id", "pc3", "-store", store,
		"-gate-state", "passed", "-branch", "feat/x", "-default-branch", "main", "-repo", dir)
	if code != 0 {
		t.Fatalf("exit = %d: stdout %q stderr %q", code, out, errOut)
	}
	if !strings.Contains(errOut, "could not be determined") {
		t.Errorf("stderr = %q, want the diagnosis that the report check could not be run", errOut)
	}
	var got struct {
		Push   bool   `json:"push"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("stdout is not the JSON Step 14 reads: %v\n%q", err, out)
	}
	if got.Push {
		t.Errorf("the push was permitted despite an undeterminable report: %q", out)
	}
	// The refusal text is unchanged, which is what keeps the parity gate green.
	if !strings.Contains(got.Reason, "pr-report.py --post") {
		t.Errorf("reason = %q, want the unchanged owed-report refusal", got.Reason)
	}
}

// prReportStore writes a one-run fixture and returns its path.
func prReportStore(t *testing.T, id string) string {
	t.Helper()
	store := filepath.Join(t.TempDir(), "runs.jsonl")
	rows := []string{
		`{"run_id":"` + id + `","phase":"plan","planned_at":"2026-01-01T00:00:00","repo":"x","agent_cap":8,"orchestrator_model":"opus","gates":{"evidence":{"planned":"run"}}}`,
		`{"run_id":"` + id + `","phase":"cycle","n":1,"applied":3,"agents":9}`,
		`{"run_id":"` + id + `","phase":"finish","outcome":"cycle-limit","executed":{"evidence":{"status":"done"}}}`,
	}
	if err := os.WriteFile(store, []byte(strings.Join(rows, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return store
}

func TestPrReportWritesTheBodyToStdout(t *testing.T) {
	store := prReportStore(t, "pr1")
	code, out, errOut := doRun(t, "pr-report", "-run-id", "pr1", "-store", store)
	if code != 0 {
		t.Fatalf("exit = %d: stderr %q", code, errOut)
	}
	// The marker and the `N cycle(s) · M agent(s)` line are what push-check looks for; if
	// either moves, every push is refused on advice that cannot succeed.
	for _, want := range []string{
		"<!-- review-loop:run=pr1 -->",
		"## review-loop",
		"1 cycle(s) · 9 agent(s)",
		"CAPPED at 9 of 8 agents",
		"| `evidence` | run | done |",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout is missing %q:\n%s", want, out)
		}
	}
	if errOut != "" {
		t.Errorf("stderr should be silent without -post: %q", errOut)
	}
}

func TestPrReportTakesTheNarrativeFromStdinAndFromAFile(t *testing.T) {
	store := prReportStore(t, "pr1")
	t.Run("stdin", func(t *testing.T) {
		code, out, errOut := doRunIn(t, strings.NewReader("two real defects\n"),
			"pr-report", "-run-id", "pr1", "-store", store)
		if code != 0 {
			t.Fatalf("exit = %d: stderr %q", code, errOut)
		}
		if !strings.Contains(out, "### Findings\n\ntwo real defects") {
			t.Errorf("the narrative from stdin is missing:\n%s", out)
		}
	})
	t.Run("a findings file", func(t *testing.T) {
		f := filepath.Join(t.TempDir(), "findings.md")
		if err := os.WriteFile(f, []byte("- one thing\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		// Stdin is fed too, and must LOSE: -findings-file is the explicit channel, and a
		// piped-in stdin that silently won would append whatever the shell happened to hand
		// the process.
		code, out, errOut := doRunIn(t, strings.NewReader("from stdin"),
			"pr-report", "-run-id", "pr1", "-store", store, "-findings-file", f)
		if code != 0 {
			t.Fatalf("exit = %d: stderr %q", code, errOut)
		}
		if !strings.Contains(out, "- one thing") || strings.Contains(out, "from stdin") {
			t.Errorf("the findings file did not win over stdin:\n%s", out)
		}
	})
	t.Run("a missing findings file is an error", func(t *testing.T) {
		code, out, _ := doRun(t, "pr-report", "-run-id", "pr1", "-store", store,
			"-findings-file", filepath.Join(t.TempDir(), "nope.md"))
		// Not silently empty: the narrative is the half of the report the record cannot
		// derive, and a typo'd path would post a report with the findings section missing.
		if code == 0 {
			t.Errorf("a missing findings file exited 0 and printed:\n%s", out)
		}
	})
}

// record.Load returns an empty map and NO error for a store that does not hold the run, so a
// report rendered off nothing is not an empty report: it is a GENUINE one reading `0 cycle(s)
// · 0 agent(s)`, which is exactly what push-check's required fingerprint collapses to. The
// two halves refuse together, by the same measurement.
func TestPrReportRefusesARunItCannotRead(t *testing.T) {
	store := prReportStore(t, "pr1")
	for _, c := range []struct{ name, id, store string }{
		{"a mistyped run id", "pr2", store},
		{"a store that does not exist", "pr1", filepath.Join(t.TempDir(), "absent.jsonl")},
	} {
		t.Run(c.name, func(t *testing.T) {
			code, out, errOut := doRun(t, "pr-report", "-run-id", c.id, "-store", c.store)
			if code == 0 {
				t.Errorf("exit 0 — a report was rendered from no record:\n%s", out)
			}
			if out != "" {
				t.Errorf("stdout should be empty on a refusal: %q", out)
			}
			// Both halves named: the id is usually not the wrong one.
			if !strings.Contains(errOut, c.id) || !strings.Contains(errOut, c.store) {
				t.Errorf("the refusal names neither the run nor the store: %q", errOut)
			}
		})
	}
}

func TestPrReportRequiresARunID(t *testing.T) {
	code, out, errOut := doRun(t, "pr-report")
	if code == 0 {
		t.Errorf("exit 0 with no -run-id:\n%s", out)
	}
	if !strings.Contains(errOut, "-run-id is required") {
		t.Errorf("stderr does not say what is missing: %q", errOut)
	}
}

// Without -post, stdout is the report BODY — so a usage banner or a parse error landing there
// would be posted as the review disclosure on the next run.
func TestPrReportFlagOutputNeverTouchesStdout(t *testing.T) {
	t.Run("help", func(t *testing.T) {
		code, out, errOut := doRun(t, "pr-report", "-h")
		if code != 0 {
			t.Errorf("exit = %d for -h, which is a request that was served", code)
		}
		if out != "" {
			t.Errorf("the usage banner landed on stdout: %q", out)
		}
		if !strings.Contains(errOut, "-run-id") {
			t.Errorf("stderr has no usage text: %q", errOut)
		}
	})
	t.Run("a mistyped flag", func(t *testing.T) {
		code, out, _ := doRun(t, "pr-report", "-run-ids", "x")
		if code == 0 {
			t.Error("a mistyped flag exited 0")
		}
		if out != "" {
			t.Errorf("the parse error landed on stdout: %q", out)
		}
	})
}

// The wiring -post depends on: the body reaching report.Post, and its notes reaching stderr
// rather than stdout. Shimmed gh answers "no PR", the ordinary case for a fresh branch.
func TestPrReportPostDefersAndReportsWhereTo(t *testing.T) {
	store := prReportStore(t, "pr1")
	dir := fixture(t, map[string]string{"README.md": "x\n"})
	shim := t.TempDir()
	if err := os.WriteFile(filepath.Join(shim, "gh"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", shim+string(os.PathListSeparator)+os.Getenv("PATH"))

	code, out, errOut := doRun(t, "pr-report", "-run-id", "pr1", "-store", store,
		"-repo", dir, "-branch", "feat/x", "-post")
	if code != 0 {
		t.Fatalf("exit = %d: stderr %q", code, errOut)
	}
	if out != "" {
		t.Errorf("-post must not print the body to stdout: %q", out)
	}
	if !strings.Contains(errOut, "deferred to") || !strings.Contains(errOut, "no PR for this branch yet") {
		t.Errorf("stderr does not say where the report went: %q", errOut)
	}
	b, err := os.ReadFile(filepath.Join(dir, ".git", "info", "review-loop-pending-report.pr1.md"))
	if err != nil {
		t.Fatalf("the report was not kept: %v", err)
	}
	if !strings.Contains(string(b), "1 cycle(s) · 9 agent(s)") {
		t.Errorf("the kept report is not the rendered one:\n%s", b)
	}
	// The derived convergence, not a flag: the label the pending note owes and the
	// disclosure in the body cannot disagree, because both come off the same read.
	if !strings.Contains(string(b), "label review:capped still owed") {
		t.Errorf("the label-owed note does not carry the derived convergence:\n%s", b)
	}
}

func TestPrReportDefaultsTheStoreFromTheEnvironment(t *testing.T) {
	store := prReportStore(t, "pr1")
	t.Setenv("REVIEW_LOOP_RUNS", store)
	// No -store. If the default wiring is gone this reads the real store, finds no `pr1`, and
	// refuses — which is how this failure now shows up rather than rendering from nothing.
	code, out, errOut := doRun(t, "pr-report", "-run-id", "pr1")
	if code != 0 {
		t.Fatalf("exit = %d: stderr %q", code, errOut)
	}
	if !strings.Contains(out, "1 cycle(s) · 9 agent(s)") {
		t.Errorf("the store named by $REVIEW_LOOP_RUNS was not read:\n%s", out)
	}
}
