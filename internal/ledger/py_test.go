package ledger

import (
	"errors"
	"regexp"
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

type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("closed") }

// A report that cannot be written is not a report: exit 1, never 0.
func TestAnUnwritableReportExits1(t *testing.T) {
	if emit(failWriter{}, obj{{"x", 1}}) != 1 {
		t.Error("emit returned 0 on a failed write")
	}
}
