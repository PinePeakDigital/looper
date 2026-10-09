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
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
)

// The store's schema, typed PER PHASE rather than as one flat row, because the same key
// means different things in different phases. `agents` is a COUNT on a cycle row and an
// operator-supplied roster on a finish row — one real run recorded
// `{"cycle1":6,"cycle2":3,"cycle3":2,"scorers":"batched haiku"}` — and a flat map hid that
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
// null in 46 of 57 cycles, `agent_cap` absent in 18 of 38 plans (measured at 8c07d63;
// the store is append-only, so these only drift upward)). So the real work is
// telling ABSENT-OR-NULL from zero, which is what the pointers below do and is the
// distinction `converged` turns on. Not three states: a *int is nil for absent and for
// null alike, and no reader here wants them apart. `disclosure()` is the first function
// that would — Python renders absent as `?` and null as `None` — and that divergence is
// decided there, on the measurement that no row in the store holds a null value in any
// count a derivation READS: `applied`, `asked`, `agents`, `unresolved_asks`, `n` and
// `agent_cap` have zero nulls between them. Not "no null count" flatly — `subagent_tokens`
// is null in 47 of 58 cycle rows, which the lines above already say, and nothing here
// derives from it.
type GateSpec struct {
	Planned string `json:"planned"`
}

// GateResult.Status is a pointer, which separates "" from absent-or-null — NOT all three.
// encoding/json leaves a pointer nil both for JSON null and for an absent key, so absent and
// null are indistinguishable by construction, and the one reader collapses "" in with them
// anyway. A plain string would behave identically today; the pointer is kept because it makes
// the collapse explicit at the reader rather than implicit in a zero value. A status that is not a string
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

// Cycle is one pass of the loop. Applied is a pointer because ITS absence is load-bearing:
// `applied` MISSING must not read as zero, or a cycle that recorded nothing earns the
// `converged` that absence was hiding. The others are pointers for uniformity, not for that
// reason — Asked and Agents both read absence AS zero, which is the Python's `or 0`.
type Cycle struct {
	Applied         *int  `json:"applied"`
	Asked           *int  `json:"asked"`
	Agents          *int  `json:"agents"`
	AnalysisChanged *bool `json:"analysis_changed"`
}

// Finish is the terminal row. Agents stays raw because `--agents` is a bare json.loads with
// no shape validation and the store holds 26 lists and 1 object for it, so typing it would
// reject real rows. Nothing in THIS package derives from it — runlog's own derive_tier does,
// reading `status` and `id` out of it to force `tier_executed: "partial"`, so the schema is
// documented in runlog's --agents help rather than being genuinely the operator's.
type Finish struct {
	UnresolvedAsks *int                  `json:"unresolved_asks"`
	Executed       map[string]GateResult `json:"executed"`
	Agents         json.RawMessage       `json:"agents"`
}

// Run is one run's phases, merged the way load() merges them — with one measured, unreachable
// exception. load() merges a non-cycle row into the run dict FIELD-WISE (`run.update(rec)`),
// where Plan and Finish below are replaced wholesale, so a SECOND plan row omitting a key the
// first carried keeps that key in Python and loses it here. It cannot happen from the writer:
// cmd_plan writes every key it owns on every call (`agent_cap` has DEFAULT_AGENT_CAP = 40 and
// type=int, `gates` is always written), as does cmd_finish, so repeated rows always carry
// identical key sets. Verified on the real store — the two runs that do have duplicate finish
// rows carry the same keys in each, so merge and last-wins give the same answer — and a
// differential run during review found it in 2 synthetic stores, both hand-built (that run
// is not reproducible from this repo; what is checkable here is the real-store half above). Cycle rows
// are not affected: load() appends those and merges nothing, which is what Cycles below does.
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
	// Err is every field in THIS run that could not be decoded, joined. Non-nil means no
	// answer derived from this run is trustworthy, so every derivation below returns it
	// rather than a verdict.
	//
	// It also means the DATA is incomplete, not merely suspect: a row that failed to decode
	// was skipped, so Cycles above is short by one for each bad cycle row, and Plan or Finish
	// is nil where the row existed but was unreadable — byte-identical to the row never
	// having been written. Nothing on this Run may be read while Err is non-nil, the exported
	// slices included; that is why every derivation checks it first instead of leaving the
	// check to the caller.
	//
	// Per run, not per store: the record is append-only and shared by every repo on the
	// machine, so one bad row written months ago must not block reads of unrelated runs.
	// But nothing may derive a verdict from a field it could not read. The case that names
	// the hazard is `unresolved_asks: "7"` reading as 0 and buying a `converged` with seven
	// findings outstanding — which belonged to the port BEFORE asksOutstanding existed, not
	// to the code this commit replaces. asksOutstanding was written to fix exactly that and
	// did: at HEAD~1 it answered "outstanding" whenever it could not read the value. Typing
	// retires the rule rather than restating it, but it is not rescuing that defect from the
	// code it deletes, and saying so would be the flattering version.
	Err error
	// errN counts every failed row, including those past maxRunErrs that Err does not list.
	errN int
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
	//
	// What this does NOT match is RETENTION, and the parity argument above covers only
	// record size. `runlog.py:136` is `deque(fh, maxlen=limit)`, which streams and holds at
	// most `limit` lines; this holds every line and applies the limit afterwards, so
	// retention is O(file) — measured at roughly 3-4x file size resident (a 200 MiB store
	// costs ~600 MiB to return one run with limit=1). Deliberately not fixed: every caller
	// passes 0, which reads the whole store by design, so a ring buffer would bound a path
	// nothing takes. The narrow claim is the true one — `limit` bounds what is RETURNED, not
	// what is read. Fix the retention when a caller first passes a limit.
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
	// How many lines the tail dropped, so the number in an error is the line an operator
	// can `sed -n Np` out of the real file. Computed before the slice, because after it
	// the index is relative to the window and names the wrong row.
	dropped := 0
	if limit > 0 && len(lines) > limit {
		dropped = len(lines) - limit
		lines = lines[dropped:]
	}

	runs := map[string]*Run{}
	for i, line := range lines {
		lineNo := dropped + i + 1
		// Two-stage: the envelope first, so a row for one run can never fail another's
		// decode, then the phase body into its own type.
		var env struct {
			RunID string          `json:"run_id"`
			Phase json.RawMessage `json:"phase"`
		}
		if err := json.Unmarshal([]byte(line), &env); err != nil {
			// A torn line never invalidates the rest of the store. This also swallows
			// well-formed JSON that has no usable `run_id` — `"run_id": 7`, a top-level
			// array, a bare string — and for those there is genuinely nowhere to hang an
			// error, since the row belongs to no run. (The Python does not drop them: it
			// keys a run by the int 7, and it DIES outright on an array or a string,
			// `AttributeError` on `rec.get`. So this is more tolerant than the oracle, not
			// less, and tolerance of an unattributable row cannot forge a verdict because
			// the row contributes to nothing.) A wrong-typed `phase` is a different case
			// and no longer comes through here — see the switch below.
			continue
		}
		if env.RunID == "" {
			continue // `if not rid: continue`; without it these collect under a phantom ""
		}
		r := runs[env.RunID]
		if r == nil {
			r = &Run{ID: env.RunID}
			runs[env.RunID] = r
		}
		// A phase this build cannot read is an ERROR, not a row to skip, and having that
		// backwards was a hole in the one direction this layer exists to close.
		//
		// load() merges every non-cycle row into the run dict FIELD-WISE and never checks
		// what `phase` says, so a finish row whose phase is misspelled, wrong-typed or
		// absent still delivers `unresolved_asks` to the Python's verdict. Measured, on a
		// store whose finish row carries 7 asks outstanding:
		//
		//	phase "finish"    python=halted   go=halted
		//	phase "finnish"   python=halted   go=CONVERGED
		//	phase 7           python=halted   go=CONVERGED
		//	phase absent      python=halted   go=CONVERGED
		//
		// Three ways to forge a clean verdict on a run with outstanding work by misspelling
		// one word. A review did report this and it was dismissed as parity on the strength
		// of the CYCLE-row case, where both sides really do read converged because such a
		// row's only contribution is to `cycles`. The finish row was never checked, and it
		// is the one carrying the field the push gate reads.
		//
		// Erroring rather than reproducing the merge: both answers deny the push (Python
		// halted, here unknown-with-an-error), and reproducing it would mean restoring the
		// map[string]any this port deleted, to interpret a row no writer emits.
		var phase string
		if err := json.Unmarshal(env.Phase, &phase); err != nil {
			r.setErr(lineNo, "unreadable-phase", fmt.Errorf(
				"phase is absent or not a string, so the row's fields cannot be placed: %w", err))
			continue
		}
		switch phase {
		case "cycle":
			var c Cycle
			if err := json.Unmarshal([]byte(line), &c); err != nil {
				r.setErr(lineNo, phase, err)
				continue
			}
			r.Cycles = append(r.Cycles, c)
		case "plan":
			var pl Plan
			if err := json.Unmarshal([]byte(line), &pl); err != nil {
				r.setErr(lineNo, phase, nameBadGates(line, "gates", err, func(v json.RawMessage) error {
					var spec GateSpec
					return json.Unmarshal(v, &spec)
				}))
				continue
			}
			r.Plan = &pl
		case "finish":
			var fi Finish
			if err := json.Unmarshal([]byte(line), &fi); err != nil {
				r.setErr(lineNo, phase, nameBadGates(line, "executed", err, func(v json.RawMessage) error {
					var res GateResult
					return json.Unmarshal(v, &res)
				}))
				continue
			}
			r.Finish = &fi
		case "nudge":
			// Carries nothing this package derives from (`nudged_at` only), and the run was
			// already created above — which is what runlog.load's setdefault does: a run
			// known only by a nudge exists and has no cycles. 17 rows in the real store.
		default:
			// Not nudge, not a modelled phase. The Python merges this row's fields into the
			// run regardless of the word, so skipping it silently is how the forgery above
			// happens. The phase name is the operator's whole lead.
			r.setErr(lineNo, "unknown-phase", fmt.Errorf(
				"phase %q is not one this build models", phase))
		}
	}
	return runs, nil
}

// nameBadGates says WHICH gate failed, because encoding/json never names a map key. A
// finish row with eight gates and one unreadable status produced
// "cannot unmarshal number into Go struct field GateResult.executed.status of type string",
// which tells an operator the field and leaves them to guess the gate out of eight — and
// the whole run is refused, so none of the other seven statuses is readable either. That
// combination is the one place the typed boundary is less diagnosable than the coercion it
// replaced, and the gate name is what closes it.
//
// Runs on the error path only, so a healthy row pays nothing.
//
// Three things here were wrong when this function was first written, all found by review of
// the commit that added it, and all of them things it had itself just fixed elsewhere:
//
//   - The names were interpolated with %s. Gate names come from the same unvalidated writer
//     as `run_id` — `cmd_finish` checks the container and demands a reason, and validates
//     neither the gate NAME nor the status type — so the injection %q was added to setErr to
//     close was reopened two format verbs below it. Reproduced through the real writer: a
//     gate named "\x1b[2K\rGATE OK: converged" exits 0, lands in the store, and erases the
//     line it is printed on; a gate name containing a newline forges a whole second entry,
//     because errors.Join already uses \n as its separator. Every name is %q now, which
//     also disambiguates a name containing a comma from two names and renders an empty name
//     as "" rather than as the bare `gate(s) :` the old comment promised was impossible.
//   - It wrapped unconditionally, so a row whose `unresolved_asks` was bad and whose gates
//     were merely also bad rendered `executed gate(s) "g": <error about unresolved_asks>`.
//     The `X: Y` form asserts Y is why X; it was not. The UnmarshalTypeError's own Field is
//     the check, and the bail-out below means a gate is named only when the row really did
//     fail on this map.
//   - Nothing bounded it. A store with many bad rows, or one 1 MiB gate name, produced a
//     multi-megabyte "message" — measured at 13 MB for 200 rows of 64 KiB names. The caps
//     below bound a single row's contribution; setErr bounds the number of rows.
func nameBadGates(line, field string, err error, decode func(json.RawMessage) error) error {
	// Only this field's failure may name this field's gates. Anything else — a different
	// field, or an error that is not a type error at all — goes back untouched.
	var ute *json.UnmarshalTypeError
	if !errors.As(err, &ute) || (ute.Field != field && !strings.HasPrefix(ute.Field, field+".")) {
		return err
	}
	var top map[string]json.RawMessage
	if json.Unmarshal([]byte(line), &top) != nil {
		return err
	}
	raw, ok := top[field]
	if !ok {
		return err
	}
	var entries map[string]json.RawMessage
	if json.Unmarshal(raw, &entries) != nil {
		return err // the map's own type is wrong; json's message already covers that
	}
	var bad []string
	for name, v := range entries {
		if decode(v) != nil {
			bad = append(bad, name)
		}
	}
	if len(bad) == 0 {
		return err
	}
	sort.Strings(bad) // map order is random; an error message must not be

	extra := 0
	if len(bad) > maxNamedGates {
		extra, bad = len(bad)-maxNamedGates, bad[:maxNamedGates]
	}
	quoted := make([]string, 0, len(bad)+1)
	for _, name := range bad {
		if len(name) > maxGateNameLen {
			name = name[:maxGateNameLen] + "..."
		}
		quoted = append(quoted, strconv.Quote(name))
	}
	if extra > 0 {
		quoted = append(quoted, fmt.Sprintf("and %d more", extra))
	}
	return fmt.Errorf("%s gate(s) %s: %w", field, strings.Join(quoted, ", "), err)
}

const (
	// Caps on one row's contribution to an operator message. A real plan has 14 gates, so
	// naming more than this is already a corrupt row rather than a thing to read, and a gate
	// name longer than this is not a gate name.
	maxNamedGates  = 8
	maxGateNameLen = 60
	// And on the number of rows a single run may contribute. Accumulating is right — the
	// first error alone hid `unresolved_asks` behind `n` — but a store with 10,000 bad rows
	// for one run produced a 1.3 MB error, and nothing downstream is prepared to print that.
	maxRunErrs = 20
)

// setErr accumulates every decode failure on the run. It used to keep only the first, on
// the stated ground that "later ones are usually the same row read again by another phase's
// body" — which describes code that does not exist: `switch env.Phase` has one arm per
// phase, so every line decodes into exactly one type and is never re-read. What keeping the
// first actually did was pick by position: encoding/json reports whichever bad field comes
// first in the WRITER's key order, and across rows the first bad row in file order. Measured,
// that put `n` (which nothing derived from) in the message while hiding `unresolved_asks` —
// the one field whose misreading motivated this whole layer — and an append-only store with
// no repair tooling turns each hidden error into another hand-edit round trip.
//
// %q on the id, not %s. `run_id` is writer-supplied and `runlog.py` never validates it
// (`a.run_id or uuid.uuid4().hex[:12]`), the store is shared by every repo on the machine,
// and this string is read by a human deciding whether a push is safe. An id holding
// "\x1b[2K\r" erases the line and reprints a forged verdict above the real error; %q
// escapes the control bytes instead. The phase needs no quoting — only the three literals
// below reach here.
func (r *Run) setErr(lineNo int, phase string, err error) {
	r.errN++
	if r.errN > maxRunErrs {
		// Bounded, because accumulating without a cap turned a store with 10,000 bad rows
		// for one run into a 1.3 MB error. The count still says how many there were, so the
		// cap cannot hide the scale of the problem — only the repetition.
		if r.errN == maxRunErrs+1 {
			r.Err = errors.Join(r.Err, fmt.Errorf(
				"run %q: more rows did not decode; only the first %d are listed", r.ID, maxRunErrs))
		}
		return
	}
	r.Err = errors.Join(r.Err, fmt.Errorf(
		"run %q: line %d (%s row) has an unreadable field: %w", r.ID, lineNo, phase, err))
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
	// `Applied != nil && *Applied == 0` matches the Python's `last.get("applied") == 0` on
	// the case that matters and NOT exactly. `None == 0` is False, so a MISSING or null
	// `applied` is not zero here either — unlike the `or 0` sites below. Where it diverges
	// is Python's bool/float arithmetic: `False == 0` is True and `0.0 == 0` is True, so the
	// Python derives `converged` for `applied: false` and `applied: 0.0` (and for
	// `asked: false`, and `analysis_changed: 0`) where the typed port returns a decode error.
	// The retired isZero carried that case with the comment `False == 0 is True, True == 0 is
	// False`, so claiming exactness here would delete documented behaviour and then assert
	// agreement over it. All four shapes are enumerated in parity_test.go's `refused`; none
	// is writer-reachable, because every one of these flags is argparse `type=int`.
	// Reading absence as zero made a cycle row with no
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
