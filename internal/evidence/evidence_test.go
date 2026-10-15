package evidence

import (
	"strings"
	"testing"
)

func TestOnlyAPlainRunIDBuildsEvidence(t *testing.T) {
	for _, id := range []string{"", "../escape", "a-->b", "a\nb", "a b", "a>b", "abc\n", "é",
		strings.Repeat("x", 65)} {
		if ValidRunID(id) {
			t.Errorf("%q accepted", id)
		}
		if m, ok := Marker(id); ok || m != "" {
			t.Errorf("Marker(%q) = %q, %v", id, m, ok)
		}
		if p, ok := PendingPath("/g", id); ok || p != "" {
			t.Errorf("PendingPath(%q) = %q, %v", id, p, ok)
		}
	}
	for _, id := range []string{"pr1", "6a0fe216ade0", "a-b_c", strings.Repeat("x", 64)} {
		if m, ok := Marker(id); !ok || m != "<!-- review-loop:run="+id+" -->" {
			t.Errorf("Marker(%q) = %q, %v", id, m, ok)
		}
		if p, ok := PendingPath("/g", id); !ok || p != "/g/info/review-loop-pending-report."+id+".md" {
			t.Errorf("PendingPath(%q) = %q, %v", id, p, ok)
		}
	}
}
