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
	// Long enough that the short-entry refusal cannot answer for this one.
	if code := Command([]string{"add", f, "a bad byte \xff in the entry", "--section", "accepted"}, &out, &errOut); code != 1 || !strings.Contains(errOut.String(), "not UTF-8") {
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

// add stamps its own date, so text that already opened with one had it written twice
// ("- <today>: 2026-10-20: …"). One leading ISO date and the colon or spaces after it are
// dropped; a date anywhere else, or a second one, is text. learn.py wrote the date twice.
func TestALeadingDateIsNotWrittenTwice(t *testing.T) {
	old := Today
	Today = func() string { return "2026-10-21" }
	t.Cleanup(func() { Today = old })
	for _, c := range []struct{ name, text, want string }{
		{"colon", "2026-10-20: the retry backoff matters", "- 2026-10-21: the retry backoff matters"},
		{"colon without a space", "2026-10-20:the retry backoff matters", "- 2026-10-21: the retry backoff matters"},
		{"space", "2026-10-20 the retry backoff matters", "- 2026-10-21: the retry backoff matters"},
		{"only the first date", "2026-10-20: 2026-10-19: the retry backoff", "- 2026-10-21: 2026-10-19: the retry backoff"},
		{"a date later in the text", "the backoff broke on 2026-10-20", "- 2026-10-21: the backoff broke on 2026-10-20"},
		{"digits that are no date", "20261020 rows were read", "- 2026-10-21: 20261020 rows were read"},
		{"leading whitespace before the date", "  2026-10-20: the retry backoff matters", "- 2026-10-21: the retry backoff matters"},
		{"a tab after the date", "2026-10-20\tthe retry backoff matters", "- 2026-10-21: the retry backoff matters"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := filepath.Join(t.TempDir(), "l.md")
			var out, errOut bytes.Buffer
			if code := Command([]string{"add", f, c.text, "--section", "accepted"}, &out, &errOut); code != 0 {
				t.Errorf("exit %d, stderr %q", code, errOut.String())
			}
			got, _ := os.ReadFile(f)
			if !strings.Contains(string(got), "\n"+c.want+"\n") {
				t.Errorf("the entry's date is wrong or written twice: want %q in\n%s", c.want, got)
			}
		})
	}
}

// Text under minEntry characters, once trimmed, is a probe or an empty argument, not a learning:
// a stray `x` was once written to a real learnings file. It is refused with exit 1 and the file
// is left alone. learn.py wrote whatever it was given, the empty string included.
func TestAShortEntryIsRefused(t *testing.T) {
	for _, c := range []struct {
		name, text string
		code       int
	}{
		{"a probe", "x", 1},
		{"empty", "", 1},
		{"short once trimmed", "   test   ", 1},
		{"short once the date is dropped", "2026-10-20: x", 1},
		{"one under", "123456789", 1},
		{"exactly the minimum", "1234567890", 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := filepath.Join(t.TempDir(), "l.md")
			body := []byte("## Accepted patterns\n- 2026-01-01: keep the retry backoff\n")
			if err := os.WriteFile(f, body, 0o644); err != nil {
				t.Fatal(err)
			}
			var out, errOut bytes.Buffer
			code := Command([]string{"add", f, c.text, "--section", "accepted"}, &out, &errOut)
			got, _ := os.ReadFile(f)
			if code != c.code {
				t.Errorf("exit %d, want %d; stderr %q", code, c.code, errOut.String())
			}
			if c.code == 1 && (!bytes.Equal(got, body) || !strings.Contains(errOut.String(), "too short")) {
				t.Errorf("a short entry was written, or refused without saying why: stderr %q, file\n%s", errOut.String(), got)
			}
		})
	}
}

// strings.Contains(x, "") is true, so bump with no text, "" or only whitespace re-dated the
// first entry in the file. learn.py did too. Refused, and nothing is written.
func TestBumpRefusesAnEmptyMatch(t *testing.T) {
	body := "## Dismissed\n- 2026-01-01: foo bar (declined)\n- 2026-01-02: other\n"
	for _, c := range []struct {
		name string
		args []string
	}{
		{"no text", []string{"bump", "{f}"}},
		{"an empty string", []string{"bump", "{f}", ""}},
		{"whitespace", []string{"bump", "{f}", " \t "}},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := filepath.Join(t.TempDir(), "l.md")
			if err := os.WriteFile(f, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			args := append([]string(nil), c.args...)
			args[1] = f
			var out, errOut bytes.Buffer
			if code := Command(args, &out, &errOut); code != 1 || !strings.Contains(errOut.String(), "match text is empty") {
				t.Errorf("exit %d, stderr %q", code, errOut.String())
			}
			if got, _ := os.ReadFile(f); string(got) != body {
				t.Errorf("the file was rewritten: %q", got)
			}
		})
	}
}

// A date alone is no learning: add strips one leading ISO date (with any whitespace before it
// and a colon, spaces or tabs after it), and what is left must still clear the length floor.
func TestADateAloneIsTooShort(t *testing.T) {
	for _, c := range []struct{ name, text string }{
		{"a bare date", "2026-10-20"},
		{"leading whitespace", "  2026-10-20"},
		{"a tab after the date", "2026-10-20\t"},
		{"a colon and a tab", "2026-10-20:\t"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := filepath.Join(t.TempDir(), "l.md")
			body := "## Accepted patterns\n- 2026-01-01: a\n"
			if err := os.WriteFile(f, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			var out, errOut bytes.Buffer
			if code := Command([]string{"add", f, c.text, "--section", "accepted"}, &out, &errOut); code != 1 || !strings.Contains(errOut.String(), "too short") {
				t.Errorf("exit %d, stderr %q", code, errOut.String())
			}
			if got, _ := os.ReadFile(f); string(got) != body {
				t.Errorf("the file was rewritten: %q", got)
			}
		})
	}
}
