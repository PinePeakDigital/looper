package report

import (
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pinepeakdigital/looper/internal/push"
	"github.com/pinepeakdigital/looper/internal/record"
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

// A detached HEAD has no PR by this route, and the report still has to survive — so this is a
// probe failure, not a crash and not a claim about PRs.
//
// Really detached, not the string "HEAD" passed as the branch. That is what this asserted
// while FindPR read `rev-parse --abbrev-ref HEAD`, whose answer for a detached HEAD IS the
// literal "HEAD" — so the test fed the code its own magic value instead of producing the state
// that generates it. `git branch --show-current` answers empty here, and the magic string is
// gone, so the fixture has to make a real detached HEAD.
func TestFindPRWithNoBranchToAskAbout(t *testing.T) {
	dir := gitRepo(t)
	// No hooks: this machine has a global pre-commit hook that reaches the network.
	mustGit(t, dir, "-c", "core.hooksPath=/dev/null", "-c", "user.email=t@example.com",
		"-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "x")
	mustGit(t, dir, "checkout", "-q", "--detach")
	log := shimGh(t, `echo 7`)
	got := FindPR(dir, "")
	if got.State != GhFailed {
		t.Fatalf("state %v number %q detail %q, want GhFailed", got.State, got.Number, got.Detail)
	}
	if calls(t, log) != "" {
		t.Errorf("gh was asked about a PR with no branch to ask about: %s", calls(t, log))
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
		// And nothing was written ANYWHERE under the repo. The earlier version of this
		// assertion checked os.Stat("/tmp/evil.md") and could not fail: the filename prefix
		// ends in a dot, so the id's first ".." glues onto it as "report..." — not a parent
		// reference — and the naive path resolves to <repo>/tmp/evil.md, measured. A
		// completely unguarded WritePending would have passed that check.
		var found []string
		if err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() && strings.HasSuffix(path, "evil.md") {
				found = append(found, path)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if len(found) > 0 {
			t.Errorf("the traversal landed at %v", found)
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

// The two halves of the gate, pinned to each other end to end: Render writes the artifact,
// WritePending puts it where the gate looks, and push.ReportLanded is the reader. If the
// marker, the `## review-loop` heading or the `N cycle(s) · M agent(s)` line moves on either
// side, every push is refused on advice that cannot succeed — post the report, which this
// run already did.
//
// Asserted by RUNNING the reader rather than by comparing format strings: the two constants
// could agree while the line they produce is assembled differently, which is the whole class
// of near-miss a string comparison misses.
func TestThePushGateFindsWhatThisPackageWrites(t *testing.T) {
	const id = "fp01"
	dir := gitRepo(t)
	store := filepath.Join(t.TempDir(), "runs.jsonl")
	rows := strings.Join([]string{
		`{"run_id":"` + id + `","phase":"plan","planned_at":"2026-01-01T00:00:00","repo":"x","agent_cap":8}`,
		`{"run_id":"` + id + `","phase":"cycle","n":1,"applied":3,"agents":9}`,
		`{"run_id":"` + id + `","phase":"cycle","n":2,"applied":1,"agents":4}`,
		`{"run_id":"` + id + `","phase":"finish","outcome":"cycle-limit"}`,
	}, "\n") + "\n"
	if err := os.WriteFile(store, []byte(rows), 0o644); err != nil {
		t.Fatal(err)
	}
	runs, err := record.Load(store, 0)
	if err != nil {
		t.Fatal(err)
	}
	r := runs[id]
	conv, err := r.Convergence()
	if err != nil {
		t.Fatal(err)
	}
	body, err := Render(r, id, conv, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := WritePending(dir, id, conv, body); err != nil {
		t.Fatal(err)
	}
	// No gh on PATH, so the probe falls through to the pending file this just wrote — which
	// is the path Step 14 actually takes, since the push gate forces loop-then-push-then-PR.
	noGh(t)
	if !push.ReportLanded(id, r, dir) {
		t.Errorf("the push gate does not recognise the report this package just rendered.\n"+
			"it was looking for %q\nthe body was:\n%s", push.Fingerprint(r), body)
	}
	// And it is not satisfied by SOME report. Checked by putting THIS body at another run's
	// pending path, so the gate has a file to read and must reject it on the marker — not on
	// the file being absent, which is what a bare second call would have measured.
	if _, err := WritePending(dir, "fp02", conv, body); err != nil {
		t.Fatal(err)
	}
	if push.ReportLanded("fp02", r, dir) {
		t.Error("one run's report, filed under another run's id, satisfied that run's gate — " +
			"the marker is not being checked")
	}
}

// The branches a coverage pass found at execution count 0. Each is a real path with its own
// failure mode, and each was reachable only by a shape no existing test produced.
func TestTheBranchesNothingReached(t *testing.T) {
	t.Run("commas carries a sign", func(t *testing.T) {
		// The `neg` branch was never entered: every fixture and every real run sums
		// subagent_tokens to a non-negative number. MinInt is here because the obvious
		// recursive rewrite of this function overflows the stack on it — see commas' comment.
		for _, c := range []struct {
			n    int
			want string
		}{
			{0, "0"}, {999, "999"}, {1000, "1,000"}, {-1, "-1"}, {-999, "-999"},
			{-1000, "-1,000"}, {-1234567, "-1,234,567"},
			{-9223372036854775808, "-9,223,372,036,854,775,808"},
		} {
			if got := commas(c.n); got != c.want {
				t.Errorf("commas(%d) = %q, want %q", c.n, got, c.want)
			}
		}
	})

	t.Run("firstLine truncates a multi-line stderr", func(t *testing.T) {
		// firstLine exists so a multi-line gh stderr cannot become several diag lines or
		// reach a PR comment. Every test fed it one line, so only the pass-through half ran.
		if got := firstLine("line one\nline two\nline three"); got != "line one" {
			t.Errorf("firstLine kept more than the first line: %q", got)
		}
		if got := firstLine("  padded  \nsecond"); got != "padded" {
			t.Errorf("firstLine did not trim: %q", got)
		}
		if got := firstLine("only one"); got != "only one" {
			t.Errorf("firstLine altered a single line: %q", got)
		}
	})

	t.Run("FindPR resolves the branch itself when none is given", func(t *testing.T) {
		// This is the PRODUCTION call shape: -branch defaults to "", so runPrReport passes
		// "" through and FindPR asks git. Every test passed an explicit branch, so the one
		// path a real run takes was the one path never exercised.
		dir := gitRepo(t)
		// No commit is made: `git branch --show-current` answers on an unborn branch, which
		// is the measurement that moved FindPR off `rev-parse --abbrev-ref HEAD`.
		mustGit(t, dir, "checkout", "-q", "-b", "feat/detected")
		log := shimGh(t, `echo 11`)
		got := FindPR(dir, "")
		if got.State != PRFound || got.Number != "11" {
			t.Fatalf("state %v number %q, want PRFound/11 (detail %q)", got.State, got.Number, got.Detail)
		}
		if !strings.Contains(calls(t, log), "--head feat/detected") {
			t.Errorf("gh was not asked about the branch git reported:\n%s", calls(t, log))
		}
	})

	t.Run("WritePending tells an unrunnable git from a non-repo", func(t *testing.T) {
		// Two differently-worded errors, and only the second was covered. A regression that
		// collapsed them — dropping the Runnable check — would have gone unnoticed.
		noGit(t)
		if _, err := WritePending(t.TempDir(), "abc123", "halted", postBody); err == nil ||
			!strings.Contains(err.Error(), "git could not be run") {
			t.Errorf("want a git-unrunnable error, got %v", err)
		}
	})

	t.Run("a report that can reach nowhere at all is an error", func(t *testing.T) {
		// The module's one stated invariant has two halves: never fail on a failed post, and
		// DO fail when the body could not be preserved. The second half had no test: every
		// Post test used a real git repo, where WritePending always succeeds.
		noGh(t)
		var diag strings.Builder
		path, err := Post(PostParams{Repo: t.TempDir(), RunID: "post01", Branch: "feat/x",
			Convergence: "capped", Body: postBody, Diag: &diag})
		if err == nil {
			t.Fatalf("Post returned nil error with nowhere to keep the report (path %q)", path)
		}
		if !strings.Contains(err.Error(), "could not be preserved") {
			t.Errorf("the error does not say the report was lost: %v", err)
		}
	})

	t.Run("a posted report whose local copy fails still succeeds", func(t *testing.T) {
		// The deliberate swallow: the report IS posted, so losing the belt-and-braces copy
		// must not fail the publish — it must say so and carry on.
		dir := gitRepo(t)
		shimGh(t, `case "$*" in "pr list"*) echo 9;; esac`)
		// Replace .git with a file, so git can run but reports no common dir.
		if err := os.RemoveAll(filepath.Join(dir, ".git")); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, ".git"), []byte("not a repo\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		var diag strings.Builder
		if _, err := Post(PostParams{Repo: dir, RunID: "post01", Branch: "feat/x",
			Convergence: "capped", Body: postBody, Diag: &diag}); err != nil {
			t.Fatalf("a failed local copy failed a successful post: %v", err)
		}
		if !strings.Contains(diag.String(), "local copy could not be written") {
			t.Errorf("stderr does not say the local copy was lost: %q", diag.String())
		}
	})

	t.Run("a binary of the wrong format could not be run", func(t *testing.T) {
		// Enumerating sentinels — ErrNotFound, ErrNotExist, ErrPermission — missed the binary
		// that EXISTS, is executable, and is the wrong format: measured, a chmod +x text file
		// gives a *fs.PathError ("exec format error") matching none of the three and not an
		// ExitError either, so it fell through and reported "gh ran and failed" for a gh that
		// never ran — telling the operator to wait for GitHub when the fix is to reinstall.
		dir := gitRepo(t)
		shim := t.TempDir()
		if err := os.WriteFile(filepath.Join(shim, "gh"), []byte("not a binary\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", shim+string(os.PathListSeparator)+os.Getenv("PATH"))
		got := FindPR(dir, "feat/x")
		if got.State != GhUnrunnable {
			t.Errorf("state %v, want GhUnrunnable — detail %q, note %q", got.State, got.Detail, got.Note())
		}
		if !strings.Contains(got.Note(), "could not be run") {
			t.Errorf("the note gives the wrong remedy: %q", got.Note())
		}
	})

	t.Run("a hung subprocess is killed and reads as a failure", func(t *testing.T) {
		// The timeout branch, which a const budget made untestable: shBudget is threaded
		// through shTimeout so this costs milliseconds instead of twenty seconds.
		dir := gitRepo(t)
		shimGh(t, `sleep 5`)
		r := shTimeout(dir, time.Millisecond, "gh", "pr", "list")
		// It RAN, and it failed. Runnable stays true on purpose: folding a kill in with
		// not-installed would answer "install gh", which is the one action that cannot help
		// a gh that hung. Code is normalised to 1 because Go reports -1 for a
		// signal-terminated process and no caller here could read -1 as "cannot tell".
		if !r.Runnable {
			t.Errorf("a killed subprocess reads as NOT RUNNABLE, which would advise installing "+
				"gh for a gh that is installed and hung: %+v", r)
		}
		if r.Code == 0 {
			t.Errorf("a killed subprocess reads as success, so a hung gh would look like an "+
				"answer about whether a PR exists: %+v", r)
		}
		if !strings.Contains(r.Err, "signal") {
			t.Errorf("the detail does not name the kill, so the diag line cannot say why: %+v", r)
		}
	})
}

// The label must move even when the comment did not land: the Python applies it whenever a PR
// number is known, and the runs whose comment failed are exactly the ones whose report is
// hardest to find, so they need the at-a-glance signal most.
func TestTheLabelMovesEvenWhenTheCommentFails(t *testing.T) {
	dir := gitRepo(t)
	log := shimGh(t, `case "$*" in
	  "pr list"*) echo 9;;
	  "pr comment"*) echo "rate limited" >&2; exit 1;;
	  *) exit 0;;
	esac`)
	var diag strings.Builder
	if _, err := Post(PostParams{Repo: dir, RunID: "post01", Branch: "feat/x",
		Convergence: "halted", Body: postBody, Label: true, Diag: &diag}); err != nil {
		t.Fatalf("Post: %v", err)
	}
	c := calls(t, log)
	if !strings.Contains(c, "pr edit 9 --add-label review:halted") {
		t.Errorf("the label was dropped because the comment failed:\n%s", c)
	}
	if !strings.Contains(diag.String(), "comment failed") {
		t.Errorf("stderr does not report the comment failure: %q", diag.String())
	}
}

// A removal that fails leaves the PREVIOUS run's label standing beside the new one. Four of
// applyLabel's five gh calls discarded their result, so that happened with no note anywhere —
// the same discarded-stderr defect this whole slice exists to stop.
func TestAFailedLabelRemovalIsReported(t *testing.T) {
	dir := gitRepo(t)
	shimGh(t, `case "$*" in
	  "pr list"*) echo 9;;
	  *"--remove-label review:converged"*) echo "label not found" >&2; exit 1;;
	  *) exit 0;;
	esac`)
	var diag strings.Builder
	if _, err := Post(PostParams{Repo: dir, RunID: "post01", Branch: "feat/x",
		Convergence: "halted", Body: postBody, Label: true, Diag: &diag}); err != nil {
		t.Fatalf("Post: %v", err)
	}
	if !strings.Contains(diag.String(), "review:converged not removed") {
		t.Errorf("a failed removal was silent, so a PR can carry two convergences at once: %q",
			diag.String())
	}
	if !strings.Contains(diag.String(), "label review:halted") {
		t.Errorf("the add was not reported: %q", diag.String())
	}
}

// noGit is noGh's mirror: a PATH with gh but no git, so WritePending's two failure modes can
// be told apart.
func noGit(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
}

func mustGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}
