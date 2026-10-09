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
	"path/filepath"
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
// is null in 48 of 59 cycle rows (measured 2026-10-08; the figure two paragraphs up is
// pinned at 8c07d63 and reads 46 of 57, which is the same fact at an earlier store), and
// nothing here derives from it.
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
	// Outcome is the orchestrator's self-report of how the run ended, and the push gate
	// blocks on three of its values. A plain string, not a pointer: absent, null and ""
	// are all "not one of the broken outcomes", which is what the Python's
	// `None not in BROKEN_OUTCOMES` says, and no reader wants them apart.
	//
	// The only ASSERTED field any derivation here reads. Convergence exists precisely
	// because this one cannot be trusted — one real run recorded `clean` while its own
	// author reported it had not converged — so it may only ever BLOCK a push, never
	// grant one. Nothing may read it as evidence that a review finished.
	Outcome        string                `json:"outcome"`
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
	// retention is O(file) rather than O(limit).
	//
	// The cost, measured on a 200 MiB store built by repeating the real one (which averages
	// 936 bytes a line): 231 MiB allocated and 218-231 MiB heap to return one run at
	// limit=1. That is 1.1-1.2x. An earlier version of this comment said "3-4x file size
	// resident (~600 MiB)", which was wrong three ways: the figure came from a store that
	// was ONE 200 MiB line, it quoted a heap number as resident, and it blamed this slice
	// when the identical store costs the identical amount at limit=0 — a single oversized
	// record is bufio's fragment concatenation, which the ring buffer below would not touch.
	//
	// Not fixed, and the honest reason is not "every caller passes 0": nothing outside this
	// package's tests calls Load at all, and two tests do pass a limit. It is that 1.1x on a
	// 143 KiB store buys nothing. The claim worth keeping is the narrow one — `limit` bounds
	// what is RETURNED, not what is read. Revisit when a caller passes a limit over a store
	// big enough for the difference to matter.
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
		// `env.RunID == ""` and not `err != nil`: a DUPLICATE run_id whose first occurrence
		// is wrong-typed returns an error AND populates RunID from the last occurrence, so
		// bailing on the error alone skipped a row Go could already attribute — and the
		// Python's last-wins merged its body. Measured:
		//
		//	{"run_id":7,"run_id":"r","phase":"nudge","unresolved_asks":7}
		//	python=halted   go=CONVERGED
		//
		// Four bytes prepended to the row from the commit message, and the guard below never
		// ran. Skipping only when there is nobody to attribute the row to is the rule the
		// comment already claimed ("the row belongs to no run"); now the code matches it.
		if err := json.Unmarshal([]byte(line), &env); err != nil && env.RunID == "" {
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
		// The envelope's own two names fold too: `Run_ID` and `PHASE` both decode here and
		// are invisible to the Python, which drops such a row for want of a `run_id` or
		// files it under no phase. Checked after the run exists so the error has an owner.
		if err := foldedKeys([]byte(line), "run_id", "phase"); err != nil {
			r.setErr(lineNo, "folded-key", err)
			continue
		}
		// A row whose FIELDS this build would not place is an ERROR. Keying that on the
		// phase WORD, which is what the first version of this guard did, closed three
		// spellings of one forgery and left three open.
		//
		// The mechanism is field placement, not vocabulary. load() merges every non-cycle
		// row into the run dict FIELD-WISE and never looks at `phase`, so any run-level
		// field on any non-cycle row reaches the Python's verdict. Measured, on a store
		// whose first cycle applied nothing:
		//
		//	{"phase":"finnish","unresolved_asks":7}              python=halted  go=CONVERGED
		//	{"phase":7,"unresolved_asks":7}                      python=halted  go=CONVERGED
		//	{"unresolved_asks":7}                                python=halted  go=CONVERGED
		//	{"phase":"nudge","unresolved_asks":7}                python=halted  go=CONVERGED
		//	{"phase":"plan","unresolved_asks":7}                 python=halted  go=CONVERGED
		//	{"phase":"finish","unresolved_asks":7,"phase":"nudge"} python=halted go=CONVERGED
		//
		// Six ways to forge a clean push on a run with seven asks outstanding. The first
		// three are misspellings; the last three spell real phases, and the duplicate-key
		// one spells two. A word-based guard cannot see them, which is why this one asks
		// instead: does the row carry a field a derivation reads, that this row's phase
		// does not model? Both sides then deny the push.
		//
		// It is also forward-compatible, which the word-based guard was not: a NEW phase
		// runlog.py adds later carries no run-level field, so an older build ignores it
		// exactly as the Python's merge does, instead of refusing every run containing one.
		var phase string
		if err := json.Unmarshal(env.Phase, &phase); err != nil {
			// Absent or not a string. phase stays "", which matches no owner below, so a
			// row like this errors if and only if it carries a field that matters.
			phase = ""
		}
		if phase != "cycle" {
			// Cycle rows are the exception: load() appends them and merges nothing, so a
			// run-level field on one is ignored by both implementations.
			if err := misplacedRunField(line, phase); err != nil {
				r.setErr(lineNo, "misplaced-field", err)
				continue
			}
		}
		switch phase {
		case "cycle":
			if err := foldedKeys([]byte(line), "applied", "asked", "agents", "analysis_changed"); err != nil {
				r.setErr(lineNo, phase, err)
				continue
			}
			var c Cycle
			if err := json.Unmarshal([]byte(line), &c); err != nil {
				r.setErr(lineNo, phase, err)
				continue
			}
			r.Cycles = append(r.Cycles, c)
		case "plan":
			if err := foldedKeys([]byte(line), "agent_cap", "gates"); err != nil {
				r.setErr(lineNo, phase, err)
				continue
			}
			if err := foldedEntryKeys([]byte(line), "gates", "planned"); err != nil {
				r.setErr(lineNo, phase, err)
				continue
			}
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
			if err := foldedKeys([]byte(line), "outcome", "unresolved_asks", "executed", "agents"); err != nil {
				r.setErr(lineNo, phase, err)
				continue
			}
			if err := foldedEntryKeys([]byte(line), "executed", "status"); err != nil {
				r.setErr(lineNo, phase, err)
				continue
			}
			var fi Finish
			if err := json.Unmarshal([]byte(line), &fi); err != nil {
				r.setErr(lineNo, phase, nameBadGates(line, "executed", err, func(v json.RawMessage) error {
					var res GateResult
					return json.Unmarshal(v, &res)
				}))
				continue
			}
			r.Finish = &fi
		default:
			// `nudge` today, and whatever runlog.py adds next. The run was already created
			// above, which is what load()'s setdefault does — a run known only by a nudge
			// exists and has no cycles — and the guard above has already established that
			// the row carries nothing a derivation would have read off the merged run.
		}
	}
	// The elision line goes here, not in setErr, because only now is the total known.
	for _, r := range runs {
		if r.errN > maxRunErrs {
			r.Err = errors.Join(r.Err, fmt.Errorf(
				"run %q: %d rows did not decode in total; the first %d are listed above",
				r.ID, r.errN, maxRunErrs))
		}
	}
	return runs, nil
}

// foldedKeys reports a key that encoding/json would FOLD onto a field this build decodes,
// but that the Python's exact `rec.get(...)` would never read. Go's decoder matches struct
// tags case-insensitively and has no case-sensitive mode, so this is a divergence the type
// system cannot express — the fifth distinct mechanism for the same forgery on this branch,
// and the one `misplacedRunField` cannot see, because these keys fold onto fields the row's
// phase legitimately owns.
//
// Measured on a store whose earlier cycle applied 5, all against the oracle:
//
//	{"phase":"cycle","Applied":0,...}                          python=halted   go=CONVERGED
//	{"phase":"cycle","applied":3,"APPLIED":0,...}              python=halted   go=CONVERGED
//	{"phase":"finish","unresolved_asks":7,"Unresolved_Asks":0} python=halted   go=CONVERGED
//	{"Run_ID":"r","phase":"cycle","applied":0,...}             python=halted   go=CONVERGED
//	{"run_id":"r","PHASE":"cycle","applied":0,...}             python=halted   go=CONVERGED
//
// and on the gate maps, where it buys a CLEAN SWEEP rather than a converged verdict:
//
//	"executed":{"g":{"status":"failed","Status":"done"}}  python={g:failed}      go={}
//	"gates":{"g":{"planned":"run","PLANNED":"skip"}}      python={g:unreported}  go={}
//
// Two of the eight go the safe way (`Asked`, `Agents`: Go refuses or caps where the Python
// converges), and they are refused here too — the rule is that a key Go reads and the Python
// does not is unreadable, not that only the dangerous half is.
//
// strings.EqualFold is the same simple case folding encoding/json uses, so it also catches
// the long s and the Kelvin sign. Checked per phase against the names that phase's struct
// actually decodes: folding `Agents` on a PLAN row would be over-firing, since nothing reads
// it there and the Python ignores it too.
func foldedKeys(raw []byte, names ...string) error {
	var top map[string]json.RawMessage
	if json.Unmarshal(raw, &top) != nil {
		return nil // not an object; the caller's own decode reports that
	}
	var bad []string
	for k := range top {
		for _, n := range names {
			if k != n && strings.EqualFold(k, n) {
				bad = append(bad, fmt.Sprintf("%s (folds onto %q)", quoteCapped(k), n))
				break
			}
		}
	}
	if len(bad) == 0 {
		return nil
	}
	sort.Strings(bad) // folded keys are reported in a stable order, not Go map order
	return fmt.Errorf(
		"key(s) %s differ from a field this build reads only by case; encoding/json folds them "+
			"onto it and the Python's exact lookup does not read them at all",
		strings.Join(bad, ", "))
}

// foldedEntryKeys is foldedKeys one level down, for the gate maps whose VALUES are objects.
func foldedEntryKeys(raw []byte, field string, names ...string) error {
	var top map[string]json.RawMessage
	if json.Unmarshal(raw, &top) != nil {
		return nil
	}
	var entries map[string]json.RawMessage
	if _, ok := top[field]; !ok {
		return nil
	}
	if json.Unmarshal(top[field], &entries) != nil {
		return nil // the map's own type is wrong; the caller's decode covers it
	}
	gates := make([]string, 0, len(entries))
	for g := range entries {
		gates = append(gates, g)
	}
	sort.Strings(gates)
	for _, g := range gates {
		if err := foldedKeys(entries[g], names...); err != nil {
			return fmt.Errorf("%s gate %s: %w", field, quoteCapped(g), err)
		}
	}
	return nil
}

// quoteCapped is %q plus the bound every operator message in this file carries, because the
// keys and gate names it prints are writer-supplied and unvalidated.
func quoteCapped(s string) string {
	if len(s) > maxGateNameLen {
		s = s[:maxGateNameLen] + "..."
	}
	return strconv.Quote(s)
}

// runLevelFields are the fields a derivation reads off the MERGED run dict, mapped to the
// phase that owns each. load() merges every non-cycle row field-wise, so one of these on a
// row whose phase does not own it still reaches the Python's verdict while the typed decode
// files the row by phase and never sees it.
//
// The map grows with the derivations, and `outcome` is here because this commit is the one
// that first reads it: Decide blocks a push on three of its values, so a misplaced
// `{"phase":"plan","outcome":"test-failure"}` would otherwise reach the Python's blocker and
// be dropped here — the same forgery, through the first field the deferred entry predicted
// it would reopen through.
//
// Still SHORT of what the Python reads off the merged run: `session_id`, `head`, `repo`,
// `finished_at` and `abandoned_missing` (review-stats' is_abandoned reads the last). Omitting
// them is safe only because no derivation in this package reads any of them, so a misplaced
// one cannot change an answer HERE — it can still change the Python's. Each becomes a
// forgery vector the moment something here reads it, and must join this map in that commit.
//
// Cycle-owned counts are absent for the opposite reason, which is permanent: load() does not
// merge cycle rows at all, so `applied` on a finish row is ignored by both implementations.
var runLevelFields = map[string]string{
	"agent_cap":       "plan",
	"gates":           "plan",
	"outcome":         "finish",
	"unresolved_asks": "finish",
	"executed":        "finish",
	"cycles":          synthesized,
}

// synthesized is the owner for a key load() BUILDS rather than reads off a row, so no row
// may carry it. It cannot be a real phase, and it cannot be "" either: a row whose phase is
// absent or wrong-typed decodes to "", and an owner of "" would exempt exactly those rows.
//
// `cycles` is the only one, and it is the sharpest form of this whole class. load() does
// `run.setdefault("cycles", []).append(rec)` for cycle rows and `run.update(rec)` for every
// other, so a non-cycle row carrying a top-level `cycles` key OVERWRITES the entire cycle
// list — and `cycles_of` reads it back out of the merged run at runlog.py:180, feeding both
// convergence() and disclosure(). Measured on a store whose one real cycle converged:
//
//	{"run_id":"r","phase":"nudge","cycles":[]}   python=unknown   go=CONVERGED
//
// The Python says "Review completeness UNKNOWN ... Treat as unreviewed" and discloses; this
// said converged and pushed clean. It worked on all 14 phase spellings, a nudge row being
// the cheapest. No writer emits the key — load() synthesizes it — and no row in the real
// store carries it, so closing it over-fires on nothing.
const synthesized = "\x00synthesized"

// misplacedRunField reports a field this row carries that a derivation reads and this row's
// phase does not own. Decodes the top level only, and only the keys — the values stay raw.
func misplacedRunField(line, phase string) error {
	var top map[string]json.RawMessage
	if json.Unmarshal([]byte(line), &top) != nil {
		// UNREACHABLE, and labelled rather than left looking like a guard. json.Unmarshal
		// runs checkValid over the whole document in both decodes, so a line whose envelope
		// struct decode succeeded is a JSON object (or `null`, which env.RunID == "" already
		// filtered), and map[string]json.RawMessage accepts every object. Verified two ways:
		// replacing this return with a panic decodes all 40 real runs and passes the whole
		// suite, and every wrong-typed-run_id line above reports mapErr == nil. Deleting it
		// also survives, which is why it is labelled — an unreachable branch that scores as
		// covered is worth saying so about, the same way the \r trim above is.
		return nil
	}
	var bad []string
	for name, owner := range runLevelFields {
		if _, ok := top[name]; ok && owner != phase {
			bad = append(bad, name)
		}
	}
	if len(bad) == 0 {
		return nil
	}
	sort.Strings(bad) // field order must not vary with Go map iteration order
	// %q on the phase, and truncated: it is writer-supplied like run_id and the gate names,
	// and an earlier version of this site had no bound at all — a 1 MiB phase produced a
	// 4 MiB message, and 20 such rows produced 80 MiB.
	shown := phase
	if len(shown) > maxGateNameLen {
		shown = shown[:maxGateNameLen] + "..."
	}
	// Each field with ITS OWN owner. Reporting runLevelFields[bad[0]] named one owner for
	// the whole list, so a nudge row carrying agent_cap, executed, gates and unresolved_asks
	// was told "only a plan row may set" — wrong for two of the four, and the singular
	// phrasing read as covering all of them. sort.Strings above made that deterministically
	// the alphabetically-first field's owner, which is arbitrary rather than informative.
	named := make([]string, 0, len(bad))
	for _, f := range bad {
		if owner := runLevelFields[f]; owner == synthesized {
			named = append(named, fmt.Sprintf("%s (which no row may set: this build builds it)", f))
		} else {
			named = append(named, fmt.Sprintf("%s (only a %s row may set it)", f, owner))
		}
	}
	return fmt.Errorf(
		"a %q row carries %s; this build reads these off the run, and the Python merges them "+
			"regardless of phase, so ignoring the row here would answer from one it read",
		shown, strings.Join(named, ", "))
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
		// for one run into a 1.3 MB error. The TOTAL is reported by Load once the loop ends
		// — see the finalise pass — because it is not known here: this is called per row,
		// and an earlier version wrote the line mid-loop and printed maxRunErrs instead,
		// which made the whole error byte-identical for 200, 2,000 and 10,000 bad rows while
		// its comment claimed the count could not be hidden.
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

// Disclosure is the line a PR must carry when the review did not converge: what stopped the
// loop, what was still outstanding when it did, and that more findings were likely. Empty
// when the run converged and owes nothing.
//
// It carries the whole remaining consequence of an unfinished review. Not converging stopped
// BLOCKING the push — a cap that strands commits just hands the decision back to a human
// every time — so a capped or halted run pushes and owes this line instead. That is why it
// is derived from the cycle rows and not written by the orchestrator: a summary the
// orchestrator composes is a summary it can soften, and softening it is free.
func (r *Run) Disclosure() (string, error) {
	conv, err := r.Convergence()
	if err != nil {
		return "", err
	}
	switch conv {
	case Converged:
		return "", nil
	case Unknown:
		// No cycle rows. Not "probably fine": nothing in the record can say whether the
		// loop still had findings when it stopped, and since omitting the rows is the
		// cheapest way to reach this state, it has to read as unreviewed rather than as
		// an absence of bad news.
		return "Review completeness UNKNOWN: this run recorded no cycles, so nothing can say " +
			"whether the loop still had findings when it stopped. Treat as unreviewed.", nil
	}

	var spent int
	for _, c := range r.Cycles {
		if c.Agents != nil {
			spent += *c.Agents // absent reads as zero, matching the Python's `or 0`
		}
	}
	last := r.Cycles[len(r.Cycles)-1] // non-empty: Convergence answers Unknown otherwise

	var head string
	switch conv {
	case Capped:
		if r.Plan == nil || r.Plan.AgentCap == nil {
			// Unreachable: Convergence derives Capped only from a recorded cap. Stated as
			// an error rather than dereferenced on faith, because the failure it guards
			// against is the two functions disagreeing about what `capped` means, and a
			// disclosure reading "CAPPED at 9 of <nil> agents" would ship that silently.
			return "", fmt.Errorf("run %q: derived %s with no recorded agent_cap", r.ID, conv)
		}
		head = fmt.Sprintf("Review CAPPED at %d of %d agents", spent, *r.Plan.AgentCap)
	case Halted:
		head = fmt.Sprintf("Review HALTED after %d cycle(s), %d agents", len(r.Cycles), spent)
	default:
		// The Python indexes a two-key dict here, so a fifth convergence value is a
		// KeyError there and must not be a headless sentence here.
		return "", fmt.Errorf("run %q: no disclosure defined for convergence %q", r.ID, conv)
	}

	// The Python's `last.get("asked") or run.get("unresolved_asks") or 0`: a zero or absent
	// per-cycle count falls through to the run-level total, so asks recorded only at finish
	// still reach the line. `== 0` and not `< 1`, because a negative is truthy in Python and
	// keeps its place — and it renders, which is right: a negative count is corrupt data the
	// reader should see, not a zero the line should hide.
	asked := 0
	if last.Asked != nil {
		asked = *last.Asked
	}
	if asked == 0 && r.Finish != nil && r.Finish.UnresolvedAsks != nil {
		asked = *r.Finish.UnresolvedAsks
	}

	// An absent `applied` renders "?" where the Python renders "None" for an explicit null —
	// a *int is nil for both, which is the one place the two-state pointer is visible. This
	// is the divergence the type comment said would be decided here: cosmetic, enumerated in
	// parity_test.go, and grounded on no row in the real store holding a null `applied`.
	// Both spellings tell the reader the same thing, and restoring three-state decoding for
	// prose would complicate the field every verdict turns on.
	applied := "?"
	if last.Applied != nil {
		applied = strconv.Itoa(*last.Applied)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%s: the last cycle applied %s fix(es)", head, applied)
	if asked != 0 {
		fmt.Fprintf(&b, " and left %d finding(s) awaiting a decision", asked)
	}
	if last.AnalysisChanged != nil && *last.AnalysisChanged {
		b.WriteString(" and the deterministic pass still had unresolved findings")
	}
	b.WriteString(". The loop had not stopped finding things — another cycle would likely find more.")
	return b.String(), nil
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

// StorePath is where the record lives: $REVIEW_LOOP_RUNS, or ~/.claude/review-loop/runs.jsonl.
// Matches runlog.STORE including the expanduser, which the Python applies to the env value
// too — a store configured as `~/alt/runs.jsonl` must name the same file in both. `~user` is
// not expanded; expanduser does, and nothing configures one.
func StorePath() string {
	p := os.Getenv("REVIEW_LOOP_RUNS")
	if p == "" {
		p = "~/.claude/review-loop/runs.jsonl"
	}
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(p[1:], "/"))
		}
	}
	return p
}
