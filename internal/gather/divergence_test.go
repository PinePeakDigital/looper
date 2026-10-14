package gather

import (
	"bytes"
	"strings"
	"testing"

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

// package.json's scripts are read only as an object of strings. context.sh tested membership
// with Python's `in` on whatever was there: a string of scripts matched "test" as a substring
// and answered `npm test`, and a list, or a lint script that was not a string, died with a
// TypeError and printed nothing — the whole Step 0 context lost to one odd package.json.
var oddScripts = []struct{ name, pkg, test, lint string }{
	{"scripts a list", `{"scripts": ["test", "lint"]}`, "null", "null"},
	{"scripts a string", `{"scripts": "run the test suite"}`, "null", "null"},
	{"lint script not a string", `{"scripts": {"lint": 5}}`, "null", `"npm run lint"`},
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
