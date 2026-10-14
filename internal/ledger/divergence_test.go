package ledger

import (
	"bytes"
	"os"
	"os/exec"
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
	// prep, when set, runs on the fixture root before the ledger is written; with noLedger,
	// no ledger is written at all.
	prep     func(t *testing.T, root string)
	noLedger bool
}

func (d divergence) setup(t *testing.T) (*fixture, string) {
	t.Helper()
	f := newFixture(t)
	if d.prep != nil {
		d.prep(t, f.root)
	}
	p := filepath.Join(f.root, ".git", "info", ledgerName(d.cmd))
	var err error
	if d.noLedger {
	} else if d.dir {
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

// .git/info as a regular file is no ledger, exists:false, as the Python said: open fails with
// ENOTDIR, and nothing can be under a file. Not a divergence — the case the unreadable rule
// above must not swallow.
func TestInfoAsAFileIsNoLedger(t *testing.T) {
	for _, cmd := range cmds {
		t.Run(cmd, func(t *testing.T) {
			want := golden.Read(t, "transcript")
			code, out, errOut := divergence{cmd: cmd, noLedger: true, prep: infoAsFile}.runGo(t)
			if got := transcript(code, out, errOut); got != want {
				t.Errorf("--- python (frozen) ---\n%s\n--- looper ---\n%s", want, got)
			}
		})
	}
}

func infoAsFile(t *testing.T, root string) {
	info := filepath.Join(root, ".git", "info")
	if err := os.RemoveAll(info); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(info, []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

var nulPin = map[string]string{
	"threat-model": "- OBSERVED: fine. [src/types.ts @ {c1s}]\n- OBSERVED: bad. [src/a\x00b.ts @ {c1s}]\n",
	"deferred":     "- DEFERRED x\n  Grounding: [src/a\x00b.ts @ {c1s}]\n  Guard: none.\n",
}

// A NUL byte in a pin's path exits 1 naming the pin's line. git cannot be asked about such a
// path — the exec fails to start, which git() reads as "nothing moved" — so the pin would look
// fresh forever. The Python died with ValueError.
func TestANULInAPinPathIsAnError(t *testing.T) {
	for _, cmd := range cmds {
		t.Run(cmd, func(t *testing.T) {
			py := golden.Read(t, "transcript")
			if !strings.HasPrefix(py, "exit=1\n") || !strings.Contains(py, "embedded null byte") {
				t.Fatalf("the frozen oracle no longer records the crash:\n%s", py)
			}
			code, out, errOut := divergence{cmd: cmd, ledger: nulPin[cmd]}.runGo(t)
			if code != 1 || out != "" || !strings.Contains(errOut, "line 2 names a path with a NUL byte") {
				t.Errorf("exit %d, stdout %q, stderr %q", code, out, errOut)
			}
		})
	}
}

var latin1Pin = map[string]string{
	"threat-model": "- OBSERVED: x. [src/naïve.ts @ {c1s}]\n",
	"deferred":     "- DEFERRED x\n  Grounding: [src/naïve.ts @ {c1s}]\n  Guard: none.\n",
}

// git output that is not UTF-8 — here `i18n.logOutputEncoding latin1` re-encoding the subject
// "touch naïve" — is read with U+FFFD for the bad bytes, as the ledger itself is. The Python's
// strict decoder died with a traceback, losing the whole worklist to one commit subject.
func TestNonUTF8GitOutputIsRead(t *testing.T) {
	for _, cmd := range cmds {
		t.Run(cmd, func(t *testing.T) {
			py := golden.Read(t, "transcript")
			if !strings.HasPrefix(py, "exit=1\n") || !strings.Contains(py, "UnicodeDecodeError") {
				t.Fatalf("the frozen oracle no longer records the crash:\n%s", py)
			}
			code, out, errOut := divergence{cmd: cmd, ledger: latin1Pin[cmd], prep: latin1Log}.runGo(t)
			if code != 0 || !strings.Contains(out, `touch na\ufffdve"`) {
				t.Errorf("exit %d, stdout %q, stderr %q", code, out, errOut)
			}
		})
	}
}

func latin1Log(t *testing.T, root string) {
	runGit(t, root, nil, "config", "i18n.logOutputEncoding", "latin1")
}

// A git common dir that is not UTF-8 exits 1 with a sentence. Decoded with U+FFFD it names no
// real file, so an existing ledger would read as missing: the deferred list empty, the threat
// model bootstrapped over. The Python died with UnicodeDecodeError. APFS refuses such a name,
// so a wrapper git reports one.
func TestNonUTF8CommonDirIsAnError(t *testing.T) {
	for _, cmd := range cmds {
		t.Run(cmd, func(t *testing.T) {
			py := golden.Read(t, "transcript")
			if !strings.HasPrefix(py, "exit=1\n") || !strings.Contains(py, "UnicodeDecodeError") {
				t.Fatalf("the frozen oracle no longer records the crash:\n%s", py)
			}
			code, out, errOut := divergence{cmd: cmd, ledger: "- x\n", prep: latin1CommonDir}.runGo(t)
			if code != 1 || out != "" || !strings.Contains(errOut, "is not UTF-8") {
				t.Errorf("exit %d, stdout %q, stderr %q", code, out, errOut)
			}
		})
	}
}

// latin1CommonDir puts a git first on PATH that answers --git-common-dir with a Latin-1 path
// and hands everything else to the real git.
func latin1CommonDir(t *testing.T, root string) {
	real, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	script := "#!/bin/sh\nif [ \"$1 $2\" = \"rev-parse --git-common-dir\" ]; then printf '/srv/caf\\351/.git\\n'; exit 0; fi\nexec '" + real + "' \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}
