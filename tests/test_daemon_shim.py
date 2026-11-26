"""bin/mesh hands over to the daemon's Go `mesh` for an agent the daemon started, and only for one."""
import os
import subprocess
import sys
from pathlib import Path

MESH = Path(__file__).resolve().parent.parent / "bin" / "mesh"


def run(tmp_path, env_extra, *args):
    env = {"PATH": os.environ["PATH"], "HOME": str(tmp_path), "MESH_HOME": str(tmp_path / "run"),
           "MESH_WORKTREES": str(tmp_path / "wt"), "MESH_PEERS_DIR": str(tmp_path / "peers"),
           "MESH_CONFIG_FILE": str(tmp_path / "config.json"), "MESH_HERDR": str(tmp_path / "no-herdr")}
    env.update(env_extra)
    return subprocess.run([sys.executable, str(MESH), *args], env=env, capture_output=True, text=True, timeout=60)


def stub(tmp_path):
    cli = tmp_path / "go-mesh"
    cli.write_text('#!/bin/sh\necho "go mesh: $*"\n')
    cli.chmod(0o755)
    return cli


def test_a_daemon_agent_gets_the_go_cli(tmp_path):
    cli = stub(tmp_path)
    r = run(tmp_path, {"AGENT_MESH_TOKEN": "t", "AGENT_MESH_AGENT": "a1", "AGENT_MESH_CLI": str(cli)}, "send", "parent", "--type", "done")
    assert r.returncode == 0, r.stderr
    assert r.stdout == "go mesh: send parent --type done\n"


def test_a_daemon_agent_without_the_go_cli_fails_loudly(tmp_path):
    for extra in ({"AGENT_MESH_TOKEN": "t", "AGENT_MESH_AGENT": "a1"},
                  {"AGENT_MESH_TOKEN": "t", "AGENT_MESH_AGENT": "a1", "AGENT_MESH_CLI": str(tmp_path / "missing")}):
        r = run(tmp_path, extra, "status")
        assert r.returncode == 1
        assert "started by the agent-mesh daemon" in r.stderr


def test_herdr_agents_and_the_user_keep_this_mesh(tmp_path):
    cli = stub(tmp_path)
    # No token: the user's shell. A token with MESH_RUN, or with no AGENT_MESH_AGENT: not an agent of the daemon.
    for extra in ({"AGENT_MESH_CLI": str(cli)}, {"AGENT_MESH_TOKEN": "t", "AGENT_MESH_AGENT": "a1", "AGENT_MESH_CLI": str(cli), "MESH_RUN": "r1"},
                  {"AGENT_MESH_TOKEN": "t", "AGENT_MESH_CLI": str(cli)}):
        r = run(tmp_path, extra, "--help")
        assert r.returncode == 0 and "go mesh" not in r.stdout
        assert "usage" in r.stdout.lower()
