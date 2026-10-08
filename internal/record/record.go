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

// The store's schema, typed PER PHASE rather than as one flat row, because the same key
// means different things in different phases. `agents` is a COUNT on a cycle row and an
// operator-supplied roster on a finish row — one real run recorded
// `{"cycle1":6,"cycle2":3,"scorers":"batched haiku"}` — and a flat map[string]any hid that
// difference behind one name.
//
// Only the fields something derives from are modelled. Unknown fields decode away silently,
// which is deliberate: the store is append-only and carries plenty this package never
// reads, and a struct that refused them would break on every field a future writer adds.
//
// Typed at the boundary, which is the whole point of this layer. A field that is present
// but the wrong shape is an ERROR naming the run and the field, not a value coerced into
// a verdict. Measured against the real store before choosing this: across 153 rows every
// numeric field is a number in every row, so the shapes the old coercion helpers guarded
// against never actually occur — while ABSENCE and null are everywhere (`subagent_tokens`
// null in 46 of 57 cycles, `agent_cap` absent in 18 of 38 plans). So the real work is
// telling absent from null from zero, which is exactly what the pointers below do and
// exactly the distinction `converged` turns on.
type GateSpec struct {
	Planned string `json:"planned"`
}

// GateResult.Status is a pointer so that absent, null and "" stay distinguishable here and
// are collapsed by the one reader that wants them collapsed. A status that is not a string
// is a decode error: the Python rendered `status: true` as "True" and a number as itself,
// and reproducing that meant a str()-alike with number and container gaps that could never
// be closed. Refusing the shape is both simpler and louder.
type GateResult struct {
	Status *string `json:"status"`
}

// Plan is the planned half of a run, written once by cmd_plan.
type Plan struct {
	AgentCap *int                `json:"agent_cap"`
	Gates    map[string]GateSpec `json:"gates"`
}

// Cycle is one pass of the loop. Every count is a pointer because absence is load-bearing:
// `applied` MISSING must not read as zero, or a cycle that recorded nothing earns the
// `converged` that absence was hiding.
type Cycle struct {
	N               *int  `json:"n"`
	Applied         *int  `json:"applied"`
	Asked           *int  `json:"asked"`
	Agents          *int  `json:"agents"`
	AnalysisChanged *bool `json:"analysis_changed"`
}

// Finish is the terminal row. Agents stays raw because its schema is the operator's: the
// `--agents` flag takes arbitrary JSON and the store holds both lists and objects for it.
// Typing it would reject real rows to no benefit, since nothing derives from it.
type Finish struct {
	Outcome        string                `json:"outcome"`
	UnresolvedAsks *int                  `json:"unresolved_asks"`
	Executed       map[string]GateResult `json:"executed"`
	Agents         json.RawMessage       `json:"agents"`
}

// Run is one run's phases, merged the way load() merges them.
type Run struct {
	ID     string
	Plan   *Plan
	Finish *Finish
	// Cycles ACCUMULATE where every other phase merges. A run has one plan and one
	// finish, so merging is right for those — but it would make each cycle clobber the
	// last, leaving only the final one and destroying the sequence convergence is
	// derived from.
	//
	// No deduplication by `n`, deliberately. It used to dedupe, last write winning, on the
	// theory that a corrected row sits after the one it corrects — but nothing in the
	// skill corrects a cycle row, and the only thing that revisits an `n` is the Step 13
	// restart, which resets the counter to 1. The dedupe served a corrector that does not
	// exist while deleting the first pass of every restart: rows n=1/20, n=2/15, n=1/5
	// counted 20 of the 40 agents spent. Every reader here takes this one list, which is
	// also a fix: disclosure() once read the raw rows while convergence() read the deduped
	// ones, so a rendered report contradicted its own derivation.
	Cycles []Cycle
	// Err is the first field in THIS run that could not be decoded. Non-nil means no
	// answer derived from this run is trustworthy, so every derivation below returns it
	// rather than a verdict.
	//
	// Per run, not per store: the record is append-only and shared by every repo on the
	// machine, so one bad row written months ago must not block reads of unrelated runs.
	// But nothing may derive a verdict from a field it could not read, which is what the
	// coercion helpers this replaced did — `unresolved_asks: "7"` read as 0 and bought a
	// `converged` with seven findings outstanding.
	Err error
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
		// Two-stage: the envelope first, so a row for one run can never fail another's
		// decode, then the phase body into its own type.
		var env struct {
			RunID string `json:"run_id"`
			Phase string `json:"phase"`
		}
		if err := json.Unmarshal([]byte(line), &env); err != nil {
			continue // a torn line never invalidates the rest of the store
		}
		if env.RunID == "" {
			continue
		}
		r := runs[env.RunID]
		if r == nil {
			r = &Run{ID: env.RunID}
			runs[env.RunID] = r
		}
		switch env.Phase {
		case "cycle":
			var c Cycle
			if err := json.Unmarshal([]byte(line), &c); err != nil {
				r.setErr(env.Phase, err)
				continue
			}
			r.Cycles = append(r.Cycles, c)
		case "plan":
			var pl Plan
			if err := json.Unmarshal([]byte(line), &pl); err != nil {
				r.setErr(env.Phase, err)
				continue
			}
			r.Plan = &pl
		case "finish":
			var fi Finish
			if err := json.Unmarshal([]byte(line), &fi); err != nil {
				r.setErr(env.Phase, err)
				continue
			}
			r.Finish = &fi
		}
		// Any other phase — `nudge` today — carries nothing this package derives from, so
		// it is skipped rather than modelled. It still created the Run above, which is
		// what runlog.load does: a run known only by a nudge exists and has no cycles.
	}
	return runs, nil
}

// setErr keeps the FIRST decode failure. The first names the field a reader should go fix;
// later ones are usually the same row read again by another phase's body.
func (r *Run) setErr(phase string, err error) {
	if r.Err == nil {
		r.Err = fmt.Errorf("run %s: %s row has an unreadable field: %w", r.ID, phase, err)
	}
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
func (r *Run) Convergence() (string, error) {
	if r.Err != nil {
		return Unknown, r.Err
	}
	if len(r.Cycles) == 0 {
		return Unknown, nil
	}
	last := r.Cycles[len(r.Cycles)-1]

	// Before the converged test, not after. Asks recorded at finish are outstanding work
	// even when no cycle row carries them, and placed after, the converged branch
	// short-circuited this and the hole stayed open: `cycle --applied 0` with no --asked,
	// then `finish --asks 7`, derived converged with no disclosure while seven findings
	// sat unresolved and the report said nothing was left to apply.
	//
	// This used to need a helper that answered YES whenever it could not tell, because it
	// is the one guard where a falsy reading WAVES A RUN THROUGH — `unresolved_asks: "7"`
	// coerced to 0 and bought a clean push with seven findings open. Typing removes the
	// question: a non-numeric value here is a decode error on the run, caught above, so
	// there is no unreadable value left for this test to misread.
	if r.Finish != nil && r.Finish.UnresolvedAsks != nil && *r.Finish.UnresolvedAsks > 0 {
		return Halted, nil
	}

	// Converged means the loop stopped with nothing left to do — all three halves. The
	// deterministic pass changing files is as much unfinished work as a non-empty
	// auto-fix bucket, and so is the ask bucket: a cycle that routed every finding to the
	// user and resolved none has not run out of findings, it has run out of what it may
	// do unattended.
	//
	// `Applied != nil && *Applied == 0`, which is the Python's `last.get("applied") == 0`
	// exactly: `None == 0` is False, so a MISSING or null `applied` is not zero here —
	// unlike the `or 0` sites below. Reading absence as zero made a cycle row with no
	// `applied` field derive `converged` where the Python derives `halted`, which is the
	// one direction that matters: it buys a clean result, with no disclosure, by omitting
	// a field. That is the hole the unresolved_asks check above exists to close, reopened
	// one line down. The pointer is what makes the distinction unmissable now.
	if last.Applied != nil && *last.Applied == 0 &&
		(last.Asked == nil || *last.Asked == 0) &&
		(last.AnalysisChanged == nil || !*last.AnalysisChanged) {
		return Converged, nil
	}

	var spent int
	for _, c := range r.Cycles {
		if c.Agents != nil {
			spent += *c.Agents // absent reads as zero here, matching the Python's `or 0`
		}
	}
	// `!= 0`, not `> 0`: the Python guard is a bare `if cap`, so a negative cap is truthy
	// there and any spend clears it. cmd_plan refuses a non-positive --agent-cap, so no
	// NEW row can carry one, but the store is append-only and never rewritten, so a
	// legacy or hand-written row still reads differently in the two implementations.
	if r.Plan != nil && r.Plan.AgentCap != nil && *r.Plan.AgentCap != 0 && spent >= *r.Plan.AgentCap {
		return Capped, nil
	}

	// Stopped with work outstanding and budget left: an operator interrupt, a test
	// failure, or a run that simply stopped.
	return Halted, nil
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
// mapped to the status they did report ("unreported" when there is none).
//
// The two isinstance guards the Python needs here are gone with the types: a gate spec that
// is a bare string, or an executed entry that is a bare string instead of an object, is now
// a decode error on the run rather than a shape each reader has to guard. That removes the
// asymmetry the old port had to document — a non-dict `executed` denied silence while a
// non-dict `gates` granted it, reporting nothing dropped for a run whose gate record was
// corrupt. Neither is reachable now.
func (r *Run) DroppedGates() (map[string]string, error) {
	if r.Err != nil {
		return nil, r.Err
	}
	out := map[string]string{}
	if r.Plan == nil {
		return out, nil
	}
	for name, spec := range r.Plan.Gates {
		if spec.Planned != "run" {
			continue // only `run` is a planned gate; `skip` was never going to happen
		}
		// Absent, null and "" all collapse to "unreported", which is the Python's
		// `st or "unreported"`. Defaulting only on ABSENCE reported "" for a gate that
		// recorded an empty status, and an empty status names nothing about the gate —
		// the status is the whole content of the alarm line.
		status := "unreported"
		if r.Finish != nil {
			if res, ok := r.Finish.Executed[name]; ok && res.Status != nil && *res.Status != "" {
				status = *res.Status
			}
		}
		if gateOK(status) {
			continue
		}
		out[name] = status
	}
	return out, nil
}
