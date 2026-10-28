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
    assert not v.is_path("and/or") and not v.is_path("Claude/agy") and not v.is_path("1/2")
    assert v.is_path("~/x") and v.is_path("src/") and v.is_path("a/b/c") and v.is_path("run.json")
    assert not v.is_path("e.g.") and not v.is_path("1.5")


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

    (after / "sub" / "AGENTS.md").write_text(AFTER.replace("https://example.com/docs and ", ""))
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
    assert run(old, tmp_path).returncode != 0
