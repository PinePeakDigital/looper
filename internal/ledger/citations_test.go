package ledger

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/pinepeakdigital/looper/internal/golden"
)

// pins.py read citations a line at a time, and that was wrong three ways on real threat models
// (looper's own among them): a claim whose pin sat on a wrapped continuation line counted as
// uncited, `claims` counted lines holding a pin rather than claims, and only the first pin on a
// line was read, so a second one was never checked. Its citation pattern also took one line or
// one range, so [a/b.go:206,225-231 @ sha] was no citation at all — the claim looked pinned and
// nothing watched it — and every other near-miss was skipped the same silent way.
//
// looper reads a claim as a block, reads every pin on a line, accepts a comma-separated list,
// and reports pin-shaped text that does not parse in broken_pins. These parity scenarios changed
// answer with it; each keeps the Python's frozen answer as `transcript` beside looper's, as
// `looper`, written by hand from looper's output and reviewed as such (internal/golden).
var citationDivergences = map[string]string{
	"tm the canonical model":                              "claims counts the four OBSERVED claims, not the three pinned lines",
	"tm the canonical model from a subdirectory":          "as the canonical model",
	"tm the canonical model from a linked worktree":       "as the canonical model",
	"tm the canonical model from a worktree subdirectory": "as the canonical model",
	"tm arguments are ignored":                            "as the canonical model",
	"tm malformed ranges are not citations":               "they are still not citations, and are now broken pins",
	"tm only the first pin on a line counts":              "every pin on a line counts",
	"tm sha lengths and case":                             "a short, long or upper-case sha is a broken pin, not nothing",
	"tm spacing around the at sign":                       "a space before the closing bracket is a broken pin, not nothing",
	"tm a path may hold a bracket but no colon":           "a path with a colon is a broken pin, not nothing",
	"tm Unicode digits and spaces":                        "a space in the path is a broken pin, not nothing",
	"tm the marker needs its colon":                       "claims counts the OBSERVED: lines, pinned or not",
	"tm a hard-wrapped claim":                             "a pin on a continuation line pins its claim",
	"tm CRLF and other line breaks":                       "claims counts claims; the unindented pin pins nothing",
	"df several pins in one entry":                        "every pin on a line counts",
}

func TestCitationsAreReadPerClaim(t *testing.T) {
	seen := map[string]bool{}
	for _, sc := range scenarios {
		if _, ok := citationDivergences[sc.name]; !ok {
			continue
		}
		seen[sc.name] = true
		t.Run(sc.name, func(t *testing.T) {
			py := golden.Read(t, "transcript") // before runGo's Chdir: the golden path is relative
			want := golden.Read(t, "looper")
			if py == want {
				t.Fatalf("the frozen Python answer and looper's are the same, so this is not a divergence:\n%s", py)
			}
			if got := runGo(t, sc, argsFor(sc)...); got != want {
				t.Errorf("--- python (frozen) ---\n%s\n--- looper (decided) ---\n%s\n--- looper (now) ---\n%s",
					py, want, got)
			}
		})
	}
	for name := range citationDivergences {
		if !seen[name] {
			t.Errorf("%q is listed as a divergence but is no scenario", name)
		}
	}
}

// Claims reads a claim as its marker line plus its continuation lines, and counts claims.
func TestClaimsAreBlocks(t *testing.T) {
	for _, c := range []struct {
		name            string
		text            string
		claims, uncited int
	}{
		{"a pin on a continuation line pins the claim",
			"- OBSERVED: a\n  wrapped. [x.go:1 @ abc1234]\n", 1, 0},
		{"a pin several lines down pins the claim",
			"- OBSERVED: a\n  b\n  c\n  [x.go:1 @ abc1234] [y.go @ abc1234]\n", 1, 0},
		{"a claim per marker line, not per pin line",
			"- OBSERVED: a [x.go @ abc1234]\n  [y.go @ abc1234]\n- OBSERVED: b\n  [z.go @ abc1234]\n- OBSERVED: c\n", 3, 1},
		{"a pin past a blank line is not the claim's",
			"- OBSERVED: a\n\n  [x.go:1 @ abc1234]\n", 1, 1},
		{"a pin past a whitespace-only line is not the claim's",
			"- OBSERVED: a\n   \n  [x.go:1 @ abc1234]\n", 1, 1},
		{"a pin on the next bullet is not the claim's",
			"- OBSERVED: a\n- INFERRED: b [x.go @ abc1234]\n", 1, 1},
		{"an indented bullet ends the claim",
			"- OBSERVED: a\n  * note [x.go @ abc1234]\n", 1, 1},
		{"a numbered item ends the claim",
			"- OBSERVED: a\n  1. note [x.go @ abc1234]\n", 1, 1},
		{"a dash with no space after it is no bullet",
			"- OBSERVED: a\n  -x [x.go @ abc1234]\n", 1, 0},
		{"a number with no dot after it is no bullet",
			"- OBSERVED: a\n  2026 [x.go @ abc1234]\n", 1, 0},
		{"an unindented line ends the claim",
			"- OBSERVED: a\n[x.go @ abc1234]\n", 1, 1},
		{"a heading ends the claim",
			"- OBSERVED: a\n## next [x.go @ abc1234]\n", 1, 1},
		{"a claim whose only pin is malformed is uncited",
			"- OBSERVED: a\n  [x.go:1 @ ABC1234]\n", 1, 1},
		{"the last claim is closed at the end of the text",
			"- OBSERVED: a [x.go @ abc1234]\n- OBSERVED: b", 2, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			if claims, uncited := Claims(c.text, "OBSERVED:"); claims != c.claims || uncited != c.uncited {
				t.Errorf("claims %d, uncited %d; want %d, %d", claims, uncited, c.claims, c.uncited)
			}
		})
	}
}

// Every pin on a line is read, a comma-separated list is one pin, and pin-shaped text that is
// not a citation is malformed.
func TestCitationsReadsEveryPin(t *testing.T) {
	for _, c := range []struct {
		name      string
		line      string
		pins      []string // path:lines of each pin
		malformed []string
	}{
		{"two pins on a line", "a [x.go:1 @ abc1234] and [y.go @ abc1235]", []string{"x.go:1", "y.go:"}, nil},
		{"a comma-separated list", "[a/b.go:206,225-231 @ abc1234]", []string{"a/b.go:206,225-231"}, nil},
		{"a list of two lines", "[a/b.go:1,2 @ abc1234]", []string{"a/b.go:1,2"}, nil},
		{"a list with a space is malformed", "[a/b.go:206, 225 @ abc1234]", nil, []string{"[a/b.go:206, 225 @ abc1234]"}},
		{"a trailing comma is malformed", "[a/b.go:206, @ abc1234]", nil, []string{"[a/b.go:206, @ abc1234]"}},
		{"an open range is malformed", "[x.go:1- @ abc1234]", nil, []string{"[x.go:1- @ abc1234]"}},
		{"an upper-case sha is malformed", "[x.go @ ABC1234]", nil, []string{"[x.go @ ABC1234]"}},
		{"a six-character sha is malformed", "[x.go @ abcdef]", nil, []string{"[x.go @ abcdef]"}},
		{"a space before the bracket is malformed", "[x.go:1 @ abc1234 ]", nil, []string{"[x.go:1 @ abc1234 ]"}},
		{"a malformed pin beside a good one", "[x.go @ abc1234] [y.go:z @ abc1234]", []string{"x.go:"}, []string{"[y.go:z @ abc1234]"}},
		{"a placeholder in prose is not a pin", "cited as `[file:line @ sha]`", nil, nil},
		{"an address is not a pin", "mail [me@example.com]", nil, nil},
		{"a price is not a pin", "costs [price @ 100]", nil, nil},
		{"a short hex word is not a pin", "ask [user@fade]", nil, nil},
		{"six hex digits without a path is a pin shape", "[see @ ABCDEF0]", nil, []string{"[see @ ABCDEF0]"}},
		{"six hex digits are a pin shape", "[see @ ABCDEF]", nil, []string{"[see @ ABCDEF]"}},
		{"five hex digits are prose", "[see @ abcde]", nil, nil},
		{"a colon on the left is still prose", "[note: see @ 100]", nil, nil},
		{"a short sha on a path is malformed", "[x.go:1 @ abc12] [a/b.go @ abc]", nil, []string{"[x.go:1 @ abc12]", "[a/b.go @ abc]"}},
		{"a bracket before a pin is the pin's", "[[x.go @ abc1234]", []string{"[x.go:"}, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			pins, bad := Citations("- OBSERVED: x\n  " + c.line + "\n")
			var got, gotBad []string
			for _, p := range pins {
				l := ""
				if p.Lines != nil {
					l = *p.Lines
				}
				if p.Line != 2 {
					t.Errorf("pin %s on line %d, want 2", p.Path, p.Line)
				}
				got = append(got, p.Path+":"+l)
			}
			for _, m := range bad {
				if m.Line != 2 {
					t.Errorf("malformed %s on line %d, want 2", m.Text, m.Line)
				}
				gotBad = append(gotBad, m.Text)
			}
			if strings.Join(got, "|") != strings.Join(c.pins, "|") ||
				strings.Join(gotBad, "|") != strings.Join(c.malformed, "|") {
				t.Errorf("pins %q, malformed %q; want %q, %q", got, gotBad, c.pins, c.malformed)
			}
		})
	}
}

// report runs the command on a ledger in the fixture repo and decodes its JSON.
func report(t *testing.T, cmd, ledger string) map[string]any {
	t.Helper()
	code, out, errOut := divergence{cmd: cmd, ledger: ledger}.runGo(t)
	if code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errOut)
	}
	var r map[string]any
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	return r
}

// briefs names each entry of a report list as line:path:lines:sha, or line:malformed-text.
func briefs(v any) []string {
	var out []string
	for _, e := range v.([]any) {
		m := e.(map[string]any)
		line := fmt.Sprint(m["line"])
		if bad, ok := m["malformed"]; ok {
			out = append(out, line+":"+bad.(string))
			continue
		}
		lines := ""
		if l, ok := m["lines"].(string); ok {
			lines = l
		}
		out = append(out, line+":"+m["path"].(string)+":"+lines+":"+m["sha"].(string)[:7])
	}
	return out
}

// short is fixture commit n's seven-character sha.
func short(n int) string {
	return strings.NewReplacer(history.pairs...).Replace(fmt.Sprintf("{c%ds}", n))
}

func jsonText(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// What the fix is for, end to end, in a real repo: a wrapped claim is cited, a comma-listed pin
// and a second pin on a line both go stale when their file moves, and a malformed pin is broken
// in line order with the unresolvable shas.
func TestThreatModelWatchesEveryPin(t *testing.T) {
	r := report(t, "threat-model", "# Threat model\n"+
		"- OBSERVED: a wrapped claim,\n  pinned twice on its second line. [src/app.ts:1 @ {c1s}] [src/types.ts:1,3-4 @ {c1s}]\n"+
		"- OBSERVED: a malformed pin first. [src/app.ts @ ABCDEF0]\n"+
		"  then an unresolvable one. [src/auth.ts:1 @ 0000000]\n"+
		"- OBSERVED: uncited.\n"+
		"- INFERRED: watched though no claim. [src/db.ts @ {c1s}] [src/auth.ts @ {c1s}]\n")
	if r["claims"] != 3.0 || r["uncited"] != 1.0 {
		t.Errorf("claims %v, uncited %v; want 3, 1", r["claims"], r["uncited"])
	}
	stale := strings.Join(briefs(r["stale"]), " ")
	if want := "3:src/types.ts:1,3-4:" + short(1) + " 7:src/auth.ts::" + short(1); stale != want {
		t.Errorf("stale %q, want %q", stale, want)
	}
	broken := strings.Join(briefs(r["broken_pins"]), " ")
	if want := "4:[src/app.ts @ ABCDEF0] 5:src/auth.ts:1:0000000"; broken != want {
		t.Errorf("broken_pins %q, want %q", broken, want)
	}
}

// The deferred list shares the citation reading: a comma-listed pin goes stale, a malformed pin
// is broken at the line it is on, and an entry whose only pin is malformed is unpinned.
func TestDeferredWatchesEveryPin(t *testing.T) {
	r := report(t, "deferred", "# Deferred\n\n"+
		"- DEFERRED x: a.\n  Grounding: b. [src/app.ts:1 @ {c1s}] [src/types.ts:1,3 @ {c1s}]\n  Guard: none.\n"+
		"- DEFERRED y: c.\n  Grounding: d. [src/app.ts:1- @ {c1s}]\n  Guard: none.\n")
	if got := strings.Join(briefs(r["stale"]), " "); got != "4:src/types.ts:1,3:"+short(1) {
		t.Errorf("stale %q", got)
	}
	if got := strings.Join(briefs(r["broken_pins"]), " "); got != "7:[src/app.ts:1- @ "+short(1)+"]" {
		t.Errorf("broken_pins %q", got)
	}
	if got := jsonText(r["unpinned"]); !strings.Contains(got, `"line":6`) || strings.Contains(got, `"line":3`) {
		t.Errorf("unpinned %s, want entry 6 only", got)
	}
}
