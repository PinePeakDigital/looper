// Package record reads the review-loop run record and derives the answers its consumers
// act on. It is a port of the read-and-derive half of runlog.py, which is the half whose
// answers gate a push.
//
// Ported rather than reimplemented: every derivation here has a reason recorded in
// runlog.py's own comments, usually naming the incident that produced it, and those
// reasons are reproduced here rather than summarised. A port that drops them invites the
// next reader to simplify a rule back into the bug it exists to prevent.
//
// The parity test in this package runs the Python against the same fixtures and requires
// identical answers. That gate is the point: until the Python is retired, the two must
// not be able to disagree.
package record

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
)

// Row is one line of the append-only store. The record's vocabulary is open — cmd_finish
// requires a reason for any status but `done` and never validates the string — so fields
// stay as decoded JSON rather than a struct that would quietly drop what it did not model.
type Row map[string]any

// Run is one run's phases, merged the way load() merges them.
type Run struct {
	ID     string
	Fields Row
	// Cycles ACCUMULATE where every other phase merges. A run has one plan and one
	// finish, so merging is right for those — but it would make each cycle clobber the
	// last, leaving only the final one and destroying the sequence convergence is
	// derived from.
	Cycles []Row
}

// DefaultTail matches runlog.TAIL_LINES. Reading the whole store is the right default for
// anything that must find one specific run: a check that silently sees no plan because the
// record scrolled past the tail is worse than a slow one.
const DefaultTail = 4000

// Load merges the store into one Run per run_id. limit <= 0 reads everything.
func Load(path string, limit int) (map[string]*Run, error) {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return map[string]*Run{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()

	// Read with a bufio.Reader, not a Scanner. A Scanner caps the line length and then
	// reports ErrTooLong, which aborted the WHOLE load and discarded every run already
	// parsed — the exact opposite of the torn-line tolerance below, and on the same
	// input: a half-written line from a concurrent writer is where an over-long read
	// comes from. runlog.py iterates the file with no ceiling on record size, and its
	// own comment says records are unbounded, so a ceiling here is a parity break too.
	var lines []string
	br := bufio.NewReader(f)
	for {
		line, err := br.ReadString('\n')
		if line != "" {
			lines = append(lines, strings.TrimRight(line, "\r\n"))
		}
		if err != nil {
			if err == io.EOF {
				break
			}
			return nil, err // a real I/O error still surfaces; only size does not
		}
	}
	if limit > 0 && len(lines) > limit {
		lines = lines[len(lines)-limit:]
	}

	runs := map[string]*Run{}
	for _, line := range lines {
		var rec Row
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue // a torn line never invalidates the rest of the store
		}
		rid, _ := rec["run_id"].(string)
		if rid == "" {
			continue
		}
		r := runs[rid]
		if r == nil {
			r = &Run{ID: rid, Fields: Row{}}
			runs[rid] = r
		}
		if phase, _ := rec["phase"].(string); phase == "cycle" {
			r.Cycles = append(r.Cycles, rec)
			continue
		}
		for k, v := range rec {
			r.Fields[k] = v
		}
	}
	return runs, nil
}

// Convergence values. Unknown is the zero value on purpose: a run with no cycle rows
// derives it, and every consumer must treat it as "did not converge", so omitting the
// rows can never buy a push.
const (
	Unknown   = "unknown"
	Converged = "converged"
	Capped    = "capped"
	Halted    = "halted"
)

// Convergence reports why the loop stopped: out of findings, out of budget, or neither.
//
// Derived, never accepted. push-check.py used to take `--clean-exit` as a flag, so the one
// safety question — was this finished being reviewed — was answered by the orchestrator
// asserting it. One real run recorded outcome `clean` while its own author reported it had
// not converged, which is exactly what that allows.
func (r *Run) Convergence() string {
	if len(r.Cycles) == 0 {
		return Unknown
	}
	last := r.Cycles[len(r.Cycles)-1]

	// Before the converged test, not after. Asks recorded at finish are outstanding work
	// even when no cycle row carries them, and placed after, the converged branch
	// short-circuited this and the hole stayed open: `cycle --applied 0` with no --asked,
	// then `finish --asks 7`, derived converged with no disclosure while seven findings
	// sat unresolved and the report said nothing was left to apply.
	if num(r.Fields["unresolved_asks"]) > 0 {
		return Halted
	}

	// Converged means the loop stopped with nothing left to do — all three halves. The
	// deterministic pass changing files is as much unfinished work as a non-empty
	// auto-fix bucket, and so is the ask bucket: a cycle that routed every finding to the
	// user and resolved none has not run out of findings, it has run out of what it may
	// do unattended.
	// `isZero`, not `num(...) == 0`. The Python is `last.get("applied") == 0`, where a
	// MISSING or null `applied` is None and `None == 0` is False — absence does NOT read
	// as zero here, unlike the `or 0` sites above and below. Routing this through num(),
	// which maps absent and null to 0, made a cycle row with no `applied` field derive
	// `converged` where the Python derives `halted`. That is the one direction that
	// matters: it buys a clean result, with no disclosure, by omitting a field — which is
	// the hole the unresolved_asks check above was added to close, reopened one line down.
	if isZero(last["applied"]) && !truthy(last["asked"]) && !truthy(last["analysis_changed"]) {
		return Converged
	}

	cap := num(r.Fields["agent_cap"])
	var spent float64
	for _, c := range r.Cycles {
		spent += num(c["agents"])
	}
	// `cap != 0`, not `cap > 0`: the Python guard is a bare `if cap`, so a negative cap is
	// truthy there and any spend clears it. cmd_plan refuses a non-positive --agent-cap,
	// so no NEW row can carry one, but the store is append-only and never rewritten, so a
	// legacy or hand-written row still reads differently in the two implementations.
	if cap != 0 && spent >= cap {
		return Capped
	}

	// Stopped with work outstanding and budget left: an operator interrupt, a test
	// failure, or a run that simply stopped.
	return Halted
}

// num reads a JSON number, treating absent and null as zero. json.Unmarshal into `any`
// gives float64 for every number, so there is one numeric type to handle.
func num(v any) float64 {
	f, _ := v.(float64)
	return f
}

// truthy follows Python's `not x` for the values this record actually holds: absent, null,
// 0 and false are falsy. A non-zero count or true is not.
func truthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case float64:
		return t != 0
	case string:
		return t != ""
	case []any:
		return len(t) != 0 // Python: an empty list is falsy
	case map[string]any:
		return len(t) != 0 // Python: an empty dict is falsy
	default:
		return v != nil
	}
}

// isZero mirrors Python's `x == 0` rather than its `not x`. The two differ on exactly the
// case that matters: absence. `None == 0` is False, so a missing key is NOT zero — while
// `False == 0` is True, so a JSON false is. Nothing else in this record compares this way;
// `applied` is the one site, and it is the one where reading absence as zero awards a
// `converged` the run did not earn.
func isZero(v any) bool {
	switch t := v.(type) {
	case float64:
		return t == 0
	case bool:
		return !t // Python: False == 0 is True, True == 0 is False
	default:
		return false // absent, null, a string, a list: none of them equal 0 in Python
	}
}

// GateOK are the statuses that mean a planned gate was handled. `n/a` is success, not a
// drop: a gate that cannot apply is not a gate that was dropped. When this had its own
// `status == "done"` test instead, every `n/a` counted as dropped and the Step 0 alarm
// fired permanently on gates nobody could fix — which teaches a reader to scroll past the
// alarms that are right.
var GateOK = []string{"done", "n/a"}

func gateOK(status string) bool {
	for _, s := range GateOK {
		if s == status {
			return true
		}
	}
	return false
}

// DroppedGates returns the planned-to-run gates that did not report a GateOK status,
// mapped to the status they did report ("unreported" when there is no entry at all).
func (r *Run) DroppedGates() map[string]string {
	planned := map[string]bool{}
	if g, ok := r.Fields["gates"].(map[string]any); ok {
		for name, v := range g {
			if spec, ok := v.(map[string]any); ok {
				if p, _ := spec["planned"].(string); p == "run" {
					planned[name] = true
				}
			}
		}
	}
	executed, _ := r.Fields["executed"].(map[string]any)
	out := map[string]string{}
	for name := range planned {
		var raw any
		if e, ok := executed[name].(map[string]any); ok {
			raw = e["status"]
		}
		status := ""
		if raw != nil {
			status = fmt.Sprint(raw)
		}
		if gateOK(status) {
			continue
		}
		// The Python stores `st or "unreported"`, so EVERY falsy status collapses to
		// "unreported" — not just an absent entry, but also "", 0 and false. Defaulting
		// only on absence reported `""`, `"0"` and `"false"` instead, which is both a
		// parity break and a worse alarm line: the status is what names the gate's
		// problem, and an empty one names nothing. None of these is hypothetical — the
		// vocabulary is open (cmd_finish never validates the status string) and a
		// non-string status reaching this path has already happened once.
		if !truthy(raw) {
			status = "unreported"
		}
		out[name] = status
	}
	return out
}
