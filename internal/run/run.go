// Package run runs an external command under a budget and says how it ended. Every looper
// command that shells out to gh or git goes through it, so the rules for bounding a command
// live here once. Each caller maps a Result onto its own oracle's shape; what to make of an
// ending is the caller's policy, not this package's.
package run

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"syscall"
	"time"
)

// Result is how one command ended.
type Result struct {
	// Started is false when no process was created: the program is missing, not executable,
	// the wrong format, or the budget expired before the fork. `Cmd.Process` is set "once
	// started", so its absence is exactly that. Enumerating sentinels missed the wrong-format
	// binary (a *fs.PathError matching none of them); testing for a non-ExitError misfiled
	// the I/O errors os/exec reports for a process that did run.
	Started bool
	// Expired is true when the budget ran out, whether it killed the child or cancelled it
	// before the fork. Read after the drain, so a budget at or below the drain bound has
	// always expired by then. A missing binary is reported before the budget is consulted, so
	// test Error's identity, not Expired, to tell the two apart.
	Expired bool
	// Drained is false when the child exited cleanly but a descendant (a credential helper gh
	// starts, a shell's background job) held stdout or stderr past the drain bound. The exit
	// status is real; Out may hold the descendant's bytes, because it writes into the same fd.
	// A child that was killed or failed reports that instead, with Drained true.
	Drained bool
	// Code is the exit status, or minus the signal that killed the child, as Python's
	// returncode is. -1 when there is no exit status: the child never started, or waiting
	// for it failed.
	Code     int
	Out, Err []byte
	Error    error // what os/exec returned
}

// drainBound is how long Wait keeps reading after the child exits or is killed. Without it
// the budget bounds nothing: CommandContext kills the direct child only, and Wait blocks until
// every holder of the pipes closes them. Measured against a shim whose last line is
// `sleep 10 &`: a 300ms budget returned after 10.15s, with code 0. One second bounds the
// drain, not the deadline, and a healthy gh never reaches it.
const drainBound = time.Second

// Run runs name with args in dir ("" is the current directory), killing it when budget runs
// out.
func Run(dir string, budget time.Duration, name string, args ...string) Result {
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.WaitDelay = drainBound
	var o, e bytes.Buffer
	cmd.Stdout, cmd.Stderr = &o, &e
	err := cmd.Run()
	r := Result{Started: cmd.Process != nil, Expired: ctx.Err() != nil,
		Drained: !errors.Is(err, exec.ErrWaitDelay), Code: -1, Out: o.Bytes(), Err: e.Bytes(), Error: err}
	if cmd.ProcessState != nil {
		r.Code = cmd.ProcessState.ExitCode()
		if ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			r.Code = -int(ws.Signal())
		}
	}
	return r
}
