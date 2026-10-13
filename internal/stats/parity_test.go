package stats

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/pinepeakdigital/looper/internal/golden"
)

// The parity gate: every scenario ran through review-stats.py and runs through Command against
// the same store and environment, and must leave the same exit status, stdout and stderr.
// review-stats.py's side is frozen under testdata/oracle (internal/golden says what a golden is
// and is not). Nothing in either report depends on the clock, a path or a generated id, so the
// one normalisation is the overflow line's pointer, which names the command to run for the
// full list: `review-stats.py` there, `looper stats` here.

type scenario struct {
	name    string
	rows    []string
	missing bool // no store file at all
	args    []string
	env     map[string]string
}

var managedEnv = []string{"CLAUDE_CODE_SESSION_ID", "AO_SESSION_ID", "REVIEW_LOOP_TAIL", "REVIEW_LOOP_RUNS"}

var s1 = map[string]string{"CLAUDE_CODE_SESSION_ID": "s1"}

func with(env map[string]string, k, v string) map[string]string {
	out := map[string]string{k: v}
	for a, b := range env {
		if a != k {
			out[a] = b
		}
	}
	return out
}

var alarmArgs = []string{"--alarm"}

// plan is a plan row as `looper record plan` writes it, trimmed to the fields stats reads.
// gates is the gate map's body, each gate planned to run.
func plan(id, at, session string, gates ...string) string {
	g := make([]string, len(gates))
	for i, name := range gates {
		g[i] = fmt.Sprintf(`%q:{"planned":"run","reason":"r"}`, name)
	}
	sess := "null"
	if session != "" {
		sess = strconv.Quote(session)
	}
	return fmt.Sprintf(`{"run_id":%q,"phase":"plan","planned_at":%q,"session_id":%s,"repo":"/r/one","orchestrator_model":"opus","session_kind":"interactive","tier_floor":"full","agent_cap":40,"gates":{%s}}`,
		id, at, sess, strings.Join(g, ","))
}

// fin is a finish row; executed is gate=status pairs. A blank status leaves the key out,
// EMPTY writes "" and NULL writes null.
func fin(id, outcome string, executed ...string) string {
	e := make([]string, 0, len(executed))
	for _, kv := range executed {
		g, st, _ := strings.Cut(kv, "=")
		switch st {
		case "":
			e = append(e, fmt.Sprintf(`%q:{"reason":"r"}`, g))
			continue
		case "EMPTY":
			e = append(e, fmt.Sprintf(`%q:{"status":"","reason":"r"}`, g))
			continue
		case "NULL":
			e = append(e, fmt.Sprintf(`%q:{"status":null,"reason":"r"}`, g))
			continue
		}
		e = append(e, fmt.Sprintf(`%q:{"status":%q,"reason":"r"}`, g, st))
	}
	return fmt.Sprintf(`{"run_id":%q,"phase":"finish","outcome":%q,"tier_executed":"full","executed":{%s}}`,
		id, outcome, strings.Join(e, ","))
}

func abandon(id, missing string) string {
	return fmt.Sprintf(`{"run_id":%q,"phase":"finish","outcome":"abandoned","abandoned_missing":%q}`, id, missing)
}

func at(day int) string { return fmt.Sprintf("2026-02-%02dT%02d:00:00", 1+day/24, day%24) }

// runs is n finished runs of the given gate statuses, ids prefix1..n, from day `from`.
func runs(prefix string, from, n int, gates []string, executed ...string) []string {
	var out []string
	for i := 1; i <= n; i++ {
		id := fmt.Sprintf("%s%d", prefix, i)
		out = append(out, plan(id, at(from+i), "s1", gates...), fin(id, "clean", executed...))
	}
	return out
}

func cat(parts ...[]string) []string {
	var out []string
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

var g1 = []string{"g"}

// windowRows is 52 runs written NEWEST FIRST, so only the planned_at sort puts them in order.
// Gate g drops in the two oldest (outside the window) and in two inside it, so a window one
// wider raises it; gate h drops three times inside, the oldest of them the first run in the
// window, so a window one narrower silences it.
func windowRows() []string {
	var rows []string
	for i := 52; i >= 1; i-- {
		id := fmt.Sprintf("w%02d", i)
		var ex []string
		switch i {
		case 1, 2, 30:
			ex = []string{"g=failed", "h=done"}
		case 3:
			ex = []string{"g=failed", "h=failed"}
		case 10, 20:
			ex = []string{"g=done", "h=failed"}
		default:
			ex = []string{"g=done", "h=done"}
		}
		rows = append(rows, plan(id, at(i), "s1", "g", "h"), fin(id, "clean", ex...))
	}
	return rows
}

// noPlannedAtRows: three runs with no planned_at drop g, after fifty clean runs in the file.
// They sort FIRST, as "" does, so the window leaves them out.
func noPlannedAtRows() []string {
	rows := runs("ok", 0, 50, g1, "g=done")
	for i := 1; i <= 3; i++ {
		id := fmt.Sprintf("np%d", i)
		rows = append(rows, fmt.Sprintf(`{"run_id":%q,"phase":"plan","session_id":"s1","gates":{"g":{"planned":"run","reason":"r"}}}`, id),
			fin(id, "clean"))
	}
	return rows
}

const longPrefix = "record_reviewed and learnings_capture and upstream_drift_check and threat_model_update both planned and"

var scenarios = []scenario{
	// ---- the alarm ------------------------------------------------------------------------
	{name: "alarm, missing store", missing: true, args: alarmArgs, env: s1},
	{name: "alarm, empty store", rows: []string{}, args: alarmArgs, env: s1},
	{name: "alarm, two drops stay silent", rows: runs("d", 0, 2, g1), args: alarmArgs, env: s1},
	{name: "alarm, three unreported drops fire", rows: runs("d", 0, 3, g1), args: alarmArgs, env: s1},
	{name: "alarm, an empty status reads as unreported", rows: cat(runs("d", 0, 1, g1, "g="), runs("e", 5, 1, g1, "g=EMPTY"), runs("f", 9, 1, g1, "g=NULL")), args: alarmArgs, env: s1},
	{name: "alarm, three skips are declined", rows: runs("s", 0, 3, g1, "g=skipped"), args: alarmArgs, env: s1},
	{name: "alarm, pending deferred and waived are declined and named", rows: cat(
		runs("p", 0, 1, g1, "g=pending"), runs("q", 3, 1, g1, "g=deferred"), runs("r", 6, 1, g1, "g=waived")), args: alarmArgs, env: s1},
	{name: "alarm, failed blocked passed and unknown spellings go loud", rows: cat(
		runs("a", 0, 3, []string{"a"}, "a=failed"),
		runs("b", 5, 1, []string{"b"}, "b=blocked"), runs("c", 7, 1, []string{"b"}, "b=passed"),
		runs("e", 9, 1, []string{"b"}, "b=FAILED"), runs("f", 11, 3, []string{"c"}, "c=probably fine")), args: alarmArgs, env: s1},
	{name: "alarm, unreported is not named beside a failure", rows: cat(runs("a", 0, 2, g1), runs("b", 5, 1, g1, "g=timeout")), args: alarmArgs, env: s1},
	{name: "alarm, done and n-a are not drops", rows: cat(runs("a", 0, 3, g1, "g=done"), runs("b", 5, 3, g1, "g=n/a")), args: alarmArgs, env: s1},
	{name: "alarm, a gate planned to skip is not a drop", rows: []string{
		`{"run_id":"k1","phase":"plan","planned_at":"2026-01-01T00:00:00","session_id":"s1","gates":{"g":{"planned":"skip","reason":"r"}}}`, fin("k1", "clean"),
		`{"run_id":"k2","phase":"plan","planned_at":"2026-01-02T00:00:00","session_id":"s1","gates":{"g":{"planned":"skip","reason":"r"}}}`, fin("k2", "clean"),
		`{"run_id":"k3","phase":"plan","planned_at":"2026-01-03T00:00:00","session_id":"s1","gates":{"g":{"planned":"skip","reason":"r"}}}`, fin("k3", "clean"),
	}, args: alarmArgs, env: s1},
	{name: "alarm, one decline and one drop stay silent", rows: cat(runs("a", 0, 1, g1, "g=skipped"), runs("b", 3, 1, g1)), args: alarmArgs, env: s1},
	{name: "alarm, two of each is raised in varying ways", rows: cat(runs("a", 0, 2, g1), runs("b", 5, 2, g1, "g=skipped")), args: alarmArgs, env: s1},
	{name: "alarm, a gate over both thresholds gets a line per bucket", rows: cat(runs("a", 0, 3, g1), runs("b", 5, 3, g1, "g=skipped")), args: alarmArgs, env: s1},
	{name: "alarm, ranked across buckets then by name", rows: cat(
		runs("a", 0, 3, []string{"aaa_drop", "zzz_declined", "mmm_drop"}, "zzz_declined=skipped"),
		runs("b", 10, 3, []string{"zzz_declined"}, "zzz_declined=skipped"),
		runs("c", 20, 3, []string{"bbb_drop"})), args: alarmArgs, env: s1},
	{name: "alarm, a status spelled varying ways loses its count", rows: runs("v", 0, 3, g1, "g=varying ways"), args: alarmArgs, env: s1},
	{name: "alarm, two abandonments stay silent", rows: []string{
		plan("x1", at(1), "s1", "g"), abandon("x1", "stopped"), plan("x2", at(2), "s1", "g"), abandon("x2", "stopped"),
	}, args: alarmArgs, env: s1},
	{name: "alarm, abandonment counted once and not per gate, with reasons grouped", rows: []string{
		plan("w1", at(1), "s1", "a", "b", "c"), abandon("w1", "plan.py re-invoked to re-read its own plan"),
		plan("w2", at(2), "s1", "a", "b", "c"), abandon("w2", "plan.py re-invoked to re-read its own plan"),
		plan("w3", at(3), "s1", "a", "b", "c"), abandon("w3", "plan.py re-invoked to re-read its own plan"),
		plan("w4", at(4), "s1", "a"), abandon("w4", "agent_7: fast path approved"),
		plan("w5", at(5), "someone-else", "a"),
	}, args: alarmArgs, env: s1},
	{name: "alarm, abandonment reasons group on flattened text", rows: []string{
		plan("f1", at(1), "s1", "g"), abandon("f1", "a  double  space"),
		plan("f2", at(2), "s1", "g"), abandon("f2", "a double space"),
		plan("f3", at(3), "s1", "g"), abandon("f3", "   a double\tspace\n "),
		plan("f4", at(4), "s1", "g"), abandon("f4", "   "),
	}, args: alarmArgs, env: s1},
	{name: "alarm, long reasons are clipped in the middle", rows: []string{
		plan("q0", at(0), "s1", "g"), abandon("q0", strings.Repeat("é", 97)),
		plan("q1", at(1), "s1", "g"), abandon("q1", longPrefix+" never reached cycle 1 of the fan-out"),
		plan("q2", at(2), "s1", "g"), abandon("q2", longPrefix+" never reached cycle 2 of the fan-out"),
		plan("q3", at(3), "s1", "g"), abandon("q3", strings.Repeat("x", 96)),
	}, args: alarmArgs, env: s1},
	{name: "alarm, a reason of exactly the clip is not clipped", rows: []string{
		plan("q1", at(1), "s1", "g"), abandon("q1", strings.Repeat("x", 96)),
		plan("q2", at(2), "s1", "g"), abandon("q2", strings.Repeat("y", 95)+"\u00e9"),
		plan("q3", at(3), "s1", "g"), abandon("q3", strings.Repeat("z", 97)),
	}, args: alarmArgs, env: s1},
	{name: "alarm, more reasons than shown, ties in run order", rows: []string{
		plan("n1", at(1), "s1", "g"), abandon("n1", "cause 1"),
		plan("n2", at(2), "s1", "g"), abandon("n2", "cause 2"),
		plan("n3", at(3), "s1", "g"), abandon("n3", "cause 3"),
		plan("n4", at(4), "s1", "g"), abandon("n4", "cause 4"),
		plan("n5", at(5), "s1", "g"), abandon("n5", "cause 4"),
		plan("n6", at(6), "s1", "g"), abandon("n6", "cause 5"),
	}, args: alarmArgs, env: s1},
	{name: "alarm, other sessions open runs are abandoned", rows: []string{
		plan("o1", at(1), "other", "g"), plan("o2", at(2), "other", "g"), plan("o3", at(3), "other2", "g"),
	}, args: alarmArgs, env: s1},
	{name: "alarm, AO_SESSION_ID is a session too", rows: []string{
		plan("o1", at(1), "other", "g"), plan("o2", at(2), "other", "g"), plan("o3", at(3), "other2", "g"),
	}, args: alarmArgs, env: map[string]string{"AO_SESSION_ID": "ao"}},
	{name: "alarm, no session env derives nothing", rows: []string{
		plan("o1", at(1), "other", "g"), plan("o2", at(2), "other", "g"), plan("o3", at(3), "other2", "g"),
	}, args: alarmArgs},
	{name: "alarm, a blank session env derives nothing", rows: []string{
		plan("o1", at(1), "other", "g"), plan("o2", at(2), "other", "g"), plan("o3", at(3), "other2", "g"),
	}, args: alarmArgs, env: map[string]string{"CLAUDE_CODE_SESSION_ID": "", "AO_SESSION_ID": ""}},
	{name: "alarm, this sessions open runs are in flight", rows: []string{
		plan("o1", at(1), "s1", "g"), plan("o2", at(2), "s1", "g"), plan("o3", at(3), "s1", "g"),
	}, args: alarmArgs, env: s1},
	{name: "alarm, a run that named no session is not abandoned", rows: []string{
		plan("o1", at(1), "", "g"),
		`{"run_id":"o2","phase":"plan","planned_at":"2026-01-02T00:00:00","gates":{"g":{"planned":"run","reason":"r"}}}`,
		`{"run_id":"o3","phase":"plan","planned_at":"2026-01-03T00:00:00","session_id":"","gates":{"g":{"planned":"run","reason":"r"}}}`,
	}, args: alarmArgs, env: s1},
	{name: "alarm, the window is the last fifty by planned_at", rows: windowRows(), args: alarmArgs, env: s1},
	{name: "alarm, runs with no planned_at sort oldest", rows: noPlannedAtRows(), args: alarmArgs, env: s1},
	{name: "alarm, bookkeeping runs take window slots", rows: func() []string {
		rows := runs("d", 0, 3, g1)
		for i := 1; i <= 50; i++ {
			rows = append(rows, fmt.Sprintf(`{"run_id":"c%d","phase":"finish","outcome":"carried","tier_executed":"carried","repo":"/r/one","planned_at":"2026-09-01T00:00:00","executed":{}}`, i))
		}
		return rows
	}(), args: alarmArgs, env: s1},
	{name: "alarm, the tail bounds what is read", rows: runs("d", 0, 3, g1), args: alarmArgs, env: with(s1, "REVIEW_LOOP_TAIL", "4")},
	{name: "alarm, a tail that does not parse reads 4000", rows: runs("d", 0, 3, g1), args: alarmArgs, env: with(s1, "REVIEW_LOOP_TAIL", "abc")},
	{name: "alarm, a negative tail reads one line", rows: runs("d", 0, 3, g1), args: alarmArgs, env: with(s1, "REVIEW_LOOP_TAIL", "-5")},
	{name: "alarm, torn and runless lines are skipped", rows: cat(runs("d", 0, 3, g1), []string{"not json", `{"phase":"plan"}`, `{"run_id":"","phase":"plan"}`, ""}),
		args: alarmArgs, env: s1},
	{name: "alarm ignores the repo filter", rows: runs("d", 0, 3, g1), args: []string{"--alarm", "--repo", "nowhere"}, env: s1},

	// ---- the report -----------------------------------------------------------------------
	{name: "report, missing store", missing: true, env: s1},
	{name: "report, empty store", rows: []string{}, env: s1},
	{name: "report, the headline partitions the runs", rows: []string{
		`{"run_id":"a1","phase":"plan","planned_at":"2026-01-01T00:00:00","session_id":"s1","repo":"r"}`,
		`{"run_id":"a1","phase":"finish","outcome":"clean","tier_executed":"full"}`,
		`{"run_id":"a2","phase":"plan","planned_at":"2026-01-02T00:00:00","session_id":"s1","repo":"r"}`,
		`{"run_id":"a2","phase":"finish","outcome":"cycle-limit","tier_executed":"partial"}`,
		`{"run_id":"a3","phase":"plan","planned_at":"2026-01-03T00:00:00","session_id":"s1","repo":"r"}`,
		`{"run_id":"a3","phase":"finish","outcome":"abandoned"}`,
		`{"run_id":"a4","phase":"plan","planned_at":"2026-01-04T00:00:00","session_id":"s9","repo":"r"}`,
		`{"run_id":"a5","phase":"plan","planned_at":"2026-01-05T00:00:00","session_id":null,"repo":"r"}`,
		`{"run_id":"k1","phase":"plan","planned_at":"2026-01-01T06:00:00","session_id":"s1","repo":"r","orchestrator_model":"(none)","session_kind":"headless","tier_floor":"skipped","gates":{}}`,
		`{"run_id":"k1","phase":"finish","outcome":"skipped","tier_executed":"skipped","executed":{}}`,
		`{"run_id":"k2","phase":"plan","planned_at":"2026-01-02T06:00:00","session_id":"s1","repo":"r"}`,
		`{"run_id":"k2","phase":"finish","outcome":"skipped","tier_executed":"skipped"}`,
		`{"run_id":"c1","phase":"finish","outcome":"carried","tier_executed":"carried","repo":"r","session_kind":"interactive"}`,
		`{"run_id":"c2","phase":"finish","outcome":"carried","tier_executed":"carried","repo":"r"}`,
		`{"run_id":"c3","phase":"finish","outcome":"carried","tier_executed":"carried","repo":"r"}`,
		`{"run_id":"n1","phase":"nudge","nudged_at":"2026-01-06T00:00:00"}`,
	}, env: s1},
	{name: "report, with no session env nothing is derived abandoned", rows: []string{
		plan("a1", at(1), "s1"), fin("a1", "clean"), plan("a2", at(2), "s9"), plan("a3", at(3), "s1"), fin("a3", "abandoned"),
	}},
	{name: "report, AO_SESSION_ID names the session", rows: []string{
		plan("a1", at(1), "ao"), plan("a2", at(2), "s9"),
	}, env: map[string]string{"AO_SESSION_ID": "ao"}},
	{name: "report, spend divides finished cycles by finished runs", rows: []string{
		`{"run_id":"f1","phase":"plan","planned_at":"2026-03-01T00:00:00","repo":"r","gates":{"threat_model":{"planned":"run","reason":"2 stale claims"}}}`,
		`{"run_id":"f1","phase":"cycle","n":1,"applied":1,"agents":4,"subagent_tokens":400000}`,
		`{"run_id":"f1","phase":"finish","finished_at":"2026-03-01T01:00:00","outcome":"clean","tier_executed":"full","executed":{"threat_model":{"status":"done"}}}`,
		`{"run_id":"o1","phase":"plan","planned_at":"2026-03-02T00:00:00","repo":"r","gates":{"threat_model":{"planned":"run","reason":"2 stale claims"}}}`,
		`{"run_id":"o1","phase":"cycle","n":1,"applied":1,"agents":20}`,
	}, env: s1},
	{name: "report, convergence and rounding", rows: []string{
		plan("v1", at(1), "s1"), `{"run_id":"v1","phase":"cycle","n":1,"applied":0,"agents":3,"subagent_tokens":1234567}`, fin("v1", "clean"),
		plan("v2", at(2), "s1"), `{"run_id":"v2","phase":"cycle","n":1,"applied":2,"agents":2}`,
		`{"run_id":"v2","phase":"cycle","n":2,"applied":1,"agents":2,"analysis_changed":true}`, fin("v2", "cycle-limit"),
		`{"run_id":"v3","phase":"plan","planned_at":"2026-02-01T03:00:00","session_id":"s1","agent_cap":4}`,
		`{"run_id":"v3","phase":"cycle","n":1,"applied":3,"agents":4}`, fin("v3", "cycle-limit"),
		plan("v4", at(4), "s1"), fin("v4", "test-failure"),
		plan("v5", at(5), "s1"), `{"run_id":"v5","phase":"cycle","n":1,"applied":0}`,
		`{"run_id":"v5","phase":"finish","outcome":"clean","unresolved_asks":2}`,
		plan("v6", at(6), "s1"), `{"run_id":"v6","phase":"cycle","n":1,"applied":0,"agents":1,"subagent_tokens":5}`, fin("v6", "clean"),
	}, env: s1},
	{name: "report, a mean exactly halfway rounds to even", rows: []string{
		plan("h1", at(1), "s1"), `{"run_id":"h1","phase":"cycle","n":1,"applied":0,"agents":5}`, fin("h1", "clean"),
		plan("h2", at(2), "s1"), `{"run_id":"h2","phase":"cycle","n":1,"applied":0,"agents":4}`, fin("h2", "clean"),
		plan("h3", at(3), "s1"), fin("h3", "clean"), plan("h4", at(4), "s1"), fin("h4", "clean"),
	}, env: s1},
	{name: "report, tokens per agent exactly halfway rounds to even", rows: []string{
		plan("h1", at(1), "s1"), `{"run_id":"h1","phase":"cycle","n":1,"applied":0,"agents":2,"subagent_tokens":2469}`, fin("h1", "clean"),
	}, env: s1},
	{name: "report, negative counts are summed as written", rows: []string{
		plan("h1", at(1), "s1"), `{"run_id":"h1","phase":"cycle","n":1,"applied":0,"agents":-3,"subagent_tokens":-7000}`, fin("h1", "clean"),
	}, env: s1},
	{name: "report, tokens without agents print neither", rows: []string{
		plan("t1", at(1), "s1"), `{"run_id":"t1","phase":"cycle","n":1,"applied":0,"agents":0,"subagent_tokens":500}`, fin("t1", "clean"),
	}, env: s1},
	{name: "report, no tokens recorded says so", rows: []string{
		plan("t1", at(1), "s1"), `{"run_id":"t1","phase":"cycle","n":1,"applied":0,"agents":2,"subagent_tokens":null}`, fin("t1", "clean"),
		plan("t2", at(2), "s1"), `{"run_id":"t2","phase":"cycle","n":1,"applied":0,"agents":3,"subagent_tokens":0}`, fin("t2", "clean"),
	}, env: s1},
	{name: "report, dropped gates by model and kind", rows: []string{
		`{"run_id":"g1","phase":"plan","planned_at":"2026-01-01T00:00:00","session_id":"s1","orchestrator_model":"opus","session_kind":"headless","gates":{"a":{"planned":"run"},"b":{"planned":"run"},"c":{"planned":"run"},"d":{"planned":"run"},"e":{"planned":"run"}}}`,
		fin("g1", "clean", "a=failed", "b=done", "c=done", "d=done", "e=done"),
		`{"run_id":"g2","phase":"plan","planned_at":"2026-01-02T00:00:00","session_id":"s1","orchestrator_model":"opus","session_kind":"headless","gates":{"a":{"planned":"run"},"b":{"planned":"run"},"c":{"planned":"run"},"d":{"planned":"run"},"e":{"planned":"run"}}}`,
		fin("g2", "clean", "a=failed", "b=skipped", "c=done", "d=done", "e=done"),
		`{"run_id":"g3","phase":"plan","planned_at":"2026-01-03T00:00:00","session_id":"s1","orchestrator_model":"opus","session_kind":"headless","gates":{"a":{"planned":"run"},"b":{"planned":"run"},"c":{"planned":"run"},"d":{"planned":"run"},"e":{"planned":"run"}}}`,
		fin("g3", "clean", "a=failed", "b=skipped", "c=n/a", "d=done", "e=done"),
		`{"run_id":"g4","phase":"plan","planned_at":"2026-01-04T00:00:00","session_id":"s1","orchestrator_model":"opus","session_kind":"headless","gates":{"a":{"planned":"run"},"b":{"planned":"run"},"c":{"planned":"run"},"d":{"planned":"run"},"e":{"planned":"run"}}}`,
		fin("g4", "clean", "a=failed", "b=skipped", "c=skipped", "d=done", "e=done"),
		`{"run_id":"g5","phase":"plan","planned_at":"2026-01-05T00:00:00","session_id":"s1","orchestrator_model":"opus","session_kind":"headless","gates":{"a":{"planned":"run"},"b":{"planned":"run"},"c":{"planned":"run"},"d":{"planned":"run"},"e":{"planned":"run"}}}`,
		fin("g5", "clean", "a=done", "b=done", "c=done", "d=x", "e=done"),
		`{"run_id":"g6","phase":"plan","planned_at":"2026-01-06T00:00:00","session_id":"s1","orchestrator_model":"opus","session_kind":"headless","gates":{"a":{"planned":"run"},"b":{"planned":"run"},"c":{"planned":"run"},"d":{"planned":"run"},"e":{"planned":"run"}}}`,
		fin("g6", "clean", "a=done", "b=done", "c=done", "d=done", "e=y"),
		`{"run_id":"g7","phase":"plan","planned_at":"2026-01-07T00:00:00","session_id":"s1","gates":{"z":{"planned":"run"}}}`,
		fin("g7", "clean"),
		`{"run_id":"g8","phase":"plan","planned_at":"2026-01-08T00:00:00","session_id":"s1","orchestrator_model":"haiku","session_kind":"ao-worker","gates":{"y":{"planned":"run"}}}`,
		`{"run_id":"g9","phase":"plan","planned_at":"2026-01-09T00:00:00","session_id":"other","orchestrator_model":"haiku","gates":{"y":{"planned":"run"}}}`,
	}, env: s1},
	{name: "report, escalations", rows: []string{
		plan("e1", at(1), "s1"),
		`{"run_id":"e1","phase":"finish","outcome":"clean","escalations":[{"gate":"security","reason":"x"},{"gate":null},{"reason":"no gate"},{"gate":""},null]}`,
		plan("e2", at(2), "s1"),
		`{"run_id":"e2","phase":"finish","outcome":"clean","escalations":[{"gate":"security"},{"gate":"agent_7"}]}`,
		plan("e3", at(3), "s1"),
		`{"run_id":"e3","phase":"finish","outcome":"clean","escalations":[null]}`,
	}, env: s1},
	{name: "report, why runs were abandoned", rows: []string{
		plan("a1", at(1), "s1"), abandon("a1", "cause  one"),
		plan("a2", at(2), "s1"), abandon("a2", " cause one"),
		plan("a3", at(3), "other"),
		plan("a4", at(4), "s1"), abandon("a4", longPrefix+" and it is not clipped in the report at all"),
		plan("a5", at(5), "s1"), abandon("a5", "   "),
	}, env: s1},
	{name: "report, unresolved asks are summed over every run", rows: []string{
		plan("u1", at(1), "s1"), `{"run_id":"u1","phase":"finish","outcome":"clean","unresolved_asks":5}`,
		plan("u2", at(2), "s1"), `{"run_id":"u2","phase":"finish","outcome":"skipped","unresolved_asks":-2}`,
		plan("u3", at(3), "s1"), `{"run_id":"u3","phase":"finish","outcome":"clean","unresolved_asks":null}`,
	}, env: s1},
	{name: "report, asks that cancel out print nothing", rows: []string{
		plan("u1", at(1), "s1"), `{"run_id":"u1","phase":"finish","outcome":"clean","unresolved_asks":2}`,
		plan("u2", at(2), "s1"), `{"run_id":"u2","phase":"finish","outcome":"clean","unresolved_asks":-2}`,
	}, env: s1},
	{name: "report, tallies read unset and blank alike", rows: []string{
		`{"run_id":"t1","phase":"plan","planned_at":"2026-01-01T00:00:00","session_id":"s1","tier_floor":"","orchestrator_model":null}`,
		`{"run_id":"t1","phase":"finish","outcome":"clean","tier_executed":null}`,
		`{"run_id":"t2","phase":"plan","planned_at":"2026-01-02T00:00:00","session_id":"s1","tier_floor":"fast","orchestrator_model":"opus","session_kind":"headless"}`,
		`{"run_id":"t2","phase":"finish","outcome":"clean","tier_executed":"fast"}`,
		`{"run_id":"t3","phase":"plan","planned_at":"2026-01-03T00:00:00","session_id":"s1","tier_floor":"fast","orchestrator_model":"opus","session_kind":"headless"}`,
	}, env: s1},
	{name: "report, the repo filter is a substring", rows: []string{
		`{"run_id":"r1","phase":"plan","planned_at":"2026-01-01T00:00:00","session_id":"s1","repo":"/code/looper"}`,
		`{"run_id":"r2","phase":"plan","planned_at":"2026-01-02T00:00:00","session_id":"s1","repo":"/code/skills"}`,
		`{"run_id":"r3","phase":"plan","planned_at":"2026-01-03T00:00:00","session_id":"s1"}`,
		`{"run_id":"r4","phase":"finish","outcome":"carried","tier_executed":"carried","repo":"/code/looper"}`,
		`{"run_id":"r5","phase":"plan","planned_at":"2026-01-05T00:00:00","session_id":"s1","repo":"/code/skills"}`,
		`{"run_id":"r5","phase":"nudge","repo":"/code/looper"}`,
		`{"run_id":"r6","phase":"plan","planned_at":"2026-01-06T00:00:00","session_id":"s1","repo":"/code/looper"}`,
		`{"run_id":"r6","phase":"cycle","n":1,"applied":0,"agents":1,"repo":"/code/skills"}`,
	}, args: []string{"--repo", "looper"}, env: s1},
	{name: "report, a repo filter with an equals sign", rows: []string{
		`{"run_id":"r1","phase":"plan","planned_at":"2026-01-01T00:00:00","session_id":"s1","repo":"/code/looper"}`,
		`{"run_id":"r2","phase":"plan","planned_at":"2026-01-02T00:00:00","session_id":"s1","repo":"/code/skills"}`,
	}, args: []string{"--repo=skills"}, env: s1},
	{name: "report, a repo filter that matches nothing", rows: []string{
		`{"run_id":"r1","phase":"plan","planned_at":"2026-01-01T00:00:00","session_id":"s1","repo":"/code/looper"}`,
	}, args: []string{"--repo", "elsewhere"}, env: s1},
	{name: "report, a blank repo filter filters nothing", rows: []string{
		`{"run_id":"r1","phase":"plan","planned_at":"2026-01-01T00:00:00","session_id":"s1","repo":"/code/looper"}`,
		`{"run_id":"r2","phase":"plan","planned_at":"2026-01-02T00:00:00","session_id":"s1"}`,
	}, args: []string{"--repo", ""}, env: s1},
	{name: "report, the tail bounds what is read", rows: cat(runs("d", 0, 3, g1), runs("e", 10, 1, g1, "g=done")),
		env: with(s1, "REVIEW_LOOP_TAIL", "3")},
	{name: "report, a run split by the tail keeps only what is inside it", rows: []string{
		plan("p1", at(1), "s1", "g"), `{"run_id":"p1","phase":"cycle","n":1,"applied":0,"agents":7}`, fin("p1", "clean"),
	}, env: with(s1, "REVIEW_LOOP_TAIL", "2")},
	{name: "report, later rows merge over earlier ones", rows: []string{
		`{"run_id":"m1","phase":"plan","planned_at":"2026-01-01T00:00:00","session_id":"s9","repo":"/a","session_kind":"headless"}`,
		`{"run_id":"m1","phase":"nudge","session_id":"s1","session_kind":"interactive","planned_at":"2026-01-09T00:00:00"}`,
		`{"run_id":"m2","phase":"plan","planned_at":"2026-01-02T00:00:00","session_id":"s9"}`,
		`{"run_id":"m2","phase":"nudge","session_id":null}`,
	}, env: s1},
}

func setEnv(t *testing.T, env map[string]string) {
	t.Helper()
	for _, k := range managedEnv {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	for k, v := range env {
		os.Setenv(k, v)
	}
}

// writeStore puts the scenario's store in a fresh temp dir and returns its path; for a missing
// store, a path that does not exist.
func writeStore(t *testing.T, sc scenario) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "runs.jsonl")
	if sc.missing {
		return p
	}
	var b strings.Builder
	for _, r := range sc.rows {
		b.WriteString(r + "\n")
	}
	if err := os.WriteFile(p, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func transcript(code int, out, errOut string) string {
	return "exit=" + strconv.Itoa(code) + "\n--- stdout ---\n" + out + "--- stderr ---\n" + errOut
}

// runGo runs Command over the scenario and returns its transcript.
func runGo(t *testing.T, sc scenario) string {
	t.Helper()
	store := writeStore(t, sc)
	setEnv(t, sc.env)
	t.Setenv("REVIEW_LOOP_RUNS", store)
	var out, errb bytes.Buffer
	code := Command(sc.args, &out, &errb)
	return transcript(code, out.String(), errb.String())
}

// canon is the one difference between two correct transcripts: the overflow line names the
// command that lists every reason, and each implementation names itself.
func canon(s string) string {
	return strings.ReplaceAll(s, "`review-stats.py` has them all", "`looper stats` has them all")
}

func TestStatsMatchesThePython(t *testing.T) {
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			want := canon(golden.Read(t, "transcript"))
			if got := runGo(t, sc); got != want {
				t.Errorf("looper stats no longer does what review-stats.py did\n--- review-stats.py (frozen) ---\n%s\n--- looper stats ---\n%s", want, got)
			}
		})
	}
}

// Usage errors are compared on the exit status alone: argparse's wording is not the contract
// (it even varies by Python version), and plan.py reads only the status.
var usageCases = []struct {
	name string
	args []string
}{
	{"an unknown flag", []string{"--nope"}},
	{"a positional argument", []string{"extra"}},
	{"repo with no value", []string{"--repo"}},
	{"help", []string{"-h"}},
}

func TestStatsUsageExitsLikeThePython(t *testing.T) {
	for _, c := range usageCases {
		t.Run(c.name, func(t *testing.T) {
			want := strings.TrimSpace(golden.Read(t, "exit"))
			setEnv(t, nil)
			t.Setenv("REVIEW_LOOP_RUNS", filepath.Join(t.TempDir(), "runs.jsonl"))
			var out, errb bytes.Buffer
			if got := strconv.Itoa(Command(c.args, &out, &errb)); got != want {
				t.Errorf("exit %s, review-stats.py exited %s (stderr %q)", got, want, errb.String())
			}
		})
	}
}
