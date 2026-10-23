package home

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func gitInit(t *testing.T) string {
	t.Helper()
	d := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "-b", "main", d},
		{"-C", d, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "--allow-empty", "-m", "c"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return d
}

func write(t *testing.T, p, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// One store per repo: a linked worktree and a subdirectory name the same one, and it is under
// $REVIEW_LOOP_HOME, not under .git.
func TestRepoDirIsOnePerRepo(t *testing.T) {
	t.Setenv("REVIEW_LOOP_HOME", t.TempDir())
	repo := gitInit(t)
	wt := filepath.Join(t.TempDir(), "wt")
	if out, err := exec.Command("git", "-C", repo, "worktree", "add", "-q", wt).CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	sub := filepath.Join(repo, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	want, err := RepoDir(repo)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(want, os.Getenv("REVIEW_LOOP_HOME")+"/repos/") {
		t.Errorf("RepoDir = %q, want it under $REVIEW_LOOP_HOME/repos", want)
	}
	for _, d := range []string{wt, sub} {
		if got, _ := RepoDir(d); got != want {
			t.Errorf("RepoDir(%s) = %q, want %q", d, got, want)
		}
	}
	other, _ := RepoDir(gitInit(t))
	if other == want {
		t.Errorf("two repos share the store %q", want)
	}
}

// A ledger at the old place is moved into the store on first use, and after that the store's
// copy is the one read, whatever is left at the old place.
func TestLedgerMovesTheOldOneIn(t *testing.T) {
	t.Setenv("REVIEW_LOOP_HOME", t.TempDir())
	repo := gitInit(t)
	old := filepath.Join(repo, ".git", "info", "review-loop-deferred.md")
	write(t, old, "old\n")
	p, err := Ledger(repo, Deferred)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(p); string(b) != "old\n" {
		t.Errorf("store copy = %q, want the old ledger", b)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Errorf("the old ledger is still there: %v", err)
	}
	write(t, old, "stale\n")
	if p2, _ := Ledger(repo, Deferred); p2 != p {
		t.Fatalf("path moved: %q then %q", p, p2)
	}
	if b, _ := os.ReadFile(p); string(b) != "old\n" {
		t.Errorf("a leftover at the old place overwrote the store: %q", b)
	}
}

// A pending report at the old place moves into its run dir; an id that cannot name a dir stays.
func TestMigratePendingMovesIntoTheRunDir(t *testing.T) {
	t.Setenv("REVIEW_LOOP_HOME", t.TempDir())
	repo := gitInit(t)
	info := filepath.Join(repo, ".git", "info")
	write(t, filepath.Join(info, "review-loop-pending-report.abc123.md"), "r\n")
	write(t, filepath.Join(info, "review-loop-pending-report.bad id.md"), "x\n")
	if err := MigratePending(repo, func(id string) bool { return !strings.Contains(id, " ") }); err != nil {
		t.Fatal(err)
	}
	rd, _ := RunDir("abc123")
	if b, _ := os.ReadFile(filepath.Join(rd, "pending-report.md")); string(b) != "r\n" {
		t.Errorf("run dir report = %q", b)
	}
	if _, err := os.Stat(filepath.Join(info, "review-loop-pending-report.bad id.md")); err != nil {
		t.Errorf("the refused id was moved or lost: %v", err)
	}
}

func TestNameKeepsOnlyPlainBytes(t *testing.T) {
	if got := name("caf\xe9 x/y"); got != "caf__x_y" {
		t.Errorf("name = %q", got)
	}
}

func TestBaseRefusesNoHome(t *testing.T) {
	t.Setenv("REVIEW_LOOP_HOME", "")
	t.Setenv("HOME", "")
	if p, err := Base(); err == nil {
		t.Errorf("Base = %q with no HOME, want an error", p)
	}
}
