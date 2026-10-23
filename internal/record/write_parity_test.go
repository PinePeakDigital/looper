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
	"time"

	"github.com/pinepeakdigital/looper/internal/golden"
)

// The write half's parity gate: every scenario ran through runlog.py, and runs through Command,
// against a copy of the same seeded store in a fresh git repo, and must leave the same exit
// status, the same stdout and stderr, and the same records behind. runlog.py's side is frozen
// under testdata/oracle, captured before its CLI was deleted. Timestamps, generated run ids and
// the temp paths and commit of each run are normalised away, because two correct runs cannot
// agree on them.
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
	// Halted, not converged, so the guard's purpose is visible: without it the 0/0 cycle row
	// below flips the finished run to converged. record-cycle-after-finish-accepted.mut selects
	// this scenario by its full name.
	{name: "halted, not converged, then no cycle after finish", steps: []wstep{
		st("plan", "--tier", "full", "--model", "m", "--gates", okGates, "--run-id", "k1"),
		st("cycle", "--run-id", "k1", "--n", "1", "--applied", "3", "--agents", "5"),
		st("convergence", "--run-id", "k1"),
		st("finish", "--run-id", "k1", "--outcome", "cycle-limit", "--tier", "full",
			"--executed", `{"agent_7":{"status":"done"},"security":{"status":"n/a","reason":"no surface"}}`,
			"--agents", `[{"id":"a","model":"opus","status":"done"},{"id":"b","status":"ok"}]`,
			"--findings", `{"auto_fix":3,"asked":0,"skipped":0}`),
		st("cycle", "--run-id", "k1", "--n", "2", "--applied", "0", "--agents", "0"),
		st("convergence", "--run-id", "k1"),
		st("abandon", "--run-id", "k1", "--missing", "nothing"),
		st("check"),
	}},
	{name: "converged, and clean", steps: []wstep{
		st("plan", "--tier", "full", "--model", "m", "--run-id", "k2"),
		st("cycle", "--run-id", "k2", "--n", "1", "--applied", "3", "--agents", "5"),
		st("cycle", "--run-id", "k2", "--n", "2", "--applied", "0", "--agents", "1"),
		st("convergence", "--run-id", "k2"),
		st("finish", "--run-id", "k2", "--outcome", "clean", "--tier", "full"),
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
		st("finish", "--run-id", "a1", "--outcome", "cycle-limit", "--tier", "fast", "--asks", "7"),
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
	{name: "pending lists the files of this branch only", branch: "feat", seed: []string{
		`{"run_id":"mine","phase":"plan","branch":"feat"}`,
		`{"run_id":"theirs","phase":"plan","branch":"main"}`,
	}, files: []string{"review-loop-pending-report.mine.md", "review-loop-pending-report.theirs.md", "review-loop-pending-report.ghost.md"},
		steps: []wstep{st("pending")}},
	{name: "pending on a detached head", detach: true, seed: []string{`{"run_id":"mine","phase":"plan","branch":"HEAD"}`},
		files: []string{"review-loop-pending-report.mine.md"}, steps: []wstep{st("pending")}},
	{name: "usage errors exit 2", steps: []wstep{
		st("plan", "--model", "m"),
		st("plan", "--tier", "full", "--model", "m", "extra"),
		st("plan", "--tier", "huge", "--model", "m"),
		st("finish", "--run-id", "x", "--outcome", "carried"),
		st("cycle", "--run-id", "x", "--n", "one", "--applied", "0", "--agents", "1"),
		st("carried", "--from", "a", "--to", "b", "--how", "maybe"),
		st("abandon", "--run-id", "x"),
		st("nudge"),
	}},
	{name: "store lines are split and stripped the way Python reads them", seed: []string{
		`{"run_id":"r4","phase":"plan","planned_at":"2026-01-01T00:00:00+00:00","repo":"{REPO}","head":"h"}`,
		"{\"run_id\":\"r4\",\"phase\":\"finish\",\"finished_at\":\"2026-01-01T00:00:00+00:00\",\"outcome\":\"clean\"}\x1c",
		"{\"run_id\":\"cr\",\"phase\":\"plan\",\"repo\":\"{REPO}\",\"head\":\"h\"}\r{\"run_id\":\"cr\",\"phase\":\"finish\",\"outcome\":\"clean\"}",
		"{\"run_id\":\"cv\",\"phase\":\"plan\",\"repo\":\"{REPO}\",\"head\":\"h\"}\r{\"run_id\":\"cv\",\"phase\":\"cycle\",\"n\":1,\"applied\":0,\"agents\":1}",
		`{"run_id":"fs","phase":"plan","repo":"{REPO}","head":"h"}`,
		"{\"run_id\":\"fs\",\"phase\":\"cycle\",\"n\":1,\"applied\":0,\"agents\":1}\x1c",
	}, steps: []wstep{
		st("abandon", "--run-id", "r4", "--missing", "x"),
		st("show", "--run-id", "cr"),
		st("convergence", "--run-id", "cv"),
		st("convergence", "--run-id", "fs"),
		st("check"),
	}},
	{name: "an unexplained gate is refused before a numeric escalations", steps: []wstep{
		st("plan", "--tier", "fast", "--model", "m", "--run-id", "ue"),
		st("finish", "--run-id", "ue", "--outcome", "clean", "--tier", "fast",
			"--executed", `{"g1":{"status":"failed"}}`, "--escalations", "5"),
	}},
	{name: "a non-list agents is recorded as given", steps: []wstep{
		st("plan", "--tier", "fast", "--model", "m", "--gates", `{"g":{"planned":"run","reason":"x"}}`, "--run-id", "nl"),
		st("finish", "--run-id", "nl", "--outcome", "clean", "--tier", "fast", "--executed", `{"g":{"status":"done"}}`,
			"--agents", `{"cycle1":6,"scorers":"batched haiku"}`),
		st("show", "--run-id", "nl"),
	}},
	{name: "precedent in every reason field", steps: []wstep{
		st("plan", "--tier", "full", "--model", "m", "--gates", okGates, "--run-id", "v1"),
		st("finish", "--run-id", "v1", "--outcome", "clean", "--tier", "full",
			"--executed", `{"agent_7":{"status":"skipped","reason":"matches an existing pattern"},"security":{"status":"done"}}`),
		st("finish", "--run-id", "v1", "--outcome", "clean", "--tier", "full",
			"--executed", `{"agent_7":{"status":"failed","reason":"follows the existing pattern"},"security":{"status":"done"}}`),
		st("finish", "--run-id", "v1", "--outcome", "clean", "--tier", "full",
			"--executed", `{"agent_7":{"status":"n/a","reason":"same pattern as auth"},"security":{"status":"done"}}`),
		st("finish", "--run-id", "v1", "--outcome", "clean", "--tier", "full",
			"--executed", `{"agent_7":{"status":"waived","reason":"identical to what main has"},"security":{"status":"done"}}`),
		st("abandon", "--run-id", "v1", "--missing", "consistent with existing code"),
		// passed is report prose, not an --executed status, and anything unenumerated goes loud.
		st("finish", "--run-id", "v1", "--outcome", "clean", "--tier", "full",
			"--executed", `{"agent_7":{"status":"passed","reason":"metric plan posted"},"security":{"status":"probably fine","reason":"2 stale claims re-read"}}`),
		st("show", "--run-id", "v1"),
	}},
	{name: "floor, above is allowed, broken outranks below", steps: []wstep{
		st("plan", "--tier", "fast", "--model", "m", "--run-id", "f1"),
		st("finish", "--run-id", "f1", "--outcome", "clean", "--tier", "full"),
		// A broken gate is recorded partial before the floor is consulted, so a claim below the
		// floor is not refused here.
		st("plan", "--tier", "full", "--model", "m", "--gates", okGates, "--run-id", "f2"),
		st("finish", "--run-id", "f2", "--outcome", "clean", "--tier", "fast",
			"--executed", `{"agent_7":{"status":"failed","reason":"timed out"},"security":{"status":"done"}}`),
		st("plan", "--tier", "fast", "--model", "m", "--run-id", "f3"),
		st("finish", "--run-id", "f3", "--outcome", "clean", "--tier", "skipped"),
	}},
	{name: "every count is refused negative", steps: []wstep{
		st("plan", "--tier", "full", "--model", "m", "--run-id", "g1"),
		st("cycle", "--run-id", "g1", "--n", "-1", "--applied", "0", "--agents", "1"),
		st("cycle", "--run-id", "g1", "--n", "1", "--applied", "0", "--asked", "-1", "--agents", "1"),
		st("cycle", "--run-id", "g1", "--n", "1", "--applied", "1", "--agents", "-100"),
		// No cycle rows: unknown, and not exit 0.
		st("convergence", "--run-id", "g1"),
	}},
	{name: "an all-skip plan does not force partial", steps: []wstep{
		st("plan", "--tier", "full", "--model", "m", "--gates", `{"t":{"planned":"skip","reason":"12 entries"}}`, "--run-id", "z1"),
		st("finish", "--run-id", "z1", "--outcome", "clean", "--tier", "full",
			"--executed", `{"t":{"status":"skipped","reason":"the plan already skipped it"}}`),
	}},
	{name: "lookups by id read past the tail", seed: []string{
		`{"run_id":"rt","phase":"plan","planned_at":"2026-01-01T00:00:00+00:00","repo":"{REPO}","head":"h","tier_floor":"full","gates":{"t":{"planned":"run","reason":"2 stale"}}}`,
		`{"run_id":"x1","phase":"nudge"}`, `{"run_id":"x2","phase":"nudge"}`, `{"run_id":"x3","phase":"nudge"}`,
	}, steps: []wstep{
		// check is bounded by the tail; the lookups of one run are not.
		stEnv(tail2, "check"),
		stEnv(map[string]string{"REVIEW_LOOP_TAIL": "-5"}, "check"),
		stEnv(tail2, "finish", "--run-id", "rt", "--outcome", "clean", "--tier", "full", "--executed", `{}`),
		stEnv(tail2, "cycle", "--run-id", "rt", "--n", "1", "--applied", "0", "--agents", "1"),
		stEnv(tail2, "convergence", "--run-id", "rt"),
		stEnv(tail2, "show", "--run-id", "rt"),
	}},
	{name: "session precedence, the check session from the environment, and ties", seed: []string{
		// One planned_at for all three: check's sort must keep store order on a tie, which is
		// the ordinary case for two plans written in the same second.
		`{"run_id":"o1","phase":"plan","planned_at":"2026-01-01T00:00:00+00:00","repo":"{REPO}","head":"h","session_id":"s1"}`,
		`{"run_id":"o2","phase":"plan","planned_at":"2026-01-01T00:00:00+00:00","repo":"{REPO}","head":"h","session_id":"s2"}`,
		`{"run_id":"o3","phase":"plan","planned_at":"2026-01-01T00:00:00+00:00","repo":"{REPO}","head":"h"}`,
	}, steps: []wstep{
		stEnv(map[string]string{"AO_SESSION_ID": "abc", "CLAUDE_CODE_SESSION_ATTENDED": "1"},
			"plan", "--tier", "fast", "--model", "m", "--run-id", "k1", "--head", "h9"),
		stEnv(map[string]string{"CLAUDE_REVIEW_LOOP_SUBAGENT": "1", "CLAUDE_CODE_SESSION_ATTENDED": "1"},
			"plan", "--tier", "fast", "--model", "m", "--run-id", "k2", "--head", "h9"),
		stEnv(map[string]string{"CLAUDE_CODE_SESSION_ATTENDED": "0"},
			"plan", "--tier", "fast", "--model", "m", "--run-id", "k3", "--head", "h9"),
		stEnv(map[string]string{"CLAUDE_CODE_SESSION_ID": "s1"}, "check", "--head", "h"),
		stEnv(map[string]string{"AO_SESSION_ID": "s2"}, "check", "--head", "h"),
		stEnv(map[string]string{"CLAUDE_CODE_SESSION_ID": "s1"}, "check", "--head", "h", "--session", "s2"),
		st("check", "--head", "h"),
	}},
	// Not hex, so the printed prefixes are compared as written: a from/to swap would otherwise
	// canonicalise to the same pair of placeholders.
	{name: "carried prints from then to", steps: []wstep{
		st("carried", "--from", "from-sha-0000000", "--to", "to-sha-11111111", "--how", "reviewed"),
	}},
	{name: "invalid JSON arguments", steps: []wstep{
		st("plan", "--tier", "full", "--model", "m", "--gates", "{nope"),
		st("plan", "--tier", "full", "--model", "m", "--run-id", "j1"),
		st("finish", "--run-id", "j1", "--outcome", "clean", "--tier", "full", "--agents", "[1,"),
	}},
}

var tail2 = map[string]string{"REVIEW_LOOP_TAIL": "2"}

// bannedPhraseScenario refuses every phrase in the port's list, uppercased and with every space
// widened to space-newline-tab, so case folding AND whitespace folding are exercised for every
// phrase. TestWriteConstantsMatchThePython holds the list equal to runlog.py's, which stays a
// library, so a phrase added on either side reaches this golden through that test's failure.
func bannedPhraseScenario() wscenario {
	sc := wscenario{name: "every banned phrase, from the oracle list"}
	for _, p := range BannedReason {
		sc.steps = append(sc.steps, st("skipped", "--reason", "12 lines; "+strings.ToUpper(strings.ReplaceAll(p, " ", " \n\t"))+" now"))
	}
	return sc
}

var managedEnv = []string{"AO_SESSION_ID", "CLAUDE_REVIEW_LOOP_SUBAGENT", "CLAUDE_CODE_SESSION_ATTENDED",
	"CLAUDE_CODE_SESSION_ID", "REVIEW_LOOP_TAIL"}

func scenarioRepo(t *testing.T, sc wscenario) (repo, common string) {
	t.Helper()
	t.Setenv("REVIEW_LOOP_HOME", t.TempDir()) // this scenario's run dirs, and no other's
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
	// Handed out through a symlink, so the working directory and git's resolved answer differ
	// on every OS and not only on macOS, whose temp dir happens to sit under one. Without it a
	// repoID that returned the working directory passed on Linux CI.
	link := filepath.Join(t.TempDir(), "repo")
	if err := os.Symlink(repo, link); err != nil {
		t.Fatal(err)
	}
	return link, common
}

type transcript struct {
	steps   []string
	records []string
}

var (
	// The offset is kept: two writers to one store must agree on it, because check sorts
	// planned_at as a string.
	tsRE   = regexp.MustCompile(`\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d([+-]\d\d:\d\d)`)
	ridRE  = regexp.MustCompile(`\b[0-9a-f]{12}\b`)
	jsonRE = regexp.MustCompile(`is not valid JSON: .*`)
	// argparse's and the port's usage errors. Their texts are not the contract (argparse's
	// wording even varies by Python version); the status is. Anything else on exit 2 —
	// `convergence` on an unknown run — is compared as written.
	usageRE = regexp.MustCompile(`(?m)^usage: |^Usage of |: the following arguments are required: |: argument -|: error: | for flag -|: unrecognized arguments: `)
)

// canon replaces every timestamp, and every id the implementation GENERATED, numbering ids by
// first appearance so that the same id reads the same everywhere in one transcript. A 12-hex
// value a scenario supplied is not an id to normalise, so it is compared as written.
func canon(s string, ids map[string]string) string {
	s = tsRE.ReplaceAllString(s, "<ts>$1")
	s = jsonRE.ReplaceAllString(s, "is not valid JSON: <err>")
	// The oracle names its own file in one message; the port names the subcommand.
	s = strings.ReplaceAll(s, "`runlog.py cycle`", "`looper record cycle`")
	return ridRE.ReplaceAllStringFunc(s, func(m string) string {
		if v, ok := ids[m]; ok {
			return v
		}
		return m
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

func runScenario(t *testing.T, sc wscenario, store string) transcript {
	t.Helper()
	var steps, recs []string
	generated := map[string]bool{}
	supplied := map[string]bool{}
	for _, stp := range sc.steps {
		for _, a := range stp.args {
			supplied[a] = true
		}
		setEnv(t, stp.env)
		t.Setenv("REVIEW_LOOP_RUNS", store)
		var out, errb bytes.Buffer
		code := Command(stp.args, &out, &errb)
		// plan (without --run-id) and skipped print the id they generated.
		if code == 0 && (stp.args[0] == "plan" || stp.args[0] == "skipped") {
			if id := strings.TrimSpace(out.String()); ridRE.MatchString(id) && len(id) == 12 {
				generated[id] = true
			}
		}
		errText := errb.String()
		if code == 2 && usageRE.MatchString(errText) {
			errText = "<usage>"
		}
		steps = append(steps, strings.Join(stp.args, " ")+"\n  exit="+strconv.Itoa(code)+
			"\n  out="+out.String()+"\n  err="+errText)
	}
	b, _ := os.ReadFile(store)
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var rec map[string]any
		if json.Unmarshal([]byte(line), &rec) != nil {
			recs = append(recs, line)
			continue
		}
		// carried generates an id it never prints; the store is the only place it shows.
		if id, ok := rec["run_id"].(string); ok && len(id) == 12 && ridRE.MatchString(id) && !supplied[id] {
			generated[id] = true
		}
		c, _ := json.Marshal(rec) // decoded and re-encoded: key order and \u escapes are not the contract
		recs = append(recs, string(c))
	}
	// Number the generated ids by first appearance across the whole transcript, steps first.
	ids := map[string]string{}
	for _, text := range append(append([]string{}, steps...), recs...) {
		for _, m := range ridRE.FindAllString(text, -1) {
			if _, seen := ids[m]; generated[m] && !seen {
				ids[m] = "<rid" + string(rune('A'+len(ids))) + ">"
			}
		}
	}
	var tr transcript
	for _, s := range steps {
		tr.steps = append(tr.steps, canon(s, ids))
	}
	for _, r := range recs {
		tr.records = append(tr.records, canon(r, ids))
	}
	return tr
}

// frozen is a transcript with everything that differs between two runs of one scenario replaced:
// the temp repo, its git dir, its commit, and the store's path. What is left is what
// runlog.py printed and wrote, and what the port must still print and write.
func frozen(tr transcript, repo, common, head, store string) string {
	text := strings.Join(tr.steps, "\n") + "\n--- store ---\n" + strings.Join(tr.records, "\n") + "\n"
	// The repo as git resolves it: on macOS t.TempDir() is under /var, a symlink to
	// /private/var, and git records the resolved form, so replacing only the unresolved path
	// left "/private<repo>" in the goldens and a Linux runner, which has no such link, failed.
	// The unresolved path keeps a placeholder of its own, so a port that reported the working
	// directory instead of git's answer still differs from the golden wherever the two differ.
	resolved, _ := filepath.EvalSymlinks(repo)
	// Pending reports are in the run dirs since narthur/skills#16; runlog.py kept them under
	// <common>/info, and the goldens were edited to <run> with that change.
	for _, r := range []struct{ from, to string }{{filepath.Join(os.Getenv("REVIEW_LOOP_HOME"), "run"), "<run>"}, {common, "<common>"}, {store, "<store>"}, {filepath.Dir(store), "<dir>"}, {resolved, "<repo>"}, {repo, "<repo-raw>"}, {head, "<head>"}} {
		text = strings.ReplaceAll(text, r.from, r.to)
	}
	return text
}

// The write half against what runlog.py printed and stored for each scenario, frozen before its
// CLI was deleted (internal/golden says what a golden is and is not). The zone is fixed because
// the goldens were captured at -05:00 and the port stamps in local time: under a real zone
// they would break at every DST change and on every machine outside it.
func TestWriteMatchesThePython(t *testing.T) {
	local := time.Local
	time.Local = time.FixedZone("capture", -5*3600)
	t.Cleanup(func() { time.Local = local })
	for _, sc := range append(append([]wscenario{}, writeScenarios...), bannedPhraseScenario()) {
		t.Run(sc.name, func(t *testing.T) {
			want := golden.Read(t, "transcript") // before the Chdir: its path is relative to this package
			repo, common := scenarioRepo(t, sc)
			t.Chdir(repo)
			id := repoID()
			head := git("rev-parse", "HEAD")
			dir := t.TempDir()
			store := filepath.Join(dir, "runs.jsonl")
			var seed string
			for _, l := range sc.seed {
				seed += strings.ReplaceAll(l, "{REPO}", id) + "\n"
			}
			if seed != "" {
				if err := os.WriteFile(store, []byte(seed), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			got := frozen(runScenario(t, sc, store), repo, common, head, store)
			if got != want {
				t.Errorf("looper record no longer does what runlog.py did\n--- runlog.py (frozen) ---\n%s\n--- looper record ---\n%s", want, got)
			}
		})
	}
}

// The first recorded divergence. runlog.py joins the broken-agent ids with ', '.join(), so an
// agent whose id is a number raises TypeError there and the run is never finished at all — a
// crash on the path that exists to make an unfinished run visible. The port stringifies the id
// and records the run as partial. Measured against the oracle: Traceback, exit 1, no row.
func TestANumericAgentIDIsRecordedNotFatal(t *testing.T) {
	repo, _ := scenarioRepo(t, wscenario{})
	t.Chdir(repo)
	setEnv(t, nil)
	store := filepath.Join(t.TempDir(), "runs.jsonl")
	t.Setenv("REVIEW_LOOP_RUNS", store)
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
	// Recorded, not merely survived: the finish row is there, partial, with the id as given.
	b, _ := os.ReadFile(store)
	var fin map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var rec map[string]any
		if json.Unmarshal([]byte(line), &rec) == nil && rec["phase"] == "finish" && rec["run_id"] == "n7" {
			fin = rec
		}
	}
	if fin == nil {
		t.Fatalf("no finish row for n7 in the store:\n%s", b)
	}
	agents, _ := fin["agents"].([]any)
	var id any
	if len(agents) == 1 {
		if a, ok := agents[0].(map[string]any); ok {
			id = a["id"]
		}
	}
	if fin["tier_executed"] != "partial" || id != float64(7) {
		t.Errorf("finish row: tier_executed %v, agents %v — want partial and id 7", fin["tier_executed"], fin["agents"])
	}
}

// The constants the port copies, compared directly. The scenarios exercise three of the
// precedent phrases; the other eighteen could drift with every scenario still green. And
// runlog.py outlives its CLI as a library — learn.py vets dismissal text with its
// reject_banned and plan.py reads its DEFAULT_AGENT_CAP — so the two lists must stay one list
// for as long as both exist.
func TestWriteConstantsMatchThePython(t *testing.T) {
	script := runlogPath(t)
	out, err := exec.Command("python3", "-c", `import json,sys; sys.path.insert(0, sys.argv[1]); import runlog
print(json.dumps({"banned": list(runlog.BANNED_REASON), "cap": runlog.DEFAULT_AGENT_CAP,
  "outcomes": list(runlog.OUTCOMES), "tiers": list(runlog.TIERS),
  "subcommand_states": list(runlog.SUBCOMMAND_STATES), "accounted": list(runlog.GATE_ACCOUNTED),
  "rank": runlog.TIER_RANK}))`, filepath.Dir(script)).Output()
	if err != nil {
		t.Fatalf("importing runlog: %v", err)
	}
	var py struct {
		Banned           []string       `json:"banned"`
		Outcomes         []string       `json:"outcomes"`
		Tiers            []string       `json:"tiers"`
		SubcommandStates []string       `json:"subcommand_states"`
		Accounted        []string       `json:"accounted"`
		Cap              int            `json:"cap"`
		Rank             map[string]int `json:"rank"`
	}
	if err := json.Unmarshal(out, &py); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name     string
		py, port any
	}{
		{"BANNED_REASON", py.Banned, BannedReason},
		{"DEFAULT_AGENT_CAP", py.Cap, DefaultAgentCap},
		// asks-open is a deliberate addition with no runlog.py counterpart (issue #41),
		// asserted by TestAsksOpenNeedsAsks; the rest of the list must still match.
		{"OUTCOMES", py.Outcomes, without(Outcomes, AsksOpen)},
		{"TIERS", py.Tiers, Tiers},
		{"SUBCOMMAND_STATES", py.SubcommandStates, SubcommandStates},
		{"GATE_ACCOUNTED", py.Accounted, GateAccounted},
		{"TIER_RANK", py.Rank, tierRank},
	} {
		a, _ := json.Marshal(c.py)
		b, _ := json.Marshal(c.port)
		if string(a) != string(b) {
			t.Errorf("%s drifted\n  runlog.py: %s\n  port:      %s", c.name, a, b)
		}
	}
}

// The second recorded divergence. A reason that is not a string reached the precedent ban
// stringified, and a list's newlines came back as the two characters "\\n", which split
// nothing: ["matches an\nexisting pattern"] as an escalation reason was recorded. runlog.py
// dies at .lower() with AttributeError, which records nothing but explains nothing; the port
// refuses with a reason. Escalations are the one reason field no earlier check sees.
func TestANonStringReasonIsRefused(t *testing.T) {
	repo, _ := scenarioRepo(t, wscenario{})
	t.Chdir(repo)
	setEnv(t, nil)
	store := filepath.Join(t.TempDir(), "runs.jsonl")
	t.Setenv("REVIEW_LOOP_RUNS", store)
	var out, errb bytes.Buffer
	if c := Command([]string{"plan", "--tier", "fast", "--model", "m", "--run-id", "ns",
		"--gates", `{"g":{"planned":"run","reason":"x"}}`}, &out, &errb); c != 0 {
		t.Fatalf("plan: %d %s", c, errb.String())
	}
	for _, reason := range []string{`["matches an\nexisting pattern"]`, `7`} {
		errb.Reset()
		c := Command([]string{"finish", "--run-id", "ns", "--outcome", "clean", "--tier", "fast",
			"--executed", `{"g":{"status":"done"}}`,
			"--escalations", `[{"gate":"g","reason":` + reason + `}]`}, &out, &errb)
		if c != 1 || !strings.Contains(errb.String(), "its reason is not a string") {
			t.Errorf("escalation reason %s: exit %d, stderr %q", reason, c, errb.String())
		}
	}
	if b, _ := os.ReadFile(store); strings.Count(string(b), "\n") != 1 {
		t.Errorf("a refused finish wrote a row:\n%s", b)
	}
}

// The third recorded divergence, and the guard that replaced it at the writer. runlog.py
// derived a verdict from the dict whatever the typed reader could decode; the port first
// recorded such a finish and reported unknown, and a recorded row of that kind turned out to be
// unrepairable — the run's decode error outlives any later row, and a finish is final. So
// `finish` now decodes its own row the way every reader will and refuses it, writing nothing.
// Where the unreadable row is an EARLIER one (a plan written by hand, or by an older build),
// finish and convergence still report unknown and say why.
func TestAnUnreadableFinishSaysWhy(t *testing.T) {
	repo, _ := scenarioRepo(t, wscenario{})
	t.Chdir(repo)
	setEnv(t, nil)
	store := filepath.Join(t.TempDir(), "runs.jsonl")
	t.Setenv("REVIEW_LOOP_RUNS", store)
	var out, errb bytes.Buffer
	for _, args := range [][]string{
		{"plan", "--tier", "fast", "--model", "m", "--run-id", "ur", "--gates", `{"g":{"planned":"run","reason":"x"}}`},
		{"cycle", "--run-id", "ur", "--n", "1", "--applied", "0", "--agents", "1"},
	} {
		if c := Command(args, &out, &errb); c != 0 {
			t.Fatalf("%v: %d %s", args, c, errb.String())
		}
	}
	before, _ := os.ReadFile(store)
	out.Reset()
	errb.Reset()
	// A numeric reason on a `done` gate: no earlier check reads it (the precedent ban looks
	// only at gates that did not finish), and GateResult.Reason is a string.
	c := Command([]string{"finish", "--run-id", "ur", "--outcome", "clean", "--tier", "fast",
		"--executed", `{"g":{"status":"done","reason":5}}`}, &out, &errb)
	if c != 1 || !strings.Contains(errb.String(), "would not decode") || !strings.Contains(errb.String(), "GateResult") {
		t.Errorf("a finish row no reader can decode was not refused with the decode error: exit %d\nout %q\nerr %q",
			c, out.String(), errb.String())
	}
	if after, _ := os.ReadFile(store); !bytes.Equal(before, after) {
		t.Errorf("a refused finish wrote to the store:\n%s", after)
	}

	// An unreadable earlier row: the warning path, which a finish can no longer reach by itself.
	if err := os.WriteFile(store, []byte(
		`{"run_id":"up","phase":"plan","repo":"r","head":"h","gates":{"g":{"planned":"run","reason":7}}}`+"\n"+
			`{"run_id":"up","phase":"cycle","n":1,"applied":0,"agents":1}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	errb.Reset()
	if c := Command([]string{"finish", "--run-id", "up", "--outcome", "clean", "--tier", "fast",
		"--executed", `{"g":{"status":"done"}}`}, &out, &errb); c != 0 {
		t.Fatalf("finish over an unreadable plan row: %d %s", c, errb.String())
	}
	if !strings.Contains(out.String(), "convergence: unknown") || !strings.Contains(errb.String(), "cannot be read") {
		t.Errorf("an unreadable run was not diagnosed at finish:\nout %q\nerr %q", out.String(), errb.String())
	}
	out.Reset()
	errb.Reset()
	if c := Command([]string{"convergence", "--run-id", "up"}, &out, &errb); c != 1 || !strings.Contains(errb.String(), "cannot be read") {
		t.Errorf("convergence on an unreadable run: exit %d, stderr %q", c, errb.String())
	}
}

// --escalations must be a list of {"gate": string, "reason": string}. runlog.py recorded any
// truthy JSON, and an object or a string became a finish row no typed reader could decode,
// which left the run unknown to pr-report and push-check for good (friction log, 2026-10-21).
// Each case pins its OWN message: a guard removed here is otherwise still caught by the
// decode check after it, and refused with an error that does not say what shape to send.
func TestEscalationsMustBeAListOfGateAndReason(t *testing.T) {
	repo, _ := scenarioRepo(t, wscenario{})
	t.Chdir(repo)
	setEnv(t, nil)
	store := filepath.Join(t.TempDir(), "runs.jsonl")
	t.Setenv("REVIEW_LOOP_RUNS", store)
	var out, errb bytes.Buffer
	if c := Command([]string{"plan", "--tier", "fast", "--model", "m", "--run-id", "es"}, &out, &errb); c != 0 {
		t.Fatalf("plan: %d %s", c, errb.String())
	}
	for _, c := range []struct{ name, esc, want string }{
		{"an object", `{"security":"ran wider"}`, "must be a JSON list of objects, " + escalationShape + " — got an object"},
		{"a string", `"security"`, "must be a JSON list of objects, " + escalationShape + " — got a string"},
		{"a list of strings", `["security"]`, "element 0 is a string"},
		{"a null element", `[{"gate":"a","reason":"b"},null]`, "element 1 is null"},
		{"no gate", `[{"reason":"ran wider"}]`, "escalation 0 names no gate"},
		{"a numeric gate", `[{"gate":7,"reason":"ran wider"}]`, "escalation 0 names no gate"},
		{"a blank gate", `[{"gate":"  ","reason":"ran wider"}]`, "escalation 0 names no gate"},
		{"no reason", `[{"gate":"security"}]`, "escalation on 'security' gives no reason"},
		{"a blank reason", `[{"gate":"security","reason":" "}]`, "escalation on 'security' gives no reason"},
		{"a numeric reason", `[{"gate":"security","reason":3}]`, "escalation on 'security' — its reason is not a string"},
	} {
		errb.Reset()
		code := Command([]string{"finish", "--run-id", "es", "--outcome", "clean", "--tier", "fast",
			"--escalations", c.esc}, &out, &errb)
		if code != 1 || !strings.Contains(errb.String(), c.want) {
			t.Errorf("escalations as %s (%s): exit %d, stderr %q, want it to contain %q", c.name, c.esc, code, errb.String(), c.want)
		}
	}
	if b, _ := os.ReadFile(store); strings.Count(string(b), "\n") != 1 {
		t.Errorf("a refused finish wrote a row:\n%s", b)
	}
	// The shape it asks for is accepted.
	errb.Reset()
	if c := Command([]string{"finish", "--run-id", "es", "--outcome", "clean", "--tier", "fast",
		"--escalations", `[{"gate":"security","reason":"ran wider"}]`}, &out, &errb); c != 0 {
		t.Errorf("a well-formed escalation was refused: exit %d, stderr %q", c, errb.String())
	}
}

// `clean` with asks still with the user is refused. The record's own derivation reads those
// asks as halted, so the row would contradict itself; before this it was written with a
// warning (friction log, 2026-10-21: a finish claimed clean with 2 unresolved asks).
func TestCleanWithUnresolvedAsksIsRefused(t *testing.T) {
	repo, _ := scenarioRepo(t, wscenario{})
	t.Chdir(repo)
	setEnv(t, nil)
	store := filepath.Join(t.TempDir(), "runs.jsonl")
	t.Setenv("REVIEW_LOOP_RUNS", store)
	var out, errb bytes.Buffer
	for _, id := range []string{"ca", "cb"} {
		if c := Command([]string{"plan", "--tier", "fast", "--model", "m", "--run-id", id}, &out, &errb); c != 0 {
			t.Fatalf("plan: %d %s", c, errb.String())
		}
	}
	before, _ := os.ReadFile(store)
	if c := Command([]string{"finish", "--run-id", "ca", "--outcome", "clean", "--tier", "fast", "--asks", "2"}, &out, &errb); c != 1 ||
		!strings.Contains(errb.String(), "refusing to finish as 'clean' with 2 unresolved asks") {
		t.Errorf("clean with 2 unresolved asks was not refused: exit %d, stderr %q", c, errb.String())
	}
	if after, _ := os.ReadFile(store); !bytes.Equal(before, after) {
		t.Errorf("a refused finish wrote to the store:\n%s", after)
	}
	// A negative count is not a count: refused under any outcome, nothing written.
	errb.Reset()
	before, _ = os.ReadFile(store)
	if c := Command([]string{"finish", "--run-id", "ca", "--outcome", "clean", "--tier", "fast", "--asks", "-1"}, &out, &errb); c != 1 ||
		!strings.Contains(errb.String(), "--asks cannot be negative (got -1)") {
		t.Errorf("a negative --asks was not refused: exit %d, stderr %q", c, errb.String())
	}
	if after, _ := os.ReadFile(store); !bytes.Equal(before, after) {
		t.Errorf("a finish with a negative --asks wrote to the store:\n%s", after)
	}
	// Narrow: the same asks under another outcome, and clean with none, are both recorded.
	errb.Reset()
	if c := Command([]string{"finish", "--run-id", "ca", "--outcome", "cycle-limit", "--tier", "fast", "--asks", "2"}, &out, &errb); c != 0 {
		t.Errorf("cycle-limit with asks was refused: exit %d, stderr %q", c, errb.String())
	}
	errb.Reset()
	if c := Command([]string{"finish", "--run-id", "cb", "--outcome", "clean", "--tier", "fast", "--asks", "0"}, &out, &errb); c != 0 {
		t.Errorf("clean with no asks was refused: exit %d, stderr %q", c, errb.String())
	}
}

// A finish is final, as it already was for cycle and abandon: a second finish for the same run
// is refused and writes nothing. The outcome first recorded may already be on a PR and may have
// cleared a push. record-cycle-after-finish-accepted.mut records why that matters for cycles.
func TestASecondFinishIsRefused(t *testing.T) {
	repo, _ := scenarioRepo(t, wscenario{})
	t.Chdir(repo)
	setEnv(t, nil)
	store := filepath.Join(t.TempDir(), "runs.jsonl")
	t.Setenv("REVIEW_LOOP_RUNS", store)
	var out, errb bytes.Buffer
	for _, args := range [][]string{
		{"plan", "--tier", "fast", "--model", "m", "--run-id", "df"},
		{"finish", "--run-id", "df", "--outcome", "cycle-limit", "--tier", "fast"},
	} {
		if c := Command(args, &out, &errb); c != 0 {
			t.Fatalf("%v: %d %s", args, c, errb.String())
		}
	}
	before, _ := os.ReadFile(store)
	errb.Reset()
	if c := Command([]string{"finish", "--run-id", "df", "--outcome", "clean", "--tier", "fast"}, &out, &errb); c != 1 ||
		!strings.Contains(errb.String(), "run 'df' is already finished as 'cycle-limit' — a finish is final") {
		t.Errorf("a second finish was not refused: exit %d, stderr %q", c, errb.String())
	}
	if after, _ := os.ReadFile(store); !bytes.Equal(before, after) {
		t.Errorf("a refused finish wrote to the store:\n%s", after)
	}
}

// `pending --all` lists every pending file with its run's branch, from any checkout — main
// included — so Step 0c can flush a report deferred on a branch it has since left. The
// default output is unchanged: TestWriteMatchesThePython's two pending scenarios pin it.
func TestPendingAllListsEveryBranch(t *testing.T) {
	sc := wscenario{files: []string{
		"review-loop-pending-report.mine.md", "review-loop-pending-report.theirs.md",
		"review-loop-pending-report.ghost.md", "review-loop-pending-report.loose.md",
		"review-loop-pending-report.det.md", "review-loop-pending-report.other.md",
	}}
	repo, common := scenarioRepo(t, sc) // on main
	t.Chdir(repo)
	setEnv(t, nil)
	store := filepath.Join(t.TempDir(), "runs.jsonl")
	t.Setenv("REVIEW_LOOP_RUNS", store)
	if err := os.WriteFile(store, []byte(strings.Join([]string{
		`{"run_id":"mine","phase":"plan","branch":"feat"}`,
		`{"run_id":"theirs","phase":"plan","branch":"main"}`,
		`{"run_id":"loose","phase":"plan"}`,
		`{"run_id":"det","phase":"plan","branch":"HEAD"}`,
		// Another repo's run: run dirs are shared, so it is neither listed nor named here.
		`{"run_id":"other","phase":"plan","branch":"feat","repo":"/elsewhere"}`,
	}, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run := filepath.Join(os.Getenv("REVIEW_LOOP_HOME"), "run")
	var out, errb bytes.Buffer
	if c := Command([]string{"pending", "--all"}, &out, &errb); c != 0 {
		t.Fatalf("pending --all: exit %d, stderr %q", c, errb.String())
	}
	// The files were left at the old place, <common>/info, and are listed from their run dirs.
	want := filepath.Join(run, "mine", "pending-report.md") + "\tfeat\n" +
		filepath.Join(run, "theirs", "pending-report.md") + "\tmain\n"
	if out.String() != want {
		t.Errorf("pending --all stdout:\n%q\nwant\n%q", out.String(), want)
	}
	if left, _ := filepath.Glob(filepath.Join(common, "info", "review-loop-pending-report.*")); len(left) != 0 {
		t.Errorf("not moved out of the old place: %q", left)
	}
	if strings.Contains(errb.String(), "other") {
		t.Errorf("another repo's report was named: %q", errb.String())
	}
	for _, f := range []string{"ghost", "loose", "det"} {
		if !strings.Contains(errb.String(), filepath.Join(run, f, "pending-report.md")+" — left in place") {
			t.Errorf("an unroutable file %s was not named on stderr: %q", f, errb.String())
		}
	}
}

// Plain `pending` is the list Step 0c flushes onto THIS branch's PR, so another repo's run on
// a branch of the same name must not be on it: run dirs are shared by every repo.
func TestPendingSkipsAnotherReposRunOnTheSameBranch(t *testing.T) {
	sc := wscenario{files: []string{"review-loop-pending-report.mine.md", "review-loop-pending-report.other.md"}}
	repo, _ := scenarioRepo(t, sc)
	t.Chdir(repo)
	if out, err := exec.Command("git", "checkout", "-qb", "feat").CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	setEnv(t, nil)
	store := filepath.Join(t.TempDir(), "runs.jsonl")
	t.Setenv("REVIEW_LOOP_RUNS", store)
	if err := os.WriteFile(store, []byte(`{"run_id":"mine","phase":"plan","branch":"feat"}`+"\n"+
		`{"run_id":"other","phase":"plan","branch":"feat","repo":"/elsewhere"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	if c := Command([]string{"pending"}, &out, &errb); c != 0 {
		t.Fatalf("pending: exit %d, stderr %q", c, errb.String())
	}
	want := filepath.Join(os.Getenv("REVIEW_LOOP_HOME"), "run", "mine", "pending-report.md") + "\n"
	if out.String() != want {
		t.Errorf("pending stdout %q, want %q", out.String(), want)
	}
}

// The fourth recorded divergence, added after the port. runlog.py recorded any non-blank
// --missing, but the push refusal now prints `--missing '<gates>'`, and pasted unchanged that
// would record the placeholder as the gates not run. The port refuses the placeholder itself,
// bare or single-quoted and whitespace-trimmed, and nothing else.
func TestThePlaceholderGatesAreRefused(t *testing.T) {
	repo, _ := scenarioRepo(t, wscenario{})
	t.Chdir(repo)
	setEnv(t, nil)
	store := filepath.Join(t.TempDir(), "runs.jsonl")
	t.Setenv("REVIEW_LOOP_RUNS", store)
	var out, errb bytes.Buffer
	if c := Command([]string{"plan", "--tier", "fast", "--model", "m", "--run-id", "ph"}, &out, &errb); c != 0 {
		t.Fatalf("plan: %d %s", c, errb.String())
	}
	for _, missing := range []string{"<gates>", " <gates>\n", "'<gates>'"} {
		out.Reset()
		errb.Reset()
		c := Command([]string{"abandon", "--run-id", "ph", "--missing", missing}, &out, &errb)
		if c != 1 || !strings.Contains(errb.String(), "placeholder") ||
			!strings.Contains(errb.String(), "planned gates that were not run and why") {
			t.Errorf("--missing %q was not refused as the placeholder: exit %d, stderr %q", missing, c, errb.String())
		}
	}
	if b, _ := os.ReadFile(store); strings.Count(string(b), "\n") != 1 {
		t.Errorf("a refused abandon wrote a row:\n%s", b)
	}
	// Narrow: a real answer that merely mentions the placeholder is still recorded, as given.
	const given = "agent_7: session died before <gates> ran"
	errb.Reset()
	if c := Command([]string{"abandon", "--run-id", "ph", "--missing", given}, &out, &errb); c != 0 {
		t.Errorf("a real --missing was refused: exit %d, stderr %q", c, errb.String())
	}
	b, _ := os.ReadFile(store)
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	var last map[string]any
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &last); err != nil ||
		last["outcome"] != "abandoned" || last["abandoned_missing"] != given {
		t.Errorf("a real --missing was not recorded as given (%v):\n%s", err, b)
	}
}

// Where runlog.py dies iterating a number or a bool as --agents or --escalations (TypeError,
// nothing recorded), the port refuses with a message and also records nothing. Recorded as given
// it would be a finish the typed reader cannot decode.
func TestANumberForAListArgumentIsRefused(t *testing.T) {
	repo, _ := scenarioRepo(t, wscenario{})
	t.Chdir(repo)
	setEnv(t, nil)
	store := filepath.Join(t.TempDir(), "runs.jsonl")
	t.Setenv("REVIEW_LOOP_RUNS", store)
	var out, errb bytes.Buffer
	if c := Command([]string{"plan", "--tier", "fast", "--model", "m", "--run-id", "nn"}, &out, &errb); c != 0 {
		t.Fatalf("plan: %d %s", c, errb.String())
	}
	for _, args := range [][]string{{"--escalations", "5"}, {"--escalations", "true"}, {"--agents", "true"}, {"--agents", "7"}} {
		errb.Reset()
		c := Command(append([]string{"finish", "--run-id", "nn", "--outcome", "clean", "--tier", "fast"}, args...), &out, &errb)
		if c != 1 || !strings.Contains(errb.String(), "must be a JSON list") {
			t.Errorf("%v: exit %d, stderr %q", args, c, errb.String())
		}
	}
	if b, _ := os.ReadFile(store); strings.Count(string(b), "\n") != 1 {
		t.Errorf("a refused finish wrote a row:\n%s", b)
	}
}

// runlog.py opens the store as strict UTF-8 and dies on one bad byte anywhere in it, on every
// subcommand that loads it. looper reads the row, the byte as U+FFFD, and still gives a verdict.
func TestInvalidUTF8InTheStoreIsReadNotFatal(t *testing.T) {
	repo, _ := scenarioRepo(t, wscenario{})
	t.Chdir(repo)
	setEnv(t, nil)
	store := filepath.Join(t.TempDir(), "runs.jsonl")
	t.Setenv("REVIEW_LOOP_RUNS", store)
	rows := `{"run_id":"iu","phase":"plan","repo":"r","head":"h"}` + "\n" +
		`{"run_id":"iu","phase":"cycle","n":1,"applied":0,"agents":1,"width_reason":"a` + "\xff" + `b"}` + "\n"
	if err := os.WriteFile(store, []byte(rows), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	if c := Command([]string{"convergence", "--run-id", "iu"}, &out, &errb); c != 0 || strings.TrimSpace(out.String()) != "converged" {
		t.Errorf("convergence over invalid UTF-8: exit %d, out %q, err %q", c, out.String(), errb.String())
	}
	// And the lenient reader the write guards use: show sees the cycle row too.
	out.Reset()
	errb.Reset()
	if c := Command([]string{"show", "--run-id", "iu"}, &out, &errb); c != 0 || !strings.Contains(out.String(), "width_reason") {
		t.Errorf("show over invalid UTF-8: exit %d, out %q, err %q", c, out.String(), errb.String())
	}
}

// Only a missing store reads as empty. runlog.py dies on any other read error, and an unreadable
// store read as "no runs" would let `check` tell the Stop hook nothing is open.
func TestAnUnreadableStoreIsAnErrorNotEmpty(t *testing.T) {
	repo, _ := scenarioRepo(t, wscenario{})
	t.Chdir(repo)
	setEnv(t, nil)
	t.Setenv("REVIEW_LOOP_RUNS", t.TempDir()) // a directory: EISDIR, not ENOENT
	for _, args := range [][]string{
		{"check"},
		{"cycle", "--run-id", "u", "--n", "1", "--applied", "0", "--agents", "1"},
		{"convergence", "--run-id", "u"},
		{"finish", "--run-id", "u", "--outcome", "clean"},
		{"abandon", "--run-id", "u", "--missing", "x"},
		{"show", "--run-id", "u"},
		{"pending"},
	} {
		var out, errb bytes.Buffer
		if c := Command(args, &out, &errb); c != 1 || !strings.Contains(errb.String(), "cannot read the run store") {
			t.Errorf("%v: exit %d, out %q, err %q", args, c, out.String(), errb.String())
		}
	}
}

func without(xs []string, drop string) []string {
	out := []string{}
	for _, x := range xs {
		if x != drop {
			out = append(out, x)
		}
	}
	return out
}

// Issue #41: a run that ends with asks still owed records asks-open, which is not a broken
// outcome. It needs at least one ask, and it derives halted, never converged.
func TestAsksOpenNeedsAsks(t *testing.T) {
	repo, _ := scenarioRepo(t, wscenario{})
	t.Chdir(repo)
	setEnv(t, nil)
	store := filepath.Join(t.TempDir(), "runs.jsonl")
	t.Setenv("REVIEW_LOOP_RUNS", store)
	var out, errb bytes.Buffer
	for _, id := range []string{"oa", "ob"} {
		if c := Command([]string{"plan", "--tier", "fast", "--model", "m", "--run-id", id}, &out, &errb); c != 0 {
			t.Fatalf("plan: %d %s", c, errb.String())
		}
	}
	before, _ := os.ReadFile(store)
	for _, asks := range [][]string{{"--asks", "0"}, nil} {
		errb.Reset()
		args := append([]string{"finish", "--run-id", "oa", "--outcome", "asks-open", "--tier", "fast"}, asks...)
		if c := Command(args, &out, &errb); c != 1 ||
			!strings.Contains(errb.String(), "refusing to finish as 'asks-open' with no unresolved asks") {
			t.Errorf("asks-open with %v was not refused: exit %d, stderr %q", asks, c, errb.String())
		}
	}
	if after, _ := os.ReadFile(store); !bytes.Equal(before, after) {
		t.Errorf("a refused finish wrote to the store:\n%s", after)
	}
	errb.Reset()
	if c := Command([]string{"cycle", "--run-id", "ob", "--n", "1", "--agents", "1", "--applied", "0", "--asked", "2"}, &out, &errb); c != 0 {
		t.Fatalf("cycle: %d %s", c, errb.String())
	}
	errb.Reset()
	if c := Command([]string{"finish", "--run-id", "ob", "--outcome", "asks-open", "--tier", "fast", "--asks", "2"}, &out, &errb); c != 0 {
		t.Fatalf("asks-open with 2 asks was refused: exit %d, stderr %q", c, errb.String())
	}
	runs, err := Load(store, 0)
	if err != nil {
		t.Fatal(err)
	}
	if conv, err := runs["ob"].Convergence(); err != nil || conv != Halted {
		t.Errorf("asks-open run derived %q (err %v), want %q", conv, err, Halted)
	}
}
