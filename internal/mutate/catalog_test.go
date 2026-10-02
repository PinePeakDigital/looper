package mutate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// write drops .mut files into a fresh catalog directory.
func catalogDir(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

const wellFormed = `# a comment, and a blank line, both ignored

target: runlog.py
verify: ./runlog.test.sh
verify: ./plan.test.sh
why:    convergence failed open on an unresolved ask
--- old
    if not last.get("asked"):
        return "converged"
--- new
    return "converged"
`

func TestParsesEveryField(t *testing.T) {
	dir := catalogDir(t, map[string]string{"conv.mut": wellFormed})
	muts, err := ParseCatalog(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(muts) != 1 {
		t.Fatalf("got %d mutations, want 1", len(muts))
	}
	m := muts[0]
	if m.Target != "runlog.py" {
		t.Errorf("target = %q", m.Target)
	}
	if len(m.Verify) != 2 || m.Verify[0] != "./runlog.test.sh" || m.Verify[1] != "./plan.test.sh" {
		t.Errorf("verify = %q, want both commands in order", m.Verify)
	}
	if m.Why != "convergence failed open on an unresolved ask" {
		t.Errorf("why = %q", m.Why)
	}
	if m.Name() != "conv" {
		t.Errorf("name = %q, want conv", m.Name())
	}
	// Indentation is syntax in some of the languages a catalog can target, Python and
	// shell among them, so the blocks must survive byte-for-byte.
	wantOld := "    if not last.get(\"asked\"):\n        return \"converged\""
	if m.Old != wantOld {
		t.Errorf("old block mangled:\n got %q\nwant %q", m.Old, wantOld)
	}
	if m.New != "    return \"converged\"" {
		t.Errorf("new block mangled: %q", m.New)
	}
}

func TestParsesExpect(t *testing.T) {
	dir := catalogDir(t, map[string]string{
		"x.mut": "target: a.py\nverify: ./t.sh\nwhy: w\nexpect: --- FAIL: TestThing\n--- old\na\n--- new\nb\n",
	})
	muts, err := ParseCatalog(dir)
	if err != nil {
		t.Fatal(err)
	}
	if muts[0].Expect != "--- FAIL: TestThing" {
		t.Errorf("expect = %q", muts[0].Expect)
	}
}

// Absent, it stays empty rather than picking up a default, because any non-empty value
// would silently change what counts as a catch for every existing entry.
func TestExpectIsOptional(t *testing.T) {
	dir := catalogDir(t, map[string]string{"x.mut": "target: a.py\nverify: ./t.sh\nwhy: w\n--- old\na\n--- new\nb\n"})
	muts, err := ParseCatalog(dir)
	if err != nil {
		t.Fatal(err)
	}
	if muts[0].Expect != "" {
		t.Errorf("expect = %q, want empty", muts[0].Expect)
	}
}

// A multi-line block keeps its interior blank lines; trimming only the one
// newline that separates a block from what follows.
func TestBlockKeepsInteriorBlankLines(t *testing.T) {
	dir := catalogDir(t, map[string]string{"x.mut": "target: a.py\nverify: ./t.sh\nwhy: w\n--- old\none\n\ntwo\n--- new\none\n"})
	muts, err := ParseCatalog(dir)
	if err != nil {
		t.Fatal(err)
	}
	if muts[0].Old != "one\n\ntwo" {
		t.Errorf("old = %q, want %q", muts[0].Old, "one\n\ntwo")
	}
}

// Every refusal below exists because the alternative is a catalog that quietly
// shrinks — and a score that flatters itself by the entries it dropped.
func TestRefusesMalformedFiles(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{"no target", "verify: ./t.sh\nwhy: w\n--- old\na\n--- new\nb\n", "no `target:`"},
		{"no verify", "target: a.py\nwhy: w\n--- old\na\n--- new\nb\n", "nothing could catch it"},
		{"no why", "target: a.py\nverify: ./t.sh\n--- old\na\n--- new\nb\n", "would not say what it means"},
		{"empty old", "target: a.py\nverify: ./t.sh\nwhy: w\n--- old\n--- new\nb\n", "empty"},
		{"identical", "target: a.py\nverify: ./t.sh\nwhy: w\n--- old\na\n--- new\na\n", "mutates nothing"},
		{"no old marker", "target: a.py\n--- new\nb\n", "no \"--- old\""},
		{"no new marker", "target: a.py\n--- old\na\n", "no \"--- new\""},
		{"markers reversed", "target: a.py\n--- new\nb\n--- old\na\n", "must come before"},
		{"unknown header", "target: a.py\nverify: ./t.sh\nwhy: w\nmodel: opus\n--- old\na\n--- new\nb\n", "unknown header"},
		{"header not key: value", "target: a.py\nverify: ./t.sh\nwhy: w\nnonsense\n--- old\na\n--- new\nb\n", "not `key: value`"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := catalogDir(t, map[string]string{"bad.mut": tc.body})
			_, err := ParseCatalog(dir)
			if err == nil {
				t.Fatal("accepted a malformed file")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error does not name the cause:\n got %v\nwant it to mention %q", err, tc.want)
			}
		})
	}
}

// One bad file poisons the whole catalog rather than being skipped past.
func TestOneBadFileFailsTheCatalog(t *testing.T) {
	dir := catalogDir(t, map[string]string{
		"good.mut": wellFormed,
		"bad.mut":  "target: a.py\n",
	})
	if _, err := ParseCatalog(dir); err == nil {
		t.Fatal("a malformed file was skipped past")
	}
}

// An empty catalog would otherwise report a perfect score over nothing.
func TestRefusesEmptyCatalog(t *testing.T) {
	dir := catalogDir(t, map[string]string{"notes.md": "not a mutation"})
	_, err := ParseCatalog(dir)
	if err == nil {
		t.Fatal("accepted a catalog with no mutations")
	}
	if !strings.Contains(err.Error(), "no .mut files") {
		t.Errorf("error does not name the cause: %v", err)
	}
}

// Sorted by path, so two runs of the same catalog report in the same order and a
// score can be diffed against the previous one.
func TestOrderIsStable(t *testing.T) {
	dir := catalogDir(t, map[string]string{
		"zebra.mut":  wellFormed,
		"alpha.mut":  wellFormed,
		"middle.mut": wellFormed,
	})
	muts, err := ParseCatalog(dir)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, m := range muts {
		got = append(got, m.Name())
	}
	if strings.Join(got, ",") != "alpha,middle,zebra" {
		t.Errorf("order = %v, want alpha,middle,zebra", got)
	}
}
