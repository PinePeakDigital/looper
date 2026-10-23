package record

// The write half: every runlog.py subcommand, as `looper record <sub>`. The read half above is
// typed and strict because its answers gate a push; this half mirrors runlog.py's dict
// semantics instead, because its guards (is there a plan, is the run already finished) must
// not refuse a write over a malformed row elsewhere in a shared, append-only store. The one
// derivation it needs — convergence, for finish's warning and the `convergence` subcommand —
// is taken from the typed read half, so there is still exactly one definition of it.

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/pinepeakdigital/looper/internal/evidence"
	"github.com/pinepeakdigital/looper/internal/home"
	"github.com/pinepeakdigital/looper/internal/run"
)

// DefaultAgentCap is runlog.DEFAULT_AGENT_CAP: a ceiling, not a target. review-stats derives
// the numbers to look at before changing it; they are deliberately not restated here.
const DefaultAgentCap = 40

// exitErr is sys.exit(msg): the message on stderr, exit 1. Code 2 is argparse's usage error.
type exitErr struct {
	code int
	msg  string
}

func (e *exitErr) Error() string { return e.msg }

func fail(format string, a ...any) error { return &exitErr{1, fmt.Sprintf(format, a...)} }

// ---- the store, read the way runlog.load() reads it -------------------------------------

// runStore is load()'s merge, keeping first-seen order: Python dicts iterate in insertion
// order, and unfinished() sorts stably on top of that order.
type runStore struct {
	order []string
	runs  map[string]map[string]any
}

func (s runStore) get(id string) map[string]any { return s.runs[id] }

var pyIntRE = regexp.MustCompile(`^[+-]?[0-9]+(_[0-9]+)*$`)

// TailLines is runlog.TAIL_LINES: REVIEW_LOOP_TAIL, at least 1, and 4000 when it does not
// parse, because a fat-fingered override must not break every invocation. Exported for the
// other reader bounded by the tail, `looper stats`, so the two cannot parse it differently.
func TailLines() int {
	v := strings.TrimSpace(os.Getenv("REVIEW_LOOP_TAIL"))
	if v == "" {
		return DefaultTail
	}
	// Python's int() takes one underscore BETWEEN digits ("1_000") and refuses "1__0" or "_5".
	// ponytail: ASCII digits and int range only; int() also takes Arabic-Indic digits and
	// unbounded values, which fall back to 4000 here. Nobody sets a tail that way.
	if !pyIntRE.MatchString(v) {
		return DefaultTail
	}
	n, err := strconv.Atoi(strings.ReplaceAll(v, "_", ""))
	if err != nil {
		return DefaultTail
	}
	return max(1, n)
}

// loadRaw is runlog.load(limit). limit <= 0 reads the whole store. The tail counts LINES, blank
// ones included, the way deque(fh, maxlen=limit) does. Only a missing store reads as empty, as
// runlog.load's `except FileNotFoundError` has it: an unreadable one is an error, never "no runs",
// or `check` reports no open run and `finish` skips its plan-based refusals.
func loadRaw(path string, limit int) (runStore, error) {
	s := runStore{runs: map[string]map[string]any{}}
	lines, _, err := storeLines(path, limit)
	if err != nil {
		return s, fail("runlog: cannot read the run store: %v", err)
	}
	for _, line := range lines {
		if line == "" {
			continue
		}
		var rec map[string]any
		d := json.NewDecoder(strings.NewReader(line))
		d.UseNumber()
		if d.Decode(&rec) != nil || rec == nil || !atEOF(d) {
			continue // a torn line never invalidates the rest of the store
		}
		rid, _ := rec["run_id"].(string)
		if rid == "" {
			continue
		}
		run, ok := s.runs[rid]
		if !ok {
			run = map[string]any{}
			s.runs[rid] = run
			s.order = append(s.order, rid)
		}
		// Cycle rows ACCUMULATE; every other phase merges.
		if rec["phase"] == "cycle" {
			cy, _ := run["cycles"].([]any)
			run["cycles"] = append(cy, rec)
			continue
		}
		for k, v := range rec {
			run[k] = v
		}
	}
	return s, nil
}

// truthy is Python's bool().
func truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case json.Number:
		f, err := x.Float64()
		return err != nil || f != 0
	case map[string]any:
		return len(x) > 0
	case []any:
		return len(x) > 0
	}
	return true
}

// pyStr is str() for the values these messages interpolate.
func pyStr(v any) string {
	switch x := v.(type) {
	case nil:
		return "None"
	case bool:
		if x {
			return "True"
		}
		return "False"
	case string:
		return x
	case json.Number:
		return x.String()
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// pyRepr is repr() of a str (and None), which the refusal messages quote names with.
func pyRepr(v any) string {
	s, ok := v.(string)
	if !ok {
		return pyStr(v)
	}
	q := byte('\'')
	if strings.Contains(s, "'") && !strings.Contains(s, `"`) {
		q = '"'
	}
	var b strings.Builder
	b.WriteByte(q)
	for _, r := range s {
		switch {
		case r == '\\':
			b.WriteString(`\\`)
		case r == rune(q):
			b.WriteByte('\\')
			b.WriteRune(r)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\x%02x`, r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte(q)
	return b.String()
}

func strip(v any) string {
	s, _ := v.(string)
	return pyStrip(s)
}

// pyStrip is str.strip(), whose whitespace includes \x1c-\x1f; strings.TrimSpace's does not,
// so a reason of only those bytes passed every "carries no reason" guard here.
func pyStrip(s string) string { return strings.TrimFunc(s, pySpace) }

func pySpace(r rune) bool { return unicode.IsSpace(r) || (r >= 0x1c && r <= 0x1f) }

// PySpace is pySpace for internal/stats, so the two packages share one definition of Python's
// whitespace and cannot drift.
var PySpace = pySpace

// ---- environment, the way runlog reads it -----------------------------------------------

func now() string { return time.Now().Format("2006-01-02T15:04:05-07:00") }

func newRunID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// git is runlog.git(): stdout, stripped, or "" on any failure.
func git(args ...string) string {
	r := run.Run("", 10*time.Second, "git", args...)
	if r.Error != nil {
		return ""
	}
	return strings.TrimSpace(string(r.Out))
}

// repoID is a repo's identity across worktrees: its common dir's parent (home.RepoID).
func repoID() string { return home.RepoID("") }

// sessionKind is runlog.session_kind(). CLAUDE_CODE_SESSION_ATTENDED is the harness's own
// answer and outranks the tty guess, which classifies every interactive Claude Code run as
// headless because its commands run with stdin detached.
func sessionKind() string {
	if os.Getenv("AO_SESSION_ID") != "" {
		return "ao-worker"
	}
	if os.Getenv("CLAUDE_REVIEW_LOOP_SUBAGENT") != "" {
		return "subagent"
	}
	if v, set := os.LookupEnv("CLAUDE_CODE_SESSION_ATTENDED"); set {
		if v != "" && v != "0" && v != "false" {
			return "interactive"
		}
		return "headless"
	}
	if !isTerminal(os.Stdin.Fd()) && !isTerminal(os.Stderr.Fd()) {
		return "headless"
	}
	return "interactive"
}

// sessionID is `os.environ.get(A) or os.environ.get(B)`: the second operand's raw value,
// None when unset, whenever the first is unset or blank.
func sessionID() any {
	if v := os.Getenv("CLAUDE_CODE_SESSION_ID"); v != "" {
		return v
	}
	if v, set := os.LookupEnv("AO_SESSION_ID"); set {
		return v
	}
	return nil
}

// ---- the precedent ban ------------------------------------------------------------------

// BannedReason is runlog.BANNED_REASON. "It has precedent in the repo" is an invalid
// justification for reduced review, not a weak one: bugs pre-exist, copying a pattern
// propagates them, and the copy is the cheapest moment to catch one.
var BannedReason = []string{
	"precedent", "already exists in the repo", "matches an existing pattern",
	"matches existing pattern", "same pattern as", "pattern copy", "pattern-copy",
	"copied from existing", "consistent with existing code", "follows the existing pattern",
	"consistent with existing pattern",
	"established pattern in this repo",
	"how the rest of the codebase",
	"how the rest of this codebase",
	"it's the convention here",
	"the convention in this repo",
	"identical to what",
	"same as what",
	"already done this way",
	"done this way elsewhere",
	"mirrors the existing",
}

type reason struct {
	where string
	text  any
}

// orderedObj is a JSON object with its key order kept: runlog iterates gates and executed in
// the order the caller wrote them, and that order reaches the refusal and partial messages.
type orderedObj struct {
	keys []string
	m    map[string]any
}

func (o *orderedObj) set(k string, v any) {
	if _, ok := o.m[k]; !ok {
		o.keys = append(o.keys, k)
	}
	o.m[k] = v
}

// reasonsIn is runlog.reasons_in(): every free-text reason a record carries.
func reasonsIn(gates, executed *orderedObj, escalations []any, fields map[string]any) []reason {
	var out []reason
	if gates != nil {
		for _, g := range gates.keys {
			if v, ok := gates.m[g].(map[string]any); ok && v["planned"] != "run" {
				out = append(out, reason{fmt.Sprintf("gate %s as %s", pyRepr(g), pyStr(v["planned"])), v["reason"]})
			}
		}
	}
	if executed != nil {
		for _, g := range executed.keys {
			if v, ok := executed.m[g].(map[string]any); ok && v["status"] != "done" {
				out = append(out, reason{fmt.Sprintf("gate %s as %s", pyRepr(g), pyStr(v["status"])), v["reason"]})
			}
		}
	}
	for _, e := range escalations {
		if v, ok := e.(map[string]any); ok {
			out = append(out, reason{fmt.Sprintf("escalation on %s", pyRepr(v["gate"])), v["reason"]})
		}
	}
	for _, kl := range [][2]string{
		{"abandoned_missing", "this abandonment"}, {"skip_reason", "this skip"},
		{"sizing_excluded", "this sizing exclusion"}, {"tier_reason", "this tier"},
		{"width_reason", "this fan-out width"},
	} {
		if truthy(fields[kl[0]]) {
			out = append(out, reason{kl[1], fields[kl[0]]})
		}
	}
	return out
}

// pyFields is str.split() with no argument. Python counts \x1c-\x1f as whitespace and Go's
// unicode.IsSpace does not; a phrase split by one would otherwise pass here and be refused there.
func pyFields(s string) []string {
	return strings.FieldsFunc(s, pySpace)
}

// rejectBanned refuses a reason that cites precedent. Whitespace-normalised, not just
// lowercased: a phrase wrapped across a line is the ordinary case, not an evasion.
//
// A reason that is not a string is refused outright. Stringified, a list's newlines come back
// as the two characters `\n`, which split nothing, so `["matches an\nexisting pattern"]` as an
// escalation reason was recorded — measured. runlog.py dies on it with AttributeError at
// .lower(), which also records nothing; this says why instead of printing a traceback.
func rejectBanned(rs []reason) error {
	for _, r := range rs {
		if _, ok := r.text.(string); !ok && truthy(r.text) {
			return fail("runlog: refusing to record %s — its reason is not a string", r.where)
		}
		low := strings.Join(pyFields(strings.ToLower(pyStrOrEmpty(r.text))), " ")
		for _, phrase := range BannedReason {
			if strings.Contains(low, phrase) {
				return fail("runlog: refusing to record %s — its reason cites precedent (%s).\n"+
					"Existing code being similar is not evidence it is correct; a copied "+
					"pattern carries its bugs, and the copy is the cheapest place to catch "+
					"them. State a measurable reason (size, no logic touched, no runtime "+
					"change) or run the gate.", r.where, pyRepr(phrase))
			}
		}
	}
	return nil
}

// pyStrOrEmpty is `(text or "")` for a value that is about to be lowercased.
func pyStrOrEmpty(v any) string {
	if !truthy(v) {
		return ""
	}
	return pyStr(v)
}

// ---- appending --------------------------------------------------------------------------

// appendRec writes one record as one line. A single write() to a file opened O_APPEND does not
// interleave with other writers on the local filesystems this store lives on, which is what
// lets concurrent worktree sessions share it without a lock (not over NFS, and not the PIPE_BUF
// guarantee, which is about pipes). os.File.Write retries a short write with a second write(2),
// which could interleave; a short write to a regular file takes ENOSPC or a signal, and
// loadRaw tolerates a torn line rather than assuming one cannot happen. Keys sorted and compact,
// as runlog writes them. Valid UTF-8 is written as UTF-8 where runlog writes \u escapes, the same
// value to every reader; invalid UTF-8 becomes U+FFFD where runlog keeps a lone surrogate.
func appendRec(rec map[string]any, rs []reason) error {
	return appendChecked(rec, rs, nil)
}

// appendChecked is appendRec with a last look at the exact bytes about to be written, after
// the precedent ban and before anything touches the store. check may be nil.
func appendChecked(rec map[string]any, rs []reason, check func(line string) error) error {
	if err := rejectBanned(rs); err != nil {
		return err
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(rec); err != nil {
		return err
	}
	if check != nil {
		if err := check(strings.TrimSuffix(buf.String(), "\n")); err != nil {
			return err
		}
	}
	path := StorePath()
	if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	_, werr := f.Write(buf.Bytes())
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	return werr
}

func appendPlain(rec map[string]any) error {
	return appendRec(rec, reasonsIn(nil, nil, nil, rec))
}

// ---- JSON arguments ---------------------------------------------------------------------

func decodeJSON(raw, what string) (any, error) {
	d := json.NewDecoder(strings.NewReader(raw))
	d.UseNumber()
	var v any
	err := d.Decode(&v)
	if err == nil && !atEOF(d) {
		err = errors.New("extra data") // json.loads refuses "{}]"; d.More() is false before a `]`
	}
	if err != nil {
		return nil, fail("runlog: --%s is not valid JSON: %v", what, err)
	}
	return v, nil
}

// atEOF reports whether nothing but whitespace follows the decoded value.
func atEOF(d *json.Decoder) bool {
	_, err := d.Token()
	return err == io.EOF
}

// objectArg is `parse_json_arg(raw) or {}`, with key order kept. A falsy value is {}; a
// truthy non-object is refused where runlog would die on .items().
func objectArg(raw *string, what string) (*orderedObj, error) {
	o := &orderedObj{m: map[string]any{}}
	if raw == nil {
		return o, nil
	}
	v, err := decodeJSON(*raw, what)
	if err != nil || !truthy(v) {
		return o, err
	}
	if _, ok := v.(map[string]any); !ok {
		return nil, fail("runlog: --%s must be a JSON object", what)
	}
	d := json.NewDecoder(strings.NewReader(*raw))
	d.UseNumber()
	if _, err := d.Token(); err != nil {
		return nil, err
	}
	for d.More() {
		t, err := d.Token()
		if err != nil {
			return nil, err
		}
		var val any
		if err := d.Decode(&val); err != nil {
			return nil, err
		}
		o.set(t.(string), val)
	}
	return o, nil
}

// listArg is `parse_json_arg(raw) or []`. A truthy non-list is kept and recorded as given,
// as runlog does; asList then iterates it as runlog would, which for an object means over keys
// that are never dicts, so nothing.
func listArg(raw *string, what string) (any, error) {
	if raw == nil {
		return []any{}, nil
	}
	v, err := decodeJSON(*raw, what)
	if err != nil || !truthy(v) {
		return []any{}, err
	}
	switch v.(type) {
	case json.Number, bool:
		// runlog.py iterates it and dies with TypeError, recording nothing; a number is no
		// shape any reader decodes, so say so instead of writing an unreadable finish.
		return nil, fail("runlog: --%s must be a JSON list", what)
	}
	return v, nil
}

func asList(v any) []any {
	l, _ := v.([]any)
	return l
}

func anyArg(raw *string, what string) (any, error) {
	if raw == nil {
		return map[string]any{}, nil
	}
	v, err := decodeJSON(*raw, what)
	if err != nil || !truthy(v) {
		return map[string]any{}, err
	}
	return v, nil
}

// ---- the plan/finish invariants ---------------------------------------------------------

// Every state the record can hold, in one place.
var (
	Outcomes = []string{"clean", "cycle-limit", "asks-open", "test-failure", "blocked", "abandoned", "skipped", "carried"}
	// AsksOpen is the one outcome runlog.py never had (issue #41): the run ended with
	// findings still waiting on the user. It is not broken, so the push gate reads the
	// derived `halted` and pushes with a disclosure. Before it, `blocked` was the only fit
	// for that ending, and push-check reads `blocked` as a broken tree.
	AsksOpen         = "asks-open"
	Tiers            = []string{"skipped", "carried", "fast", "full", "partial"}
	SubcommandStates = []string{"skipped", "carried"}
	// A waiver passes its gate, so it must not force partial; review-stats still shows it.
	GateAccounted = append(append([]string{}, GateOK...), "waived")
	tierRank      = map[string]int{"skipped": 0, "carried": 0, "fast": 1, "full": 2, "partial": 2}
)

func in(s string, set []string) bool {
	for _, x := range set {
		if x == s {
			return true
		}
	}
	return false
}

// plannedGates is the set of gates a plan said to run.
func plannedGates(plan map[string]any) map[string]bool {
	out := map[string]bool{}
	gates, _ := plan["gates"].(map[string]any)
	for g, v := range gates {
		if m, ok := v.(map[string]any); ok && m["planned"] == "run" {
			out[g] = true
		}
	}
	return out
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// unaccounted is the invariant the whole record exists for: gates the plan said to run that
// the finish neither ran nor escalated.
func unaccounted(planned map[string]bool, executed *orderedObj, escalations []any) []string {
	acc := map[string]bool{}
	for _, k := range executed.keys {
		acc[k] = true
	}
	for _, e := range escalations {
		if m, ok := e.(map[string]any); ok {
			if g, ok := m["gate"].(string); ok {
				acc[g] = true
			}
		}
	}
	var out []string
	for _, g := range sortedKeys(planned) {
		if !acc[g] {
			out = append(out, g)
		}
	}
	return out
}

// escalationShape is what --escalations must be, quoted in every refusal of it.
const escalationShape = `[{"gate":"<gate>","reason":"<why it ran above the plan>"}]`

// checkEscalations refuses --escalations that is not a list of objects naming a gate and a
// reason, both strings. runlog.py recorded any truthy JSON here, and the typed reader decodes
// the field as []Escalation, so a non-empty object or a string became a finish row no reader
// could decode — which makes the run unknown to the push gate and the report for good. An
// element with no gate or no reason decodes, but renders as an escalation of nothing, or one
// with no account of why, in the section that exists so an escalation cannot be silent.
func checkEscalations(v any) error {
	l, ok := v.([]any)
	if !ok {
		return fail("runlog: --escalations must be a JSON list of objects, %s — got %s",
			escalationShape, jsonValueKind(v))
	}
	for i, e := range l {
		m, ok := e.(map[string]any)
		if !ok {
			return fail("runlog: --escalations must be a JSON list of objects, %s — element %d is %s",
				escalationShape, i, jsonValueKind(e))
		}
		g, ok := m["gate"].(string)
		if !ok || pyStrip(g) == "" {
			return fail("runlog: escalation %d names no gate — `gate` must be the gate's name, as a "+
				"string: %s", i, escalationShape)
		}
		r, ok := m["reason"].(string)
		if !ok && m["reason"] != nil {
			// The wording rejectBanned uses for the same defect in any other reason field.
			return fail("runlog: refusing to record escalation on %s — its reason is not a string", pyRepr(g))
		}
		if pyStrip(r) == "" {
			return fail("runlog: escalation on %s gives no reason — say why it ran above the plan: %s",
				pyRepr(g), escalationShape)
		}
	}
	return nil
}

// jsonValueKind names a decoded JSON value's type for a refusal.
func jsonValueKind(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case map[string]any:
		return "an object"
	case []any:
		return "a list"
	case string:
		return "a string"
	case bool:
		return "a bool"
	}
	return "a number"
}

// readableFinish is finish's last check: the row, as it is about to be written, decoded by
// the same code every typed reader runs. Whatever the checks before it missed, a row that
// fails here would leave the run unknown to the push gate and the report — and since a
// finish is final, unrepairable. Refusing costs one re-run of the command.
func readableFinish(id string) func(line string) error {
	return func(line string) error {
		st, err := decodeLines([]string{line}, 0)
		if err != nil {
			return err
		}
		why := errors.New("it is filed under no run")
		for _, r := range st.Runs {
			if r.ID == id {
				if why = r.Err; why == nil && r.Finish == nil {
					why = errors.New("it decoded to no finish")
				}
			}
		}
		if why == nil {
			return nil
		}
		return fail("runlog: refusing to finish %s — the row as written would not decode for "+
			"the readers that gate the push and render the report, so the run would read as "+
			"unknown for good. Nothing was recorded. The decode error:\n  %v", pyRepr(id), why)
	}
}

// deriveTier makes `partial` a fact about the run rather than a label the caller picks, and
// refuses a claim below the plan's floor. claimed is nil when no --tier was given.
func deriveTier(claimed *string, executed *orderedObj, agents []any, planned map[string]bool, floor any, errOut io.Writer) (any, error) {
	var broken []string
	for _, g := range executed.keys {
		if v, ok := executed.m[g].(map[string]any); ok {
			st, _ := v["status"].(string)
			if !(in(st, GateAccounted) && v["status"] != nil) && planned[g] {
				broken = append(broken, g)
			}
		}
	}
	for _, x := range agents {
		if v, ok := x.(map[string]any); ok {
			if v["status"] != "ok" && v["status"] != "done" {
				id := any("?")
				if got, ok := v["id"]; ok {
					id = got
				}
				broken = append(broken, pyStr(id))
			}
		}
	}
	claimedV := any(nil)
	cl := ""
	if claimed != nil {
		claimedV, cl = *claimed, *claimed
	}
	if len(broken) > 0 && cl != "partial" {
		fmt.Fprintf(errOut, "runlog: recording tier `partial`, not %s — did not complete: %s\n",
			pyRepr(claimedV), strings.Join(broken, ", "))
		return "partial", nil
	}
	if fl, ok := floor.(string); ok && truthy(floor) && tierRank[cl] < tierRank[fl] {
		return nil, fail("runlog: refusing to record tier %s — the plan computed a floor of "+
			"%s. You may escalate above the floor, never below it. Record "+
			"%s (or `partial` if something did not complete).", pyRepr(claimedV), pyRepr(fl), pyRepr(fl))
	}
	return claimedV, nil
}

// storedConvergence is the typed derivation, read fresh, so finish's warning and the
// `convergence` subcommand say what the push gate and the report will say.
//
// Where runlog.py derives a verdict from a field the typed reader cannot decode (a numeric
// reason, a folded key), this answers unknown and returns why: the push gate reads the typed
// record, so a verdict it cannot reproduce is not one to report. The error is the diagnosis.
func storedConvergence(id string) (string, error) {
	runs, err := Load(StorePath(), 0)
	if err != nil {
		return Unknown, err
	}
	r, ok := runs[id]
	if !ok {
		return Unknown, nil
	}
	return r.Convergence()
}

// ---- the subcommands --------------------------------------------------------------------

type sub struct {
	fs       *flag.FlagSet
	strs     map[string]*string
	ints     map[string]*int
	bools    map[string]*bool
	required []string
}

func newSub(name string, errOut io.Writer) *sub {
	fs := flag.NewFlagSet("looper record "+name, flag.ContinueOnError)
	fs.SetOutput(errOut)
	return &sub{fs: fs, strs: map[string]*string{}, ints: map[string]*int{}, bools: map[string]*bool{}}
}

// str declares an optional string whose absence is distinguishable from "" (None vs "").
func (s *sub) str(name, usage string) {
	var p *string
	s.strs[name] = nil
	s.fs.Func(name, usage, func(v string) error { p = &v; s.strs[name] = p; return nil })
}

// num declares an int; def nil means absent reads as None.
func (s *sub) num(name string, def *int, usage string) {
	s.ints[name] = def
	s.fs.Func(name, usage, func(v string) error {
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			return fmt.Errorf("invalid int value: %q", v)
		}
		s.ints[name] = &n
		return nil
	})
}

func (s *sub) flagBool(name, usage string) { s.bools[name] = s.fs.Bool(name, false, usage) }

func (s *sub) parse(args []string, choices map[string][]string) error {
	if err := s.fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return &exitErr{0, ""} // argparse's -h exits 0 after printing help
		}
		return &exitErr{2, ""}
	}
	if s.fs.NArg() > 0 {
		return &exitErr{2, fmt.Sprintf("%s: unrecognized arguments: %s", s.fs.Name(), strings.Join(s.fs.Args(), " "))}
	}
	for _, r := range s.required {
		_, isStr := s.strs[r]
		if (isStr && s.strs[r] == nil) || (!isStr && s.ints[r] == nil) {
			return &exitErr{2, fmt.Sprintf("%s: the following arguments are required: --%s", s.fs.Name(), r)}
		}
	}
	for name, ok := range choices {
		if v := s.strs[name]; v != nil && !in(*v, ok) {
			return &exitErr{2, fmt.Sprintf("%s: argument --%s: invalid choice: %s (choose from %s)",
				s.fs.Name(), name, pyRepr(*v), strings.Join(ok, ", "))}
		}
	}
	return nil
}

func intVal(p *int) any {
	if p == nil {
		return nil
	}
	return *p
}

func strVal(p *string) any {
	if p == nil {
		return nil
	}
	return *p
}

func orStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func ip(n int) *int { return &n }

var cmdNames = []string{"plan", "finish", "cycle", "convergence", "carried", "skipped", "nudge", "abandon", "check", "show", "pending"}

// Command runs `looper record <sub> [flags]` and returns the exit status runlog.py would.
func Command(args []string, out, errOut io.Writer) int {
	if len(args) == 0 || !in(args[0], cmdNames) {
		fmt.Fprintf(errOut, "usage: looper record {%s} ...\n", strings.Join(cmdNames, ","))
		return 2
	}
	code, err := dispatch(args[0], args[1:], out, errOut)
	var ee *exitErr
	if errors.As(err, &ee) {
		if ee.msg != "" {
			fmt.Fprintln(errOut, ee.msg)
		}
		return ee.code
	}
	if err != nil {
		fmt.Fprintf(errOut, "runlog: %v\n", err)
		return 1
	}
	return code
}

func dispatch(name string, args []string, out, errOut io.Writer) (int, error) {
	s := newSub(name, errOut)
	switch name {
	case "plan":
		for _, f := range []string{"run-id", "head", "base", "tier", "model", "session-kind", "inputs", "gates", "tier-reason", "sizing-excluded"} {
			s.str(f, "")
		}
		s.num("changed-lines", nil, "raw added+deleted lines")
		s.num("agent-cap", ip(DefaultAgentCap), "cumulative subagent budget for the run")
		s.num("semantic-lines", nil, "raw minus whitespace-only, lockfiles and declared-generated files")
		s.required = []string{"tier", "model"}
		if err := s.parse(args, map[string][]string{
			"tier": {"fast", "full", "skipped"}, "session-kind": {"interactive", "ao-worker", "headless", "subagent"},
		}); err != nil {
			return 0, err
		}
		return 0, cmdPlan(s, out)
	case "finish":
		for _, f := range []string{"run-id", "tier", "executed", "escalations", "agents", "findings"} {
			s.str(f, "")
		}
		s.num("asks", ip(0), "unresolved ask-bucket items")
		s.flagBool("allow-unaccounted", "record planned gates that went unrun, marking the run partial")
		s.required = []string{"run-id", "outcome"}
		var outcomes, tiers []string
		for _, o := range Outcomes {
			if !in(o, SubcommandStates) {
				outcomes = append(outcomes, o)
			}
		}
		// Listed in -h so review-loop's preflight can probe for asks-open by name.
		s.str("outcome", "how the run ended: "+strings.Join(outcomes, ", "))
		for _, t := range Tiers {
			if t != "carried" {
				tiers = append(tiers, t)
			}
		}
		if err := s.parse(args, map[string][]string{"outcome": outcomes, "tier": tiers}); err != nil {
			return 0, err
		}
		return 0, cmdFinish(s, out, errOut)
	case "cycle":
		s.str("run-id", "")
		s.str("width-reason", "the measurement that settled the fan-out width")
		s.num("n", nil, "cycle number, 1-based")
		s.num("applied", nil, "fixes applied this cycle")
		s.num("asked", ip(0), "ask-bucket items put to the user")
		s.num("defect-findings", ip(0), "")
		s.num("comment-findings", ip(0), "")
		s.num("agents", nil, "agents spawned this cycle")
		s.num("tokens", nil, "observed subagent tokens, recorded not enforced")
		s.flagBool("analysis-changed", "the Step 4a deterministic pass changed files or left unresolved findings")
		s.required = []string{"run-id", "n", "applied", "agents"}
		if err := s.parse(args, nil); err != nil {
			return 0, err
		}
		return 0, cmdCycle(s, errOut)
	case "convergence":
		s.str("run-id", "")
		s.required = []string{"run-id"}
		if err := s.parse(args, nil); err != nil {
			return 0, err
		}
		return cmdConvergence(*s.strs["run-id"], out, errOut), nil
	case "carried":
		s.str("from", "the rewritten-away sha")
		s.str("to", "the new sha the record now covers")
		s.str("how", "")
		s.str("by", "what established the two are the same content")
		s.required = []string{"from", "to", "how"}
		if err := s.parse(args, map[string][]string{"how": {"reviewed", "skipped"}}); err != nil {
			return 0, err
		}
		return 0, cmdCarried(s, out)
	case "skipped":
		s.str("reason", "")
		s.str("model", "")
		s.required = []string{"reason"}
		if err := s.parse(args, nil); err != nil {
			return 0, err
		}
		return 0, cmdSkipped(s, out)
	case "nudge":
		s.str("run-id", "")
		s.required = []string{"run-id"}
		if err := s.parse(args, nil); err != nil {
			return 0, err
		}
		return 0, appendPlain(map[string]any{"run_id": *s.strs["run-id"], "phase": "nudge", "nudged_at": now()})
	case "abandon":
		s.str("run-id", "")
		s.str("missing", "which planned gates you are NOT running, and why")
		s.required = []string{"run-id", "missing"}
		if err := s.parse(args, nil); err != nil {
			return 0, err
		}
		return 0, cmdAbandon(s, out)
	case "check":
		s.str("head", "")
		s.str("run-id", "")
		s.str("session", "only runs this session started (default: $CLAUDE_CODE_SESSION_ID)")
		s.flagBool("force", "report an unfinished run even if already nudged")
		if err := s.parse(args, nil); err != nil {
			return 0, err
		}
		return cmdCheck(s, out)
	case "show":
		s.str("run-id", "")
		s.required = []string{"run-id"}
		if err := s.parse(args, nil); err != nil {
			return 0, err
		}
		return 0, cmdShow(*s.strs["run-id"], out)
	case "pending":
		s.flagBool("all", "every pending report of this repo's runs, each with its run's recorded branch, whatever is checked out")
		if err := s.parse(args, nil); err != nil {
			return 0, err
		}
		if *s.bools["all"] {
			return 0, cmdPendingAll(out, errOut)
		}
		return 0, cmdPending(out, errOut)
	}
	return 2, nil
}

func cmdPlan(s *sub, out io.Writer) error {
	// A non-positive cap makes `capped` unreachable or universal; neither is expressible.
	if c := s.ints["agent-cap"]; c != nil && *c <= 0 {
		return fail("runlog: --agent-cap must be positive (got %d) — a cap of zero "+
			"or less cannot bind, it only changes which label a stall gets", *c)
	}
	gates, err := objectArg(s.strs["gates"], "gates")
	if err != nil {
		return err
	}
	var bad, unexplained []string
	for _, g := range gates.keys {
		v, ok := gates.m[g].(map[string]any)
		if !ok || !(v["planned"] == "run" || v["planned"] == "skip") {
			bad = append(bad, g)
		}
	}
	sort.Strings(bad)
	if len(bad) > 0 {
		return fail("runlog: refusing to plan — these gates are not "+
			"{\"planned\": \"run\"|\"skip\", \"reason\": ...}:\n  %s", strings.Join(bad, ", "))
	}
	for _, g := range gates.keys {
		if strip(gates.m[g].(map[string]any)["reason"]) == "" {
			unexplained = append(unexplained, g)
		}
	}
	sort.Strings(unexplained)
	if len(unexplained) > 0 {
		return fail("runlog: refusing to plan — these gates carry no reason:\n  %s\nA gate's "+
			"reason is what a later run is measured against; an unexplained plan cannot be "+
			"iterated on.", strings.Join(unexplained, ", "))
	}
	inputs, err := anyArg(s.strs["inputs"], "inputs")
	if err != nil {
		return err
	}
	rid := orStr(s.strs["run-id"])
	if rid == "" {
		rid = newRunID()
	}
	head := orStr(s.strs["head"])
	if head == "" {
		head = git("rev-parse", "HEAD")
	}
	kind := orStr(s.strs["session-kind"])
	if kind == "" {
		kind = sessionKind()
	}
	rec := map[string]any{
		"run_id": rid, "phase": "plan", "planned_at": now(),
		"repo": repoID(), "branch": git("rev-parse", "--abbrev-ref", "HEAD"),
		"head": head, "base": strVal(s.strs["base"]),
		"orchestrator_model": *s.strs["model"], "session_kind": kind,
		// Linked worktrees share a git-common-dir, so repo alone would let one session's open
		// run block another's Stop.
		"session_id": sessionID(), "tier_floor": *s.strs["tier"],
		"inputs": inputs, "gates": gates.m,
		"changed_lines": intVal(s.ints["changed-lines"]), "agent_cap": intVal(s.ints["agent-cap"]),
		"semantic_lines":  intVal(s.ints["semantic-lines"]),
		"sizing_excluded": strVal(s.strs["sizing-excluded"]), "tier_reason": strVal(s.strs["tier-reason"]),
	}
	if err := appendRec(rec, reasonsIn(gates, nil, nil, rec)); err != nil {
		return err
	}
	fmt.Fprintln(out, rid)
	return nil
}

func cmdCycle(s *sub, errOut io.Writer) error {
	id := *s.strs["run-id"]
	store, err := loadRaw(StorePath(), 0)
	if err != nil {
		return err
	}
	prior := store.get(id)
	if len(prior) == 0 {
		return fail("runlog: no plan for run %s — record the plan first", pyRepr(id))
	}
	// A cycle row after `finish` silently rewrites a convergence already disclosed on a PR and
	// already used to clear a push: `--clean-exit` by another name.
	if truthy(prior["finished_at"]) || truthy(prior["outcome"]) {
		return fail("runlog: run %s is already finished — a cycle row appended now would "+
			"change a convergence that has already been reported. Start a new run.", pyRepr(id))
	}
	for _, n := range []string{"n", "applied", "asked", "agents", "tokens"} {
		if v := s.ints[n]; v != nil && *v < 0 {
			return fail("runlog: --%s cannot be negative (got %d) — a negative count pulls "+
				"cumulative spend back under the cap", n, *v)
		}
	}
	rec := map[string]any{
		"run_id": id, "phase": "cycle", "n": *s.ints["n"], "closed_at": now(),
		"applied": *s.ints["applied"], "asked": intVal(s.ints["asked"]),
		"defect_findings": intVal(s.ints["defect-findings"]), "comment_findings": intVal(s.ints["comment-findings"]),
		"analysis_changed": *s.bools["analysis-changed"], "agents": *s.ints["agents"],
		"width_reason": strVal(s.strs["width-reason"]), "subagent_tokens": intVal(s.ints["tokens"]),
	}
	if err := appendPlain(rec); err != nil {
		return err
	}
	fmt.Fprintf(errOut, "runlog: cycle %d of %s (%d applied, %d agents)\n",
		*s.ints["n"], id, *s.ints["applied"], *s.ints["agents"])
	return nil
}

func cmdConvergence(id string, out, errOut io.Writer) int {
	store, err := loadRaw(StorePath(), 0)
	if err != nil {
		fmt.Fprintln(errOut, err)
		return 1
	}
	if len(store.get(id)) == 0 {
		fmt.Fprintln(errOut, "unknown")
		return 2
	}
	c, err := storedConvergence(id)
	if err != nil {
		fmt.Fprintf(errOut, "runlog: this run's record cannot be read, so its convergence is unknown: %v\n", err)
	}
	fmt.Fprintln(out, c)
	if c == Converged {
		return 0
	}
	return 1
}

func cmdFinish(s *sub, out, errOut io.Writer) error {
	id := *s.strs["run-id"]
	executed, err := objectArg(s.strs["executed"], "executed")
	if err != nil {
		return err
	}
	// Parsed now, type-checked later: runlog.py only trips over a number here when
	// unaccounted() iterates it, after the unexplained-gate refusal, so that refusal comes first.
	escalations, err := anyArg(s.strs["escalations"], "escalations")
	if err != nil {
		return err
	}
	if m, ok := escalations.(map[string]any); ok && len(m) == 0 {
		escalations = []any{}
	}
	store, err := loadRaw(StorePath(), 0)
	if err != nil {
		return err
	}
	plan := store.get(id)
	// A finish is final, as cycle and abandon already hold: the outcome and convergence it
	// recorded may be on a PR and may have cleared a push, and a second row would rewrite them
	// silently. It does not repair a bad first row either — the typed reader keeps a run's
	// decode error whatever follows it — so the remedy for a wrong finish is a new run.
	if truthy(plan["finished_at"]) || truthy(plan["outcome"]) {
		return fail("runlog: run %s is already finished as %s — a finish is final, and a second "+
			"finish row would rewrite an outcome that may already be on a PR and have cleared a "+
			"push. Start a new run.", pyRepr(id), pyRepr(plan["outcome"]))
	}
	// Refused as cycle refuses its counts: a negative --asks is not a count, and the clean
	// check below reads only a positive one, so a -1 would pass it and be written as the run's
	// unresolved asks.
	if asks := s.ints["asks"]; asks != nil && *asks < 0 {
		return fail("runlog: --asks cannot be negative (got %d) — a negative count of unresolved "+
			"asks passes the `clean` check below and is written to the record", *asks)
	}
	// `clean` with asks still with the user is a self-report the record contradicts: the
	// derivation reads those asks as halted, so the row would say two things at once. The
	// warning below used to be all this got, and the row was written regardless.
	if asks := s.ints["asks"]; *s.strs["outcome"] == "clean" && asks != nil && *asks > 0 {
		return fail("runlog: refusing to finish as 'clean' with %d unresolved asks — the run "+
			"has findings still with the user, so it did not end clean. Record the outcome it "+
			"did end with, or resolve the asks and record a cycle first.", *asks)
	}
	// The mirror of the clean check: asks-open claims findings are still with the user, so
	// a count of zero contradicts it.
	if asks := s.ints["asks"]; *s.strs["outcome"] == AsksOpen && (asks == nil || *asks < 1) {
		return fail("runlog: refusing to finish as 'asks-open' with no unresolved asks — pass " +
			"--asks with the number of questions still owed to the user, or record the outcome " +
			"the run did end with.")
	}
	planned := plannedGates(plan)
	var unexplained []string
	for _, g := range executed.keys {
		v, ok := executed.m[g].(map[string]any)
		if !ok || (v["status"] != "done" && strip(v["reason"]) == "") {
			unexplained = append(unexplained, g)
		}
	}
	sort.Strings(unexplained)
	if len(unexplained) > 0 {
		return fail("runlog: refusing to finish — these gates carry no usable account (a status "+
			"other than \"done\" with no reason, or not a {\"status\":..,\"reason\":..} object "+
			"at all):\n  %s\nA status says what happened; the reason is the part anyone reading "+
			"this later actually needs. Give each one a measurable reason.", strings.Join(unexplained, ", "))
	}
	if err := checkEscalations(escalations); err != nil {
		return err
	}
	missing := unaccounted(planned, executed, asList(escalations))
	allow := *s.bools["allow-unaccounted"]
	if len(missing) > 0 && !allow {
		return fail("runlog: refusing to finish — the plan said to run these gates and the "+
			"finish accounts for none of them:\n  %s\nGive each one an `executed` entry (status "+
			"done/skipped/failed, with a reason for anything but done) or record an escalation. "+
			"If they genuinely went unrun and you are recording that fact, pass "+
			"--allow-unaccounted and the run is marked partial.", strings.Join(missing, ", "))
	}
	agents, err := listArg(s.strs["agents"], "agents")
	if err != nil {
		return err
	}
	for _, g := range missing {
		executed.set(g, map[string]any{"status": "failed", "reason": "unaccounted at finish"})
	}
	tier, err := deriveTier(s.strs["tier"], executed, asList(agents), planned, plan["tier_floor"], errOut)
	if err != nil {
		return err
	}
	findings, err := anyArg(s.strs["findings"], "findings")
	if err != nil {
		return err
	}
	outcome := *s.strs["outcome"]
	rec := map[string]any{
		"run_id": id, "phase": "finish", "finished_at": now(), "outcome": outcome,
		"tier_executed": tier, "executed": executed.m, "escalations": escalations,
		"agents": agents, "findings": findings, "unresolved_asks": intVal(s.ints["asks"]),
		"head_at_finish": git("rev-parse", "HEAD"),
	}
	if err := appendChecked(rec, reasonsIn(nil, executed, asList(escalations), rec), readableFinish(id)); err != nil {
		return err
	}
	// Said at finish, where it can still be fixed, rather than at the push gate.
	conv, cerr := storedConvergence(id)
	hasCycles := false
	if cy, ok := store.get(id)["cycles"].([]any); ok && len(cy) > 0 {
		hasCycles = true
	}
	switch {
	case cerr != nil:
		fmt.Fprintf(errOut, "runlog: WARNING — this run's record cannot be read, so its "+
			"convergence is unknown and the push gate will read it that way: %v\n", cerr)
	case !hasCycles:
		fmt.Fprintln(errOut, "runlog: WARNING — no cycle rows for this run, so convergence is "+
			"unknown, which every consumer reads as 'did not converge'. Record each cycle with "+
			"`looper record cycle`.")
	case outcome == "clean" && conv != Converged:
		fmt.Fprintf(errOut, "runlog: WARNING — outcome 'clean' but the cycle rows derive %s. "+
			"The derivation is what the PR and the push gate read; `clean` here is a self-report "+
			"that contradicts it. Record the missing cycle or fix the outcome.\n", pyRepr(conv))
	}
	fmt.Fprintf(out, "runlog: finished %s (%s; convergence: %s)\n", id, outcome, conv)
	return nil
}

func cmdCarried(s *sub, out io.Writer) error {
	frm, to, how := *s.strs["from"], *s.strs["to"], *s.strs["how"]
	by := any("patch-id")
	if p := s.strs["by"]; p != nil {
		by = *p
	}
	if err := appendPlain(map[string]any{
		"run_id": newRunID(), "phase": "finish", "finished_at": now(),
		"repo": repoID(), "branch": git("rev-parse", "--abbrev-ref", "HEAD"),
		"head": to, "head_at_finish": to, "session_kind": sessionKind(),
		"outcome": "carried", "tier_executed": "carried",
		"carried_from": frm, "carried_how": how, "carried_by": by, "executed": map[string]any{},
	}); err != nil {
		return err
	}
	fmt.Fprintf(out, "runlog: recorded %s record carried %s -> %s\n", how, first(frm, 12), first(to, 12))
	return nil
}

func first(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n])
	}
	return s
}

func cmdSkipped(s *sub, out io.Writer) error {
	reasonText := *s.strs["reason"]
	// Two writes: a refusal on the second would leave the first behind as a phantom open run,
	// so the reason is vetted once, up front.
	if err := rejectBanned([]reason{{"this skip", reasonText}}); err != nil {
		return err
	}
	model := "(none)"
	if p := s.strs["model"]; p != nil {
		model = *p
	}
	rid, head := newRunID(), git("rev-parse", "HEAD")
	if err := appendPlain(map[string]any{
		"run_id": rid, "repo": repoID(), "branch": git("rev-parse", "--abbrev-ref", "HEAD"),
		"head": head, "orchestrator_model": model, "session_kind": sessionKind(),
		"session_id": sessionID(), "phase": "plan", "planned_at": now(),
		"tier_floor": "skipped", "gates": map[string]any{},
	}); err != nil {
		return err
	}
	if err := appendPlain(map[string]any{
		"run_id": rid, "phase": "finish", "finished_at": now(), "outcome": "skipped",
		"tier_executed": "skipped", "executed": map[string]any{}, "skip_reason": reasonText,
		"head_at_finish": head,
	}); err != nil {
		return err
	}
	fmt.Fprintln(out, rid)
	return nil
}

func cmdAbandon(s *sub, out io.Writer) error {
	id, missing := *s.strs["run-id"], *s.strs["missing"]
	// A finish is terminal: merged field-wise, an abandonment over it would sit beside that
	// run's `executed: {all done}`.
	store, err := loadRaw(StorePath(), 0)
	if err != nil {
		return err
	}
	if o := store.get(id)["outcome"]; truthy(o) {
		return fail("runlog: %s already finished as %s — not overwriting", id, pyRepr(o))
	}
	if pyStrip(missing) == "" {
		return fail("runlog: --missing needs the gates that did not run — an abandonment with " +
			"nothing named is the silent skip this record exists to prevent")
	}
	// The push refusal's dead-session advice prints `--missing '<gates>'`. Pasted unchanged
	// it would record the placeholder as the missing gates, where the old unquoted form at
	// least failed in the shell. Only the placeholder itself is refused, bare or single-quoted.
	if m := pyStrip(missing); m == "<gates>" || m == "'<gates>'" {
		return fail("runlog: --missing is still the placeholder '<gates>' — replace it with the " +
			"planned gates that were not run and why")
	}
	if err := appendPlain(map[string]any{
		"run_id": id, "phase": "finish", "finished_at": now(), "outcome": "abandoned",
		"abandoned_missing": missing, "head_at_finish": git("rev-parse", "HEAD"),
	}); err != nil {
		return err
	}
	fmt.Fprintf(out, "runlog: marked %s abandoned — %s\n", id, missing)
	return nil
}

// unfinished is planned runs in this repo with no finish, newest first. A run recorded without
// a session id still matches a session filter: dropping it would stop guarding it silently.
func unfinished(repo, head string, session any) ([]map[string]any, error) {
	s, err := loadRaw(StorePath(), TailLines())
	if err != nil {
		return nil, err
	}
	var out []map[string]any
	for _, id := range s.order {
		run := s.runs[id]
		// The FACT of being finished, not the latest phase: a row appended after `finish`
		// overwrites `phase` in the merge and would resurrect a closed run.
		if run["repo"] != repo || truthy(run["finished_at"]) || truthy(run["outcome"]) {
			continue
		}
		if head != "" && run["head"] != head {
			continue
		}
		if truthy(session) && truthy(run["session_id"]) && run["session_id"] != session {
			continue
		}
		out = append(out, run)
	}
	sort.SliceStable(out, func(i, j int) bool {
		return pyStrOrEmpty(out[i]["planned_at"]) > pyStrOrEmpty(out[j]["planned_at"])
	})
	return out, nil
}

type openRun struct {
	RunID        any       `json:"run_id"`
	Head         any       `json:"head"`
	TierFloor    any       `json:"tier_floor"`
	PlannedGates []string  `json:"planned_gates"`
	AlsoOpen     []openRun `json:"also_open,omitempty"`
}

func cmdCheck(s *sub, out io.Writer) (int, error) {
	session := any(nil)
	if p := s.strs["session"]; p != nil && *p != "" {
		session = *p
	} else {
		session = sessionID()
	}
	open, err := unfinished(repoID(), orStr(s.strs["head"]), session)
	if err != nil {
		return 0, err
	}
	var mine []map[string]any
	for _, r := range open {
		if id := orStr(s.strs["run-id"]); id == "" || r["run_id"] == id {
			mine = append(mine, r)
		}
	}
	if len(mine) == 0 {
		fmt.Fprintln(out, "runlog: no unfinished run")
		return 0, nil
	}
	// Every unfinished run, not just the newest: a session that re-planned after an error
	// leaves an earlier one open.
	var fresh []openRun
	for _, r := range mine {
		if r["phase"] != "nudge" || *s.bools["force"] {
			fresh = append(fresh, openRun{r["run_id"], r["head"], r["tier_floor"], sortedKeys(plannedGates(r)), nil})
		}
	}
	if len(fresh) == 0 {
		fmt.Fprintln(out, "runlog: unfinished run already nudged")
		return 0, nil
	}
	payload := fresh[0]
	if len(fresh) > 1 {
		payload.AlsoOpen = fresh[1:]
	}
	if err := writeIndented(out, payload); err != nil {
		return 0, err
	}
	return 1, nil
}

// writeIndented is json.dumps(v, indent=1): the same layout, without HTML escaping.
func writeIndented(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", " ")
	return enc.Encode(v)
}

func cmdShow(id string, out io.Writer) error {
	store, err := loadRaw(StorePath(), 0)
	if err != nil {
		return err
	}
	run := store.get(id)
	if len(run) == 0 {
		return fail("runlog: no run %s", id)
	}
	return writeIndented(out, run)
}

// cmdPending is Step 0c's worklist: pending-report files whose run was planned in THIS repo on
// THIS branch. Run dirs are shared by every repo, branch and linked worktree, so listing them
// all would post another branch's report and label onto this PR.
func cmdPending(out, errOut io.Writer) error {
	br := git("rev-parse", "--abbrev-ref", "HEAD")
	common := git("rev-parse", "--path-format=absolute", "--git-common-dir")
	if br == "" || br == "HEAD" || common == "" {
		fmt.Fprintln(errOut, "runlog: no current branch — pending reports left in place")
		return nil
	}
	s, err := loadRaw(StorePath(), 0)
	if err != nil {
		return err
	}
	repo := repoID()
	ps, perr := evidence.PendingReports("")
	if perr != nil {
		fmt.Fprintf(errOut, "runlog: %v\n", perr)
	}
	for _, p := range ps {
		f, rid := p.Path, p.ID
		run := s.get(rid)
		switch {
		case run == nil:
			fmt.Fprintf(errOut, "runlog: no run record for %s — left in place\n", f)
		case otherRepo(run, repo):
		case run["branch"] == br:
			fmt.Fprintln(out, f)
		}
	}
	return nil
}

// cmdPendingAll is every pending-report file of THIS repo's runs, one per line as
// `<path>\t<branch>`, the branch being the one its run was planned on. cmdPending's filter is
// right for the PR of the branch checked out, and it is the only way a report is ever flushed —
// so a report deferred on a branch that is later left (a merged PR, or `main`, which gets no PR)
// was listed by no invocation at all and sat there for good. This lists them so each can be
// flushed against its OWN branch's PR. Unlike cmdPending it needs no current branch: the
// orphans are found from main or a detached HEAD as well. A file whose run has no record, or
// no branch recorded (or only a detached HEAD), cannot be routed, so it is named on stderr and
// left in place.
func cmdPendingAll(out, errOut io.Writer) error {
	common := git("rev-parse", "--path-format=absolute", "--git-common-dir")
	if common == "" {
		fmt.Fprintln(errOut, "runlog: not in a git repository — pending reports left in place")
		return nil
	}
	s, err := loadRaw(StorePath(), 0)
	if err != nil {
		return err
	}
	repo := repoID()
	ps, perr := evidence.PendingReports("")
	if perr != nil {
		fmt.Fprintf(errOut, "runlog: %v\n", perr)
	}
	for _, p := range ps {
		run := s.get(p.ID)
		br, _ := run["branch"].(string)
		switch {
		case run == nil:
			fmt.Fprintf(errOut, "runlog: no run record for %s — left in place\n", p.Path)
		case otherRepo(run, repo):
			// another repo's run: its own repo's pending lists it
		case br == "" || br == "HEAD":
			// "HEAD" is what plan records on a detached HEAD: no branch, so no PR to route to.
			fmt.Fprintf(errOut, "runlog: no branch recorded for %s — left in place\n", p.Path)
		default:
			fmt.Fprintf(out, "%s\t%s\n", p.Path, br)
		}
	}
	return nil
}

// otherRepo is whether run was planned in a repo other than repo. Run dirs are shared by every
// repo, so a pending report is scoped by its run's recorded repo; a row that records none
// cannot be scoped and is not filtered out.
func otherRepo(run map[string]any, repo string) bool {
	r, ok := run["repo"].(string)
	return ok && r != repo
}

// RejectBanned is runlog.reject_banned for one free-text reason, for writers outside the run
// record that take the same ban: learn's Dismissed entries.
func RejectBanned(where, text string) error { return rejectBanned([]reason{{where, text}}) }

// PyRepr is repr() of a str.
func PyRepr(s string) string { return pyRepr(s) }
