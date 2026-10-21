package plan

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pinepeakdigital/looper/internal/golden"
	"github.com/pinepeakdigital/looper/internal/record"
	"github.com/pinepeakdigital/looper/internal/report"
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
	stderr []string
}{
	// context.sh is gone: the sentences name the command that writes the context now.
	{"context not JSON", divergence{scenario: scenario{ctx: "{not json"}}, []string{"context is not valid JSON — run looper context first"}},
	{"context trailing data", divergence{scenario: scenario{ctx: ctxDefault + " {}"}}, []string{"context is not valid JSON — run looper context first"}},
	{"context file missing", divergence{scenario: scenario{ctx: ctxDefault, args: with("--context", "/nonexistent/ctx.json")}}, []string{"cannot read the context file (", "Run looper context first and point --context at what it wrote"}},
	{"no model", divergence{scenario: scenario{ctx: ctxDefault}, raw: append(with(), "--dry-run")}, []string{"--model"}},
	{"no answers", divergence{scenario: scenario{ctx: ctxDefault, args: []string{"--logic", "yes"}}}, []string{"--behavioral-goal, --runtime-change, --attacker-reachable"}},
	{"unknown flag", divergence{scenario: scenario{ctx: ctxDefault, args: with("--frobnicate", "yes")}}, []string{"frobnicate"}},
	// The tier is the plan's to name, never the caller's: there is no flag for it.
	{"a tier flag", divergence{scenario: scenario{ctx: ctxDefault, args: with("--tier", "fast")}}, []string{"-tier"}},
	{"positional argument", divergence{scenario: scenario{ctx: ctxDefault, args: append(with(), "extra")}}, []string{"extra"}},
}

func TestUsageAndInputErrorsExitAsThePythonDid(t *testing.T) {
	for _, c := range usageAndInput {
		t.Run(c.name, func(t *testing.T) {
			py := golden.Read(t, "transcript")
			// The frozen answer must be plan.py's own refusal: comparing exit codes alone passed
			// a golden that recorded python3 failing to open the script, which also exits 2.
			if !strings.Contains(py, "\nplan.py: error: ") && !strings.Contains(py, "\nplan: ") {
				t.Fatalf("the frozen oracle is not plan.py refusing:\n%s", py)
			}
			code, out, errOut, w := c.d.run(t)
			got := w.result(t, code, out, errOut)
			named := true
			for _, want := range c.stderr {
				named = named && strings.Contains(errOut, want)
			}
			if pyExit(t, got) != pyExit(t, py) || out != "" || !named {
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
	{"context is a list", `[1, 2]`, "not a JSON object — run looper context first"},
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

// A failed `gh auth status` is reported as what was observed, and called unauthenticated only
// when gh says it is logged out (TestPlanMatchesThePython's "gh unauthenticated"). plan.py said
// "gh not authenticated here" for every failure of the call, and in a sandbox it fails on the
// keyring or TLS while auth is fine (friction log, 2026-10-02), so a run recorded a false
// reason for skipping Agent #10. The skip itself stands.
//
// The goldens are plan.py's answer to an auth status that exits 1, copied from the
// "gh unauthenticated" golden rather than captured: plan.py discarded both streams of this
// call, so its answer did not depend on what gh printed, and it is gone.
func TestAFailedAuthProbeSaysWhatItSaw(t *testing.T) {
	for _, c := range []struct{ name, auth, want string }{
		// The sandbox's own output, gh 2.102.0: a host header line, then the X line.
		{"the sandbox keyring failure",
			`printf 'github.com\n  X Failed to log in to github.com account narthur (keyring)\n  - The token in keyring is invalid.\n' >&2; exit 1`,
			`"gh auth status failed (Failed to log in to github.com account narthur (keyring)) without saying gh is logged out`},
		{"a silent failure", `exit 1`, `"gh auth status failed (exit 1) without saying gh is logged out`},
	} {
		t.Run(c.name, func(t *testing.T) {
			py := golden.Read(t, "transcript")
			if !strings.Contains(py, `"gh not authenticated here (retry unsandboxed before accepting this)"`) {
				t.Fatalf("the frozen golden no longer records plan.py calling it unauthenticated:\n%s", py)
			}
			d := divergence{scenario: scenario{ctx: ctxDefault,
				gh: `case "$1 $2" in "auth status") ` + c.auth + ` ;; *) exit 1 ;; esac`}}
			_, out, _, _ := d.run(t)
			if !strings.Contains(out, c.want) || strings.Contains(out, "not authenticated") {
				t.Errorf("want %s, and no claim of being unauthenticated, in:\n%s", c.want, out)
			}
		})
	}
}

// -h prints the usage to stdout and exits 0, as argparse did; its text is Go's flag package's,
// and --context names the command that writes the file, not the retired context.sh.
func TestHelpGoesToStdout(t *testing.T) {
	var o, e bytes.Buffer
	if code := Command([]string{"-h"}, strings.NewReader(""), &o, &e); code != 0 || e.Len() != 0 ||
		!strings.Contains(o.String(), "-attacker-reachable") || !strings.Contains(o.String(), "-delta-logic") ||
		!strings.Contains(o.String(), "looper context") || strings.Contains(o.String(), "context.sh") {
		t.Errorf("exit %d, stdout %q, stderr %q", code, o.String(), e.String())
	}
}

// A follow-up after a clean exit is floored on the delta since the last reviewed sha when
// `looper context` says it is fast-path sized and the orchestrator answers --delta-logic no
// (SKILL.md's fast-path re-entry). --logic stays the whole branch's answer: the risk label
// reads it, and a branch with reviewed logic in it is the case the re-entry is for. plan.py
// predates the re-entry and never read the field, so it floored a small follow-up on a big
// branch at full, and `record finish` then refused the fast tier the skill prescribes.
//
// These goldens were captured by hand from looper before the change, which matched plan.py on
// every frozen scenario and, like plan.py, ignored context keys it did not name. Without
// --delta-logic the plan must be that golden byte for byte. With it, the golden is looper's
// own refusal of an unknown flag, in Go's words; plan.py's argparse refused it with exit 2 too.
const reentryFields = `, "last_reviewed_sha": "abc1234", "review_delta_lines": 12`

const reentryReason = `"tier_reason": "fast-path re-entry: 12 raw lines since the last reviewed sha abc1234 (fast path is <30) and no program logic in them; the branch has 40 lines of review surface"`

var reEntry = []struct {
	name string
	d    divergence
	// With --delta-logic: looper's floor, and text that must be in looper's transcript.
	floor string
	want  []string
}{
	{"a logic-free delta on a branch with logic is fast",
		divergence{scenario: scenario{ctx: ctx(`{"fast_path_eligible_by_delta": true` + reentryFields + `}`), args: with("--delta-logic", "no")}},
		"fast", []string{reentryReason, `"logic": true`, `"delta_logic": false`}},
	{"a logic-free delta on a logic-free branch is fast",
		divergence{scenario: scenario{ctx: ctx(`{"fast_path_eligible_by_delta": true` + reentryFields + `}`), args: with("--logic", "no", "--delta-logic", "no")}},
		"fast", []string{reentryReason}},
	{"a recorded re-entry floors the row at fast and records the answer",
		divergence{scenario: scenario{ctx: ctx(`{"fast_path_eligible_by_delta": true` + reentryFields + `}`), args: with("--delta-logic", "no"), record: true}},
		"fast", []string{"tier_floor=\"fast\"\n", `"delta_logic":false`, `"logic":true`}},
	{"a delta with logic in it is full",
		divergence{scenario: scenario{ctx: ctx(`{"fast_path_eligible_by_delta": true` + reentryFields + `}`), args: with("--logic", "no", "--delta-logic", "yes")}},
		"full", []string{`"tier_reason": "40 lines of review surface (fast path is <30)"`, `"delta_logic": true`}},
	{"a delta not fast-path sized is full",
		divergence{scenario: scenario{ctx: ctx(`{"fast_path_eligible_by_delta": false, "last_reviewed_sha": "abc1234", "review_delta_lines": 30}`), args: with("--logic", "no", "--delta-logic", "no")}},
		"full", []string{`"tier_reason": "40 lines of review surface (fast path is <30)"`}},
	{"no reviewed sha is full",
		divergence{scenario: scenario{ctx: ctx(`{"fast_path_eligible_by_delta": false, "last_reviewed_sha": null, "review_delta_lines": 0}`), args: with("--logic", "no", "--delta-logic", "no")}},
		"full", []string{`"tier_reason": "40 lines of review surface (fast path is <30)"`}},
	// looper context never says true without a sha, but a context that does must not floor
	// a delta measured against nothing.
	{"a delta verdict with no reviewed sha is full",
		divergence{scenario: scenario{ctx: ctx(`{"fast_path_eligible_by_delta": true, "last_reviewed_sha": null, "review_delta_lines": 12}`), args: with("--logic", "no", "--delta-logic", "no")}},
		"full", []string{`"tier_reason": "40 lines of review surface (fast path is <30)"`}},
	// An empty sha names no baseline any more than a null one: context's eligibility is
	// bool(last_reviewed), so a hand-written "" must not floor at fast either.
	{"a delta verdict with an empty reviewed sha is full",
		divergence{scenario: scenario{ctx: ctx(`{"fast_path_eligible_by_delta": true, "last_reviewed_sha": "", "review_delta_lines": 12}`), args: with("--logic", "no", "--delta-logic", "no")}},
		"full", []string{`"tier_reason": "40 lines of review surface (fast path is <30)"`}},
	{"a null delta verdict is full",
		divergence{scenario: scenario{ctx: ctx(`{"fast_path_eligible_by_delta": null` + reentryFields + `}`), args: with("--logic", "no", "--delta-logic", "no")}},
		"full", []string{`"tier_reason": "40 lines of review surface (fast path is <30)"`}},
	// Fast-path sized branch-wide as well: plan.py's size reason stands.
	{"a small branch keeps the size reason",
		divergence{scenario: scenario{ctx: ctx(`{"fast_path_eligible_by_size": true, "changed_lines": 12, "semantic_lines": 12, "fast_path_eligible_by_delta": true` + reentryFields + `}`), args: with("--logic", "no", "--delta-logic", "no")}},
		"fast", []string{`"tier_reason": "12 lines of review surface and no program logic touched"`}},
	// Unasked, nothing is lowered: an orchestrator that predates --delta-logic plans as before.
	{"without a delta-logic answer a small delta is full",
		divergence{scenario: scenario{ctx: ctx(`{"fast_path_eligible_by_delta": true` + reentryFields + `}`), args: with("--logic", "no")}}, "", nil},
	{"without a delta-logic answer a branch with logic is full",
		divergence{scenario: scenario{ctx: ctx(`{"fast_path_eligible_by_delta": true` + reentryFields + `}`)}}, "", nil},
}

func TestAReEntryIsSizedAgainstTheDelta(t *testing.T) {
	for _, c := range reEntry {
		t.Run(c.name, func(t *testing.T) {
			py := golden.Read(t, "transcript")
			code, out, errOut, w := c.d.run(t)
			got := w.result(t, code, out, errOut)
			if c.floor == "" {
				if got != py {
					t.Errorf("--- python (frozen) ---\n%s\n--- looper ---\n%s", py, got)
				}
				return
			}
			// The record of what is being diverged from: plan.py had no such flag. The golden is
			// looper's own refusal from before the change, not plan.py's argparse text.
			if !strings.HasPrefix(py, "exit=2\n") || !strings.Contains(py, "delta-logic") {
				t.Fatalf("the frozen golden no longer records the old looper refusing --delta-logic:\n%s", py)
			}
			want := append([]string{"exit=0\n", `"tier_floor": "` + c.floor + `"`}, c.want...)
			for _, s := range want {
				if !strings.Contains(got, s) {
					t.Errorf("want %s in:\n%s", s, got)
				}
			}
		})
	}
}

// --delta-logic lowers the floor on "no", so an answer that is neither yes nor no is a usage
// error rather than plan.py's reading of it as "no": a typo must not floor a re-entry at fast.
func TestAGarbledDeltaAnswerIsAUsageError(t *testing.T) {
	for _, v := range []string{"yse", "maybe", ""} {
		t.Run(fmt.Sprintf("%q", v), func(t *testing.T) {
			d := divergence{scenario: scenario{ctx: ctx(`{"fast_path_eligible_by_delta": true` + reentryFields + `}`),
				args: with("--delta-logic", v)}}
			code, out, errOut, _ := d.run(t)
			if code != 2 || out != "" || !strings.Contains(errOut, "-delta-logic") {
				t.Errorf("exit %d, stdout %q, stderr %q; want exit 2, no plan, stderr naming -delta-logic", code, out, errOut)
			}
		})
	}
}

// Strict is not narrower than documented: every spelling the README promises still answers.
func TestEveryDocumentedDeltaAnswerIsRead(t *testing.T) {
	for v, want := range map[string][]string{
		"y": {`"tier_floor": "full"`, `"delta_logic": true`}, "true": {`"tier_floor": "full"`, `"delta_logic": true`},
		"1": {`"tier_floor": "full"`, `"delta_logic": true`}, "YES": {`"tier_floor": "full"`, `"delta_logic": true`},
		"n": {`"tier_floor": "fast"`, `"delta_logic": false`}, "false": {`"tier_floor": "fast"`, `"delta_logic": false`},
		"0": {`"tier_floor": "fast"`, `"delta_logic": false`}, "NO": {`"tier_floor": "fast"`, `"delta_logic": false`},
	} {
		t.Run(v, func(t *testing.T) {
			d := divergence{scenario: scenario{ctx: ctx(`{"fast_path_eligible_by_delta": true` + reentryFields + `}`),
				args: with("--delta-logic", v)}}
			code, out, errOut, _ := d.run(t)
			if code != 0 {
				t.Fatalf("--delta-logic %s: exit %d, stderr %q", v, code, errOut)
			}
			for _, s := range want {
				if !strings.Contains(out, s) {
					t.Errorf("--delta-logic %s: want %s in:\n%s", v, s, out)
				}
			}
		})
	}
}

// The delta answer is recorded for the run record's sake only: the risk label is the branch's
// and reads --logic, so a logic-free follow-up on a branch with logic stays risk:medium.
// Answering --logic about the delta is what flipped skills#11 to risk:low.
func TestTheDeltaAnswerLeavesTheRiskAlone(t *testing.T) {
	d := divergence{scenario: scenario{ctx: ctx(`{"fast_path_eligible_by_delta": true` + reentryFields + `}`),
		args: with("--runtime-change", "no", "--delta-logic", "no"), record: true}}
	code, _, errOut, w := d.run(t)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	runs, err := record.Load(w.store, 0)
	if err != nil {
		t.Fatal(err)
	}
	var p *record.Plan
	for _, r := range runs {
		if r.Plan != nil {
			p = r.Plan
		}
	}
	if p == nil || p.TierFloor != "fast" {
		t.Fatalf("no fast plan row in %+v", runs)
	}
	if got := report.Risk(p); got != "medium" {
		t.Errorf("risk %q with --logic yes and --delta-logic no; want medium", got)
	}
}

// The re-entry fields are type-checked as every other field is. plan.py never read them, so
// any value planned as if the field were absent.
var badReEntry = []struct{ name, ctx, stderr string }{
	{"the delta verdict is a number", ctx(`{"fast_path_eligible_by_delta": 1}`), "fast_path_eligible_by_delta is 1, not true or false"},
	{"the delta is a string", ctx(`{"review_delta_lines": "12"}`), "review_delta_lines is \"12\", not a whole number"},
	{"the reviewed sha is a number", ctx(`{"last_reviewed_sha": 7}`), "last_reviewed_sha is 7, not a string"},
}

func TestAWrongReEntryFieldIsAnError(t *testing.T) {
	for _, c := range badReEntry {
		t.Run(c.name, func(t *testing.T) {
			if py := golden.Read(t, "transcript"); !strings.HasPrefix(py, "exit=0\n") {
				t.Fatalf("the frozen oracle no longer records the field being ignored:\n%s", py)
			}
			code, out, errOut, _ := divergence{scenario: scenario{ctx: c.ctx}}.run(t)
			if code != 1 || out != "" || !strings.Contains(errOut, c.stderr) {
				t.Errorf("exit %d, stdout %q, stderr %q; want exit 1, no stdout, stderr naming %q", code, out, errOut, c.stderr)
			}
		})
	}
}
