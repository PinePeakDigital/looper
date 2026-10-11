package golden

import (
	"os"
	"path/filepath"
	"testing"
)

// recorder stands in for a test so Read's failure can be observed rather than suffered. It
// records instead of stopping, so a Read that wrongly carried on is visible too.
type recorder struct {
	testing.TB
	name            string
	failed, skipped bool
}

func (r *recorder) Helper()               {}
func (r *recorder) Name() string          { return r.name }
func (r *recorder) Fatalf(string, ...any) { r.failed = true }
func (r *recorder) Skip(...any)           { r.skipped = true }
func (r *recorder) Skipf(string, ...any)  { r.skipped = true }
func (r *recorder) SkipNow()              { r.skipped = true }
func (r *recorder) Errorf(string, ...any) { r.failed = true }
func (r *recorder) Error(...any)          { r.failed = true }
func (r *recorder) Fatal(...any)          { r.failed = true }

// The one behaviour the parity gates rest on. Asserted directly because the gates cannot see
// its absence: three report goldens are legitimately EMPTY (an oracle that crashed printed
// nothing on stdout), so a Read that answered "" for a missing file would leave those cases
// passing with their goldens deleted — measured before this test existed.
func TestAMissingGoldenFailsAndNeverSkips(t *testing.T) {
	t.Chdir(t.TempDir())
	r := &recorder{name: "TestX/a_case"}
	Read(r, "out")
	if !r.failed || r.skipped {
		t.Errorf("Read of a missing golden: failed=%v skipped=%v, want failed and not skipped",
			r.failed, r.skipped)
	}
}

func TestReadFindsTheGoldenBySubtestName(t *testing.T) {
	t.Chdir(t.TempDir())
	p := filepath.Join("testdata", "oracle", "TestX", "a_case", "out")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("frozen"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := &recorder{name: "TestX/a_case"}
	if got := Read(r, "out"); got != "frozen" || r.failed || r.skipped {
		t.Errorf("Read = %q, failed=%v skipped=%v", got, r.failed, r.skipped)
	}
}
