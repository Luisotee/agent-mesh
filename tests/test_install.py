"""install.sh in a temp HOME, with `go`, `systemctl` and `herdr` stubbed: the daemon install, the links, the rules
blocks, the Claude Code plugin taking over the skills, an upgrade from the bin/mesh era's links, and --uninstall."""
import json
import shutil
import subprocess
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
BASH = shutil.which("bash") or "bash"
SKILLS = ["check-plan", "delegate", "delegate-review", "find-peers", "harvest-peers", "make-concise", "self-review"]
RETIRED = ["delegate-agy", "delegate-claude", "delegate-claude-agy"]

# a go that builds the packages it is given into the -o folder, as `go build -o dir/ ./cmd/a ./cmd/b` does
GO_STUB = '''#!/bin/sh
out=""; prev=""
for a in "$@"; do
  [ "$prev" = -o ] && out="$a"
  case "$a" in ./cmd/*) mkdir -p "$out"; printf '#!/bin/sh\\n' > "$out${a#./cmd/}"; chmod +x "$out${a#./cmd/}";; esac
  prev="$a"
done
'''


def install(home: Path, *args: str, clis: tuple[str, ...] = ("claude", "agy"), go_fails: bool = False,
            with_systemctl: bool = True, herdr: bool = False, repo: Path = ROOT, path: str = "",
            ) -> subprocess.CompletedProcess:
    """Run install.sh with HOME=home and a PATH of stubs: `clis` are stand-ins that only exist; `go` builds
    nothing real. The machine's own herdr, systemd and CLIs are never reached (no ~/.local/bin on PATH)."""
    stubs = home / "stub-bin"
    stubs.mkdir(exist_ok=True)
    for cli in clis:
        (stubs / cli).write_text("#!/bin/sh\nexit 0\n")
        (stubs / cli).chmod(0o755)
    go = "#!/bin/sh\necho boom >&2; exit 1\n" if go_fails else GO_STUB
    (stubs / "go").write_text(go)
    (stubs / "go").chmod(0o755)
    if with_systemctl:
        (stubs / "systemctl").write_text(f'#!/bin/sh\necho "$@" >> {home / "systemctl.log"}\n')
        (stubs / "systemctl").chmod(0o755)
    if herdr:  # lists the retired plugin, and logs every call
        (stubs / "herdr").write_text(f'#!/bin/sh\necho "$@" >> {home / "herdr.log"}\n'
                                     '[ "$1 $2" = "plugin list" ] && echo local.mesh-notify\nexit 0\n')
        (stubs / "herdr").chmod(0o755)
    return subprocess.run([BASH, str(repo / "install.sh"), *args], capture_output=True, text=True,
                          env={"HOME": str(home), "PATH": path or f"{stubs}:/usr/bin:/bin"}, timeout=60)


def calls(home: Path, log: str) -> list[str]:
    f = home / log
    return f.read_text().splitlines() if f.exists() else []


def lib(home: Path) -> Path:
    return home / ".local" / "lib" / "agent-mesh"


def skills(home: Path, root: str) -> list[str]:
    folder = home / root / "skills"
    return sorted(p.name for p in folder.iterdir()) if folder.exists() else []


def claude_skills(home: Path) -> list[str]:
    return skills(home, ".claude")


def gemini_skills(home: Path) -> list[str]:
    return skills(home, ".gemini/config")


def plugin_installed(home: Path, scope: str = "user") -> None:
    record = home / ".claude" / "plugins" / "installed_plugins.json"
    record.parent.mkdir(parents=True, exist_ok=True)
    record.write_text(json.dumps({"version": 2, "plugins": {"agent-mesh@agent-mesh": [
        {"scope": scope, "installPath": "/x", "version": "abc123def456"}]}}))


# ── a fresh install ──


def test_install_builds_the_daemon_and_links_mesh_the_protocol_and_the_skills_into_it(tmp_path):
    done = install(tmp_path)
    assert done.returncode == 0, done.stderr
    for b in ("meshd", "mesh-term", "mesh"):
        assert (lib(tmp_path) / "bin" / b).stat().st_mode & 0o111, b
    assert not list(lib(tmp_path).glob(".stage.*"))
    for link, target in ((".local/bin/mesh", "bin/mesh"), (".config/agent-mesh", "share/protocol")):
        path = tmp_path / link
        assert path.is_symlink() and path.resolve() == (lib(tmp_path) / target).resolve(), link
    assert (tmp_path / ".config" / "agent-mesh" / "PROTOCOL.md").exists()  # the snippet's fallback path holds
    for root in (".claude", ".gemini/config"):
        assert skills(tmp_path, root) == SKILLS, root
        for s in SKILLS:
            link = tmp_path / root / "skills" / s
            assert link.is_symlink() and link.resolve() == (lib(tmp_path) / "share" / "skills" / s).resolve()
    units = tmp_path / ".config" / "systemd" / "user"
    for unit in ("agent-mesh.service", "agent-mesh-term.service"):
        assert (units / unit).read_text() == (ROOT / "daemon" / "packaging" / "systemd" / unit).read_text()
        dropin = (units / f"{unit}.d" / "path.conf").read_text()
        assert dropin.startswith("[Service]\nEnvironment=\"PATH=") and "/usr/bin" in dropin
    assert calls(tmp_path, "systemctl.log") == ["--user daemon-reload", "--user enable agent-mesh.service",
                                                "--user restart agent-mesh.service"]
    assert not (tmp_path / ".local" / "bin" / "meshctl").exists() and not (tmp_path / ".local" / "state").exists()


def test_the_rules_block_goes_into_claude_and_agy_only_and_names_the_daemons_protocol(tmp_path):
    assert install(tmp_path).returncode == 0
    for file in (".claude/CLAUDE.md", ".gemini/AGENTS.md"):
        text = (tmp_path / file).read_text()
        assert "<!-- agent-mesh:begin" in text and "📬 [mesh]" in text and "$AGENT_MESH_SHARE/protocol/PROTOCOL.md" in text
        assert "herdr" not in text and "mesh reset" not in text and "mesh brief" not in text
    assert not (tmp_path / ".copilot").exists() and not (tmp_path / ".config" / "opencode").exists()


def test_a_rerun_changes_nothing_and_the_block_stays_single(tmp_path):
    assert install(tmp_path).returncode == 0
    before = (tmp_path / ".claude" / "CLAUDE.md").read_text()
    assert install(tmp_path).returncode == 0
    assert (tmp_path / ".claude" / "CLAUDE.md").read_text() == before and before.count("agent-mesh:begin") == 1
    assert claude_skills(tmp_path) == SKILLS


def test_the_daemon_flag_is_accepted_as_an_alias(tmp_path):
    assert install(tmp_path, "--daemon").returncode == 0
    assert (lib(tmp_path) / "bin" / "meshd").exists() and (tmp_path / ".local" / "bin" / "mesh").is_symlink()


# ── the Claude Code plugin ──


def test_installed_plugin_keeps_the_skills_out_on_a_plain_rerun(tmp_path):
    install(tmp_path)
    plugin_installed(tmp_path)
    done = install(tmp_path)
    assert done.returncode == 0 and "plugin is installed" in done.stdout
    assert claude_skills(tmp_path) == []
    assert gemini_skills(tmp_path) == SKILLS  # agy still gets them


def test_a_project_scoped_plugin_doesnt_count(tmp_path):
    plugin_installed(tmp_path, scope="local")
    install(tmp_path)
    assert claude_skills(tmp_path) == SKILLS


def test_plugin_flag_skips_the_skills_before_the_plugin_is_installed(tmp_path):
    install(tmp_path)
    assert install(tmp_path, "--plugin").returncode == 0
    assert claude_skills(tmp_path) == []


def test_unknown_or_conflicting_arguments_change_nothing(tmp_path):
    for args in (["--plugins"], ["--plugin", "--uninstall"], ["--uninstall", "--plugin"]):
        done = install(tmp_path, *args)
        assert done.returncode == 2 and "usage:" in done.stderr, args
    assert not (tmp_path / ".local").exists() and claude_skills(tmp_path) == []


# ── which CLIs get what ──


def test_an_agy_only_machine_gets_only_agys_skills_and_rules(tmp_path):
    done = install(tmp_path, clis=("agy",))
    assert done.returncode == 0 and "claude is not on PATH" in done.stdout
    assert claude_skills(tmp_path) == [] and not (tmp_path / ".claude" / "CLAUDE.md").exists()
    assert gemini_skills(tmp_path) == SKILLS
    assert "agent-mesh:begin" in (tmp_path / ".gemini" / "AGENTS.md").read_text()


def test_a_claude_only_machine_gets_no_agy_files(tmp_path):
    assert install(tmp_path, clis=("claude",)).returncode == 0
    assert claude_skills(tmp_path) == SKILLS
    assert gemini_skills(tmp_path) == [] and not (tmp_path / ".gemini" / "AGENTS.md").exists()


def test_a_cli_missing_from_path_on_a_rerun_removes_nothing(tmp_path):
    """agy may be on another shell's PATH only: a re-run without it adds nothing for it and removes nothing."""
    assert install(tmp_path).returncode == 0
    before = (claude_skills(tmp_path), gemini_skills(tmp_path), (tmp_path / ".gemini" / "AGENTS.md").read_text())
    (tmp_path / "stub-bin" / "agy").unlink()
    done = install(tmp_path, clis=("claude",))
    assert done.returncode == 0 and "nothing new goes into" in done.stdout
    assert (claude_skills(tmp_path), gemini_skills(tmp_path), (tmp_path / ".gemini" / "AGENTS.md").read_text()) == before


def test_no_cli_at_all_warns(tmp_path):
    done = install(tmp_path, clis=())
    assert done.returncode == 0 and "neither Claude Code (claude) nor agy is on PATH" in done.stderr


# ── an upgrade from the bin/mesh era ──

OLD_BLOCK = "\n<!-- agent-mesh:begin (managed by agent-mesh/install.sh) -->\n## agent-mesh\nneeds herdr\n<!-- agent-mesh:end -->\n"


def old_install(home: Path) -> None:
    """What the old install.sh left: links into the checkout (deleted by the pull, so dangling), the retired
    skills' links, the Copilot and OpenCode blocks, the meshctl link and the herdr plugin."""
    (home / ".local" / "bin").mkdir(parents=True)
    (home / ".config").mkdir()
    (home / ".local" / "bin" / "mesh").symlink_to(ROOT / "bin" / "mesh")
    (home / ".config" / "agent-mesh").symlink_to(ROOT / "protocol")
    for root in (".claude", ".gemini/config"):
        (home / root / "skills").mkdir(parents=True)
        for s in ("delegate-claude", "delegate-agy", "delegate-review", "check-plan", "make-concise"):
            (home / root / "skills" / s).symlink_to(ROOT / "skills" / s)
    for file in (".claude/CLAUDE.md", ".gemini/AGENTS.md"):
        (home / file).write_text("# mine\n" + OLD_BLOCK)
    (home / ".copilot").mkdir()
    (home / ".copilot" / "copilot-instructions.md").write_text("# copilot mine\n" + OLD_BLOCK)
    (home / ".config" / "opencode").mkdir()
    (home / ".config" / "opencode" / "AGENTS.md").write_text(OLD_BLOCK.lstrip("\n"))
    (home / ".local" / "lib" / "agent-mesh" / "bin").mkdir(parents=True)
    (home / ".local" / "bin" / "meshctl").symlink_to(home / ".local" / "lib" / "agent-mesh" / "bin" / "mesh")


def test_an_upgrade_repoints_the_old_links_and_removes_the_retired_ones(tmp_path):
    old_install(tmp_path)
    assert not (tmp_path / ".local" / "bin" / "mesh").exists()  # dangling: the checkout no longer has bin/mesh
    done = install(tmp_path, herdr=True)
    assert done.returncode == 0, done.stderr
    mesh = tmp_path / ".local" / "bin" / "mesh"
    assert mesh.resolve() == (lib(tmp_path) / "bin" / "mesh").resolve()
    assert (tmp_path / ".config" / "agent-mesh").resolve() == (lib(tmp_path) / "share" / "protocol").resolve()
    for root in (".claude", ".gemini/config"):
        assert skills(tmp_path, root) == SKILLS, root  # delegate-claude and delegate-agy are gone, delegate is there
        for s in SKILLS:
            assert (tmp_path / root / "skills" / s).resolve().exists()
    assert not (tmp_path / ".local" / "bin" / "meshctl").exists() and not (tmp_path / ".local" / "bin" / "meshctl").is_symlink()
    # the old block is replaced by the new one, and the user's own lines stay
    claude = (tmp_path / ".claude" / "CLAUDE.md").read_text()
    assert claude.startswith("# mine") and claude.count("agent-mesh:begin") == 1 and "needs herdr" not in claude
    # a CLI the daemon has no adapter for loses the block; an OpenCode file with only the block goes
    assert (tmp_path / ".copilot" / "copilot-instructions.md").read_text().strip() == "# copilot mine"
    assert not (tmp_path / ".config" / "opencode" / "AGENTS.md").exists()
    assert "plugin unlink local.mesh-notify" in calls(tmp_path, "herdr.log")


def test_an_upgrade_leaves_a_users_own_files_and_the_old_run_state_alone(tmp_path):
    old_install(tmp_path)
    (tmp_path / ".claude" / "skills" / "mine").mkdir()
    (tmp_path / ".claude" / "skills" / "delegate-claude").unlink()
    (tmp_path / ".claude" / "skills" / "delegate-claude").write_text("a real file of the user's")
    old_state = tmp_path / ".local" / "state" / "agent-mesh" / "mtest"
    old_state.mkdir(parents=True)
    (old_state / "run.json").write_text("{}")
    (tmp_path / ".config" / "agent-mesh.json").write_text('{"compact": {}}')
    assert install(tmp_path).returncode == 0
    assert (tmp_path / ".claude" / "skills" / "mine").is_dir()
    assert (tmp_path / ".claude" / "skills" / "delegate-claude").read_text() == "a real file of the user's"
    assert (old_state / "run.json").read_text() == "{}" and (tmp_path / ".config" / "agent-mesh.json").exists()


def test_a_failed_build_leaves_the_old_links_working(tmp_path):
    """The rollback and the safety of an upgrade: nothing is re-pointed before the daemon is built."""
    old_install(tmp_path)
    done = install(tmp_path, go_fails=True)
    assert done.returncode == 1 and "nothing was changed" in done.stderr
    assert (tmp_path / ".local" / "bin" / "mesh").readlink() == ROOT / "bin" / "mesh"
    assert (tmp_path / ".config" / "agent-mesh").readlink() == ROOT / "protocol"
    assert "delegate-claude" in claude_skills(tmp_path)
    assert "herdr" not in " ".join(calls(tmp_path, "herdr.log"))


# ── the daemon's own install steps ──


def copy_of_repo(tmp_path: Path) -> Path:
    """The parts of the checkout the install reads, so a test can change daemon/share without touching it."""
    repo = tmp_path / "repo"
    (repo / "daemon").mkdir(parents=True)
    shutil.copy(ROOT / "install.sh", repo / "install.sh")
    shutil.copytree(ROOT / "rules", repo / "rules")
    shutil.copytree(ROOT / "daemon" / "cmd", repo / "daemon" / "cmd")
    (repo / "daemon" / "share" / "protocol").mkdir(parents=True)
    (repo / "daemon" / "share" / "skills").mkdir()
    shutil.copytree(ROOT / "daemon" / "packaging", repo / "daemon" / "packaging")
    return repo


def test_install_copies_share_and_is_safe_to_rerun(tmp_path):
    repo, home = copy_of_repo(tmp_path), tmp_path / "home"
    home.mkdir()
    (repo / "daemon" / "share" / ".claude-plugin").mkdir()
    (repo / "daemon" / "share" / ".claude-plugin" / "plugin.json").write_text("{}")  # a hidden folder is copied too
    (repo / "daemon" / "share" / "marker.md").write_text("x")
    assert install(home, repo=repo).returncode == 0
    assert (lib(home) / "share" / "marker.md").read_text() == "x"
    assert (lib(home) / "share" / ".claude-plugin" / "plugin.json").read_text() == "{}"
    (repo / "daemon" / "share" / "marker.md").unlink()
    assert install(home, repo=repo).returncode == 0
    assert not (lib(home) / "share" / "marker.md").exists() and not (lib(home) / "share.old").exists()


def test_a_changed_path_or_terminal_host_binary_is_noted_but_not_applied(tmp_path):
    first = install(tmp_path)
    assert first.returncode == 0 and "agent-mesh-term.service'" not in first.stderr  # a first install has no host to restart
    again = install(tmp_path)
    assert "restart agent-mesh-term.service" not in again.stderr  # nothing changed
    stubs = tmp_path / "stub-bin"
    changed = install(tmp_path, path=f"{stubs}:/usr/bin:/bin:/opt/more")
    assert "restart agent-mesh-term.service" in changed.stderr
    # and the terminal host is never restarted by the installer itself
    assert not any("agent-mesh-term" in c and "restart" in c for c in calls(tmp_path, "systemctl.log"))
    assert "/opt/more" in (tmp_path / ".config" / "systemd" / "user" / "agent-mesh-term.service.d" / "path.conf").read_text()


def test_the_path_dropin_escapes_what_systemd_would_read(tmp_path):
    stubs = tmp_path / "stub-bin"
    done = install(tmp_path, path=f'{stubs}:/usr/bin:/odd%dir:/back\\slash:/q"uote')
    assert done.returncode == 0, done.stderr
    dropin = (tmp_path / ".config" / "systemd" / "user" / "agent-mesh.service.d" / "path.conf").read_text()
    assert "/odd%%dir" in dropin and "/back\\\\slash" in dropin and '/q\\"uote' in dropin


def test_a_failed_first_build_leaves_no_folders_behind(tmp_path):
    done = install(tmp_path, go_fails=True)
    assert done.returncode == 1 and "nothing was changed" in done.stderr
    assert not (tmp_path / ".local").exists() and not (tmp_path / ".config").exists()


def test_a_changed_terminal_unit_is_noted_too(tmp_path):
    repo = copy_of_repo(tmp_path)
    home = tmp_path / "home"
    home.mkdir()
    assert install(home, repo=repo).returncode == 0
    unit = repo / "daemon" / "packaging" / "systemd" / "agent-mesh-term.service"
    unit.write_text(unit.read_text() + "# changed\n")
    assert "restart agent-mesh-term.service" in install(home, repo=repo).stderr


def test_a_checkout_without_share_protocol_or_skills_changes_nothing(tmp_path):
    for missing in ("protocol", "skills"):
        repo = copy_of_repo(tmp_path / missing)
        home = tmp_path / missing / "home"
        home.mkdir()
        assert install(home, repo=repo).returncode == 0
        before = sorted(str(p) for p in home.rglob("*") if p.is_symlink())
        shutil.rmtree(repo / "daemon" / "share" / missing)
        done = install(home, repo=repo)
        assert done.returncode == 1 and f"daemon/share/{missing} is missing" in done.stderr
        assert sorted(str(p) for p in home.rglob("*") if p.is_symlink()) == before
        assert (lib(home) / "share" / missing).is_dir()  # the installed copy is still there


def test_a_rules_file_without_a_block_is_left_alone_even_when_read_only(tmp_path):
    for rel in (".copilot/copilot-instructions.md", ".config/opencode/AGENTS.md"):
        f = tmp_path / rel
        f.parent.mkdir(parents=True, exist_ok=True)
        f.write_text("# my own rules\n")
        f.chmod(0o444)
    before = {rel: (tmp_path / rel).stat().st_mtime_ns for rel in (".copilot/copilot-instructions.md", ".config/opencode/AGENTS.md")}
    done = install(tmp_path)
    assert done.returncode == 0, done.stderr
    assert "cleaned" not in done.stdout
    for rel, mtime in before.items():
        assert (tmp_path / rel).read_text() == "# my own rules\n" and (tmp_path / rel).stat().st_mtime_ns == mtime
        assert not (tmp_path / (rel + ".bak-agent-mesh")).exists()


def bare_path(home: Path, *tools: str) -> str:
    """A PATH holding only the named tools (symlinks), so `go` and `systemctl` are absent whatever the machine has."""
    folder = home / "bare-bin"
    folder.mkdir(exist_ok=True)
    for tool in ("mktemp", "cp", "mv", "rm", "rmdir", "mkdir", "ln", "readlink", "dirname", "chmod", "cmp", "grep",
                 "python3", "touch", *tools):
        (folder / tool).unlink(missing_ok=True)
        (folder / tool).symlink_to(shutil.which(tool) or tool)
    return str(folder)


def run_bare(home: Path, bare: Path) -> subprocess.CompletedProcess:
    return subprocess.run([BASH, str(ROOT / "install.sh")], capture_output=True, text=True,
                          env={"HOME": str(home), "PATH": str(bare)}, timeout=60)


def test_install_without_cmp_still_installs(tmp_path):
    bare = Path(bare_path(tmp_path))
    (bare / "cmp").unlink()
    (bare / "go").write_text(GO_STUB)
    (bare / "go").chmod(0o755)
    done = run_bare(tmp_path, bare)
    assert done.returncode == 0, done.stderr
    assert (lib(tmp_path) / "bin" / "mesh-term").exists()


def test_a_failed_build_changes_nothing(tmp_path):
    assert install(tmp_path).returncode == 0
    mesh = lib(tmp_path) / "bin" / "mesh"
    mesh.write_text("old")
    done = install(tmp_path, go_fails=True)
    assert done.returncode == 1 and "nothing was changed" in done.stderr
    assert mesh.read_text() == "old"
    assert not list(lib(tmp_path).glob(".stage.*"))


def test_install_without_go_changes_nothing(tmp_path):
    done = run_bare(tmp_path, Path(bare_path(tmp_path)))
    assert done.returncode == 1 and "needs Go" in done.stderr and not (tmp_path / ".local").exists()


def test_install_without_systemctl_or_with_a_failing_one_still_installs(tmp_path):
    bare = Path(bare_path(tmp_path))
    (bare / "go").write_text(GO_STUB)
    (bare / "go").chmod(0o755)
    done = run_bare(tmp_path, bare)
    assert done.returncode == 0 and "systemctl is not on PATH" in done.stderr, done.stderr
    assert (lib(tmp_path) / "bin" / "meshd").exists()
    # a systemctl that fails (no user session) is a warning, not a failed install
    (bare / "systemctl").write_text("#!/bin/sh\nexit 1\n")
    (bare / "systemctl").chmod(0o755)
    done = run_bare(tmp_path, bare)
    assert done.returncode == 0 and "could not start agent-mesh.service" in done.stderr, done.stderr


def test_a_real_file_where_a_link_goes_is_left_alone(tmp_path):
    (tmp_path / ".local" / "bin").mkdir(parents=True)
    (tmp_path / ".local" / "bin" / "mesh").write_text("mine")
    done = install(tmp_path)
    assert done.returncode == 0 and "skip:" in done.stderr
    assert (tmp_path / ".local" / "bin" / "mesh").read_text() == "mine"


# ── --uninstall ──


def test_uninstall_reverses_the_install_and_keeps_both_state_dirs_and_the_users_own_files(tmp_path):
    assert install(tmp_path).returncode == 0
    (tmp_path / ".claude" / "skills" / "mine").mkdir()
    (tmp_path / ".claude" / "CLAUDE.md").write_text("# mine\n" + (tmp_path / ".claude" / "CLAUDE.md").read_text())
    states = [tmp_path / ".local" / "state" / n for n in ("agent-mesh-daemon", "agent-mesh")]
    for state in states:
        state.mkdir(parents=True)
        (state / "keep").write_text("runs")
    done = install(tmp_path, "--uninstall")
    assert done.returncode == 0, done.stderr
    assert not (tmp_path / ".local" / "bin" / "mesh").exists() and not (tmp_path / ".config" / "agent-mesh").is_symlink()
    assert claude_skills(tmp_path) == ["mine"] and gemini_skills(tmp_path) == []
    assert (tmp_path / ".claude" / "CLAUDE.md").read_text().strip() == "# mine"
    assert "agent-mesh:begin" not in (tmp_path / ".gemini" / "AGENTS.md").read_text()
    assert not lib(tmp_path).exists()
    for unit in ("agent-mesh.service", "agent-mesh-term.service"):
        assert not (tmp_path / ".config" / "systemd" / "user" / unit).exists()
        assert not (tmp_path / ".config" / "systemd" / "user" / f"{unit}.d").exists()
    assert "--user disable --now agent-mesh.service agent-mesh-term.service" in calls(tmp_path, "systemctl.log")
    assert all((s / "keep").read_text() == "runs" for s in states)


def test_uninstall_also_clears_what_the_old_install_left(tmp_path):
    old_install(tmp_path)
    assert install(tmp_path, "--uninstall", herdr=True).returncode == 0
    assert claude_skills(tmp_path) == [] and gemini_skills(tmp_path) == []
    assert not (tmp_path / ".local" / "bin" / "mesh").is_symlink() and not (tmp_path / ".config" / "agent-mesh").is_symlink()
    assert "plugin unlink local.mesh-notify" in calls(tmp_path, "herdr.log")
    for file in (".claude/CLAUDE.md", ".gemini/AGENTS.md", ".copilot/copilot-instructions.md"):
        assert "agent-mesh:begin" not in (tmp_path / file).read_text(), file
