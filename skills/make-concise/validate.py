#!/usr/bin/env python3
"""Compare instruction files before and after a concise rewrite.

    validate.py BEFORE AFTER [--also FILE ...] [--keep TEXT ...] [--limit BYTES]

BEFORE and AFTER are two files, or two directories: every file under BEFORE is
compared with the file at the same relative path under AFTER. Text moved out of
a file counts as kept when it is in any AFTER file, or in an --also file (a new
document the detail moved to).

Errors (exit 1): a fenced block missing or changed; a marked block changed or
missing; an inline code span, URL, path or `@` import of BEFORE found in no
AFTER file; a --keep text that BEFORE has and AFTER lacks; a rules file
(CLAUDE.md, AGENTS.md, GEMINI.md) over --limit bytes. Warnings (exit 0): a bare
file name gone (`Next.js` looks like one), new code spans, URLs or paths, a
removed heading, a file that grew. Exit 2 is a usage error. Stdlib only.
"""
import argparse
import re
import sys
from dataclasses import dataclass, field
from pathlib import Path

AGY_RULE_LIMIT = 24_000  # bytes; agy truncates a rule file past this
RULE_FILES = {"claude.md", "agents.md", "gemini.md"}

FENCE_OPEN = re.compile(r"^\s*(`{3,}|~{3,})(.*)$")
MARKER = re.compile(
    r"<!--\s*(?:(?P<kw1>begin|end)\s*:\s*(?P<name1>[\w.-]+)|(?P<name2>[\w.-]+)\s*:\s*(?P<kw2>begin|end)\b)[^>]*?-->",
    re.IGNORECASE)
# a span may wrap across lines, but not across a blank line (a new paragraph)
CODE_SPAN = re.compile(r"(`+)(?!`)((?:(?!\n[ \t]*\n).)+?)(?<!`)\1(?!`)", re.DOTALL)
URL = re.compile(r"https?://[^\s<>()\[\]`'\"]+")
IMPORT = re.compile(r"(?:^|\s)@([~./\w-]\S*)", re.MULTILINE)
WORD = re.compile(r"[^\s()\[\]{}<>\"'`,;:!?*|]+")
EXT = re.compile(r"\.[A-Za-z][A-Za-z0-9]{0,5}$")
HEADING = re.compile(r"^ {0,3}#{1,6}\s+(.+?)\s*#*\s*$", re.MULTILINE)
TRAILING = ".,;:)!?"


def read(path):
    return Path(path).read_bytes().decode("utf-8")  # no newline translation: CRLF stays CRLF


def fenced_blocks(text):
    """(fence-less text, [block, ...]): each block is its opener line through its closer, no final newline."""
    lines = text.splitlines(keepends=True)
    prose, blocks, i = [], [], 0
    while i < len(lines):
        m = FENCE_OPEN.match(lines[i].rstrip("\r\n"))
        if not m or (m.group(1)[0] == "`" and "`" in m.group(2)):
            prose.append(lines[i])
            i += 1
            continue
        fence, j = m.group(1), i + 1
        close = re.compile(r"^\s*" + re.escape(fence[0]) + "{" + str(len(fence)) + r",}\s*$")
        while j < len(lines) and not close.match(lines[j].rstrip("\r\n")):
            j += 1
        blocks.append("".join(lines[i:j + 1]).rstrip("\r\n"))
        prose.append("\n")
        i = j + 1
    return "".join(prose), blocks


def marked_blocks(text):
    """{name: [text from a begin marker through its end marker, ...]}; unpaired markers are ignored."""
    blocks, open_at = {}, {}
    for m in MARKER.finditer(text):
        name = (m.group("name1") or m.group("name2")).lower()
        kind = (m.group("kw1") or m.group("kw2")).lower()
        if kind == "begin":
            open_at.setdefault(name, m.start())
        elif name in open_at:
            blocks.setdefault(name, []).append(text[open_at.pop(name):m.end()])
    return blocks


def path_kind(word):
    """'path' (has a slash: an error when lost), 'name' (a bare file name: a warning), or None."""
    if re.fullmatch(r"[\d./-]+", word) or word.startswith("@"):
        return None  # numbers, dates, ratios; imports are counted on their own
    if "/" not in word:
        if re.fullmatch(r"[\w.-]*[A-Za-z_-][\w-]*\.[A-Za-z][A-Za-z0-9]{0,5}", word) and not re.match(r"(e\.g|i\.e|etc|vs)\b", word):
            return "name"
        return None
    if (word.startswith(("~", "/", "./", "../")) or word.endswith("/")
            or EXT.search(word.rsplit("/", 1)[-1]) or word.count("/") >= 2):
        return "path"
    return None


def is_path(word):
    return path_kind(word.rstrip(".")) is not None


@dataclass
class Facts:
    fences: list = field(default_factory=list)
    marked: dict = field(default_factory=dict)
    spans: set = field(default_factory=set)
    urls: set = field(default_factory=set)
    paths: set = field(default_factory=set)
    names: set = field(default_factory=set)
    imports: set = field(default_factory=set)
    headings: list = field(default_factory=list)

    @property
    def tokens(self):
        """Everything a reader copies: a path put in backticks, or a span taken out of them, is still kept."""
        return self.spans | self.urls | self.paths | self.names | self.imports


def facts(text):
    marked = marked_blocks(text)
    for blocks in marked.values():  # compared whole, so left out of the other checks
        for block in blocks:
            text = text.replace(block, "\n")
    prose, fences = fenced_blocks(text)
    spans = {" ".join(m.group(2).split()) for m in CODE_SPAN.finditer(prose)}
    bare = URL.sub(" ", CODE_SPAN.sub(" ", prose))
    words = [w.rstrip(".") for w in WORD.findall(bare)]
    imports = {i.rstrip(TRAILING) for i in IMPORT.findall(bare)}
    return Facts(
        fences=fences, marked=marked, spans=spans,
        urls={u.rstrip(TRAILING) for u in URL.findall(text)},
        paths={w for w in words if path_kind(w) == "path"},
        names={w for w in words if path_kind(w) == "name"},
        imports={"@" + i for i in imports if "/" in i or EXT.search(i)},
        headings=[h.strip() for h in HEADING.findall(prose)],
    )


def compare(name, before, after, keep=(), limit=AGY_RULE_LIMIT, elsewhere=None):
    """(errors, warnings) for one file's rewrite. `elsewhere` is the Facts of every other AFTER text,
    where moved text counts as kept."""
    old, new = facts(before), facts(after)
    kept_tokens = new.tokens | (elsewhere.tokens if elsewhere else set())
    kept_fences = set(new.fences) | set(elsewhere.fences if elsewhere else ())
    errors, warnings = [], []
    for block in old.fences:
        if block not in kept_fences:
            errors.append(f"fenced block missing or changed: {block.splitlines()[0].strip()} {_preview(block)}")
    for marker, blocks in old.marked.items():
        now = new.marked.get(marker, [])
        if not now:
            errors.append(f"marked block {marker!r} missing")
        elif now != blocks:
            errors.append(f"marked block {marker!r} changed (must stay byte-identical)")
    for label, attr in (("inline code", "spans"), ("URL", "urls"), ("path", "paths"), ("@ import", "imports")):
        errors += [f"{label} missing: {item}" for item in sorted(getattr(old, attr) - kept_tokens)]
    warnings += [f"file name missing: {item}" for item in sorted(old.names - kept_tokens)]
    for label, attr in (("inline code", "spans"), ("URL", "urls"), ("path", "paths"), ("@ import", "imports")):
        warnings += [f"{label} added: {item}" for item in sorted(getattr(new, attr) - old.tokens)]
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


def merged(texts):
    """One Facts holding the tokens and fences of several texts."""
    out = Facts()
    for text in texts:
        f = facts(text)
        out.fences += f.fences
        for attr in ("spans", "urls", "paths", "names", "imports"):
            getattr(out, attr).update(getattr(f, attr))
    return out


def _preview(block):
    body = " ".join(block.split())
    return f"({body[:60]}…)" if len(body) > 60 else f"({body})"


def pairs(before, after):
    if not before.is_dir():
        return [(after.name, before, after)]
    return [(str(p.relative_to(before)), p, after / p.relative_to(before))
            for p in sorted(before.rglob("*")) if p.is_file()]


def main(argv=None):
    ap = argparse.ArgumentParser(description="Compare instruction files before and after a concise rewrite.")
    ap.add_argument("before", type=Path)
    ap.add_argument("after", type=Path)
    ap.add_argument("--also", action="append", default=[], type=Path, metavar="FILE",
                    help="another file the rewrite moved detail into: its text counts as kept (repeatable)")
    ap.add_argument("--keep", action="append", default=[], metavar="TEXT",
                    help="text that code matches on: must stay if BEFORE has it (repeatable)")
    ap.add_argument("--limit", type=int, default=AGY_RULE_LIMIT, help="max bytes for CLAUDE.md/AGENTS.md/GEMINI.md")
    args = ap.parse_args(argv)
    if not args.before.exists() or not args.after.exists():
        ap.error("BEFORE and AFTER must exist")
    if args.before.is_dir() != args.after.is_dir():
        ap.error("BEFORE and AFTER must both be files or both directories")
    if missing := [str(p) for p in args.also if not p.is_file()]:
        ap.error(f"--also file not found: {', '.join(missing)}")
    todo = pairs(args.before, args.after)
    afters = {new: read(new) for _, _, new in todo if new.is_file()}
    extra = [read(p) for p in args.also]
    failed = False
    for name, old, new in todo:
        if new not in afters:
            print(f"{name}: ERROR missing after the rewrite")
            failed = True
            continue
        before, after = read(old), afters[new]
        elsewhere = merged([t for p, t in afters.items() if p != new] + extra)
        errors, warnings = compare(name, before, after, args.keep, args.limit, elsewhere)
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
