package gather

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pinepeakdigital/looper/internal/golden"
)

// Where context.sh crashed or answered by accident, looper decides instead. Each case keeps
// context.sh's frozen answer beside looper's, so the test fails if the record of what is being
// diverged FROM is lost. README.md lists them.

type divergence struct {
	scenario
	args []string
}

func (d divergence) run(t *testing.T) (int, string, string) {
	t.Helper()
	d.build(t)
	var o, e bytes.Buffer
	code := Command(d.args, &o, &e)
	return code, o.String(), e.String()
}

// -h prints a usage line and exits 0, and any other argument is a usage error. context.sh read
// no arguments at all, so `context.sh -h` fetched, sized and printed the whole context.
func TestArgumentsAreNotIgnored(t *testing.T) {
	py := golden.Read(t, "transcript")
	if !strings.HasPrefix(py, "exit=0\n--- stdout ---\n{") {
		t.Fatalf("the frozen oracle no longer records context.sh ignoring -h:\n%s", py)
	}
	code, out, errOut := divergence{scenario: scenario{}, args: []string{"-h"}}.run(t)
	if code != 0 || !strings.HasPrefix(out, "usage: looper context") || errOut != "" {
		t.Errorf("-h: exit %d, stdout %q, stderr %q", code, out, errOut)
	}
	code, out, errOut = divergence{scenario: scenario{}, args: []string{"extra"}}.run(t)
	if code != 2 || out != "" || !strings.Contains(errOut, "takes no arguments") {
		t.Errorf("extra: exit %d, stdout %q, stderr %q", code, out, errOut)
	}
}

// package.json's scripts are read only as an object. context.sh tested membership with Python's
// `in` on whatever was there: a string of scripts matched "test" as a substring and answered
// `npm test`, and a list of scripts, or a lint script that was a number or null, died with a
// TypeError and printed nothing — the whole Step 0 context lost to one odd package.json. (A lint
// script that is a list or an object answers as Python's `in` does; that is parity.) A
// package.json using Python's JSON extensions (NaN, Infinity) is not JSON, and reads as none.
var oddScripts = []struct{ name, pkg, test, lint string }{
	{"scripts a list", `{"scripts": ["test", "lint"]}`, "null", "null"},
	{"scripts a string", `{"scripts": "run the test suite"}`, "null", "null"},
	{"lint script a number", `{"scripts": {"lint": 5}}`, "null", `"npm run lint"`},
	{"package json with NaN", `{"scripts": {"test": "x"}, "v": NaN}`, "null", "null"},
}

func TestPackageScriptsAreAnObject(t *testing.T) {
	for _, c := range oddScripts {
		t.Run(c.name, func(t *testing.T) {
			golden.Read(t, "transcript") // what context.sh did, kept beside the decision
			code, out, _ := divergence{scenario: scenario{files: map[string]string{"package.json": c.pkg}}}.run(t)
			if code != 0 || !strings.Contains(out, `"test_cmd": `+c.test+",") || !strings.Contains(out, `"lint_cmd": `+c.lint+",") {
				t.Errorf("exit %d:\n%s", code, out)
			}
		})
	}
}

// A Makefile that is not UTF-8 is still searched for its test target, read as text mode reads
// it but with U+FFFD for the bad bytes. context.sh's read_text died on it, printing nothing:
// one Latin-1 comment took the whole Step 0 context with it.
func TestALatin1MakefileIsRead(t *testing.T) {
	py := golden.Read(t, "transcript")
	if !strings.Contains(py, "UnicodeDecodeError") {
		t.Fatalf("the frozen oracle no longer records the crash:\n%s", py)
	}
	code, out, _ := divergence{scenario: scenario{files: map[string]string{"Makefile": "# caf\xe9\ntest:\n\tgo test\n"}}}.run(t)
	if code != 0 || !strings.Contains(out, `"test_cmd": "make test",`) {
		t.Errorf("exit %d:\n%s", code, out)
	}
}

// A Go module has a test and a lint command. context.sh detected neither, so looper's own
// context answered null for both and the loop warned and ran no linter. go.mod is a manifest:
// it ranks below Cargo.toml and above the Makefile fallback for the test command, and after
// every other linter for the lint command, as each detector there wins in its listed order.
var goModules = []struct {
	name       string
	files      map[string]string
	test, lint string
	fix        bool
}{
	{"go.mod alone", map[string]string{"go.mod": "module x\n"}, `"go test ./..."`, `"go fmt ./... && go vet ./..."`, true},
	{"go.mod over a Makefile", map[string]string{"go.mod": "module x\n", "Makefile": "test:\n\tgo test -race ./...\n"}, `"go test ./..."`, `"go fmt ./... && go vet ./..."`, true},
	{"Cargo.toml over go.mod", map[string]string{"go.mod": "module x\n", "Cargo.toml": "[package]\n"}, `"cargo test"`, `"go fmt ./... && go vet ./..."`, true},
	{"npm scripts over go.mod", map[string]string{"go.mod": "module x\n", "package.json": `{"scripts": {"test": "t", "lint": "l"}}`}, `"npm test"`, `"npm run lint"`, false},
	{"ruff over go.mod", map[string]string{"go.mod": "module x\n", "ruff.toml": ""}, `"go test ./..."`, `"ruff check --fix"`, true},
}

func TestAGoModuleIsDetected(t *testing.T) {
	for _, c := range goModules {
		t.Run(c.name, func(t *testing.T) {
			code, out, _ := divergence{scenario: scenario{files: c.files}}.run(t)
			for _, want := range []string{`"test_cmd": ` + c.test + ",", `"lint_cmd": ` + c.lint + ",", `"lint_fix": ` + strconv.FormatBool(c.fix) + ","} {
				if code != 0 || !strings.Contains(out, want) {
					t.Errorf("exit %d, want %s in:\n%s", code, want, out)
				}
			}
		})
	}
}

// Learnings that are not UTF-8 reach the JSON with U+FFFD for each bad byte. context.sh passed
// them through the environment, where Python's surrogateescape turned each into a lone
// surrogate escape, \udcXX — JSON many parsers refuse.
func TestLatin1LearningsAreReplaced(t *testing.T) {
	py := golden.Read(t, "transcript")
	if !strings.Contains(py, `\udce9`) {
		t.Fatalf("the frozen oracle no longer records the surrogate escape:\n%s", py)
	}
	_, out, _ := divergence{scenario: scenario{learn: "- caf\xe9\n"}}.run(t)
	if !strings.Contains(out, `"learnings": "- caf\ufffd",`) {
		t.Errorf("got:\n%s", out)
	}
}

// Where context.sh died, or said nothing, looper answers — each case keeps the shell's frozen
// transcript and asserts what looper prints instead.
var decided = []struct {
	name string
	d    divergence
	want []string
}{
	// An unfetched or missing base: context.sh printed 0 lines and no reason, which reads as an
	// empty diff. The plan's own scan fails the same way and plans wide; the context says why.
	{"an unreadable diff is said", divergence{scenario: scenario{gh: `case "$1 $2" in "pr view") echo nosuch ;; *) exit 1 ;; esac`}},
		[]string{`"sizing_excluded": "the diff against origin/nosuch...HEAD could not be read, so nothing was sized",`}},
	// A base starting with '-' is no git branch name, and `git fetch origin <base>` would read it
	// as an option. context.sh passed it on.
	{"a base starting with a dash is refused", divergence{scenario: scenario{gh: `case "$1 $2" in "pr view") echo -x ;; *) exit 1 ;; esac`}},
		[]string{`"base_branch": null,`}},
	// No HOME is no review store. context.sh died under set -u; a path relative to the repo
	// would have trusted a store the repo itself ships.
	{"no HOME is no store", divergence{scenario: scenario{store: func(w world) string { return w.shas["main"] + "\n" },
		prep: func(t *testing.T, w world) {
			t.Setenv("HOME", "")
			if err := os.Unsetenv("HOME"); err != nil {
				t.Fatal(err)
			}
			write(t, filepath.Join(w.repo, ".claude", "review-loop", "reviewed-shas"), w.shas["main"]+"\n")
		}}}, []string{`"last_reviewed_sha": null,`}},
	// A Makefile that is a directory has no test target. context.sh's read_text died on it.
	{"a Makefile directory", divergence{scenario: scenario{files: map[string]string{"Makefile/x": "test:\n"}}},
		[]string{`"test_cmd": null,`}},
	// Learnings too big for the environment. context.sh passed them to Python through it, and
	// past the platform's argument limit Python never started: exit 126, nothing printed.
	{"learnings too big for the environment", divergence{scenario: scenario{learn: strings.Repeat("- "+strings.Repeat("x", 198)+"\n", 6000)}},
		[]string{`"learnings_entries": 6000,`}},
	// A name that is not UTF-8 (core.quotePath=false) is reported as context.sh's helper failing:
	// the raw count stands. The diff stat's bad bytes are U+FFFD where context.sh wrote \udcXX.
	// APFS refuses such a name on disk, so it goes straight into the index: the diff compares
	// commits, not the working tree.
	{"a name that is not UTF-8", divergence{scenario: scenario{files: map[string]string{"a.js": strings.ReplaceAll(jsBody, "  ", "      ")},
		prep: nonUTF8Name}},
		[]string{`"semantic_lines": 7,`, `"sizing_excluded": "sizing helper produced no count; using the raw total",`, `caf\ufffd.txt`}},
}

func TestCrashesAndSilencesAreDecided(t *testing.T) {
	for _, c := range decided {
		t.Run(c.name, func(t *testing.T) {
			golden.Read(t, "transcript") // what context.sh did, kept beside the decision
			code, out, _ := c.d.run(t)
			if code != 0 {
				t.Fatalf("exit %d:\n%s", code, out)
			}
			for _, w := range c.want {
				if !strings.Contains(out, w) {
					t.Errorf("want %s in:\n%s", w, out)
				}
			}
		})
	}
}

// gh and git fetch reach the network, and context.sh waited on them forever: a stalled gh hung
// Step 0 with nothing printed. Past netTimeout looper gives up and falls back. A gh that exits
// while a grandchild holds its pipes is cut off a second later. There is no shell transcript:
// the shell never finished.
func TestAStalledProbeIsGivenUp(t *testing.T) {
	for _, c := range []struct {
		name, gh, want string
		timeout        time.Duration
	}{
		{"a gh that never answers", `case "$1 $2" in "pr view") exec sleep 8 ;; *) exit 1 ;; esac`, `"base_branch": "main",`, 200 * time.Millisecond},
		{"a gh whose grandchild lingers", `case "$1 $2" in "pr view") sleep 8 & echo release ;; *) exit 1 ;; esac`, `"base_branch": "release",`, 3 * time.Second},
	} {
		t.Run(c.name, func(t *testing.T) {
			defer func(d time.Duration) { netTimeout = d }(netTimeout)
			netTimeout = c.timeout
			start := time.Now()
			code, out, _ := divergence{scenario: scenario{gh: c.gh}}.run(t)
			if took := time.Since(start); took > 6*time.Second {
				t.Errorf("took %s", took)
			}
			if code != 0 || !strings.Contains(out, c.want) {
				t.Errorf("exit %d, want %s in:\n%s", code, c.want, out)
			}
		})
	}
}

func nonUTF8Name(t *testing.T, w world) {
	t.Helper()
	git(t, w.repo, "config", "core.quotePath", "false")
	cmd := exec.Command("git", "hash-object", "-w", "--stdin")
	cmd.Dir, cmd.Stdin = w.repo, strings.NewReader("1\n")
	blob, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	git(t, w.repo, "update-index", "--add", "--cacheinfo", "100644,"+strings.TrimSpace(string(blob))+",caf\xe9.txt")
	git(t, w.repo, "commit", "-q", "-m", "latin-1 name")
}

// A learnings store that cannot be found is said on the caller's stderr, not swallowed: an
// empty learnings field reads as "nothing dismissed".
func TestUnfoundLearningsAreSaid(t *testing.T) {
	code, out, errOut := divergence{scenario: scenario{prep: func(t *testing.T, w world) {
		t.Setenv("REVIEW_LOOP_HOME", "")
		t.Setenv("HOME", "")
		if err := os.Unsetenv("HOME"); err != nil {
			t.Fatal(err)
		}
	}}}.run(t)
	if code != 0 || !strings.Contains(out, `"learnings": null,`) || !strings.Contains(errOut, "learnings unreadable") {
		t.Errorf("exit %d, stderr %q, stdout:\n%s", code, errOut, out)
	}
}
