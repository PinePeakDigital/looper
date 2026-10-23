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
	p := f.ledger(t, ledgerName(d.cmd))
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
			want := "looper " + cmd + ": cannot read <store>/" + ledgerName(cmd) + ": "
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
			got := transcript(code, out, errOut)
			// The threat model's report on that ledger is no longer the Python's: it counts
			// claims, not pinned lines (TestCitationsAreReadPerClaim). So the test is looper's own
			// report on the ledger with U+FFFD written there, and the Python's where it still holds.
			if cmd == "threat-model" {
				code, out, errOut = divergence{cmd: cmd, ledger: strings.ToValidUTF8(invalidUTF8[cmd], "\ufffd")}.runGo(t)
				want = transcript(code, out, errOut)
			}
			if !strings.Contains(want, `\ufffd`) {
				t.Fatalf("the report on the substituted ledger names no U+FFFD:\n%s", want)
			}
			if got != want {
				t.Errorf("--- report on the U+FFFD ledger ---\n%s\n--- looper on the invalid one ---\n%s", want, got)
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

// .git/info as a regular file, where a ledger could only have been left by the old layout, is
// no ledger, exists:false, as the Python said: open fails with ENOTDIR, and nothing can be
// under a file, so there is nothing to move into the store. Not a divergence — the case the
// unreadable rule above must not swallow.
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

// A store under a regular file holds no ledger either: ENOTDIR is "none", as the Python read it.
// (`.git/info` as a file no longer reaches read at all — the ledger is in the store.)
func TestAStoreUnderAFileIsNoLedger(t *testing.T) {
	for _, cmd := range cmds {
		t.Run(cmd, func(t *testing.T) {
			f := filepath.Join(t.TempDir(), "file")
			if err := os.WriteFile(f, []byte("x\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			t.Chdir(newFixture(t).root) // not divergence.setup, which makes the store dir
			t.Setenv("REVIEW_LOOP_HOME", f)
			var o, e bytes.Buffer
			code, out, errOut := command(cmd)(nil, &o, &e), o.String(), e.String()
			if code != 0 || !strings.Contains(out, `"exists": false`) {
				t.Errorf("exit %d, stderr %q, stdout:\n%s", code, errOut, out)
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

// An entry marked `RESOLVED @ <sha>` is listed under merged once that sha is on origin/HEAD,
// and only then: one resolved on a commit the default branch lacks stays deferred, and so does
// an entry with no mark. Not from the Python, which had no merged field.
func TestMergedNamesResolvedEntriesOnTheDefaultBranch(t *testing.T) {
	f := newFixture(t)
	first := runGit(t, f.root, nil, "rev-list", "--max-parents=0", "HEAD")
	head := runGit(t, f.root, nil, "rev-parse", "HEAD")
	runGit(t, f.root, nil, "update-ref", "refs/remotes/origin/main", first)
	runGit(t, f.root, nil, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/main")
	ledger := "- DEFERRED a\n  RESOLVED @ " + first[:7] + " (run r1)\n" +
		"- DEFERRED b\n  RESOLVED @ " + head + " (run r2)\n" +
		"- DEFERRED c\n" +
		// Prose quoting the marker is not the marker: d is unresolved, and dropping it would
		// lose the deferral.
		"- DEFERRED d\n  Guard: once RESOLVED @ " + first[:7] + " lands, re-check\n"
	if err := os.WriteFile(f.ledger(t, DeferredFile), []byte(ledger), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(f.root)
	var out, errb bytes.Buffer
	if code := Deferred(nil, &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	want := `"merged": [
    {
      "line": 1,
      "text": "- DEFERRED a"
    }
  ]`
	if !strings.Contains(out.String(), want) {
		t.Errorf("want only entry a merged, got:\n%s", out.String())
	}
}

// With no origin/HEAD the default branch is unknown, and merged is null, not [] — "could not
// tell" must not read as "none merged".
func TestMergedIsNullWithNoDefaultBranch(t *testing.T) {
	f := newFixture(t)
	head := runGit(t, f.root, nil, "rev-parse", "HEAD")
	if err := os.WriteFile(f.ledger(t, DeferredFile), []byte("- DEFERRED a\n  RESOLVED @ "+head+" (run r1)\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(f.root)
	var out, errb bytes.Buffer
	if code := Deferred(nil, &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	if !strings.Contains(out.String(), `"merged": null`) {
		t.Errorf("want merged null with no origin/HEAD, got:\n%s", out.String())
	}
}
