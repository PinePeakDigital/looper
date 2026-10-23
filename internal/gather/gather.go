// Package gather is `looper context`: review-loop's context.sh, Step 0's one call that
// resolves the base branch (and fetches it), loads the learnings file, detects the test and
// lint commands, and sizes the branch diff — raw, as review surface, and since the last
// review — printed as one JSON object for the orchestrator and `looper plan`.
package gather

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/pinepeakdigital/looper/internal/home"
	"github.com/pinepeakdigital/looper/internal/py"
	"github.com/pinepeakdigital/looper/internal/run"
)

type obj = py.Obj

// Lockfiles at any depth. A dependency bump is a real change, but its line count is noise:
// a one-line version edit regenerates thousands of lines. Supply-chain risk is the Step 4a
// analyzers' job, not size's.
var lockfiles = strings.Fields(`pnpm-lock.yaml package-lock.json npm-shrinkwrap.json yarn.lock
Cargo.lock poetry.lock uv.lock Pipfile.lock Gemfile.lock composer.lock go.sum mix.lock pubspec.lock`)

const (
	compactionThreshold = 40 // sweep before the ~50-entry cap so it self-heals early
	regrowth            = 8  // re-sweeping needs real growth since the last sweep
	learnCap            = 50 // learn.py prune's default: past this the age-based eviction takes over
)

// out runs a command and returns its stdout with trailing newlines removed, as a shell
// command substitution does, or "" and false when it fails.
func out(name string, args ...string) (string, bool) {
	o, err := exec.Command(name, args...).Output()
	return strings.TrimRight(string(o), "\n"), err == nil
}

// netTimeout bounds the calls that reach the network (gh, git fetch), which context.sh left
// unbounded: a stalled one hung Step 0 with nothing printed. A variable so a test can shorten it.
var netTimeout = 30 * time.Second

// probe is the stdout of a call that reaches the network, whatever its exit, as `$(...)` was:
// cut off at netTimeout, and at run's drain bound when a grandchild (a credential helper)
// still holds its pipes.
func probe(name string, args ...string) string {
	return strings.TrimRight(string(run.Run("", netTimeout, name, args...).Out), "\n")
}

// sum is `awk '{a+=$1; d+=$2} END {print a+d+0}'` over numstat: binary files' "-" count 0.
func sum(numstat string) int {
	n := 0
	for _, l := range strings.Split(numstat, "\n") {
		f := strings.Fields(l)
		for i := 0; i < 2 && i < len(f); i++ {
			n += awkNum(f[i])
		}
	}
	return n
}

// awkNum is awk's numeric value of a field: its leading digits, else 0.
func awkNum(s string) int {
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	n, _ := strconv.Atoi(s[:i])
	return n
}

func baseBranch() string {
	if b := probe("gh", "pr", "view", "--json", "baseRefName", "-q", ".baseRefName"); b != "" {
		return b
	}
	if b := probe("gh", "repo", "view", "--json", "defaultBranchRef", "-q", ".defaultBranchRef.name"); b != "" {
		return b
	}
	b, _ := out("git", "symbolic-ref", "refs/remotes/origin/HEAD")
	return strings.TrimPrefix(b, "refs/remotes/origin/")
}

// swept matches the sweep marker loosely on purpose: a stray \r from a CRLF file, a missing
// space or a trailing period must not silently revert the trigger to the bare count.
var swept = regexp.MustCompile(`^[ \t\n\v\f\r]*<!--[ \t\n\v\f\r]*[Ss]wept:[ \t\n\v\f\r]*([0-9]+)`)

// learnings is the learnings file's text (trailing newlines removed), its entry count, and the
// count the last sweep left, or nil when the file has never been swept.
func learnings(errOut io.Writer) (string, int, *string) {
	p, err := home.Ledger("", home.Learnings)
	if err != nil {
		// Said, not swallowed: an empty learnings field reads as "nothing dismissed", so every
		// dismissed finding would be raised again with no sign of why. (A store that resolves but
		// holds no readable file is still silent, as context.sh was.)
		fmt.Fprintf(errOut, "looper context: learnings unreadable, reviewing without them: %v\n", err)
		return "", 0, nil
	}
	if fi, err := os.Stat(p); err != nil || !fi.Mode().IsRegular() {
		return "", 0, nil
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return "", 0, nil
	}
	entries := 0
	var last *string
	for _, l := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(l, "- ") {
			entries++
		}
		if m := swept.FindStringSubmatch(l); m != nil {
			last = &m[1]
		}
	}
	// A command substitution drops NUL bytes and every trailing newline.
	return strings.TrimRight(strings.ReplaceAll(string(b), "\x00", ""), "\n"), entries, last
}

// indentSensitive: every suffix, lowercased, since `values.yml.j2` and `Up.PY` hide their
// indentation from a -w diff as surely as `app.py` does.
var (
	indent      = map[string]bool{}
	indentNames = map[string]bool{"makefile": true, "gnumakefile": true, "makefile.am": true, "makefile.in": true, "gnumakefile.am": true}
	indentStems = map[string]bool{"makefile": true, "gnumakefile": true}
)

func init() {
	for _, e := range strings.Fields(`.py .pyi .yml .yaml .hs .nim .elm .coffee .sass .styl .slim
.haml .pug .jade .cr .mk .make .md .markdown .feature`) {
		indent[e] = true
	}
}

func indentSensitive(path string) bool {
	name := strings.ToLower(path[strings.LastIndex(path, "/")+1:])
	parts := strings.Split(name, ".")
	if indentNames[name] || indentStems[parts[0]] {
		return true
	}
	for _, p := range parts[1:] {
		if indent["."+p] {
			return true
		}
	}
	return false
}

// perFile is the numstat per path, as the Python sizing helper read it; ok is false where the
// helper would have died (a count that is not a number).
func perFile(args []string) (map[string]int, []string, bool) {
	o, _ := exec.Command("git", append([]string{"diff", "--numstat"}, args...)...).Output()
	// Python's strict decode died on a name that is not UTF-8 (core.quotePath=false), and
	// context.sh then fell back to the raw count. Replacing the bytes instead would size on.
	if !utf8.Valid(o) {
		return nil, nil, false
	}
	acc, order := map[string]int{}, []string{}
	for _, l := range py.Splitlines(py.Decode(o)) {
		p := strings.Split(l, "\t")
		if len(p) != 3 {
			continue
		}
		n := 0
		for _, x := range p[:2] {
			if x == "-" {
				continue
			}
			v, err := strconv.Atoi(strings.TrimSpace(x))
			if err != nil {
				return nil, nil, false
			}
			n += v
		}
		if _, seen := acc[p[2]]; !seen {
			order = append(order, p[2])
		}
		acc[p[2]] = n
	}
	return acc, order, true
}

// size measures the branch against origin/<base>: the raw line count, and the review surface
// — raw minus lockfiles, files .gitattributes marks linguist-generated, and whitespace-only
// lines in files where whitespace cannot carry meaning — with what was dropped, said.
func size(base string) (changed, semantic int, diffstat, excluded string) {
	rng := "origin/" + base + "...HEAD"
	diffstat, _ = out("git", "diff", "--stat", rng)
	ns, ok := out("git", "diff", "--numstat", rng)
	if !ok {
		// An unfetched or missing base. context.sh printed 0 lines and said nothing, which
		// reads as an empty diff; the plan's scan then fails the same way and plans wide.
		return 0, 0, diffstat, "the diff against " + rng + " could not be read, so nothing was sized"
	}
	changed = sum(ns)

	// No positive pathspec: an exclude-only list already means "everything but these", and a
	// `.` was repo-relative only from the repo root.
	args := []string{rng, "--"}
	for _, f := range lockfiles {
		args = append(args, ":(exclude,glob)**/"+f, ":(exclude,literal)"+f)
	}
	names, _ := exec.Command("git", "diff", "--name-only", rng).Output()
	attr := exec.Command("git", "check-attr", "--stdin", "linguist-generated")
	attr.Stdin = bytes.NewReader(names)
	attrs, _ := attr.Output()
	var gen []string
	for _, l := range strings.Split(string(attrs), "\n") {
		if g, ok := strings.CutSuffix(l, ": linguist-generated: set"); ok && g != "" {
			gen = append(gen, g)
			// literal: `app/[id]/page.tsx` must not exclude more than the one file.
			args = append(args, ":(exclude,literal)"+g)
		}
	}

	raw, order, ok1 := perFile(args)
	blind, _, ok2 := perFile(append([]string{"-w"}, args...))
	if !ok1 || !ok2 {
		// Fail toward MORE review: no count from the helper is the raw total, never 0.
		return changed, changed, diffstat, "sizing helper produced no count; using the raw total"
	}
	kept := 0
	for _, f := range order {
		kept += raw[f]
		if indentSensitive(f) {
			semantic += raw[f]
		} else {
			semantic += blind[f]
		}
	}
	var said []string
	if changed-kept > 0 {
		said = append(said, fmt.Sprintf("%d line(s) in lockfiles or generated files", changed-kept))
	}
	if kept-semantic > 0 {
		said = append(said, fmt.Sprintf("%d whitespace-only line(s), indentation-sensitive files excepted", kept-semantic))
	}
	if len(gen) > 0 {
		said = append(said, "generated per .gitattributes: "+strings.Join(gen, ", "))
	}
	return changed, semantic, diffstat, strings.Join(said, "; ")
}

// lastReviewed is the newest of HEAD's last 200 ancestors that the review gate recorded as
// reviewed, and the raw size of everything since. Raw on purpose: it can only over-count,
// which makes the delta fast path harder to reach, never easier.
func lastReviewed() (string, int) {
	// No HOME is no store: a path relative to the repo would trust a store the repo ships.
	home, err := os.UserHomeDir()
	if err != nil {
		return "", 0
	}
	b, err := os.ReadFile(filepath.Join(home, ".claude", "review-loop", "reviewed-shas"))
	if err != nil || len(b) == 0 {
		return "", 0
	}
	stamped := map[string]bool{}
	for _, l := range strings.Split(string(b), "\n") {
		stamped[l] = true
	}
	revs, _ := out("git", "rev-list", "-n", "200", "HEAD")
	for _, sha := range strings.Split(revs, "\n") {
		if sha != "" && stamped[sha] {
			ns, _ := out("git", "diff", "--numstat", sha+"..HEAD")
			return sha, sum(ns)
		}
	}
	return "", 0
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// commands detects the test and lint commands from the working directory, as context.sh did.
func commands() (test, lint value, fix bool) {
	scripts := map[string]any{}
	// Python's read_text refused bytes that are not UTF-8, and context.sh read that as no scripts.
	if b, err := os.ReadFile("package.json"); err == nil && utf8.Valid(b) {
		var pkg map[string]any
		if json.Unmarshal(b, &pkg) == nil {
			if s, ok := pkg["scripts"].(map[string]any); ok {
				scripts = s
			}
		}
	}
	_, hasTest := scripts["test"]
	switch {
	case hasTest:
		test = "npm test"
	case exists("pyproject.toml") || exists("pytest.ini"):
		test = "pytest"
	case exists("Cargo.toml"):
		test = "cargo test"
	case exists("go.mod"):
		// Not in context.sh, which answered null for a Go module. A manifest, so it ranks with
		// Cargo.toml above the Makefile fallback.
		test = "go test ./..."
	default:
		if b, err := os.ReadFile("Makefile"); err == nil && regexp.MustCompile(`(?m)^test:`).MatchString(py.Decode(b)) {
			test = "make test"
		}
	}
	eslint, _ := filepath.Glob(".eslintrc*")
	lintScript, hasLint := scripts["lint"]
	_, hasLintFix := scripts["lint:fix"]
	switch {
	case hasLintFix:
		lint, fix = "npm run lint:fix", true
	case hasLint:
		lint = "npm run lint"
		// Python's `in`: a substring of a string, an element of a list, a key of an object.
		switch v := lintScript.(type) {
		case string:
			fix = strings.Contains(v, "--fix")
		case []any:
			for _, e := range v {
				fix = fix || e == "--fix"
			}
		case map[string]any:
			_, fix = v["--fix"]
		}
	case exists("ruff.toml") || exists(".ruff.toml"):
		lint, fix = "ruff check --fix", true
	case len(eslint) > 0:
		lint, fix = "eslint --fix", true
	case exists(".rubocop.yml"):
		lint, fix = "rubocop -A", true
	case exists("go.mod"):
		// go fmt rewrites, as the other fixers do, and skips testdata where gofmt -w . would
		// not; go vet reports. Either failing fails the command.
		lint, fix = "go fmt ./... && go vet ./...", true
	}
	return test, lint, fix
}

type value = py.Value

func orNil(s string) value {
	if s == "" {
		return nil
	}
	return s
}

// Command is `looper context`. It takes no arguments; -h prints its usage. Exit 0 with the
// JSON, whatever git and gh could or could not answer — each unanswered question is a null
// or a 0 the plan reads conservatively.
func Command(args []string, stdout, errOut io.Writer) int {
	if len(args) > 0 {
		if args[0] == "-h" || args[0] == "--help" {
			fmt.Fprintln(stdout, "usage: looper context    # Step 0's context as JSON, from the working directory")
			return 0
		}
		fmt.Fprintf(errOut, "looper context: takes no arguments, got %q\n", args[0])
		return 2
	}
	base := baseBranch()
	if strings.HasPrefix(base, "-") {
		// Never a branch name git accepts, and `git fetch origin <base>` would read it as an
		// option. Only a hostile origin/HEAD could supply one.
		base = ""
	}
	if base != "" {
		probe("git", "fetch", "origin", base) // best effort, as context.sh did
	}
	learn, entries, sweptAt := learnings(errOut)

	var changed, semantic int
	var diffstat, excluded string
	if base != "" {
		changed, semantic, diffstat, excluded = size(base)
	}
	last, delta := lastReviewed()
	test, lint, fix := commands()

	// A sweep only ever reduces the count, so a recorded count above today's is a bad write;
	// trusting it would disarm the trigger until the file grew past it. Read it as never-swept.
	var sweptN value
	rearm := -1
	if sweptAt != nil {
		if n, err := strconv.Atoi(*sweptAt); err == nil && n <= entries {
			sweptN = n
			rearm = n + regrowth
			if n < learnCap-2 {
				rearm = min(rearm, learnCap-2)
			}
		}
	}
	due := entries >= compactionThreshold && (rearm < 0 || entries >= rearm)

	_, err := io.WriteString(stdout, py.Encode(obj{
		{"base_branch", orNil(base)},
		{"test_cmd", test},
		{"lint_cmd", lint},
		{"lint_fix", fix},
		{"learnings", orNil(learn)},
		{"learnings_entries", entries},
		{"learnings_compaction_due", due},
		{"learnings_swept_entries", sweptN},
		{"today", time.Now().Format("2006-01-02")},
		{"diff_stat", orNil(diffstat)},
		{"changed_lines", changed},
		{"semantic_lines", semantic},
		{"sizing_excluded", orNil(excluded)},
		{"fast_path_eligible_by_size", 0 < changed && semantic < 30},
		{"last_reviewed_sha", orNil(last)},
		{"review_delta_lines", delta},
		{"fast_path_eligible_by_delta", last != "" && 0 < delta && delta < 30},
	}))
	if err != nil {
		return 1
	}
	return 0
}
