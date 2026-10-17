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
looper mutate [-catalog mutations] [-root .] [-check]
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
a block**, and so is a run that was never finished; both are deliberate divergences from
the Python — see below.

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
run at all, because the gate then fails closed with a reason — "run `looper pr-report
--run-id <id> --post` first" — that is the wrong instruction for a missing binary or a killed
subprocess. The reason itself cannot say so: the parity gate compares the decoded
reason for exact equality against what the Python printed, and it had no second stream. A refusal
for a report that is genuinely unposted stays silent, so the note's presence is the
signal.

**An empty record blocks, and the Python permits — a deliberate divergence in the
verdict.** When the run id resolves to no readable row — a wrong `-store`, a
missing file, a mistyped `-run-id`, or every row for that run torn — the record loads
as empty *with no error*. Convergence then reads `unknown`, which is a disclosure
rather than a block; no recorded outcome can block, because there is none; and the
required report fingerprint collapses to `0 cycle(s) · 0 agent(s)`, which is exactly
what `looper pr-report` renders for a run with no cycle rows — about half the runs in the author's own
store (22 of 45 when last counted) — a figure given as a ratio because the store lives
outside the repo and gains runs several times a day, so no commit pin and no date fixes it. So a *genuine* report, rendered by the real tool for a real
zero-cycle run, satisfies the gate off a record that said nothing, with no forgery at
all. Measured: changing only `$HOME` turned a recorded `test-failure` refusal into a
granted push, in both implementations.

`push-check.py` permitted there, faithfully following the design's own split. This
port refuses, because an empty record is not a run that stopped looking — it is the
absence of any evidence that a run happened, and misresolving the store is otherwise
the cheapest way to make this gate say yes. The divergence is asserted rather than
left to drift, by `TestTheEmptyRecordDivergence`, against the Python's frozen output.

**A run with no finish row blocks too, and the Python permitted it.** Step 14 closes the
record (`looper record finish`) before it posts the report or asks this gate, so a run that
reaches here unfinished skipped a step, and the report it posted rendered no outcome and no
tier. The Python read the finish only for a broken outcome, so it never noticed; the Stop
hook was the only thing that did, once per run and only when the session ended. Asserted by
`TestTheUnfinishedRunDivergence`, against the Python's frozen verdict for such a store.
Any finish row under this run's id closes it, whatever its outcome; a nudge does not. In
practice that is `looper record finish --run-id <id>` (`record abandon` closes it too, but
`abandoned` then blocks as broken; `record skipped` and `record carried` mint runs of their
own). The block sits ahead of every check but the empty record, so an operator who never
reached Step 14 is told to close the run rather than to post a report or change branch. A
session that died mid-loop gets out the same way: a `cycle-limit` finish is not broken, so
the run pushes with whatever disclosure its cycles derive.

stderr still carries a note naming the run and the store that was read, because the
refusal cannot say *why* nothing was readable. A run whose rows were all torn is
absent from the loaded record exactly as a run that was never written is, and
`record.Load` drops a torn line without counting it — so the note can name a store
that `grep` finds the run id in. Distinguishing the two needs `Load` to report how
many rows it dropped.

A note on reading the frozen oracle output: it is **not** byte-identical to this
command's stdout and never was. `json.dumps` defaults to `ensure_ascii=True`, so the
Python escapes the em dash in the common refusal reason as `\u2014` where Go emits it
literally. The parity gate compares the decoded JSON, field by field, which is the
level at which the two agree.

A port of `push-check.py`, which is now retired. Its parity gate compares against what
the Python printed for each case before it went — frozen under
`internal/push/testdata/oracle` — and requires the same JSON, `disclose` wording
included, since a reworded disclosure is a behaviour change no verdict comparison
catches. A deliberate change edits the frozen file in the same commit as the code: the
refusal above said `pr-report.py --post` until the switch commit retired that script, and
now names the run id, so the command can be run as printed.

## `looper pr-report`

Renders the Step 14 review disclosure from the run record, and publishes it.

```text
looper pr-report -run-id <id> [-post] [-label] [-findings-file f|-]
                 [-repo dir] [-branch b] [-store path]
```

Without `-post` the body goes to stdout and nothing else does. With `-post` it comments
on this branch's PR and *also* keeps a local copy, or — when it cannot comment — keeps
only the local copy at `.git/info/review-loop-pending-report.<run-id>.md` for Step 0c to
flush. The body is never dropped: a report that exists in neither place is what leaves
`push-check` refusing the push on advice that cannot succeed.

The split the Python states and this keeps: **facts** come from the record and are not
retyped by the orchestrator, so they cannot drift from what was recorded; the
**narrative** arrives via `-findings-file` (a path, or `-` for stdin) and is appended verbatim,
because what the agents found is not derivable and must not be faked. Stdin is read only under
`-findings-file -`: the older rule, read it whenever it is not a terminal, hung an
orchestrator's `pr-report --post` on an inherited pipe that nobody writes to.

**It refuses a run it cannot read.** `record.Load` returns an empty map and no error for
a mistyped id or a wrong `-store`, and a report rendered from that is not an empty
report — it is a genuine-looking one reading `0 cycle(s) · 0 agent(s)`, which is exactly
the fingerprint `push-check` would then accept. Both halves of the gate refuse there, on
the same measurement.

**"No PR" is no longer asserted from a failed `gh`.** `pr-report.py` read
`rc, num, _ = sh("gh", "pr", "view", ...)` and branched on `rc != 0 or not num`, printing
"no PR yet" — one assertion standing in for three causes, with the only stream that says
which discarded into `_`. The advice it gave cannot fix the other two, and on the
author's machine `gh` has already failed with a keychain TLS error while `git push`
worked. This port asks `gh pr list --head <branch>`, which exits 0 with empty output for
"there are none" and non-zero only when gh itself failed, and reports five states
distinctly: a PR was found, gh answered that there is none, gh could not be run at all,
gh ran and failed, or there was no branch to ask about at all — a detached HEAD, an
unborn branch, or a git that could not answer, in which case gh is never invoked and
saying so is the whole point. gh's own stderr is carried for the two that are gh's, and
git's for the one that is git's. Every one of them still
keeps the body. Asserted by `TestThePostDiagnosisDivergence`, which also pins that the
retired Python conflated them.

With `-label` it applies `review:<convergence>` to the PR and removes the other three, so
two cannot stand at once — the at-a-glance half of the signal, so a reader need not open
a comment to learn whether the review finished. The convergence is derived from the same
read as the disclosure in the body, not taken as a flag. A label failure never fails a
publish that already succeeded, and the label is applied whenever a PR was found — including
when the comment itself failed, since nothing about `gh pr edit` depends on the comment having
landed. Every local copy carries a note saying the label is still owed, the successful-post
copy included; that is what the Python wrote and Step 0c reads, and narrowing it to the
deferred case would be a divergence rather than a fix.

Parity with `pr-report.py`, now retired, is gated against what it rendered for each
fixture before it went, frozen under `internal/report/testdata/oracle` — byte for byte,
and a missing frozen file fails the test rather than skipping it. The fixtures cover the
shapes no real row happens to hold — an executed gate with no `status` key, a roster entry
whose `id` is a number, a `findings: 0` that renders as *nothing* because `cell()` is
`str(text or "")`. Until the switch, every run in the author's live store was also rendered
through both and diffed; that comparison went with the oracle, because a store that gains
runs every day cannot be frozen. Where the two cannot agree, the divergence is a test
rather than a comment: `TestTheEnumeratedRenderDivergences` names each one with both
renderings, and `TestThreeShapesCrashTheOracleAndAreRefusedHere` pins three record shapes
on which the Python tracebacked — dropping the whole report — and this refuses at the
typed boundary with the offending field named.

## `looper record`

The run record's writer: every `runlog.py` subcommand, with the same records, refusals and
exit statuses for the flag syntax the skill uses (`--flag value` and `--flag=value`; argparse's
prefix abbreviations such as `--run` for `--run-id` are not accepted).

```text
looper record plan --tier <fast|full|skipped> --model <m> [--gates json] [--inputs json] ...
looper record cycle --run-id <id> --n <k> --applied <n> --agents <n> [--asked n] ...
looper record finish --run-id <id> --outcome <o> [--tier t] [--executed json] [--agents json] ...
looper record {convergence,show,nudge} --run-id <id>
looper record abandon --run-id <id> --missing <gates>
looper record skipped --reason <r>
looper record carried --from <sha> --to <sha> --how reviewed|skipped
looper record check [--head sha] [--session id] [--run-id id] [--force]   # exit 1: a run is open, or the store is unreadable (stdout empty)
looper record pending                                                     # this branch's pending reports
```

It appends to the same store (`$REVIEW_LOOP_RUNS`, default `~/.claude/review-loop/runs.jsonl`)
one record per line, in a single `O_APPEND` write. Its guards — is there a plan, is the run
already finished — read the store the way `runlog.load()` does, as merged dicts, so a malformed
row elsewhere in a shared store never blocks a write. Convergence, for `finish`'s warning and
the `convergence` subcommand, comes from the typed read half above, so it has one definition.

`TestWriteMatchesThePython` runs 37 scenarios through `looper record` against a seeded store and
requires the exit status, stdout, stderr and decoded records that `runlog.py` produced for the
same scenario, frozen under `internal/record/testdata/oracle` before its command line was deleted. It normalises timestamps (keeping their offset), run ids the implementation generated, the JSON decoder's own error text, the one message that names `runlog.py cycle` (the port names `looper record cycle`), and the wording of argparse's and Go's usage errors (exit 2 is still compared); the temp repo's path, git dir and commit; everything else is compared as written. The test pins the local zone to the -05:00 the goldens were captured in, so a port writing UTC timestamps fails on any machine. Each scenario is a sequence of
calls, because most of what the write half guards is a relationship between calls.

Where `runlog.py` crashes with a traceback, `looper record` decides instead, and each case has
its own test: a numeric agent id is recorded (the run as partial), a number or bool as `--agents` or `--escalations` is refused, and a reason that is not a
string — a list, a number — is refused, since stringified it slipped past the precedent ban. A
truthy non-object `--gates` or `--executed` is refused with a message where `runlog.py` dies
on `.items()`. A store holding invalid UTF-8 inside a JSON string is read, the bad bytes as U+FFFD, where
`runlog.py` dies decoding it on every subcommand that reads the store (`cycle`, `convergence`,
`finish`, `abandon`, `check`, `show`, `pending`) — one stray byte would brick the gate; a bad
byte outside a string makes its row unparseable, and that row is skipped as a torn line is. And where a finish carries a field the typed reader cannot decode (a numeric gate
reason, an escalations object), `finish` and `convergence` report `unknown` with the decode
error, where `runlog.py` derived a verdict from the dict: the push gate reads the typed record,
so that verdict is one it would never reproduce.

The flag syntax is Go's, not argparse's: unique prefixes are not expanded, `-flag` works as
well as `--flag`, and a value may begin with `-`. Lowercasing for the precedent ban is Go's, which
is stricter than Python's on `İ` and never looser. Records carry non-ASCII text as UTF-8 where `runlog.py` writes `\u` escapes; every JSON reader decodes the two to the same value.

`TestWriteConstantsMatchThePython` imports `runlog.py` and compares the copied constants directly: the precedent phrases, the default agent cap, and the outcome and tier vocabularies. `runlog.py` outlives its command line as a library that `learn.py` and `plan.py` import, so the two lists must stay one list now that the scenarios no longer run against it; one scenario sends every phrase in the port's list through `skipped`, and this test is what keeps that list `runlog.py`'s.

## `looper stats`

Reads the run record for what actually happens across runs: a port of the skill's
`review-stats.py`, with the same two consumers.

```text
looper stats                    # the cadence report; --repo <substring> filters it by repo path
looper stats --alarm            # plan.py at Step 0: repeat-offender gates, or nothing
```

The alarm raises a gate that did not complete, or was declined with a reason (`skipped`,
`pending`, `deferred`, `waived`), three times in the last 50 runs, and an abandoned run once
per run rather than once per gate it planned, with the abandon reasons grouped. Any other
status, `blocked` and `passed` included, is a failure to complete. A run is abandoned when it
says so, or when it is open and was planned by a session other than the one asking
(`CLAUDE_CODE_SESSION_ID`, else `AO_SESSION_ID`); with neither set, or for a run that named no
session, nothing is derived. The report splits runs into finished, abandoned, open, skipped and
carried, then tallies outcome, tiers, session kind and model, convergence and spend over the
finished runs, dropped gates, escalations, abandon reasons and unresolved asks. Both read the
store the push gate reads (`$REVIEW_LOOP_RUNS`), bounded by `$REVIEW_LOOP_TAIL` lines as
`runlog.py` is. Exit 0 for either report, 2 for a usage error, 1 for a store that exists and
cannot be read; plan.py prints the alarm only on 0. Flags are Go's, so `-alarm` works too, and
argparse's habits do not: no prefix abbreviations (`--al` is a usage error), `--repo -x` filters
on `-x`, and `--alarm=false` is the report. plan.py passes the exact `--alarm`.

`TestStatsMatchesThePython` runs 61 stores through `looper stats` and requires the exit status,
stdout and stderr that `review-stats.py` printed for the same store and environment, frozen
under `internal/stats/testdata/oracle` (captured at narthur/skills 839aa49, each under six hash
seeds to prove the fixture's output did not depend on one). The one normalisation is the
alarm's overflow line, which names the command that lists every reason: `review-stats.py`
there, `looper stats` here (`TestTheOverflowLinePointsAtLooper`). Usage errors are compared on
the exit status alone (`TestStatsUsageExitsLikeThePython`).

The divergences, each with its own test:

- **A run this build cannot read is named, not counted.** `looper stats` reads the record
  through the push gate's typed loader, so a run with ANY field that loader decodes in the wrong
type — including ones stats never prints, such as a cycle's `n`, an escalation's `reason` or an
integral float like `"agents":2.0` —
  a numeric gate status, a list for `gates`, `executed` or an outcome, a string count, a
  non-string `planned_at`, `repo`, `session_id`, `session_kind` or `abandoned_missing`, a
  run-level field on a phase that does not own it — is unreadable as a whole. It keeps its
  place in the order, so it still takes a slot in the alarm's window, and nothing is derived
  from it. The alarm adds one line, `N run(s) here could not be read, so no gate above counts
  them`, whenever one sits inside the window (an unreadable run outside it says nothing, so an
  old bad row does not speak at every Step 0); the report counts it in `runs:` and as
  `unreadable`, and lists each decode error at the end. On these shapes `review-stats.py` either
  coerced the value (`g did not complete (123)`, a list printed as `['g']`, a string `applied`
  read as not-zero, a non-dict escalation skipped) or died with a traceback in one or both
  modes. `TestShapesThisBuildCannotReadAreNamed` runs these and the two below, 17 shapes in
  both modes, with the oracle's answer for each frozen beside it.
- **A row whose `run_id` is a truthy non-string** (`7`, `true`, a list) belongs to no run here.
  `review-stats.py` keyed a run by the number and died on the list. The report counts such rows
  at the end; the alarm, which has no run to place in its window, is what it is without them.
- **A row that is valid JSON but not an object** is skipped as a torn line is, where
  `review-stats.py` died on `.get`.
- **A key that differs from `run_id` only in case** (`RUN_ID`, `Run_Id`) makes its run
  unreadable, where `review-stats.py`'s exact-key dict ignored the row. That is the push gate's
  refusal of case-folded keys (encoding/json matches field names case-insensitively, which was a
  forgery path), and stats inherits it rather than loosening a security check for parity.
- **JSON Go's decoder refuses** — a bare `NaN`, a lone UTF-16 surrogate — drops the row as a torn
  line is dropped, and a surrogate elsewhere reads as U+FFFD, which can change a sort or what
  falls in the alarm's window. `review-stats.py` accepted both and died printing a surrogate.
  The store's writer is `looper record` now, which writes neither.
- **No readable run to report** (a `--repo` filter that matches nothing, or a store holding only
  rows whose `run_id` is not a string) prints `no runs recorded` when the store truly holds none,
  as `review-stats.py` does, and `no runs this build can read` plus a count of those rows when it
  holds them — they cannot be shown to be in another repo. `review-stats.py` keyed such a run by
  the number and reported it (`TestARepoFilterMatchingNothingSaysNoRuns`). With every run unreadable, the
  tallies are left out rather than printed empty (`TestNoEmptyTallyWhenEveryRunIsUnreadable`).
- **Invalid UTF-8 in the store** is read, the bad bytes as U+FFFD, as every looper reader does;
  `review-stats.py` died decoding it, taking the Step 0 alarm with it
  (`TestInvalidUTF8IsReadNotFatal`).
- **An unreadable store** (not a missing one, which is "no runs recorded") exits 1 with
  `looper stats: cannot read the run store: …` where `review-stats.py` exited 1 with a traceback
  (`TestAnUnreadableStoreIsAnError`).
- **Ties between gates first dropped by the same run** print by name. `review-stats.py`
  collected them from a set, whose order is hash-seeded per process, so its order there changed
  between invocations (`TestGatesFirstDroppedByOneRunTieByName`).

## `looper threat-model` and `looper deferred`

The review-loop's two pinned ledgers, under `<git-common-dir>/info`: a port of the skill's
`threat-model.py` (Step 2b, and parsed by plan.py), `deferred.py` (Step 2a) and the `pins.py`
they share, in `internal/ledger`.

```text
looper threat-model   # review-loop-threat-model.md: {path, exists, claims, stale, broken_pins, uncited}
looper deferred       # review-loop-deferred.md: {path, exists, entries, stale, unpinned, broken_pins, unguarded}
```

Both make the same kind of statement — something true about the code at a commit, cited as
`[path:line @ sha]`, `[path:start-end @ sha]` or `[path @ sha]` — and a pin is **stale** when
`git log <sha>..HEAD -- <path>` lists anything, reported with those commits. A pin whose sha
names no commit here is in `broken_pins`: `git log` fails on it, which reads exactly like
"nothing moved", so without that list it would look checked and be unwatched. The threat model
counts each line holding `OBSERVED:` but no pin as `uncited`. The deferred list reads each line
containing `DEFERRED` as an entry, with the indented non-blank lines after it, names entries
with no pin (`unpinned`) and entries with no `Guard` anywhere in them (`unguarded`). Both run
in the working directory, as the Python did — there is no `--repo` — and take the ledger path
from `git rev-parse --git-common-dir`, so a linked worktree reads the main checkout's ledgers.
Outside a repo the path is `""` and the report says `exists: false`. Arguments are ignored, as
the Python ignored them; exit 0 for any report.

`TestLedgersMatchThePython` runs 63 scenarios through both commands and requires the exit
status, stdout and stderr the Python printed for the same repo, ledger and working directory,
byte for byte — key order, indentation, `ensure_ascii` escapes — frozen under
`internal/ledger/testdata/oracle` (captured at narthur/skills 8c3b0f4). The fixture repo is
built with a fixed identity, fixed author and committer dates and no global or system git
config, so its shas are the same on every machine and the goldens hold them as they are; the
one normalisation is the temp directory, which a worktree's absolute common dir prints. Parity
reaches Python's text model, not Go's: `\s` and `str.strip()` are Python's Unicode whitespace,
`\d` is Unicode `Nd` as of the Unicode 16.0 tables the oracle's Python carried (spelled out,
since go1.24's are 15.0), lines split where `str.splitlines()` splits, after universal newlines.

The divergences, each with its own test:

- **An unreadable ledger** (not a missing one, which is still `exists: false`) exits 1 with
  `looper <cmd>: cannot read <path>: …` and nothing on stdout. The Python caught every `OSError`
  as `exists: false`, so an unreadable deferred list reported nothing deferred and an unreadable
  threat model sent Step 2b to bootstrap over it; plan.py reads a non-zero exit as "run the
  update rather than assume clean" (`TestAnUnreadableLedgerIsAnError`).
- **A ledger that is not valid UTF-8** is read, each run of bad bytes as one U+FFFD, and the
  report is the Python's own report on the ledger with U+FFFD written there. The Python died
  with `UnicodeDecodeError`, losing the whole worklist to one bad byte in a prose line
  (`TestInvalidUTF8LedgerIsRead`).
- **`--selftest` as the first argument** exits 2 with a pointer to `go test ./internal/ledger/`.
  The Python ran its own checks and printed `ok`; looper has none to run, and printing `ok` would
  claim one did (`TestSelftestIsRefused`). Anywhere else it is ignored with the other arguments.
- **git output that is not UTF-8** — a commit subject under `i18n.logOutputEncoding latin1`,
  say — is read with U+FFFD for the bad bytes, as the ledger is. The Python died with
  `UnicodeDecodeError` on one commit subject (`TestNonUTF8GitOutputIsRead`).
- **A git common dir that is not UTF-8** exits 1 with a sentence. Decoded with U+FFFD it names
  no real file, so an existing ledger would read as missing; the Python died with
  `UnicodeDecodeError` (`TestNonUTF8CommonDirIsAnError`).
- **A NUL byte in a pin's path** exits 1 naming the pin's line. git cannot be asked about such a
  path, and a failed git reads as "nothing moved", so the pin would look fresh forever; the
  Python died with `ValueError` (`TestANULInAPinPathIsAnError`).

`.git/info` as a regular file is not a divergence: it is no ledger, `exists: false`, as the
Python said, and the unreadable rule above does not swallow it (`TestInfoAsAFileIsNoLedger`).

## `looper context`

The review-loop's Step 0, a port of the skill's `context.sh` in `internal/gather`. From the
working directory it resolves the base branch (the PR's base, else the repo default, else
`origin/HEAD`) and fetches it. It also loads the learnings file and works out whether a staleness
sweep is due, detects the test and lint commands, and sizes the branch diff. That size is given
three ways: raw, as review surface (lockfiles, files `.gitattributes` marks
`linguist-generated`, and whitespace-only lines are dropped, except in files where indentation
is syntax), and since the last commit the review gate stamped. It prints one JSON object that
`looper plan --context` reads.

```text
looper context > "$(git rev-parse --git-dir)/review-loop-context.json"
```

`TestContextMatchesTheShell` runs 45 scenarios through `looper context` and requires what
context.sh printed for the same fixture repo, stub `gh` and review store, byte for byte, frozen
under `internal/gather/testdata/oracle` (captured at narthur/skills 865f93d). The date is the one
normalisation.

The divergences, each with its own test:

- **Arguments are not ignored.** `-h` prints a usage line and any other argument is a usage error
  (exit 2); context.sh read none, so `context.sh -h` fetched and sized the whole branch
  (`TestArgumentsAreNotIgnored`).
- **package.json's `scripts` is read only as an object.** context.sh tested Python membership on
  whatever was there. A string of scripts matched `test` as a substring and answered `npm test`.
  A list of scripts, or a `lint` script that was a number or null, crashed it with no output at
  all. (A `lint` script that is a list or an object answers as Python's `in` does, which is
  parity.) A package.json using Python's JSON extensions, such as `NaN`, is not JSON, and reads as
  no scripts (`TestPackageScriptsAreAnObject`).
- **A Makefile that is not UTF-8 is still searched** for its `test:` target. context.sh's strict
  decode crashed on it, losing the whole context (`TestALatin1MakefileIsRead`).
- **Text that is not UTF-8 reaches the JSON with U+FFFD** for each bad byte, in the learnings and in
  the diff stat. context.sh turned each such byte into a lone-surrogate escape (`\udcXX`), which
  many JSON parsers refuse (`TestLatin1LearningsAreReplaced`). Learnings lines are matched as
  bytes. context.sh's `grep` and `sed` matched by locale, which on some platforms skipped a line
  holding invalid UTF-8 and took Unicode spaces as `[[:space:]]`.
- **Where context.sh died or said nothing, looper answers** (`TestCrashesAndSilencesAreDecided`):
  - A diff against `origin/<base>` that cannot be read (an unfetched or missing base) is named
    in `sizing_excluded`. context.sh printed 0 lines and no reason, which reads as an empty diff.
  - A base branch starting with `-` is refused (`base_branch: null`). `git fetch origin <base>`
    would read it as an option, and git accepts no such branch name.
  - No `HOME` means no review store. context.sh died under `set -u`, and a path relative to the
    repo would trust a store the repo ships.
  - A `Makefile` that is a directory has no test target. context.sh crashed on it.
  - Learnings too big to pass through the environment are read. context.sh's Python never
    started (exit 126).
  - A changed file whose name is not UTF-8 (with `core.quotePath=false`) gets context.sh's
    fallback: the raw count, with `sizing helper produced no count`. Decoding it with U+FFFD
    instead would have sized on and under-counted.
- **gh and `git fetch` give up after 30 seconds** (`TestAStalledProbeIsGivenUp`). context.sh waited
  on them forever, so a stalled gh hung Step 0 with nothing printed. What a timed-out probe printed
  stands, as for any other exit, and the base falls back past it when that is nothing. A gh that
  exits while a grandchild (a credential helper) still holds its pipes is cut off a second later.

## `looper plan`

The review-loop's Step 0b, a port of the skill's `plan.py` in `internal/plan`: which gates
run and the tier floor, from context.sh's JSON and four answers only the orchestrator can give.

```text
looper plan --model <orchestrator model> --logic yes|no --behavioral-goal yes|no \
            --runtime-change yes|no --attacker-reachable yes|no \
            [--spec-artifact yes|no] [--context <context.json>] [--dry-run]
```

It prints the plan as JSON (`json.dumps(indent=2)`, byte for byte) and the Step 0 alarm, if
any, on stderr, routed to `ao report --checkpoint` under `AO_SESSION_ID`. Unless `--dry-run`, it
then records the plan row and prints the run_id on the last line. The context is read from
`--context`, or stdin. The answers take `yes`, `y`, `true` or `1` in any case as yes, and
anything else as no, as plan.py's did. It asks `gh` whether the branch has a PR and whether the
repo has any, `git` for the remote and the branch's largest changed file, and reads the threat
model, the alarm and the record write in process: plan.py ran those as `~/.local/bin/looper`,
and the branches it needed for a missing looper cannot happen here.

`TestPlanMatchesThePython` runs 43 scenarios through `looper plan` and requires what plan.py
did over the same fixture repo, stub `gh` and `ao`, threat model and run store: the exit
status, stdout, stderr, what `ao` was asked and the plan row it recorded, frozen under
`internal/plan/testdata/oracle` (captured at narthur/skills 791c4d1, with plan.py running
looper v0.2.6's threat-model, stats and record). A run_id, the temp directory and the row's
timestamps are the only normalisations.

The divergences, each with its own test:

- **Error sentences are looper's own** and say `looper plan`. The exit statuses are plan.py's:
  1 for a context that cannot be read or parsed, 2 for a usage error. Nothing reaches stdout
  either way (`TestUsageAndInputErrorsExitAsThePythonDid`). `-h` prints Go's usage text to
  stdout and exits 0, where argparse printed its own (`TestHelpGoesToStdout`).
- **A context that is not an object, or a field of the wrong type,** exits 1 naming it. A count
  must be a whole number, a flag true or false, a name a string; `null` and a missing field
  read as plan.py's defaults, except a `null` `learnings_entries`, which plan.py printed as
  "None learnings entries" or crashed on, and which is an error here. plan.py read the JSON through `dict.get` and truthiness, so a list
  crashed it and a string where a count belongs passed through into the plan and the record.
  context.sh writes neither (`TestAWrongContextIsAnError`).
- **An abbreviated flag is a usage error.** argparse took any unambiguous prefix, such as
  `--log`; Go's flag package takes none (`TestAnAbbreviatedFlagIsRefused`).
- **A failed record write** passes the record's error and exit status on and then says nothing
  was recorded, as plan.py did; the sentence says to re-run `looper plan`
  (`TestAFailedRecordSaysSo`).
- **Changed files' names are read as bytes, whole.** plan.py read `git diff -z` in text mode,
  which turns a `\r` in a name into `\n`, and stripped it, which drops a leading space from the
  first name; either way that file was not found and counted as no lines, so a 900-line file
  could leave Agent #7 planned `skip`. A name that is not UTF-8 would fail text mode's strict
  decoding (not measured: APFS refuses such names) (`TestANameGitPrintsIsCounted`).
- **A largest-file scan that cannot run plans Agents #7 and #8,** and the reason names git's
  error. plan.py read a failed `git diff` (an unfetched base, say) as a 0-line file and planned
  #7 `skip` "under both floors" on a diff it had not measured (`TestAnUnmeasuredDiffPlansWide`).
- **A probe stops at its timeout** even when it leaves a grandchild holding its output open;
  plan.py's `subprocess.run` did, and a Go wait without `WaitDelay` would not. The scan's two
  `git` calls get plan.py's 15 seconds too (`TestATimedOutProbeStopsAtItsTimeout`). A probe
  that exits while a grandchild still holds its output is answered by its exit status and what
  it printed, a second after it exits; plan.py waited for the grandchild up to the timeout
  (`TestAProbeWithALingeringGrandchildIsAnswered`).

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

## `looper version`

Prints the module version `go install …@vX.Y.Z` stamped into the binary. A build from a
checkout depends on how it was built: `go build` prints a pseudo-version
(`v0.2.2-0.<date>-<hash>`, `+dirty` if modified); `go run` and `-buildvcs=false` print
`(devel)`. review-loop's Step 0 keys on the exit
status alone, not the string: a looper too old to have `version` (v0.2.1 and earlier, which
includes every one without `push-check` and `pr-report`) answers "unknown command" with
exit 2. That makes it a floor of v0.2.2, not a capability check — when Step 14 comes to need a
newer subcommand, Step 0 needs a real version comparison, or a probe for that subcommand.
