package learn

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// learn.py protects only "PATTERN:", while the entries it is meant to protect are written
// "PATTERN — …": it evicted them first. Measured against the oracle on this exact input: it
// kept `c` and dropped both PATTERNs.
func TestPruneKeepsAnEmDashPattern(t *testing.T) {
	lines := []string{"## Accepted patterns", "- 2020-01-01: PATTERN — a", "- 2020-01-02: PATTERN — b", "- 2026-01-01: c"}
	kept, dropped := Prune(lines, 2)
	if len(dropped) != 1 || dropped[0] != "- 2026-01-01: c" {
		t.Errorf("dropped %q, want only the one non-PATTERN entry; kept %q", dropped, kept)
	}
}

// learn.py dies on a file that is not UTF-8 and writes nothing. Decoding would substitute
// U+FFFD and the rewrite would then replace every bad byte in the file.
func TestANonUTF8FileIsLeftAlone(t *testing.T) {
	f := filepath.Join(t.TempDir(), "l.md")
	body := []byte("## Dismissed\n- 2026-01-01: caf\xe9\n")
	if err := os.WriteFile(f, body, 0o644); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := Command([]string{"bump", f, "caf"}, &out, &errOut); code != 1 || !strings.Contains(errOut.String(), "not UTF-8") {
		t.Errorf("exit %d, stderr %q", code, errOut.String())
	}
	if got, _ := os.ReadFile(f); !bytes.Equal(got, body) {
		t.Errorf("the file was rewritten: %q", got)
	}
}

// learn.py truncated the file and then died encoding an entry that is not UTF-8. Written as it
// came, it would lock the file: every later call refuses a file that is not UTF-8.
func TestANonUTF8EntryIsRefused(t *testing.T) {
	f := filepath.Join(t.TempDir(), "l.md")
	body := []byte("## Accepted patterns\n- 2026-01-01: a\n")
	if err := os.WriteFile(f, body, 0o644); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := Command([]string{"add", f, "bad\xff", "--section", "accepted"}, &out, &errOut); code != 1 {
		t.Errorf("exit %d, stderr %q", code, errOut.String())
	}
	if got, _ := os.ReadFile(f); !bytes.Equal(got, body) {
		t.Errorf("the file changed: %q", got)
	}
}

// learn.py printed a traceback for a file it could not read; this names the cause.
func TestAnUnreadableFileIsAnError(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := Command([]string{"bump", t.TempDir(), "x"}, &out, &errOut); code != 1 || !strings.Contains(errOut.String(), "can't read") {
		t.Errorf("exit %d, stderr %q", code, errOut.String())
	}
}

// re's \d is Unicode; this date is ASCII digits only, so an entry dated in another script's
// digits is ordinary text here. learn.py would have bumped it.
func TestAnEntryDateIsASCIIDigits(t *testing.T) {
	if Bump([]string{"- \u0662\u0660\u0662\u0666-01-01: x"}, "x", "2026-01-02") {
		t.Error("an Arabic-Indic date was read as an entry")
	}
}
