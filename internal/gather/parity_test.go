package gather

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/pinepeakdigital/looper/internal/golden"
)

// The parity gate: every scenario ran through context.sh (narthur/skills 865f93d) and runs
// through `looper context` here, over the same fixture repo, stub gh and review store. Exit
// status, stdout and stderr are frozen under testdata/oracle; the date is the one normalisation.

// world is a scenario's surroundings: a repo at <base>/r whose origin is a bare repo beside it,
// with main pushed, and the branch `feature` checked out one commit ahead.
type world struct {
	base, repo string
	shas       map[string]string // "main", "feature": full shas
}

type scenario struct {
	name   string
	gh     string                      // the stub gh's body; "" fails every call
	prep   func(t *testing.T, w world) // after the fixture is built, before running
	cwd    string                      // relative to the repo
	store  func(w world) string        // the reviewed-shas store's contents; nil is none
	files  map[string]string           // written at the repo root and committed on feature
	learn  string                      // the learnings file; "" is none
	onMain map[string]string           // committed on main, before the branch
}

func gitEnv(t *testing.T, base string) {
	t.Helper()
	for _, k := range []string{"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_COMMON_DIR",
		"GIT_OBJECT_DIRECTORY", "GIT_NAMESPACE", "GIT_CONFIG", "GIT_CONFIG_PARAMETERS", "GIT_CONFIG_COUNT"} {
		t.Setenv(k, "")
		if err := os.Unsetenv(k); err != nil {
			t.Fatal(err)
		}
	}
	for k, v := range map[string]string{"GIT_CONFIG_GLOBAL": os.DevNull, "GIT_CONFIG_NOSYSTEM": "1",
		"GIT_AUTHOR_NAME": "t", "GIT_AUTHOR_EMAIL": "t@t", "GIT_COMMITTER_NAME": "t",
		"GIT_COMMITTER_EMAIL": "t@t", "GIT_AUTHOR_DATE": "2026-01-01T00:00:00Z",
		"GIT_COMMITTER_DATE": "2026-01-01T00:00:00Z", "GIT_TERMINAL_PROMPT": "0"} {
		t.Setenv(k, v)
	}
	ceil := base
	if resolved, err := filepath.EvalSymlinks(base); err == nil && resolved != base {
		ceil += string(os.PathListSeparator) + resolved
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

func write(t *testing.T, p, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// main holds a.js, app.py and a lockfile; feature changes what the scenario's files say.
const (
	jsBody = "function f(x) {\n  if (x) {\n    return 1;\n  }\n}\n"
	pyBody = "def f(x):\n    if x:\n        g()\n    return 1\n"
)

func (sc scenario) build(t *testing.T) world {
	t.Helper()
	base := t.TempDir()
	gitEnv(t, base)
	w := world{base: base, repo: filepath.Join(base, "r"), shas: map[string]string{}}
	git(t, base, "init", "-q", "--bare", "-b", "main", filepath.Join(base, "up"))
	git(t, base, "init", "-q", "-b", "main", w.repo)
	write(t, filepath.Join(w.repo, "a.js"), jsBody)
	write(t, filepath.Join(w.repo, "app.py"), pyBody)
	write(t, filepath.Join(w.repo, "web", "package-lock.json"), "{\n  \"v\": 1\n}\n")
	for p, body := range sc.onMain {
		write(t, filepath.Join(w.repo, p), body)
	}
	git(t, w.repo, "add", "-A")
	git(t, w.repo, "commit", "-q", "-m", "base")
	git(t, w.repo, "remote", "add", "origin", filepath.Join(base, "up"))
	git(t, w.repo, "push", "-q", "-u", "origin", "main")
	git(t, w.repo, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/main")
	w.shas["main"] = git(t, w.repo, "rev-parse", "HEAD")
	git(t, w.repo, "checkout", "-q", "-b", "feature")
	files := sc.files
	if files == nil {
		files = map[string]string{"b.js": "1\n2\n3\n"}
	}
	for p, body := range files {
		if body == "DELETE" {
			if err := os.Remove(filepath.Join(w.repo, p)); err != nil {
				t.Fatal(err)
			}
		} else {
			write(t, filepath.Join(w.repo, p), body)
		}
	}
	git(t, w.repo, "add", "-A")
	git(t, w.repo, "commit", "-q", "-m", "feature")
	w.shas["feature"] = git(t, w.repo, "rev-parse", "HEAD")
	if sc.learn != "" {
		write(t, filepath.Join(w.repo, ".git", "info", "review-loop-learnings.md"), sc.learn)
	}

	bin := filepath.Join(base, "bin")
	gh := sc.gh
	if gh == "" {
		gh = "exit 1"
	}
	write(t, filepath.Join(bin, "gh"), "#!/bin/sh\n"+gh+"\n")
	if err := os.Chmod(filepath.Join(bin, "gh"), 0o755); err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(base, "home")
	if sc.store != nil {
		write(t, filepath.Join(home, ".claude", "review-loop", "reviewed-shas"), sc.store(w))
	}
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	if sc.prep != nil {
		sc.prep(t, w)
	}
	t.Chdir(filepath.Join(w.repo, sc.cwd))
	return w
}

var today = regexp.MustCompile(`"today": "\d{4}-\d{2}-\d{2}"`)

func transcript(code int, out, errOut string) string {
	return "exit=" + strconv.Itoa(code) + "\n--- stdout ---\n" + today.ReplaceAllString(out, `"today": "<today>"`) +
		"--- stderr ---\n" + errOut
}

func entries(n int) string {
	var b strings.Builder
	b.WriteString("# Learnings\n\n")
	for i := range n {
		b.WriteString("- 2026-10-01: entry " + strconv.Itoa(i) + "\n")
	}
	return b.String()
}

const prBase = `case "$1 $2" in "pr view") echo main ;; *) exit 1 ;; esac`

var scenarios = []scenario{
	{name: "base from the PR", gh: prBase},
	{name: "base from the repo default", gh: `case "$1 $2" in "repo view") echo main ;; *) exit 1 ;; esac`},
	{name: "base from origin HEAD"},
	{name: "no base at all", prep: func(t *testing.T, w world) { git(t, w.repo, "symbolic-ref", "--delete", "refs/remotes/origin/HEAD") }},
	{name: "a reindent is not review surface", files: map[string]string{"a.js": strings.ReplaceAll(jsBody, "  ", "      ")}},
	{name: "a dedent in python is", files: map[string]string{"app.py": "def f(x):\n    if x:\n        pass\n    g()\n    return 1\n"}},
	// Reindented, not added: an added file has no whitespace-only lines for -w to hide.
	{name: "indentation-sensitive files reindented",
		onMain: map[string]string{"values.yml.j2": "a:\n  b: 1\n", "Makefile.am": "all:\n  true\n", "x.JS": "if (a) {\n  b()\n}\n"},
		files:  map[string]string{"values.yml.j2": "a:\n    b: 1\n", "Makefile.am": "all:\n\ttrue\n", "x.JS": "if (a) {\n    b()\n}\n"}},
	{name: "indentation-sensitive suffixes", files: map[string]string{
		"values.yml.j2": "a:\n  b: 1\n", "Up.PY": "x = 1\n", "Makefile.am": "all:\n\ttrue\n", "notes.MD": "- a\n  - b\n"}},
	{name: "lockfiles at any depth", files: map[string]string{"web/package-lock.json": "{\n  \"v\": 2,\n  \"w\": 3\n}\n", "go.sum": "x v1 h1:a\n", "b.js": "1\n"}},
	{name: "generated files are excluded and named", files: map[string]string{
		".gitattributes": "gen/** linguist-generated\napp/[id]/page.tsx linguist-generated\n",
		"gen/out.js":     "1\n2\n3\n4\n", "app/[id]/page.tsx": "x\n", "app/a/page.tsx": "y\n"}},
	{name: "a binary file counts nothing", files: map[string]string{"img.bin": "\x00\x01\x02binary", "b.js": "1\n"}},
	{name: "a rename", files: map[string]string{"a.js": "DELETE", "moved.js": jsBody}},
	{name: "a deletion", files: map[string]string{"app.py": "DELETE"}},
	{name: "a big diff", files: map[string]string{"big.js": strings.Repeat("x\n", 40)}},
	{name: "from a subdirectory", cwd: "web", files: map[string]string{"web/x.js": "1\n2\n", "b.js": "1\n"}},
	{name: "learnings with entries", learn: entries(3) + "- café, naïve — 💡\n"},
	{name: "learnings header only", learn: "# Learnings\n\nNothing yet.\n"},
	{name: "learnings due, never swept", learn: entries(40)},
	{name: "learnings swept, not regrown", learn: entries(45) + "<!-- swept: 41 -->\n"},
	{name: "learnings swept, regrown to the clamp", learn: entries(48) + "<!-- swept: 44 -->\n"},
	{name: "learnings swept past the clamp", learn: entries(56) + "<!-- swept: 56 -->\n"},
	{name: "learnings swept past the clamp, regrown", learn: entries(64) + "<!-- swept: 56 -->\n"},
	{name: "a sweep count above the live count", learn: entries(41) + "<!-- swept: 3600000000 -->\n"},
	{name: "sweep markers, loose", learn: entries(45) + "  <!--Swept:41 -->.\r\n<!-- swept:44-->\n"},
	{name: "sweep marker, CRLF file", learn: strings.ReplaceAll(entries(45)+"<!-- swept: 44 -->\n", "\n", "\r\n")},
	{name: "learnings trailing blank lines", learn: entries(2) + "\n\n\n"},
	{name: "npm test and lint fix", files: map[string]string{"package.json": `{"scripts": {"test": "jest", "lint": "eslint . --fix"}}`}},
	{name: "npm lint-fix script wins", files: map[string]string{"package.json": `{"scripts": {"lint": "eslint .", "lint:fix": "eslint --fix ."}}`}},
	{name: "npm lint script a list", files: map[string]string{"package.json": `{"scripts": {"lint": ["eslint", "--fix"]}}`}},
	{name: "npm lint script an object", files: map[string]string{"package.json": `{"scripts": {"lint": {"--fix": 1}}}`}},
	{name: "npm lint without fix", files: map[string]string{"package.json": `{"scripts": {"lint": "eslint ."}}`}},
	{name: "package json unparseable", files: map[string]string{"package.json": `{"scripts": `, "pytest.ini": "[pytest]\n"}},
	{name: "pyproject and ruff", files: map[string]string{"pyproject.toml": "[x]\n", "ruff.toml": "\n"}},
	{name: "cargo and eslintrc", files: map[string]string{"Cargo.toml": "[package]\n", ".eslintrc.json": "{}\n"}},
	{name: "make test and rubocop", files: map[string]string{"Makefile": "all:\n\ttrue\ntest:\n\tgo test\n", ".rubocop.yml": "x: 1\n"}},
	{name: "a Makefile without test", files: map[string]string{"Makefile": "all:\n\ttrue\n\ttest:\n"}},
	{name: "reviewed at the parent", store: func(w world) string { return w.shas["main"] + "\n" }},
	{name: "reviewed at HEAD", store: func(w world) string { return "x\n" + w.shas["feature"] + "\n" }},
	{name: "reviewed nowhere on this branch", store: func(w world) string { return strings.Repeat("0", 40) + "\n" }},
	{name: "a reviewed delta too big for the fast path", files: map[string]string{"big.js": strings.Repeat("x\n", 30)},
		store: func(w world) string { return w.shas["main"] + "\n" }},
	// The size verdict keys on semantic lines: raw is over 30 here, review surface is 1.
	{name: "a lockfile bump beside a small edit", files: map[string]string{
		"web/package-lock.json": "{\n" + strings.Repeat("  \"v\": 2,\n", 40) + "}\n", "b.js": "1\n"}},
	// A path that looks generated is counted unless the repo marks it linguist-generated.
	{name: "a dist path without the marker", files: map[string]string{"dist/out.js": "1\n2\n3\n"}},
	// The regrowth margin from both sides: swept+7 is not due, swept+8 is.
	{name: "learnings swept, one short of regrowth", learn: entries(43) + "<!-- swept: 36 -->\n"},
	{name: "learnings swept, regrown by the margin", learn: entries(44) + "<!-- swept: 36 -->\n"},
	// A big branch reviewed at its tip, then a one-line follow-up: the branch is still too big,
	// the delta is not.
	{name: "a small follow-up on a big reviewed branch", files: map[string]string{"big.js": strings.Repeat("x\n", 40)},
		store: func(w world) string { return w.shas["feature"] + "\n" },
		prep: func(t *testing.T, w world) {
			write(t, filepath.Join(w.repo, "b.js"), "1\n")
			git(t, w.repo, "add", "-A")
			git(t, w.repo, "commit", "-q", "-m", "follow-up")
		}},
}

func TestContextMatchesTheShell(t *testing.T) {
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			want := golden.Read(t, "transcript")
			sc.build(t)
			var o, e bytes.Buffer
			code := Command(nil, &o, &e)
			if got := transcript(code, o.String(), e.String()); got != want {
				t.Errorf("looper context no longer does what context.sh did\n--- shell (frozen) ---\n%s\n--- looper ---\n%s", want, got)
			}
		})
	}
}
