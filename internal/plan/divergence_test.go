package plan

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pinepeakdigital/looper/internal/golden"
)

// Where plan.py crashed, answered falsely, or is spelled differently on purpose, looper decides
// instead. Each case keeps plan.py's frozen answer beside looper's, so the test fails if the
// record of what is being diverged FROM is lost. README.md lists them.

type divergence struct {
	scenario
	prep func(t *testing.T, w world) // after the world is built
	raw  []string                    // the whole argv, when it must not carry --model or --dry-run
}

func (d divergence) argv() []string {
	if d.raw != nil {
		return d.raw
	}
	return d.scenario.argv()
}

func (d divergence) run(t *testing.T) (code int, out, errOut string, w world) {
	t.Helper()
	w = d.build(t)
	if d.prep != nil {
		d.prep(t, w)
	}
	w.apply(t)
	var o, e bytes.Buffer
	code = Command(d.argv(), strings.NewReader(d.ctx), &o, &e)
	return code, o.String(), e.String(), w
}

func pyExit(t *testing.T, py string) string {
	t.Helper()
	return strings.SplitN(py, "\n", 2)[0]
}

// Error sentences are looper's own and say `looper plan`; the exit statuses are plan.py's:
// 1 for a context that cannot be read, 2 for a usage error, 0 for -h. Nothing reaches stdout,
// so no partial plan can be read as a plan.
var usageAndInput = []struct {
	name   string
	d      divergence
	stderr string
}{
	{"context not JSON", divergence{scenario: scenario{ctx: "{not json"}}, "context is not valid JSON"},
	{"context trailing data", divergence{scenario: scenario{ctx: ctxDefault + " {}"}}, "context is not valid JSON"},
	{"context file missing", divergence{scenario: scenario{ctx: ctxDefault, args: with("--context", "/nonexistent/ctx.json")}}, "cannot read the context file"},
	{"no model", divergence{scenario: scenario{ctx: ctxDefault}, raw: append(with(), "--dry-run")}, "--model"},
	{"no answers", divergence{scenario: scenario{ctx: ctxDefault, args: []string{"--logic", "yes"}}}, "--behavioral-goal, --runtime-change, --attacker-reachable"},
	{"unknown flag", divergence{scenario: scenario{ctx: ctxDefault, args: with("--frobnicate", "yes")}}, "frobnicate"},
	{"positional argument", divergence{scenario: scenario{ctx: ctxDefault, args: append(with(), "extra")}}, "extra"},
}

func TestUsageAndInputErrorsExitAsThePythonDid(t *testing.T) {
	for _, c := range usageAndInput {
		t.Run(c.name, func(t *testing.T) {
			py := golden.Read(t, "transcript")
			code, out, errOut, w := c.d.run(t)
			got := w.result(t, code, out, errOut)
			if pyExit(t, got) != pyExit(t, py) || out != "" || !strings.Contains(errOut, c.stderr) {
				t.Errorf("--- python (frozen) ---\n%s\n--- looper ---\n%s\nwant the same exit, no stdout, stderr naming %q", py, got, c.stderr)
			}
		})
	}
}

// A context that is not an object, or a field of the wrong type, exits 1 naming it. plan.py
// read context.sh's JSON with dict.get and Python truthiness: a list crashed it with a
// traceback, and a string where a count belongs passed straight through into the plan JSON
// and the record. context.sh writes neither; the check is what the plan's arithmetic assumes.
var badContext = []struct{ name, ctx, stderr string }{
	{"context is a list", `[1, 2]`, "not a JSON object"},
	{"a count is a string", ctx(`{"changed_lines": "40"}`), "changed_lines is \"40\", not a whole number"},
	{"a count is fractional", ctx(`{"semantic_lines": 12.5}`), "semantic_lines is 12.5, not a whole number"},
	{"a flag is a number", ctx(`{"fast_path_eligible_by_size": 1}`), "fast_path_eligible_by_size is 1, not true or false"},
	{"a name is a number", ctx(`{"base_branch": 7}`), "base_branch is 7, not a string"},
	{"learnings entries null", ctx(`{"learnings_entries": null}`), "learnings_entries is null"},
}

func TestAWrongContextIsAnError(t *testing.T) {
	for _, c := range badContext {
		t.Run(c.name, func(t *testing.T) {
			golden.Read(t, "transcript") // what plan.py did, kept beside the decision
			code, out, errOut, _ := divergence{scenario: scenario{ctx: c.ctx}}.run(t)
			if code != 1 || out != "" || !strings.Contains(errOut, c.stderr) {
				t.Errorf("exit %d, stdout %q, stderr %q; want exit 1, no stdout, stderr naming %q", code, out, errOut, c.stderr)
			}
		})
	}
}

// An abbreviated flag is a usage error. argparse took any unambiguous prefix (--log for
// --logic); Go's flag package takes none, and a skill that only ever spells them out loses
// nothing by it.
func TestAnAbbreviatedFlagIsRefused(t *testing.T) {
	py := golden.Read(t, "transcript")
	if !strings.HasPrefix(py, "exit=0\n") {
		t.Fatalf("the frozen oracle no longer records argparse taking the prefix:\n%s", py)
	}
	code, out, _, _ := divergence{scenario: scenario{ctx: ctxDefault, args: []string{"--log", "yes",
		"--behavioral-goal", "yes", "--runtime-change", "yes", "--attacker-reachable", "no"}}}.run(t)
	if code != 2 || out != "" {
		t.Errorf("exit %d, stdout %q; want exit 2 and no plan", code, out)
	}
}

// When the record write fails, the plan is on stdout and the record's own error and exit
// status are passed on, then one sentence says nothing was recorded — as plan.py did, except
// that the sentence says to re-run `looper plan`.
func TestAFailedRecordSaysSo(t *testing.T) {
	py := golden.Read(t, "transcript")
	d := divergence{scenario: scenario{ctx: ctxDefault, store: "dir", record: true}}
	code, out, errOut, w := d.run(t)
	got := w.result(t, code, out, errOut)
	want := strings.ReplaceAll(strings.ReplaceAll(py, "plan: COMPUTED", "looper plan: COMPUTED"), "re-run plan.py", "re-run looper plan")
	if got != want {
		t.Errorf("--- python (frozen), renamed ---\n%s\n--- looper ---\n%s", want, got)
	}
}

// A changed file's name is read as bytes, whole. plan.py read git's output in text mode, which
// turns a \r in a name into \n, and stripped it, which drops a leading space from the first
// name; either way the big file it named was not found and counted as no lines, and Agent #7
// was planned skip on a diff with a 900-line file in it.
func TestANameGitPrintsIsCounted(t *testing.T) {
	for _, name := range []string{"b\rig.txt", " lead.txt"} {
		t.Run(strings.NewReplacer("\r", "CR", " ", "SP").Replace(name), func(t *testing.T) {
			py := golden.Read(t, "transcript")
			if !strings.Contains(py, `"40 lines of review surface, largest changed file 3 lines \u2014 under both floors (150/800)"`) {
				t.Fatalf("the frozen oracle no longer records the name being missed:\n%s", py)
			}
			_, out, _, _ := divergence{scenario: scenario{ctx: ctxDefault}, prep: renameBig(name)}.run(t)
			if !strings.Contains(out, `"40 lines of review surface, largest changed file 900 lines"`) {
				t.Errorf("the 900-line file was not counted:\n%s", out)
			}
		})
	}
}

// renameBig renames big.txt, the only big file on the branch, and commits it.
func renameBig(to string) func(t *testing.T, w world) {
	return func(t *testing.T, w world) {
		t.Helper()
		if err := os.Rename(filepath.Join(w.repo, "big.txt"), filepath.Join(w.repo, to)); err != nil {
			t.Fatal(err)
		}
		git(t, w.repo, "add", "-A")
		git(t, w.repo, "commit", "-q", "-m", "rename")
	}
}

// A largest-file scan that cannot run plans Agent #7 and #8, saying why. plan.py read a failed
// git as a 0-line file, so a small diff whose base was not fetched planned #7 skip "under both
// floors" — a measurement it had not made. When no number decides it, the skill takes the
// wider option.
func TestAnUnmeasuredDiffPlansWide(t *testing.T) {
	py := golden.Read(t, "transcript")
	if !strings.Contains(py, `"40 lines of review surface \u2014 under both floors (150/800)"`) {
		t.Fatalf("the frozen oracle no longer records the skip:\n%s", py)
	}
	_, out, _, _ := divergence{scenario: scenario{ctx: ctx(`{"base_branch": "nosuch"}`)}}.run(t)
	for _, want := range []string{
		`"40 lines of review surface \u2014 the largest changed file could not be measured (fatal: `,
		`"planned as substantial, since the diff could not be measured (#7's reason) \u2014 skip the observability half if the project logs nothing"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("want %s in:\n%s", want, out)
		}
	}
}

// A probe that outlives its timeout is stopped at it, even when it leaves a grandchild holding
// its pipes, and the gate says it timed out in TimeoutExpired's words. Without WaitDelay the
// wait ran on until the grandchild exited.
func TestATimedOutProbeStopsAtItsTimeout(t *testing.T) {
	defer func(g time.Duration) { ghTimeout = g }(ghTimeout)
	ghTimeout = time.Second
	d := divergence{scenario: scenario{ctx: ctxDefault, gh: `case "$1 $2" in "auth status") exit 0 ;; "pr view") exit 1 ;; "pr list") sleep 30 & sleep 30 ;; esac`}}
	start := time.Now()
	_, out, _, _ := d.run(t)
	if el := time.Since(start); el > 10*time.Second {
		t.Errorf("took %v; the 1s timeout did not hold", el)
	}
	want := `could not confirm PR history (Command '['gh', 'pr', 'list', '--state', 'all', '--limit', '1', '--json', 'number']' timed out after 1 seconds)`
	if !strings.Contains(out, want) {
		t.Errorf("want %s in:\n%s", want, out)
	}
}

// A probe that exits while a grandchild still holds its pipes is answered by what it printed and
// its exit status, a second later. WaitDelay's own error used to stand in for the answer, so a
// gh whose credential helper lingered read as no PR, unauthenticated, or no history.
func TestAProbeWithALingeringGrandchildIsAnswered(t *testing.T) {
	d := divergence{scenario: scenario{ctx: ctxDefault, gh: `case "$1 $2" in "auth status") (sleep 5) & exit 0 ;; "pr view") echo 17; (sleep 5) & ;; "pr list") echo '[{"number":1}]'; (sleep 5) & ;; esac`}}
	start := time.Now()
	_, out, _, _ := d.run(t)
	if el := time.Since(start); el > 4500*time.Millisecond {
		t.Errorf("took %v; each probe waited out its grandchild", el)
	}
	for _, want := range []string{`"a PR exists to comment on"`, `"gh authenticated, github remote present, PR history confirmed"`} {
		if !strings.Contains(out, want) {
			t.Errorf("want %s in:\n%s", want, out)
		}
	}
}

// -h prints the usage to stdout and exits 0, as argparse did; its text is Go's flag package's.
func TestHelpGoesToStdout(t *testing.T) {
	var o, e bytes.Buffer
	if code := Command([]string{"-h"}, strings.NewReader(""), &o, &e); code != 0 || e.Len() != 0 ||
		!strings.Contains(o.String(), "-attacker-reachable") {
		t.Errorf("exit %d, stdout %q, stderr %q", code, o.String(), e.String())
	}
}
