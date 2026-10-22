// Package plan is `looper plan`: review-loop's plan.py, the Step 0b review plan. It decides
// which gates run and the tier floor from `looper context`'s JSON, four yes/no answers only the
// orchestrator can give, and what git, gh and the two ledgers report; then, unless
// --dry-run, writes the plan row of the run record and prints its run_id last.
//
// The threat model, the Step 0 alarm and the record write are looper's own commands, run in
// process: plan.py ran them as ~/.local/bin/looper, so "looper is missing" was a branch it
// needed and this cannot reach.
package plan

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/pinepeakdigital/looper/internal/ledger"
	"github.com/pinepeakdigital/looper/internal/py"
	"github.com/pinepeakdigital/looper/internal/record"
	runner "github.com/pinepeakdigital/looper/internal/run"
	"github.com/pinepeakdigital/looper/internal/stats"
)

const (
	structuralLines = 150 // Agent #7's "substantial diff" floor, per SKILL.md
	bigFileLines    = 800
)

type (
	obj   = py.Obj
	value = py.Value
)

// Timeouts, plan.py's: git's 15s and gh's and ao's 20s. Variables so a test can shorten one.
var gitTimeout, ghTimeout = 15 * time.Second, 20 * time.Second

// call runs a command with a timeout and returns its exit status and raw output. A command
// that cannot start, or outlives its timeout, is exit 1 with why as stderr, as subprocess
// raised them: a missing binary first, since subprocess raises that before any timeout. A
// child killed by a signal is minus the signal, as Python's returncode is.
func call(timeout time.Duration, name string, args ...string) (int, []byte, string) {
	r := runner.Run("", timeout, name, args...)
	switch {
	case !r.Started && !errors.Is(r.Error, context.DeadlineExceeded):
		return 1, nil, r.Error.Error()
	case r.Expired:
		return 1, nil, fmt.Sprintf("Command '%s' timed out after %d seconds", pyList(name, args), int(timeout.Seconds()))
	}
	// A child that exited while a grandchild still held its pipes (Drained false) has still
	// answered: its status and what it printed stand. plan.py waited for that grandchild up
	// to the timeout; run waits its drain bound.
	return r.Code, r.Out, string(r.Err)
}

// run is plan.py's run(): call, with stdout and stderr decoded as text mode and stripped.
func run(timeout time.Duration, name string, args ...string) (int, string, string) {
	code, out, err := call(timeout, name, args...)
	return code, py.Strip(py.Decode(out)), py.Strip(py.Decode([]byte(err)))
}

// pyList is repr() of the argv list, for TimeoutExpired's message.
func pyList(name string, args []string) string {
	q := []string{"'" + name + "'"}
	for _, a := range args {
		q = append(q, "'"+a+"'")
	}
	return "[" + strings.Join(q, ", ") + "]"
}

func gate(planned, reason string) obj {
	return obj{{"planned", planned}, {"reason", reason}}
}

// threatModel is `looper threat-model`'s report, or nil and why there is none to read. The
// why is the command's own sentence — which ledger, which pin — so the plan says what to fix.
func threatModel() (map[string]any, string) {
	var o, e bytes.Buffer
	if rc := ledger.ThreatModel(nil, &o, &e); rc != 0 {
		if why := py.Strip(e.String()); why != "" {
			return nil, why
		}
		return nil, fmt.Sprintf("exit %d", rc)
	}
	var tm map[string]any
	if json.Unmarshal(o.Bytes(), &tm) != nil {
		return nil, "its report is not JSON"
	}
	return tm, ""
}

// prExists: is there a PR for this branch? Gates the report, evidence and measurement.
func prExists() bool {
	rc, out, _ := run(ghTimeout, "gh", "pr", "view", "--json", "number", "-q", ".number")
	if rc != 0 || out == "" {
		return false
	}
	for _, r := range out {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// githubReachable: can Agent #10 do its job here, and if not, WHICH reason. The two skip
// conditions — gh cannot reach the API, the repo has no PR history to mine — need different
// fixes, and a gate reason is read later as evidence.
func githubReachable() (bool, string) {
	rc, out, _ := run(gitTimeout, "git", "remote", "-v")
	if rc != 0 || !strings.Contains(out, "github.com") {
		return false, "no github remote"
	}
	if rc, out, err := run(ghTimeout, "gh", "auth", "status"); rc != 0 {
		// Unauthenticated only when gh SAYS so. plan.py reported "not authenticated" for any
		// failure of this call, and in a sandbox it fails for a keychain or TLS reason (x509:
		// OSStatus -26276) while `gh auth status` outside it reports logged in — a false
		// cause, recorded in the run as the reason Agent #10 was skipped (friction log,
		// 2026-10-02). The skip stands either way; the reason says what was observed.
		if strings.Contains(strings.ToLower(out+"\n"+err), "not logged in") {
			return false, "gh not authenticated here (retry unsandboxed before accepting this)"
		}
		why := authFailure(err, out)
		if why == "" {
			why = fmt.Sprintf("exit %d", rc)
		}
		return false, "gh auth status failed (" + why + ") without saying gh is logged out — " +
			"auth is unconfirmed, not known to be missing (retry unsandboxed before accepting this)"
	}
	// One API call, not per-file: a repo with zero PRs cannot have PR comments on any file.
	rc, out, err := run(ghTimeout, "gh", "pr", "list", "--state", "all", "--limit", "1", "--json", "number")
	if rc != 0 {
		// Fail OPEN on a failed probe — #10 can discover an empty repo itself — but say so.
		why := fmt.Sprintf("exit %d", rc)
		if err != "" {
			why = py.Splitlines(err)[0]
		}
		return true, "gh authenticated, but could not confirm PR history (" + why + ") — assuming reachable"
	}
	if out == "[]" || out == "" {
		return false, "gh works, but this repo has no pull requests at all — there are no review " +
			"comments to mine, which is a different skip from an auth failure and needs no fix"
	}
	return true, "gh authenticated, github remote present, PR history confirmed"
}

// authFailure is the line of `gh auth status` output that says what failed: its first `X`
// line, without the mark, else its first non-blank line; stderr before stdout. Not simply the
// first line, which is the host header — measured in the sandbox (gh 2.102.0), the output
// was "github.com" then "  X Failed to log in to github.com account narthur (keyring)".
func authFailure(streams ...string) string {
	for _, s := range streams {
		for _, l := range py.Splitlines(s) {
			if rest, ok := strings.CutPrefix(strings.TrimSpace(l), "X "); ok {
				return strings.TrimSpace(rest)
			}
		}
	}
	for _, s := range streams {
		for _, l := range py.Splitlines(s) {
			if l = strings.TrimSpace(l); l != "" {
				return l
			}
		}
	}
	return ""
}

// biggestChangedFile is the line count of the largest file this branch changed, as it is at
// HEAD, and why it could not be measured when it could not. Paths stay bytes end to end:
// plan.py read git's output in text mode and stripped it, so a \r in a name became \n, a
// leading space on the first name was lost, and either file then matched nothing and read as
// 0 lines. A git that fails is said, not read as 0: plan.py's 0 there planned Agent #7 skip
// on a diff whose size it had not measured.
func biggestChangedFile(base string) (int, string) {
	if base == "" {
		return 0, ""
	}
	gitOut := func(args ...string) ([]byte, string) {
		code, out, err := call(gitTimeout, "git", args...)
		if code == 0 {
			return out, ""
		}
		// git's fatal: or error: line, which says why; warnings come before it and usage hints
		// after. Otherwise the first line.
		if l := py.Splitlines(py.Strip(err)); len(l) > 0 {
			for _, x := range l {
				if strings.HasPrefix(x, "fatal:") || strings.HasPrefix(x, "error:") {
					return nil, x
				}
			}
			return nil, l[0]
		}
		return nil, fmt.Sprintf("git %s exited %d", args[0], code)
	}
	root, why := gitOut("rev-parse", "--show-toplevel")
	if why != "" {
		return 0, why
	}
	names, why := gitOut("diff", "--name-only", "-z", "origin/"+base+"...HEAD")
	if why != "" {
		return 0, why
	}
	top := strings.TrimSuffix(string(root), "\n")
	biggest := 0
	for _, name := range strings.Split(string(names), "\x00") {
		if name == "" {
			continue
		}
		// Any read error, as plan.py's OSError: a deleted file has no size at HEAD, and a
		// submodule is a directory.
		b, err := os.ReadFile(filepath.Join(top, name))
		if err != nil {
			continue
		}
		n := bytes.Count(b, []byte("\n"))
		if len(b) > 0 && b[len(b)-1] != '\n' {
			n++
		}
		biggest = max(biggest, n)
	}
	return biggest, ""
}

// answers are the four (five) inputs no script can measure. Booleans, never prose: free text
// is where improvisation re-enters wearing a manifest.
type answers struct {
	logic, behavioralGoal, runtimeChange, attackerReachable, specArtifact bool

	// deltaLogic is whether the delta since last_reviewed_sha touches program logic, asked
	// only for the fast-path re-entry. Optional and fail-closed: unasked, no floor is lowered.
	// logic stays the whole branch's answer, which the risk label reads.
	deltaLogic, deltaLogicGiven bool
}

// context is what plan.py read from context.sh's JSON, plus the re-entry fields `looper
// context` adds, which plan.py never read.
type contextJSON struct {
	changed, semantic, entries int
	excluded, base             *string
	fastBySize, due            bool
	swept                      string // f"{swept}": the count, or None

	// The fast-path re-entry: the newest reviewed ancestor of HEAD and the raw size of
	// everything since it. A context that predates them reads as no re-entry.
	lastReviewed *string
	delta        int
	fastByDelta  bool
}

func readContext(raw []byte) (contextJSON, error) {
	var c contextJSON
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var v any
	if !json.Valid(raw) || d.Decode(&v) != nil {
		return c, errors.New("context is not valid JSON — run looper context first")
	}
	m, isObj := v.(map[string]any)
	if !isObj {
		return c, errors.New("context is not a JSON object — run looper context first")
	}
	num := func(k string) (int, bool, error) {
		v, ok := m[k]
		if !ok || v == nil {
			return 0, false, nil
		}
		n, isNum := v.(json.Number)
		i, err := strconv.Atoi(string(n))
		if !isNum || err != nil {
			return 0, false, fmt.Errorf("context field %s is %s, not a whole number", k, string(mustJSON(v)))
		}
		return i, true, nil
	}
	str := func(k string) (*string, error) {
		v, ok := m[k]
		if !ok || v == nil {
			return nil, nil
		}
		s, isStr := v.(string)
		if !isStr {
			return nil, fmt.Errorf("context field %s is %s, not a string", k, string(mustJSON(v)))
		}
		return &s, nil
	}
	flag := func(k string) (bool, error) {
		v, ok := m[k]
		if !ok || v == nil {
			return false, nil
		}
		b, isBool := v.(bool)
		if !isBool {
			return false, fmt.Errorf("context field %s is %s, not true or false", k, string(mustJSON(v)))
		}
		return b, nil
	}
	var err error
	var has bool
	if c.changed, _, err = num("changed_lines"); err != nil {
		return c, err
	}
	if c.semantic, has, err = num("semantic_lines"); err != nil {
		return c, err
	} else if !has {
		c.semantic = c.changed // a context.sh predating semantic sizing
	}
	if c.entries, _, err = num("learnings_entries"); err != nil {
		return c, err
	} else if v, ok := m["learnings_entries"]; ok && v == nil {
		// Not plan.py's default: its .get("learnings_entries", 0) defaults only a MISSING key,
		// so null printed as "None learnings entries", or crashed comparing None with 40.
		return c, errors.New("context field learnings_entries is null, not a whole number")
	}
	swept, has, err := num("learnings_swept_entries")
	if err != nil {
		return c, err
	}
	c.swept = "None"
	if has {
		c.swept = strconv.Itoa(swept)
	}
	if c.excluded, err = str("sizing_excluded"); err != nil {
		return c, err
	}
	if c.base, err = str("base_branch"); err != nil {
		return c, err
	}
	if c.fastBySize, err = flag("fast_path_eligible_by_size"); err != nil {
		return c, err
	}
	if c.due, err = flag("learnings_compaction_due"); err != nil {
		return c, err
	}
	if c.lastReviewed, err = str("last_reviewed_sha"); err != nil {
		return c, err
	}
	if c.delta, _, err = num("review_delta_lines"); err != nil {
		return c, err
	}
	c.fastByDelta, err = flag("fast_path_eligible_by_delta")
	return c, err
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

func orEmpty(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// build is plan.py's build(): the plan, in the key order the Python dict was written in.
func build(c contextJSON, a answers) obj {
	excluded := orEmpty(c.excluded)
	gates := obj{}
	set := func(k string, g obj) { gates = append(gates, py.Field{Key: k, Val: g}) }

	// --- tier floor: every threshold keys on the semantic count, the review surface, except
	// the fast-path re-entry, which keys on the raw delta since last_reviewed_sha.
	var tier, tierReason string
	switch {
	case c.fastBySize && !a.logic:
		tier = "fast"
		tierReason = fmt.Sprintf("%d lines of review surface and no program logic touched", c.semantic)
		if excluded != "" {
			tierReason += fmt.Sprintf(" (%d raw; excluded %s)", c.changed, excluded)
		}
	case c.fastByDelta && orEmpty(c.lastReviewed) != "" && a.deltaLogicGiven && !a.deltaLogic:
		// The fast-path re-entry: a follow-up after a clean exit is sized against what changed
		// since the last reviewed sha, not the whole branch. plan.py never read the field, so
		// a small follow-up on a big branch floored at full and `record finish` refused fast.
		// `looper context` says true only when there is a reviewed sha to size against; the sha
		// is checked here too, so a context that says otherwise cannot floor a delta measured
		// against nothing. An empty sha names no baseline either, and context's own test is
		// `bool(last_reviewed)`, so it counts as absent. The logic question is the delta's own: the branch may well hold
		// logic already reviewed.
		tier = "fast"
		tierReason = fmt.Sprintf("fast-path re-entry: %d raw lines since the last reviewed sha %s "+
			"(fast path is <30) and no program logic in them; the branch has %d lines of review surface",
			c.delta, orEmpty(c.lastReviewed), c.semantic)
	default:
		tier = "full"
		var why []string
		if !c.fastBySize {
			w := fmt.Sprintf("%d lines of review surface (fast path is <30)", c.semantic)
			if excluded != "" {
				w += fmt.Sprintf("; %d raw, excluded %s", c.changed, excluded)
			}
			why = append(why, w)
		}
		if a.logic {
			why = append(why, "diff touches program logic")
		}
		tierReason = strings.Join(why, "; ")
		if tierReason == "" {
			tierReason = "default"
		}
	}

	// --- always-on
	set("upstream_drift_check", gate("run", "cheap, no LLM"))
	set("learnings_capture", gate("run", "always"))
	hasPR := prExists()
	if hasPR {
		set("pr_report", gate("run", "a PR exists to comment on"))
	} else {
		set("pr_report", gate("skip", "no PR for this branch — defer to the pending-report file, "+
			"or report in-session on a repo that does not use PRs"))
	}
	set("record_reviewed", gate("run", "on clean exit"))

	// --- the sweep: two conditions gate it, so the reason says which one held.
	var why string
	switch {
	case c.due:
		why = fmt.Sprintf("%d learnings entries — at/over the 40 threshold", c.entries)
	case c.entries < 40:
		why = fmt.Sprintf("%d learnings entries — under the 40 threshold", c.entries)
	default:
		why = fmt.Sprintf("%d learnings entries, at/over the 40 threshold, but the last sweep left %s "+
			"and the file has not grown far enough past it to be worth re-sweeping", c.entries, c.swept)
	}
	set("staleness_sweep", gate(map[bool]string{true: "run", false: "skip"}[c.due], why))

	// --- threat model
	if tm, why := threatModel(); tm == nil {
		set("threat_model", gate("run", "looper threat-model failed ("+why+") — run the update rather than assume clean"))
	} else if !truthy(tm["exists"]) {
		set("threat_model", gate("run", "no threat model yet — bounded bootstrap"))
	} else if stale, broken := length(tm["stale"]), length(tm["broken_pins"]); stale+broken > 0 {
		// Step 2b: a broken pin is on the same worklist. Its sha resolves nowhere, so it can
		// never go stale, and a plan keyed on stale alone skipped the one agent that fixes it.
		set("threat_model", gate("run", fmt.Sprintf("%d stale claim(s), %d broken pin(s) — each is a worklist item", stale, broken)))
	} else if a.attackerReachable {
		set("threat_model", gate("run", "diff touches attacker-reachable surface"))
	} else {
		set("threat_model", gate("skip", "no stale claims and no attacker-reachable surface in the diff"))
	}

	if tier == "fast" {
		set("security_review", gate("skip", "fast path folds the finder into the single reviewer"))
	} else {
		set("security_review", gate("run", "runs every cycle"))
	}

	// --- conditional agents
	if tier == "fast" {
		for _, id := range []string{"agent_7_structural", "agent_8_observability", "agent_9_intent",
			"agent_10_prior_feedback", "agent_11_spec"} {
			set(id, gate("skip", "fast path runs no conditional agents"))
		}
	} else {
		// Only pay the per-file scan when the line count alone hasn't decided it.
		big, scanFailed := 0, ""
		if c.semantic < structuralLines {
			big, scanFailed = biggestChangedFile(orEmpty(c.base))
		}
		// An unmeasured diff plans wide: a redundant agent costs tokens, a skipped one ships.
		substantial := c.semantic >= structuralLines || big >= bigFileLines || scanFailed != ""
		r := fmt.Sprintf("%d lines of review surface", c.semantic)
		switch {
		case scanFailed != "":
			r += " — the largest changed file could not be measured (" + scanFailed + "), so planned as substantial"
		case big != 0:
			r += fmt.Sprintf(", largest changed file %d lines", big)
		}
		if !substantial {
			r += fmt.Sprintf(" — under both floors (%d/%d)", structuralLines, bigFileLines)
		}
		if substantial {
			set("agent_7_structural", gate("run", r))
			if scanFailed != "" {
				set("agent_8_observability", gate("run", "planned as substantial, since the diff could not be measured (#7's reason) — skip the observability half if the project logs nothing"))
			} else {
				set("agent_8_observability", gate("run", "substantial diff — skip the observability half if the project logs nothing"))
			}
		} else {
			set("agent_7_structural", gate("skip", r))
			set("agent_8_observability", gate("skip", "not a substantial diff (#7's threshold)"))
		}
		if a.behavioralGoal {
			set("agent_9_intent", gate("run", "reviewable intent established at Step 4b"))
		} else {
			set("agent_9_intent", gate("skip", "no statable behavioral goal — nothing to reconcile against"))
		}
		ok, why := githubReachable()
		set("agent_10_prior_feedback", gate(map[bool]string{true: "run", false: "skip"}[ok], why))
		if a.specArtifact {
			set("agent_11_spec", gate("run", "written spec artifact captured verbatim at Step 4b"))
		} else {
			set("agent_11_spec", gate("skip", "no written spec artifact — never synthesise one from the diff"))
		}
	}

	// --- post-loop gates
	switch {
	case a.runtimeChange && hasPR:
		set("evidence_gate", gate("run", "runtime behavior changes and a PR exists to attach evidence to"))
	case !a.runtimeChange:
		set("evidence_gate", gate("skip", "diff changes no runtime functionality"))
	default:
		set("evidence_gate", gate("skip", "no PR to attach evidence to — deferred, not dropped (Step 0c)"))
	}
	if a.runtimeChange {
		set("measurement_gate", gate("run", "user-facing behavior may change — waive in the report if the repo cannot measure"))
	} else {
		set("measurement_gate", gate("skip", "no user-facing behavior change"))
	}

	inputs := obj{
		{"logic", a.logic},
		{"behavioral_goal", a.behavioralGoal},
		{"runtime_behavior_change", a.runtimeChange},
		{"attacker_reachable", a.attackerReachable},
		{"spec_artifact", a.specArtifact},
	}
	// Recorded only when asked, so the run record shows why a floor was lowered and a plan
	// without it is byte for byte what it was. report.Risk reads its three keys by name.
	if a.deltaLogicGiven {
		inputs = append(inputs, py.Field{Key: "delta_logic", Val: a.deltaLogic})
	}
	return obj{
		{"tier_floor", tier},
		{"tier_reason", tierReason},
		// The loop's only bound, in cumulative agent invocations: printed because a cap
		// nothing tells the orchestrator about bounds nothing.
		{"agent_cap", record.DefaultAgentCap},
		{"changed_lines", c.changed},
		{"semantic_lines", c.semantic},
		{"sizing_excluded", c.excluded},
		{"base_branch", c.base},
		{"inputs", inputs},
		{"gates", gates},
	}
}

// truthy is Python's bool() over a decoded JSON value.
func truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case float64:
		return x != 0
	case string:
		return x != ""
	case []any:
		return len(x) > 0
	case map[string]any:
		return len(x) > 0
	}
	return true
}

// length is len(tm.get(k) or []) for a list; the report's lists are always lists.
func length(v any) int {
	l, _ := v.([]any)
	return len(l)
}

// yesno is plan.py's argparse type: yes, y, true or 1, any case, is yes; anything else is no.
//
// strict refuses anything that is neither a yes nor a no/n/false/0, as a usage error. It is
// for --delta-logic, whose "no" LOWERS the floor: read plan.py's way, a typo such as `yse`
// would be a "no" and floor a re-entry at fast. The flags plan.py had keep its reading.
type yesno struct {
	v      *bool
	set    bool
	strict bool
}

func (y *yesno) String() string { return "" }
func (y *yesno) Set(s string) error {
	switch strings.ToLower(s) {
	case "yes", "y", "true", "1":
		*y.v = true
	case "no", "n", "false", "0":
		*y.v = false
	default:
		if y.strict {
			return fmt.Errorf("want yes or no, not %q", s)
		}
		*y.v = false
	}
	y.set = true
	return nil
}

// Command is `looper plan`. Exit 0 with the plan (and, recorded, its run_id last); 2 for a
// usage error; 1 when the context cannot be read or the record write fails, with the
// record's own exit status in the second case.
func Command(args []string, in io.Reader, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("plan", flag.ContinueOnError)
	// Help to stdout, as argparse printed it; a usage error to stderr.
	fs.SetOutput(io.Discard)
	var a answers
	ctxPath := fs.String("context", "", "the JSON file looper context wrote (default: stdin)")
	model := fs.String("model", "", "the ORCHESTRATOR's model id — the only model that varies across runs")
	dry := fs.Bool("dry-run", false, "print the plan without recording it")
	required := map[string]*yesno{}
	var deltaLogic *yesno
	for _, f := range []struct {
		name string
		v    *bool
		help string
	}{
		{"logic", &a.logic, "yes|no: does the diff change program logic?"},
		{"behavioral-goal", &a.behavioralGoal, "yes|no: is there a statable behavioral goal? (gates #9)"},
		{"runtime-change", &a.runtimeChange, "yes|no: does runtime behavior change? (gates the evidence + measurement gates)"},
		{"attacker-reachable", &a.attackerReachable, "yes|no: is any changed path attacker-reachable?"},
		{"spec-artifact", &a.specArtifact, "yes|no: did Step 4b capture a written spec artifact verbatim? (gates #11)"},
		{"delta-logic", &a.deltaLogic, "yes|no: does the delta since last_reviewed_sha change program logic? (the fast-path re-entry; unasked, no floor is lowered)"},
	} {
		y := &yesno{v: f.v}
		fs.Var(y, f.name, f.help)
		if f.name != "spec-artifact" && f.name != "delta-logic" {
			required[f.name] = y
		}
		if f.name == "delta-logic" {
			y.strict = true
			deltaLogic = y
		}
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fs.SetOutput(out)
			fs.Usage()
			return 0
		}
		fmt.Fprintf(errOut, "looper plan: %v\n", err)
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(errOut, "looper plan: unrecognized arguments: %s\n", strings.Join(fs.Args(), " "))
		return 2
	}
	var missing []string
	modelSet := false
	fs.Visit(func(f *flag.Flag) { modelSet = modelSet || f.Name == "model" })
	if !modelSet {
		missing = append(missing, "--model")
	}
	for _, n := range []string{"logic", "behavioral-goal", "runtime-change", "attacker-reachable"} {
		if !required[n].set {
			missing = append(missing, "--"+n)
		}
	}
	if len(missing) > 0 {
		fmt.Fprintf(errOut, "looper plan: the following arguments are required: %s\n", strings.Join(missing, ", "))
		return 2
	}
	a.deltaLogicGiven = deltaLogic.set

	var raw []byte
	var err error
	if *ctxPath != "" {
		if raw, err = os.ReadFile(*ctxPath); err != nil {
			fmt.Fprintf(errOut, "looper plan: cannot read the context file (%v). Run looper context first "+
				"and point --context at what it wrote.\n", err)
			return 1
		}
	} else if raw, err = io.ReadAll(in); err != nil {
		fmt.Fprintf(errOut, "looper plan: cannot read the context from stdin: %v\n", err)
		return 1
	}
	c, err := readContext(raw)
	if err != nil {
		fmt.Fprintf(errOut, "looper plan: %v\n", err)
		return 1
	}

	plan := build(c, a)
	if _, err := io.WriteString(out, py.Encode(plan)); err != nil {
		return 1
	}

	// Repeat count beats calendar: surface a gate that keeps not completing at the one moment
	// the skill is already open.
	var ao, ae bytes.Buffer
	var alarm string
	if rc := stats.Command([]string{"--alarm"}, &ao, &ae); rc != 0 {
		// An empty alarm is also what a working alarm prints over a healthy record, so
		// staying quiet made "nothing is repeating" and "nothing could be read" the same
		// output — and an unreadable store is exactly when the alarm matters. Carry on: the
		// plan does not depend on it.
		why := py.Strip(ae.String())
		if why == "" {
			why = fmt.Sprintf("looper stats exited %d", rc)
		}
		alarm = "review-loop alarm unavailable: " + why
	} else {
		alarm = py.Strip(ao.String())
	}
	if alarm != "" {
		fmt.Fprint(errOut, "\n"+alarm+"\n")
		// stderr reaches only a human watching this run; the unattended runs are the ones
		// most likely to accumulate repeated failures, so route it as unresolved asks are.
		if os.Getenv("AO_SESSION_ID") != "" {
			run(ghTimeout, "ao", "report", "--checkpoint", "--note", alarm)
		}
	}
	if *dry {
		return 0
	}

	var gates, inputs value
	for _, f := range plan {
		switch f.Key {
		case "gates":
			gates = f.Val
		case "inputs":
			inputs = f.Val
		}
	}
	var ro, re bytes.Buffer
	rc := record.Command([]string{"plan",
		"--tier", plan[0].Val.(string),
		"--model", *model,
		"--base", orEmpty(c.base),
		"--changed-lines", strconv.Itoa(c.changed),
		"--semantic-lines", strconv.Itoa(c.semantic),
		"--sizing-excluded", orEmpty(c.excluded),
		"--tier-reason", plan[1].Val.(string),
		"--agent-cap", strconv.Itoa(record.DefaultAgentCap),
		"--inputs", py.Encode(inputs),
		"--gates", py.Encode(gates),
	}, &ro, &re)
	rstrip := func(s string) string { return strings.TrimRightFunc(s, record.PySpace) }
	if s := rstrip(re.String()); s != "" {
		fmt.Fprintln(errOut, s)
	}
	if s := rstrip(ro.String()); s != "" {
		fmt.Fprintln(out, s)
	}
	if rc != 0 {
		// The plan JSON is already on stdout and looks complete. Say plainly that it was not
		// recorded, or the orchestrator proceeds with no run_id and no Stop-hook net under it.
		fmt.Fprintln(errOut, "looper plan: COMPUTED BUT NOT RECORDED — no run_id exists, the Stop "+
			"hook will not track this run. Fix the error above and re-run looper plan before "+
			"starting the review.")
	}
	return rc
}
