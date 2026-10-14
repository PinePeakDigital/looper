package ledger

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pinepeakdigital/looper/internal/golden"
)

// Where the Python crashed or answered falsely, looper decides instead. Each divergence keeps
// the Python's frozen answer beside it, so the test fails if the record of what is being
// diverged FROM is lost, and asserts looper's decision. README.md lists them.

var cmds = []string{"threat-model", "deferred"}

// divergence is one fixture for a divergence test: the ledger to write (or, with dir, a
// directory where the ledger should be) and the arguments to pass.
type divergence struct {
	cmd    string
	ledger string
	dir    bool
	args   []string
}

func (d divergence) setup(t *testing.T) (*fixture, string) {
	t.Helper()
	f := newFixture(t)
	p := filepath.Join(f.root, ".git", "info", ledgerName(d.cmd))
	var err error
	if d.dir {
		err = os.Mkdir(p, 0o755)
	} else {
		err = os.WriteFile(p, []byte(f.subst.Replace(d.ledger)), 0o644)
	}
	if err != nil {
		t.Fatal(err)
	}
	return f, f.root
}

func (d divergence) runGo(t *testing.T) (code int, out, errOut string) {
	t.Helper()
	f, dir := d.setup(t)
	t.Chdir(dir)
	var o, e bytes.Buffer
	code = command(d.cmd)(d.args, &o, &e)
	return code, f.normalise(o.String()), f.normalise(e.String())
}

// An unreadable ledger — here a directory where the file should be — is an error, exit 1 and a
// sentence. The Python caught every OSError on open as exists:false, so an unreadable deferred
// list reported nothing deferred, and an unreadable threat model sent Step 2b to bootstrap a
// model over the one it could not read. A missing file is still exists:false.
func TestAnUnreadableLedgerIsAnError(t *testing.T) {
	for _, cmd := range cmds {
		t.Run(cmd, func(t *testing.T) {
			py := golden.Read(t, "transcript")
			if !strings.HasPrefix(py, "exit=0\n") || !strings.Contains(py, `"exists": false`) {
				t.Fatalf("the frozen oracle no longer records exists:false for a directory:\n%s", py)
			}
			code, out, errOut := divergence{cmd: cmd, dir: true}.runGo(t)
			want := "looper " + cmd + ": cannot read .git/info/" + ledgerName(cmd) + ": "
			if code != 1 || out != "" || !strings.HasPrefix(errOut, want) {
				t.Errorf("exit %d, stdout %q, stderr %q; want exit 1, no stdout, stderr starting %q",
					code, out, errOut, want)
			}
		})
	}
}

var invalidUTF8 = map[string]string{
	"threat-model": "- OBSERVED: caf\xe9. [src/types.ts @ {c1s}]\n- OBSERVED: \xff uncited\n- OBSERVED: [src/\xfe\xff.ts @ 0000000]\n",
	"deferred":     "- DEFERRED x: caf\xe9.\n  Grounding: [src/types.ts @ {c1s}]\n  Guard: none.\n- DEFERRED \xfe\xff y\n",
}

// A ledger that is not valid UTF-8 is read, each run of bad bytes as one U+FFFD, exactly as if
// it had held U+FFFD there: the report is the Python's report on that substituted ledger
// (frozen as `replaced`). The Python's strict decoder died with a traceback instead, taking
// Step 2a's or 2b's whole worklist with it over one bad byte in a prose line.
func TestInvalidUTF8LedgerIsRead(t *testing.T) {
	for _, cmd := range cmds {
		t.Run(cmd, func(t *testing.T) {
			py := golden.Read(t, "transcript")
			if !strings.HasPrefix(py, "exit=1\n") || !strings.Contains(py, "UnicodeDecodeError") {
				t.Fatalf("the frozen oracle no longer records the crash:\n%s", py)
			}
			want := golden.Read(t, "replaced")
			if !strings.HasPrefix(want, "exit=0\n") || !strings.Contains(want, `\ufffd`) {
				t.Fatalf("the frozen report on the substituted ledger is not a report:\n%s", want)
			}
			code, out, errOut := divergence{cmd: cmd, ledger: invalidUTF8[cmd]}.runGo(t)
			if got := transcript(code, out, errOut); got != want {
				t.Errorf("--- python on the U+FFFD ledger (frozen) ---\n%s\n--- looper on the invalid one ---\n%s", want, got)
			}
		})
	}
}

// --selftest as the first argument ran the Python's own checks and printed "ok". looper has
// none to run — they are this package's tests — so it refuses with exit 2 rather than print a
// report nobody asked for, or an "ok" for a check that did not happen. Anywhere but first,
// --selftest is ignored with every other argument, as the Python ignored it (the parity table's
// "arguments are ignored" cases).
func TestSelftestIsRefused(t *testing.T) {
	for _, cmd := range cmds {
		t.Run(cmd, func(t *testing.T) {
			py := golden.Read(t, "transcript")
			if py != "exit=0\n--- stdout ---\nok\n--- stderr ---\n" {
				t.Fatalf("the frozen oracle no longer records the selftest's ok:\n%s", py)
			}
			code, out, errOut := divergence{cmd: cmd, ledger: "", args: []string{"--selftest"}}.runGo(t)
			if code != 2 || out != "" || !strings.Contains(errOut, "go test ./internal/ledger/") {
				t.Errorf("exit %d, stdout %q, stderr %q", code, out, errOut)
			}
		})
	}
}
