"""install.sh in a temp HOME: arguments, and the Claude Code plugin taking over the skills."""
import json
import subprocess
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent


def install(home: Path, *args: str, clis: tuple[str, ...] = ("claude", "agy")) -> subprocess.CompletedProcess:
    # PATH without ~/.local/bin, so the machine's herdr is never touched; `clis` are stand-ins that only exist
    fakes = home / "fake-bin"
    fakes.mkdir(exist_ok=True)
    for cli in clis:
        (fakes / cli).write_text("#!/bin/sh\nexit 0\n")
        (fakes / cli).chmod(0o755)
    return subprocess.run(["bash", str(ROOT / "install.sh"), *args], capture_output=True, text=True,
                          env={"HOME": str(home), "PATH": f"{fakes}:/usr/bin:/bin"}, timeout=60)


def claude_skills(home: Path) -> list[str]:
    folder = home / ".claude" / "skills"
    return sorted(p.name for p in folder.iterdir()) if folder.exists() else []


def plugin_installed(home: Path, scope: str = "user") -> None:
    record = home / ".claude" / "plugins" / "installed_plugins.json"
    record.parent.mkdir(parents=True, exist_ok=True)
    record.write_text(json.dumps({"version": 2, "plugins": {"agent-mesh@agent-mesh": [
        {"scope": scope, "installPath": "/x", "version": "abc123def456"}]}}))


def test_plain_install_links_the_skills(tmp_path):
    assert install(tmp_path).returncode == 0
    assert len(claude_skills(tmp_path)) == 7
    assert (tmp_path / ".local" / "bin" / "mesh").is_symlink()


def test_installed_plugin_keeps_the_skills_out_on_a_plain_rerun(tmp_path):
    install(tmp_path)
    plugin_installed(tmp_path)
    done = install(tmp_path)
    assert done.returncode == 0 and "plugin is installed" in done.stdout
    assert claude_skills(tmp_path) == []
    assert len(list((tmp_path / ".gemini" / "config" / "skills").iterdir())) == 7  # agy still gets them


def test_a_project_scoped_plugin_doesnt_count(tmp_path):
    plugin_installed(tmp_path, scope="local")
    install(tmp_path)
    assert len(claude_skills(tmp_path)) == 7


def test_plugin_flag_skips_the_skills_before_the_plugin_is_installed(tmp_path):
    install(tmp_path)
    assert install(tmp_path, "--plugin").returncode == 0
    assert claude_skills(tmp_path) == []


def test_unknown_or_conflicting_arguments_change_nothing(tmp_path):
    for args in (["--plugins"], ["--plugin", "--uninstall"], ["--uninstall", "--plugin"]):
        done = install(tmp_path, *args)
        assert done.returncode == 2 and "usage:" in done.stderr, args
    assert not (tmp_path / ".local").exists() and claude_skills(tmp_path) == []


def test_uninstall_removes_the_links(tmp_path):
    install(tmp_path)
    assert install(tmp_path, "--uninstall").returncode == 0
    assert claude_skills(tmp_path) == [] and not (tmp_path / ".local" / "bin" / "mesh").exists()


def gemini_skills(home: Path) -> list[str]:
    folder = home / ".gemini" / "config" / "skills"
    return sorted(p.name for p in folder.iterdir()) if folder.exists() else []


def test_an_agy_only_machine_gets_only_agys_skills_and_rules(tmp_path):
    done = install(tmp_path, clis=("agy",))
    assert done.returncode == 0 and "claude is not on PATH" in done.stdout
    assert claude_skills(tmp_path) == [] and not (tmp_path / ".claude" / "CLAUDE.md").exists()
    assert "delegate-claude" not in gemini_skills(tmp_path) and "delegate-agy" in gemini_skills(tmp_path)
    assert len(gemini_skills(tmp_path)) == 6
    assert "agent-mesh:begin" in (tmp_path / ".gemini" / "AGENTS.md").read_text()


def test_a_claude_only_machine_gets_no_delegate_agy(tmp_path):
    assert install(tmp_path, clis=("claude",)).returncode == 0
    assert "delegate-agy" not in claude_skills(tmp_path) and len(claude_skills(tmp_path)) == 6
    assert gemini_skills(tmp_path) == [] and not (tmp_path / ".gemini" / "AGENTS.md").exists()


def test_a_cli_gone_since_the_last_install_loses_its_skill(tmp_path):
    assert install(tmp_path).returncode == 0
    assert "delegate-agy" in claude_skills(tmp_path)
    (tmp_path / "fake-bin" / "agy").unlink()
    assert install(tmp_path, clis=("claude",)).returncode == 0
    assert "delegate-agy" not in claude_skills(tmp_path)  # our own link, removed


def test_no_cli_at_all_warns(tmp_path):
    done = install(tmp_path, clis=())
    assert done.returncode == 0 and "neither Claude Code (claude) nor agy is on PATH" in done.stderr
