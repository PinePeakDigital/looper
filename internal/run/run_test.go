package run

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func shim(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "shim")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestHowACommandEnded(t *testing.T) {
	for _, c := range []struct {
		name, body string
		budget     time.Duration
		check      func(Result) bool
		within     time.Duration
	}{
		{"a clean exit", "echo hi", time.Minute,
			func(r Result) bool {
				return r.Started && !r.Expired && r.Drained && r.Code == 0 && string(r.Out) == "hi\n"
			}, 5 * time.Second},
		{"an exit status", "echo oops >&2; exit 3", time.Minute,
			func(r Result) bool { return r.Started && r.Code == 3 && string(r.Err) == "oops\n" }, 5 * time.Second},
		{"a signal", "kill -TERM $$", time.Minute,
			func(r Result) bool { return r.Started && r.Code == -15 }, 5 * time.Second},
		{"past the budget", "exec sleep 8", 200 * time.Millisecond,
			func(r Result) bool { return r.Started && r.Expired && r.Code == -9 }, 4 * time.Second},
		// A descendant holding the pipes after a clean exit: the status stands, the output is
		// not the child's alone, and the call returns a drain bound later, not when it exits.
		{"a lingering grandchild", "sleep 8 & echo 42", time.Minute,
			func(r Result) bool { return r.Started && !r.Expired && !r.Drained && r.Code == 0 }, 4 * time.Second},
		// The same descendant behind a killed child: the budget still bounds the call.
		{"a grandchild behind a killed child", "sleep 8 & exec sleep 8", 200 * time.Millisecond,
			func(r Result) bool { return r.Started && r.Expired }, 4 * time.Second},
	} {
		t.Run(c.name, func(t *testing.T) {
			start := time.Now()
			r := Run("", c.budget, shim(t, c.body))
			if took := time.Since(start); took > c.within {
				t.Errorf("took %s, want under %s", took, c.within)
			}
			if !c.check(r) {
				t.Errorf("got %+v (out %q)", r, r.Out)
			}
		})
	}
}

func TestWhatNeverStarted(t *testing.T) {
	dir := t.TempDir()
	notExec := filepath.Join(dir, "plain")
	wrongFormat := filepath.Join(dir, "text")
	if err := os.WriteFile(notExec, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(wrongFormat, []byte("not a program\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ name, prog string }{
		{"a missing binary", filepath.Join(dir, "nope")},
		{"a file that is not executable", notExec},
		{"a binary of the wrong format", wrongFormat},
	} {
		t.Run(c.name, func(t *testing.T) {
			if r := Run("", time.Minute, c.prog); r.Started || r.Expired || r.Code != -1 || r.Error == nil {
				t.Errorf("got %+v", r)
			}
		})
	}
	// The budget cancels before the fork: not started, expired, and the error is the context's.
	t.Run("a budget that expires before the fork", func(t *testing.T) {
		r := Run("", time.Nanosecond, shim(t, "echo hi"))
		if r.Started || !r.Expired || !errors.Is(r.Error, context.DeadlineExceeded) {
			t.Errorf("got %+v", r)
		}
	})
}
