# looper

Tooling for the `review-loop` skill — a multi-cycle, multi-agent code review loop.

## Why this repo exists

The skill grew to the point where its correctness rested on test suites nobody had
checked. Reviewing it turned up twelve assertions that could not fail: greps
matching a hard-coded table header, an inverted test satisfied by a crash printing
nothing, a row count identical under both behaviours, a fixture whose setup was
silently refused. All of them passed, continuously, for days.

So the first thing built here is not more of the skill. It is a way to measure
whether the tests are worth anything.

## `looper mutate`

Reintroduces a known defect into the code, runs the suites that should notice, and
reports whether they did.

```
looper mutate [-catalog mutations] [-root .]
```

- **caught** — a verify command failed. The suite does its job.
- **survived** — every verify command passed with the defect present. A hole in the
  suite: that defect can ship green.
- **stale** — the anchor no longer matches, so nothing was tested. A hole in the
  catalog, and scored as a failure too; a catalog that quietly stops applying
  flatters the score it produces.

Exits non-zero on any survivor or stale entry, so CI can gate on it.

### Writing a mutation

One `.mut` file per defect, under `mutations/`:

```
target: internal/mutate/mutate.go
verify: go test ./internal/mutate/ -count=1 -run 'TestPreservesMode'
why:    rewriting a file drops its executable bit, breaking whatever runs it
--- old
	perm := info.Mode().Perm()
--- new
	perm := os.FileMode(0o644)
```

`verify:` is a shell command line that must **fail** once the edit is applied;
repeat it for more than one. The `old` block must occur exactly once in the target,
and both blocks are taken verbatim — indentation included, since it is syntax in
most of what this catalog targets.

`why:` is what makes a survivor actionable six months later. Prefer a defect that
actually shipped over one invented to pad the score.

### Safety

Mutation testing edits your working tree. The runner therefore:

- **refuses to start on a dirty tree**, and restores from bytes held in memory
  rather than `git checkout`. Doing this by hand, `git checkout --` silently
  reverted uncommitted work six times in one session, and twice produced a *wrong
  conclusion* — a guard looked untested when the restore had already deleted the
  assertion testing it.
- **requires the anchor to occur exactly once**, so an edit that matched nothing
  cannot read as a working guard.
- **preserves the file mode**, after a fresh create dropped an executable bit and
  the hook requiring it failed three times for reasons that looked unrelated.
- **gives verify commands no stdin**, so one that reads it cannot hang the run.
- **restores on interrupt**, then exits 130.

The catalog's first entries are the runner's own rules, so the tool measures itself.

## `looper docs`

```
looper docs <dir>
```

Reports every flag the markdown under `<dir>` (and `<dir>/references/`) attributes to
a script that does not have it. SKILL.md documented a `--max-cycles` flag for a full
day after it was deleted; this is the cheap half of that finding class. The expensive
half — prose that states a step order the scripts refuse — still needs a reader.

A flag counts as declared if its literal appears anywhere in the script's source.
Narrower rules were wrong in both directions: matching only `add_argument(` missed a
script that parses `sys.argv` by hand, and reading `--help` output would make a script
that cannot start look flagless.

Attribution is per line: a flag is checked against the scripts named on the same line.
Prose spanning lines is skipped rather than guessed at — the aim is a signal worth
acting on every time it fires, not a complete one.
