// Package stats reads the run record for what actually happens across review-loop runs. It is
// a port of the skill's review-stats.py, and like that script it has two consumers:
//
//	looper stats            ad-hoc, when you want to know
//	looper stats --alarm    called by plan.py at Step 0, a few lines or silence
//
// The alarm exists because repeat count beats calendar. A gate dropped three times is a
// stronger signal than "it is time for a batch review", and Step 0 is the one moment the
// skill is already open and would act on it.
//
// Its own package rather than part of internal/record, like push and report: it is a
// CONSUMER of the record, and everything it needs from the record is exported — the typed
// load in store order, the derivations (DroppedGates, Convergence), the merged run-level
// text fields, and the store path and tail. Inside internal/record it could reach the
// write half's dict reader instead, and a second reading of the store is the drift this
// port exists to retire.
package stats

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/pinepeakdigital/looper/internal/record"
)

// AlarmThreshold and AlarmWindow are review-stats.py's ALARM_THRESHOLD and ALARM_WINDOW: a
// gate is raised on its third non-completion within the last fifty runs.
const (
	AlarmThreshold = 3
	AlarmWindow    = 50
)

// DeclinedStates are the statuses that account for a gate rather than fail it. A gate the
// run accounted for is a different signal from one it failed to complete, and the two have
// different fixes — counting them together made the alarm report correct behaviour as
// failure: five reported misses of `record_reviewed` over 21 runs contained no real miss.
//
// NEITHER vocabulary is closed. `looper record finish` requires a reason for every status but
// `done` and never validates the string, so any spelling can reach here. What settles it is
// the DEFAULT: an unrecognised status is far likelier to be a new failure or a bug than a new
// deliberate word, so anything not listed falls to the loud bucket, and the message names it.
// Enumerating the failures instead put `error`, `timeout`, `crashed` and even `FAILED` in the
// benign bucket; enumerating the deliberate words from memory missed `waived`, which
// references/measurement-gate.md documents as passing the gate.
//
// `blocked` and `passed` are deliberately absent. references/finish.md contrasts a blocked
// evidence gate with a waived one ("A *waived* gate is not blocked — that one pushes."), and
// `passed` is report prose, not an `--executed` status: accounting for it here would put this
// alarm at odds with the tier `looper record finish` derives, which also leaves it out.
//
// Not fixed by widening record.GateOK: the tier reads that to decide `partial`, and a run that
// did not complete a planned gate genuinely is partial.
var DeclinedStates = []string{"skipped", "pending", "deferred", "waived"}

const (
	noAbandonRecord     = "no abandon record — the run stopped and nothing said why"
	abandonReasonsShown = 3
	reasonClip          = 96
	runAbandoned        = "(run abandoned)"
)

// run is one run as stats reads it, or the reason it cannot be read.
type run struct {
	r                                   *record.Run
	plannedAt, repo, sessionID, kind    string
	abandonedMissing                    string
	repoOK                              bool // the repo field itself read, so the filter may apply
	err                                 error
	outcome, tierFloor, tierExec, model string
}

// read takes what stats needs off a loaded run. A run is unreadable when any field stats
// reads could not be: the typed decode refused a row (r.Err), or a merged text field is not a
// string. Unreadable runs keep their place in the order — they are runs, and the window
// counts them — but nothing is derived from them; both modes say how many there were.
func read(r *record.Run) run {
	x := run{r: r, err: r.Err}
	var errs []error
	if r.Err != nil {
		errs = append(errs, r.Err)
	}
	text := func(field string, into *string) bool {
		v, err := r.Text(field)
		if err != nil {
			errs = append(errs, err)
			return false
		}
		*into = v
		return true
	}
	text("planned_at", &x.plannedAt)
	x.repoOK = text("repo", &x.repo)
	text("session_id", &x.sessionID)
	text("session_kind", &x.kind)
	text("abandoned_missing", &x.abandonedMissing)
	x.err = errors.Join(errs...)
	if r.Plan != nil {
		x.tierFloor, x.model = r.Plan.TierFloor, r.Plan.OrchestratorModel
	}
	if r.Finish != nil {
		x.outcome, x.tierExec = r.Finish.Outcome, r.Finish.TierExecuted
	}
	return x
}

// load is review-stats.load(): runs oldest-first, so the last N are the most recent N. The
// sort is STABLE over store order, as Python's sorted() is over load()'s insertion order,
// which decides every tie and where every run with no planned_at lands — first.
func load() ([]run, int, error) {
	s, err := record.LoadStore(record.StorePath(), record.TailLines())
	if err != nil {
		return nil, 0, err
	}
	runs := make([]run, 0, len(s.Runs))
	for _, r := range s.Runs {
		runs = append(runs, read(r))
	}
	sort.SliceStable(runs, func(i, j int) bool { return runs[i].plannedAt < runs[j].plannedAt })
	return runs, s.Unattributed, nil
}

// currentSession is `CLAUDE_CODE_SESSION_ID or AO_SESSION_ID`, where a blank value is as good
// as unset.
func currentSession() string {
	if v := os.Getenv("CLAUDE_CODE_SESSION_ID"); v != "" {
		return v
	}
	return os.Getenv("AO_SESSION_ID")
}

// abandoned reports an open run that no live session owns.
//
// Nothing writes this outcome when it happens: the Stop hook fires at every turn boundary, so
// it cannot tell a run mid-flight from one that died. Reading is the moment the question is
// answerable — a plan with no finish, from a session that is not the one asking, was
// abandoned. Invoked with no session env, nothing can tell a dead run from one in flight
// elsewhere, and a run that never named a session is unknown, not dead: guessing there
// reported every headless run (session_id null) abandoned, and three of those in 50 tripped
// the alarm at Step 0 of every later run.
func (x run) abandoned(here string) bool {
	if x.outcome != "" {
		// Explicitly abandoned counts as abandoned wherever it is read; anything else with an
		// outcome is finished.
		return x.outcome == "abandoned"
	}
	if here == "" {
		return false
	}
	return x.sessionID != "" && x.sessionID != here
}

// counter is collections.Counter as these reports use it: most_common orders by count,
// highest first, and keeps first-insertion order among ties.
type counter struct {
	keys []string
	n    map[string]int
}

func newCounter() *counter { return &counter{n: map[string]int{}} }

func (c *counter) add(k string, d int) {
	if _, ok := c.n[k]; !ok {
		c.keys = append(c.keys, k)
	}
	c.n[k] += d
}

func (c *counter) len() int { return len(c.keys) }

func (c *counter) mostCommon(limit int) []string {
	out := append([]string(nil), c.keys...)
	sort.SliceStable(out, func(i, j int) bool { return c.n[out[i]] > c.n[out[j]] })
	if limit >= 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

func (c *counter) joined(sep string) string {
	parts := make([]string, 0, len(c.keys))
	for _, k := range c.mostCommon(-1) {
		parts = append(parts, fmt.Sprintf("%s=%d", k, c.n[k]))
	}
	return strings.Join(parts, sep)
}

// flat is the GROUPING key, not just a display step: keying the counter on the raw text and
// flattening only at print time made three whitespace-variant copies of one reason print as
// three separate `1x` lines — the failure the grouping was added to fix.
func flat(s string) string { return strings.Join(strings.FieldsFunc(s, record.PySpace), " ") }

// clip bounds a reason for display, truncated in the MIDDLE. Two reasons sharing a long
// prefix — likely here, since these name gates — clipped from the end alone printed as
// identical lines with separate counts, which reads as the counter being broken. Measured in
// characters, as Python's len() is.
func clip(s string) string {
	r := []rune(flat(s))
	if len(r) <= reasonClip {
		return string(r)
	}
	head := (reasonClip - 1) * 2 / 3
	return string(r[:head]) + "…" + string(r[len(r)-(reasonClip-head-1):])
}

// naming is the statuses behind a count, for the message — `unreported` adds nothing to read.
func naming(seen map[string]bool) string {
	var named []string
	for s := range seen {
		if s != "unreported" {
			named = append(named, s)
		}
	}
	if len(named) == 0 {
		return ""
	}
	sort.Strings(named)
	return " (" + strings.Join(named, ", ") + ")"
}

func declined(status string) bool {
	for _, s := range DeclinedStates {
		if s == status {
			return true
		}
	}
	return false
}

// droppedNames is DroppedGates in a fixed order. The Python iterates a set of gate names here,
// whose order is hash-seeded per process, so which of two gates first dropped by the same run
// is "first seen" — and so which leads a tie in the report — changes from one invocation to
// the next. Sorted by name, this one does not.
func droppedNames(x run) ([]string, map[string]string) {
	d, err := x.r.DroppedGates()
	if err != nil {
		// Every caller skips unreadable runs first, and DroppedGates fails only on r.Err, so this
		// cannot run today. Loud if it ever does: an empty map here would read as "nothing
		// dropped" — the quiet clean sweep the DroppedGates deferral decided against.
		panic("stats: droppedNames on an unreadable run: " + err.Error())
	}
	names := make([]string, 0, len(d))
	for g := range d {
		names = append(names, g)
	}
	sort.Strings(names)
	return names, d
}

type hit struct {
	n    int
	gate string
	what string
}

func alarm(out io.Writer, runs []run) {
	recent := runs
	if len(recent) > AlarmWindow {
		recent = recent[len(recent)-AlarmWindow:]
	}
	here := currentSession()
	dropped, declinedC, abandonWhy := newCounter(), newCounter(), newCounter()
	dropWhy, declineWhy := map[string]map[string]bool{}, map[string]map[string]bool{}
	note := func(m map[string]map[string]bool, g, st string) {
		if m[g] == nil {
			m[g] = map[string]bool{}
		}
		m[g][st] = true
	}
	unreadable := 0
	for _, x := range recent {
		if x.err != nil {
			// Counted, not dropped. Every gate this run planned is invisible to the counts
			// below, so a window holding it can stay silent over a real repeat offender; the
			// alarm says so instead of looking clean.
			unreadable++
			continue
		}
		if x.abandoned(here) {
			// One event, one counter. An abandoned run has every planned gate unreported by
			// construction, so also attributing it per gate turned one abandonment into nine
			// gate failures. And an EXPLICIT abandon is not a softer signal than an inferred
			// one: three of them are three reviews that did not happen.
			dropped.add(runAbandoned, 1)
			// Keep WHY. The count alone is not actionable and reads as broken. An inferred
			// abandonment has no reason to print, and saying so IS the actionable fact.
			why := x.abandonedMissing
			if why == "" {
				why = noAbandonRecord
			}
			abandonWhy.add(flat(why), 1)
			continue
		}
		names, d := droppedNames(x)
		for _, g := range names {
			st := d[g]
			if declined(st) {
				declinedC.add(g, 1)
				note(declineWhy, g, st)
			} else {
				dropped.add(g, 1)
				note(dropWhy, g, st)
			}
		}
	}

	var hits []hit
	for _, g := range dropped.keys {
		if n := dropped.n[g]; n >= AlarmThreshold {
			hits = append(hits, hit{n, g, "did not complete" + naming(dropWhy[g])})
		}
	}
	for _, g := range declinedC.keys {
		if n := declinedC.n[g]; n >= AlarmThreshold {
			hits = append(hits, hit{n, g, "was declined with a reason" + naming(declineWhy[g])})
		}
	}
	// A gate that fails BOTH ways still needs raising: two of each is four non-completions,
	// and thresholding the buckets independently left that silent.
	reported := map[string]bool{}
	for _, h := range hits {
		reported[h.gate] = true
	}
	both := append(append([]string(nil), dropped.keys...), declinedC.keys...)
	seen := map[string]bool{}
	for _, g := range both {
		if seen[g] {
			continue
		}
		seen[g] = true
		total := dropped.n[g] + declinedC.n[g]
		if !reported[g] && total >= AlarmThreshold {
			hits = append(hits, hit{total, g, fmt.Sprintf(
				"did not complete %dx and was declined %dx, in varying ways", dropped.n[g], declinedC.n[g])})
		}
	}
	if len(hits) == 0 && unreadable == 0 {
		return
	}
	// Sorted across both buckets, because the printed order reads as a ranking.
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].n != hits[j].n {
			return hits[i].n > hits[j].n
		}
		return hits[i].gate < hits[j].gate
	})
	fmt.Fprintf(out, "review-loop alarm — over the last %d runs:\n", len(recent))
	for _, h := range hits {
		// The Python tests the MESSAGE for "varying ways", not which bucket it came from, so a
		// status spelled that way drops its count too. Reproduced, not fixed: it is a parity
		// port, and the line still names the gate.
		if strings.Contains(h.what, "varying ways") {
			fmt.Fprintf(out, "  %s %s\n", h.gate, h.what)
		} else {
			fmt.Fprintf(out, "  %s %s %dx\n", h.gate, h.what, h.n)
		}
		if h.gate == runAbandoned {
			// Grouped, commonest first, so one cause repeated reads as one cause repeated.
			for _, why := range abandonWhy.mostCommon(abandonReasonsShown) {
				fmt.Fprintf(out, "      %dx %s\n", abandonWhy.n[why], clip(why))
			}
			if rest := abandonWhy.len() - abandonReasonsShown; rest > 0 {
				fmt.Fprintf(out, "      ...and %d other reason(s) — `looper stats` has them all\n", rest)
			}
		}
	}
	if unreadable > 0 {
		fmt.Fprintf(out, "  %d run(s) here could not be read, so no gate above counts them — "+
			"`looper stats` names them\n", unreadable)
	}
	if len(hits) > 0 {
		fmt.Fprintln(out, "  Repeat count beats calendar: fix the gate or change the plan, don't log it again.")
	}
}

func in(s string, set []string) bool {
	for _, x := range set {
		if x == s {
			return true
		}
	}
	return false
}

func or(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// thousands is Python's `{:,.0f}`.
func thousands(f float64) string {
	s := strconv.FormatFloat(f, 'f', 0, 64)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

func report(out io.Writer, all []run, unattributed int, repo string) {
	runs := all
	if repo != "" {
		runs = nil
		for _, x := range all {
			// A run whose repo field could not be read is kept: it cannot be shown to be
			// elsewhere, and dropping it would hide exactly the run that needs reading.
			if !x.repoOK || strings.Contains(x.repo, repo) {
				runs = append(runs, x)
			}
		}
	}
	if len(runs) == 0 {
		// The Python's answer when there is truly nothing: the headline and tallies below with
		// zero runs printed `runs: 0` and five bare `outcome: ` lines. Rows that belong to no run
		// make "no runs recorded" false — the store holds rows — so that case says what is true
		// and names them; they cannot be shown to be in another repo, any more than a run whose
		// repo field cannot be read.
		if unattributed == 0 {
			fmt.Fprintln(out, "no runs recorded")
			return
		}
		fmt.Fprintln(out, "no runs this build can read")
		fmt.Fprintf(out, "  %d row(s) carry a run_id that is not a string, so they belong to no run "+
			"this build can read\n", unattributed)
		return
	}
	var ok, bad []run
	for _, x := range runs {
		if x.err != nil {
			bad = append(bad, x)
		} else {
			ok = append(ok, x)
		}
	}
	here := currentSession()
	var fin, lost []run
	skipped, carried := 0, 0
	for _, x := range ok {
		switch {
		case x.abandoned(here):
			lost = append(lost, x)
		case in(x.outcome, record.SubcommandStates):
			// Bookkeeping, not review passes: `carried` is a rebase moving an existing record
			// onto a new sha, `skipped` is a change judged beneath the loop. Each has an
			// outcome, so each would otherwise count as finished and inflate the one number
			// here that reports review cadence.
			if x.outcome == "skipped" {
				skipped++
			} else {
				carried++
			}
		case x.outcome != "":
			fin = append(fin, x)
		}
	}
	open := len(ok) - len(fin) - len(lost) - skipped - carried
	label := "in flight (this session)"
	if here == "" {
		label = "open (unknown — run from the session that owns them)"
	}
	fmt.Fprintf(out, "runs: %d  finished: %d  abandoned: %d  %s: %d", len(runs), len(fin), len(lost), label, open)
	if skipped > 0 {
		fmt.Fprintf(out, "  skipped (judged beneath the loop, not a review): %d", skipped)
	}
	if carried > 0 {
		fmt.Fprintf(out, "  carried (rebase bookkeeping, not a review): %d", carried)
	}
	if len(bad) > 0 {
		fmt.Fprintf(out, "  unreadable (listed at the end, counted nowhere else): %d", len(bad))
	}
	fmt.Fprintln(out)

	tally := func(label string, key func(run) string) {
		if len(ok) == 0 {
			return // every run unreadable: an empty tally line says nothing the list below doesn't
		}
		c := newCounter()
		for _, x := range ok {
			c.add(or(key(x), "(unset)"), 1)
		}
		fmt.Fprintf(out, "\n%s: %s\n", label, c.joined("  "))
	}
	tally("outcome", func(x run) string { return x.outcome })
	tally("tier floor", func(x run) string { return x.tierFloor })
	tally("tier executed", func(x run) string { return x.tierExec })
	tally("session kind", func(x run) string { return x.kind })
	tally("orchestrator model", func(x run) string { return x.model })

	// Convergence and spend, derived from the cycle rows over FINISHED runs — the only place
	// the agent cap's number can be chosen with evidence rather than guessed.
	conv := newCounter()
	for _, x := range fin {
		c, _ := x.r.Convergence() // r.Err is nil for every run in ok
		conv.add(c, 1)
	}
	if conv.len() > 0 {
		fmt.Fprintf(out, "\nconvergence: %s\n", conv.joined("  "))
	}

	// Agents is the cap's unit because it is derivable; tokens are the real cost. Both over
	// `fin`, the denominator's own population: summing cycles across ALL runs while dividing
	// by finished ones reported "mean agents/run: 24.0" for one finished 4-agent run beside an
	// in-flight 20-agent one.
	cycles, agents, tokens := 0, 0, 0
	for _, x := range fin {
		for _, c := range x.r.Cycles {
			cycles++
			if c.Agents != nil {
				agents += *c.Agents
			}
			if c.SubagentTokens != nil {
				tokens += *c.SubagentTokens
			}
		}
	}
	if cycles > 0 {
		bits := []string{fmt.Sprintf("cycles: %d", cycles), fmt.Sprintf("agents: %d", agents),
			fmt.Sprintf("mean agents/run: %.1f", float64(agents)/float64(len(fin)))}
		if tokens != 0 && agents != 0 {
			bits = append(bits, "mean tokens/agent: "+thousands(float64(tokens)/float64(agents)))
		} else if tokens == 0 {
			// Absence is the finding: the cap cannot move off its agent-count proxy until
			// something records what the agents actually cost.
			bits = append(bits, "tokens: none recorded")
		}
		fmt.Fprintf(out, "\n%s\n", strings.Join(bits, "  "))
	}

	drops := newCounter()
	byModel, byKind := map[string]*counter{}, map[string]*counter{}
	bump := func(m map[string]*counter, k, g string) {
		if m[k] == nil {
			m[k] = newCounter()
		}
		m[k].add(g, 1)
	}
	for _, x := range ok {
		names, _ := droppedNames(x)
		for _, g := range names {
			drops.add(g, 1)
			bump(byModel, or(x.model, "(unset)"), g)
			bump(byKind, or(x.kind, "(unset)"), g)
		}
	}
	if drops.len() > 0 {
		fmt.Fprintln(out, "\ngates planned but not completed:")
		for _, g := range drops.mostCommon(-1) {
			fmt.Fprintf(out, "  %3d  %s\n", drops.n[g], g)
		}
		breakdown := func(m map[string]*counter) {
			keys := make([]string, 0, len(m))
			for k := range m {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				var parts []string
				for _, g := range m[k].mostCommon(4) {
					parts = append(parts, fmt.Sprintf("%s×%d", g, m[k].n[g]))
				}
				fmt.Fprintf(out, "    %s: %s\n", k, strings.Join(parts, ", "))
			}
		}
		fmt.Fprintln(out, "\n  by orchestrator model:")
		breakdown(byModel)
		fmt.Fprintln(out, "  by session kind:")
		breakdown(byKind)
	}

	esc := newCounter()
	for _, x := range ok {
		if x.r.Finish == nil {
			continue
		}
		for _, e := range x.r.Finish.Escalations {
			if e.IsNull() {
				continue
			}
			g := "(unnamed)"
			if e.Gate != nil && *e.Gate != "" {
				g = *e.Gate
			}
			esc.add(g, 1)
		}
	}
	if esc.len() > 0 {
		fmt.Fprintln(out, "\nescalations above the computed floor (thresholds may be too loose):")
		for _, g := range esc.mostCommon(-1) {
			fmt.Fprintf(out, "  %3d  %s\n", esc.n[g], g)
		}
	}

	// Every abandonment reason, untruncated: the alarm shows the top few and points here.
	why := newCounter()
	for _, x := range lost {
		why.add(flat(or(x.abandonedMissing, noAbandonRecord)), 1)
	}
	if why.len() > 0 {
		fmt.Fprintln(out, "\nwhy runs were abandoned:")
		for _, w := range why.mostCommon(-1) {
			fmt.Fprintf(out, "  %3d  %s\n", why.n[w], w)
		}
	}

	asks := 0
	for _, x := range ok {
		if x.r.Finish != nil && x.r.Finish.UnresolvedAsks != nil {
			asks += *x.r.Finish.UnresolvedAsks
		}
	}
	if asks != 0 {
		fmt.Fprintf(out, "\nunresolved ask-bucket items across all runs: %d\n", asks)
	}

	// Not in review-stats.py, which either stringified these, crashed on them, or counted them
	// as it happened to. A run this build cannot read is still a run, so it is named here
	// rather than left out of every number above without a trace.
	if len(bad) > 0 || unattributed > 0 {
		fmt.Fprintln(out, "\nruns this build could not read (counted in runs:, nowhere else):")
		for _, x := range bad {
			for _, line := range strings.Split(x.err.Error(), "\n") {
				fmt.Fprintf(out, "  %s\n", line)
			}
		}
		if unattributed > 0 {
			fmt.Fprintf(out, "  %d row(s) carry a run_id that is not a string, so they belong to no run "+
				"this build can read (not counted in runs:)\n", unattributed)
		}
	}
}

// Usage is the command line, for cmd/looper's banner and -h.
const Usage = "usage: looper stats [--alarm] [--repo <substring>]"

// Command runs `looper stats` and returns review-stats.py's exit status: 0 for either report,
// 2 for a usage error, 1 when the store exists and cannot be read.
func Command(args []string, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("stats", flag.ContinueOnError)
	fs.SetOutput(errOut)
	fs.Usage = func() { fmt.Fprintln(errOut, Usage) }
	alarmMode := fs.Bool("alarm", false, "print only repeat-offender gates; silent when clean")
	repo := fs.String("repo", "", "filter to runs whose repo path contains this")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(errOut, "%s\nlooper stats: unrecognized arguments: %s\n", Usage, strings.Join(fs.Args(), " "))
		return 2
	}
	runs, unattributed, err := load()
	if err != nil {
		// review-stats.py dies here with a traceback (IsADirectoryError, PermissionError) and
		// exit 1. Same status, a sentence instead — an unreadable store is never "no runs".
		fmt.Fprintf(errOut, "looper stats: cannot read the run store: %v\n", err)
		return 1
	}
	if *alarmMode {
		alarm(out, runs)
	} else {
		report(out, runs, unattributed, *repo)
	}
	return 0
}
