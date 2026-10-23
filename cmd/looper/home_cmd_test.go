package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pinepeakdigital/looper/internal/home"
)

// `looper home` is what the skill's `$(looper home)/<ledger>.md` snippets expand: the repo's
// store, one line, under $REVIEW_LOOP_HOME.
func TestHomePrintsTheRepoStore(t *testing.T) {
	t.Chdir(fixture(t, map[string]string{"a.txt": "a\n"}))
	want, err := home.RepoDir("")
	if err != nil {
		t.Fatal(err)
	}
	code, out, errOut := doRun(t, "home")
	if code != 0 || out != want+"\n" {
		t.Fatalf("home = %d %q (stderr %q), want 0 %q", code, out, errOut, want+"\n")
	}
	if !strings.HasPrefix(out, os.Getenv("REVIEW_LOOP_HOME")+"/repos/") {
		t.Errorf("%q is not under $REVIEW_LOOP_HOME/repos", out)
	}
	t.Setenv("REVIEW_LOOP_HOME", "")
	t.Setenv("HOME", "")
	if code, out, _ := doRun(t, "home"); code == 0 {
		t.Errorf("with no home directory, home exited 0 and printed %q", out)
	}
}

// With no -findings-file, pr-report takes the run dir's narrative.md (narthur/skills#16: a
// builder that could not pass the flag posted a bare report). A flag still wins, and with no
// narrative.md the report renders without one rather than failing.
func TestPrReportDefaultsTheNarrativeToTheRunDir(t *testing.T) {
	store := prReportStore(t, "pr1")
	rd, err := home.RunDir("pr1")
	if err != nil {
		t.Fatal(err)
	}
	narr := filepath.Join(rd, "narrative.md")
	if err := os.MkdirAll(rd, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(narr) })
	if err := os.WriteFile(narr, []byte("DEFAULTED\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, out, _ := doRun(t, "pr-report", "-run-id", "pr1", "-store", store); !strings.Contains(out, "DEFAULTED") {
		t.Errorf("the run dir's narrative.md was not used:\n%s", out)
	}
	f := filepath.Join(t.TempDir(), "other.md")
	if err := os.WriteFile(f, []byte("FLAGGED\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, out, _ := doRun(t, "pr-report", "-run-id", "pr1", "-store", store, "-findings-file", f)
	if !strings.Contains(out, "FLAGGED") || strings.Contains(out, "DEFAULTED") {
		t.Errorf("-findings-file did not win over the default:\n%s", out)
	}
	if err := os.Remove(narr); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := doRun(t, "pr-report", "-run-id", "pr1", "-store", store)
	if code != 0 || strings.Contains(out, "DEFAULTED") {
		t.Errorf("with no narrative.md: exit %d, stderr %q, out:\n%s", code, errOut, out)
	}
}
