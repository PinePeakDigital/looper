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
	"net/url"
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

// PRsForBranch is the `gh api` endpoint listing the PRs whose head is branch, newest first and
// in every state: the PR a report is posted to (report.FindPR) and the PR whose comments the
// push gate reads (push's report probe). One spelling, here, so the two cannot disagree on
// which PR carries the evidence. `state=all` is the state filter the `gh pr list --state all`
// it replaced used: a report belongs on its branch's PR after that PR merges too.
//
// REST rather than `gh pr list`/`gh pr view`, which go through gh's GraphQL endpoint. The
// friction log records a sandbox where gh's GraphQL calls failed with a keychain TLS error
// (`x509: OSStatus -26276`) while its REST calls worked, so the probes refused or deferred
// while the comment they guarded would have landed.
//
// `{owner}` and `{repo}` are gh's own placeholders, filled from the repo gh runs in; gh fills
// them anywhere in the endpoint, query included (checked with GH_DEBUG=api, gh 2.102.0). The
// colon after `{owner}` is sent encoded (%3A) on purpose: gh also substitutes the legacy
// `:owner`/`:repo`/`:branch` spellings, so a literal colon before a branch named `branch-x`
// would be rewritten to the CURRENT branch. The branch is query-escaped, so a `#`, `&` or `+`
// in its name is sent as part of the name. `head` is matched against the base repo's owner,
// so a PR opened from a fork is not found by this route.
func PRsForBranch(branch string) string {
	return "repos/{owner}/{repo}/pulls?state=all&head={owner}%3A" + url.QueryEscape(branch)
}

// PRComments is the `gh api` endpoint for PR n's conversation comments, which is where
// `gh pr comment` posts. Callers pass --paginate: a long-lived PR outgrows one page.
func PRComments(n int) string {
	return fmt.Sprintf("repos/{owner}/{repo}/issues/%d/comments", n)
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
