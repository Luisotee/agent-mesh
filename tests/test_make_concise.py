"""The make-concise skill's validator: what a rewrite may and may not lose."""
import importlib.util
import subprocess
import sys
from pathlib import Path

import pytest

SCRIPT = Path(__file__).resolve().parent.parent / "skills" / "make-concise" / "validate.py"
_spec = importlib.util.spec_from_file_location("make_concise_validate", SCRIPT)
v = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(v)

BEFORE = """# Rules

It is important to note that you should always run `uvx pytest -q tests` before you commit, because CI runs it too.
See https://example.com/docs for details, and the notes in docs/design.md.

```bash
./install.sh --uninstall
```

@docs/extra.md

<!-- BEGIN:nextjs-agent-rules -->
This is NOT the Next.js you know.
<!-- END:nextjs-agent-rules -->

## Style

Use plain sentences.
"""

AFTER = """# Rules

Always run `uvx pytest -q tests` before committing: CI runs it too.
Details: https://example.com/docs and docs/design.md.

```bash
./install.sh --uninstall
```

@docs/extra.md

<!-- BEGIN:nextjs-agent-rules -->
This is NOT the Next.js you know.
<!-- END:nextjs-agent-rules -->

## Style

Use plain sentences.
"""


def check(after, before=BEFORE, name="CLAUDE.md", **kw):
    return v.compare(name, before, after, **kw)


def test_a_faithful_rewrite_passes():
    errors, warnings = check(AFTER)
    assert errors == [] and warnings == []


def test_a_changed_fenced_block_fails():
    errors, _ = check(AFTER.replace("--uninstall\n```", "\n```"))
    assert any("fenced block" in e for e in errors)


def test_a_dropped_code_span_fails():
    errors, _ = check(AFTER.replace("`uvx pytest -q tests`", "the tests"))
    assert errors == ["inline code missing: uvx pytest -q tests"]


def test_a_dropped_url_fails():
    errors, _ = check(AFTER.replace("https://example.com/docs and ", ""))
    assert errors == ["URL missing: https://example.com/docs"]


def test_a_dropped_prose_path_fails():
    errors, _ = check(AFTER.replace(" and docs/design.md", ""))
    assert errors == ["path missing: docs/design.md"]


def test_a_dropped_import_fails():
    errors, _ = check(AFTER.replace("@docs/extra.md\n", ""))
    assert errors == ["@ import missing: @docs/extra.md"]


def test_slash_words_are_not_paths():
    k = v.path_kind
    assert k("and/or") is k("Claude/agy") is k("1/2") is k("e.g") is k("1.5") is k("@a/b.md") is None
    assert k("~/x") == k("src/") == k("a/b/c") == k("./x") == k("docs/x.md") == "path"
    assert k("run.json") == k("Next.js") == "name"


@pytest.mark.parametrize("begin,end", [
    ("<!-- BEGIN:nextjs-agent-rules -->", "<!-- END:nextjs-agent-rules -->"),
    ("<!-- agent-mesh:begin (managed by agent-mesh/install.sh) -->", "<!-- agent-mesh:end -->"),
])
def test_a_marked_block_must_stay_byte_identical(begin, end):
    before = f"Intro.\n\n{begin}\nKeep   this.\n{end}\n"
    assert check(before, before=before) == ([], [])
    errors, _ = check(before.replace("Keep   this", "Keep this"), before=before)
    assert len(errors) == 1 and "changed" in errors[0]
    errors, _ = check("Intro.\n", before=before)
    assert len(errors) == 1 and "missing" in errors[0]


def test_kept_text_must_stay():
    before = BEFORE.replace("Use plain sentences.", "You are agent (worker) in run x. Use plain sentences.")
    errors, _ = check(AFTER, before=before, keep=["You are agent (worker) in run"])
    assert errors == ["kept text missing: You are agent (worker) in run"]
    # a --keep text the old file never had is no error
    assert check(AFTER, keep=["absent everywhere"])[0] == []


def test_size_limit_applies_to_rules_files_only():
    big = AFTER + "x" * 200
    assert any("over the 100-byte limit" in e for e in check(big, name="docs/AGENTS.md", limit=100)[0])
    assert check(big, name="protocol/ORCHESTRATE.md", limit=100)[0] == []


def test_merging_duplicates_passes_and_new_items_only_warn():
    before = "Run `make`.\n\nAgain: run `make`.\n"
    assert check("Run `make`.\n", before=before) == ([], [])
    errors, warnings = check("Run `make` or `make all` (~/notes/).\n", before=before)
    assert errors == []
    assert "inline code added: make all" in warnings and "path added: ~/notes/" in warnings
    assert "the file grew" in warnings


def test_a_removed_heading_warns():
    errors, warnings = check(AFTER.replace("## Style\n\n", ""))
    assert errors == [] and warnings == ["heading removed: Style"]


def run(*args):
    return subprocess.run([sys.executable, str(SCRIPT), *map(str, args)], capture_output=True, text=True)


def test_cli_compares_directories(tmp_path):
    before, after = tmp_path / "before", tmp_path / "repo"
    for root, text in ((before, BEFORE), (after, AFTER)):
        (root / "sub").mkdir(parents=True)
        (root / "CLAUDE.md").write_text(text)
        (root / "sub" / "AGENTS.md").write_text(text)
    out = run(before, after)
    assert out.returncode == 0, out.stdout
    assert "CLAUDE.md:" in out.stdout and "sub/AGENTS.md:" in out.stdout

    # a URL dropped from one file but still in another AFTER file counts as kept (C1)
    (after / "sub" / "AGENTS.md").write_text(AFTER.replace("https://example.com/docs and ", ""))
    assert run(before, after).returncode == 0
    (after / "CLAUDE.md").write_text(AFTER.replace("https://example.com/docs and ", ""))
    out = run(before, after)
    assert out.returncode == 1 and "ERROR URL missing" in out.stdout

    (after / "sub" / "AGENTS.md").unlink()
    out = run(before, after)
    assert out.returncode == 1 and "sub/AGENTS.md: ERROR missing" in out.stdout


def test_cli_files_and_usage(tmp_path):
    old, new = tmp_path / "old.md", tmp_path / "CLAUDE.md"
    old.write_text(BEFORE)
    new.write_text(AFTER)
    assert run(old, new).returncode == 0
    assert run(old, new, "--limit", "10").returncode == 1
    assert run(old, tmp_path / "nope.md").returncode == 2
    assert run(old, tmp_path).returncode == 2  # file vs directory is a usage error (C12)
    assert run(old, new, "--also", tmp_path / "nope.md").returncode == 2


def test_also_counts_detail_moved_to_a_new_doc(tmp_path):
    """C1: detail moved out of a rules file into another document is kept, not lost."""
    old, new, doc = tmp_path / "old.md", tmp_path / "CLAUDE.md", tmp_path / "design.md"
    old.write_text(BEFORE)
    new.write_text(AFTER.replace("```bash\n./install.sh --uninstall\n```\n", "See design.md.\n")
                   .replace("`uvx pytest -q tests`", "the tests"))
    doc.write_text("Run `uvx pytest -q tests`.\n\n```bash\n./install.sh --uninstall\n```\n")
    out = run(old, new)
    assert out.returncode == 1 and "fenced block" in out.stdout and "inline code missing" in out.stdout
    out = run(old, new, "--also", doc)
    assert out.returncode == 0, out.stdout


def test_a_path_put_in_backticks_is_kept():
    """C2: spans and paths are one set of kept tokens."""
    assert check(AFTER.replace("and docs/design.md", "and `docs/design.md`"))[0] == []
    before = "Run `make` in src/app/ first.\n"
    assert check("Run make in `src/app/` first.\n", before=before)[0] == ["inline code missing: make"]


def test_a_code_span_may_wrap_across_lines():
    """C3: a hard-wrapped span is one span, with its inner whitespace normalised."""
    before = "Run `uvx pytest\n  -q tests` and then `make`.\n"
    assert check("Run `uvx pytest -q tests`, then `make`.\n", before=before) == ([], [])
    # but not across a paragraph break
    assert "b c" not in v.facts("a `b\n\nc` d\n").spans


def test_crlf_is_read_as_is(tmp_path):
    """C4, H: CRLF isn't translated, so sizes are real; a CRLF -> LF rewrite is one warning, not a changed block."""
    block = "<!-- BEGIN:x -->\r\nkeep\r\n<!-- END:x -->"
    old, new = tmp_path / "old.md", tmp_path / "AGENTS.md"
    old.write_bytes(f"Intro.\r\n{block}\r\n".encode())
    new.write_bytes(f"Intro.\r\n{block}\r\n".encode())
    out = run(old, new)
    assert out.returncode == 0 and f"{len(old.read_bytes())} -> {len(new.read_bytes())} bytes" in out.stdout
    new.write_bytes(f"Intro.\n{block.replace(chr(13), '')}\n".encode())
    out = run(old, new)
    assert out.returncode == 0 and "warn  line endings changed (CRLF -> LF)" in out.stdout
    assert f"-> {len(new.read_bytes())} bytes" in out.stdout


def test_crlf_to_lf_keeps_fences_and_spans():
    """H, A: fences and spans compare across line endings; a CRLF blank line still ends a span."""
    before = "Run `make\r\n  all`.\r\n\r\n```\r\nmake\r\n```\r\n"
    errors, warnings = check(before.replace("\r\n", "\n"), before=before)
    assert errors == [] and warnings == ["line endings changed (CRLF -> LF)"]
    assert "b c" not in v.facts("a `b\r\n\r\nc` d\r\n").spans


def test_a_span_does_not_pair_across_list_items_or_headings():
    """B: a stray backtick in one list item doesn't swallow the next item."""
    f = v.facts("- item ` one\n- item two `x`\n# Head ` y\n> quote `z`\n")
    assert f.spans == {"x", "z"}


def test_a_reindented_fence_is_unchanged():
    """C: a fence moved in or out of a list item is the same block."""
    before = "1. Run:\n\n   ```bash\n   make all\n   ```\n"
    assert check("Run:\n\n```bash\nmake all\n```\n", before=before)[0] == []


def test_spaces_inside_a_span_are_compared():
    """E: only a line break (and its indent) inside a span is collapsed."""
    assert v.facts("`a  b` and `c\n   d`").spans == {"a  b", "c d"}
    assert check("`a b`\n", before="`a  b`\n")[0] == ["inline code missing: a  b"]


def test_an_invented_file_name_warns():
    """F."""
    assert "file name added: setup.cfg" in check(AFTER + "See setup.cfg.\n")[1]


def test_dedup_into_a_file_that_had_it_warns_and_moves_dont_add(tmp_path):
    """D, G: a token deleted where another file already had it is 'kept only in'; a token moved
    between rewritten files is neither missing nor added."""
    before, after = tmp_path / "before", tmp_path / "after"
    before.mkdir(), after.mkdir()
    (before / "AGENTS.md").write_text("Run `make` and `lint`.\n")
    (before / "CLAUDE.md").write_text("Run `make`. See docs/a.md.\n")
    (after / "AGENTS.md").write_text("Run `lint`. See docs/a.md.\n")
    (after / "CLAUDE.md").write_text("Run `make`.\n")
    out = run(before, after)
    assert out.returncode == 0, out.stdout
    assert "warn  inline code kept only in CLAUDE.md: make" in out.stdout
    assert "added" not in out.stdout and "missing" not in out.stdout


def test_a_file_that_is_not_utf8_is_an_error(tmp_path):
    """J."""
    old, new = tmp_path / "old.md", tmp_path / "CLAUDE.md"
    old.write_text("hi\n")
    new.write_bytes(b"caf\xe9\n")
    out = run(old, new)
    assert out.returncode == 1 and "ERROR not UTF-8" in out.stdout and "Traceback" not in out.stderr


def test_every_marked_block_of_a_name_is_checked():
    """C5: two blocks with one name: a change to the first one is caught."""
    blk = "<!-- BEGIN:x -->\n{}\n<!-- END:x -->\n"
    before = blk.format("one") + "middle\n" + blk.format("two")
    assert check(before, before=before) == ([], [])
    errors, _ = check(blk.format("ONE") + "middle\n" + blk.format("two"), before=before)
    assert errors == ["marked block 'x' changed (must stay byte-identical)"]


def test_an_indented_fence_is_a_fence():
    """C6: a fence nested in a list item (indented 4+ spaces) is compared too."""
    before = "1. Run:\n\n       ```bash\n       make all\n       ```\n"
    assert v.fenced_blocks(before)[1] == ["```bash\nmake all\n```"]  # dedented by the opener's indent
    errors, _ = check("1. Run:\n\n       ```bash\n       make\n       ```\n", before=before)
    assert len(errors) == 1 and "fenced block" in errors[0]


def test_a_final_fence_without_a_trailing_newline_is_unchanged():
    """C7: the newline after the closing fence isn't part of the block."""
    before = "Run:\n\n```\nmake\n```\n"
    assert check(before.rstrip("\n"), before=before)[0] == []


def test_imports_lose_trailing_punctuation_and_mentions_are_not_imports():
    """C8: `@docs/x.md.` is the import `@docs/x.md`; `@alice` is a mention."""
    f = v.facts("See @docs/x.md. Ask @alice, or @team (see @notes/y.md).\n")
    assert f.imports == {"@docs/x.md", "@notes/y.md"}
    assert check("@docs/x.md\n", before="Read @docs/x.md, first.\n")[0] == []


def test_a_dropped_bare_name_only_warns():
    """C9: `Next.js`, `github.com`, `os.path` look like file names: losing one is a warning."""
    before = "This is Next.js on github.com, using os.path and run.json in src/x.py.\n"
    errors, warnings = check("Uses src/x.py.\n", before=before)
    assert errors == []
    assert {"file name missing: Next.js", "file name missing: github.com",
            "file name missing: os.path", "file name missing: run.json"} <= set(warnings)
    assert check("Uses it.\n", before=before)[0] == ["path missing: src/x.py"]
