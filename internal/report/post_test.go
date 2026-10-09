package report

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pinepeakdigital/looper/internal/push"
)

// shimGh puts a scripted `gh` first on PATH and returns the path of the file it logs its
// argv to. The log is what lets a test assert WHICH call was made rather than only what came
// back: the post path's whole job is choosing between commenting, deferring and labelling,
// and a test that only reads the return value cannot tell a skipped comment from a failed one.
func shimGh(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "calls.log")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + log + "\n" + body + "\n"
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return log
}

// noGh puts a PATH with NO gh on it, and no git either when bare is false — the
// not-installed case, which must read as "unknown", not as "there is no PR".
func noGh(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	// git has to stay reachable: the deferral writes into its --git-common-dir, and a test
	// that removes both cannot tell a missing gh from a missing git.
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Skip("no git on PATH")
	}
	if err := os.Symlink(gitPath, filepath.Join(dir, "git")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
}

func calls(t *testing.T, log string) string {
	t.Helper()
	b, err := os.ReadFile(log)
	if err != nil {
		return ""
	}
	return string(b)
}

func gitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if out, err := exec.Command("git", "init", "-q", dir).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	return dir
}

// The defect that motivated this slice. pr-report.py reads `rc, num, _ = sh("gh","pr","view",
// ...)` and then `if rc != 0 or not num:` prints "no PR yet" — asserting the first of three
// causes and discarding, into `_`, the only stream that says which it was. On this machine gh
// has already failed with a keychain TLS error while `git push` worked, so the wrong branch is
// the reachable one, and its advice ("create a PR") cannot fix it.
func TestFindPRDistinguishesTheThreeCauses(t *testing.T) {
	for _, c := range []struct {
		name, gh   string
		noGh       bool
		want       PRState
		saysInNote string
	}{
		{name: "a PR exists", gh: `echo 7`, want: PRFound, saysInNote: "PR #7"},
		// gh pr list EXITS 0 with no output when there are none — the whole reason the probe
		// asks `pr list` rather than `pr view`, which exits 1 for this and for every failure
		// alike. Verified against the live repo before the switch.
		{name: "no PR for the branch", gh: `exit 0`, want: NoPR, saysInNote: "no PR for this branch yet"},
		{name: "gh is not installed", noGh: true, want: GhUnrunnable, saysInNote: "could not be run"},
		{name: "gh fails on TLS", gh: `echo "tls: failed to verify certificate: x509: OSStatus -26276" >&2; exit 1`,
			want: GhFailed, saysInNote: "x509"},
		{name: "gh fails on auth", gh: `echo "gh: authentication required" >&2; exit 4`,
			want: GhFailed, saysInNote: "authentication required"},
		// A killed gh RAN. Folding it in with not-installed would answer "install gh", which
		// is the one action that cannot help — and the kill is usually this probe's own
		// timeout firing on a gh that hung.
		{name: "gh is killed", gh: `kill -TERM $$`, want: GhFailed, saysInNote: "signal: terminated"},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := gitRepo(t)
			if c.noGh {
				noGh(t)
			} else {
				shimGh(t, c.gh)
			}
			got := FindPR(dir, "feat/x")
			if got.State != c.want {
				t.Fatalf("state %v, want %v (detail %q, note %q)", got.State, c.want, got.Detail, got.Note())
			}
			if !strings.Contains(got.Note(), c.saysInNote) {
				t.Errorf("the note does not name the cause: %q does not contain %q", got.Note(), c.saysInNote)
			}
			// The one assertion that fails if the three collapse back into one: only the
			// answered-no case may say there is no PR.
			if c.want != NoPR && strings.Contains(got.Note(), "no PR for this branch") {
				t.Errorf("%v asserts there is no PR, which it never established: %q", c.want, got.Note())
			}
		})
	}
}

// A detached HEAD and an unresolvable branch have no PR by this route, and the report still
// has to survive — so this is a probe failure, not a crash and not a claim about PRs.
func TestFindPRWithNoBranchToAskAbout(t *testing.T) {
	dir := gitRepo(t)
	log := shimGh(t, `echo 7`)
	got := FindPR(dir, "HEAD")
	if got.State != GhFailed {
		t.Fatalf("state %v, want GhFailed", got.State)
	}
	if calls(t, log) != "" {
		t.Errorf("gh was asked anyway, with a branch name of %q: %s", "HEAD", calls(t, log))
	}
}

const postBody = "<!-- review-loop:run=post01 -->\n\n## review-loop\n\nbody text\n"

// Every path that cannot comment must still keep the body. "Fail on a missing report, never on
// a failed post" is the module's own rule, and the push gate reads the pending file, so a
// dropped body leaves the push refused on advice that cannot succeed.
func TestPostKeepsTheBodyOnEveryUnpostablePath(t *testing.T) {
	for _, c := range []struct {
		name, gh   string
		noGh       bool
		saysInDiag string
		commented  bool
	}{
		{name: "no PR", gh: `exit 0`, saysInDiag: "no PR for this branch yet"},
		{name: "gh unrunnable", noGh: true, saysInDiag: "could not be run"},
		{name: "gh failing", gh: `echo "502 Bad Gateway" >&2; exit 1`, saysInDiag: "502 Bad Gateway"},
		// A PR exists and the COMMENT fails: the one case where the Python already deferred,
		// kept here because it is the case a refactor is most likely to drop.
		{name: "the comment fails", gh: `case "$*" in "pr list"*) echo 9;; *) echo "rate limited" >&2; exit 1;; esac`,
			saysInDiag: "rate limited", commented: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := gitRepo(t)
			if c.noGh {
				noGh(t)
			} else {
				shimGh(t, c.gh)
			}
			var diag strings.Builder
			path, err := Post(PostParams{Repo: dir, RunID: "post01", Branch: "feat/x",
				Convergence: "capped", Body: postBody, Diag: &diag})
			if err != nil {
				t.Fatalf("Post: %v", err)
			}
			b, readErr := os.ReadFile(path)
			if readErr != nil {
				t.Fatalf("the report was not kept: %v", readErr)
			}
			if !strings.HasPrefix(string(b), postBody) {
				t.Errorf("the kept body is not the rendered one:\n%s", b)
			}
			// Step 0c reads this note to apply the label the post never got to.
			if !strings.Contains(string(b), "label review:capped still owed") {
				t.Errorf("no label-owed note for Step 0c:\n%s", b)
			}
			if !strings.Contains(diag.String(), c.saysInDiag) {
				t.Errorf("stderr does not say why: %q does not contain %q", diag.String(), c.saysInDiag)
			}
			if got := strings.Contains(diag.String(), "comment failed"); got != c.commented {
				t.Errorf("comment-failed reported %v, want %v: %q", got, c.commented, diag.String())
			}
		})
	}
}

func TestPostCommentsAndStillKeepsALocalCopy(t *testing.T) {
	dir := gitRepo(t)
	log := shimGh(t, `case "$*" in "pr list"*) echo 9;; esac`)
	var diag strings.Builder
	path, err := Post(PostParams{Repo: dir, RunID: "post01", Branch: "feat/x",
		Convergence: "converged", Body: postBody, Diag: &diag})
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	if !strings.Contains(calls(t, log), "pr comment 9 --body") {
		t.Errorf("the comment was never posted:\n%s", calls(t, log))
	}
	// The local copy is not belt-and-braces by accident: a post that succeeded while the
	// read-back failed refused the push forever and posted a duplicate on every retry.
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("no local copy after a successful post: %v", err)
	}
	if !strings.HasPrefix(string(b), postBody) {
		t.Errorf("the local copy is not the posted body:\n%s", b)
	}
	if !strings.Contains(diag.String(), "posted to PR #9") {
		t.Errorf("stderr does not say it posted: %q", diag.String())
	}
}

// The label is the at-a-glance half of the signal: a reader should not have to open a comment
// to learn whether the review finished. Three removals and one add, so two cannot stand at once.
func TestPostMovesTheLabel(t *testing.T) {
	dir := gitRepo(t)
	log := shimGh(t, `case "$*" in "pr list"*) echo 9;; esac`)
	var diag strings.Builder
	if _, err := Post(PostParams{Repo: dir, RunID: "post01", Branch: "feat/x",
		Convergence: "capped", Body: postBody, Label: true, Diag: &diag}); err != nil {
		t.Fatalf("Post: %v", err)
	}
	c := calls(t, log)
	if !strings.Contains(c, "label create review:capped --color fbca04") {
		t.Errorf("the label was not created:\n%s", c)
	}
	if !strings.Contains(c, "pr edit 9 --add-label review:capped") {
		t.Errorf("the label was not applied:\n%s", c)
	}
	for _, other := range []string{"review:converged", "review:halted", "review:unknown"} {
		if !strings.Contains(c, "pr edit 9 --remove-label "+other) {
			t.Errorf("%s was not removed, so two labels can stand at once:\n%s", other, c)
		}
	}
	if strings.Contains(c, "--remove-label review:capped") {
		t.Errorf("the label it just applied was also removed:\n%s", c)
	}
}

// A failed label must not cost a posted report: the publish already succeeded, and losing it
// to a missing `labels` scope would be the module's own rule inverted.
func TestALabelFailureDoesNotFailThePost(t *testing.T) {
	dir := gitRepo(t)
	shimGh(t, `case "$*" in "pr list"*) echo 9;; "pr comment"*) exit 0;; *) echo "missing scope" >&2; exit 1;; esac`)
	var diag strings.Builder
	if _, err := Post(PostParams{Repo: dir, RunID: "post01", Branch: "feat/x",
		Convergence: "capped", Body: postBody, Label: true, Diag: &diag}); err != nil {
		t.Fatalf("a label failure failed the post: %v", err)
	}
	if !strings.Contains(diag.String(), "label review:capped failed: missing scope") {
		t.Errorf("stderr does not name the label failure: %q", diag.String())
	}
}

func TestWritePending(t *testing.T) {
	t.Run("the path is per-run, under the git common dir", func(t *testing.T) {
		dir := gitRepo(t)
		path, err := WritePending(dir, "abc123", "halted", postBody)
		if err != nil {
			t.Fatal(err)
		}
		// Per RUN, not per repo: that directory is shared by every worktree, and one fixed
		// name let two concurrent sessions overwrite each other's report.
		// Through EvalSymlinks: git's --path-format=absolute resolves them, and on macOS the
		// temp dir is /var -> /private/var, so a literal Join comparison fails on the link
		// rather than on the path shape this asserts.
		real, err := filepath.EvalSymlinks(dir)
		if err != nil {
			t.Fatal(err)
		}
		if want := filepath.Join(real, ".git", "info", "review-loop-pending-report.abc123.md"); path != want {
			t.Errorf("path %q, want %q", path, want)
		}
	})
	t.Run("a traversing run id is refused", func(t *testing.T) {
		dir := gitRepo(t)
		// The id reaches a path here, and it arrives from -run-id. Refused rather than
		// cleaned: a cleaned id would write a file the gate then cannot find.
		if _, err := WritePending(dir, "../../../../tmp/evil", "halted", postBody); err == nil {
			t.Error("a traversing run id was accepted")
		}
		if _, err := os.Stat("/tmp/evil.md"); err == nil {
			t.Error("the traversal landed")
		}
	})
	t.Run("no git dir is an error, not a silent drop", func(t *testing.T) {
		// A report with nowhere to go is the one outcome worth failing on — the caller must
		// not be told it was kept.
		if _, err := WritePending(t.TempDir(), "abc123", "halted", postBody); err == nil {
			t.Error("a non-repo was accepted")
		}
	})
	t.Run("the label-owed note defaults to unknown", func(t *testing.T) {
		dir := gitRepo(t)
		path, err := WritePending(dir, "abc123", "", postBody)
		if err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(b), "label review:unknown still owed") {
			t.Errorf("an empty convergence left no label owed:\n%s", b)
		}
	})
}

// The two halves of the gate validate the run id separately — internal/push must not import
// this package, or the writer and the reader of the pending path become one package that can
// agree with itself by accident. This is the assertion that keeps the duplicate honest.
func TestTheRunIDGuardMatchesThePushGates(t *testing.T) {
	for _, id := range []string{
		"", "abc123", "A-b_9", "../../etc/passwd", "a/b", "a.b", "a b", "a\x00b", "ab;rm -rf /",
		strings.Repeat("a", 64), strings.Repeat("a", 65), "é", "-",
	} {
		if got, want := validRunID(id), push.ValidRunID(id); got != want {
			t.Errorf("%q: this package says %v, the push gate says %v — the writer and the "+
				"reader of the pending path now disagree about what a run id is", id, got, want)
		}
	}
}

// An unrecognised convergence word cannot come from the record — record.Convergence answers
// only the four — but it arrives here as a plain string, and the nil-map read it used to take
// would have cost a posted report its label with no note saying why.
func TestAnUnknownConvergenceAppliesNoLabel(t *testing.T) {
	dir := gitRepo(t)
	log := shimGh(t, `case "$*" in "pr list"*) echo 9;; esac`)
	var diag strings.Builder
	if _, err := Post(PostParams{Repo: dir, RunID: "post01", Branch: "feat/x",
		Convergence: "mostly-fine", Body: postBody, Label: true, Diag: &diag}); err != nil {
		t.Fatalf("Post: %v", err)
	}
	if strings.Contains(calls(t, log), "--add-label") {
		t.Errorf("a label was invented for an unknown convergence:\n%s", calls(t, log))
	}
	if !strings.Contains(diag.String(), `no label for convergence "mostly-fine"`) {
		t.Errorf("stderr does not say the label was skipped: %q", diag.String())
	}
}
