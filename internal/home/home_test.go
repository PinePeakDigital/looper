package home

import (
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
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

// The migrated copy keeps the source's mode: 0600 stays private under a 077 umask, and 0644 is
// not narrowed to CreateTemp's 0600.
func TestMoveKeepsTheSourceMode(t *testing.T) {
	for _, mode := range []os.FileMode{0o600, 0o644} {
		d := t.TempDir()
		src, dst := filepath.Join(d, "src"), filepath.Join(d, "out", "dst")
		write(t, src, "x\n")
		if err := os.Chmod(src, mode); err != nil {
			t.Fatal(err)
		}
		if err := move(src, dst); err != nil {
			t.Fatal(err)
		}
		if fi, err := os.Stat(dst); err != nil || fi.Mode().Perm() != mode {
			t.Errorf("dst mode = %v, %v; want %v", fi.Mode(), err, mode)
		}
	}
}

// A copy that fails partway leaves no dst, so the old ledger is still the one read next time.
func TestMoveLeavesNoPartialFile(t *testing.T) {
	d := t.TempDir()
	src, dst := filepath.Join(d, "src"), filepath.Join(d, "dst")
	write(t, src, strings.Repeat("x", 128<<10))
	var old syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_FSIZE, &old); err != nil {
		t.Skip(err)
	}
	signal.Ignore(syscall.SIGXFSZ)
	defer signal.Reset(syscall.SIGXFSZ)
	lim := old
	lim.Cur = 32 << 10
	if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &lim); err != nil {
		t.Skip(err)
	}
	err := move(src, dst)
	if rerr := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &old); rerr != nil {
		t.Fatal(rerr)
	}
	if err == nil {
		t.Fatal("move succeeded past the file size limit")
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Errorf("a partial dst exists: %v", err)
	}
	if left, _ := filepath.Glob(filepath.Join(d, ".migrate-*")); len(left) != 0 {
		t.Errorf("temp files left: %v", left)
	}
	if _, err := os.Stat(src); err != nil {
		t.Errorf("src lost: %v", err)
	}
}

// One report that cannot move does not stop the rest, and the error names it.
func TestMigratePendingContinuesPastAFailure(t *testing.T) {
	t.Setenv("REVIEW_LOOP_HOME", t.TempDir())
	repo := gitInit(t)
	info := filepath.Join(repo, ".git", "info")
	write(t, filepath.Join(info, "review-loop-pending-report.aaa.md"), "a\n")
	write(t, filepath.Join(info, "review-loop-pending-report.bbb.md"), "b\n")
	ra, _ := RunDir("aaa")
	write(t, ra, "") // a file where the run dir should be
	err := MigratePending(repo, func(string) bool { return true })
	if err == nil || !strings.Contains(err.Error(), "pending-report.aaa.md left in place") {
		t.Errorf("err = %v", err)
	}
	rb, _ := RunDir("bbb")
	if b, _ := os.ReadFile(filepath.Join(rb, "pending-report.md")); string(b) != "b\n" {
		t.Errorf("bbb not moved: %q", b)
	}
}

// A run dir that already holds its report keeps it, and the old copy stays put.
func TestMigratePendingLeavesAnExistingReport(t *testing.T) {
	t.Setenv("REVIEW_LOOP_HOME", t.TempDir())
	repo := gitInit(t)
	old := filepath.Join(repo, ".git", "info", "review-loop-pending-report.abc123.md")
	write(t, old, "old\n")
	rd, _ := RunDir("abc123")
	write(t, filepath.Join(rd, "pending-report.md"), "new\n")
	if err := MigratePending(repo, func(string) bool { return true }); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(rd, "pending-report.md")); string(b) != "new\n" {
		t.Errorf("run dir report = %q, want it untouched", b)
	}
	if _, err := os.Stat(old); err != nil {
		t.Errorf("the old report was removed: %v", err)
	}
}

// A symlink or a directory at the old ledger path is not a ledger: nothing is copied and the
// source is left alone.
func TestLedgerIgnoresANonRegularOldFile(t *testing.T) {
	for _, kind := range []string{"symlink", "dir"} {
		t.Run(kind, func(t *testing.T) {
			t.Setenv("REVIEW_LOOP_HOME", t.TempDir())
			repo := gitInit(t)
			old := filepath.Join(repo, ".git", "info", "review-loop-deferred.md")
			if kind == "dir" {
				if err := os.MkdirAll(old, 0o755); err != nil {
					t.Fatal(err)
				}
			} else {
				target := filepath.Join(t.TempDir(), "elsewhere")
				write(t, target, "x\n")
				if err := os.Symlink(target, old); err != nil {
					t.Fatal(err)
				}
			}
			p, err := Ledger(repo, Deferred)
			if err != nil {
				t.Fatal(err)
			}
			if rd, _ := RepoDir(repo); p != filepath.Join(rd, Deferred) {
				t.Errorf("Ledger = %q, want the store path", p)
			}
			if _, err := os.Lstat(p); !os.IsNotExist(err) {
				t.Errorf("a store file was made: %v", err)
			}
			if _, err := os.Lstat(old); err != nil {
				t.Errorf("the old path was removed: %v", err)
			}
		})
	}
}

// A git that times out, or leaves its pipe held open, is an error, not "outside a repo": the
// old code fell back to the cwd and so named another store. A missing git is outside a repo.
func TestRepoDirRefusesAGitThatCannotAnswer(t *testing.T) {
	t.Setenv("REVIEW_LOOP_HOME", t.TempDir())
	repo := gitInit(t)
	real, err := exec.LookPath("git")
	if err != nil {
		t.Skip("no git on PATH")
	}
	path := os.Getenv("PATH")
	defer func(b time.Duration) { gitBudget = b }(gitBudget)
	// Each case has its own budget: below run's 1s drain bound the budget has always expired
	// by the time Run returns, so the held pipe must run under a longer one to reach !Drained.
	for name, c := range map[string]struct {
		body   string
		budget time.Duration
	}{
		"held pipe": {"sleep 3 &\nexec " + real + ` "$@"`, 5 * time.Second},
		"timeout":   {"exec sleep 30", 300 * time.Millisecond},
	} {
		gitBudget = c.budget
		body := c.body
		shim := t.TempDir()
		write(t, filepath.Join(shim, "git"), "#!/bin/sh\n"+body+"\n")
		if err := os.Chmod(filepath.Join(shim, "git"), 0o755); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", shim+string(os.PathListSeparator)+path)
		if p, err := RepoDir(repo); err == nil {
			t.Errorf("%s: RepoDir = %q, want an error", name, p)
		}
	}
	t.Setenv("PATH", t.TempDir())
	if _, err := RepoDir(repo); err != nil {
		t.Errorf("no git: RepoDir errored (%v), want the cwd-keyed store", err)
	}
}

// A common dir holding glob characters still has its reports found.
func TestMigratePendingUnderAGlobishRepo(t *testing.T) {
	t.Setenv("REVIEW_LOOP_HOME", t.TempDir())
	repo := filepath.Join(t.TempDir(), "r[1*")
	if out, err := exec.Command("git", "init", "-q", repo).CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	write(t, filepath.Join(repo, ".git", "info", "review-loop-pending-report.abc123.md"), "r\n")
	if err := MigratePending(repo, func(string) bool { return true }); err != nil {
		t.Fatal(err)
	}
	rd, _ := RunDir("abc123")
	if b, _ := os.ReadFile(filepath.Join(rd, "pending-report.md")); string(b) != "r\n" {
		t.Errorf("run dir report = %q", b)
	}
}
