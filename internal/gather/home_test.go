package gather

import (
	"os"
	"testing"
)

// TestMain keeps every test off the real ~/.claude/review-loop: whatever reaches the store
// through internal/home lands in this temp dir instead.
func TestMain(m *testing.M) {
	d, err := os.MkdirTemp("", "looper-home-")
	if err != nil {
		panic(err)
	}
	os.Setenv("REVIEW_LOOP_HOME", d)
	code := m.Run()
	os.RemoveAll(d)
	os.Exit(code)
}
