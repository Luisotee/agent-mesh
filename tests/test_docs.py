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


def test_no_identity_phrasing_for_agy():
    """agy's gateway answers a fake 429 RESOURCE_EXHAUSTED to any prompt holding second-person identity
    phrasing that names another assistant; agy loads every skill's description (and may read the bodies and the docs) on
    every turn. Say "in a Claude Code session", never address the reader as that assistant."""
    import re
    bad = re.compile(r"\byou\s+are\s+(" + "claude" + r"|agy|copilot|opencode|gemini|codex)\b", re.I)
    files = [p for p in ROOT.rglob("*.md") if not {".git", "node_modules"} & set(p.parts)]
    files += [*ROOT.glob(".claude-plugin/*.json")]
    hits = [f"{p.relative_to(ROOT)}:{text[:m.start()].count(chr(10)) + 1}"
            for p in files for text in [p.read_text()] for m in bad.finditer(text)]
    assert not hits, hits
