"""The repo's own rule files stay readable by every agent that loads them."""
import subprocess
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
AGY_RULE_LIMIT = 24_000  # bytes; agy truncates a rule file past this
PARTS = ("daemon", "proto", "desktop", "android")  # each has its own CLAUDE.md, pointed at by the root's


def rules_files(*patterns: str) -> list[Path]:
    """Tracked and new (not ignored) files matching `patterns`, so a package's own CLAUDE.md under node_modules/
    or build/ doesn't count and a rules file not yet committed does."""
    out = subprocess.run(["git", "ls-files", "-z", "--cached", "--others", "--exclude-standard", *patterns],
                         cwd=ROOT, capture_output=True, text=True, check=True).stdout
    return sorted({ROOT / name for name in out.split("\0") if name and (ROOT / name).exists()})


def test_rules_files_fit_agy():
    files = rules_files("*CLAUDE.md", "*AGENTS.md", "*GEMINI.md")
    assert ROOT / "CLAUDE.md" in files and all(ROOT / part / "CLAUDE.md" in files for part in PARTS)
    for path in files:
        size = len(path.read_bytes())
        assert size <= AGY_RULE_LIMIT, (
            f"{path.relative_to(ROOT)} is {size} bytes, over agy's {AGY_RULE_LIMIT}-byte rule-file limit, so agy "
            "agents lose its end: move detail to docs/design.md (its \"Code map\") or the part's README and keep a gist")


def test_agents_md_is_claude_md():
    for claude in rules_files("*CLAUDE.md"):
        agents = claude.with_name("AGENTS.md")
        assert agents.is_symlink() and agents.resolve() == claude.resolve(), \
            f"{agents.relative_to(ROOT)} must be a symlink to the CLAUDE.md beside it"


def test_root_points_at_each_part():
    text = (ROOT / "CLAUDE.md").read_text()
    missing = [part for part in PARTS if f"`{part}/CLAUDE.md`" not in text]
    assert not missing, f"CLAUDE.md's layout list names no rules file for {missing}"


def test_no_identity_phrasing_for_agy():
    """agy's gateway answers a fake 429 RESOURCE_EXHAUSTED to any prompt holding second-person identity
    phrasing that names another assistant; agy loads every skill's description (and may read the bodies and the docs) on
    every turn. Say "in a Claude Code session", never address the reader as that assistant."""
    import re
    bad = re.compile(r"\byou\s+are\s+(" + "claude" + r"|agy|copilot|opencode|gemini|codex)\b", re.I)
    files = [p for p in ROOT.rglob("*.md") if not {".git", "node_modules"} & set(p.parts)]
    files += [*ROOT.glob(".claude-plugin/*.json"), *ROOT.glob("daemon/share/.claude-plugin/*.json")]
    hits = [f"{p.relative_to(ROOT)}:{text[:m.start()].count(chr(10)) + 1}"
            for p in files for text in [p.read_text()] for m in bad.finditer(text)]
    assert not hits, hits


def test_risky_and_large_keep_their_definitions():
    """The user's 2026-11-28 definitions (ORCHESTRATE "Tiers" and "Sizes"): risky is money, data that matters, exposure
    to others or a deploy, never concurrency; size counts hand-written files, never lines. The old wording must not
    come back in the shipped protocol, skills, settings or docs."""
    old = ("(auth, money, data migrations, concurrency)", "(security, money, data migrations, concurrency)",
           "a few hundred lines", "1,000 changed lines", "additions,deletions,changedFiles")
    files = [*ROOT.glob("daemon/share/**/*.md"), ROOT / "README.md", ROOT / "daemon/README.md",
             ROOT / "docs/apps.md", ROOT / "daemon/internal/settings/schema.go"]
    hits = [f"{p.relative_to(ROOT)}: {s}" for p in files for s in old if s in p.read_text()]
    assert not hits, hits
    orch = (ROOT / "daemon/share/protocol/ORCHESTRATE.md").read_text()
    assert "- **Risky** means" in orch and "never by lines" in orch
