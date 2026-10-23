package plan

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/pinepeakdigital/looper/internal/golden"
	"github.com/pinepeakdigital/looper/internal/home"
	"github.com/pinepeakdigital/looper/internal/record"
)

// The parity gate: every scenario ran through plan.py (narthur/skills 791c4d1), and runs
// through `looper plan` here, over the same fixture repo, the same stub gh and ao, the same
// ledger and the same run store. Its exit status, stdout, stderr, what ao was asked and the
// plan row it recorded are frozen under testdata/oracle (internal/golden says what a golden is).
// plan.py ran looper's threat-model, stats and record by path; at v0.2.6 those were the code
// this package calls in process, so the goldens hold their answers too.

// scenario is one case: the context.sh JSON, plan's flags, and what the world around it says.
type scenario struct {
	name   string
	ctx    string   // context.sh's JSON, on stdin
	args   []string // after --model m; nil means the defaults below
	gh     string   // the stub gh's body; "" is the default
	remote string   // origin's URL; "" is the GitHub default, "none" is no remote
	ledger string   // the threat model; "" is none, "dir" a directory in its place. {head}/{base} are shas
	store  string   // "" no store yet, "alarm" three declined threat models, "dir" unreadable
	ao     bool     // run under AO_SESSION_ID, with a stub ao
	record bool     // without --dry-run: write the plan row
}

const ctxDefault = `{"changed_lines": 40, "semantic_lines": 40, "sizing_excluded": null, ` +
	`"fast_path_eligible_by_size": false, "base_branch": "main", "learnings_entries": 10, ` +
	`"learnings_compaction_due": false, "learnings_swept_entries": null}`

var argsDefault = []string{"--logic", "yes", "--behavioral-goal", "yes", "--runtime-change", "yes",
	"--attacker-reachable", "no"}

// ghDefault authenticates, finds no PR for the branch, and finds PR history in the repo.
const ghDefault = `case "$1 $2" in
"auth status") exit 0 ;;
"pr view") echo "no pull requests found for branch" >&2; exit 1 ;;
"pr list") echo '[{"number":1}]' ;;
esac`

func ctx(over string) string {
	var m map[string]any
	json.Unmarshal([]byte(ctxDefault), &m)
	var o map[string]any
	if err := json.Unmarshal([]byte(over), &o); err != nil {
		panic(err)
	}
	for k, v := range o {
		if v == "DELETE" {
			delete(m, k)
		} else {
			m[k] = v
		}
	}
	b, _ := json.Marshal(m)
	return string(b)
}

func with(over ...string) []string {
	out := append([]string{}, argsDefault...)
	for i := 0; i < len(over); i += 2 {
		replaced := false
		for j := 0; j < len(out); j += 2 {
			if out[j] == over[i] {
				out[j+1], replaced = over[i+1], true
			}
		}
		if !replaced {
			out = append(out, over[i], over[i+1])
		}
	}
	return out
}

var scenarios = []scenario{
	{name: "full, a big file decides 7 and 8", ctx: ctxDefault},
	{name: "fast path", ctx: ctx(`{"fast_path_eligible_by_size": true, "changed_lines": 12, "semantic_lines": 12}`), args: with("--logic", "no")},
	{name: "fast path with exclusions", ctx: ctx(`{"fast_path_eligible_by_size": true, "changed_lines": 400, "semantic_lines": 12, "sizing_excluded": "package-lock.json (388)"}`), args: with("--logic", "no")},
	{name: "full by size with exclusions", ctx: ctx(`{"changed_lines": 900, "semantic_lines": 200, "sizing_excluded": "go.sum (700)"}`), args: with("--logic", "no")},
	{name: "small but logic", ctx: ctx(`{"fast_path_eligible_by_size": true, "changed_lines": 12, "semantic_lines": 12}`)},
	{name: "no semantic count falls back to raw", ctx: ctx(`{"semantic_lines": "DELETE", "changed_lines": 77}`)},
	{name: "null changed lines", ctx: ctx(`{"changed_lines": null, "semantic_lines": null}`)},
	{name: "over the structural floor skips the scan", ctx: ctx(`{"changed_lines": 150, "semantic_lines": 150}`)},
	{name: "no base, so no scan", ctx: ctx(`{"base_branch": null}`)},
	{name: "empty sizing exclusion is none", ctx: ctx(`{"sizing_excluded": "", "changed_lines": 300, "semantic_lines": 300}`)},
	{name: "sweep due", ctx: ctx(`{"learnings_entries": 45, "learnings_compaction_due": true}`)},
	{name: "sweep not regrown", ctx: ctx(`{"learnings_entries": 45, "learnings_swept_entries": 41}`)},
	{name: "sweep never swept", ctx: ctx(`{"learnings_entries": 45}`)},
	{name: "no learnings fields at all", ctx: `{"changed_lines": 10, "fast_path_eligible_by_size": true, "base_branch": ""}`, args: with("--logic", "no")},
	{name: "yes and no spellings", ctx: ctxDefault, args: with("--logic", "Y", "--behavioral-goal", "TRUE", "--runtime-change", "1", "--attacker-reachable", "nah", "--spec-artifact", "yes")},
	{name: "every answer no", ctx: ctxDefault, args: with("--logic", "no", "--behavioral-goal", "no", "--runtime-change", "No", "--attacker-reachable", "false", "--spec-artifact", "0")},
	{name: "flags as equals and repeated", ctx: ctxDefault, args: []string{"--logic=no", "--logic=yes", "--behavioral-goal=y", "--runtime-change=n", "--attacker-reachable=1"}},
	{name: "a PR exists", ctx: ctxDefault, gh: `case "$1 $2" in "auth status") exit 0 ;; "pr view") echo 17 ;; "pr list") echo '[{"number":1}]' ;; esac`},
	{name: "a PR view that is not a number", ctx: ctxDefault, gh: `case "$1 $2" in "auth status") exit 0 ;; "pr view") echo abc ;; "pr list") echo '[{"number":1}]' ;; esac`},
	// gh's own logged-out sentence on stderr. The golden was captured with a stub that printed
	// nothing; plan.py discarded both streams of this call (`rc, _, _`, as the port did), so
	// its answer is the same for either stub. looper now reads the stream, and says "not
	// authenticated" only when gh says so — TestAFailedAuthProbeSaysWhatItSaw has the rest.
	{name: "gh unauthenticated", ctx: ctxDefault, gh: `case "$1 $2" in "auth status") echo 'You are not logged into any GitHub hosts. To log in, run: gh auth login' >&2; exit 1 ;; *) exit 1 ;; esac`},
	{name: "no PRs at all", ctx: ctxDefault, gh: `case "$1 $2" in "auth status") exit 0 ;; "pr view") exit 1 ;; "pr list") echo '[]' ;; esac`},
	{name: "PR list fails with a message", ctx: ctxDefault, gh: `case "$1 $2" in "auth status") exit 0 ;; "pr view") exit 1 ;; "pr list") printf '  \nHTTP 502: bad gateway\nsecond line\n' >&2; exit 1 ;; esac`},
	{name: "PR list fails silently", ctx: ctxDefault, gh: `case "$1 $2" in "auth status") exit 0 ;; "pr view") exit 1 ;; "pr list") exit 4 ;; esac`},
	{name: "PR list killed by a signal", ctx: ctxDefault, gh: `case "$1 $2" in "auth status") exit 0 ;; "pr view") exit 1 ;; "pr list") kill -TERM $$ ;; esac`},
	{name: "PR list prints nothing", ctx: ctxDefault, gh: `case "$1 $2" in "auth status") exit 0 ;; "pr view") exit 1 ;; "pr list") echo ;; esac`},
	{name: "not a github remote", ctx: ctxDefault, remote: "https://gitlab.com/o/r.git"},
	{name: "no remote", ctx: ctxDefault, remote: "none"},
	{name: "fast path skips the github probe", ctx: ctx(`{"fast_path_eligible_by_size": true}`), args: with("--logic", "no"), remote: "none"},
	{name: "threat model stale", ctx: ctxDefault, ledger: "- OBSERVED: big. [big.txt @ {base}]\n"},
	{name: "threat model broken pin", ctx: ctxDefault, ledger: "- OBSERVED: x. [big.txt @ 0000000]\n"},
	{name: "threat model stale and broken", ctx: ctxDefault, ledger: "- OBSERVED: big. [big.txt @ {base}]\n- OBSERVED: x. [a.txt @ 0000000]\n"},
	{name: "threat model clean, attacker reachable", ctx: ctxDefault, args: with("--attacker-reachable", "yes"), ledger: "- OBSERVED: big. [big.txt @ {head}]\n"},
	{name: "threat model clean", ctx: ctxDefault, ledger: "- OBSERVED: big. [big.txt @ {head}]\n"},
	{name: "threat model unreadable", ctx: ctxDefault, ledger: "dir"},
	{name: "threat model NUL pin", ctx: ctxDefault, ledger: "- OBSERVED: x. [a\x00b.txt @ {head}]\n"},
	{name: "alarm", ctx: ctxDefault, store: "alarm"},
	{name: "alarm unavailable", ctx: ctxDefault, store: "dir"},
	{name: "alarm under AO", ctx: ctxDefault, store: "alarm", ao: true},
	{name: "alarm unavailable under AO", ctx: ctxDefault, store: "dir", ao: true},
	{name: "AO with nothing to say", ctx: ctxDefault, ao: true},
	{name: "recorded", ctx: ctxDefault, record: true},
	{name: "recorded fast", ctx: ctx(`{"fast_path_eligible_by_size": true, "sizing_excluded": "yarn.lock (40)"}`), args: with("--logic", "no"), record: true},
	{name: "recorded with alarm", ctx: ctx(`{"base_branch": null}`), store: "alarm", record: true},
}

// gitEnv makes git deterministic and blind to this machine, as the ledger fixtures do, and
// fixes what the record reads from the session's environment.
func gitEnv(t *testing.T, base string) {
	t.Helper()
	for _, k := range []string{"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_COMMON_DIR",
		"GIT_OBJECT_DIRECTORY", "GIT_NAMESPACE", "GIT_CONFIG", "GIT_CONFIG_PARAMETERS",
		"GIT_CONFIG_COUNT", "AO_SESSION_ID", "CLAUDE_REVIEW_LOOP_SUBAGENT",
		"CLAUDE_CODE_SESSION_ATTENDED", "REVIEW_LOOP_TAIL"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	for k, v := range map[string]string{"GIT_CONFIG_GLOBAL": os.DevNull, "GIT_CONFIG_NOSYSTEM": "1",
		"GIT_AUTHOR_NAME": "t", "GIT_AUTHOR_EMAIL": "t@t", "GIT_COMMITTER_NAME": "t",
		"GIT_COMMITTER_EMAIL": "t@t", "GIT_AUTHOR_DATE": "2026-01-01T00:00:00Z",
		"GIT_COMMITTER_DATE": "2026-01-01T00:00:00Z", "CLAUDE_CODE_SESSION_ID": "session"} {
		t.Setenv(k, v)
	}
	ceil := base
	if real, err := filepath.EvalSymlinks(base); err == nil && real != base {
		ceil += string(os.PathListSeparator) + real
	}
	t.Setenv("GIT_CEILING_DIRECTORIES", ceil)
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func write(t *testing.T, p, body string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
}

// world is one scenario's surroundings: the repo to run in, the env to run with, and where
// the stub ao writes what it was asked.
type world struct {
	base, repo, store, aoLog string
	env                      map[string]string
}

// build makes the scenario's world. The repo: main has a.txt and gone.txt; origin/main points
// there; the branch adds big.txt (900 lines, the last unterminated), a name with a space and
// a non-ASCII name, and deletes gone.txt — so the scan reads a big file, counts an unterminated
// last line, survives a deleted path and a literal name.
func (sc scenario) build(t *testing.T) world {
	t.Helper()
	base := t.TempDir()
	gitEnv(t, base)
	w := world{base: base, repo: filepath.Join(base, "r"), store: filepath.Join(base, "runs.jsonl"),
		aoLog: filepath.Join(base, "ao.log"), env: map[string]string{}}
	git(t, base, "init", "-q", "-b", "main", w.repo)
	write(t, filepath.Join(w.repo, "a.txt"), "a\n", 0o644)
	write(t, filepath.Join(w.repo, "gone.txt"), strings.Repeat("g\n", 2000), 0o644)
	git(t, w.repo, "add", "-A")
	git(t, w.repo, "commit", "-q", "-m", "base")
	baseSha := git(t, w.repo, "rev-parse", "--short=7", "HEAD")
	git(t, w.repo, "update-ref", "refs/remotes/origin/main", "HEAD")
	git(t, w.repo, "checkout", "-q", "-b", "feature")
	write(t, filepath.Join(w.repo, "big.txt"), strings.TrimSuffix(strings.Repeat("x\n", 900), "\n"), 0o644)
	write(t, filepath.Join(w.repo, "with space.txt"), "1\n2\n3\n", 0o644)
	write(t, filepath.Join(w.repo, "naïve.txt"), "1\n", 0o644)
	if err := os.Remove(filepath.Join(w.repo, "gone.txt")); err != nil {
		t.Fatal(err)
	}
	git(t, w.repo, "add", "-A")
	git(t, w.repo, "commit", "-q", "-m", "feature")
	head := git(t, w.repo, "rev-parse", "--short=7", "HEAD")
	switch sc.remote {
	case "none":
	case "":
		git(t, w.repo, "remote", "add", "origin", "git@github.com:o/r.git")
	default:
		git(t, w.repo, "remote", "add", "origin", sc.remote)
	}

	// The repo's store, under the $REVIEW_LOOP_HOME TestMain set, which the looper subprocess
	// inherits. Not <git-common-dir>/info: a directory there would read as nothing to move in.
	rd, err := home.RepoDir(w.repo)
	if err != nil {
		t.Fatal(err)
	}
	tm := filepath.Join(rd, home.ThreatModel)
	switch sc.ledger {
	case "":
	case "dir":
		if err := os.MkdirAll(tm, 0o755); err != nil {
			t.Fatal(err)
		}
	default:
		write(t, tm, strings.NewReplacer("{head}", head, "{base}", baseSha).Replace(sc.ledger), 0o644)
	}

	bin := filepath.Join(base, "bin")
	gh := sc.gh
	if gh == "" {
		gh = ghDefault
	}
	write(t, filepath.Join(bin, "gh"), "#!/bin/sh\n"+gh+"\n", 0o755)
	write(t, filepath.Join(bin, "ao"), "#!/bin/sh\nfor a in \"$@\"; do printf '%s\\n' \"$a\"; done >> '"+w.aoLog+"'\n", 0o755)
	w.env["PATH"] = bin + string(os.PathListSeparator) + os.Getenv("PATH")
	w.env["REVIEW_LOOP_RUNS"] = w.store
	if sc.ao {
		w.env["AO_SESSION_ID"] = "s1"
	}

	switch sc.store {
	case "dir":
		if err := os.Mkdir(w.store, 0o755); err != nil {
			t.Fatal(err)
		}
	case "alarm":
		// Three runs that each declined a planned threat model: the Step 0 alarm's repeat count.
		// From the fixture repo, so the rows name it rather than wherever the test ran.
		t.Setenv("REVIEW_LOOP_RUNS", w.store)
		t.Chdir(w.repo)
		for range 3 {
			var o, e bytes.Buffer
			if record.Command([]string{"plan", "--tier", "full", "--model", "m",
				"--gates", `{"threat_model":{"planned":"run","reason":"2 stale claims"}}`}, &o, &e) != 0 {
				t.Fatalf("seeding the store: %s", e.String())
			}
			id := strings.TrimSpace(o.String())
			if record.Command([]string{"finish", "--run-id", id, "--outcome", "clean",
				"--tier", "full", "--executed", `{"threat_model":{"status":"skipped","reason":"gh unauthenticated"}}`},
				&o, &e) != 0 {
				t.Fatalf("seeding the store: %s", e.String())
			}
		}
	}
	return w
}

func (sc scenario) argv() []string {
	a := sc.args
	if a == nil {
		a = argsDefault
	}
	out := append([]string{"--model", "m"}, a...)
	if !sc.record {
		out = append(out, "--dry-run")
	}
	return out
}

var runID = regexp.MustCompile(`(?m)^[0-9a-f]{12}$`)

// result is what a scenario produced: the transcript, then what ao was asked and the newest
// plan row, each normalised.
func (w world) result(t *testing.T, code int, out, errOut string) string {
	t.Helper()
	norm := func(s string) string {
		if d, err := home.RepoDir(w.repo); err == nil {
			s = strings.ReplaceAll(s, d, "<store>") // its name carries a hash of the temp path
		}
		if real, err := filepath.EvalSymlinks(w.base); err == nil && real != w.base {
			s = strings.ReplaceAll(s, real, "<tmp>")
		}
		return runID.ReplaceAllString(strings.ReplaceAll(s, w.base, "<tmp>"), "<run_id>")
	}
	ao, _ := os.ReadFile(w.aoLog)
	return "exit=" + strconv.Itoa(code) + "\n--- stdout ---\n" + norm(out) + "--- stderr ---\n" + norm(errOut) +
		"--- ao ---\n" + norm(string(ao)) + "--- plan row ---\n" + norm(planRow(t, w.store))
}

// planRow is the last plan row in the store, its run_id and timestamps replaced: they differ
// between any two runs, and every other field is what plan passed to record plan.
func planRow(t *testing.T, store string) string {
	t.Helper()
	b, err := os.ReadFile(store)
	if err != nil {
		return ""
	}
	var row map[string]any
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var r map[string]any
		if json.Unmarshal([]byte(l), &r) == nil && r["phase"] == "plan" {
			row = r
		}
	}
	if row == nil {
		return ""
	}
	for k := range row {
		if k == "run_id" || strings.HasSuffix(k, "_at") || k == "ts" {
			row[k] = "<" + k + ">"
		}
	}
	keys := make([]string, 0, len(row))
	for k := range row {
		keys = append(keys, k)
	}
	var sb strings.Builder
	sort.Strings(keys)
	for _, k := range keys {
		v, _ := json.Marshal(row[k])
		sb.WriteString(k + "=" + string(v) + "\n")
	}
	return sb.String()
}

func (w world) apply(t *testing.T) {
	for k, v := range w.env {
		t.Setenv(k, v)
	}
	t.Chdir(w.repo)
}

func TestPlanMatchesThePython(t *testing.T) {
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			want := golden.Read(t, "transcript")
			w := sc.build(t)
			w.apply(t)
			var o, e bytes.Buffer
			code := Command(sc.argv(), strings.NewReader(sc.ctx), &o, &e)
			if got := w.result(t, code, o.String(), e.String()); got != want {
				t.Errorf("looper plan no longer does what plan.py did\n--- python (frozen) ---\n%s\n--- looper ---\n%s", want, got)
			}
		})
	}
}
