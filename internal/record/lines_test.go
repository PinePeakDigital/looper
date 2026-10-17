package record

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// The one tokenizer both readers share: Python's text mode lines, str.strip()ped, blank ones
// kept for the tail's count.
func TestStoreLinesAreRunlogsLines(t *testing.T) {
	p := filepath.Join(t.TempDir(), "runs.jsonl")
	if err := os.WriteFile(p, []byte("a\r\nb\rc\n\n \x1cd \ne"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		limit   int
		want    []string
		dropped int
	}{
		{0, []string{"a", "b", "c", "", "d", "e"}, 0},
		{3, []string{"", "d", "e"}, 3},
	} {
		got, dropped, err := storeLines(p, c.limit)
		if err != nil || !reflect.DeepEqual(got, c.want) || dropped != c.dropped {
			t.Errorf("limit %d: %q, dropped %d, %v; want %q, dropped %d", c.limit, got, dropped, err, c.want, c.dropped)
		}
	}
	if got, _, err := storeLines(filepath.Join(t.TempDir(), "none"), 0); got != nil || err != nil {
		t.Errorf("a missing store: %q, %v", got, err)
	}
}
