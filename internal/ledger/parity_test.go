package ledger

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/pinepeakdigital/looper/internal/golden"
	"github.com/pinepeakdigital/looper/internal/home"
)

// The parity gate: every scenario ran through threat-model.py or deferred.py, and runs through
// ThreatModel or Deferred, against the same fixture repo, ledger and working directory, and must
// leave the same exit status, stdout and stderr. The Python's side is frozen under
// testdata/oracle (internal/golden says what a golden is and is not).
//
// The fixture repo's commits are DETERMINISTIC — fixed identity, fixed author and committer
// dates, no global or system git config — so every sha in a ledger and in a `commits` list is
// the same on every run and every machine, and the goldens hold real shas. The normalisation is
// the store dir, which `path` always names and which differs per run (it is named by a hash of
// the fixture's temp path): it becomes <store>. The fixture's temp directory, in both spellings
// (as given and with symlinks resolved; macOS's /var is /private/var), becomes <tmp> as a
// fallback for any output that carries it; no golden does today.

type scenario struct {
	name    string
	cmd     string // "threat-model" or "deferred"
	ledger  string // {c1} is commit 1's full sha, {c1s} its first seven; {blob} is a blob's
	missing bool   // no ledger file at all
	cwd     string // relative to the fixture: "" is the repo root; "wt" a linked worktree; "out" no repo
}

// commits is the fixture history, oldest first. Each touches what its message says, so a pin
// on a file is stale exactly when a later commit here touched that file.
var commits = []struct {
	msg   string
	files map[string]string // "" deletes
}{
	{"base", map[string]string{
		"src/app.ts":       "line 1\nline 2\nline 3\nline 4\n",
		"src/auth.ts":      "export const token = 1\n",
		"src/db.ts":        "export const db = {}\n",
		"src/types.ts":     "export type Sketch = { draw(): void }\n",
		"src/unrelated.ts": "other\n",
		"src/naïve.ts":     "naïve\n",
		"docs/old.md":      "old\n",
	}},
	{"unrelated change", map[string]string{"src/unrelated.ts": "other2\n"}},
	{"allow async draws", map[string]string{"src/types.ts": "export type Sketch = { draw(): void | Promise<void> }\n"}},
	{"rework auth — π", map[string]string{"src/auth.ts": "export const token = 2\n"}},
	{"drop the old doc", map[string]string{"docs/old.md": ""}},
	{"rework auth again", map[string]string{"src/auth.ts": "export const token = 3\n"}},
	{"touch naïve", map[string]string{"src/naïve.ts": "naïve 2\n"}},
	// Not UTF-8. git re-encodes such a subject as Latin-1 on the way out, so both sides read ÿ.
	{"latin \xff subject", map[string]string{"src/latin.ts": "x\n"}},
}

// gitEnv makes git deterministic and blind to this machine: no global or system config (a
// global hook, gpgsign or log.decorate would change what runs or what prints), a fixed identity,
// and discovery that cannot climb out of the fixture into some enclosing repo.
func gitEnv(t *testing.T, base string) {
	t.Helper()
	for _, k := range []string{"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_COMMON_DIR",
		"GIT_OBJECT_DIRECTORY", "GIT_NAMESPACE", "GIT_CONFIG", "GIT_CONFIG_PARAMETERS",
		"GIT_CONFIG_COUNT"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_AUTHOR_NAME", "t")
	t.Setenv("GIT_AUTHOR_EMAIL", "t@t")
	t.Setenv("GIT_COMMITTER_NAME", "t")
	t.Setenv("GIT_COMMITTER_EMAIL", "t@t")
	ceil := base
	if real, err := filepath.EvalSymlinks(base); err == nil && real != base {
		ceil += string(os.PathListSeparator) + real
	}
	t.Setenv("GIT_CEILING_DIRECTORIES", ceil)
}

func runGit(t *testing.T, dir string, env []string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// fixture is a repo at <base>/r with the history above, and the placeholder values for it.
type fixture struct {
	base, root string
	subst      *strings.Replacer
}

// history is the fixture history, built once per test binary and copied into each case's own
// directory: building it is two dozen git processes, and the parity table and every mutation
// that runs it would pay that per case. A copy holds the same objects, so the same shas.
var history struct {
	sync.Once
	root  string
	pairs []string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	base := t.TempDir()
	gitEnv(t, base)
	history.Do(func() {
		dir, err := os.MkdirTemp("", "ledger-history-")
		if err != nil {
			t.Fatal(err)
		}
		history.pairs = buildHistory(t, dir)
		history.root = filepath.Join(dir, "r")
	})
	if history.pairs == nil {
		t.Fatal("the fixture history could not be built (see the first case's failure)")
	}
	root := filepath.Join(base, "r")
	if err := os.CopyFS(root, os.DirFS(history.root)); err != nil {
		t.Fatal(err)
	}
	return &fixture{base: base, root: root, subst: strings.NewReplacer(history.pairs...)}
}

func TestMain(m *testing.M) {
	// Off the real ~/.claude/review-loop: the store under test lands in this temp dir.
	h, err := os.MkdirTemp("", "looper-home-")
	if err != nil {
		panic(err)
	}
	os.Setenv("REVIEW_LOOP_HOME", h)
	code := m.Run()
	os.RemoveAll(h)
	if history.root != "" {
		os.RemoveAll(filepath.Dir(history.root)) // the MkdirTemp above, and nothing else
	}
	os.Exit(code)
}

// buildHistory commits the history at <dir>/r with fixed dates and returns the placeholder
// pairs for it.
func buildHistory(t *testing.T, dir string) []string {
	root := filepath.Join(dir, "r")
	runGit(t, dir, nil, "init", "-q", "-b", "main", root)
	var pairs []string
	for i, c := range commits {
		for name, body := range c.files {
			p := filepath.Join(root, filepath.FromSlash(name))
			if body == "" {
				if err := os.Remove(p); err != nil {
					t.Fatal(err)
				}
				continue
			}
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		date := fmt.Sprintf("2026-01-%02dT12:00:00+00:00", i+1)
		env := []string{"GIT_AUTHOR_DATE=" + date, "GIT_COMMITTER_DATE=" + date}
		runGit(t, root, env, "add", "-A")
		runGit(t, root, env, "commit", "-q", "-m", c.msg)
		sha := runGit(t, root, nil, "rev-parse", "HEAD")
		n := strconv.Itoa(i + 1)
		pairs = append(pairs, "{c"+n+"}", sha, "{c"+n+"s}", sha[:7])
	}
	pairs = append(pairs, "{blob}", runGit(t, root, nil, "rev-parse", "HEAD:src/app.ts"))
	return pairs
}

func ledgerName(cmd string) string {
	if cmd == "threat-model" {
		return ThreatModelFile
	}
	return DeferredFile
}

// prepare writes the scenario's ledger and returns the directory to run in.
func (f *fixture) prepare(t *testing.T, sc scenario) string {
	t.Helper()
	if !sc.missing {
		p := f.ledger(t, ledgerName(sc.cmd))
		if err := os.WriteFile(p, []byte(f.subst.Replace(sc.ledger)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	switch {
	case sc.cwd == "out":
		d := filepath.Join(f.base, "out")
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		return d
	case sc.cwd == "wt" || strings.HasPrefix(sc.cwd, "wt/"):
		runGit(t, f.root, nil, "worktree", "add", "-q", filepath.Join(f.base, "wt"))
		return filepath.Join(f.base, filepath.FromSlash(sc.cwd))
	}
	return filepath.Join(f.root, filepath.FromSlash(sc.cwd))
}

// ledger is where the repo's store keeps ledger name (home.RepoDir), with its dir created.
func (f *fixture) ledger(t *testing.T, name string) string {
	t.Helper()
	d, err := home.RepoDir(f.root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(d, name)
}

// normalise replaces the repo's store, whose name carries a hash of the temp path, with
// <store>, and the fixture's temp directory, in both spellings, with <tmp>.
func (f *fixture) normalise(s string) string {
	if d, err := home.RepoDir(f.root); err == nil {
		s = strings.ReplaceAll(s, d, "<store>")
	}
	if real, err := filepath.EvalSymlinks(f.base); err == nil && real != f.base {
		s = strings.ReplaceAll(s, real, "<tmp>")
	}
	return strings.ReplaceAll(s, f.base, "<tmp>")
}

func transcript(code int, out, errOut string) string {
	return "exit=" + strconv.Itoa(code) + "\n--- stdout ---\n" + out + "--- stderr ---\n" + errOut
}

func command(cmd string) func([]string, io.Writer, io.Writer) int {
	if cmd == "threat-model" {
		return ThreatModel
	}
	return Deferred
}

// runGo runs the scenario through the Go command, in its directory, and returns the
// normalised transcript.
func runGo(t *testing.T, sc scenario, args ...string) string {
	t.Helper()
	f := newFixture(t)
	dir := f.prepare(t, sc)
	t.Chdir(dir)
	var out, errb bytes.Buffer
	code := command(sc.cmd)(args, &out, &errb)
	return f.normalise(transcript(code, out.String(), errb.String()))
}

const tmCanonical = `# Threat model — fixture
<!-- Maintained by review-loop Step 2b. OBSERVED claims are cited and pinned; INFERRED are not. -->

## Architecture
- OBSERVED: routes authenticate in middleware. [src/app.ts:2 @ {c1s}]
- OBSERVED: token lifecycle. [src/auth.ts:1-1 @ {c1s}]
- OBSERVED: whole-file claim. [src/db.ts @ {c1}]
- OBSERVED: no citation, so it is really an inference.
- INFERRED: task text is user-authored and untrusted.
`

const dfCanonical = `# Deferred findings

- DEFERRED 2026-10-01 (run abc123abc123): async draws could overlap.
  Grounding: no async Sketch exists. [src/types.ts:1 @ {c1s}]
  Guard: none — would need a new test file.

- DEFERRED 2026-10-01 (run abc123abc123): no pin on this one.
  Grounding: nothing uses it.
`

var scenarios = []scenario{
	// ---- threat-model: where the ledger is, and whether it is there ---------------------
	{name: "tm outside a repo", cmd: "threat-model", missing: true, cwd: "out"},
	{name: "tm missing file", cmd: "threat-model", missing: true},
	{name: "tm empty file", cmd: "threat-model", ledger: ""},
	{name: "tm only INFERRED claims", cmd: "threat-model", ledger: "- INFERRED: a.\n- INFERRED: b.\n"},
	{name: "tm the canonical model", cmd: "threat-model", ledger: tmCanonical},
	{name: "tm the canonical model from a subdirectory", cmd: "threat-model", ledger: tmCanonical, cwd: "src"},
	{name: "tm the canonical model from a linked worktree", cmd: "threat-model", ledger: tmCanonical, cwd: "wt"},
	{name: "tm the canonical model from a worktree subdirectory", cmd: "threat-model", ledger: tmCanonical, cwd: "wt/src"},

	// ---- threat-model: staleness ----------------------------------------------------------
	{name: "tm a pin at HEAD is fresh", cmd: "threat-model", ledger: "- OBSERVED: x. [src/auth.ts:1 @ {c7}]\n"},
	{name: "tm an unrelated commit does not wake a pin", cmd: "threat-model", ledger: "- OBSERVED: x. [src/app.ts:1 @ {c1s}]\n"},
	{name: "tm a moved file is stale with its commit", cmd: "threat-model", ledger: "- OBSERVED: x. [src/types.ts:1 @ {c1s}]\n"},
	{name: "tm a file moved twice lists both commits newest first", cmd: "threat-model", ledger: "- OBSERVED: x. [src/auth.ts:1 @ {c2}]\n"},
	{name: "tm a pin after the first move sees only the second", cmd: "threat-model", ledger: "- OBSERVED: x. [src/auth.ts:1 @ {c4s}]\n"},
	{name: "tm a deleted file is stale", cmd: "threat-model", ledger: "- OBSERVED: x. [docs/old.md:1 @ {c1s}]\n"},
	{name: "tm a path that never existed is fresh", cmd: "threat-model", ledger: "- OBSERVED: x. [src/nope.ts:1 @ {c1s}]\n"},
	{name: "tm a non-ASCII path and commit subject", cmd: "threat-model", ledger: "- OBSERVED: naïve. [src/naïve.ts:1 @ {c1s}]\n- OBSERVED: π. [src/auth.ts @ {c3s}]\n"},
	{name: "tm a commit subject that is not UTF-8", cmd: "threat-model", ledger: "- OBSERVED: x. [src/latin.ts @ {c1s}]\n"},
	{name: "tm a directory pin", cmd: "threat-model", ledger: "- OBSERVED: entry points. [src/ @ {c1s}]\n"},

	// ---- threat-model: broken pins --------------------------------------------------------
	{name: "tm unresolvable shas are broken", cmd: "threat-model", ledger: "- OBSERVED: a. [src/app.ts:1 @ deadbeefcafe]\n- OBSERVED: b. [src/types.ts:1 @ 0000000]\n- OBSERVED: c. [src/app.ts:1 @ {c1s}]\n"},
	{name: "tm a blob sha is broken", cmd: "threat-model", ledger: "- OBSERVED: a. [src/app.ts:1 @ {blob}]\n"},
	{name: "tm a broken pin on a moved file is broken and not stale", cmd: "threat-model", ledger: "- OBSERVED: a. [src/types.ts:1 @ abcdef0123456]\n"},

	// ---- threat-model: the citation shape -------------------------------------------------
	{name: "tm ranges single lines and whole files", cmd: "threat-model", ledger: "- OBSERVED: a. [src/types.ts:1-4 @ {c1s}]\n- OBSERVED: b. [src/types.ts:3 @ {c1s}]\n- OBSERVED: c. [src/types.ts @ {c1s}]\n- OBSERVED: d. [src/types.ts:10-2 @ {c1s}]\n"},
	{name: "tm malformed ranges are not citations", cmd: "threat-model", ledger: "- OBSERVED: a. [src/types.ts:1- @ {c1s}]\n- OBSERVED: b. [src/types.ts:-3 @ {c1s}]\n- OBSERVED: c. [src/types.ts:1-2-3 @ {c1s}]\n- OBSERVED: d. [src/types.ts:x @ {c1s}]\n"},
	{name: "tm only the first pin on a line counts", cmd: "threat-model", ledger: "- OBSERVED: a. [src/auth.ts:1 @ {c1s}] and [src/types.ts:1 @ {c1s}]\n"},
	{name: "tm sha lengths and case", cmd: "threat-model", ledger: "- OBSERVED: six. [src/types.ts @ abcdef]\n- OBSERVED: upper. [src/types.ts @ ABCDEF0]\n- OBSERVED: forty-one. [src/types.ts @ {c1}a]\n- OBSERVED: forty. [src/types.ts @ {c1}]\n"},
	{name: "tm spacing around the at sign", cmd: "threat-model", ledger: "- OBSERVED: a. [src/types.ts:1@{c1s}]\n- OBSERVED: b. [src/types.ts:1   @\t{c1s}]\n- OBSERVED: c. [src/auth.ts :1 @ {c1s}]\n- OBSERVED: d. [src/types.ts:1 @ {c1s} ]\n"},
	{name: "tm a path may hold a bracket but no colon", cmd: "threat-model", ledger: "- OBSERVED: a. [[src/types.ts:1 @ {c1s}]\n- OBSERVED: b. [c:/src/types.ts:1 @ {c1s}]\n- OBSERVED: c. [src/a]pp.ts:1 @ {c1s}]\n"},
	{name: "tm Unicode digits and spaces", cmd: "threat-model", ledger: "- OBSERVED: a. [src/types.ts:\u0661\u0662 @ {c1s}]\n- OBSERVED: b. [src/types.ts:1\u00a0@\u3000{c1s}]\n- OBSERVED: c. [src/app\u2003.ts:1 @ {c1s}]\n- OBSERVED: d. [src/types.ts:\U0001e5f1 @ {c1s}]\n"},
	{name: "tm the marker needs its colon", cmd: "threat-model", ledger: "- OBSERVED — not counted.\n- observed: lower case.\n- note: OBSERVED: mid-line counts.\n- OBSERVED:\n"},
	{name: "tm a hard-wrapped claim", cmd: "threat-model", ledger: "- OBSERVED: all routes authenticate in shared middleware; handlers do\n  not re-check. [src/types.ts:2 @ {c1s}]\n- OBSERVED: wrapped and stale, the pin on\n  the third line. And another\n  line. [src/types.ts @ {c1s}]\n"},
	{name: "tm CRLF and other line breaks", cmd: "threat-model", ledger: "- OBSERVED: a.\r\n[src/types.ts @ {c1s}]\r- OBSERVED: b.\f[src/types.ts @ {c1s}]\u2028- OBSERVED: c.\x1c- OBSERVED: d.\x1f[src/auth.ts @ {c1s}]\u0085x\n"},
	{name: "tm a BOM and escapes in the path", cmd: "threat-model", ledger: "\ufeff- OBSERVED: a. [src/\"q\\b.ts @ 0000000]\n- OBSERVED: b. [src/\x7f\x01.ts @ 0000000]\n- OBSERVED: c. [src/\U0001F600.ts:1 @ 0000000]\n"},
	{name: "tm no trailing newline", cmd: "threat-model", ledger: "- OBSERVED: a. [src/types.ts @ {c1s}]"},
	{name: "tm duplicate pins are each reported", cmd: "threat-model", ledger: "- OBSERVED: a. [src/types.ts @ {c1s}]\n- OBSERVED: b. [src/types.ts @ {c1s}]\n"},
	{name: "tm arguments are ignored", cmd: "threat-model", ledger: tmCanonical},

	// ---- deferred: where the ledger is, and whether it is there ---------------------------
	{name: "df outside a repo", cmd: "deferred", missing: true, cwd: "out"},
	{name: "df missing file", cmd: "deferred", missing: true},
	{name: "df empty file", cmd: "deferred", ledger: ""},
	{name: "df a header and no entries", cmd: "deferred", ledger: "# Deferred findings\n\nnothing yet\n"},
	{name: "df the canonical list", cmd: "deferred", ledger: dfCanonical},
	{name: "df the canonical list from a subdirectory", cmd: "deferred", ledger: dfCanonical, cwd: "src"},
	{name: "df the canonical list from a linked worktree", cmd: "deferred", ledger: dfCanonical, cwd: "wt"},
	{name: "df the canonical list from a worktree subdirectory", cmd: "deferred", ledger: dfCanonical, cwd: "wt/src"},

	// ---- deferred: staleness and broken pins ----------------------------------------------
	{name: "df an untouched pin is quiet", cmd: "deferred", ledger: "- DEFERRED 2026-10-01: a.\n  Grounding: b. [src/app.ts:1 @ {c1s}]\n  Guard: none.\n"},
	{name: "df a moved file wakes it", cmd: "deferred", ledger: "- DEFERRED 2026-10-01: a.\n  Grounding: b. [src/types.ts:1 @ {c1s}]\n  Guard: none.\n"},
	{name: "df a file moved twice", cmd: "deferred", ledger: "- DEFERRED 2026-10-01: a.\n  Grounding: b. [src/auth.ts @ {c1}]\n  Guard: none.\n"},
	{name: "df broken pins", cmd: "deferred", ledger: "- DEFERRED 2026-10-01: a.\n  Grounding: b. [src/types.ts:1 @ 0000000]\n  Guard: none.\n- DEFERRED 2026-10-01: c.\n  Grounding: d. [src/app.ts:1 @ {blob}]\n  Guard: none.\n- DEFERRED 2026-10-01: e.\n  Grounding: f. [src/app.ts:1 @ {c1s}]\n  Guard: none.\n"},
	{name: "df the same pin in two entries", cmd: "deferred", ledger: "- DEFERRED x: a.\n  Grounding: [src/types.ts @ {c2s}]\n  Guard: none.\n- DEFERRED y: b.\n  Grounding: [src/types.ts @ {c2s}]\n  Guard: none.\n"},

	// ---- deferred: what belongs to an entry -----------------------------------------------
	{name: "df two entries one pinned", cmd: "deferred", ledger: "- DEFERRED 2026-10-01: first, pinned.\n  Grounding: nothing async. [src/types.ts:1 @ {c3s}]\n- DEFERRED 2026-10-01: second, not pinned.\n  Grounding: nothing uses it.\n"},
	{name: "df a footer citation is not swallowed", cmd: "deferred", ledger: "- DEFERRED 2026-10-01 (run abc123abc123): no pin on this one.\n  Grounding: nothing uses it.\n\n<!-- last reviewed [docs/old.md:1 @ {c1s}] -->\n"},
	{name: "df an unindented line is not a continuation", cmd: "deferred", ledger: "- DEFERRED x: a.\n  Grounding: b.\nfooter [src/types.ts:1 @ {c1s}]\n"},
	{name: "df a skipped line inside an entry shifts its pin line", cmd: "deferred", ledger: "- DEFERRED x: a.\n\n  Grounding: b. [src/types.ts:1 @ {c1s}]\nunindented\n  Guard: [src/auth.ts @ {c1s}]\n"},
	{name: "df several pins in one entry", cmd: "deferred", ledger: "- DEFERRED x: a. [src/types.ts:1-2 @ {c1s}]\n  Grounding: b. [src/types.ts:1 @ {c1s}] and [src/auth.ts:1 @ {c1s}]\n  and [docs/old.md @ {c1s}]\n  Guard: none.\n"},
	{name: "df a hard-wrapped entry names its first line", cmd: "deferred", ledger: "  - DEFERRED 2026-10-01 (run abc123abc123): two async Sketch draws could overlap and\n    corrupt the canvas mid-render.\n    Grounding: no async Sketch exists; `trails` is synchronous. [src/types.ts:14 @ {c1s}]\n    Guard: none — a type-level assertion would need a new test file, so not cheap.   \n"},
	{name: "df an indented entry is named stripped", cmd: "deferred", ledger: "   - DEFERRED x: indented, unpinned, unguarded.   \n\u3000- DEFERRED y: ideographic.\u00a0\n"},
	{name: "df an entry with no Guard line", cmd: "deferred", ledger: "- DEFERRED x: a.\n  Grounding: b. [src/app.ts:1 @ {c1s}]\n"},
	{name: "df Guard anywhere in the text counts", cmd: "deferred", ledger: "- DEFERRED x: a Guardian object.\n  Grounding: b.\n- DEFERRED y: c.\n  Grounding: d. Guard\n- DEFERRED z: e.\n  guard: lower case.\n"},
	{name: "df DEFERRED anywhere on a line starts an entry", cmd: "deferred", ledger: "UNDEFERRED\n  Grounding: [src/types.ts @ {c1s}]\nnote: DEFERRED until later\n- deferred lower case is not an entry\n  Guard: x\n"},
	{name: "df Unicode indentation and blank lines", cmd: "deferred", ledger: "- DEFERRED x: a.\n\t Grounding: tab. [src/auth.ts @ {c1s}]\n\u00a0 Guard: nbsp.\n\u3000\n\u2003[src/types.ts @ {c1s}]\n"},
	{name: "df CRLF and a form feed", cmd: "deferred", ledger: "- DEFERRED x: a.\r\n  Grounding: b. [src/types.ts @ {c1s}]\r\n  Guard: none.\f  [src/auth.ts @ {c1s}]\r\n"},
	{name: "df escapes in the named text", cmd: "deferred", ledger: "- DEFERRED \"q\" \\ \x7f π \U0001F600:\tx\n"},
	{name: "df an entry pinned only on its DEFERRED line", cmd: "deferred", ledger: "- DEFERRED x: a. [src/types.ts @ {c1s}]\n  Guard: none.\n"},
	{name: "df arguments are ignored", cmd: "deferred", ledger: dfCanonical},
}

// argsFor is what the "arguments are ignored" scenarios pass; every other scenario passes none.
func argsFor(sc scenario) []string {
	if strings.HasSuffix(sc.name, "arguments are ignored") {
		return []string{"--repo", "elsewhere", "-h", "--selftest"}
	}
	return nil
}

func TestLedgersMatchThePython(t *testing.T) {
	for _, sc := range scenarios {
		if _, ok := citationDivergences[sc.name]; ok {
			continue // TestCitationsAreReadPerClaim runs it, against the Python's answer and looper's
		}
		t.Run(sc.name, func(t *testing.T) {
			want := golden.Read(t, "transcript") // before runGo's Chdir: the golden path is relative
			if got := runGo(t, sc, argsFor(sc)...); got != want {
				t.Errorf("looper %s no longer does what %s.py did\n--- python (frozen) ---\n%s\n--- looper ---\n%s",
					sc.cmd, sc.cmd, want, got)
			}
		})
	}
}
