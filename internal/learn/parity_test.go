package learn

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pinepeakdigital/looper/internal/golden"
)

// The parity gate: each scenario ran through learn.py and runs through Command on the same
// starting file and must leave the same exit status, stdout, stderr and file. The Python's
// side is frozen under testdata/oracle; LEARN_ORACLE=<path to learn.py> recaptures it while
// that file still exists. Two normalisations: the date an entry is stamped with is <today>,
// and the temp file's path is {f}.
//
// A usage error, or help, is compared on exit status, file and its final stderr line: argparse's
// usage text names learn.py and lists its --selftest, and this command has neither.

type scenario struct {
	name  string
	file  *string // nil: no file
	args  []string
	usage bool
}

func s(v string) *string { return &v }

const base = "# Review Loop Learnings\n\n## Dismissed\n<!-- c -->\n- 2026-01-01: foo bar (declined)\n\n## Accepted patterns\n<!-- c -->\n- 2026-02-01: keep the retry backoff\n"

var scenarios = []scenario{
	{name: "add_dismissed", file: s(base), args: []string{"add", "{f}", "a new dismissal", "--section", "dismissed"}},
	{name: "add_accepted", file: s(base), args: []string{"add", "{f}", "a new pattern", "--section", "accepted"}},
	{name: "add_section_before_text", file: s(base), args: []string{"add", "{f}", "--section", "accepted", "first of the patterns"}},
	{name: "add_section_equals", file: s(base), args: []string{"add", "{f}", "an equals-joined section", "--section=dismissed"}},
	{name: "add_creates_the_skeleton", args: []string{"add", "{f}", "the first dismissal", "--section", "dismissed"}},
	{name: "add_appends_a_missing_section", file: s("# L\n\n## Dismissed\n- 2026-01-01: a\n"), args: []string{"add", "{f}", "a pattern for the new section", "--section", "accepted"}},
	{name: "add_after_several_comments", file: s("## Dismissed\n<!-- a -->\n  <!-- b -->\n- 2026-01-01: a\n"), args: []string{"add", "{f}", "a new dismissal", "--section", "dismissed"}},
	{name: "add_header_with_spaces", file: s("  ## Dismissed  \n- 2026-01-01: a\n"), args: []string{"add", "{f}", "a new dismissal", "--section", "dismissed"}},
	{name: "add_banned_dismissal", file: s(base), args: []string{"add", "{f}", "consistent with existing code here", "--section", "dismissed"}},
	{name: "add_banned_wrapped", file: s(base), args: []string{"add", "{f}", "this is how the rest of the\n  codebase does it", "--section", "dismissed"}},
	{name: "add_banned_but_accepted", file: s(base), args: []string{"add", "{f}", "flag code that mirrors the existing retry helper", "--section", "accepted"}},
	{name: "add_without_section", file: s(base), args: []string{"add", "{f}", "x"}, usage: true},
	{name: "add_bad_section", file: s(base), args: []string{"add", "{f}", "x", "--section", "other"}, usage: true},
	{name: "bump_hit", file: s(base), args: []string{"bump", "{f}", "foo bar"}},
	{name: "bump_first_of_two", file: s(base), args: []string{"bump", "{f}", "e"}},
	{name: "bump_miss", file: s(base), args: []string{"bump", "{f}", "nope"}},
	{name: "bump_miss_quotes", file: s(base), args: []string{"bump", "{f}", "it's"}},
	{name: "bump_missing_file", args: []string{"bump", "{f}", "x"}},
	{name: "bump_ignores_a_malformed_date", file: s("- 2026-1-01: x\n- 2026-01-02: x\n"), args: []string{"bump", "{f}", "x"}},
	{name: "prune_under_cap", file: s(base), args: []string{"prune", "{f}"}},
	{name: "prune_dismissed_first", file: s("## Dismissed\n- 2026-03-01: a\n- 2026-01-01: b\n## Accepted patterns\n- 2026-01-15: PATTERN: keep me\n- 2025-01-01: d\n"), args: []string{"prune", "{f}", "--cap", "2"}},
	{name: "prune_one", file: s("## Dismissed\n- 2026-03-01: a\n- 2026-01-01: b\n"), args: []string{"prune", "{f}", "--cap=1"}},
	{name: "prune_more_than_there_are", file: s("- 2026-03-01: PATTERN: a\n- 2026-01-01: PATTERN: b\n- 2026-01-02: c\n"), args: []string{"prune", "{f}", "--cap", "0"}},
	{name: "prune_missing_file", args: []string{"prune", "{f}"}},
	{name: "prune_bad_cap", file: s(base), args: []string{"prune", "{f}", "--cap", "x"}, usage: true},
	{name: "crlf_and_no_final_newline", file: s("## Dismissed\r\n- 2026-01-01: a\r\n- 2026-01-02: b"), args: []string{"bump", "{f}", "a"}},
	{name: "splitlines_breaks", file: s("## Dismissed\v- 2026-01-01: a\x1c- 2026-01-02: b tail\n"), args: []string{"bump", "{f}", "b"}},
	{name: "dash_led_text_with_a_space", file: s(base), args: []string{"add", "{f}", "--no-verify was used", "--section", "accepted"}},
	{name: "dash_led_bump_text", file: s(base), args: []string{"bump", "{f}", "- foo"}},
	{name: "negative_number_text", file: s(base), args: []string{"add", "{f}", "-1234567890", "--section", "accepted"}},
	{name: "dash_digit_text", file: s(base), args: []string{"add", "{f}", "-5% slower when cold", "--section", "accepted"}},
	{name: "dash_digit_text_without_a_space", file: s(base), args: []string{"add", "{f}", "-5%-slower-when-cold", "--section", "accepted"}},
	{name: "option_value_with_a_space", file: s(base), args: []string{"add", "{f}", "x", "--section=accepted now"}, usage: true},
	{name: "ambiguous_abbreviation_with_a_spaced_value", file: s(base), args: []string{"add", "{f}", "--section", "dismissed", "--s=a b"}, usage: true},
	{name: "short_help_with_a_space", file: s(base), args: []string{"add", "{f}", "--section", "dismissed", "-h x"}, usage: true},
	{name: "bare_dashes_before_an_equals", file: s(base), args: []string{"add", "{f}", "--=x y z", "--section", "dismissed"}, usage: true},
	{name: "plain_short_help", file: s(base), args: []string{"bump", "{f}", "-h"}, usage: true},
	{name: "double_dash", file: s(base), args: []string{"add", "{f}", "--section", "accepted", "--", "-x after a double dash"}},
	{name: "abbreviated_option", file: s(base), args: []string{"add", "{f}", "an abbreviated section", "--sec", "dismissed"}},
	{name: "abbreviated_cap", file: s("- 2026-01-01: a\n- 2026-01-02: b\n"), args: []string{"prune", "{f}", "--ca=1"}},
	{name: "ambiguous_abbreviation", file: s(base), args: []string{"add", "{f}", "x", "--s", "dismissed"}, usage: true},
	{name: "option_missing_its_value", file: s(base), args: []string{"add", "{f}", "x", "--section", "--cap", "1"}, usage: true},
	{name: "empty_file_argument", args: []string{"add", "", "x", "--section", "accepted"}, usage: true},
	{name: "no_arguments", usage: true},
	{name: "unknown_command", file: s(base), args: []string{"rm", "{f}"}, usage: true},
	{name: "too_many_arguments", file: s(base), args: []string{"bump", "{f}", "a", "b"}, usage: true},
}

func TestLearnMatchesThePython(t *testing.T) {
	oracle := os.Getenv("LEARN_ORACLE")
	old := Today
	Today = func() string { return "<today>" }
	t.Cleanup(func() { Today = old })
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			if oracle != "" {
				capture(t, oracle, sc)
			}
			f, args := setup(t, sc)
			var out, errOut bytes.Buffer
			code := Command(args, &out, &errOut)
			norm := func(s string) string { return strings.ReplaceAll(s, f, "{f}") }
			got := map[string]string{"exit": strconv.Itoa(code), "stdout": norm(out.String()),
				"stderr": norm(errOut.String()), "file": readOr(t, f)}
			keys := []string{"exit", "stdout", "stderr", "file"}
			if sc.usage {
				// The usage line names learn.py and its --selftest; the error line after it is
				// compared, with the program's name swapped.
				keys = []string{"exit", "file"}
				lines := strings.Split(strings.TrimSpace(golden.Read(t, "stderr")), "\n")
				want := strings.Replace(lines[len(lines)-1], "learn.py:", "looper learn:", 1)
				if !strings.HasSuffix(strings.TrimSpace(got["stderr"]), want) {
					t.Errorf("stderr: got\n%q\nwant it to end with\n%q", got["stderr"], want)
				}
			}
			for _, k := range keys {
				if want := golden.Read(t, k); got[k] != want {
					t.Errorf("%s: got\n%q\nwant\n%q", k, got[k], want)
				}
			}
		})
	}
}

func setup(t *testing.T, sc scenario) (string, []string) {
	f := filepath.Join(t.TempDir(), "learnings.md")
	if sc.file != nil {
		if err := os.WriteFile(f, []byte(*sc.file), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	args := make([]string, len(sc.args))
	for i, a := range sc.args {
		args[i] = strings.ReplaceAll(a, "{f}", f)
	}
	return f, args
}

func readOr(t *testing.T, f string) string {
	b, err := os.ReadFile(f)
	if errors.Is(err, fs.ErrNotExist) {
		return "<absent>"
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// capture runs learn.py on the scenario and writes its goldens.
func capture(t *testing.T, oracle string, sc scenario) {
	f, args := setup(t, sc)
	cmd := exec.Command("python3", append([]string{oracle}, args...)...)
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	code := 0
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			t.Fatal(err)
		}
		code = ee.ExitCode()
	}
	today := time.Now().Format("2006-01-02")
	norm := func(s string) string { return strings.ReplaceAll(strings.ReplaceAll(s, today, "<today>"), f, "{f}") }
	dir := filepath.Join("testdata", "oracle", filepath.FromSlash(t.Name()))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for k, v := range map[string]string{"exit": strconv.Itoa(code), "stdout": norm(out.String()),
		"stderr": norm(errOut.String()), "file": norm(readOr(t, f))} {
		if err := os.WriteFile(filepath.Join(dir, k), []byte(v), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}
