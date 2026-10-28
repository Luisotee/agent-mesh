#!/usr/bin/env python3
"""Compare instruction files before and after a concise rewrite.

    validate.py BEFORE AFTER [--keep TEXT ...] [--limit BYTES]

BEFORE and AFTER are two files, or two directories: every file under BEFORE is
compared with the file at the same relative path under AFTER.

Errors (exit 1): a fenced block or marked block missing or changed; an inline
code span, URL, path or `@` import of BEFORE missing from AFTER; a --keep text
that BEFORE has and AFTER lacks; a rules file (CLAUDE.md, AGENTS.md, GEMINI.md)
over --limit bytes. Warnings (exit 0): new code spans, URLs or paths, a removed
heading, a file that grew. Exit 2 is a usage error. Stdlib only.
"""
import argparse
import re
import sys
from dataclasses import dataclass, field
from pathlib import Path

AGY_RULE_LIMIT = 24_000  # bytes; agy truncates a rule file past this
RULE_FILES = {"claude.md", "agents.md", "gemini.md"}

FENCE_OPEN = re.compile(r"^ {0,3}(`{3,}|~{3,})(.*)$")
MARKER = re.compile(
    r"<!--\s*(?:(?P<kw1>begin|end)\s*:\s*(?P<name1>[\w.-]+)|(?P<name2>[\w.-]+)\s*:\s*(?P<kw2>begin|end)\b)[^>]*?-->",
    re.IGNORECASE)
CODE_SPAN = re.compile(r"(`+)(?!`)(.+?)(?<!`)\1(?!`)")
URL = re.compile(r"https?://[^\s<>()\[\]`'\"]+")
IMPORT = re.compile(r"(?:^|\s)(@[~./\w-][^\s]*)", re.MULTILINE)
WORD = re.compile(r"[^\s()\[\]{}<>\"'`,;:!?*|]+")
EXT = re.compile(r"\.[A-Za-z][A-Za-z0-9]{0,5}$")
HEADING = re.compile(r"^ {0,3}#{1,6}\s+(.+?)\s*#*\s*$", re.MULTILINE)


def fenced_blocks(text):
    """(fence-less text, [block, ...]) where each block is its opener line through its closer."""
    lines = text.splitlines(keepends=True)
    prose, blocks, i = [], [], 0
    while i < len(lines):
        m = FENCE_OPEN.match(lines[i].rstrip("\n"))
        if not m or (m.group(1)[0] == "`" and "`" in m.group(2)):
            prose.append(lines[i])
            i += 1
            continue
        fence, j = m.group(1), i + 1
        close = re.compile(r"^ {0,3}" + re.escape(fence[0]) + "{" + str(len(fence)) + r",}\s*$")
        while j < len(lines) and not close.match(lines[j].rstrip("\n")):
            j += 1
        blocks.append("".join(lines[i:j + 1]))
        prose.append("\n")
        i = j + 1
    return "".join(prose), blocks


def marked_blocks(text):
    """{name: text from the begin marker through the end marker}; unpaired markers are ignored."""
    blocks, open_at = {}, {}
    for m in MARKER.finditer(text):
        name = (m.group("name1") or m.group("name2")).lower()
        kind = (m.group("kw1") or m.group("kw2")).lower()
        if kind == "begin":
            open_at.setdefault(name, m.start())
        elif name in open_at:
            blocks[name] = text[open_at.pop(name):m.end()]
    return blocks


def is_path(word):
    word = word.rstrip(".")
    if "/" not in word and not EXT.search(word):
        return False
    if re.fullmatch(r"[\d./-]+", word) or word.startswith("@"):
        return False  # numbers, dates, ratios; imports are counted on their own
    if "/" not in word:
        return bool(re.fullmatch(r"[\w.-]*[A-Za-z_-][\w-]*\.[A-Za-z][A-Za-z0-9]{0,5}", word)) and not re.match(r"(e\.g|i\.e|etc|vs)\b", word)
    return (word.startswith(("~", "/", "./", "../")) or word.endswith("/")
            or bool(EXT.search(word.rsplit("/", 1)[-1])) or word.count("/") >= 2)


@dataclass
class Facts:
    fences: list = field(default_factory=list)
    marked: dict = field(default_factory=dict)
    spans: set = field(default_factory=set)
    urls: set = field(default_factory=set)
    paths: set = field(default_factory=set)
    imports: set = field(default_factory=set)
    headings: list = field(default_factory=list)


def facts(text):
    marked = marked_blocks(text)
    for block in marked.values():  # compared whole, so left out of the other checks
        text = text.replace(block, "\n")
    prose, fences = fenced_blocks(text)
    spans = {m.group(2).strip() for m in CODE_SPAN.finditer(prose)}
    bare = CODE_SPAN.sub(" ", prose)
    urls = {u.rstrip(".,;:") for u in URL.findall(text)}
    bare_no_urls = URL.sub(" ", bare)
    return Facts(
        fences=fences, marked=marked, spans=spans, urls=urls,
        paths={w.rstrip(".") for w in WORD.findall(bare_no_urls) if is_path(w)},
        imports=set(IMPORT.findall(bare_no_urls)),
        headings=[h.strip() for h in HEADING.findall(prose)],
    )


def compare(name, before, after, keep=(), limit=AGY_RULE_LIMIT):
    """(errors, warnings) for one file's rewrite."""
    old, new = facts(before), facts(after)
    errors, warnings = [], []
    for block in old.fences:
        if block not in new.fences:
            errors.append(f"fenced block missing or changed: {block.splitlines()[0].strip()} {_preview(block)}")
    for marker, block in old.marked.items():
        if marker not in new.marked:
            errors.append(f"marked block {marker!r} missing")
        elif new.marked[marker] != block:
            errors.append(f"marked block {marker!r} changed (must stay byte-identical)")
    for label, attr in (("inline code", "spans"), ("URL", "urls"), ("path", "paths"), ("@ import", "imports")):
        gone = getattr(old, attr) - getattr(new, attr)
        errors += [f"{label} missing: {item}" for item in sorted(gone)]
        added = getattr(new, attr) - getattr(old, attr)
        warnings += [f"{label} added: {item}" for item in sorted(added)]
    for text in keep:
        if text in before and text not in after:
            errors.append(f"kept text missing: {text}")
    size = len(after.encode())
    if Path(name).name.lower() in RULE_FILES and size > limit:
        errors.append(f"{size} bytes, over the {limit}-byte limit (agy truncates a rules file past it)")
    warnings += [f"heading removed: {h}" for h in old.headings if h not in new.headings]
    if size > len(before.encode()):
        warnings.append("the file grew")
    return errors, warnings


def _preview(block):
    body = " ".join(block.split())
    return f"({body[:60]}…)" if len(body) > 60 else f"({body})"


def pairs(before, after):
    if before.is_dir() != after.is_dir():
        raise SystemExit("validate.py: BEFORE and AFTER must both be files or both directories")
    if not before.is_dir():
        return [(after.name, before, after)]
    return [(str(p.relative_to(before)), p, after / p.relative_to(before))
            for p in sorted(before.rglob("*")) if p.is_file()]


def main(argv=None):
    ap = argparse.ArgumentParser(description="Compare instruction files before and after a concise rewrite.")
    ap.add_argument("before", type=Path)
    ap.add_argument("after", type=Path)
    ap.add_argument("--keep", action="append", default=[], metavar="TEXT",
                    help="text that code matches on: must stay if BEFORE has it (repeatable)")
    ap.add_argument("--limit", type=int, default=AGY_RULE_LIMIT, help="max bytes for CLAUDE.md/AGENTS.md/GEMINI.md")
    args = ap.parse_args(argv)
    if not args.before.exists() or not args.after.exists():
        ap.error("BEFORE and AFTER must exist")
    failed = False
    for name, old, new in pairs(args.before, args.after):
        if not new.is_file():
            print(f"{name}: ERROR missing after the rewrite")
            failed = True
            continue
        before, after = old.read_text(), new.read_text()
        errors, warnings = compare(name, before, after, args.keep, args.limit)
        b, a = len(before.encode()), len(after.encode())
        print(f"{name}: {b} -> {a} bytes ({(a - b) / b * 100 if b else 0:+.0f}%)")
        for e in errors:
            print(f"  ERROR {e}")
        for w in warnings:
            print(f"  warn  {w}")
        failed |= bool(errors)
    return 1 if failed else 0


if __name__ == "__main__":
    sys.exit(main())
