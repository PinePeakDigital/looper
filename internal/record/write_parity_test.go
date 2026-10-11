package record

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// The write half's parity gate: every scenario runs through runlog.py and through Command,
// each against its own copy of the same seeded store, in the same git repo, and must leave the
// same exit status, the same stdout and stderr, and the same records behind. Timestamps and
// generated run ids are the only things normalised away, because they are the only things two
// correct implementations cannot agree on.
//
// A scenario is a SEQUENCE, not one call: most of what the write half guards is a relationship
// between calls — a cycle before its plan, a cycle after its finish, an abandon over a finish.

type wstep struct {
	args []string
	env  map[string]string // "" sets the variable blank; unsetVar removes it
}

const unsetVar = "\x00unset"

type wscenario struct {
	name   string
	seed   []string // store lines; {REPO} is replaced with the scenario repo's identity
	branch string   // checked out before the steps; "" stays on main
	detach bool
	files  []string // created under <git-common-dir>/info/
	steps  []wstep
}

func st(args ...string) wstep { return wstep{args: args} }

func stEnv(env map[string]string, args ...string) wstep { return wstep{args: args, env: env} }

const okGates = `{"agent_7":{"planned":"run","reason":"big"},"security":{"planned":"run","reason":"every cycle"},"agent_11":{"planned":"skip","reason":"no spec"}}`

var writeScenarios = []wscenario{
	{name: "plan then show", steps: []wstep{
		st("plan", "--tier", "full", "--model", "m", "--gates", okGates, "--run-id", "r1",
			"--semantic-lines", "12", "--changed-lines", "20", "--sizing-excluded", "a lockfile",
			"--tier-reason", "logic touched", "--base", "main", "--inputs", `{"logic":true}`),
		st("show", "--run-id", "r1"),
	}},
	{name: "plan generates an id and reads head", steps: []wstep{
		st("plan", "--tier", "fast", "--model", "m"),
		st("check"),
	}},
	{name: "plan refuses a malformed gate", steps: []wstep{
		st("plan", "--tier", "full", "--model", "m", "--gates", `{"z":"run","a":{"planned":"maybe","reason":"x"},"ok":{"planned":"run","reason":"y"}}`),
	}},
	{name: "plan refuses an unexplained gate", steps: []wstep{
		st("plan", "--tier", "full", "--model", "m", "--gates", `{"b":{"planned":"run","reason":"  "},"a":{"planned":"skip"}}`),
	}},
	{name: "plan refuses precedent in a skip reason, wrapped", steps: []wstep{
		st("plan", "--tier", "full", "--model", "m", "--gates", `{"a":{"planned":"skip","reason":"it Matches  an\nexisting\tpattern"}}`),
	}},
	{name: "plan refuses precedent in the tier reason and sizing", steps: []wstep{
		st("plan", "--tier", "fast", "--model", "m", "--tier-reason", "done this way elsewhere"),
		st("plan", "--tier", "fast", "--model", "m", "--sizing-excluded", "same as what main has"),
	}},
	{name: "plan refuses a non-positive cap", steps: []wstep{
		st("plan", "--tier", "full", "--model", "m", "--agent-cap", "0"),
		st("plan", "--tier", "full", "--model", "m", "--agent-cap", "-3"),
	}},
	{name: "plan with falsy JSON arguments", steps: []wstep{
		st("plan", "--tier", "full", "--model", "m", "--gates", "null", "--inputs", "[]", "--run-id", "f1"),
		st("show", "--run-id", "f1"),
	}},
	{name: "session kind and id from the environment", steps: []wstep{
		stEnv(map[string]string{"AO_SESSION_ID": "ao1"}, "plan", "--tier", "fast", "--model", "m", "--run-id", "s1"),
		stEnv(map[string]string{"CLAUDE_REVIEW_LOOP_SUBAGENT": "1"}, "plan", "--tier", "fast", "--model", "m", "--run-id", "s2"),
		stEnv(map[string]string{"CLAUDE_CODE_SESSION_ATTENDED": "1", "CLAUDE_CODE_SESSION_ID": "cc"}, "plan", "--tier", "fast", "--model", "m", "--run-id", "s3"),
		stEnv(map[string]string{"CLAUDE_CODE_SESSION_ATTENDED": "false"}, "plan", "--tier", "fast", "--model", "m", "--run-id", "s4"),
		stEnv(map[string]string{"CLAUDE_CODE_SESSION_ATTENDED": ""}, "plan", "--tier", "fast", "--model", "m", "--run-id", "s5"),
		stEnv(map[string]string{"AO_SESSION_ID": ""}, "plan", "--tier", "fast", "--model", "m", "--run-id", "s6"),
		st("plan", "--tier", "fast", "--model", "m", "--run-id", "s7", "--session-kind", "subagent"),
	}},
	{name: "a cycle needs a plan and a non-negative count", steps: []wstep{
		st("cycle", "--run-id", "ghost", "--n", "1", "--applied", "0", "--agents", "1"),
		st("plan", "--tier", "full", "--model", "m", "--run-id", "c1"),
		st("cycle", "--run-id", "c1", "--n", "1", "--applied", "-1", "--agents", "1"),
		st("cycle", "--run-id", "c1", "--n", "1", "--applied", "1", "--agents", "1", "--tokens", "-5"),
		st("cycle", "--run-id", "c1", "--n", "1", "--applied", "2", "--agents", "4", "--asked", "1",
			"--defect-findings", "2", "--comment-findings", "1", "--analysis-changed", "--tokens", "900",
			"--width-reason", "semantic_lines=40"),
		st("cycle", "--run-id", "c1", "--n", "2", "--applied", "0", "--agents", "1", "--width-reason", "mirrors the existing loop"),
		st("show", "--run-id", "c1"),
	}},
	{name: "converged, then no cycle after finish", steps: []wstep{
		st("plan", "--tier", "full", "--model", "m", "--gates", okGates, "--run-id", "k1"),
		st("cycle", "--run-id", "k1", "--n", "1", "--applied", "3", "--agents", "5"),
		st("cycle", "--run-id", "k1", "--n", "2", "--applied", "0", "--agents", "1"),
		st("convergence", "--run-id", "k1"),
		st("finish", "--run-id", "k1", "--outcome", "clean", "--tier", "full",
			"--executed", `{"agent_7":{"status":"done"},"security":{"status":"n/a","reason":"no surface"}}`,
			"--agents", `[{"id":"a","model":"opus","status":"done"},{"id":"b","status":"ok"}]`,
			"--findings", `{"auto_fix":3,"asked":0,"skipped":0}`),
		st("cycle", "--run-id", "k1", "--n", "3", "--applied", "0", "--agents", "1"),
		st("abandon", "--run-id", "k1", "--missing", "nothing"),
		st("convergence", "--run-id", "k1"),
		st("check"),
	}},
	{name: "finish refuses unaccounted and unexplained gates", steps: []wstep{
		st("plan", "--tier", "full", "--model", "m", "--gates", okGates, "--run-id", "u1"),
		st("finish", "--run-id", "u1", "--outcome", "clean", "--tier", "full", "--executed", `{"agent_7":{"status":"done"}}`),
		st("finish", "--run-id", "u1", "--outcome", "clean", "--tier", "full", "--executed", `{"security":"done","agent_7":{"status":"skipped"}}`),
		st("finish", "--run-id", "u1", "--outcome", "clean", "--tier", "full", "--executed", `{"agent_7":{"status":"done"}}`, "--allow-unaccounted"),
		st("show", "--run-id", "u1"),
	}},
	{name: "finish derives partial from gates and agents, in order", steps: []wstep{
		st("plan", "--tier", "full", "--model", "m", "--gates", okGates, "--run-id", "p1"),
		st("cycle", "--run-id", "p1", "--n", "1", "--applied", "0", "--agents", "2"),
		st("finish", "--run-id", "p1", "--outcome", "clean", "--tier", "full",
			"--executed", `{"security":{"status":"failed","reason":"tool crashed"},"agent_7":{"status":"skipped","reason":"tiny"},"agent_11":{"status":"skipped","reason":"planned skip"}}`,
			"--agents", `[{"model":"haiku","status":"timeout"},{"id":"seven","status":"failed"},{"id":"x","status":"done"}]`),
		st("show", "--run-id", "p1"),
	}},
	{name: "finish waived and n/a do not force partial", steps: []wstep{
		st("plan", "--tier", "full", "--model", "m", "--gates", okGates, "--run-id", "w1"),
		st("cycle", "--run-id", "w1", "--n", "1", "--applied", "0", "--agents", "1"),
		st("finish", "--run-id", "w1", "--outcome", "clean", "--tier", "full",
			"--executed", `{"agent_7":{"status":"waived","reason":"repo cannot measure"},"security":{"status":"n/a","reason":"none"}}`),
	}},
	{name: "finish refuses a tier below the floor", steps: []wstep{
		st("plan", "--tier", "full", "--model", "m", "--run-id", "t1"),
		st("finish", "--run-id", "t1", "--outcome", "clean", "--tier", "fast"),
		st("finish", "--run-id", "t1", "--outcome", "clean"),
		st("finish", "--run-id", "t1", "--outcome", "blocked", "--tier", "partial"),
	}},
	{name: "finish with an escalation, and one citing precedent", steps: []wstep{
		st("plan", "--tier", "full", "--model", "m", "--gates", okGates, "--run-id", "e1"),
		st("finish", "--run-id", "e1", "--outcome", "clean", "--tier", "full",
			"--executed", `{"agent_7":{"status":"done"}}`, "--escalations", `[{"gate":"security","reason":"precedent says so"}]`),
		st("finish", "--run-id", "e1", "--outcome", "clean", "--tier", "full",
			"--executed", `{"agent_7":{"status":"done"}}`, "--escalations", `[{"gate":"security","reason":"ran wider"}]`),
	}},
	{name: "asks at finish make it halted", steps: []wstep{
		st("plan", "--tier", "fast", "--model", "m", "--run-id", "a1"),
		st("cycle", "--run-id", "a1", "--n", "1", "--applied", "0", "--agents", "1"),
		st("finish", "--run-id", "a1", "--outcome", "clean", "--tier", "fast", "--asks", "7"),
		st("convergence", "--run-id", "a1"),
	}},
	{name: "capped", steps: []wstep{
		st("plan", "--tier", "fast", "--model", "m", "--run-id", "cap", "--agent-cap", "3"),
		st("cycle", "--run-id", "cap", "--n", "1", "--applied", "2", "--agents", "3"),
		st("convergence", "--run-id", "cap"),
		st("convergence", "--run-id", "nobody"),
		st("finish", "--run-id", "cap", "--outcome", "cycle-limit", "--tier", "fast"),
	}},
	{name: "finish with no plan and no cycles", steps: []wstep{
		st("finish", "--run-id", "orphan", "--outcome", "test-failure", "--tier", "fast"),
		st("show", "--run-id", "orphan"),
	}},
	{name: "skipped, carried, nudge, abandon", steps: []wstep{
		st("skipped", "--reason", "a one-word typo in a comment", "--model", "sonnet"),
		st("skipped", "--reason", "already done this way"),
		st("carried", "--from", "1234567890abcdef1234", "--to", "fedcba0987654321fedc", "--how", "skipped", "--by", "patch-id --verbatim"),
		st("plan", "--tier", "full", "--model", "m", "--gates", okGates, "--run-id", "n1"),
		st("nudge", "--run-id", "n1"),
		st("check"),
		st("check", "--force"),
		st("abandon", "--run-id", "n1", "--missing", "   "),
		st("abandon", "--run-id", "n1", "--missing", "agent_7, security: the session ended"),
		st("abandon", "--run-id", "n1", "--missing", "again"),
		st("check"),
	}},
	{name: "check reports every open run, newest first, in this repo only", seed: []string{
		`{"run_id":"old","phase":"plan","planned_at":"2026-01-01T00:00:00+00:00","repo":"{REPO}","head":"h1","tier_floor":"full","gates":{"b":{"planned":"run","reason":"x"},"a":{"planned":"run","reason":"y"}},"session_id":"s1"}`,
		`{"run_id":"elsewhere","phase":"plan","planned_at":"2026-06-01T00:00:00+00:00","repo":"/some/other/repo","head":"h1"}`,
		`{"run_id":"new","phase":"plan","planned_at":"2026-05-01T00:00:00+00:00","repo":"{REPO}","head":"h2","tier_floor":"fast","session_id":"s2"}`,
		`{"run_id":"anon","phase":"plan","planned_at":"2026-03-01T00:00:00+00:00","repo":"{REPO}","head":"h2"}`,
		`not json at all`,
		`{"run_id":"done","phase":"plan","planned_at":"2026-07-01T00:00:00+00:00","repo":"{REPO}","head":"h2"}`,
		`{"run_id":"done","phase":"finish","finished_at":"2026-07-01T00:00:00+00:00","outcome":"clean"}`,
		`{"run_id":"done","phase":"nudge","nudged_at":"2026-07-02T00:00:00+00:00"}`,
	}, steps: []wstep{
		st("check"),
		st("check", "--head", "h2"),
		st("check", "--session", "s1"),
		st("check", "--run-id", "anon"),
		stEnv(map[string]string{"REVIEW_LOOP_TAIL": "3"}, "check"),
		stEnv(map[string]string{"REVIEW_LOOP_TAIL": "nonsense"}, "check", "--head", "h1"),
		st("show", "--run-id", "done"),
		st("show", "--run-id", "missing"),
	}},
	{name: "pending lists this branch's files only", branch: "feat", seed: []string{
		`{"run_id":"mine","phase":"plan","branch":"feat"}`,
		`{"run_id":"theirs","phase":"plan","branch":"main"}`,
	}, files: []string{"review-loop-pending-report.mine.md", "review-loop-pending-report.theirs.md", "review-loop-pending-report.ghost.md"},
		steps: []wstep{st("pending")}},
	{name: "pending on a detached head", detach: true, seed: []string{`{"run_id":"mine","phase":"plan","branch":"HEAD"}`},
		files: []string{"review-loop-pending-report.mine.md"}, steps: []wstep{st("pending")}},
	{name: "usage errors exit 2", steps: []wstep{
		st("plan", "--model", "m"),
		st("plan", "--tier", "huge", "--model", "m"),
		st("finish", "--run-id", "x", "--outcome", "carried"),
		st("cycle", "--run-id", "x", "--n", "one", "--applied", "0", "--agents", "1"),
		st("carried", "--from", "a", "--to", "b", "--how", "maybe"),
		st("abandon", "--run-id", "x"),
		st("nudge"),
	}},
	{name: "invalid JSON arguments", steps: []wstep{
		st("plan", "--tier", "full", "--model", "m", "--gates", "{nope"),
		st("plan", "--tier", "full", "--model", "m", "--run-id", "j1"),
		st("finish", "--run-id", "j1", "--outcome", "clean", "--tier", "full", "--agents", "[1,"),
	}},
}

var managedEnv = []string{"AO_SESSION_ID", "CLAUDE_REVIEW_LOOP_SUBAGENT", "CLAUDE_CODE_SESSION_ATTENDED",
	"CLAUDE_CODE_SESSION_ID", "REVIEW_LOOP_TAIL"}

func scenarioRepo(t *testing.T, sc wscenario) (repo, common string) {
	t.Helper()
	repo = t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "core.hooksPath=/dev/null", "-c", "user.email=t@t", "-c", "user.name=t"}, args...)...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q", "-b", "main")
	run("commit", "-q", "--allow-empty", "-m", "seed")
	if sc.branch != "" {
		run("checkout", "-q", "-b", sc.branch)
	}
	if sc.detach {
		run("checkout", "-q", "--detach")
	}
	out, err := exec.Command("git", "-C", repo, "rev-parse", "--path-format=absolute", "--git-common-dir").Output()
	if err != nil {
		t.Fatal(err)
	}
	common = strings.TrimSpace(string(out))
	for _, f := range sc.files {
		if err := os.WriteFile(filepath.Join(common, "info", f), []byte("report\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return repo, common
}

type transcript struct {
	steps   []string
	records []string
}

var (
	tsRE   = regexp.MustCompile(`\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d[+-]\d\d:\d\d`)
	ridRE  = regexp.MustCompile(`\b[0-9a-f]{12}\b`)
	jsonRE = regexp.MustCompile(`is not valid JSON: .*`)
)

// canon replaces every timestamp and every generated 12-hex id, numbering ids by first
// appearance so that the same id reads the same everywhere in one transcript.
func canon(s string, ids map[string]string) string {
	s = tsRE.ReplaceAllString(s, "<ts>")
	s = jsonRE.ReplaceAllString(s, "is not valid JSON: <err>")
	// The oracle names its own file in one message; the port names the subcommand.
	s = strings.ReplaceAll(s, "`runlog.py cycle`", "`looper record cycle`")
	return ridRE.ReplaceAllStringFunc(s, func(m string) string {
		if _, ok := ids[m]; !ok {
			ids[m] = "<rid" + string(rune('A'+len(ids))) + ">"
		}
		return ids[m]
	})
}

func setEnv(t *testing.T, env map[string]string) {
	for _, k := range managedEnv {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	for k, v := range env {
		if v == unsetVar {
			os.Unsetenv(k)
			continue
		}
		os.Setenv(k, v)
	}
}

func runScenario(t *testing.T, sc wscenario, repo, store string, python string) transcript {
	t.Helper()
	ids := map[string]string{}
	var tr transcript
	for _, stp := range sc.steps {
		setEnv(t, stp.env)
		t.Setenv("REVIEW_LOOP_RUNS", store)
		var out, errb bytes.Buffer
		code := 0
		if python != "" {
			cmd := exec.Command("python3", append([]string{python}, stp.args...)...)
			cmd.Dir = repo
			cmd.Stdout, cmd.Stderr = &out, &errb
			if err := cmd.Run(); err != nil {
				ee, ok := err.(*exec.ExitError)
				if !ok {
					t.Fatalf("python: %v", err)
				}
				code = ee.ExitCode()
			}
		} else {
			code = Command(stp.args, &out, &errb)
		}
		errText := errb.String()
		if code == 2 {
			errText = "<usage>" // argparse's and flag's usage texts are not the contract; the status is
		}
		tr.steps = append(tr.steps, canon(strings.Join(stp.args, " ")+"\n  exit="+strconv.Itoa(code)+
			"\n  out="+out.String()+"\n  err="+errText, ids))
	}
	b, _ := os.ReadFile(store)
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var rec map[string]any
		if json.Unmarshal([]byte(line), &rec) != nil {
			tr.records = append(tr.records, canon(line, ids))
			continue
		}
		c, _ := json.Marshal(rec) // decoded and re-encoded: key order and \u escapes are not the contract
		tr.records = append(tr.records, canon(string(c), ids))
	}
	return tr
}

func TestWriteMatchesThePython(t *testing.T) {
	script := runlogPath(t)
	for _, sc := range writeScenarios {
		t.Run(sc.name, func(t *testing.T) {
			repo, _ := scenarioRepo(t, sc)
			t.Chdir(repo)
			id := repoID()
			dir := t.TempDir()
			stores := [2]string{filepath.Join(dir, "py.jsonl"), filepath.Join(dir, "go.jsonl")}
			for _, s := range stores {
				var seed string
				for _, l := range sc.seed {
					seed += strings.ReplaceAll(l, "{REPO}", id) + "\n"
				}
				if seed != "" {
					if err := os.WriteFile(s, []byte(seed), 0o600); err != nil {
						t.Fatal(err)
					}
				}
			}
			py := runScenario(t, sc, repo, stores[0], script)
			goT := runScenario(t, sc, repo, stores[1], "")
			for i := range py.steps {
				if py.steps[i] != goT.steps[i] {
					t.Errorf("step %d differs\n--- runlog.py ---\n%s\n--- looper record ---\n%s", i+1, py.steps[i], goT.steps[i])
				}
			}
			if strings.Join(py.records, "\n") != strings.Join(goT.records, "\n") {
				t.Errorf("the stores differ\n--- runlog.py ---\n%s\n--- looper record ---\n%s",
					strings.Join(py.records, "\n"), strings.Join(goT.records, "\n"))
			}
		})
	}
}

// The one recorded divergence. runlog.py joins the broken-agent ids with ', '.join(), so an
// agent whose id is a number raises TypeError there and the run is never finished at all — a
// crash on the path that exists to make an unfinished run visible. The port stringifies the id
// and records the run as partial. Measured against the oracle: Traceback, exit 1, no row.
func TestANumericAgentIDIsRecordedNotFatal(t *testing.T) {
	repo, _ := scenarioRepo(t, wscenario{})
	t.Chdir(repo)
	setEnv(t, nil)
	t.Setenv("REVIEW_LOOP_RUNS", filepath.Join(t.TempDir(), "runs.jsonl"))
	var out, errb bytes.Buffer
	if c := Command([]string{"plan", "--tier", "fast", "--model", "m", "--run-id", "n7"}, &out, &errb); c != 0 {
		t.Fatalf("plan: %d %s", c, errb.String())
	}
	errb.Reset()
	c := Command([]string{"finish", "--run-id", "n7", "--outcome", "clean", "--tier", "fast",
		"--agents", `[{"id":7,"status":"failed"}]`}, &out, &errb)
	if c != 0 || !strings.Contains(errb.String(), "did not complete: 7") {
		t.Errorf("a numeric agent id: exit %d, stderr %q", c, errb.String())
	}
}
