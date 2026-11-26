"""The Claude Code plugin ships the same skills install.sh links."""
import json
import re
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent


def install_list(name: str) -> list[str]:
    match = re.search(rf"^{name}=\(([^)]*)\)", (ROOT / "install.sh").read_text(), re.M)
    assert match, f"install.sh has no {name}=(...)"
    return match.group(1).split()


def test_plugin_ships_install_skills():
    """The plugin loads every skills/<name>/SKILL.md (its default skills folder): each must be one install.sh
    links, and each one install.sh links must be there."""
    shipped = {p.parent.name for p in (ROOT / "skills").glob("*/SKILL.md")}
    assert shipped == set(install_list("SKILLS"))
    assert not shipped & set(install_list("RETIRED_SKILLS"))


def test_marketplace_lists_the_repo_plugin():
    plugin = json.loads((ROOT / ".claude-plugin" / "plugin.json").read_text())
    market = json.loads((ROOT / ".claude-plugin" / "marketplace.json").read_text())
    assert [(p["name"], p["source"]) for p in market["plugins"]] == [(plugin["name"], "./")]
    assert "skills" not in plugin  # a `skills` key adds folders; the default skills/ is the one install.sh links
    assert "version" not in plugin and "version" not in market["plugins"][0]  # users track commits, like the CLI
    named = set(re.findall(r"[\w-]+", market["plugins"][0]["description"]))
    assert set(install_list("SKILLS")) <= named
