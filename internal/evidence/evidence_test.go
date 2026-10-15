package evidence

import (
	"os"
	"path/filepath"
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

func TestPendingReportsListsEveryFileInPathOrder(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "info"), 0o755); err != nil {
		t.Fatal(err)
	}
	var want []string
	for _, id := range []string{"a", "a-1", "x y"} {
		p := filepath.Join(dir, "info", "review-loop-pending-report."+id+".md")
		if err := os.WriteFile(p, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		want = append(want, p+"="+id)
	}
	var got []string
	for _, p := range PendingReports(dir) {
		got = append(got, p.Path+"="+p.ID)
	}
	// Path order, as runlog.py sorted: `-` sorts before `.`, so a-1's file precedes a's.
	want[0], want[1] = want[1], want[0]
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("got %q, want %q", got, want)
	}
}
