// Package evidence is the format of the review report as evidence: the marker that names a
// run inside a report body, the pending-report path a report waits at until a PR exists, and
// the rule for what a run id may be.
//
// internal/report WRITES the evidence and internal/push READS it. Each half keeps its own
// logic, so that a bug in one cannot be agreed with by the other. The format, though, lives
// only here, because two copies of it are what allowed the `-->` forgery to ship with CI
// green: the two run-id rules disagreed, and the test comparing them had no case for `>` (cmd/looper/main_test.go,
// TestPrReportRefusesARunIDThatCouldForgeTheComment).
package evidence

import (
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
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

// Pending is a file at the pending-report location and the run id its name carries.
type Pending struct{ Path, ID string }

// PendingReports lists every file at the pending-report location under commonDir, sorted by
// path as runlog.py's glob was. The ID is read off the name unchecked, so a stray file still
// surfaces for the caller to name rather than vanishing.
func PendingReports(commonDir string) []Pending {
	files, _ := filepath.Glob(filepath.Join(commonDir, "info", "review-loop-pending-report.*.md"))
	sort.Strings(files)
	out := make([]Pending, len(files))
	for i, f := range files {
		out[i] = Pending{f, strings.TrimSuffix(strings.TrimPrefix(filepath.Base(f), "review-loop-pending-report."), ".md")}
	}
	return out
}
