#!/usr/bin/env python3
"""Every .mut parses the way catalog.go parses it, and every anchor occurs EXACTLY once.

This exists because the only other thing that notices either problem is `looper mutate`,
fifteen minutes into CI, and it notices by failing to load the catalog AT ALL -- one bad
entry takes every other entry's guard with it. `go test ./...` does not parse these files.

Everything here is done in BYTES, and that is the whole design. catalog.go reads the file
with os.ReadFile and mutate.go counts the anchor with bytes.Count, so any decoding this
script does is a divergence. Text mode cost two real defects, both in the direction where
this script says ok and the catalog is unloadable:

  * universal newlines rewrite \\r\\n to \\n before the script sees a byte, so a CRLF .mut
    passed here and gave `no "--- old" section` from ParseCatalog -- measured. A CRLF
    TARGET with a multi-line anchor was worse: text mode counted 1, bytes.Count counts 0,
    so the entry would score Stale after the full run.
  * a latin-1 byte in a .mut or in its target raised UnicodeDecodeError, which is a
    traceback and a red CI step where catalog.go accepts the file.

Bytes also happen to settle one Go/Python mismatch for free: Go's strings.TrimSpace trims
by unicode.IsSpace, which does NOT include \\x1c, while Python's str.strip() does -- so
`target\\x1c:` diverged in text mode. bytes.strip() trims only ASCII whitespace, like Go.

The MARKERS are found with Index(text, "--- old\\n") ANYWHERE in the file -- not anchored to
a line start -- and the block is everything between them with exactly ONE trailing newline
stripped. Both details matter: an anchor may legitimately end mid-line with `--- new`
appended straight onto it, which ten entries here do. A shell `grep -cF` cannot do this job
at all: with a multi-line anchor it counts matching LINES, not occurrences of the block.

The HEADER follows the same source. catalog.go reads every non-blank, non-`#` line before
`--- old` as `key: value`, rejects any key it does not know, takes the LAST value for each
key except `verify:` which it appends, and requires target/why/verify. A `why:` whose prose
wraps onto a second line therefore makes that line a header, and the catalog dies.

Discovery matches filepath.WalkDir, which is what ParseCatalog uses: os.walk, so dot-files
and dot-directories are included. glob.glob excludes anything beginning with `.` unless
include_hidden=True, which left the recursion half-closed -- measured, `mutations/._a.mut`
(the AppleDouble sidecar tar and FAT copies create) killed the catalog while this printed
ok, and `mutations/.sub/one.mut` was loaded and run by the real runner unchecked. Walking
also skips directories named `*.mut`, which WalkDir skips via !d.IsDir() and glob returned
as a file (IsADirectoryError).
"""
import os, sys

OLD, NEW = b"--- old\n", b"--- new\n"
KEYS = {b"target", b"verify", b"why", b"expect"}  # catalog.go's switch, same spellings
bad = 0

files = sorted(
    os.path.join(dirpath, name)
    for dirpath, _, names in os.walk("mutations")
    for name in names
    if name.endswith(".mut")
)
if not files:
    # Which of the two, named rather than guessed: the first version of this message said
    # "the directory or the glob is gone" and printed it for a tree where both were fine
    # and ParseCatalog loaded an entry (a .mut under a dot-directory, which glob skipped).
    if not os.path.isdir("mutations"):
        print("FAIL: there is no mutations/ directory here — wrong working directory?")
    else:
        print("FAIL: mutations/ exists but holds no .mut file at any depth")
    sys.exit(1)

for path in files:
    try:
        text = open(path, "rb").read()
    except OSError as e:
        print(f"{path}: cannot read: {e}")
        bad += 1
        continue
    o, n = text.find(OLD), text.find(NEW)
    if o < 0 or n < 0 or n < o:
        print(f"{path}: missing or out-of-order --- old / --- new markers")
        bad += 1
        continue
    header, target, verify = {}, None, []
    for line in text[:o].split(b"\n"):
        line = line.strip()
        if not line or line.startswith(b"#"):
            continue
        key, sep, val = line.partition(b":")
        if not sep:
            print(f"{path}: header line is not `key: value`: {line!r}")
            bad += 1
            break
        key = key.strip()
        if key not in KEYS:
            print(f"{path}: unknown header {key!r} — a wrapped `why:` does this")
            bad += 1
            break
        header[key] = val.strip()  # LAST wins, as catalog.go assigns
        if key == b"verify":
            verify.append(val.strip())  # appended, as catalog.go appends
    else:
        target = header.get(b"target")
        if not target or not header.get(b"why") or not verify:
            print(f"{path}: missing target:/why:/verify:")
            bad += 1
            target = None
    if target is None:
        continue
    block = text[o + len(OLD):n]
    if block.endswith(b"\n"):
        block = block[:-1]
    new = text[n + len(NEW):]
    if new.endswith(b"\n"):
        new = new[:-1]
    if not block:
        print(f"{path}: empty old block")
        bad += 1
        continue
    # catalog.go rejects old == new ("so it mutates nothing"), and that error aborts
    # ParseCatalog for every entry. Tested after the empty-old case, in catalog.go's order.
    if block == new:
        print(f"{path}: old and new are identical, so it mutates nothing")
        bad += 1
        continue
    name = target.decode("utf-8", "replace")
    try:
        src = open(target, "rb").read()
    except OSError as e:
        print(f"{path}: cannot read target: {e}")
        bad += 1
        continue
    c = src.count(block)
    if c != 1:
        print(f"{path}: anchor occurs {c} time(s) in {name}")
        bad += 1
print(f"{'FAIL' if bad else 'ok'}: {len(files)} entries, {bad} that catalog.go would reject, "
      f"whose target is unreadable, or whose anchor is not unique")
sys.exit(1 if bad else 0)
