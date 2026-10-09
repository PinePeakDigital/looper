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
recorded dismissal, or the default branch. **"There is no evidence either way" is also
a block**, and that one is a deliberate divergence from the Python — see below.

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
subprocess. The reason itself cannot say so: the parity gate compares the decoded
reason for exact equality against the Python's, which has no second stream. A refusal
for a report that is genuinely unposted stays silent, so the note's presence is the
signal.

**An empty record blocks, and the Python permits — the one deliberate divergence in
the verdict.** When the run id resolves to no readable row — a wrong `-store`, a
missing file, a mistyped `-run-id`, or every row for that run torn — the record loads
as empty *with no error*. Convergence then reads `unknown`, which is a disclosure
rather than a block; no recorded outcome can block, because there is none; and the
required report fingerprint collapses to `0 cycle(s) · 0 agent(s)`, which is exactly
what `pr-report.py` renders for a run with no cycle rows — 22 of the 42 runs in the
author's own store at `22d086e`, a figure pinned to a commit because the store is append-only
and an unqualified count goes stale on the next run. So a *genuine* report, rendered by the real tool for a real
zero-cycle run, satisfies the gate off a record that said nothing, with no forgery at
all. Measured: changing only `$HOME` turned a recorded `test-failure` refusal into a
granted push, in both implementations.

`push-check.py` still permits there, faithfully following the design's own split. This
port refuses, because an empty record is not a run that stopped looking — it is the
absence of any evidence that a run happened, and misresolving the store is otherwise
the cheapest way to make this gate say yes. The divergence is asserted rather than
left to drift, by `TestTheEmptyRecordDivergence`, which also pins that the Python still
permits: if that ever changes, the divergence has closed and the case goes back in the
agreement table.

stderr still carries a note naming the run and the store that was read, because the
refusal cannot say *why* nothing was readable. A run whose rows were all torn is
absent from the loaded record exactly as a run that was never written is, and
`record.Load` drops a torn line without counting it — so the note can name a store
that `grep` finds the run id in. Distinguishing the two needs `Load` to report how
many rows it dropped.

A note on comparing the two implementations by hand: their stdout is **not** byte-
identical and never has been. `json.dumps` defaults to `ensure_ascii=True`, so the
Python escapes the em dash in the common refusal reason as `\u2014` where Go emits it
literally. The parity gate compares the decoded JSON, field by field, which is the
level at which the two agree.

A port of `push-check.py`, with a parity gate that runs the Python against the same
store, repo and flags and requires the same JSON — `disclose` wording included,
since a reworded disclosure is a behaviour change no verdict comparison catches.

## `looper pr-report`

Renders the Step 14 review disclosure from the run record, and publishes it.

```text
looper pr-report -run-id <id> [-post] [-label] [-findings-file f]
                 [-repo dir] [-branch b] [-store path]
```

Without `-post` the body goes to stdout and nothing else does. With `-post` it comments
on this branch's PR and *also* keeps a local copy, or — when it cannot comment — keeps
only the local copy at `.git/info/review-loop-pending-report.<run-id>.md` for Step 0c to
flush. The body is never dropped: a report that exists in neither place is what leaves
`push-check` refusing the push on advice that cannot succeed.

The split the Python states and this keeps: **facts** come from the record and are not
retyped by the orchestrator, so they cannot drift from what was recorded; the
**narrative** arrives on stdin or via `-findings-file` and is appended verbatim, because
what the agents found is not derivable and must not be faked.

**It refuses a run it cannot read.** `record.Load` returns an empty map and no error for
a mistyped id or a wrong `-store`, and a report rendered from that is not an empty
report — it is a genuine-looking one reading `0 cycle(s) · 0 agent(s)`, which is exactly
the fingerprint `push-check` would then accept. Both halves of the gate refuse there, on
the same measurement.

**"No PR" is no longer asserted from a failed `gh`.** `pr-report.py` reads
`rc, num, _ = sh("gh", "pr", "view", ...)` and branches on `rc != 0 or not num`, printing
"no PR yet" — one assertion standing in for three causes, with the only stream that says
which discarded into `_`. The advice it gives cannot fix the other two, and on the
author's machine `gh` has already failed with a keychain TLS error while `git push`
worked. This port asks `gh pr list --head <branch>`, which exits 0 with empty output for
"there are none" and non-zero only when gh itself failed, and reports four states
distinctly: a PR was found, gh answered that there is none, gh could not be run at all,
or gh ran and failed — carrying gh's own stderr in the last two. Every one of them still
keeps the body. Asserted by `TestThePostDiagnosisDivergence`, which also pins that the
Python still conflates them.

With `-label` it applies `review:<convergence>` to the PR and removes the other three, so
two cannot stand at once — the at-a-glance half of the signal, so a reader need not open
a comment to learn whether the review finished. The convergence is derived from the same
read as the disclosure in the body, not taken as a flag. A label failure never fails a
publish that already succeeded, and the label is applied whenever a PR was found — including
when the comment itself failed, since nothing about `gh pr edit` depends on the comment having
landed. Every local copy carries a note saying the label is still owed, the successful-post
copy included; that is what the Python writes and Step 0c reads, and narrowing it to the
deferred case would be a divergence rather than a fix.

Parity with `pr-report.py` is gated two ways: every run in the author's live store is
rendered by both and compared byte for byte, and a fixture table covers the shapes no
real row happens to hold — an executed gate with no `status` key, a roster entry whose
`id` is a number, a `findings: 0` that renders as *nothing* because `cell()` is
`str(text or "")`. Where the two cannot agree, the divergence is a test rather than a
comment: `TestTheEnumeratedRenderDivergences` names each one with both renderings, and
`TestThreeShapesCrashTheOracleAndAreRefusedHere` pins three record shapes on which the
Python tracebacks — dropping the whole report — and this refuses at the typed boundary
with the offending field named.

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
