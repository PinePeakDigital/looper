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

```text
looper mutate [-catalog mutations] [-root .]
```

- **caught** — a verify command failed, and failed the way the mutation said it would.
  The suite does its job.
- **survived** — every verify command passed with the defect present. A hole in the
  suite: that defect can ship green.
- **stale** — the target could not be read, or the anchor no longer matches it exactly
  once, so nothing was tested. A hole in the catalog, and scored as a failure too; a
  catalog that quietly stops applying flatters the score it produces.
- **broken** — a verify command failed, but not with the marker the mutation named, so
  something other than the named assertion is what broke. Usually the mutation itself not
  compiling. Also scored as a failure: that entry tested nothing while reading like a catch.

Exits non-zero on any survivor, stale or broken entry, so CI can gate on it.

Before mutating anything, each distinct verify command is run once against the unmodified
code and has to pass. A command already failing — a flaky test, a broken environment,
someone else's regression — fails identically with the mutation applied, so without this
every entry it guards would score as caught while proving nothing. Commands are
deduplicated, so this costs far less than one extra run per mutation.

### Writing a mutation

One `.mut` file per defect, under `mutations/`:

```text
target: internal/mutate/mutate.go
verify: go test ./internal/mutate/ -count=1 -run 'TestRestoreRecreatesWithTheOriginalMode'
expect: --- FAIL:
why:    a restore that recreates a deleted target brings it back without its executable bit
--- old
	perm := info.Mode().Perm()
--- new
	_ = info
	perm := os.FileMode(0o644)
```

`verify:` is a shell command line that must **fail** once the edit is applied; repeat it
for more than one. The `old` block must occur exactly once in the target, and both blocks
are taken verbatim — indentation included, since it is syntax in some of the languages a
catalog can target.

`expect:` is optional and carries most of the weight. It is a substring the failing output
must contain for the failure to count as a catch, because a non-zero exit on its own lies
in at least three ways:

- the mutation does not **compile**, so the build fails and the suite never runs. Two of
  this repo's own nineteen entries were in that state, and the score read 19/19.
- `go test -run` with a pattern matching **no tests** exits 0, so a renamed test turns a
  stale catalog entry into what looks like a hole in the suite.
- a suite **already red** for an unrelated reason fails exactly the same way.

Write the marker your suite prints when an assertion actually fails — `--- FAIL:` for Go.
Entries with no `expect:` keep the old behaviour, so an existing catalog still runs.

Note also that a mutation which deletes the last use of a variable will not compile in Go.
Keep it used (`_ = info` above) or the entry reports as broken.

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
- **restores on interrupt** — SIGINT, SIGTERM, SIGHUP and SIGQUIT, the last two because
  their default disposition runs no deferred functions at all — then exits 130.
- **treats a failed restore as fatal.** Ignoring it meant the tool could print a clean
  score, exit 0, and leave a mutated file on disk, which is the one outcome the clean-tree
  refusal exists to make impossible.
- **refuses a target git does not track.** An untracked or ignored file passes the
  clean-tree check, because `git status --porcelain` does not list ignored files, but has no
  committed copy to recover from if the process dies before the restore.
- **gives the mutated file a later modification time**, and puts the original back on
  restore. The baseline run would otherwise prime any cache keyed on `(mtime, size)` —
  Python's `.pyc`, make, most watchers — and a same-length mutation landing in the same
  second would be checked against the bytecode the baseline had just built. Measured: an
  `x > 0` → `x < 0` mutation re-imported stale bytecode and reported the suite as missing
  the defect. (`go test` hashes content and was never affected; most things are not Go.)
- **kills the process group** on timeout, not just the shell, whose grandchild test runner
  would otherwise outlive it.

The catalog's first entries are the runner's own rules, so the tool measures itself.

## `looper push-check`

Answers Step 14's one question — may this branch be auto-pushed — and prints the
answer as JSON on stdout with exit 0, whether or not the push is permitted. A
refusal is a successful answer; exiting non-zero would make it indistinguishable
from the tool failing to produce one.

```text
looper push-check -run-id <id> [-gate-state passed|skipped|blocked]
                  [-unresolved-skip] [-branch b] [-default-branch b]
                  [-repo dir] [-store path]
```

```json
{"push":true,"reason":"capped (review not finished), evidence gate ok, feature branch with upstream",
 "convergence":"capped","disclose":"Review CAPPED at 9 of 8 agents: ..."}
```

The decision is split in two, and the split is the whole design. **"We stopped
looking" is a disclosure**: a capped, halted or unknown run pushes and owes the
`disclose` line, because a cap that strands commits just moves the decision back to
a human every time. **"It is broken" is a block**: a recorded `test-failure`,
`blocked` or `abandoned` outcome, a blocked evidence gate, a finding skipped with no
recorded dismissal, or the default branch.

Two things are read from the record rather than accepted as arguments, and both used
to be flags:

- **convergence**, which used to arrive as `--clean-exit`. The one safety question
  the gate exists to answer was answered by the orchestrator asserting it, and one
  real run recorded `clean` for a run its own author reported as unfinished.
- **the outcome**, whose channel was deleted with `--clean-exit` and never replaced:
  a run that recorded `test-failure` with a failed gate was permitted, with the
  reason "converged, evidence gate ok".

The report check is the third, and it reads the artifact rather than a claim about
it. A push is refused until this run's rendered report is found either in a PR
comment or in `.git/info/review-loop-pending-report.<run-id>.md`, carrying all three
of the run's marker, the `## review-loop` heading, and its `N cycle(s) · M agent(s)`
line — numbers that cannot be produced without rendering from the record. Requiring the marker alone was cheaper
to forge (37 bytes of `printf` for a real run id) than the self-report it replaced. It is required on
every terminal exit, converged included: the incident behind the design was a clean
exit on a fresh branch whose summary never reached the PR.

stdout carries the JSON and nothing else; `-h` and flag errors go to stderr. stderr
also carries a one-line note when neither `gh pr view` nor `git rev-parse` could be
run at all, because the gate then fails closed with a reason — "run `pr-report.py
--post` first" — that is the wrong instruction for a missing binary or a killed
subprocess. The reason itself cannot say so: parity pins it byte-for-byte against the
Python, which has no second stream. A refusal for a report that is genuinely unposted
stays silent, so the note's presence is the signal.

One state is not covered by that note and is worth knowing: if the store does not
hold the run — a wrong `-store`, a missing file, a mistyped `-run-id` — the record
loads as empty with no error, the required fingerprint collapses to `0 cycle(s) · 0
agent(s)`, and the refusal reads as an unposted report with nothing on stderr. The
store path that was read is printed nowhere.

A port of `push-check.py`, with a parity gate that runs the Python against the same
store, repo and flags and requires the same JSON — `disclose` wording included,
since a reworded disclosure is a behaviour change no verdict comparison catches.

## `looper docs`

```text
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
