// Package golden holds what the retired Python oracles printed, so the parity tests keep
// comparing byte for byte after push-check.py and pr-report.py were deleted.
//
// The files are a record of what the Python DID over these fixtures, not a spec. A deliberate
// behaviour change edits the golden in the same commit as the code; a fixture change has no
// oracle left to recapture from, so its golden is written by hand and reviewed as such. A test
// may replace what differs between two correct runs (a temp path, a commit) with a placeholder
// before comparing; the golden then holds the placeholder, and the test names what it replaces.
package golden

import (
	"os"
	"path/filepath"
	"testing"
)

// Read returns the frozen output for this test under key. A missing golden fails the test,
// never skips it: `go test` reports a skip as success, which is how a parity gate goes inert.
func Read(t testing.TB, key string) string {
	t.Helper()
	p := filepath.Join("testdata", "oracle", filepath.FromSlash(t.Name()), key)
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("no frozen oracle output for this case: %v", err)
	}
	return string(b)
}
