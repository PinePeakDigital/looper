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
			// The \r half is defensive only, and deliberately untested: a trailing \r
			// sits OUTSIDE the JSON object, where encoding/json already tolerates it as
			// whitespace, so no assertion here can distinguish trimming it from not. A
			// test for it would pass either way, which is worse than no test.
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
	// Not `num(...) > 0`. This is the one guard where FALSY means "carry on to the
	// converged check", so a value num() cannot read — a string where a count belongs —
	// collapsed to 0 and waved the run through. Measured: `unresolved_asks: "7"` with a
	// zero-fix last cycle derived `converged` in Go while the Python raised TypeError. A
	// clean push, no disclosure, seven findings outstanding: this guard's own hole,
	// reopened through a type confusion rather than an ordering mistake.
	//
	// `asked` and `analysis_changed` below are safe from this by accident of polarity —
	// they are tested with `!truthy(...)`, so an unreadable value BLOCKS convergence. Only
	// this one had to be told.
	if asksOutstanding(r.Fields["unresolved_asks"]) {
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
//
// It also maps a NON-numeric value — a string where a count belongs — to zero, and that is
// a deliberate divergence from the Python rather than an oversight. The Python does
// arithmetic on the raw value, so `agents: "5"` raises TypeError and the process dies with
// a traceback instead of returning any of the four words; `agent_cap: "40"` likewise.
// Matching that exactly would mean reproducing an unhandled crash, which is not a decision
// the original made, just a place it has none. Degrading to zero is safe in the only
// direction that matters here — but only because the ONE site where a falsy reading waves
// a run through now refuses to use it. That was not true when this comment first claimed
// it: `unresolved_asks: "7"` read as 0 and derived `converged`. See asksOutstanding below.
// Everywhere else num() is reached, an unreadable value can only understate a capped run
// as halted, and both of those deny the push. Reviewed and kept 2026-10-03.
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
		// Unreachable in practice: json.Unmarshal into `any` yields only nil, bool,
		// float64, string, []any and map[string]any, and all six are handled above. Kept
		// because the compiler cannot know that, and because a Row can in principle be
		// built in Go rather than decoded.
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

// asksOutstanding answers "does the record say findings are still with the user", and it
// answers YES whenever it cannot tell. Every other reader in this file can degrade a value
// it cannot parse to zero, because zero there denies a push. Here zero GRANTS one: it is
// the falsy reading that lets Convergence() go on to award `converged`. So a present
// value that is neither a number nor empty — a string, a list, anything — counts as
// outstanding rather than as none. That diverges from the Python, which raises TypeError
// and dies, and the divergence is deliberate: both refuse the clean push, and this one
// also keeps reading the rest of the store.
func asksOutstanding(v any) bool {
	if f, ok := v.(float64); ok {
		return f > 0 // the ordinary case: a count
	}
	return truthy(v) // absent, null, 0 and "" are none; anything else is unreadable, so yes
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

// pyStr renders a JSON-decoded value the way Python's str() does. review-stats.py calls
// str() on a gate status EXPLICITLY (review-stats.py:197) before testing it or printing it,
// and its comment there records why: a non-string status is not rejected at write time, and
// formatting one into the message crashed the whole alarm with a TypeError. So this is the
// Python's own coercion, not an inference from how the text is later interpolated.
//
// Named for the operation rather than for the status, because the operation is not
// status-specific: disclosure() — the remaining piece of the read-and-derive half this
// package ports — renders raw record values at three sites the same way, and one of them
// (`asked`) can hold a list.
//
// Where it agrees with str(), and where it does not. Strings agree. Booleans needed the case:
// str(True) is "True" and fmt.Sprint(true) is "true". Numbers agree only for integer-valued
// JSON literals below 1e6 — JSON numbers decode to float64 here and to int or float there, so
// `5` agrees while `5.0` gives "5" against "5.0", `1000000` gives "1e+06" against "1000000",
// a JSON integer past 2^53 loses its digits entirely, and `-0` gives "-0" against "0".
// Containers disagree too: ['a'] against [a].
//
// The number and container gaps are left open deliberately, and no fixture claims otherwise.
// Closing them means either Python's float repr and arbitrary-precision ints in Go, or
// decoding the whole store with json.Decoder.UseNumber() — which would change what num(),
// truthy() and isZero() receive for every field, to buy parity on a status that is already a
// malformed record. The bool case earned its two lines because `status: false` is load-bearing
// (it collapses to "unreported") and `true` is its direct mirror.
func pyStr(raw any) string {
	if v, ok := raw.(bool); ok && v {
		return "True"
	}
	// No nil case and no string case: fmt.Sprint is already the identity on a string, and
	// DroppedGates replaces every falsy status — nil and false included — with "unreported"
	// before anything reads it, so a nil arm here would be unobservable. Verified by
	// mutation: a sentinel in either place survives the whole suite and the parity gate.
	return fmt.Sprint(raw)
}

// DroppedGates returns the planned-to-run gates that did not report a GateOK status,
// mapped to the status they did report ("unreported" when there is no entry at all).
//
// Both type assertions below have no parity fixture, because the Python has no answer to
// compare: review-stats.py does `(run.get("gates") or {}).items()` and `executed.get(g)`, so
// a truthy NON-mapping raises AttributeError there and takes the whole alarm down with it.
// Go degrades instead, and the two directions are not equally safe — which is why the
// direction is stated here, as num() and asksOutstanding() each state theirs:
//
//	executed: "nope"  -> Python raises; Go reports {"g":"unreported"}. Denies silence. Safe.
//	gates: ["g"]      -> Python raises; Go reports {}. GRANTS silence: a run whose gate
//	                     record is corrupt reports nothing dropped, and the alarm that
//	                     exists to notice a missing gate sees a clean run.
//
// The second is the bad direction and it is accepted knowingly: a crash is not available as
// a behaviour here, the shape is unreachable through cmd_finish (runlog.py:597 refuses a
// non-dict executed value), and inventing a sentinel gate name would put a word in the
// alarm that no gate has. record_test.go pins both answers so the direction cannot drift.
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
		status := pyStr(raw)
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
