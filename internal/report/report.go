// Package report renders the Step 14 review disclosure from the run record.
//
// A port of pr-report.py, and the other half of the gate in internal/push: this writes the
// artifact that push-check verifies. The two must agree on the fingerprint — the marker, the
// `## review-loop` heading, and the `N cycle(s) · M agent(s)` line — or every push is refused
// on advice that cannot succeed. push.Fingerprint is the reader and Render the writer;
// TestThePushGateFindsWhatThisPackageWrites pins them by RUNNING the reader against what this
// renders, rather than by comparing the two format strings, which could agree while the lines
// they assemble do not.
//
// The split the Python states and this keeps: FACTS come from the record and are not retyped
// by the orchestrator, so they cannot drift from what was recorded; NARRATIVE arrives from the
// caller and is appended verbatim, because what the agents found is not derivable and must not
// be faked.
package report

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"unicode"

	"github.com/pinepeakdigital/looper/internal/record"
)

// Marker is the per-run needle push-check looks for. Keyed on the run id so one repo's
// concurrent worktree sessions cannot satisfy each other's gate.
const Marker = "<!-- review-loop:run=%s -->"

// PendingName is the deferral path, relative to --git-common-dir. Per RUN, not per repo: that
// directory is shared by every worktree of a repo, so a single fixed name let two concurrent
// sessions overwrite each other's report — after which one push was refused and Step 0c posted
// the survivor to the wrong branch's PR.
const PendingName = "info/review-loop-pending-report.%s.md"

// Label is the at-a-glance signal, so a reader need not open a comment to learn whether the
// review finished. The vocabulary is runlog's derived one, exactly.
type Label struct {
	Name, Colour, Description string
}

// Labels is keyed by convergence. record.Convergence answers only these four words, which
// TestConvergenceAnswersOnlyTheFourWords asserts over a generated matrix — so a missing key
// here is unreachable rather than defended against.
var Labels = map[string]Label{
	"converged": {"review:converged", "0e8a16", "review-loop ran out of findings"},
	"capped":    {"review:capped", "fbca04", "review-loop hit its agent budget with findings outstanding"},
	"halted":    {"review:halted", "d93f0b", "review-loop stopped with findings outstanding"},
	"unknown":   {"review:unknown", "b60205", "review-loop recorded no cycles — completeness unknown"},
}

// cell flattens one free-text value for a markdown table cell or bullet.
//
// Every reason in this report was written by an LLM into a record this package does not own,
// and the report is posted as a PR comment — so a newline in a reason FORGES document
// structure: a reason ending "\n\n> **Review converged**" renders as its own blockquote and
// contradicts the disclosure three lines above it. strings.Fields splits on every Unicode
// space, which is what Python's bare `.split()` does, so this collapses the same set.
func cell(s string) string {
	return strings.ReplaceAll(strings.Join(strings.Fields(s), " "), "|", `\|`)
}

// cellJSON is cell() over a raw JSON scalar, reproducing the Python's `str(text or "")` —
// which is FALSINESS, not emptiness. `cell(0)` is "" there, because `0 or ""` is "", and the
// store's roster really does carry `findings: 0`: it renders as "ok,  finding(s)" with the
// value simply gone. Measured against all 41 real runs; reading `0` as "0" was the first
// divergence this port produced, and it is the kind a reader would never notice because the
// output still looks plausible.
//
// The falsy JSON scalars are 0, 0.0, false, null and "". Everything else renders from its JSON
// text, which str() matches exactly for an int and for a plain decimal float. Two gaps, both
// enumerated in TestTheStrAlikeDivergences rather than left implied: a container renders as
// JSON here and as a Python repr there (`{"a":1}` vs `{'a': 1}`), and an exponent-form number
// renders as written here and normalised there (`1e2` vs `100.0`). Neither shape occurs in any
// of the 137 roster entries in the store, and closing them means a str() emulator — the same
// trade record.GateStatus names and refuses.
func cellJSON(b []byte) string {
	t := strings.TrimSpace(string(b))
	switch t {
	case "0", "0.0", "-0", "false", "null", `""`:
		return ""
	case "true":
		return "True" // str(True), not JSON's spelling
	}
	if len(t) >= 2 && t[0] == '"' && t[len(t)-1] == '"' {
		var str string
		if json.Unmarshal(b, &str) == nil {
			return cell(str)
		}
	}
	return cell(t)
}

// orZero renders a *int the way the Python's `or 0` does: absent and null are both 0.
func orZero(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

// raw renders a *int the way a bare Python f-string interpolation does — `{c.get('n')}` with
// no `or` — which prints the literal "None" for a missing key. Deliberately not orZero: an
// absent cycle number is a defect in the record and printing 0 would hide it behind a
// plausible value.
func raw(p *int) string {
	if p == nil {
		return "None"
	}
	return fmt.Sprintf("%d", *p)
}

// Render builds the report body. conv is the DERIVED convergence, passed in rather than
// recomputed so the caller's label, the disclosure and this body cannot disagree.
func Render(r *record.Run, runID, conv, narrative string) (string, error) {
	disclose, err := r.Disclosure()
	if err != nil {
		return "", err
	}
	var out []string
	add := func(lines ...string) { out = append(out, lines...) }

	add(fmt.Sprintf(Marker, runID), "", "## review-loop", "")

	// Disclosure first and unabbreviated. A capped run is allowed to push BECAUSE it says
	// so, and burying that under a table would make the push silent in practice.
	if disclose != "" {
		add("> **"+disclose+"**", "")
	} else {
		add("Review **converged** — the final cycle found nothing left to apply.", "")
	}

	agents := r.AgentsSpent()
	tokens := 0
	for _, c := range r.Cycles {
		tokens += orZero(c.SubagentTokens)
	}

	outcome, tier := "unrecorded", "unrecorded"
	if r.Finish != nil {
		if r.Finish.Outcome != "" {
			outcome = r.Finish.Outcome
		}
		if r.Finish.TierExecuted != "" {
			tier = r.Finish.TierExecuted
		}
	}
	floor, model := "unrecorded", "?"
	if r.Plan != nil {
		if r.Plan.TierFloor != "" {
			floor = r.Plan.TierFloor
		}
		if m := cell(r.Plan.OrchestratorModel); m != "" {
			model = m
		}
	}
	if conv == "" {
		conv = "unknown"
	}
	add(fmt.Sprintf("- **Outcome** `%s` · **convergence** `%s` · **tier** `%s` (floor `%s`)",
		outcome, conv, tier, floor))
	runLine := fmt.Sprintf("- **Run** `%s` · orchestrator `%s` · %d cycle(s) · %d agent(s)",
		runID, model, len(r.Cycles), agents)
	if tokens != 0 {
		runLine += fmt.Sprintf(" · ~%s subagent tokens", commas(tokens))
	}
	add(runLine, "")

	if r.Plan != nil && r.Plan.ChangedLines != nil {
		line := fmt.Sprintf("- **Size** %d raw line(s)", *r.Plan.ChangedLines)
		if sem := r.Plan.SemanticLines; sem != nil && *sem != *r.Plan.ChangedLines {
			line += fmt.Sprintf(", %d of review surface", *sem)
		}
		if x := r.Plan.SizingExcluded; x != "" {
			line += " — excluded: " + cell(x)
		}
		add(line, "")
	}

	if len(r.Cycles) > 0 {
		add("### Cycles", "",
			"| # | applied | asked | defects | comment-accuracy | agents | analysis |",
			"|---|---|---|---|---|---|---|")
		for _, c := range r.Cycles {
			analysis := "clean"
			if c.AnalysisChanged != nil && *c.AnalysisChanged {
				analysis = "changed files"
			}
			add(fmt.Sprintf("| %s | %s | %d | %d | %d | %d | %s |",
				raw(c.N), raw(c.Applied), orZero(c.Asked), orZero(c.DefectFindings),
				orZero(c.CommentFindings), orZero(c.Agents), analysis))
		}
		add("")
	}

	if names := gateNames(r); len(names) > 0 {
		add("### Gates", "", "| gate | planned | status | why |", "|---|---|---|---|")
		for _, g := range names {
			planned, why := "—", ""
			if r.Plan != nil {
				if spec, ok := r.Plan.Gates[g]; ok {
					if spec.Planned != "" {
						planned = spec.Planned
					}
					why = spec.Reason
				}
			}
			// "**unreported**" is bold on purpose: a gate the plan said to RUN and that
			// reported nothing is the single thing this table exists to surface. It is the
			// default for BOTH "no executed entry" and "an entry with no status key" —
			// `e.get("status", "**unreported**")` — where a null or empty status renders as
			// nothing instead. record.GateStatus exists to keep those apart.
			status := "**unreported**"
			if r.Finish != nil {
				if res, ok := r.Finish.Executed[g]; ok {
					if res.Status.Present {
						status = res.Status.Value
					}
					if res.Reason != "" {
						why = res.Reason
					}
				}
			}
			add(fmt.Sprintf("| `%s` | %s | %s | %s |", g, planned, cell(status), cell(why)))
		}
		add("")
	}

	if r.Finish != nil && len(r.Finish.Escalations) > 0 {
		add("### Escalated above the plan", "")
		for _, e := range r.Finish.Escalations {
			gate := "None" // interpolated raw by the Python; absent and null both print this
			if e.Gate != nil {
				gate = *e.Gate
			}
			add(fmt.Sprintf("- `%s` — %s", gate, cell(e.Reason)))
		}
		add("")
	}

	if roster := decodeRoster(r); len(roster) > 0 {
		add("### Agents", "")
		for _, a := range roster {
			// All four fields are orchestrator free text from `finish --agents <json>`, so
			// all four are collapsed — not just status. A newline in `model` rendered a real
			// blockquote reading "Review converged" four lines under a HALTED disclosure.
			model := cellJSON(a.Model)
			if model == "" {
				model = "?"
			}
			add(fmt.Sprintf("- `%s` (%s) — %s, %s finding(s)",
				cellJSON(a.ID), model, cellJSON(a.Status), a.findings()))
		}
		add("")
	}

	if narrative != "" {
		add("### Findings", "", strings.TrimSpace(narrative), "")
	}
	// rstrip(), not rstrip("\n"): the Python strips every trailing whitespace character, and a
	// last line ending in a space would otherwise keep it here and lose it there.
	return strings.TrimRightFunc(strings.Join(out, "\n"), unicode.IsSpace) + "\n", nil
}

// gateNames is the union of planned and executed gate names, sorted — so a gate that reported
// without being planned, and one planned that never reported, both appear.
func gateNames(r *record.Run) []string {
	seen := map[string]bool{}
	if r.Plan != nil {
		for g := range r.Plan.Gates {
			seen[g] = true
		}
	}
	if r.Finish != nil {
		for g := range r.Finish.Executed {
			seen[g] = true
		}
	}
	names := make([]string, 0, len(seen))
	for g := range seen {
		names = append(names, g)
	}
	sort.Strings(names)
	return names
}

// agentEntry is the roster shape, decoded HERE rather than in internal/record on purpose.
// `--agents` is a bare json.loads with no shape validation, and the store holds 29 lists and
// one OBJECT for it, so typing it at the record boundary would reject a real row. The Python
// guards with `isinstance(roster, list)` and `isinstance(a, dict)` and renders nothing when
// either fails; decodeRoster reproduces exactly that tolerance, and keeps it out of the typed
// boundary the push gate depends on.
// All four fields are raw. Typing the first three cost the WHOLE ENTRY when any one of them
// held a non-string: `{"id": 5, ...}` failed the decode and the agent vanished from the
// roster, where the Python renders "5". The roster is the evidence of what actually ran, so
// dropping a row is the one failure mode here that reads as fewer agents having been spawned.
type agentEntry struct {
	ID       json.RawMessage `json:"id"`
	Model    json.RawMessage `json:"model"`
	Status   json.RawMessage `json:"status"`
	Findings json.RawMessage `json:"findings"`
}

// findings renders whatever the key held. `cell(a.get('findings', '?'))` means an ABSENT key
// prints "?" — and a present FALSY one prints nothing at all, which cellJSON reproduces. All
// 121 present entries in the store hold an int; 16 entries omit the key.
//
// The other three fields take no default, so `cell(a.get('id'))` is cell(None) for an absent
// key: the empty string, which is cellJSON's answer for a zero-length RawMessage too.
func (a agentEntry) findings() string {
	if len(a.Findings) == 0 {
		return "?"
	}
	return cellJSON(a.Findings)
}

func decodeRoster(r *record.Run) []agentEntry {
	if r.Finish == nil || len(r.Finish.Agents) == 0 {
		return nil
	}
	var raw []json.RawMessage
	if json.Unmarshal(r.Finish.Agents, &raw) != nil {
		return nil // not a list — the object case, which the Python skips too
	}
	var out []agentEntry
	for _, e := range raw {
		var a agentEntry
		if json.Unmarshal(e, &a) != nil {
			continue // not a dict — `isinstance(a, dict)` skips it there too
		}
		out = append(out, a)
	}
	return out
}

// commas renders a thousands-separated integer, which is Python's `{n:,}`.
func commas(n int) string {
	s := fmt.Sprintf("%d", n)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	var parts []string
	for len(s) > 3 {
		parts = append([]string{s[len(s)-3:]}, parts...)
		s = s[:len(s)-3]
	}
	parts = append([]string{s}, parts...)
	out := strings.Join(parts, ",")
	if neg {
		out = "-" + out
	}
	return out
}
