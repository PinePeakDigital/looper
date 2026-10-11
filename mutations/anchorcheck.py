#!/usr/bin/env python3
"""Every .mut parses, and every anchor occurs EXACTLY once in its target.

Parsing is byte-for-byte internal/mutate/catalog.go's parseFile: the markers are
found with Index(text, "--- old\n") / Index(text, "--- new\n") ANYWHERE in the
file -- not anchored to a line start -- and the block is everything between them
with exactly ONE trailing newline stripped. Both details matter: an anchor may
legitimately end mid-line, with `--- new` appended straight onto it, which ten
entries here do. A shell `grep -cF` cannot do this job at all -- with a
multi-line anchor it counts matching LINES, not occurrences of the block.

The HEADER is parsed by the same rule for the same reason. catalog.go reads every
non-blank, non-`#` line before `--- old` as `key: value` and rejects any key it
does not know, so a `why:` whose prose is wrapped onto a second line makes that
line a header -- and the whole catalog then fails to load. Nothing else sees it:
`go test ./...` does not parse these files, and `looper mutate` finds it only
after the fifteen minutes this script exists to avoid spending. Measured: the
entry rewritten in this commit failed exactly that way in a verification clone.
"""
import glob, sys

OLD, NEW = "--- old\n", "--- new\n"
KEYS = {"target", "verify", "why", "expect"}  # catalog.go's switch, same spellings
bad = 0
files = sorted(glob.glob("mutations/*.mut"))
for path in files:
    text = open(path).read()
    o, n = text.find(OLD), text.find(NEW)
    if o < 0 or n < 0 or n < o:
        print(f"{path}: missing or out-of-order --- old / --- new markers")
        bad += 1
        continue
    header, target, why, verify = {}, None, None, []
    for line in text[:o].split("\n"):
        line = line.strip()
        if not line or line.startswith("#"):
            continue
        key, sep, val = line.partition(":")
        if not sep:
            print(f"{path}: header line is not `key: value`: {line!r}")
            bad += 1
            break
        key = key.strip()
        if key not in KEYS:
            print(f"{path}: unknown header {key!r} — a wrapped `why:` does this")
            bad += 1
            break
        header.setdefault(key, val.strip())
        if key == "verify":
            verify.append(val.strip())
    else:
        target, why = header.get("target"), header.get("why")
        if not target or not why or not verify:
            print(f"{path}: missing target:/why:/verify:")
            bad += 1
            target = None
    if target is None:
        continue
    block = text[o + len(OLD):n]
    if block.endswith("\n"):
        block = block[:-1]
    if not block:
        print(f"{path}: empty old block")
        bad += 1
        continue
    try:
        src = open(target).read()
    except OSError as e:
        print(f"{path}: cannot read target: {e}")
        bad += 1
        continue
    c = src.count(block)
    if c != 1:
        print(f"{path}: anchor occurs {c} time(s) in {target}")
        bad += 1
print(f"{'FAIL' if bad else 'ok'}: {len(files)} entries, {bad} unparseable or with a non-unique anchor")
sys.exit(1 if bad else 0)
