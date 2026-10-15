// Package evidence is the format of the review report as evidence: the marker that names a
// run inside a report body, the pending-report path a report waits at until a PR exists, and
// the rule for what a run id may be.
//
// internal/report WRITES the evidence and internal/push READS it. Each half keeps its own
// logic, so that a bug in one cannot be agreed with by the other. The format, though, lives
// only here, because two copies of it are what allowed the `-->` forgery to ship with CI
// green: the two run-id rules disagreed and no test compared them (cmd/looper/main_test.go,
// TestPrReportRefusesARunIDThatCouldForgeTheComment).
package evidence

import (
	"fmt"
	"path/filepath"
	"regexp"
)

// A run id is interpolated raw into a path segment and into a public PR comment, so `../`
// would escape the git dir and `-->` would close the marker early. The ids runlog writes are
// hex, so the hole has only ever been exercised on purpose.
var runIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// ValidRunID reports whether id is safe to place in a marker or a path. Marker and
// PendingPath already enforce it; callers use it to refuse early with an explanation.
func ValidRunID(id string) bool { return runIDRe.MatchString(id) }

// Marker is the comment at the top of every report body. It is keyed on the run id so that
// concurrent worktree sessions in one repo cannot satisfy each other's gate. It refuses an
// invalid id.
func Marker(id string) (string, bool) {
	if !ValidRunID(id) {
		return "", false
	}
	return fmt.Sprintf("<!-- review-loop:run=%s -->", id), true
}

// PendingPath is where a report is deferred under the git common dir until a PR exists. It
// is per RUN rather than per repo, because every worktree shares that dir; a single fixed name
// let concurrent sessions overwrite each other's report. It refuses an invalid id.
func PendingPath(commonDir, id string) (string, bool) {
	if !ValidRunID(id) {
		return "", false
	}
	return filepath.Join(commonDir, "info", "review-loop-pending-report."+id+".md"), true
}
