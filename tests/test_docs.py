"""The repo's own rule file stays readable by every agent that loads it."""
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
AGY_RULE_LIMIT = 24_000  # bytes; agy truncates a rule file past this


def test_claude_md_fits_agy():
    size = len((ROOT / "CLAUDE.md").read_bytes())
    assert size <= AGY_RULE_LIMIT, (
        f"CLAUDE.md is {size} bytes, over agy's {AGY_RULE_LIMIT}-byte rule-file limit, so agy agents lose its end: "
        "move detail to docs/design.md (its \"Code map\") and keep a gist in CLAUDE.md")


def test_agents_md_is_claude_md():
    agents = ROOT / "AGENTS.md"
    assert agents.is_symlink() and agents.resolve() == (ROOT / "CLAUDE.md").resolve()
