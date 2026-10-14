package ledger

import (
	"errors"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"
)

// spaceClass is the regexp spelling of isSpace, which is record.PySpace. Nothing else ties the
// two together, so a change to either would split the citation regexp's idea of whitespace
// from the stripping's without a parity fixture necessarily noticing.
func TestSpaceClassIsIsSpace(t *testing.T) {
	re := regexp.MustCompile(`^[` + spaceClass + `]$`)
	for r := rune(0); r <= utf8.MaxRune; r++ {
		if r >= 0xd800 && r <= 0xdfff {
			continue // surrogates are not encodable runes
		}
		if got, want := re.MatchString(string(r)), isSpace(r); got != want {
			t.Errorf("U+%04X: spaceClass %v, isSpace %v", r, got, want)
		}
	}
}

// The line breaks are str.splitlines()'s, frozen from Python 3: [c for c in range(0x110000)
// if len(('a'+chr(c)+'b').splitlines()) == 2]. Half of them appear in no parity fixture.
func TestLineBreaksAreSplitlines(t *testing.T) {
	want := map[rune]bool{0x0a: true, 0x0b: true, 0x0c: true, 0x0d: true, 0x1c: true,
		0x1d: true, 0x1e: true, 0x85: true, 0x2028: true, 0x2029: true}
	for r := rune(0); r <= utf8.MaxRune; r++ {
		if r >= 0xd800 && r <= 0xdfff {
			continue
		}
		if got := len(splitlines("a" + string(r) + "b")); got != 2 && want[r] || got != 1 && !want[r] {
			t.Errorf("U+%04X: %d line(s)", r, got)
		}
	}
}

// Escapes no parity fixture reaches, frozen from Python 3's json.dumps.
func TestQuoteIsJSONDumps(t *testing.T) {
	for in, want := range map[string]string{
		"\b":         `"\b"`,
		"\U00090000": `"\uda00\udc00"`,
		"\U0010ffff": `"\udbff\udfff"`,
	} {
		var b strings.Builder
		quote(&b, in)
		if b.String() != want {
			t.Errorf("quote(%q) = %s, want %s", in, b.String(), want)
		}
	}
}

type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("closed") }

// A report that cannot be written is not a report: exit 1, never 0.
func TestAnUnwritableReportExits1(t *testing.T) {
	if emit(failWriter{}, obj{{"x", 1}}) != 1 {
		t.Error("emit returned 0 on a failed write")
	}
}
