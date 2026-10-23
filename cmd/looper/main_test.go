package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pinepeakdigital/looper/internal/evidence"
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

// doRunIn is doRun with a stdin to feed: pr-report reads its narrative from there under
// `-findings-file -`, which is how the skill pipes an orchestrator's findings in.
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

// The push-check subcommand's own surface: the flags it refuses, and its exit statuses. A
// refusal prints its answer on stdout AND exits exactly 3 — not 0, which let
// `looper push-check … && git push` push on push:false (friction log, twice), and not 1 (the tool could not decide),
// which means the tool failed to answer and prints nothing on stdout.
func TestPushCheck(t *testing.T) {
	dir := t.TempDir()
	store := filepath.Join(dir, "runs.jsonl")
	rows := []string{
		`{"run_id":"pc1","phase":"plan","planned_at":"2026-01-01T00:00:00","repo":"x","agent_cap":40}`,
		`{"run_id":"pc1","phase":"cycle","n":1,"applied":0,"asked":0,"agents":3}`,
		`{"run_id":"pc1","phase":"finish","outcome":"clean"}`,
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

	// A refusal is still an answer: exit 3, JSON on stdout, push false. This run has no
	// report landed anywhere, which is what refuses it.
	code, out, errOut := doRun(t, "push-check", "-run-id", "pc1", "-store", store,
		"-gate-state", "passed", "-branch", "feat/x", "-default-branch", "main", "-repo", dir)
	if code != 3 {
		t.Fatalf("a refusal exited %d, want 3: stdout %q stderr %q", code, out, errOut)
	}
	// The refusal is not an error, so run prints nothing for it: stderr carries only the
	// diagnosis notes push documents, and none applies to a report that is simply unposted.
	if strings.Contains(errOut, "push refused") {
		t.Errorf("the refusal was printed as an error: %q", errOut)
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
	if !strings.Contains(got.Reason, "looper pr-report --run-id pc1 --post") {
		t.Errorf("reason = %q, want the owed-report refusal this case is about", got.Reason)
	}
	if got.Convergence != "converged" {
		t.Errorf("convergence = %q, want converged", got.Convergence)
	}
	if got.Disclose != nil {
		t.Errorf("a converged run owes no disclosure, got %q", *got.Disclose)
	}
}

// A run with a record but no finish row refuses through the same channel as every other
// refusal — exit 3, JSON on stdout — naming the run and the commands that close it.
func TestPushCheckRefusesAnUnfinishedRun(t *testing.T) {
	dir := t.TempDir()
	store := filepath.Join(dir, "runs.jsonl")
	rows := []string{
		`{"run_id":"pc4","phase":"plan","planned_at":"2026-01-01T00:00:00","repo":"x","agent_cap":40}`,
		`{"run_id":"pc4","phase":"cycle","n":1,"applied":0,"asked":0,"agents":3}`,
	}
	if err := os.WriteFile(store, []byte(strings.Join(rows, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := doRun(t, "push-check", "-run-id", "pc4", "-store", store,
		"-gate-state", "passed", "-branch", "feat/x", "-default-branch", "main", "-repo", dir)
	if code != 3 {
		t.Fatalf("a refusal exited %d, want 3: stdout %q stderr %q", code, out, errOut)
	}
	var got struct {
		Push   bool   `json:"push"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("stdout is not the JSON Step 14 reads: %v\n%q", err, out)
	}
	if got.Push || !strings.Contains(got.Reason, "run pc4 was never finished") ||
		!strings.Contains(got.Reason, "looper record finish --run-id pc4") {
		t.Errorf("an unfinished run was not refused as one: %q", out)
	}
	// A dead session's way out is abandon plus a fresh run, not a finish: a cycle-limit
	// finish would push a tip its cycles only partly reviewed. Finish stays first. The
	// placeholder is quoted and named as one: pasted bare, a shell reads <gates> as a redirect.
	if !strings.Contains(got.Reason, "if the session that planned it is gone, run "+
		"`looper record abandon --run-id pc4 --missing '<gates>'`, with '<gates>' replaced by "+
		"the planned gates not run and why, and start a fresh run") ||
		strings.Index(got.Reason, "record finish") > strings.Index(got.Reason, "record abandon") {
		t.Errorf("the refusal lacks the dead-session clause after finish: %q", got.Reason)
	}
	if !strings.Contains(errOut, "may be torn") {
		t.Errorf("stderr does not mention a torn finish row: %q", errOut)
	}
}

// A permitted push exits 0 with its JSON, so `looper push-check … && git push` pushes exactly
// when push is true. No branch flags: both are read from git in -repo, which is the shape the
// friction log's refusals came from ("current or default branch unknown" on a feature branch).
func TestPushCheckPermittedExitsZero(t *testing.T) {
	store := prReportStore(t, "pr1")
	dir := fixture(t, map[string]string{"README.md": "x\n"})
	// Named here rather than trusting init.defaultBranch, which is master on some machines.
	git(t, dir, "branch", "-M", "main")
	git(t, dir, "update-ref", "refs/remotes/origin/main", "HEAD")
	git(t, dir, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/main")
	git(t, dir, "checkout", "-q", "-b", "feat/x")
	shim := t.TempDir()
	// gh answers "no PR" to every call, so pr-report defers the body to the pending file and
	// push-check finds it there.
	if err := os.WriteFile(filepath.Join(shim, "gh"), []byte("#!/bin/sh\necho '[]'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", shim+string(os.PathListSeparator)+os.Getenv("PATH"))
	if code, _, errOut := doRun(t, "pr-report", "-run-id", "pr1", "-store", store, "-repo", dir, "-post"); code != 0 {
		t.Fatalf("pr-report exited %d: %q", code, errOut)
	}

	code, out, errOut := doRun(t, "push-check", "-run-id", "pr1", "-store", store,
		"-gate-state", "passed", "-repo", dir)
	var got struct {
		Push   bool   `json:"push"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("stdout is not the JSON Step 14 reads (exit %d, stderr %q): %v\n%q", code, errOut, err, out)
	}
	if !got.Push || code != 0 {
		t.Errorf("a permitted push: exit %d push %v reason %q, want exit 0 and push true", code, got.Push, got.Reason)
	}

	// The same repo on its default branch refuses with exit 3: the default branch was read
	// from origin/HEAD, not left unknown.
	git(t, dir, "checkout", "-q", "main")
	code, out, _ = doRun(t, "push-check", "-run-id", "pr1", "-store", store,
		"-gate-state", "passed", "-repo", dir)
	if code != 3 || !strings.Contains(out, "never auto-push to it") {
		t.Errorf("on the default branch: exit %d, stdout %q; want exit 3 and the default-branch refusal", code, out)
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
		`{"run_id":"pc2","phase":"finish","outcome":"cycle-limit"}`,
	}
	if err := os.WriteFile(store, []byte(strings.Join(rows, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("REVIEW_LOOP_RUNS", store)

	// No -store. If the default wiring is gone this reads some other file, finds no run, and
	// derives `unknown` instead of the `capped` the fixture records — which is exactly the
	// silent failure a wrong store path produces in production.
	// Exit 3: no report has landed in this non-repo dir, so it refuses — with the JSON this
	// test reads.
	code, out, errOut := doRun(t, "push-check", "-run-id", "pc2",
		"-gate-state", "passed", "-branch", "feat/x", "-default-branch", "main", "-repo", dir)
	if code != 3 {
		t.Fatalf("exit = %d, want 3: stdout %q stderr %q", code, out, errOut)
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
	// gh shimmed to fail AND no review-loop home, so neither probe can run — the one
	// state that owes the note.
	shim := t.TempDir()
	if err := os.WriteFile(filepath.Join(shim, "gh"), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", shim+string(os.PathListSeparator)+os.Getenv("PATH"))
	// And no home, so there is no run dir to look in for a kept report.
	t.Setenv("REVIEW_LOOP_HOME", "")
	t.Setenv("HOME", "")

	dir := t.TempDir()
	store := filepath.Join(dir, "runs.jsonl")
	rows := []string{
		`{"run_id":"pc3","phase":"plan","planned_at":"2026-01-01T00:00:00","repo":"x","agent_cap":8}`,
		`{"run_id":"pc3","phase":"cycle","n":1,"applied":3,"agents":9}`,
		`{"run_id":"pc3","phase":"finish","outcome":"clean"}`,
	}
	if err := os.WriteFile(store, []byte(strings.Join(rows, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	code, out, errOut := doRun(t, "push-check", "-run-id", "pc3", "-store", store,
		"-gate-state", "passed", "-branch", "feat/x", "-default-branch", "main", "-repo", dir)
	if code != 3 {
		t.Fatalf("exit = %d, want 3: stdout %q stderr %q", code, out, errOut)
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
	// The refusal text stays the owed-report one the push parity golden pins.
	if !strings.Contains(got.Reason, "looper pr-report --run-id pc3 --post") {
		t.Errorf("reason = %q, want the owed-report refusal", got.Reason)
	}
}

// prReportStore writes a one-run fixture and returns its path.
func prReportStore(t *testing.T, id string) string {
	t.Helper()
	store := filepath.Join(t.TempDir(), "runs.jsonl")
	// The id is JSON-ENCODED, not concatenated: the forging ids this store has to carry include
	// a literal newline and a CR, which would otherwise tear the JSONL row and make every such
	// fixture test an unreadable store rather than the shape it names.
	enc, err := json.Marshal(id)
	if err != nil {
		t.Fatal(err)
	}
	q := string(enc)
	rows := []string{
		`{"run_id":` + q + `,"phase":"plan","planned_at":"2026-01-01T00:00:00","repo":"x","agent_cap":8,"orchestrator_model":"opus","inputs":{"logic":true,"runtime_behavior_change":false,"attacker_reachable":false},"gates":{"evidence":{"planned":"run"}}}`,
		`{"run_id":` + q + `,"phase":"cycle","n":1,"applied":3,"agents":9}`,
		`{"run_id":` + q + `,"phase":"finish","outcome":"cycle-limit","executed":{"evidence":{"status":"done"}}}`,
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
	t.Run("stdin, when asked for with -findings-file -", func(t *testing.T) {
		code, out, errOut := doRunIn(t, strings.NewReader("two real defects\n"),
			"pr-report", "-run-id", "pr1", "-store", store, "-findings-file", "-")
		if code != 0 {
			t.Fatalf("exit = %d: stderr %q", code, errOut)
		}
		if !strings.Contains(out, "### Findings\n\ntwo real defects") {
			t.Errorf("the narrative from stdin is missing:\n%s", out)
		}
	})
	// The RULE end to end, not readNarrative alone: with the flag's default flipped to "-",
	// every readNarrative test stayed green while the CLI went back to reading stdin unasked,
	// which is the hang this rule removed. Measured by the cycle-1 reviewer.
	t.Run("stdin without the flag is not read", func(t *testing.T) {
		code, out, errOut := doRunIn(t, strings.NewReader("from stdin\n"),
			"pr-report", "-run-id", "pr1", "-store", store)
		if code != 0 {
			t.Fatalf("exit = %d: stderr %q", code, errOut)
		}
		if strings.Contains(out, "from stdin") {
			t.Errorf("stdin was read without -findings-file -:\n%s", out)
		}
	})
	t.Run("a findings file", func(t *testing.T) {
		f := filepath.Join(t.TempDir(), "findings.md")
		if err := os.WriteFile(f, []byte("- one thing\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		// Stdin is fed too, and must be ignored: a named file is the narrative, and a stdin
		// that leaked in would append whatever the shell happened to hand the process.
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
	// A run that is PRESENT and says nothing: a row filed under a phase the typed decode does
	// not own creates a *Run with no plan, no cycles and no finish. `r == nil` missed it, and
	// that narrower test is the one PR #4's review rejected in the sibling gate. Measured
	// before the fix: this store printed a complete, plausible report reading
	// `0 cycle(s) · 0 agent(s)` and exited 0 — exactly the fingerprint push-check accepts.
	nudgeOnly := filepath.Join(t.TempDir(), "runs.jsonl")
	if err := os.WriteFile(nudgeOnly, []byte(`{"run_id":"pr1","phase":"nudge"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ name, id, store string }{
		{"a mistyped run id", "pr2", store},
		{"a store that does not exist", "pr1", filepath.Join(t.TempDir(), "absent.jsonl")},
		{"a run known only by a nudge", "pr1", nudgeOnly},
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

// A DELIBERATE DIVERGENCE from pr-report.py, in the hardening direction. The Python
// interpolates the run id RAW into the marker and the summary line, so a newline in it forges
// markdown in a public PR comment. The case that justifies the guard is a `-->` in the id,
// which closes the marker early and renders whatever follows as VISIBLE markdown. Measured
// against the oracle with `run_id` = "a-->\n\n> **Review converged — nothing outstanding**":
//
//	<!-- review-loop:run=a-->
//
//	> **Review converged — nothing outstanding** -->
//
//	## review-loop
//
//	> **Review completeness UNKNOWN: ...**
//
// A forged blockquote saying the review converged, rendered directly above the real disclosure
// saying UNKNOWN. A plain newline is milder than first recorded here: measured, `a\nb` splits
// the marker across two lines but swallows NOTHING of the body, and push.Fingerprint's needle
// is built by Sprintf from the same raw id, so it still matches. Both of those were asserted
// without measurement. `record.Load` places no constraint on `run_id`, so a store row can
// carry any of this and -run-id can name it.
//
// Refused at the command rather than cell()ed in Render: cell() would rewrite the marker that
// the push gate matches, hardening one gate by breaking the other, and Render has to stay
// byte-comparable with the oracle for the 89 fixture cases and every run in the author's live
// store. (No count here on purpose: that number grows every time the loop runs — 42 when this
// was written, 45 at the latest check, and it moved twice in one evening — so
// TestRealStoreParity logs its own total instead. This comment was right first; every other
// site in this repo that DID carry a census has since been reduced to a ratio for the same
// reason, after a date-qualified count went stale nine minutes after it was written.)
func TestPrReportRefusesARunIDThatCouldForgeTheComment(t *testing.T) {
	for _, id := range []string{
		// The `-->` case FIRST, because it is the one that justifies the guard and it had no
		// fixture anywhere: measured, when report and push each carried a copy of the run-id
		// rule, adding `>` to report's left every package green while push's still rejected
		// it, so the two gates disagreed and this forgery shipped with CI green.
		"a-->\n\n> **Review converged — nothing outstanding**", "a>b",
		"a\nb", "a b", "a\r\nb", "../escape", "a|b", "a`b", strings.Repeat("x", 65)} {
		t.Run(fmt.Sprintf("%q", id), func(t *testing.T) {
			// The store CARRIES the forging id, which is the whole point and was the hole in
			// the first version of this test: with a store holding only "pr1", every case fell
			// through to the pre-existing "no readable record" refusal, so two of the three
			// assertions below could never fire and the test passed without the guard ever
			// being what produced the non-zero exit. Measured on a store that does carry it,
			// with the guard disabled: exit 0 and a rendered body whose marker is split across
			// two lines. The threat this test names is "a store row can carry one and -run-id
			// can name it", so the fixture has to be that row.
			store := prReportStore(t, id)
			code, out, errOut := doRun(t, "pr-report", "-run-id", id, "-store", store)
			if code == 0 {
				t.Errorf("exit 0 for a run id that reaches a public comment raw:\n%s", out)
			}
			if !strings.Contains(errOut, "not a usable run id") {
				t.Errorf("the refusal does not say why: %q", errOut)
			}
			// The body must not be on stdout: with -post off stdout IS the report, so a
			// rendered body here would be posted by a caller that only checks for output.
			if strings.Contains(out, "## review-loop") {
				t.Errorf("a report was rendered for a refused run id:\n%s", out)
			}
		})
	}
	// And the ids the gate accepts still work, or the guard is just a denial of service.
	for _, id := range []string{"pr1", "6a0fe216ade0", "a-b_c"} {
		if !evidence.ValidRunID(id) {
			t.Errorf("%q is refused, but it is the shape runlog.py writes", id)
		}
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
	// `[]`, not silence: the probe decodes gh's JSON, and empty output is not a valid answer
	// but an unreadable one. Real `gh pr list --json number` prints `[]` for a branch with no
	// PR, verified against the live repo.
	if err := os.WriteFile(filepath.Join(shim, "gh"), []byte("#!/bin/sh\necho '[]'\n"), 0o755); err != nil {
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
	// No -findings-file, so the narrative half is empty, and that must be visible: before
	// stdin became opt-in a narrative piped in without the flag was read, and after it the
	// same pipe is silently dropped unless something says so.
	if !strings.Contains(errOut, "stdin is piped but not read") {
		t.Errorf("a post with no narrative was silent about it: %q", errOut)
	}
	// The shape production hands it: os.Stdin is always an *os.File, so the MODE decides,
	// and the strings.Reader above never reaches that check. With the check returning false
	// for every pipe, the whole tree stayed green (measured by the cycle-2 reviewer).
	prd, pwr, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	_, _ = pwr.WriteString("a narrative nobody asked for\n")
	_ = pwr.Close()
	defer prd.Close()
	if _, _, errOut := doRunIn(t, prd, "pr-report", "-run-id", "pr1", "-store", store,
		"-repo", dir, "-branch", "feat/x", "-post"); !strings.Contains(errOut, "stdin is piped but not read") {
		t.Errorf("a real pipe without -findings-file - was dropped silently: %q", errOut)
	}
	// And quiet when stdin is a character device: a `</dev/null` flush has no narrative to
	// lose, and a warning on every one of those is noise that buries the real case.
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer devnull.Close()
	if _, _, errOut := doRunIn(t, devnull, "pr-report", "-run-id", "pr1", "-store", store,
		"-repo", dir, "-branch", "feat/x", "-post"); strings.Contains(errOut, "stdin is piped") {
		t.Errorf("a </dev/null post warned about a narrative it could not have had: %q", errOut)
	}
	b, err := os.ReadFile(filepath.Join(os.Getenv("REVIEW_LOOP_HOME"), "run", "pr1", "pending-report.md"))
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
	// And the risk off the plan's inputs, from the same record.
	if !strings.Contains(string(b), "--add-label risk:medium") {
		t.Errorf("the pending note does not owe the plan's risk label:\n%s", b)
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

// Stdin is opt-in. Before, it was read whenever it was not a terminal, so an inherited pipe
// that nobody writes to (an agent harness's stdin) hung `pr-report --post` with no output.
// The fixture is exactly that: an *os.File pipe whose writer stays open. Without the flag
// the read must not happen; a regression blocks, which the timeout turns into a failure
// instead of a hung test binary.
func TestStdinIsReadOnlyWhenAskedFor(t *testing.T) {
	t.Run("an idle pipe is not read without -findings-file -", func(t *testing.T) {
		rd, wr, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		defer rd.Close()
		defer wr.Close()
		type result struct {
			got string
			err error
		}
		done := make(chan result, 1)
		go func() {
			got, err := readNarrative("", rd)
			done <- result{got, err}
		}()
		select {
		case r := <-done:
			if r.err != nil || r.got != "" {
				t.Errorf("no flag gave %q, %v; want an empty narrative", r.got, r.err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("readNarrative blocked on a pipe nobody writes to: stdin is being read " +
				"without -findings-file -, which hangs an orchestrator's pr-report")
		}
	})
	t.Run("a real pipe is read under -findings-file -", func(t *testing.T) {
		rd, wr, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		defer rd.Close()
		go func() {
			_, _ = wr.WriteString("from a pipe")
			_ = wr.Close()
		}()
		got, err := readNarrative("-", rd)
		if err != nil || got != "from a pipe" {
			t.Errorf("-findings-file - gave %q, %v; want the piped narrative", got, err)
		}
	})
}

// `mutate -check` is what CI runs ahead of the fifteen-minute score, and before this test
// its non-zero exit was covered by nothing: deleting the three lines that produce the error
// left `go vet` clean and both packages' tests green, while the binary printed the problems
// AND a false `ok:` line and exited 0 — green CI on a catalog that cannot run. Every sibling
// exit path here has a test (TestMutateAllCaughtExitsZero and the two non-zero ones); the
// gate CI actually depends on had none.
func TestMutateCheckFailsOnACatalogThatWouldNotRun(t *testing.T) {
	dir := fixture(t, map[string]string{
		"app.py": "def f(x):\n    return x > 0\n",
		"t.sh":   "#!/bin/sh\nexit 0\n",
		// A stale anchor: the catalog says `x >= 0`, the file says `x > 0`. This is the
		// rename-rot case, which a full run reports as Stale fifteen minutes in.
		"mutations/stale.mut": "target: app.py\nverify: ./t.sh\nwhy:    w\n--- old\nx >= 0\n--- new\nx < 0\n",
	})
	code, out, errOut := doRun(t, "mutate", "-check", "-root", dir,
		"-catalog", filepath.Join(dir, "mutations"))
	if code != 1 {
		t.Fatalf("exit = %d, want 1 — CI would be green on a catalog that cannot run\n%s\n%s",
			code, out, errOut)
	}
	// The problem goes to STDERR and names the entry; `ok:` must not appear anywhere.
	if !strings.Contains(errOut, "anchor occurs 0 times") {
		t.Errorf("stderr does not say what is wrong: %q", errOut)
	}
	if !strings.Contains(errOut, "stale.mut") {
		t.Errorf("stderr does not name the entry, so CI says a hole exists without saying "+
			"which: %q", errOut)
	}
	if strings.Contains(out, "ok:") || strings.Contains(errOut, "ok:") {
		t.Errorf("the ok line was printed for a catalog that would not run: %q / %q", out, errOut)
	}
}

// The other direction, and the reason the test above means anything: a catalog that WOULD
// run exits 0 and says so on stdout. A -check that always failed would satisfy every
// assertion above while making the gate useless.
func TestMutateCheckPassesACatalogThatWouldRun(t *testing.T) {
	dir := fixture(t, map[string]string{
		"app.py":             "def f(x):\n    return x > 0\n",
		"t.sh":               "#!/bin/sh\nexit 0\n",
		"mutations/flip.mut": caughtCatalog,
	})
	code, out, errOut := doRun(t, "mutate", "-check", "-root", dir,
		"-catalog", filepath.Join(dir, "mutations"))
	if code != 0 {
		t.Fatalf("exit = %d, want 0\n%s\n%s", code, out, errOut)
	}
	if !strings.Contains(out, "ok: 1 entries") {
		t.Errorf("stdout does not report what was checked: %q", out)
	}
	// And it must NOT have run the verify command — that is the whole point of -check, and
	// the fifteen minutes it saves. `t.sh` here exits 0 either way, so the discriminator is
	// the absence of a score.
	if strings.Contains(out, "score") {
		t.Errorf("-check ran the full score instead of stopping: %q", out)
	}
}

// The skill's Step 0 gate: `looper version` must succeed, print one line on stdout and nothing
// on stderr. An older looper answers "unknown command" with exit 2, which is how Step 0 tells
// a binary that has push-check and pr-report from one that does not.
func TestVersionAnswersOnStdout(t *testing.T) {
	code, out, errOut := doRun(t, "version")
	if code != 0 || errOut != "" {
		t.Fatalf("version: code %d, stderr %q", code, errOut)
	}
	if lines := strings.Split(strings.TrimRight(out, "\n"), "\n"); len(lines) != 1 || lines[0] == "" {
		t.Errorf("version printed %q, want one non-empty line", out)
	}
}

// plan.py runs `looper stats --alarm` at Step 0 and prints stdout only on exit 0. The flag is
// spelled as plan.py passes it, and the store comes from the environment as it does there.
func TestStatsAlarmAsPlanPyCallsIt(t *testing.T) {
	dir := t.TempDir()
	store := filepath.Join(dir, "runs.jsonl")
	var rows string
	for i := 1; i <= 3; i++ {
		rows += fmt.Sprintf(`{"run_id":"d%d","phase":"plan","planned_at":"2026-01-0%dT00:00:00","session_id":"s1","gates":{"g":{"planned":"run"}}}`+"\n", i, i)
		rows += fmt.Sprintf(`{"run_id":"d%d","phase":"finish","outcome":"clean","executed":{}}`+"\n", i)
	}
	if err := os.WriteFile(store, []byte(rows), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("REVIEW_LOOP_RUNS", store)
	t.Setenv("CLAUDE_CODE_SESSION_ID", "s1")
	t.Setenv("AO_SESSION_ID", "")
	t.Setenv("REVIEW_LOOP_TAIL", "")
	code, out, errOut := doRun(t, "stats", "--alarm")
	if code != 0 || errOut != "" || !strings.Contains(out, "  g did not complete 3x\n") {
		t.Errorf("stats --alarm: exit %d, stdout %q, stderr %q", code, out, errOut)
	}
	// The report, and its status passed through rather than mapped: 1 for a store that exists
	// and cannot be read, which plan.py must not mistake for a clean alarm.
	if code, out, _ := doRun(t, "stats"); code != 0 || !strings.HasPrefix(out, "runs: 3  finished: 3") {
		t.Errorf("stats: exit %d, stdout %q", code, out)
	}
	t.Setenv("REVIEW_LOOP_RUNS", dir)
	if code, out, errOut := doRun(t, "stats", "--alarm"); code != 1 || out != "" || !strings.Contains(errOut, "cannot read the run store") {
		t.Errorf("stats --alarm on a directory: exit %d, stdout %q, stderr %q", code, out, errOut)
	}
	if code, _, _ := doRun(t, "stats", "--bogus"); code != 2 {
		t.Errorf("stats --bogus: exit %d, want 2", code)
	}
}

// The skill runs `looper threat-model` and `looper deferred` bare, from the repo, and plan.py
// parses threat-model's stdout as JSON. Outside any repo both report no ledger and exit 0, as
// the Python did; the statuses are the subcommands' own, passed through rather than mapped.
func TestLedgerCommandsAsTheSkillCallsThem(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GIT_CEILING_DIRECTORIES", dir)
	t.Setenv("GIT_DIR", "")
	os.Unsetenv("GIT_DIR")
	t.Chdir(dir)
	for cmd, want := range map[string]string{
		"threat-model": "{\n  \"path\": \"\",\n  \"exists\": false,\n  \"claims\": 0,\n  \"stale\": [],\n  \"broken_pins\": [],\n  \"uncited\": 0\n}\n",
		"deferred":     "{\n  \"path\": \"\",\n  \"exists\": false,\n  \"entries\": 0,\n  \"stale\": [],\n  \"unpinned\": []\n}\n",
	} {
		if code, out, errOut := doRun(t, cmd); code != 0 || out != want || errOut != "" {
			t.Errorf("%s: exit %d, stdout %q, stderr %q", cmd, code, out, errOut)
		}
		if code, out, _ := doRun(t, cmd, "--selftest"); code != 2 || out != "" {
			t.Errorf("%s --selftest: exit %d, stdout %q; want 2 and nothing", cmd, code, out)
		}
	}
}
