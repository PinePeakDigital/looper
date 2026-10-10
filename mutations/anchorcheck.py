#!/usr/bin/env python3
"""Every .mut anchor must occur EXACTLY once in its target, as a substring.

Parsing is byte-for-byte internal/mutate/catalog.go's parseFile: the markers are
found with Index(text, "--- old\n") / Index(text, "--- new\n") ANYWHERE in the
file -- not anchored to a line start -- and the block is everything between them
with exactly ONE trailing newline stripped. Both details matter: an anchor may
legitimately end mid-line, with `--- new` appended straight onto it, which ten
entries here do. A shell `grep -cF` cannot do this job at all -- with a
multi-line anchor it counts matching LINES, not occurrences of the block.
"""
import glob, sys

OLD, NEW = "--- old\n", "--- new\n"
bad = 0
files = sorted(glob.glob("mutations/*.mut"))
for path in files:
    text = open(path).read()
    target = next((l.split(":", 1)[1].strip()
                   for l in text.splitlines() if l.startswith("target:")), None)
    o, n = text.find(OLD), text.find(NEW)
    if target is None or o < 0 or n < 0 or n < o:
        print(f"{path}: unparseable header or markers")
        bad += 1
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
print(f"{'FAIL' if bad else 'ok'}: {len(files)} entries, {bad} with a non-unique anchor")
sys.exit(1 if bad else 0)
