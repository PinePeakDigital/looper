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
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"

	"github.com/pinepeakdigital/looper/internal/home"
)

// A run id is interpolated raw into a path segment and into a public PR comment, so `../`
// would escape the run dirs and `-->` would close the marker early. The ids runlog writes are
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

// PendingPath is where a report is deferred until a PR exists: <run dir>/pending-report.md.
// Per RUN, because one repo's worktrees and branches run the loop concurrently, and in the run
// dir, because a worktree-isolated builder cannot write under .git (narthur/skills#16). It
// refuses an invalid id, which would traverse out of the run dirs.
func PendingPath(id string) (string, bool) {
	if !ValidRunID(id) {
		return "", false
	}
	rd, err := home.RunDir(id)
	if err != nil {
		return "", false
	}
	return filepath.Join(rd, PendingName), true
}

// PendingName is a pending report's file name inside its run dir.
const PendingName = "pending-report.md"

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

// PendingReports lists every pending report in every run dir, sorted by path, after moving
// any left at the old place under repo's git common dir ("" is the working directory's) into
// their run dirs.
// Run dirs are shared by every repo, so the caller scopes the list by the run record's repo.
// The ID is the dir's name unchecked, so a stray dir still surfaces for the caller to name
// rather than vanishing. The error says which reports could not be moved or listed; the list
// is still every report that could be.
func PendingReports(repo string) ([]Pending, error) {
	merr := home.MigratePending(repo, ValidRunID)
	base, err := home.Base()
	if err != nil {
		return nil, errors.Join(merr, err)
	}
	// Listed, not globbed: a $REVIEW_LOOP_HOME holding `[` made Glob fail and list nothing.
	des, err := os.ReadDir(filepath.Join(base, "run"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, errors.Join(merr, err)
	}
	out := []Pending{}
	for _, de := range des {
		f := filepath.Join(base, "run", de.Name(), PendingName)
		if _, err := os.Lstat(f); err == nil {
			out = append(out, Pending{f, de.Name()})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path }) // by path, as Glob gave
	return out, merr
}
