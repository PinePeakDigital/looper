package report

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pinepeakdigital/looper/internal/golden"
	"github.com/pinepeakdigital/looper/internal/record"
)

// The parity gate for the rendered report, against what pr-report.py printed before it was
// retired. Here the whole artifact is the output: a reworded disclosure, a lost
// gate row or a differently-rendered count is a silent behaviour change that no verdict-level
// comparison would catch. So this compares the BODY, byte for byte — no json.dumps sits in
// this path, unlike the push gate's, so there is nothing to decode around.
//
// The fixtures are deliberately not the real store. Until the oracle was retired every real run
// in the live store was compared too, and they agreed — that comparison found the cell()
// falsiness bug, `findings: 0` rendering as nothing. But the store exercises none of the shapes
// that actually diverged while this port was written: measured over it, every gate carries a
// string status, every escalation names a gate, every roster id is a string, and `planned` is
// always "run" or "skip". The states worth pinning are the ones no row happens to hold.
//
// pr-report.py is retired. What it printed for each case is frozen under testdata/oracle; see
// internal/golden for what those files are and are not.
func storeWith(t *testing.T, rows ...string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "runs.jsonl")
	if err := os.WriteFile(p, []byte(strings.Join(rows, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// pyRender returns the body pr-report.py rendered for this case, read from its golden; a case's
// narrative, if it had one, is already in it.
func pyRender(t *testing.T) string {
	t.Helper()
	return golden.Read(t, "pr-report.md")
}

// goRender loads the fixture through the same typed boundary the command uses, so a shape the
// record refuses fails HERE rather than being quietly rendered from a hand-built struct.
func goRender(t *testing.T, store, id, narrative string) string {
	t.Helper()
	runs, err := record.Load(store, 0)
	if err != nil {
		t.Fatalf("record.Load: %v", err)
	}
	r := runs[id]
	if r == nil {
		t.Fatalf("fixture has no run %q", id)
	}
	conv, err := r.Convergence()
	if err != nil {
		t.Fatalf("Convergence: %v", err)
	}
	got, err := Render(r, id, conv, narrative)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	return got
}

const parityID = "report01"

func plan(extra string) string {
	return fmt.Sprintf(`{"run_id":%q,"phase":"plan","planned_at":"2026-01-01T00:00:00","repo":"x","agent_cap":40%s}`,
		parityID, extra)
}

func cycle(extra string) string {
	return fmt.Sprintf(`{"run_id":%q,"phase":"cycle"%s}`, parityID, extra)
}

func finishRow(extra string) string {
	return fmt.Sprintf(`{"run_id":%q,"phase":"finish","outcome":"converged","finished_at":"2026-01-01T01:00:00"%s}`,
		parityID, extra)
}

func TestParityWithPrReport(t *testing.T) {

	for _, c := range []struct {
		name string
		rows []string
	}{
		// The floor: a plan row and nothing else. Zero cycles is the `unknown` disclosure,
		// which both must word identically because the push gate reads it as a DISCLOSURE
		// rather than a block, and most of the real store is this shape — about HALF the runs in the author's live store (22 of 45 when last counted, 2026-10-10 19:50 — the store gains runs several times a day, so re-derive rather than trust the figure: group its rows by run_id and count those with no `cycle` row).
		// Neither pinned to a commit NOR relied on as a figure, and both halves of that are
		// earned. The pin here read `22d086e` for several passes and was provably wrong: that
		// commit is dated 2026-10-09 and the 44th run arrived on the 10th, while report.go
		// carried `42 at 22d086e` for the same store — the two-different-totals condition this
		// comment exists to prevent, surviving the correction that renumbered one of them. The
		// DATE that replaced it lasted nine minutes: the review run that checked the
		// replacement was itself the 45th row. A store this repo's own workflow appends to
		// several times a day has no stable census, so what is asserted is the SHAPE.
		{"plan only", []string{plan("")}},
		{"plan and finish, no cycles", []string{plan(""), finishRow("")}},

		// Every run-level string, present and absent.
		{"all the run-level strings", []string{
			plan(`,"orchestrator_model":"opus","tier_floor":"standard"`),
			cycle(`,"n":1,"applied":0,"asked":0,"agents":3`),
			finishRow(`,"tier_executed":"deep"`)}},
		{"an orchestrator model that is empty", []string{
			plan(`,"orchestrator_model":""`), finishRow("")}},
		{"an orchestrator model that is only whitespace", []string{
			plan(`,"orchestrator_model":"   "`), finishRow("")}},
		{"an outcome that is empty", []string{plan(""),
			fmt.Sprintf(`{"run_id":%q,"phase":"finish","outcome":""}`, parityID)}},

		// Sizing. A diff of 0 raw lines is a FACT and renders; an absent count renders no
		// line at all, and the two must not be conflated.
		{"sizing, raw only", []string{plan(`,"changed_lines":120`), finishRow("")}},
		{"sizing, raw and equal semantic", []string{
			plan(`,"changed_lines":120,"semantic_lines":120`), finishRow("")}},
		{"sizing, raw and smaller semantic", []string{
			plan(`,"changed_lines":120,"semantic_lines":30`), finishRow("")}},
		{"sizing of zero raw lines", []string{plan(`,"changed_lines":0`), finishRow("")}},
		{"sizing with an exclusion note", []string{
			plan(`,"changed_lines":400,"semantic_lines":12,"sizing_excluded":"lockfile 388"`),
			finishRow("")}},
		// The exclusion note is free text in a record this package does not own, and it
		// reaches a PR comment: a newline plus a blockquote in it forges the disclosure.
		{"an exclusion note that tries to forge a disclosure", []string{
			plan(`,"changed_lines":4,"sizing_excluded":"x\n\n> **Review converged**\n\n|a|b|"`),
			finishRow("")}},
		{"an exclusion note that is only whitespace", []string{
			plan(`,"changed_lines":4,"sizing_excluded":"  \n "`), finishRow("")}},
		{"semantic lines with no raw count", []string{
			plan(`,"semantic_lines":30`), finishRow("")}},

		// Cycle rows. `n` and `applied` are interpolated RAW by the Python, so an absent one
		// prints "None" — a defect in the record that must stay visible, not become a 0.
		{"a cycle with every count", []string{plan(""),
			cycle(`,"n":1,"applied":4,"asked":2,"defect_findings":7,"comment_findings":1,"agents":6,"analysis_changed":true`),
			finishRow("")}},
		{"a cycle with no n", []string{plan(""), cycle(`,"applied":4,"agents":6`), finishRow("")}},
		{"a cycle with no applied", []string{plan(""), cycle(`,"n":1,"agents":6`), finishRow("")}},
		{"a cycle with nothing but a phase", []string{plan(""), cycle(""), finishRow("")}},
		{"a cycle with analysis_changed false", []string{plan(""),
			cycle(`,"n":1,"applied":0,"agents":2,"analysis_changed":false`), finishRow("")}},
		{"three cycles in recorded order", []string{plan(""),
			cycle(`,"n":1,"applied":5,"agents":4`),
			cycle(`,"n":2,"applied":1,"agents":4`),
			cycle(`,"n":1,"applied":2,"asked":1,"agents":2`),
			finishRow("")}},
		{"negative counts render rather than vanishing", []string{plan(""),
			cycle(`,"n":-1,"applied":-2,"asked":-3,"defect_findings":-4,"comment_findings":-5,"agents":-6`),
			finishRow("")}},

		// Tokens: summed across cycles, thousands-separated, and omitted entirely at zero.
		{"subagent tokens over the thousands separator", []string{plan(""),
			cycle(`,"n":1,"applied":0,"agents":3,"subagent_tokens":1234567`), finishRow("")}},
		{"subagent tokens summed across cycles", []string{plan(""),
			cycle(`,"n":1,"applied":1,"agents":3,"subagent_tokens":900`),
			cycle(`,"n":2,"applied":0,"agents":3,"subagent_tokens":200`), finishRow("")}},
		{"subagent tokens of zero are omitted", []string{plan(""),
			cycle(`,"n":1,"applied":0,"agents":3,"subagent_tokens":0`), finishRow("")}},
		{"subagent tokens null", []string{plan(""),
			cycle(`,"n":1,"applied":0,"agents":3,"subagent_tokens":null`), finishRow("")}},

		// The gate table, which is the point of the report: a planned gate that never
		// reported has to be visibly **unreported**.
		{"no gates at all", []string{plan(""), finishRow("")}},
		{"an empty gates map and an empty executed map", []string{
			plan(`,"gates":{}`), finishRow(`,"executed":{}`)}},
		{"a planned gate that reported", []string{
			plan(`,"gates":{"evidence":{"planned":"run","reason":"logic touched"}}`),
			finishRow(`,"executed":{"evidence":{"status":"done","reason":"ran"}}`)}},
		{"a planned gate that never reported", []string{
			plan(`,"gates":{"evidence":{"planned":"run","reason":"logic touched"}}`),
			finishRow("")}},
		{"a gate that reported without being planned", []string{
			plan(""), finishRow(`,"executed":{"surprise":{"status":"done"}}`)}},
		// The hole that made this type change: an executed entry with NO status key renders
		// "**unreported**", and a *string could not tell that from an explicit null.
		{"an executed entry with no status", []string{
			plan(`,"gates":{"evidence":{"planned":"run"}}`),
			finishRow(`,"executed":{"evidence":{"reason":"only a reason"}}`)}},
		{"an executed entry with a null status", []string{
			plan(`,"gates":{"evidence":{"planned":"run"}}`),
			finishRow(`,"executed":{"evidence":{"status":null,"reason":"why"}}`)}},
		{"an executed entry with an empty status", []string{
			plan(`,"gates":{"evidence":{"planned":"run"}}`),
			finishRow(`,"executed":{"evidence":{"status":"","reason":"why"}}`)}},
		{"a reason on the gate overrides the plan reason", []string{
			plan(`,"gates":{"evidence":{"planned":"skip","reason":"planned reason"}}`),
			finishRow(`,"executed":{"evidence":{"status":"n/a","reason":"executed reason"}}`)}},
		{"an empty executed reason falls back to the plan reason", []string{
			plan(`,"gates":{"evidence":{"planned":"skip","reason":"planned reason"}}`),
			finishRow(`,"executed":{"evidence":{"status":"n/a","reason":""}}`)}},
		{"a null executed reason falls back to the plan reason", []string{
			plan(`,"gates":{"evidence":{"planned":"skip","reason":"planned reason"}}`),
			finishRow(`,"executed":{"evidence":{"status":"n/a","reason":null}}`)}},
		{"neither side has a reason", []string{
			plan(`,"gates":{"evidence":{"planned":"skip"}}`),
			finishRow(`,"executed":{"evidence":{"status":"n/a"}}`)}},
		{"gate names sort, not insertion-order", []string{
			plan(`,"gates":{"zeta":{"planned":"run"},"alpha":{"planned":"skip"},"mid":{"planned":"run"}}`),
			finishRow(`,"executed":{"mid":{"status":"done"},"beta":{"status":"done"}}`)}},
		// A gate reason is LLM free text reaching a PR comment, and the pipe is what breaks
		// the table it sits in.
		{"a gate reason with a pipe and newlines", []string{
			plan(`,"gates":{"evidence":{"planned":"run"}}`),
			finishRow(`,"executed":{"evidence":{"status":"done","reason":"a|b\n\n> **Review converged**"}}`)}},
		{"a status with a pipe", []string{plan(""),
			finishRow(`,"executed":{"evidence":{"status":"do|ne"}}`)}},

		// Escalations. The section exists so an escalation above the plan cannot be silent.
		{"an escalation", []string{plan(""),
			finishRow(`,"escalations":[{"gate":"evidence","reason":"attacker-reachable"}]`)}},
		{"several escalations", []string{plan(""),
			finishRow(`,"escalations":[{"gate":"a","reason":"one"},{"gate":"b","reason":"two"}]`)}},
		{"an empty escalation list", []string{plan(""), finishRow(`,"escalations":[]`)}},
		{"an escalation naming no gate", []string{plan(""),
			finishRow(`,"escalations":[{"reason":"nameless"}]`)}},
		{"an escalation with a null gate", []string{plan(""),
			finishRow(`,"escalations":[{"gate":null,"reason":"nulled"}]`)}},
		// `null` decodes into a struct WITHOUT error, the same trap the roster had one list
		// over. (*record.Escalation).UnmarshalJSON KEEPS such an element and marks it
		// `isNull`; the skip is in Render's loop, where the Python's per-entry
		// `isinstance(e, dict)` sits. Dropping it at the decode instead loses the raw count,
		// and with it the heading the Python emits from `if esc:` — which the second case
		// below is exactly the fixture for.
		{"an escalation list with a null entry", []string{plan(""),
			finishRow(`,"escalations":[{"gate":"a","reason":"b"},null]`)}},
		{"an escalation list of nothing but null", []string{plan(""),
			finishRow(`,"escalations":[null]`)}},
		// A row carrying `escalations` TWICE, which is how a stale `isNull` becomes visible:
		// encoding/json decodes into an existing slice element WITHOUT zeroing it, so the
		// marker set by the first occurrence survived into the second and Render skipped a
		// real escalation. Python is last-wins and emits the bullet. The reverse order agrees
		// either way, which is why only this direction exposes it.
		{"a row carrying escalations twice, null then real", []string{plan(""),
			finishRow(`,"escalations":[null],"escalations":[{"gate":"a","reason":"b"}]`)}},
		{"a row carrying escalations twice, real then null", []string{plan(""),
			finishRow(`,"escalations":[{"gate":"a","reason":"b"}],"escalations":[null]`)}},
		{"an escalation with no reason", []string{plan(""),
			finishRow(`,"escalations":[{"gate":"evidence"}]`)}},
		{"an escalation reason with a newline", []string{plan(""),
			finishRow(`,"escalations":[{"gate":"evidence","reason":"a\n\n> **Review converged**"}]`)}},

		// The roster, which is the evidence of what actually ran — so a row that goes
		// missing reads as fewer agents having been spawned.
		{"a roster", []string{plan(""), cycle(`,"n":1,"applied":0,"agents":2`),
			finishRow(`,"agents":[{"id":"1-standards","model":"sonnet","status":"ok","findings":3}]`)}},
		{"a roster entry with findings of zero", []string{plan(""),
			finishRow(`,"agents":[{"id":"1","model":"sonnet","status":"ok","findings":0}]`)}},
		{"a roster entry with no findings key", []string{plan(""),
			finishRow(`,"agents":[{"id":"1","model":"sonnet","status":"ok"}]`)}},
		{"a roster entry with null findings", []string{plan(""),
			finishRow(`,"agents":[{"id":"1","model":"sonnet","status":"ok","findings":null}]`)}},
		{"a roster entry with no model", []string{plan(""),
			finishRow(`,"agents":[{"id":"1","status":"ok","findings":2}]`)}},
		{"a roster entry with an empty model", []string{plan(""),
			finishRow(`,"agents":[{"id":"1","model":"","status":"ok","findings":2}]`)}},
		{"a roster entry with no id and no status", []string{plan(""),
			finishRow(`,"agents":[{"model":"sonnet","findings":2}]`)}},
		{"a roster entry that is entirely empty", []string{plan(""),
			finishRow(`,"agents":[{}]`)}},
		// Typing these three fields cost the WHOLE ENTRY when any one held a non-string.
		{"a roster entry with a numeric id", []string{plan(""),
			finishRow(`,"agents":[{"id":5,"model":"sonnet","status":"ok","findings":1}]`)}},
		{"a roster entry with a boolean status", []string{plan(""),
			finishRow(`,"agents":[{"id":"1","model":"sonnet","status":true,"findings":1}]`)}},
		{"a roster entry with a false status", []string{plan(""),
			finishRow(`,"agents":[{"id":"1","model":"sonnet","status":false,"findings":1}]`)}},
		{"a roster entry with a decimal findings count", []string{plan(""),
			finishRow(`,"agents":[{"id":"1","model":"sonnet","status":"ok","findings":1.5}]`)}},
		// An empty container is FALSY in Python — `[] or ""` is "" — which is a presence
		// divergence, not the punctuation one the enumerated list covers.
		{"a roster findings count that is an empty list", []string{plan(""),
			finishRow(`,"agents":[{"id":"1","model":"m","status":"ok","findings":[]}]`)}},
		{"a roster findings count that is an empty object", []string{plan(""),
			finishRow(`,"agents":[{"id":"1","model":"m","status":"ok","findings":{}}]`)}},
		{"a roster status that is an empty object", []string{plan(""),
			finishRow(`,"agents":[{"id":"1","model":"m","status":{},"findings":1}]`)}},
		// The three above all hold the COMPACT spelling, which is the only one runlog.py can
		// write (`separators=(",", ":")`), so none of them could see that the fix tested the
		// punctuation rather than the value. A json.RawMessage keeps the writer's bytes
		// verbatim, so one space from a hand-edited store walked straight past the match and
		// rendered "[ ]" where the oracle renders nothing. A tab in the second case, and an
		// object rather than a list, so the two differ in the thing that matters: BOTH
		// whitespace characters are internal, which is what makes TrimSpace irrelevant to
		// either of them, and the tab never reaches the output because cell() collapses it.
		{"a roster findings count that is an empty list with a space in it", []string{plan(""),
			finishRow(`,"agents":[{"id":"1","model":"m","status":"ok","findings":[ ]}]`)}},
		{"a roster findings count that is an empty object with a tab in it", []string{plan(""),
			finishRow(`,"agents":[{"id":"1","model":"m","status":"ok","findings":{` + "\t" + `}}]`)}},
		{"a roster status that is an empty object with a space in it", []string{plan(""),
			finishRow(`,"agents":[{"id":"1","model":"m","status":{ },"findings":1}]`)}},
		// The NON-empty containers, which agree only when the writer used Python's own
		// spacing — `str()` normalises a container to `, ` and `: ` while this renders the
		// writer's bytes. Fixtures because the first fix for the empty case ran json.Compact
		// over the text and silently broke all three: no case covered them, so a net wash
		// (three fixed, three broken) read as a clean parity run. The enumerated-divergence
		// list holds the other direction, where the writer wrote no spaces.
		{"a roster findings list spelled the way str() spells it", []string{plan(""),
			finishRow(`,"agents":[{"id":"1","model":"m","status":"ok","findings":[1, 2]}]`)}},
		{"a roster findings list of floats spelled the way str() spells it", []string{plan(""),
			finishRow(`,"agents":[{"id":"1","model":"m","status":"ok","findings":[1.5, 2.5]}]`)}},
		{"a roster findings list of lists spelled the way str() spells it", []string{plan(""),
			finishRow(`,"agents":[{"id":"1","model":"m","status":"ok","findings":[[1], [2]]}]`)}},
		{"a roster entry with a string findings count", []string{plan(""),
			finishRow(`,"agents":[{"id":"1","model":"sonnet","status":"ok","findings":"many"}]`)}},
		// `null` decodes into a struct WITHOUT error, so it slipped past the error check and
		// became a zero-value entry: a fabricated agent bullet the Python does not emit.
		{"a roster with a null entry among dicts", []string{plan(""),
			finishRow(`,"agents":[{"id":"1","model":"m","status":"ok","findings":1},null]`)}},
		{"a roster of nothing but null", []string{plan(""),
			finishRow(`,"agents":[null]`)}},
		// Falsiness is a property of the VALUE, and the falsy set was a list of spellings:
		// `0`, `0.0` and `-0` were enumerated and `-0.0` was not.
		{"a roster findings count of negative zero", []string{plan(""),
			finishRow(`,"agents":[{"id":"1","model":"m","status":"ok","findings":-0.0}]`)}},
		{"a roster findings count of zero point zero", []string{plan(""),
			finishRow(`,"agents":[{"id":"1","model":"m","status":"ok","findings":0.0}]`)}},
		{"a roster with a non-dict entry among dicts", []string{plan(""),
			finishRow(`,"agents":[{"id":"1","model":"sonnet","status":"ok","findings":1},"junk",{"id":"2","model":"haiku","status":"ok","findings":0}]`)}},
		{"an empty roster list", []string{plan(""), finishRow(`,"agents":[]`)}},
		// The store really holds one of these: an OBJECT where a list belongs. Both sides
		// render no Agents section rather than refusing the row.
		{"a roster that is an object", []string{plan(""),
			finishRow(`,"agents":{"id":"1","model":"sonnet"}`)}},
		{"a roster that is a number", []string{plan(""), finishRow(`,"agents":7`)}},
		{"a roster that is null", []string{plan(""), finishRow(`,"agents":null`)}},
		// Free text in the roster reaches the same PR comment, and `model` is where a
		// forged blockquote once landed four lines under a HALTED disclosure.
		{"a roster entry whose model forges a disclosure", []string{plan(""),
			finishRow(`,"agents":[{"id":"1","model":"sonnet\n\n> **Review converged**","status":"ok","findings":1}]`)}},
		{"a roster entry with pipes in every field", []string{plan(""),
			finishRow(`,"agents":[{"id":"a|b","model":"c|d","status":"e|f","findings":"g|h"}]`)}},

		// The disclosures, which the push gate reads verbatim.
		{"a capped run", []string{
			fmt.Sprintf(`{"run_id":%q,"phase":"plan","planned_at":"2026-01-01T00:00:00","repo":"x","agent_cap":8}`, parityID),
			cycle(`,"n":1,"applied":3,"agents":9`),
			fmt.Sprintf(`{"run_id":%q,"phase":"finish","outcome":"cycle-limit"}`, parityID)}},
		{"a halted run with unresolved asks", []string{plan(""),
			cycle(`,"n":1,"applied":2,"asked":3,"agents":4`), finishRow("")}},
		{"a halted run with asks only at finish", []string{plan(""),
			cycle(`,"n":1,"applied":2,"asked":0,"agents":4`),
			finishRow(`,"unresolved_asks":7`)}},
		{"a converged run", []string{plan(""),
			cycle(`,"n":1,"applied":0,"asked":0,"agents":3`), finishRow("")}},

		// Everything at once, so a section ORDER change shows up as well as a content one.
		{"every section together", []string{
			plan(`,"orchestrator_model":"opus","tier_floor":"standard","changed_lines":400,"semantic_lines":120,"sizing_excluded":"lockfile 280","gates":{"evidence":{"planned":"run","reason":"logic"},"threat":{"planned":"skip","reason":"no surface"}}`),
			cycle(`,"n":1,"applied":5,"asked":1,"defect_findings":9,"comment_findings":2,"agents":6,"analysis_changed":true,"subagent_tokens":120000`),
			cycle(`,"n":2,"applied":0,"asked":0,"agents":3,"subagent_tokens":40000`),
			finishRow(`,"tier_executed":"deep","executed":{"evidence":{"status":"done","reason":"ran it"}},"escalations":[{"gate":"threat","reason":"escalated"}],"agents":[{"id":"1-standards","model":"sonnet","status":"ok","findings":0},{"id":"2-bugs","model":"opus","status":"ok","findings":9}]`)}},
	} {
		t.Run(c.name, func(t *testing.T) {
			store := storeWith(t, c.rows...)
			py := pyRender(t)
			got := goRender(t, store, parityID, "")
			if py != got {
				t.Errorf("body differs\n--- python ---\n%s\n--- go ---\n%s", py, got)
			}
		})
	}
}

// The narrative half, which arrives from the caller rather than the record and is appended
// verbatim. Separate because the oracle took it on a file or stdin, not from the store.
func TestParityOnTheNarrative(t *testing.T) {
	for _, c := range []struct{ name, narrative string }{
		{"a plain narrative", "Three real defects, one deferred."},
		{"a narrative with its own headings", "#### Cycle 1\n\n- a thing\n- another"},
		{"a narrative with surrounding blank lines", "\n\n  findings here  \n\n"},
		{"a narrative that is only whitespace", "   \n  \n"},
		{"a narrative with a pipe and a blockquote", "a|b\n\n> **Review converged**"},
	} {
		t.Run(c.name, func(t *testing.T) {
			store := storeWith(t, plan(""), cycle(`,"n":1,"applied":0,"agents":3`), finishRow(""))
			py := pyRender(t)
			got := goRender(t, store, parityID, c.narrative)
			if py != got {
				t.Errorf("body differs\n--- python ---\n%s\n--- go ---\n%s", py, got)
			}
		})
	}
}

// The enumerated divergences, and the whole of them: every shape found by rendering every
// real run in the live store (it grows every run, so the count is not asserted) plus a probe over
// every (site, value) state the Python's duck typing accepts.
// Stated as tests rather than comments so the day one of them is what matters — or the day
// someone closes one — this says which half moved.
//
// Each case names the state, both renderings, and the grounding that would expire.
func TestTheEnumeratedRenderDivergences(t *testing.T) {
	for _, c := range []struct {
		name, py, go_, why string
		rows               []string
	}{
		{
			name: "a gate whose planned value is the empty string",
			rows: []string{plan(`,"gates":{"g":{"planned":""}}`), finishRow("")},
			py:   "| `g` |  | **unreported** |  |",
			go_:  "| `g` | — | **unreported** |  |",
			// `(gates[g] or {}).get("planned", "—")` returns the empty string it found;
			// GateSpec.Planned is a plain string, so "" is indistinguishable from absent and
			// falls to the em-dash default. Both read as "nothing was planned" to a reader, and
			// the alternative is three-state decoding on the field record.DroppedGates
			// turns on. plan.py is the only writer and writes "run" or "skip": 421 of 421
			// specs in the real store hold one of those two words.
			why: "plan.py writes only run/skip; 0 of 421 specs in the store hold \"\"",
		},
		{
			name: "a gate whose planned value is null",
			rows: []string{plan(`,"gates":{"g":{"planned":null}}`), finishRow("")},
			py:   "| `g` | None | **unreported** |  |",
			go_:  "| `g` | — | **unreported** |  |",
			why:  "same field, same writer; the Python prints None because .get found a key",
		},
		{
			name: "a roster findings value that is an object",
			rows: []string{plan(""),
				finishRow(`,"agents":[{"id":"1","model":"m","status":"ok","findings":{"a":1}}]`)},
			py:  "- `1` (m) — ok, {'a': 1} finding(s)",
			go_: `- ` + "`1`" + ` (m) — ok, {"a":1} finding(s)`,
			// str() of a container is a Python repr: single quotes, a space after the colon.
			// Reproducing it means a repr emulator, which is the trade record.GateStatus
			// names and refuses. The value renders either way — only its punctuation moves.
			why: "0 of 121 present findings values in the store is a container",
		},
		{
			name: "a roster findings value that is a list",
			rows: []string{plan(""),
				finishRow(`,"agents":[{"id":"1","model":"m","status":"ok","findings":[1,2]}]`)},
			py:  "- `1` (m) — ok, [1, 2] finding(s)",
			go_: "- `1` (m) — ok, [1,2] finding(s)",
			why: "same repr gap, same measurement",
		},
		{
			name: "a roster findings value written in exponent form",
			rows: []string{plan(""),
				finishRow(`,"agents":[{"id":"1","model":"m","status":"ok","findings":1e2}]`)},
			py:  "- `1` (m) — ok, 100.0 finding(s)",
			go_: "- `1` (m) — ok, 1e2 finding(s)",
			// json.loads normalises to a float and str() prints the normal form; the raw JSON
			// text keeps what was written. Only the spelling of the number moves.
			why: "all 121 present findings values in the store are plain ints",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			store := storeWith(t, c.rows...)
			py := pyRender(t)
			got := goRender(t, store, parityID, "")
			if !strings.Contains(py, c.py) {
				t.Errorf("the frozen golden no longer records the Python rendering %q — it was "+
					"edited, and this divergence needs restating:\n%s", c.py, py)
			}
			if !strings.Contains(got, c.go_) {
				t.Errorf("this side no longer renders %q — the divergence may be closed:\n%s", c.go_, got)
			}
			if py == got {
				t.Errorf("the two now AGREE; delete this case rather than keeping a stale divergence")
			}
			_ = c.why
		})
	}
}

// The disclosure's own divergence shows up here too, because the report quotes it. Owned by
// internal/push's TestTheDisclosureWordingDivergence — a null `applied` renders "?" there and
// "None" here — and asserted again at this level so a reader of the REPORT is not left to
// infer that the body inherits it.
func TestTheDisclosureDivergenceReachesTheBody(t *testing.T) {
	store := storeWith(t, plan(""), cycle(`,"n":1,"applied":null,"agents":2`), finishRow(""))
	py := pyRender(t)
	got := goRender(t, store, parityID, "")
	if !strings.Contains(py, "applied None fix(es)") {
		t.Errorf("the frozen golden's disclosure was edited:\n%s", py)
	}
	if !strings.Contains(got, "applied ? fix(es)") {
		t.Errorf("this side's disclosure has moved:\n%s", got)
	}
	// The CYCLE ROW agrees even though the disclosure does not: `{c.get('applied')}` prints
	// "None" and raw(nil) prints "None". Pinned so a fix to the disclosure is not mistaken
	// for a fix to the table, which was never broken.
	for _, side := range []string{py, got} {
		if !strings.Contains(side, "| 1 | None | 0 | 0 | 0 | 2 | clean |") {
			t.Errorf("the cycle row no longer agrees:\n%s", side)
		}
	}
}

// The other divergence class, which never reaches Render at all: a field of the wrong TYPE.
// The Python duck-types everything through str(), so `status: true` renders "True" and
// `applied: 2.0` renders "2.0"; the typed boundary in internal/record refuses the row and
// says which field, which is the louder half of the trade and the safe direction for the push
// gate — a record it cannot read blocks rather than deriving `unknown` from nothing.
//
// Asserted as a LIST because the policy is the claim: every one of these must fail at the
// load, not render something plausible. A shape that starts loading is a type that was
// widened, and this test is where that shows up.
func TestWrongTypedFieldsAreRefusedAtTheLoad(t *testing.T) {
	for _, c := range []struct {
		name string
		rows []string
	}{
		{"a numeric planned value", []string{plan(`,"gates":{"g":{"planned":3}}`), finishRow("")}},
		{"an executed entry that is not an object", []string{plan(""), finishRow(`,"executed":{"g":"done"}`)}},
		{"a numeric gate status", []string{plan(""), finishRow(`,"executed":{"g":{"status":3}}`)}},
		{"a boolean gate status", []string{plan(""), finishRow(`,"executed":{"g":{"status":true}}`)}},
		{"an escalations value that is not a list", []string{plan(""), finishRow(`,"escalations":{"gate":"a"}`)}},
		{"an escalation that is not an object", []string{plan(""), finishRow(`,"escalations":["a"]`)}},
		{"a numeric escalation gate", []string{plan(""), finishRow(`,"escalations":[{"gate":7}]`)}},
		{"a numeric escalation reason", []string{plan(""), finishRow(`,"escalations":[{"gate":"a","reason":7}]`)}},
		{"a non-boolean analysis_changed", []string{plan(""), cycle(`,"n":1,"applied":1,"analysis_changed":"yes"`), finishRow("")}},
		{"a decimal applied count", []string{plan(""), cycle(`,"n":1,"applied":2.0`), finishRow("")}},
		{"a numeric outcome", []string{plan(""), fmt.Sprintf(`{"run_id":%q,"phase":"finish","outcome":7}`, parityID)}},
		{"a numeric sizing_excluded", []string{plan(`,"changed_lines":5,"sizing_excluded":7`), finishRow("")}},
		{"a numeric orchestrator_model", []string{plan(`,"orchestrator_model":7`), finishRow("")}},
	} {
		t.Run(c.name, func(t *testing.T) {
			store := storeWith(t, c.rows...)
			// The oracle succeeded on this row — the capture kept a rendered body — or this is
			// not a divergence but a shape neither side accepts, and belongs in the test below.
			// Only the golden's existence is checked here; its content is not compared.
			pyRender(t)
			if refusedAtLoad(t, store) {
				return
			}
			t.Errorf("this row LOADED — the type was widened, so the report now renders " +
				"something where it used to refuse. Move the case into the agreement table " +
				"and compare it against its golden instead of leaving it here.")
		})
	}
}

// The third class, and the one worth knowing about: three record shapes made pr-report.py
// TRACEBACK, so the report was not rendered at all. `render()` guards `gates[g]` with
// isinstance for the `planned` lookup on line 133 and then calls `.get("reason")` on the same
// unguarded value three lines later, and `run.get("gates") or {}` admits a list — on which
// `.get` does not exist. A crash here drops the whole report, which is the failure the module's
// own docstring says it exists to prevent ("Fail on a MISSING report, never on a failed POST").
//
// Not fixed in the Python before it was retired, since fixing it would have changed the
// oracle this suite measured against; line 133 is in that deleted file. Recorded instead,
// because "both sides refuse" is not the same claim as "both sides agree" — this side refuses
// with the field named, where the oracle refused with a stack trace.
func TestThreeShapesCrashTheOracleAndAreRefusedHere(t *testing.T) {
	for _, c := range []struct {
		name, pyError string
		rows          []string
	}{
		{"a gates entry that is not an object", "'str' object has no attribute 'get'",
			[]string{plan(`,"gates":{"g":"run"}`), finishRow("")}},
		{"a gates map that is not an object", "'list' object has no attribute 'get'",
			[]string{plan(`,"gates":[1,2]`), finishRow("")}},
		{"an executed map that is not an object", "'list' object has no attribute 'get'",
			[]string{plan(""), finishRow(`,"executed":[1]`)}},
	} {
		t.Run(c.name, func(t *testing.T) {
			store := storeWith(t, c.rows...)
			out, stderr := rawPyRender(t)
			if stderr == "" {
				t.Errorf("the frozen golden no longer records the oracle crashing on this "+
					"shape — it was edited; it records stdout:\n%s", out)
			} else if !strings.Contains(stderr, c.pyError) {
				t.Errorf("the frozen golden records a different crash than this case "+
					"names — it was edited:\n%s", stderr)
			}
			if !refusedAtLoad(t, store) {
				t.Errorf("this row LOADED while the oracle crashed on it — the comparison " +
					"is no longer a refusal on both sides, so the case needs restating")
			}
		})
	}
}

// refusedAtLoad reports whether the typed boundary rejects the fixture, at either the line
// decode or the folded read. Both count: the claim is that nothing plausible gets rendered.
func refusedAtLoad(t *testing.T, store string) bool {
	t.Helper()
	runs, err := record.Load(store, 0)
	if err != nil {
		return true
	}
	r := runs[parityID]
	if r == nil {
		return true
	}
	_, err = r.Convergence()
	return err != nil
}

// rawPyRender returns what the oracle printed on stdout and stderr, for the cases whose point is
// that it failed. Only the final line of a traceback is kept: it names the error, and the frames
// above it named paths on the machine that captured it.
func rawPyRender(t *testing.T) (string, string) {
	t.Helper()
	return golden.Read(t, "stdout"), golden.Read(t, "stderr")
}

// The one DELIBERATE behavioural divergence in this slice, and the reason it exists.
//
// pr-report.py's post path reads `rc, num, _ = sh("gh", "pr", "view", ...)` and branches on
// `if rc != 0 or not num:`, printing "no PR yet — deferred to <path>". That is one assertion
// standing in for three causes — no PR, no gh, a gh that ran and failed — and the stream that
// says which was discarded into `_`. The advice it gives ("Step 0c flushes it", i.e. once you
// make a PR) cannot fix the other two, and on this machine gh has already failed with a
// keychain TLS error while `git push` worked, so the unfixable branch is the reachable one.
//
// Both sides keep the body on every path; only the operator-facing note differs. Approved as
// a divergence rather than reproduced, because reproducing it means shipping the defect the
// slice was for.
func TestThePostDiagnosisDivergence(t *testing.T) {
	store := storeWith(t, plan(""), cycle(`,"n":1,"applied":0,"agents":3`), finishRow(""))
	// A gh that RUNS and FAILS: the case both sides must handle and only one explains.
	shimGh(t, `echo "tls: failed to verify certificate: x509: OSStatus -26276" >&2; exit 1`)
	dir := gitRepo(t)

	pyErr := golden.Read(t, "stderr")
	if !strings.Contains(pyErr, "no PR yet") {
		t.Errorf("the frozen oracle output no longer asserts there is no PR:\n%s", pyErr)
	}
	// What the oracle kept at the deferral path, captured before this side ever wrote there.
	pyKept := golden.Read(t, "pending-report.md")

	var diag strings.Builder
	body := goRender(t, store, parityID, "")
	path, err := Post(PostParams{Repo: dir, RunID: parityID, Branch: "feat/x",
		Convergence: "converged", Body: body, Diag: &diag})
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	if strings.Contains(diag.String(), "no PR") {
		t.Errorf("this side asserts there is no PR, which it never established:\n%s", diag.String())
	}
	if !strings.Contains(diag.String(), "x509") {
		t.Errorf("this side does not carry gh's own reason, which is the point of the "+
			"divergence:\n%s", diag.String())
	}
	// The half that must NOT diverge: the body survives on both sides, because the push gate
	// reads this file and a dropped report leaves the push refused on impossible advice.
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the report was not kept: %v", err)
	}
	if !strings.HasPrefix(string(b), body) {
		t.Errorf("the kept body is not the rendered one:\n%s", b)
	}
	// And the two kept the same BODY — the half push-check reads, since the marker and the
	// fingerprint both live there. The trailing label-owed note differs by one thing, named
	// below: it tells Step 0c which command to run. The Python named itself, and the first
	// port named `looper pr-report --label`, which applies nothing without -post; the command
	// that labels a PR is gh's.
	const note = "\n<!-- review-loop: label"
	pyBody, _, _ := strings.Cut(pyKept, note)
	goBody, goNote, _ := strings.Cut(string(b), note)
	if pyBody != goBody {
		t.Errorf("the deferred bodies differ\n--- python ---\n%s\n--- go ---\n%s", pyBody, goBody)
	}
	if !strings.Contains(goNote, "`gh pr edit <n> --add-label review:converged`") ||
		!strings.Contains(goNote, "run "+parityID) {
		t.Errorf("the label-owed note does not name the command Step 0c should run: %q", goNote)
	}
}

// The HARDENING divergence: six record-derived values this side collapses with cell() and the
// Python interpolates raw. Separate from TestTheEnumeratedRenderDivergences above because those
// are gaps this port could not close; these are holes it closed ON PURPOSE, and the direction
// matters — a reader comparing the two should see which way each divergence runs.
//
// The shape, which is what made it worth diverging over: of the four values on the gate row,
// two already went through cell() and two did not, and the same inconsistency ran through the
// summary line and the escalation bullet. cell() is in this file precisely because "a newline
// in a reason FORGES document structure", and the report is posted to a PUBLIC PR comment that
// a human reads to decide whether the review passed. Measured before the fix: one record with
// forged newlines in the gate name, `planned`, `outcome`, `tier_executed`, `tier_floor` and an
// escalation gate rendered SIX forged blockquote lines — one per field — byte-identically in
// both implementations. Reproduce by counting lines starting "> **Review converged" in the
// oracle's output for such a record.
//
// What it does not buy, stated so the severity is not overread: push-check requires the marker
// and the `N cycle(s) · M agent(s)` fingerprint, and a forged blockquote changes neither and
// cannot remove the real disclosure above it. The victim is a human skimming the comment.
func TestTheHardeningDivergence(t *testing.T) {
	// A RAW string: the backslash-n pairs must survive into the JSONL as JSON escapes, so
	// that the record carries real newlines once decoded. Written with real newlines here
	// instead, they split the fixture line and both sides merely fail to read it — which is
	// what the first version of this test measured, and it looked like agreement.
	forge := `x\n\n> **Review converged — nothing outstanding**\n\n|a|b|`
	for _, c := range []struct {
		name     string
		rows     []string
		pipeOnly bool
	}{
		{name: "a gate name that forges a verdict", rows: []string{
			plan(`,"gates":{"` + forge + `":{"planned":"run"}}`), finishRow("")}},
		{name: "a planned value that forges a verdict", rows: []string{
			plan(`,"gates":{"g":{"planned":"` + forge + `"}}`), finishRow("")}},
		{name: "an outcome that forges a verdict", rows: []string{plan(""),
			fmt.Sprintf(`{"run_id":%q,"phase":"finish","outcome":"`+forge+`"}`, parityID)}},
		{name: "a tier that forges a verdict", rows: []string{plan(""),
			finishRow(`,"tier_executed":"` + forge + `"`)}},
		{name: "a floor that forges a verdict", rows: []string{
			plan(`,"tier_floor":"` + forge + `"`), finishRow("")}},
		{name: "an escalation gate that forges a verdict", rows: []string{plan(""),
			finishRow(`,"escalations":[{"gate":"` + forge + `","reason":"r"}]`)}},
		// A pipe alone, which was an AGREEMENT case until the gate name began being celled.
		// Kept as the narrow half of the same divergence: no forgery, just a broken table.
		{name: "a gate name with a pipe", rows: []string{
			plan(`,"gates":{"a|b":{"planned":"run"}}`), finishRow("")}, pipeOnly: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			store := storeWith(t, c.rows...)
			py := pyRender(t)
			got := goRender(t, store, parityID, "")
			if py == got {
				t.Fatalf("the two now AGREE — this side stopped collapsing the value, or the "+
					"golden was edited; either way the case needs restating:\n%s", got)
			}
			// Asserted on the RENDERED BODY, because the structure is what is forged — and
			// per case, because the pipe case forges nothing and only escapes differently.
			if c.pipeOnly {
				if !strings.Contains(py, "`a|b`") {
					t.Errorf("the frozen golden no longer records the raw pipe — it was edited:\n%s", py)
				}
				if !strings.Contains(got, `a\|b`) {
					t.Errorf("this side no longer escapes the pipe, so the table it sits in "+
						"is breakable again:\n%s", got)
				}
				return
			}
			if !strings.Contains(py, "\n> **Review converged") {
				t.Errorf("the frozen golden no longer records the forged blockquote — it "+
					"was edited:\n%s", py)
			}
			if strings.Contains(got, "\n> **Review converged") {
				t.Errorf("this side rendered a forged blockquote — the hardening is gone:\n%s", got)
			}
			// The collapse is what removes the structure, so the oracle's body must have more
			// lines. A substring check alone would pass on a body that merely reworded.
			if strings.Count(py, "\n") <= strings.Count(got, "\n") {
				t.Errorf("the oracle's body is not longer than this one, so the forged "+
					"structure was not collapsed:\n--- python ---\n%s\n--- go ---\n%s", py, got)
			}
		})
	}
}
