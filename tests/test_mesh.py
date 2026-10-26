import json
import os
import shlex
import subprocess
import time
from pathlib import Path

import pytest
from conftest import load_mesh


MESH_SCRIPT = Path(__file__).resolve().parent.parent / "bin" / "mesh"

def bare(doorbell):
    """A doorbell without its ring token."""
    import re
    return re.sub(r" \(ref [0-9a-f]{6}\)", "", doorbell)


def as_worker(monkeypatch, run):
    monkeypatch.setenv("MESH_RUN", run)
    monkeypatch.setenv("MESH_SELF", "repo-fix-typo")


def sent_plan(mesh, run, name="repo-fix-typo"):
    """The plan `mesh approve` needs to find waiting: the worker's newest report since its last task, already
    read by the root (an unread report ends a watch at once)."""
    parent = mesh.load_run(run)["agents"][name]["parent"]
    mesh.mark_read(mesh.record(run, name, parent, "plan", "The plan."))


# ── init / identity ──────────────────────────────────────────────────────


def test_init_names_root_and_maps_pane(mesh, fake, run, capsys):
    assert fake.called("agent", "rename", "w1:p1", "repo-lead")
    data = mesh.load_run(run)
    assert data["root"] == "repo-lead"
    assert data["agents"]["repo-lead"]["kind"] == "claude"
    # the root has no MESH_* env, so its identity comes from the pane map
    import argparse
    assert mesh.identity(argparse.Namespace()) == ("mtest", "repo-lead")


def test_init_makes_the_folders_agents_write_into(mesh, run):
    for folder in mesh.RUN_FOLDERS:
        assert (mesh.run_dir(run) / folder).is_dir()


def test_mesh_dir_makes_the_folders_of_an_older_run(mesh, run, monkeypatch, capsys):
    for folder in mesh.RUN_FOLDERS:
        (mesh.run_dir(run) / folder).rmdir()
    as_worker(monkeypatch, run)
    assert mesh.main(["dir"]) == 0
    assert capsys.readouterr().out.strip() == str(mesh.run_dir(run))
    assert all((mesh.run_dir(run) / folder).is_dir() for folder in mesh.RUN_FOLDERS)


def test_mesh_dir_makes_nothing_for_a_run_that_isnt_live(mesh, run, capsys):
    assert mesh.main(["dir", "--run", "mgone", "--as", "someone"]) == 0
    assert capsys.readouterr().out.strip() == str(mesh.run_dir("mgone"))
    assert not mesh.run_dir("mgone").exists()


def test_mesh_dir_still_prints_the_path_when_it_cant_make_the_folders(mesh, run, monkeypatch, capsys):
    as_worker(monkeypatch, run)
    def refuse(run_id):
        raise PermissionError(13, "Permission denied")
    monkeypatch.setattr(mesh, "make_run_folders", refuse)
    capsys.readouterr()
    assert mesh.main(["dir"]) == 0
    out, err = capsys.readouterr()
    assert out.strip() == str(mesh.run_dir(run))
    assert "can't make the folders" in err and "Permission denied" in err


def test_a_missing_file_is_a_mesh_error(mesh, run, tmp_path, monkeypatch, capsys):
    as_worker(monkeypatch, run)
    missing = tmp_path / "reports" / "nope.md"
    capsys.readouterr()
    assert mesh.main(["send", "parent", "--type", "done", "--file", str(missing)]) == 1
    err = capsys.readouterr().err
    assert f"can't read {missing}: No such file or directory" in err and "Traceback" not in err
    assert mesh.main(["send", "parent", "--type", "done", "--file", str(tmp_path)]) == 1  # a folder
    assert "can't read" in capsys.readouterr().err
    assert not mesh.unread(run, "repo-lead")


def test_undecodable_stdin_is_a_mesh_error(mesh, run, monkeypatch, capsys):
    import io
    as_worker(monkeypatch, run)
    monkeypatch.setattr(mesh.sys, "stdin", io.TextIOWrapper(io.BytesIO(b"\xff\xfe bad"), encoding="utf-8"))
    capsys.readouterr()
    assert mesh.main(["send", "parent", "--type", "done", "--file", "-"]) == 1
    err = capsys.readouterr().err
    assert "can't read stdin" in err and "Traceback" not in err


def test_spawn_with_a_missing_task_file_leaves_no_worktree(mesh, fake, run, repo, tmp_path, capsys):
    capsys.readouterr()
    assert mesh.main(["spawn", "--repo", str(repo), "--model", "flash", "--worktree", "Fix",
                      "--task-file", str(tmp_path / "nope.md")]) == 1
    assert "can't read" in capsys.readouterr().err
    assert mesh.load_run(run)["worktrees"] == []
    assert "repo-fix" not in mesh.load_run(run)["agents"]


def test_resume_and_ask_with_a_missing_file_fail_cleanly(mesh, fake, run, repo, tmp_path, capsys):
    read_only_asker(mesh, run, status="retired", cwd=str(repo), repo=str(repo))
    stub_new_pane(fake)
    fake.on("agent", "get", error="agent_not_found")
    capsys.readouterr()
    assert mesh.main(["resume", "repo-fix-typo", "--task-file", str(tmp_path / "nope.md")]) == 1
    assert mesh.main(["ask", "--repo", str(repo), "--file", str(tmp_path / "nope.md")]) == 1
    err = capsys.readouterr().err
    assert err.count("can't read") == 2 and "Traceback" not in err
    assert not fake.called("agent", "start")


def test_init_outside_herdr_fails(mesh, fake, monkeypatch, capsys):
    monkeypatch.delenv("HERDR_ENV", raising=False)
    assert mesh.main(["init", "--mode", "agy"]) == 1
    assert "inside a herdr pane" in capsys.readouterr().err


def test_identity_without_run_fails(mesh, fake, monkeypatch, capsys):
    monkeypatch.delenv("HERDR_PANE_ID", raising=False)
    assert mesh.main(["inbox"]) == 1
    assert "not in a mesh run" in capsys.readouterr().err


def test_identity_error_points_to_agents_and_status(mesh, fake, monkeypatch, capsys):
    monkeypatch.delenv("HERDR_PANE_ID", raising=False)
    assert mesh.main(["whoami"]) == 1
    err = capsys.readouterr().err
    assert "`mesh agents`" in err and "`mesh status --run <id>`" in err


def inherited(mesh, run, monkeypatch, **entry):
    """The worker as a Claude session `rec-s`, and this process as another session with its MESH_* identity."""
    with mesh.editing_run(run) as data:
        data["agents"]["repo-fix-typo"].update({"kind": "claude", "session": "rec-s", **entry})
    monkeypatch.setenv("MESH_RUN", run)
    monkeypatch.setenv("MESH_SELF", "repo-fix-typo")
    monkeypatch.setenv("CLAUDE_CODE_SESSION_ID", "other-s")


def test_an_identity_inherited_from_another_session_is_ignored(mesh, fake, run, monkeypatch, capsys):
    inherited(mesh, run, monkeypatch, view="bg", pane=None)
    monkeypatch.delenv("HERDR_PANE_ID")  # a background session: no pane to fall back on
    capsys.readouterr()
    assert mesh.main(["whoami"]) == 1
    err = capsys.readouterr().err
    assert "ignored MESH_SELF=repo-fix-typo" in err and "rec-s" in err and "other-s" in err


def test_an_inherited_identity_falls_back_to_the_pane_map(mesh, fake, run, monkeypatch, capsys):
    inherited(mesh, run, monkeypatch, view="bg", pane=None)  # HERDR_PANE_ID is still the root's pane
    capsys.readouterr()
    assert mesh.main(["whoami"]) == 0
    out, err = capsys.readouterr()
    assert json.loads(out)["name"] == "repo-lead" and "acting as repo-lead" in err
    assert [e["name"] for e in mesh.read_log(run) if e["event"] == "identity_ignored"] == ["repo-lead"]


def test_an_inherited_identity_takes_no_run_the_pane_disagrees_with(mesh, fake, run, monkeypatch, capsys):
    inherited(mesh, run, monkeypatch, view="bg", pane=None)
    (mesh.run_dir("mother") / "run.json").parent.mkdir(parents=True)
    worker = mesh.load_run(run)["agents"]["repo-fix-typo"]
    (mesh.run_dir("mother") / "run.json").write_text(json.dumps({"id": "mother", "agents": {"repo-fix-typo": worker}}))
    capsys.readouterr()
    assert mesh.main(["whoami", "--run", "mother"]) == 1
    assert "this pane is repo-lead of run mtest, not of run mother" in capsys.readouterr().err


@pytest.mark.parametrize("where", ["archived", "never"])
def test_an_identity_naming_no_live_run_is_inherited(mesh, fake, run, monkeypatch, capsys, where):
    # A daemon outlives the run whose worker started it, so a spare's MESH_RUN can name a stopped run.
    if where == "archived":
        (mesh.mesh_home() / "archive" / "mold").mkdir(parents=True)
        (mesh.mesh_home() / "archive" / "mold" / "run.json").write_text("{}")
    monkeypatch.setenv("MESH_RUN", "mold")
    monkeypatch.setenv("MESH_SELF", "proj-review")
    monkeypatch.setenv("CLAUDE_CODE_SESSION_ID", "other-s")
    monkeypatch.delenv("HERDR_PANE_ID")
    capsys.readouterr()
    assert mesh.main(["whoami"]) == 1
    err = capsys.readouterr().err
    assert err.startswith("mesh: not in a mesh run (ignored MESH_RUN=mold: no such run (stopped?))")


def test_a_pane_worker_in_its_own_pane_keeps_its_identity_whatever_its_session(mesh, fake, run, monkeypatch, capsys):
    inherited(mesh, run, monkeypatch, view="pane")  # herdr doesn't say which session runs there
    monkeypatch.setenv("HERDR_PANE_ID", "w2:p5")
    capsys.readouterr()
    assert mesh.main(["whoami"]) == 0
    assert json.loads(capsys.readouterr().out)["name"] == "repo-fix-typo"
    assert mesh.load_run(run)["agents"]["repo-fix-typo"]["session"] == "rec-s"


def test_a_pane_worker_whose_session_changed_is_rebound(mesh, fake, run, monkeypatch, capsys):
    inherited(mesh, run, monkeypatch, view="pane")  # e.g. `/clear`, or plan mode's "clear context"
    monkeypatch.setenv("HERDR_PANE_ID", "w2:p5")
    monkeypatch.setenv("CLAUDE_PID", "5000")
    in_pane(fake, "w2:p5", 5000)  # the same process, now in another session
    capsys.readouterr()
    assert mesh.main(["whoami"]) == 0
    assert json.loads(capsys.readouterr().out)["name"] == "repo-fix-typo"
    assert mesh.load_run(run)["agents"]["repo-fix-typo"]["session"] == "other-s"


def test_a_session_that_isnt_the_one_in_the_pane_is_not_its_worker(mesh, fake, run, monkeypatch, capsys):
    # A spare that inherited a pane worker's HERDR_PANE_ID along with its MESH_SELF.
    inherited(mesh, run, monkeypatch, view="pane")
    monkeypatch.setenv("HERDR_PANE_ID", "w2:p5")
    monkeypatch.setenv("CLAUDE_PID", "7000")
    in_pane(fake, "w2:p5", 5000)
    capsys.readouterr()
    assert mesh.main(["whoami"]) == 1
    assert "ignored MESH_SELF=repo-fix-typo" in capsys.readouterr().err


@pytest.mark.parametrize("kind", ["agy", "copilot"])
def test_a_claude_session_is_never_an_agy_or_copilot_agent(mesh, fake, run, monkeypatch, capsys, kind):
    inherited(mesh, run, monkeypatch, kind=kind, view="pane", pane="w2:p5")
    monkeypatch.setenv("HERDR_PANE_ID", "w2:p5")  # even in that agent's pane
    capsys.readouterr()
    assert mesh.main(["whoami"]) == 1
    assert f"that agent is {kind}, and this is Claude session other-s" in capsys.readouterr().err


@pytest.mark.parametrize("case", ["same session", "starting", "agy outside Claude", "named"])
def test_an_identity_is_kept_when_nothing_shows_it_inherited(mesh, fake, run, monkeypatch, capsys, case):
    extra = {"starting": {"status": "starting", "spawned": mesh.now_iso()},
             "agy outside Claude": {"kind": "agy"}}.get(case, {})
    inherited(mesh, run, monkeypatch, view="bg", pane=None, **extra)
    monkeypatch.delenv("HERDR_PANE_ID")
    if case == "same session":
        monkeypatch.setenv("CLAUDE_CODE_SESSION_ID", "rec-s")
    if case == "agy outside Claude":
        monkeypatch.delenv("CLAUDE_CODE_SESSION_ID")
    capsys.readouterr()
    assert mesh.main(["whoami", *(["--as", "repo-fix-typo"] if case == "named" else [])]) == 0
    assert json.loads(capsys.readouterr().out)["name"] == "repo-fix-typo"


def in_pane(fake, pane, leader, *members, session=None):
    """herdr's view of a pane: the process group leading its foreground (and other members, MCP servers say),
    plus the session its hook last reported (which anything started with the pane's ids can overwrite)."""
    fake.on("pane", "process-info", "--pane", pane, result={"process_info": {
        "foreground_process_group_id": leader,
        "foreground_processes": [{"pid": pid} for pid in (leader, *members) if pid]}})
    if session:
        fake.on("agent", "get", pane, result={"agent": {"agent_session": {"value": session}}})


def as_claude(monkeypatch, session, pid):
    monkeypatch.setenv("CLAUDE_CODE_SESSION_ID", session)
    monkeypatch.setenv("CLAUDE_PID", str(pid))


def restored(mesh, run, monkeypatch, fake, leader=5000, **entry):
    """The worker as Claude session `rec-s` in pane w2:p5, and this process as that session, relaunched there by
    a herdr restore: herdr's variables, none of mesh's. `leader` leads the pane's foreground group."""
    with mesh.editing_run(run) as data:
        data["agents"]["repo-fix-typo"].update({"kind": "claude", "view": "pane", "session": "rec-s", **entry})
    monkeypatch.setenv("HERDR_PANE_ID", "w2:p5")
    as_claude(monkeypatch, "rec-s", 5000)
    if leader:
        in_pane(fake, "w2:p5", leader)


def test_a_pane_worker_herdr_restored_without_its_identity_is_found_by_its_pane(mesh, fake, run, monkeypatch, capsys):
    restored(mesh, run, monkeypatch, fake)
    capsys.readouterr()
    assert mesh.main(["whoami"]) == 0
    assert json.loads(capsys.readouterr().out)["name"] == "repo-fix-typo"
    assert mesh.main(["whoami", "--run", run]) == 0
    assert json.loads(capsys.readouterr().out)["name"] == "repo-fix-typo"


@pytest.mark.parametrize("case", ["other session", "other pane", "nested", "herdr can't say", "retired", "agy",
                                  "other run", "archived"])
def test_a_pane_worker_is_found_by_its_pane_only_as_its_own_process(mesh, fake, run, monkeypatch, capsys, case):
    entry = {"retired": {"status": "retired"}, "agy": {"kind": "agy"}}.get(case, {})
    leader = {"nested": 7000, "herdr can't say": None}.get(case, 5000)  # nested: a `claude -c -p` of its tool
    restored(mesh, run, monkeypatch, fake, leader=leader, **entry)
    if case == "other session":
        monkeypatch.setenv("CLAUDE_CODE_SESSION_ID", "other-s")
    if case == "other pane":  # a pane id reused after herdr lost its session
        monkeypatch.setenv("HERDR_PANE_ID", "w2:p6")
        in_pane(fake, "w2:p6", 5000)
    if case == "herdr can't say":
        fake.on("pane", "process-info", "--pane", "w2:p5", error=("pane_not_found", "gone"))
    if case == "archived":
        (mesh.mesh_home() / "archive").mkdir()
        mesh.run_dir(run).rename(mesh.mesh_home() / "archive" / run)
    capsys.readouterr()
    assert mesh.main(["whoami", *(["--run", "mother"] if case == "other run" else [])]) == 1
    assert "not in a mesh run" in capsys.readouterr().err


def test_a_session_on_two_workers_of_a_pane_is_found_by_neither(mesh, fake, run, monkeypatch, capsys):
    restored(mesh, run, monkeypatch, fake)
    worker = mesh.load_run(run)["agents"]["repo-fix-typo"]
    (mesh.run_dir("mother")).mkdir()
    (mesh.run_dir("mother") / "run.json").write_text(json.dumps({"id": "mother", "agents": {"repo-old": worker}}))
    capsys.readouterr()
    assert mesh.main(["whoami"]) == 1
    assert "match several workers (repo-old of run mother, repo-fix-typo of run mtest)" in capsys.readouterr().err
    assert mesh.main(["whoami", "--run", run]) == 0  # its run tells them apart
    assert json.loads(capsys.readouterr().out)["name"] == "repo-fix-typo"


def test_a_name_passed_is_never_swapped_for_the_pane_worker(mesh, fake, run, monkeypatch, capsys):
    restored(mesh, run, monkeypatch, fake)
    capsys.readouterr()
    assert mesh.main(["whoami", "--as", "repo-lead"]) == 1  # no run to take it in: as before, an error
    assert "not in a mesh run" in capsys.readouterr().err
    assert mesh.main(["whoami", "--as", "repo-fix-typo"]) == 0
    assert json.loads(capsys.readouterr().out)["name"] == "repo-fix-typo"


def test_a_broken_run_file_doesnt_break_the_pane_worker_lookup(mesh, fake, run, monkeypatch, capsys):
    restored(mesh, run, monkeypatch, fake)
    for name, text in (("mbad1", '{"agents": {"x": null}}'), ("mbad2", '{"agents": []}'), ("mbad3", "[]")):
        mesh.run_dir(name).mkdir()
        (mesh.run_dir(name) / "run.json").write_text(text)
    capsys.readouterr()
    assert mesh.main(["whoami"]) == 0
    assert json.loads(capsys.readouterr().out)["name"] == "repo-fix-typo"


def test_an_inherited_identity_falls_back_to_the_pane_worker(mesh, fake, run, monkeypatch, capsys):
    restored(mesh, run, monkeypatch, fake)
    monkeypatch.setenv("MESH_RUN", "mold")  # a stopped run's identity, from the daemon that relaunched it
    monkeypatch.setenv("MESH_SELF", "proj-review")
    capsys.readouterr()
    assert mesh.main(["whoami"]) == 0
    out, err = capsys.readouterr()
    assert json.loads(out)["name"] == "repo-fix-typo" and "acting as repo-fix-typo, this pane's worker" in err
    assert [e["name"] for e in mesh.read_log(run) if e["event"] == "identity_ignored"] == ["repo-fix-typo"]


def test_a_pane_worker_herdr_restored_still_cant_stop_its_run(mesh, fake, run, monkeypatch, capsys):
    restored(mesh, run, monkeypatch, fake)
    capsys.readouterr()
    assert mesh.main(["stop", run]) == 1
    assert "repo-fix-typo is a worker of run mtest" in capsys.readouterr().err
    assert (mesh.run_dir(run) / "run.json").exists()


def test_a_pane_worker_on_two_runs_cant_stop_either(mesh, fake, run, monkeypatch, capsys):
    restored(mesh, run, monkeypatch, fake)
    worker = mesh.load_run(run)["agents"]["repo-fix-typo"]
    mesh.run_dir("mother").mkdir()
    (mesh.run_dir("mother") / "run.json").write_text(json.dumps({"id": "mother", "root": "x",
                                                                 "agents": {"repo-old": worker}}))
    capsys.readouterr()
    for target in ("mother", run):
        assert mesh.main(["stop", target, "--run", target]) == 1
        err = capsys.readouterr().err
        assert "repo-old is a worker of run mother" in err and "pass --run and --as" not in err
    assert (mesh.run_dir("mother") / "run.json").exists() and (mesh.run_dir(run) / "run.json").exists()


def test_a_pane_worker_herdr_restored_cant_stop_another_run_by_naming_it(mesh, fake, run, monkeypatch, capsys):
    restored(mesh, run, monkeypatch, fake)
    mesh.run_dir("mother").mkdir()
    (mesh.run_dir("mother") / "run.json").write_text(json.dumps({"id": "mother", "root": "x", "agents": {}}))
    capsys.readouterr()
    assert mesh.main(["stop", "mother", "--run", "mother"]) == 1
    assert "repo-fix-typo is a worker of run mtest" in capsys.readouterr().err
    assert (mesh.run_dir("mother") / "run.json").exists()


@pytest.mark.parametrize("herdr_says", ["root-s", "child-s", None])
def test_the_root_in_its_own_pane_is_the_root(mesh, fake, run, monkeypatch, capsys, herdr_says):
    # A mesh command from the root's own tool. herdr's session report may name a `claude -c -p` the root ran
    # from a tool (its hook reports `resume` for the pane, and herdr takes it): the process decides.
    as_claude(monkeypatch, "root-s", 4000)
    in_pane(fake, "w1:p1", 4000, 4100, session=herdr_says)
    capsys.readouterr()
    assert mesh.main(["whoami"]) == 0
    assert json.loads(capsys.readouterr().out)["name"] == "repo-lead"


def test_a_session_that_isnt_the_root_panes_process_is_not_the_root(mesh, fake, run, monkeypatch, capsys):
    # A background session whose daemon the user started in the root's pane; its own hook made herdr report it.
    as_claude(monkeypatch, "bg-s", 7000)
    (Path(os.environ["MESH_CC_SOCKS"])).mkdir(parents=True, exist_ok=True)
    (Path(os.environ["MESH_CC_SOCKS"]) / "7000.sock").write_text("")
    in_pane(fake, "w1:p1", 4000, session="bg-s")
    capsys.readouterr()
    assert mesh.main(["whoami"]) == 1
    err = capsys.readouterr().err
    assert "not in a mesh run" in err and "Claude session bg-s isn't the process running there" in err
    assert "--run mtest --as repo-lead" in err
    assert mesh.load_run(run)["agents"]["repo-lead"]["address"] is None  # its socket isn't taken over


def test_a_resumed_root_is_the_panes_process_and_records_its_new_socket(mesh, fake, run, monkeypatch, capsys):
    as_claude(monkeypatch, "root-s2", 4500)
    (Path(os.environ["MESH_CC_SOCKS"])).mkdir(parents=True, exist_ok=True)
    (Path(os.environ["MESH_CC_SOCKS"]) / "4500.sock").write_text("")
    in_pane(fake, "w1:p1", 4500)
    assert mesh.main(["whoami"]) == 0
    assert mesh.load_run(run)["agents"]["repo-lead"]["address"].endswith("/4500.sock")


def test_a_member_of_the_root_panes_foreground_that_isnt_its_leader_is_not_the_root(mesh, fake, run, monkeypatch,
                                                                                   capsys):
    # A Claude started by an MCP server or a hook shares the pane's foreground group without leading it.
    as_claude(monkeypatch, "mcp-s", 4100)
    in_pane(fake, "w1:p1", 4000, 4100)
    capsys.readouterr()
    assert mesh.main(["whoami"]) == 1
    assert "isn't the process running there" in capsys.readouterr().err


def test_without_a_group_leader_the_foreground_members_decide(mesh, fake, run, monkeypatch, capsys):
    as_claude(monkeypatch, "root-s", 4000)
    fake.on("pane", "process-info", "--pane", "w1:p1", result={"process_info": {"foreground_processes": [{"pid": 5}]}})
    assert mesh.main(["whoami"]) == 1
    fake.on("pane", "process-info", "--pane", "w1:p1", result={"process_info": {"foreground_processes": [{"pid": 4000}]}})
    assert mesh.main(["whoami"]) == 0

def test_agent_binary_reads_the_command_not_the_path(mesh):
    assert mesh.agent_binary(["claude", "--resume", "s1"]) == "claude"
    assert mesh.agent_binary(["/home/u/.local/share/claude/versions/2.1.310/claude"]) == "claude"  # auto-updated
    assert mesh.agent_binary(["node", "/home/u/.nvm/versions/node/v24/bin/claude"]) == "claude"
    assert mesh.agent_binary(["agy", "--dangerously-skip-permissions"]) == "agy"
    assert mesh.agent_binary(["/bin/bash", "-c", "claude -p hi"]) is None  # a shell tool's command
    assert mesh.agent_binary([]) is None and mesh.agent_binary(None) is None


def test_a_worker_herdr_lost_is_gone_only_if_its_pane_runs_no_cli(mesh, fake, run):
    # after a Claude Code auto-update herdr can show no agent for a live one
    fake.on("agent", "get", errors=["agent_not_found"] * 10)
    entry = mesh.agent_entry(mesh.load_run(run), "repo-fix-typo")  # agy, in pane w2:p5

    def pane_runs(*argv):
        fake.on("pane", "process-info", "--pane", "w2:p5", result={"process_info": {
            "foreground_process_group_id": 9, "shell_pid": 8, "foreground_processes": [{"pid": 9, "argv": list(argv)}]}})

    pane_runs("agy", "--dangerously-skip-permissions")
    assert mesh.agent_status("repo-fix-typo", entry) == "unknown"
    pane_runs("-bash")
    assert mesh.agent_status("repo-fix-typo", entry) == "gone"
    pane_runs("claude")  # not its CLI
    assert mesh.agent_status("repo-fix-typo", entry) == "gone"
    for code in ("pane_not_found", "cli_error"):  # closed, or herdr can't say: its registration stands
        fake.on("pane", "process-info", "--pane", "w2:p5", error=code)
        assert mesh.agent_status("repo-fix-typo", entry) == "gone"
        assert fake.calls[-1][:2] == ("pane", "process-info")
    assert mesh.agent_status("repo-fix-typo") == "gone"  # no entry, no pane to look at


def test_a_claude_session_in_an_agy_roots_pane_is_not_the_root(mesh, fake, run, monkeypatch, capsys):
    with mesh.editing_run(run) as data:
        data["agents"]["repo-lead"]["kind"] = "agy"
    as_claude(monkeypatch, "other-s", 4000)
    capsys.readouterr()
    assert mesh.main(["whoami"]) == 1
    assert "which is agy, and this is Claude session other-s" in capsys.readouterr().err


def test_a_refused_session_gets_past_with_as(mesh, fake, run, monkeypatch, capsys):
    as_claude(monkeypatch, "child-s", 6000)
    in_pane(fake, "w1:p1", 4000)
    assert mesh.main(["whoami", "--run", run, "--as", "repo-lead"]) == 0
    assert json.loads(capsys.readouterr().out)["name"] == "repo-lead"


def test_a_refused_sessions_ask_starts_no_run(mesh, fake, run, repo, monkeypatch, capsys):
    # A nested `claude -p` in the root's pane: it gets the refusal, not a run of its own over the root's pane map.
    as_claude(monkeypatch, "child-s", 6000)
    in_pane(fake, "w1:p1", 4000, session="root-s")
    fake.on("agent", "list", result={"agents": [{"pane_id": "w1:p1", "agent": "claude", "cwd": "/x/repo"}]})
    mapping = (mesh.mesh_home() / "by-pane" / "w1_p1.json").read_text()
    capsys.readouterr()
    assert mesh.main(["ask", "--repo", str(repo), "--body", "why?"]) == 1
    assert "isn't the process running there" in capsys.readouterr().err
    assert (mesh.mesh_home() / "by-pane" / "w1_p1.json").read_text() == mapping
    assert sorted(p.name for p in mesh.mesh_home().iterdir() if (p / "run.json").exists()) == [run]


def test_init_refuses_a_claude_that_isnt_the_panes_process(mesh, fake, monkeypatch, capsys):
    fake.on("agent", "list", result={"agents": [{"pane_id": "w1:p1", "agent": "claude", "cwd": "/x/repo"}]})
    monkeypatch.setenv("HERDR_PANE_ID", "w1:p1")
    as_claude(monkeypatch, "child-s", 6000)
    in_pane(fake, "w1:p1", 4000)
    capsys.readouterr()
    assert mesh.main(["init", "--mode", "claude"]) == 1
    err = capsys.readouterr().err
    assert "isn't the process running in pane w1:p1" in err and "`exec claude`" in err  # how a wrapper gets there
    assert not (mesh.mesh_home() / "by-pane" / "w1_p1.json").exists()


@pytest.mark.parametrize("herdr_says", ["other-s", None])
def test_a_nested_claude_in_a_workers_pane_is_not_the_worker(mesh, fake, run, monkeypatch, capsys, herdr_says):
    # A `claude -c -p` from the worker's tool inherits its MESH_SELF and pane, and herdr may take its report.
    inherited(mesh, run, monkeypatch, view="pane")
    monkeypatch.setenv("HERDR_PANE_ID", "w2:p5")
    monkeypatch.setenv("CLAUDE_PID", "6000")
    in_pane(fake, "w2:p5", 5000, session=herdr_says)
    capsys.readouterr()
    assert mesh.main(["whoami"]) == 1
    err = capsys.readouterr().err
    assert "ignored MESH_SELF=repo-fix-typo" in err and "--as repo-fix-typo" in err
    assert mesh.load_run(run)["agents"]["repo-fix-typo"]["session"] == "rec-s"


def fake_ancestry(mesh, monkeypatch, *sockets):
    """os.getpid() → 9001 → 9002 → 9003 → 1, with a `<pid>.sock` file (live or stale) at each of `sockets`."""
    chain = [os.getpid(), 9001, 9002, 9003, 1]
    monkeypatch.setattr(mesh, "parent_pid", lambda pid: chain[chain.index(pid) + 1] if pid in chain[:-1] else 0)
    socks = Path(os.environ["MESH_CC_SOCKS"])
    socks.mkdir(parents=True, exist_ok=True)
    for pid in sockets:
        (socks / f"{pid}.sock").write_text("")


def test_a_stale_socket_up_the_tree_doesnt_cost_a_pane_worker_its_identity(mesh, fake, run, monkeypatch, capsys):
    # A socket file a crashed Claude left at a pid since reused by this worker's own ancestors.
    fake_ancestry(mesh, monkeypatch, 9001, 9003)
    inherited(mesh, run, monkeypatch, view="pane")  # after `/clear`
    monkeypatch.setenv("HERDR_PANE_ID", "w2:p5")
    monkeypatch.setenv("CLAUDE_PID", "9003")
    in_pane(fake, "w2:p5", 9003)
    capsys.readouterr()
    assert mesh.main(["whoami"]) == 0
    assert json.loads(capsys.readouterr().out)["name"] == "repo-fix-typo"
    assert mesh.load_run(run)["agents"]["repo-fix-typo"]["session"] == "other-s"


def test_claude_address_is_claude_pids_socket(mesh, monkeypatch):
    fake_ancestry(mesh, monkeypatch, 9001, 9003)
    assert mesh.claude_address().endswith("/9001.sock")  # no CLAUDE_PID: the nearest socket up the tree
    monkeypatch.setenv("CLAUDE_PID", "9003")
    assert mesh.claude_address().endswith("/9003.sock")  # not the stale one nearer


def test_a_bg_worker_keeps_its_identity_wherever_it_runs(mesh, fake, cli, run, monkeypatch, capsys):
    # A daemon's spare is no pane's process: only pane identities ask herdr.
    inherited(mesh, run, monkeypatch, view="bg", pane=None)
    as_claude(monkeypatch, "rec-s", 7000)
    in_pane(fake, "w1:p1", 4000)
    capsys.readouterr()
    assert mesh.main(["whoami"]) == 0
    assert json.loads(capsys.readouterr().out)["name"] == "repo-fix-typo"


def test_an_entry_starting_for_too_long_is_checked_again(mesh, fake, run, monkeypatch, capsys):
    inherited(mesh, run, monkeypatch, view="bg", pane=None, status="starting", spawned="2026-01-01T00:00:00+00:00",
              models=[{"model": "opus", "at": "2026-01-01T00:01:00+00:00"}])
    monkeypatch.delenv("HERDR_PANE_ID")
    capsys.readouterr()
    assert mesh.main(["whoami"]) == 1
    assert "ignored MESH_SELF=repo-fix-typo" in capsys.readouterr().err


# ── send / inbox ─────────────────────────────────────────────────────────


def test_message_is_stored_before_the_doorbell(mesh, fake, run, monkeypatch):
    stored_at_ring = []

    def prompt(*args, timeout=None):
        fake.calls.append(args)
        if args[:2] == ("agent", "prompt"):
            stored_at_ring.append(len(mesh.unread(run, "repo-fix-typo")))
        return {}

    monkeypatch.setattr(mesh, "herdr", prompt)
    assert mesh.main(["send", "repo-fix-typo", "--type", "answer", "--body", "Use the bare number."]) == 0
    assert stored_at_ring == [1]


def test_doorbell_retries_while_blocked(mesh, fake, run, capsys):
    fake.on("agent", "prompt", errors=["agent_blocked", "agent_blocked"])
    assert mesh.main(["send", "repo-fix-typo", "--type", "info", "--body", "hi"]) == 0
    assert len(fake.called("agent", "prompt")) == 3
    assert [e["tries"] for e in mesh.read_log(run) if e["event"] == "rung"] == [1]  # refused prompts typed nothing


def test_failed_doorbell_keeps_the_message(mesh, fake, run, capsys):
    fake.on("agent", "prompt", errors=["agent_blocked"] * 20)
    assert mesh.main(["send", "repo-fix-typo", "--type", "question", "--body", "Which base branch?"]) == 2
    assert "did not ring" in capsys.readouterr().err
    assert len(mesh.unread(run, "repo-fix-typo")) == 1
    events = [e["event"] for e in mesh.read_log(run)]
    assert "doorbell_failed" in events


def test_doorbell_falls_back_to_pane_id(mesh, fake, run):
    fake.on("agent", "prompt", "repo-fix-typo", error="agent_not_found")
    assert mesh.main(["send", "repo-fix-typo", "--type", "info", "--body", "hi"]) == 0
    assert fake.called("agent", "prompt", "w2:p5")
    rung = [e for e in mesh.read_log(run) if e["event"] == "rung"]
    assert [(e["via"], e["tries"]) for e in rung] == [("working", 1)]  # the name herdr didn't know typed nothing


def test_ring_reminds_of_unread_messages(mesh, fake, run, monkeypatch, capsys):
    fixed_token(mesh, monkeypatch)
    assert mesh.main(["ring", "repo-fix-typo"]) == 0
    assert "no unread messages" in capsys.readouterr().out
    mesh.record(run, "repo-lead", "repo-fix-typo", "task", "Fix it.")
    assert mesh.main(["ring", "repo-fix-typo"]) == 0
    assert json.loads(capsys.readouterr().out)["via"] == "working"
    assert fake.called("agent", "prompt")[-1][3] == "📬 [mesh] (ref abc123) you have 1 unread message(s) (run `mesh inbox`)"
    rung = [e for e in mesh.read_log(run) if e["event"] == "rung"]
    assert [(e["to"], e["via"], e["file"]) for e in rung] == [("repo-fix-typo", "working", mesh.unread(run, "repo-fix-typo")[-1].name)]


def test_send_to_parent_and_inbox_marks_read(mesh, fake, run, monkeypatch, capsys):
    as_worker(monkeypatch, run)
    assert mesh.main(["send", "parent", "--type", "question", "--body", "Include _db.py?"]) == 0
    doorbell = fake.called("agent", "prompt")[-1]
    assert doorbell[2] == "repo-lead"
    assert bare(doorbell[3]).startswith("📬 [mesh] question from repo-fix-typo: Include _db.py?")

    monkeypatch.delenv("MESH_RUN")
    monkeypatch.delenv("MESH_SELF")  # back to the root (pane map)
    capsys.readouterr()
    assert mesh.main(["inbox", "--peek"]) == 0
    assert "Include _db.py?" in capsys.readouterr().out
    assert len(mesh.unread(run, "repo-lead")) == 1
    assert mesh.main(["inbox"]) == 0
    assert mesh.unread(run, "repo-lead") == []
    capsys.readouterr()
    mesh.main(["inbox"])
    assert "No unread messages." in capsys.readouterr().out
    mesh.main(["inbox", "--all", "--json"])
    assert json.loads(capsys.readouterr().out)[0]["type"] == "question"


def test_the_reminder_restates_who_the_worker_is_and_its_task(mesh, fake, run, monkeypatch, capsys):
    with mesh.editing_run(run) as data:
        data["agents"]["repo-fix-typo"]["phase"] = "plan"
    mesh.deliver(run, "repo-lead", "repo-fix-typo", "task", "# Fix the typo\n\nIn README.md.")
    capsys.readouterr()
    assert mesh.main(["whoami", "--reminder", "compact", "--run", run, "--as", "repo-fix-typo"]) == 0
    out = capsys.readouterr().out
    assert out.startswith("agent-mesh reminder (compact): you are repo-fix-typo, a worker in run mtest, and your "
                          "parent is repo-lead.")
    assert "Current task: Fix the typo (" in out and "-repo-lead-task.md" in out
    assert "Phase: planning: research, then send a `plan`" in out
    assert "it shows your current task again" in out
    assert "Run `mesh inbox --run mtest --as repo-fix-typo` now" in out  # a cleared bg session can't rebind
    assert "(worker) in agent-mesh run" not in out  # agy's conversation lookup searches for the first doorbell
    assert [(e["name"], e["source"]) for e in mesh.read_log(run) if e["event"] == "reminder"] == \
        [("repo-fix-typo", "compact")]
    mesh.deliver(run, "repo-fix-typo", "repo-lead", "plan", "The plan.")
    mesh.main(["whoami", "--reminder", "resume", "--run", run, "--as", "repo-fix-typo"])
    out = capsys.readouterr().out
    assert "Phase: your plan is waiting for its answer" in out and "again" not in out
    mesh.deliver(run, "repo-lead", "repo-fix-typo", "task", f"{mesh.USER_REVIEW_PREFIX}\n\nBuild it.")
    mesh.main(["whoami", "--reminder", "compact", "--run", run, "--as", "repo-fix-typo"])
    out = capsys.readouterr().out
    assert "Phase: a plan the user reviews: if the user hasn't approved your plan in your pane yet" in out
    assert "once it's approved, make the change" in out
    mesh.deliver(run, "repo-fix-typo", "repo-lead", "done", "Built.")
    mesh.main(["whoami", "--reminder", "compact", "--run", run, "--as", "repo-fix-typo"])
    out = capsys.readouterr().out
    assert "you reported `done`" in out and "again" not in out  # nothing to show again
    assert mesh.main(["whoami", "--reminder", "clear", "--run", "gone", "--as", "x"]) == 0  # never an error
    assert capsys.readouterr().out == ""


def test_inbox_shows_the_current_task_again_once_after_a_reset(mesh, fake, run, monkeypatch, capsys):
    as_worker(monkeypatch, run)
    mesh.deliver(run, "repo-lead", "repo-fix-typo", "task", "Fix the typo.")
    mesh.deliver(run, "repo-lead", "repo-fix-typo", "answer", "Both files.")
    mesh.main(["inbox"])
    mesh.log_event(run, "reminder", name="repo-fix-typo", source="resume")  # a relaunch keeps its context
    capsys.readouterr()
    mesh.main(["inbox"])
    assert capsys.readouterr().out == "No unread messages.\n"
    mesh.log_event(run, "rebind", name="repo-fix-typo", session="new-s")  # a /clear
    mesh.deliver(run, "repo-lead", "repo-fix-typo", "info", "FYI.")
    mesh.main(["inbox"])
    out = capsys.readouterr().out
    assert out.count("(again: your session was cleared or compacted") == 2
    assert out.index("Fix the typo.") < out.index("Both files.") < out.index("FYI.")
    mesh.main(["inbox"])
    assert capsys.readouterr().out == "No unread messages.\n"
    # a new task after the reset is simply unread: nothing is shown twice
    mesh.log_event(run, "reminder", name="repo-fix-typo", source="compact")
    mesh.deliver(run, "repo-lead", "repo-fix-typo", "task", "Next one.")
    mesh.main(["inbox", "--json"])
    assert [(m["body"], m.get("again")) for m in json.loads(capsys.readouterr().out)] == [("Next one.", None)]
    # an answer it never read, after a reset: shown as new, not "again"
    mesh.deliver(run, "repo-lead", "repo-fix-typo", "answer", "Unread one.")
    mesh.log_event(run, "rebind", name="repo-fix-typo", session="newer-s")
    mesh.main(["inbox", "--json"])
    assert [(m["body"], m.get("again")) for m in json.loads(capsys.readouterr().out)] == \
        [("Next one.", True), ("Unread one.", None)]


def test_inbox_does_not_show_a_reported_task_again_and_all_counts_as_shown(mesh, fake, run, monkeypatch, capsys):
    as_worker(monkeypatch, run)
    mesh.deliver(run, "repo-lead", "repo-fix-typo", "task", "Fix the typo.")
    mesh.main(["inbox"])
    mesh.deliver(run, "repo-fix-typo", "repo-lead", "done", "Fixed.")
    mesh.log_event(run, "reminder", name="repo-fix-typo", source="compact")
    capsys.readouterr()
    mesh.main(["inbox"])  # it reported done on it: nothing to remind it of
    assert capsys.readouterr().out == "No unread messages.\n"
    mesh.deliver(run, "repo-lead", "repo-fix-typo", "task", "Next one.")
    mesh.main(["inbox"])
    mesh.log_event(run, "reminder", name="repo-fix-typo", source="clear")
    capsys.readouterr()
    mesh.main(["inbox", "--all", "--json"])  # shows everything, the current task marked, and counts as shown
    assert [(m["body"], m.get("again")) for m in json.loads(capsys.readouterr().out)] == \
        [("Fix the typo.", None), ("Next one.", True)]
    mesh.main(["inbox"])
    assert capsys.readouterr().out == "No unread messages.\n"


def test_send_rejects_unknown_agent_and_empty_body(mesh, fake, run, capsys):
    assert mesh.main(["send", "nobody", "--type", "info", "--body", "x"]) == 1
    assert mesh.main(["send", "repo-fix-typo", "--type", "info", "--body", "  "]) == 1


def wait(mesh, until="30m", why="The full suite runs in the background."):
    return mesh.main(["send", "parent", "--type", "wait", "--until", until, "--body", why])


def test_a_wait_is_stored_and_logged_without_a_doorbell(mesh, fake, run, monkeypatch, capsys):
    mesh.deliver(run, "repo-lead", "repo-fix-typo", "task", "Fix the typo.")
    as_worker(monkeypatch, run)
    assert wait(mesh) == 1  # its task is still unread: the wait would belong to a task it hasn't read
    err = capsys.readouterr().err
    assert "not sent" in err and "task from repo-lead at " in err and "Fix the typo." in err
    assert "hold off the watch for a task you haven't read" in err and "act on them, first" in err
    assert "count as your reply" not in err and "--unread-ok" not in err  # a report's way out only
    assert mesh.main(["send", "parent", "--type", "wait", "--until", "30m", "--body", "Tests.", "--unread-ok"]) == 1
    assert mesh.unread(run, "repo-lead") == []
    mesh.main(["inbox"])
    rings = len(fake.called("agent", "prompt"))
    capsys.readouterr()
    assert wait(mesh) == 0
    captured = capsys.readouterr()
    out = json.loads(captured.out)
    assert "for ending this turn" in captured.err and "send a fresh wait" in captured.err
    assert len(fake.called("agent", "prompt")) == rings  # asks nothing of the parent
    event = [e for e in mesh.read_log(run) if e["event"] == "send"][-1]
    assert (event["type"], event["until"]) == ("wait", out["until"])
    assert 29 * 60 < mesh.epoch(event["until"]) - time.time() <= 30 * 60
    body = mesh.unread(run, "repo-lead")[-1].read_text()
    assert "Waiting until " in body and "The full suite runs in the background." in body
    assert mesh.since_last_task(mesh.read_log(run), "repo-fix-typo")[1] == 0  # not a report
    assert mesh.declared_wait(mesh.read_log(run), "repo-fix-typo")["until"] == out["until"]
    mesh.deliver(run, "repo-lead", "repo-fix-typo", "answer", "Also the other file.")
    assert mesh.declared_wait(mesh.read_log(run), "repo-fix-typo") is None  # a new answer ends it


def test_a_wait_needs_a_near_future_until_and_comes_from_a_worker(mesh, fake, run, monkeypatch, capsys):
    as_worker(monkeypatch, run)
    assert mesh.main(["send", "parent", "--type", "wait", "--body", "x"]) == 1
    assert mesh.main(["send", "parent", "--type", "info", "--until", "5m", "--body", "x"]) == 1
    assert wait(mesh, "2000-01-01T00:00:00+00:00") == 1
    assert wait(mesh, "5h") == 1
    assert wait(mesh, "soon") == 1
    err = capsys.readouterr().err
    assert "needs --until" in err and "is for `--type wait`" in err and "in the past" in err
    assert "more than 4 h away" in err and "give a duration" in err
    assert mesh.main(["send", "repo-lead", "--type", "wait", "--until", "5m", "--body", "x", "--as", "repo-lead"]) == 1


def test_until_takes_a_duration_a_clock_time_or_iso(mesh):
    now = mesh.datetime(2026, 10, 25, 12, 0, tzinfo=mesh.timezone.utc)
    assert mesh.parse_until("1h30m", now) - now == mesh.timedelta(minutes=90)
    assert mesh.parse_until("90s", now) - now == mesh.timedelta(seconds=90)
    assert mesh.parse_until("2026-10-25T13:00:00+00:00", now) - now == mesh.timedelta(hours=1)
    local = now.astimezone()
    later = mesh.parse_until(f"{(local.hour + 1) % 24:02d}:{local.minute:02d}", now)
    assert later - now == mesh.timedelta(hours=1)
    earlier = mesh.parse_until(f"{local.hour:02d}:{local.minute:02d}", now)  # now, so the next one: tomorrow
    assert earlier - now == mesh.timedelta(days=1)
    with pytest.raises(mesh.MeshError):
        mesh.parse_until("25:00", now)


def test_until_takes_a_z_suffix_and_refuses_a_huge_duration(mesh):
    now = mesh.datetime(2026, 10, 25, 12, 0, tzinfo=mesh.timezone.utc)
    assert mesh.parse_until("2026-10-25T13:00:00Z", now) - now == mesh.timedelta(hours=1)  # 3.10's fromisoformat
    with pytest.raises(mesh.MeshError, match="more than 4 h away"):
        mesh.parse_until("99999999999999h", now)


def test_a_clock_time_across_a_dst_change_keeps_its_wall_time(mesh, monkeypatch):
    monkeypatch.setenv("TZ", "Europe/Berlin")
    time.tzset()
    try:
        now = mesh.datetime(2026, 10, 24, 23, 0, tzinfo=mesh.timezone.utc)  # 01:00 CEST; clocks go back at 03:00
        at = mesh.parse_until("09:00", now)  # after the change: 09:00 CET is 08:00 UTC, not 07:00
        assert at == mesh.datetime(2026, 10, 25, 8, 0, tzinfo=mesh.timezone.utc)
    finally:
        monkeypatch.delenv("TZ")
        time.tzset()


@pytest.mark.parametrize("mtype", ["plan", "question", "done", "failed"])
def test_a_report_waits_until_the_workers_task_is_read(mesh, fake, run, monkeypatch, capsys, mtype):
    mesh.deliver(run, "repo-lead", "repo-fix-typo", "task", "# Task 2: add retries\n\nDetails.")
    as_worker(monkeypatch, run)
    capsys.readouterr()
    assert mesh.main(["send", "parent", "--type", mtype, "--body", "Task 1 is done."]) == 1
    err = capsys.readouterr().err
    assert "not sent" in err and "task from repo-lead at " in err and "# Task 2: add retries" in err
    assert "`mesh inbox`" in err and "--unread-ok" in err
    assert mesh.unread(run, "repo-lead") == []  # nothing stored
    assert mesh.main(["inbox", "--peek"]) == 0  # a peek leaves it unread
    assert mesh.main(["send", "parent", "--type", mtype, "--body", "Task 1 is done."]) == 1
    assert mesh.main(["inbox"]) == 0
    assert mesh.main(["send", "parent", "--type", mtype, "--body", "Tasks 1 and 2 are done."]) == 0
    assert len(mesh.unread(run, "repo-lead")) == 1


def test_an_unread_answer_holds_a_report_and_info_or_the_flag_gets_through(mesh, fake, run, monkeypatch, capsys):
    mesh.deliver(run, "repo-lead", "repo-fix-typo", "answer", "changes: split it in two")
    as_worker(monkeypatch, run)
    assert mesh.main(["send", "parent", "--type", "plan", "--body", "The old plan."]) == 1
    assert "answer from repo-lead" in capsys.readouterr().err
    assert mesh.main(["send", "parent", "--type", "info", "--body", "Still on it."]) == 0
    assert mesh.main(["send", "parent", "--type", "done", "--body", "As is.", "--unread-ok"]) == 0
    assert len(mesh.unread(run, "repo-lead")) == 2
    assert len(mesh.unread(run, "repo-fix-typo")) == 1  # still unread: the flag reads nothing


def test_the_root_sends_whatever_its_mailbox_holds(mesh, fake, run, monkeypatch):
    as_worker(monkeypatch, run)
    assert mesh.main(["send", "parent", "--type", "question", "--body", "Include _db.py?"]) == 0
    monkeypatch.delenv("MESH_RUN")
    monkeypatch.delenv("MESH_SELF")  # the root, by its pane, with the question unread
    assert mesh.main(["send", "repo-fix-typo", "--type", "answer", "--body", "Yes."]) == 0
    assert mesh.main(["send", "repo-fix-typo", "--type", "task", "--body", "And the tests."]) == 0


def test_first_message_introduces_the_agent(mesh, fake, run):
    mesh.deliver(run, "repo-lead", "repo-fix-typo", "task", "Do X.", first=True)
    text = fake.called("agent", "prompt")[-1][3]
    assert "You are repo-fix-typo (worker)" in text
    assert "your parent is repo-lead" in text
    assert "PROTOCOL.md" in text


# ── models ───────────────────────────────────────────────────────────────

AGY_MODELS = """gemini-4-argon-medium\tGemini 4 Argon (Medium)
gemini-4-argon-high\tGemini 4 Argon (High)
gemini-3.8-flash-high\tGemini 3.8 Flash (High)
gemini-3.8-flash-medium\tGemini 3.8 Flash (Medium)
gemini-3.8-flash-low\tGemini 3.8 Flash (Low)
gemini-3.10-flash-low\tGemini 3.10 Flash (Low)
claude-opus-5-5-high\tClaude Opus 5.5 (High)
claude-sonnet-5-5-high\tClaude Sonnet 5.5 (High)
"""

USAGE = """Gemini Models\tWeekly Limit Remaining\t74%\t2026-10-29T17:21:38Z
Gemini Models\tFive Hour Limit Remaining\t52%\t2026-10-24T05:50:54Z
Claude and GPT models\tWeekly Limit Remaining\t49%\t2026-10-30T19:57:12Z
Claude and GPT models\tFive Hour Limit Remaining\t100%\t2026-10-24T10:11:57Z
Gemini 4 Argon\tGemini 4 Argon Weekly Limit\t0%\t2026-10-29T17:26:00Z
"""


def with_quotas(mesh, monkeypatch, usage=USAGE):
    """A fresh `agy models` cache and fixed quotas, so nothing runs agy."""
    mesh.write_atomic(mesh.mesh_home() / "agy-models.txt", AGY_MODELS)
    monkeypatch.setattr(mesh, "agy_usage", lambda force=False: mesh.parse_usage(usage))


def test_pick_agy_model_newest_family_and_effort(mesh):
    assert mesh.pick_agy_model(AGY_MODELS, "flash", "low") == "gemini-3.10-flash-low"  # 3.10 is newer than 3.8
    assert mesh.pick_agy_model(AGY_MODELS, "flash", "high") == "gemini-3.8-flash-high"
    assert mesh.pick_agy_model(AGY_MODELS, "argon", "high") == "gemini-4-argon-high"
    assert mesh.pick_agy_model(AGY_MODELS, "gemini-3.8-flash-high", "low") == "gemini-3.8-flash-high"
    with pytest.raises(mesh.MeshError):
        mesh.pick_agy_model(AGY_MODELS, "flash", "max")


def test_pick_agy_model_resolves_claude_families(mesh):
    assert mesh.pick_agy_model(AGY_MODELS, "opus", "high") == "claude-opus-5-5-high"
    assert mesh.pick_agy_model(AGY_MODELS, "sonnet", "high") == "claude-sonnet-5-5-high"
    assert mesh.resolve_agy_model("claude-sonnet-5-5-low", "high") == "claude-sonnet-5-5-low"  # exact id


def test_usage_rows_give_each_model_its_tightest_limit(mesh):
    rows = mesh.parse_usage("Fetching...\n" + USAGE)
    assert len(rows) == 5 and rows[0]["left"] == 74
    assert mesh.model_quota("gemini-4-argon-high", "Gemini 4 Argon (High)", rows) == (0, "2026-10-29T17:26:00Z")
    assert mesh.model_quota("gemini-3.8-flash-high", "Gemini 3.8 Flash (High)", rows)[0] == 52
    assert mesh.model_quota("claude-opus-5-5-high", "Claude Opus 5.5 (High)", rows)[0] == 49
    assert mesh.model_quota("gpt-oss-120b-medium", "GPT-OSS 120B (Medium)", rows)[0] == 49
    assert mesh.model_quota("other-model", "Other", rows) is None


def test_auto_plans_on_the_first_model_with_quota(mesh, monkeypatch):
    with_quotas(mesh, monkeypatch)
    chain = mesh.PLAN_CHAIN["agy"]
    assert mesh.choose_agy_model(chain, "high")[0] == "claude-opus-5-5-high"
    assert mesh.choose_agy_model(chain, "high", skip=("claude-opus-5-5-high",))[0] == "gemini-3.8-flash-high"
    with_quotas(mesh, monkeypatch, USAGE.replace("74%", "5%").replace("49%", "3%"))
    model, notes = mesh.choose_agy_model(chain, "high")
    assert model == "gemini-3.8-flash-high" and "every model is under 10%" in notes[-1]
    with pytest.raises(mesh.MeshError, match="resets 2026-10-29"):
        mesh.choose_agy_model(chain, "high", fallback=False)
    monkeypatch.setenv("MESH_MIN_QUOTA", "4")
    assert mesh.choose_agy_model(chain, "high")[0] == "gemini-3.8-flash-high"
    monkeypatch.setattr(mesh, "agy_usage", lambda force=False: None)  # quotas unreadable: never block
    assert mesh.choose_agy_model(chain, "high")[0] == "gemini-4-argon-high"


def test_usage_is_cached_and_force_reads_it_again(mesh, monkeypatch):
    calls = []

    def fake_run(argv, **kwargs):
        calls.append(argv)
        return subprocess.CompletedProcess(argv, 0, stdout=USAGE, stderr="")

    monkeypatch.setattr(mesh.subprocess, "run", fake_run)
    assert mesh.agy_usage()[4]["left"] == 0
    assert mesh.agy_usage()[0]["left"] == 74 and len(calls) == 1  # from the cache
    mesh.agy_usage(force=True)
    assert len(calls) == 2 and calls[0][:3] == ["agy", "-p", "/usage"]
    monkeypatch.setattr(mesh.subprocess, "run", lambda argv, **k: subprocess.CompletedProcess(argv, 1, "", "offline"))
    assert mesh.agy_usage(force=True) is None


def test_claude_argv_keeps_alias_and_picks_the_session_id(mesh):
    argv, model, session = mesh.agent_argv("claude", "opus", "high")
    assert model == "opus"
    assert argv == ["--session-id", session, "--model", "opus", "--effort", "high", "--dangerously-skip-permissions"]
    assert mesh.agent_argv("claude", "opus", "high")[2] != session


# ── watch ────────────────────────────────────────────────────────────────


def test_watch_returns_when_child_reports_done(mesh, fake, run, monkeypatch):
    fake.on("agent", "get", result={"agent": {"agent_status": "working"}})

    def sleep(_):
        if not mesh.sends_from(run, "repo-fix-typo", ("done",)):
            mesh.deliver(run, "repo-fix-typo", "repo-lead", "done", "All green.")

    monkeypatch.setattr(mesh.time, "sleep", sleep)
    assert mesh.main(["watch", "repo-fix-typo"]) == 0


def states_then(mesh, monkeypatch, *states):
    """agent_status answers `states` in turn, then the last one for good."""
    it = iter(states)
    monkeypatch.setattr(mesh, "agent_status", lambda name, *_: next(it, states[-1]))


def test_watch_nudges_a_silent_stop_once_then_alerts(mesh, fake, run, monkeypatch, capsys):
    mesh.deliver(run, "repo-lead", "repo-fix-typo", "task", "Fix the typo.")
    states_then(mesh, monkeypatch, "working", "working", "idle")
    fake.screen("repo-fix-typo", "● Edited README.md\n─────────────────\n> \n─────────────────\n? for shortcuts")
    assert mesh.main(["watch", "repo-fix-typo"]) == 3
    out = capsys.readouterr().out
    assert "without sending a message (nudged once, its task is still unread)" in out
    assert "last lines of its pane:\n    ● Edited README.md" in out
    nudges = [e for e in mesh.read_log(run) if e["event"] == "nudge"]
    assert [(e["name"], e["received"]) for e in nudges] == [("repo-fix-typo", 1)]
    assert "turn ended without a mesh send" in fake.called("agent", "prompt")[-1][3]
    assert "Your turn ended" in mesh.unread(run, "repo-fix-typo")[-1].read_text()


def test_a_nudged_worker_that_reports_is_not_an_alert(mesh, fake, run, monkeypatch):
    mesh.deliver(run, "repo-lead", "repo-fix-typo", "task", "Fix the typo.")
    states = iter(["working", "idle", "idle", "idle", "idle", "idle", "idle", "idle", "working", "idle"])

    def status(name, *_):
        state = next(states, "idle")
        if mesh.nudge_of(mesh.read_log(run), name, 1) and not mesh.sends_from(run, name):
            mesh.deliver(run, name, "repo-lead", "done", "Fixed.")
        return state

    monkeypatch.setattr(mesh, "agent_status", status)
    assert mesh.main(["watch", "repo-fix-typo"]) == 0


def test_a_second_watch_does_not_nudge_again_for_the_same_task(mesh, fake, run, monkeypatch, capsys):
    mesh.deliver(run, "repo-lead", "repo-fix-typo", "task", "Fix the typo.")
    states_then(mesh, monkeypatch, "working", "idle")
    assert mesh.main(["watch", "repo-fix-typo"]) == 3
    monkeypatch.setattr(mesh, "age_seconds", lambda stamp: 3600.0)  # the next watch starts an hour later
    states_then(mesh, monkeypatch, "working", "idle")
    assert mesh.main(["watch", "repo-fix-typo"]) == 3
    assert len([e for e in mesh.read_log(run) if e["event"] == "nudge"]) == 1
    # a new task earns a new nudge
    mesh.deliver(run, "repo-lead", "repo-fix-typo", "answer", "Also the other one.")
    states_then(mesh, monkeypatch, "working", "idle")
    assert mesh.main(["watch", "repo-fix-typo"]) == 3
    assert [e["received"] for e in mesh.read_log(run) if e["event"] == "nudge"] == [1, 2]


def test_two_watches_at_once_nudge_once_and_both_wait_for_the_answer(mesh, fake, run, monkeypatch):
    # `mesh ask`'s own watch and the root's `--any --alerts` watch, on the same pane worker
    mesh.deliver(run, "repo-lead", "repo-fix-typo", "task", "Fix the typo.")
    state = {"now": "working"}
    monkeypatch.setattr(mesh, "agent_status", lambda name, *_: state["now"])
    a, b = mesh.Watch(run, "repo-fix-typo", 10.0), mesh.Watch(run, "repo-fix-typo", 10.0)
    assert a.tick(5) is None and b.tick(5) is None
    state["now"] = "idle"
    for _ in range(3):  # 10 s of idle: A nudges, then B finds A's fresh nudge and waits as A does
        assert a.tick(5) is None and b.tick(5) is None
    assert len([e for e in mesh.read_log(run) if e["event"] == "nudge"]) == 1
    assert [e["rung"] for e in mesh.read_log(run) if e["event"] == "nudge_rung"] == [True]
    monkeypatch.setattr(mesh, "age_seconds", lambda stamp: 60.0)  # still silent once the nudge is old
    events = [w.tick(5) for w in (a, b) for _ in range(3)]
    assert [e[0] for e in events if e] == [3, 3]
    assert "(nudged once" in next(e for e in events if e)[1]


def test_a_nudge_that_did_not_ring_says_so(mesh, fake, run, monkeypatch, capsys):
    mesh.deliver(run, "repo-lead", "repo-fix-typo", "task", "Fix the typo.")
    fake.on("agent", "prompt", errors=["agent_blocked"] * 10)
    states_then(mesh, monkeypatch, "working", "idle")
    assert mesh.main(["watch", "repo-fix-typo"]) == 3
    assert "(its nudge was not delivered (agent_blocked)" in capsys.readouterr().out
    nudges = [c for c in fake.called("agent", "prompt") if "turn ended" in c[3]]
    assert len(nudges) == 3  # the short schedule: one try and two retries


def test_a_nudge_held_by_a_draft_says_someone_is_typing_there(mesh, fake, run, monkeypatch, capsys):
    mesh.deliver(run, "repo-lead", "repo-fix-typo", "task", "Fix the typo.")
    fake.screen("repo-fix-typo", "\n".join(claude_pane("❯\xa0half a sentence")))
    states_then(mesh, monkeypatch, "working", "idle")
    assert mesh.main(["watch", "repo-fix-typo"]) == 3
    assert "its nudge was not delivered: someone has a draft typed in its pane" in capsys.readouterr().out
    assert not [c for c in fake.called("agent", "prompt") if "turn ended" in c[3]]
    rung = [e for e in mesh.read_log(run) if e["event"] == "nudge_rung"]
    assert rung[0]["rung"] is False and rung[0]["error"] == "human_draft"


def test_an_info_from_the_worker_is_not_a_report(mesh, fake, run, monkeypatch, capsys):
    mesh.deliver(run, "repo-lead", "repo-fix-typo", "task", "Fix the typo.")
    mesh.deliver(run, "repo-fix-typo", "repo-lead", "info", "Tests take a while.")
    states_then(mesh, monkeypatch, "working", "idle")
    assert mesh.main(["watch", "repo-fix-typo"]) == 3
    assert [e["event"] for e in mesh.read_log(run) if e["event"].startswith("nudge")] == ["nudge", "nudge_rung"]


def declare_wait(mesh, run, minutes):
    """A wait from repo-fix-typo ending `minutes` from now (negative: already over)."""
    until = (mesh.datetime.now(mesh.timezone.utc) + mesh.timedelta(minutes=minutes)).isoformat(timespec="seconds")
    mesh.log_event(run, "send", sender="repo-fix-typo", to="repo-lead", type="wait", file="w.md", until=until)
    return until


def test_a_declared_wait_is_neither_nudged_nor_alerted_until_it_ends(mesh, fake, run, monkeypatch, capsys):
    mesh.deliver(run, "repo-lead", "repo-fix-typo", "task", "Fix the typo.")
    declare_wait(mesh, run, 30)
    monkeypatch.setattr(mesh, "agent_status", lambda name, *_: "idle")
    watch = mesh.Watch(run, "repo-fix-typo", 10.0)
    monkeypatch.setattr(mesh, "age_seconds", lambda stamp: 3600.0)  # its task is old: judged from the start
    assert all(watch.tick(5) is None for _ in range(20))
    assert not [e for e in mesh.read_log(run) if e["event"] in ("nudge", "alert")]
    until = declare_wait(mesh, run, -12)  # the wait is over: the normal rules
    assert mesh.main(["watch", "repo-fix-typo"]) == 3
    out = capsys.readouterr().out
    assert "(nudged once" in out and f"its wait until {until} ended" in out
    assert [e["event"] for e in mesh.read_log(run) if e["event"].startswith("nudge")] == ["nudge", "nudge_rung"]


def stop_hook(mesh, monkeypatch, capsys, run, **payload):
    """Run the worker's Stop hook as Claude Code does (JSON on stdin) and return the block reason, or None."""
    import io
    monkeypatch.setattr(mesh.sys, "stdin", io.StringIO(json.dumps({"hook_event_name": "Stop", **payload})))
    capsys.readouterr()
    assert mesh.main(["hook", "stop", "--run", run, "--as", "repo-fix-typo"]) == 0
    out = capsys.readouterr().out
    return json.loads(out)["reason"] if out else None


def test_the_stop_hook_blocks_a_silent_turn_end_once_per_task(mesh, fake, run, monkeypatch, capsys):
    assert stop_hook(mesh, monkeypatch, capsys, run) is None  # no task yet
    mesh.deliver(run, "repo-lead", "repo-fix-typo", "task", "Fix the typo.")
    mesh.mark_read(mesh.unread(run, "repo-fix-typo")[0])
    reason = stop_hook(mesh, monkeypatch, capsys, run, permission_mode="bypassPermissions")
    assert "without a `mesh send`" in reason and "unread" not in reason
    assert [(e["received"], e["via"], e["rung"]) for e in mesh.read_log(run) if e["event"] == "nudge"] == \
        [(1, "stop-hook", True)]
    assert stop_hook(mesh, monkeypatch, capsys, run, stop_hook_active=True) is None
    assert stop_hook(mesh, monkeypatch, capsys, run) is None  # this task's nudge is spent
    mesh.deliver(run, "repo-lead", "repo-fix-typo", "answer", "Also the other one.")
    # an answer it hasn't read yet: a block for the mail alone, once, which keeps the answer's nudge
    reason = stop_hook(mesh, monkeypatch, capsys, run)
    assert "1 unread mesh message (answer)" in reason and "without a `mesh send`" not in reason
    assert stop_hook(mesh, monkeypatch, capsys, run) is None  # the same unread mail never blocks twice
    assert [e["received"] for e in mesh.read_log(run) if e["event"] == "nudge"] == [1]
    mesh.mark_read(mesh.unread(run, "repo-fix-typo")[0])
    assert "without a `mesh send`" in stop_hook(mesh, monkeypatch, capsys, run)  # read, then silent
    assert [e["received"] for e in mesh.read_log(run) if e["event"] == "nudge"] == [1, 2]


def test_the_nudge_a_mail_block_kept_still_comes_in_its_continuation(mesh, fake, run, monkeypatch, capsys):
    # task read, a question sent, the answer lands as the turn winds down: the hook blocks for the mail alone;
    # the worker reads it and works on in that continuation, then ends silently: that stop gets the nudge
    mesh.deliver(run, "repo-lead", "repo-fix-typo", "task", "Fix the typo.")
    mesh.mark_read(mesh.unread(run, "repo-fix-typo")[0])
    mesh.deliver(run, "repo-fix-typo", "repo-lead", "question", "Which file?")
    mesh.deliver(run, "repo-lead", "repo-fix-typo", "answer", "README.md.")
    assert "without a `mesh send`" not in stop_hook(mesh, monkeypatch, capsys, run)
    mesh.mark_read(mesh.unread(run, "repo-fix-typo")[0])
    assert "without a `mesh send`" in stop_hook(mesh, monkeypatch, capsys, run, stop_hook_active=True)
    assert [e["received"] for e in mesh.read_log(run) if e["event"] == "nudge"] == [2]
    assert stop_hook(mesh, monkeypatch, capsys, run, stop_hook_active=True) is None  # never twice


def test_a_continuation_gets_no_mail_block_without_new_mail(mesh, fake, run, monkeypatch, capsys):
    mesh.deliver(run, "repo-lead", "repo-fix-typo", "task", "Fix the typo.")
    mesh.deliver(run, "repo-lead", "repo-fix-typo", "info", "FYI.")
    assert "2 unread mesh messages" in stop_hook(mesh, monkeypatch, capsys, run)
    mesh.mark_read(mesh.unread(run, "repo-fix-typo")[-1])  # reads one, the task still waits
    assert stop_hook(mesh, monkeypatch, capsys, run, stop_hook_active=True) is None
    mesh.deliver(run, "repo-lead", "repo-fix-typo", "info", "One more.")
    assert "2 unread mesh messages" in stop_hook(mesh, monkeypatch, capsys, run, stop_hook_active=True)


def test_mail_that_arrives_after_the_current_task_was_read_comes_with_the_nudge(mesh, fake, run, monkeypatch,
                                                                               capsys):
    mesh.deliver(run, "repo-lead", "repo-fix-typo", "task", "Fix the typo.")
    mesh.mark_read(mesh.unread(run, "repo-fix-typo")[0])
    mesh.deliver(run, "repo-lead", "repo-fix-typo", "info", "FYI.")
    reason = stop_hook(mesh, monkeypatch, capsys, run)
    assert "1 unread mesh message (info)" in reason and "without a `mesh send`" in reason
    assert [e["received"] for e in mesh.read_log(run) if e["event"] == "nudge"] == [1]


def test_the_stop_hook_lets_a_reported_or_planning_worker_stop(mesh, fake, run, monkeypatch, capsys):
    mesh.deliver(run, "repo-lead", "repo-fix-typo", "task", "Fix the typo.")
    assert stop_hook(mesh, monkeypatch, capsys, run, permission_mode="plan") is None
    with mesh.editing_run(run) as data:
        data["agents"]["repo-fix-typo"]["plan_review"] = "user"
    assert stop_hook(mesh, monkeypatch, capsys, run) is None  # no permission_mode: it may be in plan mode
    with mesh.editing_run(run) as data:
        data["agents"]["repo-fix-typo"].update(plan_review=None, status="retiring")
    assert stop_hook(mesh, monkeypatch, capsys, run) is None
    with mesh.editing_run(run) as data:
        data["agents"]["repo-fix-typo"]["status"] = "running"
    mesh.mark_read(mesh.unread(run, "repo-fix-typo")[0])
    mesh.deliver(run, "repo-fix-typo", "repo-lead", "plan", "The plan.")
    assert stop_hook(mesh, monkeypatch, capsys, run) is None
    assert not [e for e in mesh.read_log(run) if e["event"] == "nudge"]
    mesh.deliver(run, "repo-lead", "repo-fix-typo", "info", "FYI.")  # mail it hasn't read, after its report
    assert "1 unread mesh message (info)" in stop_hook(mesh, monkeypatch, capsys, run)


def test_the_stop_hook_never_fails_a_session(mesh, fake, run, monkeypatch, capsys):
    import io
    for stdin in ("", "not json", "[1]"):
        monkeypatch.setattr(mesh.sys, "stdin", io.StringIO(stdin))
        assert mesh.main(["hook", "stop", "--run", "gone", "--as", "x"]) == 0
    monkeypatch.setattr(mesh, "read_log", lambda run_id: 1 / 0)
    assert stop_hook(mesh, monkeypatch, capsys, run) is None


def test_the_stop_hook_lets_a_waiting_worker_stop_but_not_over_unread_mail(mesh, fake, run, monkeypatch, capsys):
    mesh.deliver(run, "repo-lead", "repo-fix-typo", "task", "Fix the typo.")
    mesh.mark_read(mesh.unread(run, "repo-fix-typo")[0])
    declare_wait(mesh, run, 30)
    assert stop_hook(mesh, monkeypatch, capsys, run) is None
    assert not [e for e in mesh.read_log(run) if e["event"] == "nudge"]  # kept for after the wait
    mesh.deliver(run, "repo-lead", "repo-fix-typo", "info", "FYI.")
    # a later turn end (the doorbell woke it) ends the wait: the mail, and the nudge as after no wait
    reason = stop_hook(mesh, monkeypatch, capsys, run)
    assert "1 unread mesh message (info)" in reason and "without a `mesh send`" in reason


def test_mail_at_the_turn_end_that_declares_a_wait_blocks_and_the_wait_still_covers_one(
        mesh, fake, run, monkeypatch, capsys):
    mesh.deliver(run, "repo-lead", "repo-fix-typo", "task", "Fix the typo.")
    mesh.mark_read(mesh.unread(run, "repo-fix-typo")[0])
    declare_wait(mesh, run, 30)
    mesh.deliver(run, "repo-lead", "repo-fix-typo", "info", "FYI.")
    assert "without a `mesh send`" not in stop_hook(mesh, monkeypatch, capsys, run)  # the mail block alone
    assert not [e for e in mesh.read_log(run) if e["event"] == "wait_turn"]  # that turn didn't end
    assert stop_hook(mesh, monkeypatch, capsys, run, stop_hook_active=True) is None  # now it does
    assert [e["event"] for e in mesh.read_log(run) if e["event"].startswith("wait_")] == ["wait_turn"]


def wait_events(mesh, run):
    return [e["event"] for e in mesh.read_log(run) if e["event"] in ("wait_turn", "wait_over")]


def test_a_wait_covers_one_turn_end_and_a_wake_ends_it_with_the_nudge(mesh, fake, run, monkeypatch, capsys):
    mesh.deliver(run, "repo-lead", "repo-fix-typo", "task", "Fix the typo.")
    mesh.mark_read(mesh.unread(run, "repo-fix-typo")[0])
    declare_wait(mesh, run, 30)
    assert stop_hook(mesh, monkeypatch, capsys, run) is None  # the turn end that declared it
    assert wait_events(mesh, run) == ["wait_turn"]
    assert mesh.waiting_until(mesh.read_log(run), "repo-fix-typo")
    # it woke (its background job finished) and stops again without a report
    assert mesh.HOOK_NUDGE in stop_hook(mesh, monkeypatch, capsys, run)
    assert wait_events(mesh, run) == ["wait_turn", "wait_over"]
    assert mesh.waiting_until(mesh.read_log(run), "repo-fix-typo") is None
    assert stop_hook(mesh, monkeypatch, capsys, run, stop_hook_active=True) is None  # the nudge is spent


def test_a_wake_after_the_nudge_was_spent_gets_one_wait_reminder(mesh, fake, run, monkeypatch, capsys):
    mesh.deliver(run, "repo-lead", "repo-fix-typo", "task", "Fix the typo.")
    mesh.mark_read(mesh.unread(run, "repo-fix-typo")[0])
    assert mesh.HOOK_NUDGE in stop_hook(mesh, monkeypatch, capsys, run)  # nudged, so it declares a wait
    declare_wait(mesh, run, 30)
    assert stop_hook(mesh, monkeypatch, capsys, run, stop_hook_active=True) is None
    assert stop_hook(mesh, monkeypatch, capsys, run) == mesh.WAIT_OVER  # woke, stops again: one reminder
    assert stop_hook(mesh, monkeypatch, capsys, run, stop_hook_active=True) is None  # then the watch's turn
    # a fresh wait covers its next turn end again, and the wake after it gets its own reminder
    mesh.log_event(run, "send", sender="repo-fix-typo", to="repo-lead", type="wait", file="w2.md",
                   until=(mesh.datetime.now(mesh.timezone.utc) + mesh.timedelta(minutes=30)).isoformat())
    assert stop_hook(mesh, monkeypatch, capsys, run) is None
    assert stop_hook(mesh, monkeypatch, capsys, run) == mesh.WAIT_OVER
    assert wait_events(mesh, run) == ["wait_turn", "wait_over"] * 2


def test_the_watch_ends_an_agy_wait_when_the_worker_wakes_and_stops_again(mesh, fake, run, monkeypatch, capsys):
    mesh.deliver(run, "repo-lead", "repo-fix-typo", "task", "Fix the typo.")
    mesh.mark_read(mesh.unread(run, "repo-fix-typo")[0])
    declare_wait(mesh, run, 30)
    state = {"now": "idle"}
    monkeypatch.setattr(mesh, "agent_status", lambda name, *_: state["now"])
    watch = mesh.Watch(run, "repo-fix-typo", 10.0)
    assert all(watch.tick(5) is None for _ in range(4))  # the turn end that declared it
    assert wait_events(mesh, run) == ["wait_turn"]
    state["now"] = "working"  # it woke
    assert watch.tick(5) is None
    state["now"] = "idle"
    assert watch.tick(5) is None and watch.tick(5) is None  # idle for --grace: the wait is over, nudged
    assert wait_events(mesh, run) == ["wait_turn", "wait_over"]
    assert [e["event"] for e in mesh.read_log(run) if e["event"].startswith("nudge")] == ["nudge", "nudge_rung"]
    monkeypatch.setattr(mesh, "age_seconds", lambda stamp: 60.0)
    events = [watch.tick(5) for _ in range(3)]
    status, line = next(e for e in events if e)
    assert status == 3 and "(nudged once" in line and "ended when it woke" in line


def test_an_agy_wake_with_the_nudge_spent_gets_the_wait_reminder_then_an_alert(mesh, fake, run, monkeypatch):
    mesh.deliver(run, "repo-lead", "repo-fix-typo", "task", "Fix the typo.")
    mesh.mark_read(mesh.unread(run, "repo-fix-typo")[0])
    assert mesh.claim_nudge(run, "repo-fix-typo", 1)
    declare_wait(mesh, run, 30)
    states_then(mesh, monkeypatch, "idle", "idle", "working", "idle")
    watch = mesh.Watch(run, "repo-fix-typo", 10.0)
    assert all(watch.tick(5) is None for _ in range(5))
    reminders = [c for c in fake.called("agent", "prompt") if "woke during your declared wait" in c[3]]
    assert len(reminders) == 1
    assert [e["rung"] for e in mesh.read_log(run) if e["event"] == "wait_reminder"] == [True]
    monkeypatch.setattr(mesh, "age_seconds", lambda stamp: 60.0)
    status, line = next(e for e in (watch.tick(5) for _ in range(3)) if e)
    assert status == 3 and "(reminded once at its wake" in line


def test_a_wait_reminder_held_by_a_draft_says_so_over_the_old_nudge(mesh, fake, run, monkeypatch):
    mesh.deliver(run, "repo-lead", "repo-fix-typo", "task", "Fix the typo.")
    mesh.mark_read(mesh.unread(run, "repo-fix-typo")[0])
    assert mesh.claim_nudge(run, "repo-fix-typo", 1)
    mesh.log_event(run, "nudge_rung", name="repo-fix-typo", received=1, rung=True)  # the old nudge rang
    declare_wait(mesh, run, 30)
    states_then(mesh, monkeypatch, "idle", "idle", "working", "idle")
    watch = mesh.Watch(run, "repo-fix-typo", 10.0)
    assert all(watch.tick(5) is None for _ in range(3))
    fake.screen("repo-fix-typo", "\n".join(claude_pane("❯\xa0half a sentence")))
    assert all(watch.tick(5) is None for _ in range(2))  # the reminder is held by the draft
    assert [e["error"] for e in mesh.read_log(run) if e["event"] == "wait_reminder"] == ["human_draft"]
    monkeypatch.setattr(mesh, "age_seconds", lambda stamp: 60.0)
    status, line = next(e for e in (watch.tick(5) for _ in range(3)) if e)
    assert status == 3 and "its reminder was not delivered: someone has a draft typed in its pane" in line


def test_a_fresh_wait_after_a_wake_starts_its_idle_count_again(mesh, fake, run, monkeypatch):
    mesh.deliver(run, "repo-lead", "repo-fix-typo", "task", "Fix the typo.")
    mesh.mark_read(mesh.unread(run, "repo-fix-typo")[0])
    declare_wait(mesh, run, 30)
    state = {"now": "idle"}
    monkeypatch.setattr(mesh, "agent_status", lambda name, *_: state["now"])
    watch = mesh.Watch(run, "repo-fix-typo", 10.0)
    for now, ticks in (("idle", 4), ("working", 1), ("idle", 2)):  # declared, woke, stopped: the wait is over
        state["now"] = now
        assert all(watch.tick(5) is None for _ in range(ticks))
    assert wait_events(mesh, run) == ["wait_turn", "wait_over"]
    state["now"] = "working"  # nudged, it sends a fresh wait and goes on working
    assert watch.tick(5) is None
    mesh.log_event(run, "send", sender="repo-fix-typo", to="repo-lead", type="wait", file="w2.md",
                   until=(mesh.datetime.now(mesh.timezone.utc) + mesh.timedelta(minutes=30)).isoformat())
    state["now"] = "idle"  # a few seconds between agy's steps, inside the turn that declares it
    assert watch.tick(5) is None
    assert wait_events(mesh, run) == ["wait_turn", "wait_over"]  # not yet its turn end
    state["now"] = "working"
    assert watch.tick(5) is None
    state["now"] = "idle"  # that turn's real end
    assert all(watch.tick(5) is None for _ in range(3))
    assert wait_events(mesh, run) == ["wait_turn", "wait_over", "wait_turn"]
    assert not [c for c in fake.called("agent", "prompt") if "woke during your declared wait" in c[3]]


def test_a_wait_note_with_a_malformed_time_does_not_crash(mesh, run):
    mesh.log_event(run, "send", sender="repo-fix-typo", to="repo-lead", type="wait", file="w.md", until="soon")
    assert "ended ? ago" in mesh.wait_note(mesh.read_log(run), "repo-fix-typo")
    mesh.log_event(run, "wait_over", name="repo-fix-typo", file="w.md")
    assert "ended when it woke" in mesh.wait_note(mesh.read_log(run), "repo-fix-typo")


def test_a_claude_workers_wait_is_not_ended_by_the_watch(mesh, fake, run, monkeypatch):
    with mesh.editing_run(run) as data:
        data["agents"]["repo-fix-typo"]["kind"] = "claude"
    mesh.deliver(run, "repo-lead", "repo-fix-typo", "task", "Fix the typo.")
    declare_wait(mesh, run, 30)
    states_then(mesh, monkeypatch, "idle", "idle", "idle", "working", "idle")
    monkeypatch.setattr(mesh.Watch, "limited", lambda *_: None)
    watch = mesh.Watch(run, "repo-fix-typo", 10.0)
    assert all(watch.tick(5) is None for _ in range(10))  # its Stop hook sees its turn ends, not the watch
    assert wait_events(mesh, run) == []


def test_a_watch_after_the_stop_hook_nudged_alerts_without_nudging_again(mesh, fake, run, monkeypatch, capsys):
    mesh.deliver(run, "repo-lead", "repo-fix-typo", "task", "Fix the typo.")
    mesh.mark_read(mesh.unread(run, "repo-fix-typo")[0])
    assert stop_hook(mesh, monkeypatch, capsys, run)
    monkeypatch.setattr(mesh, "age_seconds", lambda stamp: 3600.0)
    states_then(mesh, monkeypatch, "working", "idle")
    assert mesh.main(["watch", "repo-fix-typo"]) == 3
    assert "(nudged once by its Stop hook" in capsys.readouterr().out
    assert not [c for c in fake.called("agent", "prompt") if "turn ended" in c[3]]


def test_idle_time_while_the_worker_is_reopened_does_not_count(mesh, fake, run, monkeypatch):
    mesh.deliver(run, "repo-lead", "repo-fix-typo", "task", "Fix the typo.")
    with mesh.editing_run(run) as data:
        data["agents"]["repo-fix-typo"]["status"] = "starting"
    states = iter(["working", "idle", *["gone"] * 20])
    monkeypatch.setattr(mesh, "agent_status", lambda name, *_: next(states, "gone"))
    watch = mesh.Watch(run, "repo-fix-typo", 30.0)
    assert all(watch.tick(5) is None for _ in range(22))
    assert not [e for e in mesh.read_log(run) if e["event"] == "nudge"]


def test_a_report_sent_before_the_watch_started_is_not_a_silent_stop(mesh, fake, run, monkeypatch):
    mesh.deliver(run, "repo-lead", "repo-fix-typo", "task", "Review PR 6.")
    mesh.deliver(run, "repo-fix-typo", "repo-lead", "done", "No findings.")
    for path in mesh.unread(run, "repo-lead"):
        mesh.mark_read(path)  # an unread one would end the watch at once (see the unread-report tests)
    states = iter(["working", *["idle"] * 10, "gone"])
    monkeypatch.setattr(mesh, "agent_status", lambda name, *_: next(states))
    # the watch starts after the done, sees the end of that turn, and neither nudges nor alerts 3
    assert mesh.main(["watch", "--any", "--alerts"]) == 5
    assert not [e for e in mesh.read_log(run) if e["event"] == "nudge"]

def test_watch_returns_at_once_on_a_report_its_parent_has_not_read(mesh, fake, run, monkeypatch, capsys):
    # sent while no watch ran (or its doorbell was lost): the baseline would hide it
    mesh.deliver(run, "repo-lead", "repo-fix-typo", "task", "Fix the typo.")
    mesh.deliver(run, "repo-fix-typo", "repo-lead", "info", "Tests take a while.")
    mesh.deliver(run, "repo-fix-typo", "repo-lead", "plan", "My plan.")
    states_then(mesh, monkeypatch, "idle")
    for argv in (["repo-fix-typo"], ["--any", "--alerts"]):  # a lost SendMessage too
        assert mesh.main(["watch", *argv]) == 0
        assert capsys.readouterr().out.startswith("repo-fix-typo sent a plan you haven't read yet")
    # `mesh ask` counts its own baseline and reads the reply itself
    assert mesh.Watch(run, "repo-fix-typo", 30.0, finals_at_start=0).unread_report is None
    # once read, a plan is only where its turn ended: the watch waits for the answer
    for path in mesh.unread(run, "repo-lead"):
        mesh.mark_read(path)
    watch = mesh.Watch(run, "repo-fix-typo", 30.0)
    assert all(watch.tick(5) is None for _ in range(40))


def test_watch_any_reports_a_gone_workers_unread_report(mesh, fake, run, monkeypatch, capsys):
    mesh.deliver(run, "repo-lead", "repo-fix-typo", "task", "Fix the typo.")
    mesh.deliver(run, "repo-fix-typo", "repo-lead", "done", "Fixed.")
    states_then(mesh, monkeypatch, "gone")  # it reported, then its pane closed: no live worker is left
    assert mesh.main(["watch", "--any", "--alerts"]) == 0
    assert capsys.readouterr().out.startswith("repo-fix-typo sent a done you haven't read yet")
    for path in mesh.unread(run, "repo-lead"):
        mesh.mark_read(path)
    assert mesh.main(["watch", "--any"]) == 0
    assert capsys.readouterr().out.startswith("no live workers to watch")


def test_watch_ignores_an_idle_blip_mid_turn(mesh, fake, run, monkeypatch):
    mesh.deliver(run, "repo-lead", "repo-fix-typo", "task", "Fix the typo.")
    # two idle polls (10s) inside a turn, under the 30s grace: agy between two steps
    states = iter(["working", "idle", "idle", "working", "working", "gone"])
    monkeypatch.setattr(mesh, "agent_status", lambda name, *_: next(states))
    assert mesh.main(["watch", "repo-fix-typo"]) == 5
    assert not [e for e in mesh.read_log(run) if e["event"] == "nudge"]


def old_tasks(mesh, monkeypatch, age=3600.0):
    """Every logged time reads `age` seconds old: a task delivered long before the watch."""
    monkeypatch.setattr(mesh, "age_seconds", lambda stamp: age)


def test_watch_judges_a_worker_it_never_saw_working(mesh, fake, run, monkeypatch, capsys):
    # its turn ended before the watch's first poll (or it never picked the task up), long ago
    mesh.deliver(run, "repo-lead", "repo-fix-typo", "task", "Fix the typo.")
    old_tasks(mesh, monkeypatch)
    states_then(mesh, monkeypatch, "idle")
    assert mesh.main(["watch", "repo-fix-typo"]) == 3
    assert "(nudged once, its task is still unread)" in capsys.readouterr().out
    assert [e["received"] for e in mesh.read_log(run) if e["event"] == "nudge"] == [1]


def test_a_read_task_is_not_called_unread(mesh, fake, run, monkeypatch, capsys):
    mesh.deliver(run, "repo-lead", "repo-fix-typo", "task", "Fix the typo.")
    for path in mesh.unread(run, "repo-fix-typo"):
        mesh.mark_read(path)
    old_tasks(mesh, monkeypatch)
    states_then(mesh, monkeypatch, "idle")
    assert mesh.main(["watch", "repo-fix-typo"]) == 3
    out = capsys.readouterr().out
    assert "(nudged once)" in out and "unread" not in out


def test_a_fresh_task_gets_the_longer_grace_before_an_idle_worker_is_judged(mesh, fake, run, monkeypatch):
    mesh.deliver(run, "repo-lead", "repo-fix-typo", "task", "Fix the typo.")
    monkeypatch.setattr(mesh, "agent_status", lambda name, *_: "idle")
    watch = mesh.Watch(run, "repo-fix-typo", 10.0)  # judged once the task is 40 s old
    age = {"now": 0.0}
    monkeypatch.setattr(mesh, "age_seconds", lambda stamp: age["now"])
    for _ in range(8):  # idle well past --grace, but the task is at most 35 s old
        assert watch.tick(5) is None
        age["now"] += 5
    assert not [e for e in mesh.read_log(run) if e["event"] == "nudge"]
    assert watch.tick(5) is None  # 40 s: nudged
    assert len([e for e in mesh.read_log(run) if e["event"] == "nudge"]) == 1


def test_a_worker_without_a_task_is_never_judged(mesh, fake, run, monkeypatch):
    old_tasks(mesh, monkeypatch)
    monkeypatch.setattr(mesh, "agent_status", lambda name, *_: "idle")
    watch = mesh.Watch(run, "repo-fix-typo", 10.0)
    assert all(watch.tick(5) is None for _ in range(50))
    assert not [e for e in mesh.read_log(run) if e["event"] == "nudge"]


@pytest.mark.parametrize("report", ["plan", "question", "done"])
def test_an_idle_worker_waiting_on_its_report_is_never_nudged(mesh, fake, run, monkeypatch, report):
    mesh.deliver(run, "repo-lead", "repo-fix-typo", "task", "Fix the typo.")
    mesh.deliver(run, "repo-fix-typo", "repo-lead", report, "Over to you.")
    old_tasks(mesh, monkeypatch)
    monkeypatch.setattr(mesh, "agent_status", lambda name, *_: "idle")
    calls = []
    stopped = mesh.Watch.stopped
    monkeypatch.setattr(mesh.Watch, "stopped", lambda self, *a: calls.append(1) or stopped(self, *a))
    # a watch that saw it work, and one that never did: neither nudges however long it waits
    worked = mesh.Watch(run, "repo-fix-typo", 10.0, finals_at_start=99)
    worked.worked = True
    fresh = mesh.Watch(run, "repo-fix-typo", 10.0, finals_at_start=99)
    assert all(w.tick(5, alerts_only=True) is None for _ in range(100) for w in (worked, fresh))
    assert calls  # judged at least once, and found the report each time
    assert not [e for e in mesh.read_log(run) if e["event"] == "nudge"]


def test_a_turn_shorter_than_a_poll_after_an_answer_is_judged(mesh, fake, run, monkeypatch):
    # the plan was judged and approved; the next turn ends between two polls without a report
    mesh.deliver(run, "repo-lead", "repo-fix-typo", "task", "Fix the typo.")
    state = {"now": "working"}
    monkeypatch.setattr(mesh, "agent_status", lambda name, *_: state["now"])
    watch = mesh.Watch(run, "repo-fix-typo", 10.0)
    assert watch.tick(5) is None
    mesh.deliver(run, "repo-fix-typo", "repo-lead", "plan", "1. Fix it.")
    state["now"] = "idle"
    assert all(watch.tick(5) is None for _ in range(3))  # stopped(): a plan waiting for its answer
    mesh.deliver(run, "repo-lead", "repo-fix-typo", "answer", "approved")
    old_tasks(mesh, monkeypatch)
    assert all(watch.tick(5) is None for _ in range(3))
    assert [e["received"] for e in mesh.read_log(run) if e["event"] == "nudge"] == [2]


def test_a_background_worker_idle_from_the_start_is_alerted_not_nudged(mesh, fake, run, monkeypatch, capsys):
    with mesh.editing_run(run) as data:
        data["agents"]["repo-fix-typo"].update(view="bg", bg_id="abc123", kind="claude")
    mesh.deliver(run, "repo-lead", "repo-fix-typo", "task", "Fix the typo.")
    old_tasks(mesh, monkeypatch)
    states_then(mesh, monkeypatch, "idle")
    assert mesh.main(["watch", "repo-fix-typo"]) == 3
    out = capsys.readouterr().out
    assert "without sending a message (its task is still unread); inspect it with `claude logs abc123`" in out
    assert not [e for e in mesh.read_log(run) if e["event"] == "nudge"]


@pytest.mark.parametrize("view", ["pane", "bg"])
def test_a_restarted_watch_does_not_alert_again_for_the_same_task(mesh, fake, run, monkeypatch, capsys, view):
    if view == "bg":
        with mesh.editing_run(run) as data:
            data["agents"]["repo-fix-typo"].update(view="bg", bg_id="abc123", kind="claude")
    mesh.deliver(run, "repo-lead", "repo-fix-typo", "task", "Fix the typo.")
    old_tasks(mesh, monkeypatch)
    states_then(mesh, monkeypatch, "idle")
    assert mesh.main(["watch", "--any", "--alerts"]) == 3  # the first watch alerts
    assert [e["received"] for e in mesh.read_log(run) if e["event"] == "alert"] == [1]
    # the root handles the alert and starts the watch again: the same silent task is not alerted twice
    assert mesh.main(["watch", "--any", "--alerts", "--timeout", "1", "--interval", "5"]) == 6
    # a new answer re-arms it
    mesh.deliver(run, "repo-lead", "repo-fix-typo", "answer", "Try again.")
    assert mesh.main(["watch", "--any", "--alerts"]) == 3
    assert [e["received"] for e in mesh.read_log(run) if e["event"] == "alert"] == [1, 2]


def test_a_working_reading_rearms_an_alerted_task(mesh, fake, run, monkeypatch):
    mesh.deliver(run, "repo-lead", "repo-fix-typo", "task", "Fix the typo.")
    old_tasks(mesh, monkeypatch)
    states_then(mesh, monkeypatch, "idle")
    assert mesh.main(["watch", "repo-fix-typo"]) == 3
    states_then(mesh, monkeypatch, "working", "idle")  # it worked again, then stopped silently again
    assert mesh.main(["watch", "repo-fix-typo"]) == 3


def test_grace_zero_still_waits_before_judging_a_fresh_task(mesh, fake, run, monkeypatch):
    mesh.deliver(run, "repo-lead", "repo-fix-typo", "task", "Fix the typo.")
    monkeypatch.setattr(mesh, "agent_status", lambda name, *_: "idle")  # its pane is still booting
    watch = mesh.Watch(run, "repo-fix-typo", 0.0)
    assert watch.start_grace == mesh.SETTLE_GRACE
    assert all(watch.tick(5) is None for _ in range(3))
    assert not [e for e in mesh.read_log(run) if e["event"] == "nudge"]


def test_an_unread_answer_is_named_as_an_answer(mesh, fake, run, monkeypatch, capsys):
    mesh.deliver(run, "repo-lead", "repo-fix-typo", "task", "Fix the typo.")
    for path in mesh.unread(run, "repo-fix-typo"):
        mesh.mark_read(path)
    mesh.deliver(run, "repo-lead", "repo-fix-typo", "answer", "approved")
    old_tasks(mesh, monkeypatch)
    states_then(mesh, monkeypatch, "idle")
    assert mesh.main(["watch", "repo-fix-typo"]) == 3
    assert "(nudged once, its answer is still unread)" in capsys.readouterr().out


def test_a_background_worker_is_not_nudged(mesh, fake, run, monkeypatch, capsys):
    with mesh.editing_run(run) as data:
        data["agents"]["repo-fix-typo"].update(view="bg", bg_id="abc123", kind="claude")
    mesh.deliver(run, "repo-lead", "repo-fix-typo", "task", "Fix the typo.")
    states_then(mesh, monkeypatch, "working", "idle")
    assert mesh.main(["watch", "repo-fix-typo"]) == 3
    out = capsys.readouterr().out
    assert "`claude logs abc123`" in out and "mesh send repo-fix-typo --type info" in out
    assert not [e for e in mesh.read_log(run) if e["event"] == "nudge"]


def test_a_background_worker_its_stop_hook_nudged_is_alerted_as_such(mesh, fake, run, monkeypatch, capsys):
    with mesh.editing_run(run) as data:
        data["agents"]["repo-fix-typo"].update(view="bg", bg_id="abc123", kind="claude")
    mesh.deliver(run, "repo-lead", "repo-fix-typo", "task", "Fix the typo.")
    mesh.mark_read(mesh.unread(run, "repo-fix-typo")[0])
    assert stop_hook(mesh, monkeypatch, capsys, run)
    states_then(mesh, monkeypatch, "working", "idle")
    assert mesh.main(["watch", "repo-fix-typo"]) == 3
    out = capsys.readouterr().out
    assert "(its Stop hook nudged it once" in out and "--type info" not in out


def test_watch_question_then_idle_is_not_an_alert(mesh, fake, run, monkeypatch):
    states = iter(["working", "idle", "idle", "gone"])

    def status(name, *_):
        state = next(states)
        if state == "idle" and not mesh.sends_from(run, name):
            mesh.deliver(run, name, "repo-lead", "question", "Which base?")
        return state

    monkeypatch.setattr(mesh, "agent_status", status)
    # the question keeps the watch going; it ends only when the agent disappears
    assert mesh.main(["watch", "repo-fix-typo"]) == 5


def test_watch_plan_then_idle_waits_for_review(mesh, fake, run, monkeypatch):
    states = iter(["working", "idle", "idle", "gone"])

    def status(name, *_):
        state = next(states)
        if state == "idle" and not mesh.sends_from(run, name):
            mesh.deliver(run, name, "repo-lead", "plan", "1. Patch reactions.py")
        return state

    monkeypatch.setattr(mesh, "agent_status", status)
    # a plan awaiting review is neither a stall nor a final report
    assert mesh.main(["watch", "repo-fix-typo"]) == 5


def test_watch_alerts_when_blocked(mesh, fake, run, monkeypatch, capsys):
    monkeypatch.setattr(mesh, "agent_status", lambda name, *_: "blocked")
    fake.screen("repo-fix-typo", "\n".join(["older"] * 20 + ["  Select effort", "  ❯ high", "    medium", "", ""]))
    assert mesh.main(["watch", "repo-fix-typo"]) == 4
    out = capsys.readouterr().out
    assert out.startswith("ALERT repo-fix-typo is blocked on a UI prompt")
    assert out.rstrip().endswith("    older\n      Select effort\n      ❯ high\n        medium")
    assert out.count("older") == 5  # the last 8 non-empty lines


# ── worktrees / stop ─────────────────────────────────────────────────────


@pytest.fixture
def repo(tmp_path):
    path = tmp_path / "proj"
    path.mkdir()
    for cmd in (["init", "-q", "-b", "main"], ["config", "user.email", "t@t"], ["config", "user.name", "t"],
                ["commit", "-q", "--allow-empty", "-m", "init"]):
        subprocess.run(["git", *cmd], cwd=path, check=True)
    return path

def claude_worker(mesh, run, **extra):
    with mesh.editing_run(run) as data:
        data["agents"]["repo-fix-typo"].update(kind="claude", **extra)


LIMIT_SCREEN = "\n".join(["older"] * 30 + ["  ⎿  You've hit your limit · resets 7pm (America/Los_Angeles)", "",
                                           "─" * 20, "❯ ", "─" * 20])


def test_usage_limit_reads_only_the_last_lines_of_the_pane(mesh):
    lines = LIMIT_SCREEN.splitlines()
    assert mesh.usage_limit(lines) == "7pm (America/Los_Angeles)"
    assert mesh.usage_limit(["  ❯ 1. Stop and wait for limit to reset", "    2. Add funds"]) == ""  # no reset shown
    assert mesh.usage_limit(["5-hour limit reached · resets 3pm"]) == "3pm"
    assert mesh.usage_limit(lines + ["● Carrying on."] * 20) is None  # it has scrolled up: resolved
    assert mesh.usage_limit(["We discussed the rate limit in the API docs."]) is None
    assert mesh.usage_limit(["  ⎿  API Error: Rate limit reached"]) is None  # a transient rate limit, not the usage one
    # the agent's own output quoting one (a test fixture, a grep) isn't the screen
    assert mesh.usage_limit(['● Added LIMIT = "You\'ve hit your limit · resets 7pm" to the fixtures.',
                             "  ⎿  tests/test_quota.py:12: msg = \"You've hit your limit · resets 9pm\""]) is None
    assert mesh.usage_limit(None) is None


def test_a_claude_worker_at_its_usage_limit_is_exit_7_once_per_task(mesh, fake, run, monkeypatch, capsys):
    claude_worker(mesh, run)
    mesh.deliver(run, "repo-lead", "repo-fix-typo", "task", "Fix the typo.")
    monkeypatch.setattr(mesh, "agent_status", lambda name, *_: "blocked")  # its rate-limit options menu
    fake.screen("repo-fix-typo", LIMIT_SCREEN)
    assert mesh.main(["watch", "repo-fix-typo"]) == 7
    out = capsys.readouterr().out
    assert out.startswith("ALERT repo-fix-typo hit Claude's usage limit (resets 7pm (America/Los_Angeles)); ")
    assert "--type task --body" in out and "mesh switch" not in out
    watch = mesh.Watch(run, "repo-fix-typo", 30.0)  # a restarted watch waits quietly
    assert all(watch.tick(5) is None for _ in range(10))
    mesh.deliver(run, "repo-lead", "repo-fix-typo", "task", "The usage limit has reset: continue.")
    assert mesh.main(["watch", "repo-fix-typo"]) == 7  # the new task re-arms it
    assert [e["kind"] for e in mesh.read_log(run) if e["event"] == "alert"] == ["limit", "limit"]


def test_a_claude_worker_stopped_by_its_usage_limit_is_not_nudged(mesh, fake, run, monkeypatch, capsys):
    claude_worker(mesh, run)
    mesh.deliver(run, "repo-lead", "repo-fix-typo", "task", "Fix the typo.")
    states_then(mesh, monkeypatch, "working", "idle")
    fake.screen("repo-fix-typo", LIMIT_SCREEN)
    assert mesh.main(["watch", "repo-fix-typo"]) == 7
    assert not [e for e in mesh.read_log(run) if e["event"] == "nudge"]
    assert not [c for c in fake.called("agent", "prompt") if "turn ended" in c[3]]


def test_a_usage_limit_during_a_declared_wait_is_still_exit_7(mesh, fake, run, monkeypatch, capsys):
    claude_worker(mesh, run)
    mesh.deliver(run, "repo-lead", "repo-fix-typo", "task", "Fix the typo.")
    declare_wait(mesh, run, 30)
    states_then(mesh, monkeypatch, "working", "idle")
    fake.screen("repo-fix-typo", LIMIT_SCREEN)
    assert mesh.main(["watch", "repo-fix-typo"]) == 7
    assert "hit Claude's usage limit (resets 7pm" in capsys.readouterr().out
    assert not [e for e in mesh.read_log(run) if e["event"] == "nudge"]


def test_an_agy_pane_showing_limit_text_is_not_a_claude_limit(mesh, fake, run, monkeypatch):
    monkeypatch.setattr(mesh, "agent_status", lambda name, *_: "blocked")
    fake.screen("repo-fix-typo", LIMIT_SCREEN)
    assert mesh.main(["watch", "repo-fix-typo"]) == 4


def write_transcript(mesh, cwd, session, *events):
    path = mesh.claude_sessions_dir(cwd) / f"{session}.jsonl"
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text("".join(json.dumps(e) + "\n" for e in events))
    return path


def api_error(code, text):
    return {"type": "assistant", "isApiErrorMessage": True, "error": code,
            "message": {"model": "<synthetic>", "content": [{"type": "text", "text": text}]}}


REPLY = {"type": "assistant", "message": {"model": "claude-opus-5-5", "content": [{"type": "text", "text": "Done."}]}}


def test_a_background_worker_at_its_usage_limit_is_read_from_its_transcript(mesh, fake, run, monkeypatch, tmp_path,
                                                                            capsys):
    monkeypatch.setenv("CLAUDE_CONFIG_DIR", str(tmp_path / "cc"))
    cwd = str(tmp_path / "wt")
    claude_worker(mesh, run, view="bg", cwd=cwd, session="s1", bg_id="ab12")
    write_transcript(mesh, cwd, "s1", REPLY, {"type": "user"}, api_error("rate_limit", "You've hit your limit · resets 3am (UTC)"))
    mesh.deliver(run, "repo-lead", "repo-fix-typo", "task", "Fix the typo.")
    states_then(mesh, monkeypatch, "working", "idle")
    assert mesh.main(["watch", "repo-fix-typo"]) == 7
    out = capsys.readouterr().out
    assert "usage limit (resets 3am (UTC))" in out and "pass its send_message on" in out


def test_transcript_limit_reads_only_the_last_reply(mesh, monkeypatch, tmp_path):
    monkeypatch.setenv("CLAUDE_CONFIG_DIR", str(tmp_path / "cc"))
    entry = {"kind": "claude", "cwd": str(tmp_path / "wt")}
    write_transcript(mesh, entry["cwd"], "s1", api_error("rate_limit", "You've hit your limit"))
    assert mesh.transcript_limit(entry, "s1") == ""
    write_transcript(mesh, entry["cwd"], "s1", api_error("rate_limit", "API Error: Rate limit reached"))
    assert mesh.transcript_limit(entry, "s1") is None  # the code alone isn't enough: a plain rate limit
    write_transcript(mesh, entry["cwd"], "s1", api_error("rate_limit", "You've hit your limit"), {"type": "user"}, REPLY)
    assert mesh.transcript_limit(entry, "s1") is None  # it went on after the reset
    write_transcript(mesh, entry["cwd"], "s1", api_error("authentication_failed", "Login expired · Please run /login"))
    assert mesh.transcript_limit(entry, "s1") is None
    assert mesh.transcript_limit(entry, "missing") is None and mesh.transcript_limit({**entry, "kind": "agy"}, "s1") is None


def mesh_argv(run="mtest", name="repo-fix-typo", session="s1", deny=(), script=None):
    """A Claude command line as mesh starts a pane worker: its deny list, and `--settings` carrying the worker's
    Stop hook, which runs this mesh (`script`: another path)."""
    command = shlex.join([script or str(MESH_SCRIPT), "hook", "stop", "--run", run, "--as", name])
    hooks = {"Stop": [{"hooks": [{"type": "command", "command": command}]}]}
    return ["claude", *(["--disallowedTools", *deny] if deny else []), "--resume", session,
            "--settings", json.dumps({"hooks": hooks})]


def claude_in_pane(fake, pane, *argv):
    fake.on("pane", "process-info", "--pane", pane, result={"process_info": {
        "foreground_process_group_id": 9, "shell_pid": 8,
        "foreground_processes": [{"pid": 9, "argv": list(argv or mesh_argv())}]}})


def stale_transcript(mesh, monkeypatch, tmp_path, minutes=50):
    """A Claude worker whose session file last changed `minutes` ago, with its task delivered long before."""
    monkeypatch.setenv("CLAUDE_CONFIG_DIR", str(tmp_path / "cc"))
    cwd = str(tmp_path / "wt")
    path = write_transcript(mesh, cwd, "s1", REPLY)
    then = time.time() - minutes * 60
    os.utime(path, (then, then))
    monkeypatch.setattr(mesh, "epoch", lambda stamp: 0.0)
    return cwd, path


def test_a_worker_working_with_a_still_transcript_is_exit_8_once_per_task(mesh, fake, run, monkeypatch, tmp_path,
                                                                         capsys):
    cwd, path = stale_transcript(mesh, monkeypatch, tmp_path)
    claude_worker(mesh, run, cwd=cwd, session="s1")
    mesh.deliver(run, "repo-lead", "repo-fix-typo", "task", "Fix the typo.")
    monkeypatch.setattr(mesh, "agent_status", lambda name, *_: "working")
    claude_in_pane(fake, "w2:p5")
    fake.screen("repo-fix-typo", "● Bash(npm test -- --watch)\n  ⎿  Running…\n✻ Working… (51m 3s)")
    assert mesh.main(["watch", "repo-fix-typo"]) == 8
    out = capsys.readouterr().out
    assert out.startswith("ALERT repo-fix-typo may be hung: it reads working, and its transcript hasn't changed "
                          "for 50 min")
    assert "last lines of its pane:\n    ● Bash(npm test -- --watch)" in out
    assert not fake.called("pane", "close") and not fake.called("agent", "send-keys")
    watch = mesh.Watch(run, "repo-fix-typo", 30.0)  # restarted: not again for this task
    assert all(watch.tick(5) is None for _ in range(5))
    mesh.deliver(run, "repo-lead", "repo-fix-typo", "answer", "Kill the watcher and go on.")
    assert mesh.Watch(run, "repo-fix-typo", 30.0).tick(5)[0] == 8  # a new answer re-arms it
    assert mesh.Watch(run, "repo-fix-typo", 30.0, hang=0).tick(5) is None
    os.utime(path)  # it writes again: progress
    mesh.deliver(run, "repo-lead", "repo-fix-typo", "task", "Next.")
    assert mesh.Watch(run, "repo-fix-typo", 30.0).tick(5) is None


def test_a_declared_wait_is_not_hung(mesh, fake, run, monkeypatch, tmp_path):
    cwd, _ = stale_transcript(mesh, monkeypatch, tmp_path)
    claude_worker(mesh, run, cwd=cwd, session="s1")
    mesh.deliver(run, "repo-lead", "repo-fix-typo", "task", "Fix the typo.")
    until = declare_wait(mesh, run, 30)
    real = mesh.datetime.fromisoformat
    monkeypatch.setattr(mesh, "epoch", lambda stamp: real(stamp).timestamp() if stamp == until else 0.0)
    monkeypatch.setattr(mesh, "agent_status", lambda name, *_: "working")
    watch = mesh.Watch(run, "repo-fix-typo", 10.0)
    assert all(watch.tick(5) is None for _ in range(5))
    assert not [e for e in mesh.read_log(run) if e["event"] == "alert"]


def test_a_working_registration_over_a_bare_shell_is_gone(mesh, fake, run, monkeypatch, tmp_path, capsys):
    cwd, _ = stale_transcript(mesh, monkeypatch, tmp_path)
    claude_worker(mesh, run, cwd=cwd, session="s1")
    mesh.deliver(run, "repo-lead", "repo-fix-typo", "task", "Fix the typo.")
    monkeypatch.setattr(mesh, "agent_status", lambda name, *_: "working")
    claude_in_pane(fake, "w2:p5", "-bash")  # herdr kept the registration after Claude died
    assert mesh.main(["watch", "repo-fix-typo"]) == 5
    assert "herdr still reads it working, but its pane runs no claude process" in capsys.readouterr().out


def test_gone_and_stopped_alerts_show_the_workers_work(mesh, fake, cli, run, repo, monkeypatch, capsys):
    subprocess.run(["git", "checkout", "-q", "-b", "mesh/mtest/fix"], cwd=repo, check=True)
    base = subprocess.run(["git", "rev-parse", "main"], cwd=repo, capture_output=True, text=True).stdout.strip()
    subprocess.run(["git", "commit", "-q", "--allow-empty", "-m", "fix"], cwd=repo, check=True)
    (repo / "wip.txt").write_text("half done\n")
    with mesh.editing_run(run) as data:
        data["agents"]["repo-fix-typo"].update(repo=str(repo), cwd=str(repo), branch="mesh/mtest/fix", base=base)
    cli.on("pr", "list", stdout=json.dumps([{"number": 31, "url": "https://github.com/o/r/pull/31"}]))
    mesh.deliver(run, "repo-lead", "repo-fix-typo", "task", "Fix the typo.")
    monkeypatch.setattr(mesh, "agent_status", lambda name, *_: "gone")
    assert mesh.main(["watch", "repo-fix-typo"]) == 5
    assert ("without reporting done/failed\n  its work: 1 commit(s) past its base, not pushed, PR #31 open, "
            "worktree dirty") in capsys.readouterr().out
    assert cli.called("pr", "list", "--head", "mesh/mtest/fix")
    (repo / "wip.txt").unlink()
    subprocess.run(["git", "reset", "-q", "--hard", base], cwd=repo, check=True)
    states_then(mesh, monkeypatch, "working", "idle")
    calls = len(cli.called("pr"))
    assert mesh.main(["watch", "repo-fix-typo"]) == 3
    assert "its work: no commits past its base, worktree clean" in capsys.readouterr().out
    assert len(cli.called("pr")) == calls  # no commits, no PR to look for


def test_a_worker_that_reported_is_never_hung_or_gone_over_a_stale_working(mesh, fake, run, monkeypatch, tmp_path):
    # it sent `done` (and the root read it), but herdr kept reading it working
    cwd, _ = stale_transcript(mesh, monkeypatch, tmp_path)
    claude_worker(mesh, run, cwd=cwd, session="s1")
    mesh.deliver(run, "repo-lead", "repo-fix-typo", "task", "Fix the typo.")
    mesh.deliver(run, "repo-fix-typo", "repo-lead", "done", "Fixed.")
    for path in mesh.unread(run, "repo-lead"):
        mesh.mark_read(path)
    monkeypatch.setattr(mesh, "agent_status", lambda name, *_: "working")
    for argv in (["claude"], ["-bash"]):
        claude_in_pane(fake, "w2:p5", *argv)
        watch = mesh.Watch(run, "repo-fix-typo", 30.0)
        assert all(watch.tick(5) is None for _ in range(5))
    # reported during this watch, then a new task, and its CLI died: as the gone branch, it's finished
    watch = mesh.Watch(run, "repo-fix-typo", 30.0)
    watch.reported = True
    mesh.deliver(run, "repo-lead", "repo-fix-typo", "answer", "One more thing.")
    assert watch.tick(5, alerts_only=True) is None and watch.finished


def test_hung_skips_a_task_already_alerted_for_its_usage_limit(mesh, fake, run, monkeypatch, tmp_path):
    cwd, _ = stale_transcript(mesh, monkeypatch, tmp_path)
    claude_worker(mesh, run, cwd=cwd, session="s1")
    mesh.deliver(run, "repo-lead", "repo-fix-typo", "task", "Fix the typo.")
    mesh.record_alert(run, "repo-fix-typo", 1, "limit")
    monkeypatch.setattr(mesh, "agent_status", lambda name, *_: "working")
    fake.calls.clear()  # the delivery looked at its pane once (relaunched_outside_mesh)
    assert mesh.Watch(run, "repo-fix-typo", 30.0).tick(5) is None
    assert not fake.called("pane", "process-info")  # no probing every poll


def test_a_hung_agy_worker_is_judged_by_its_conversation_and_log(mesh, fake, run, monkeypatch, tmp_path):
    log = tmp_path / "agy.log"
    log.write_text("step\n")
    then = time.time() - 3000
    os.utime(log, (then, then))
    with mesh.editing_run(run) as data:
        data["agents"]["repo-fix-typo"]["log"] = str(log)
    monkeypatch.setattr(mesh, "epoch", lambda stamp: 0.0)
    mesh.deliver(run, "repo-lead", "repo-fix-typo", "task", "Fix the typo.")
    monkeypatch.setattr(mesh, "agent_status", lambda name, *_: "working")
    assert mesh.Watch(run, "repo-fix-typo", 30.0).tick(5)[0] == 8
    # its conversation db took a row a minute ago: not hung (a new task, so the alert above doesn't excuse it)
    db = Path(os.environ["MESH_AGY_CONVERSATIONS"]) / "c1.db"
    db.parent.mkdir(parents=True)
    db.write_bytes(b"")
    with mesh.editing_run(run) as data:
        data["agents"]["repo-fix-typo"]["session"] = "c1"
    mesh.deliver(run, "repo-lead", "repo-fix-typo", "task", "Next.")
    assert mesh.Watch(run, "repo-fix-typo", 30.0).tick(5) is None


def test_a_lost_registration_waiting_on_its_plan_is_not_hung(mesh, fake, run, monkeypatch, tmp_path):
    cwd, _ = stale_transcript(mesh, monkeypatch, tmp_path)
    claude_worker(mesh, run, cwd=cwd, session="s1")
    mesh.deliver(run, "repo-lead", "repo-fix-typo", "task", "Fix the typo.")
    monkeypatch.setattr(mesh, "agent_status", lambda name, *_: "unknown")
    assert mesh.Watch(run, "repo-fix-typo", 30.0).tick(5)[0] == 8  # nothing reported: it may be hung
    mesh.deliver(run, "repo-lead", "repo-fix-typo", "task", "Plan this.")
    mesh.deliver(run, "repo-fix-typo", "repo-lead", "plan", "My plan.")
    for path in mesh.unread(run, "repo-lead"):
        mesh.mark_read(path)
    assert mesh.Watch(run, "repo-fix-typo", 30.0).tick(5) is None


def test_stop_removes_clean_worktrees_and_keeps_dirty(mesh, fake, run, repo, capsys):
    clean, _, _ = mesh.add_worktree(run, repo, "clean", None)
    subprocess.run(["git", "commit", "-q", "--allow-empty", "-m", "work"], cwd=clean, check=True)
    dirty, branch, _ = mesh.add_worktree(run, repo, "dirty", None)
    (dirty / "wip.txt").write_text("unfinished")
    assert branch == "mesh/mtest/dirty"

    capsys.readouterr()
    assert mesh.main(["stop"]) == 0
    out = json.loads(capsys.readouterr().out)
    assert not clean.exists()
    assert dirty.exists()
    assert out["worktrees_kept"][0]["dirty"] is True
    assert "repo-fix-typo" in out["closed"]
    assert fake.called("pane", "close", "w2:p5")
    assert fake.called("agent", "rename", "w1:p1", "--clear")
    assert (mesh.mesh_home() / "archive" / "mtest" / "run.json").exists()
    assert not (mesh.mesh_home() / "by-pane" / "w1_p1.json").exists()
    # a branch with commits survives the worktree removal
    assert "mesh/mtest/clean" in subprocess.run(["git", "branch"], cwd=repo, capture_output=True, text=True).stdout
    assert out["branches_deleted"] == []


ROOT_ONLY = (["init", "--mode", "agy"], ["spawn", "--repo", "."], ["ring", "repo-lead"], ["watch", "--any"],
             ["approve", "repo-fix-typo"], ["switch", "repo-fix-typo"], ["retire", "repo-fix-typo"],
             ["resume", "repo-fix-typo"], ["open", "repo-fix-typo"], ["ask", "--repo", ".", "--body", "x"],
             ["peers", "add", "/nowhere"], ["peers", "remove", "p"], ["peers", "edit", "p", "--about", "x"],
             ["peers", "refresh"], ["worktree", "--slug", "x"], ["reset"])
WORKER_RUNS = (["whoami"], ["whoami", "--reminder", "clear"], ["inbox"], ["dir"], ["peers", "list"], ["status"],
               ["log"], ["agents"], ["send", "parent", "--type", "info", "--body", "FYI."])


def command_of(mesh, argv):
    return mesh.command_name(mesh.build_parser().parse_args(argv))


def test_every_command_is_either_a_workers_or_the_roots(mesh):
    parser = mesh.build_parser()
    sub = next(a for a in parser._actions if isinstance(a, mesh.argparse._SubParsersAction))
    peers = next(a for a in sub.choices["peers"]._actions if a.dest == "action").choices
    names = {f"peers {a}" for a in peers} | set(sub.choices) - {"peers"}
    root_only = {command_of(mesh, argv) for argv in ROOT_ONLY}
    assert names == mesh.WORKER_COMMANDS | mesh.OWN_WORKER_CHECK | root_only  # a new command is listed somewhere
    assert not root_only & mesh.WORKER_COMMANDS


def test_a_worker_is_refused_every_command_but_its_own(mesh, fake, run, monkeypatch, capsys):
    as_worker(monkeypatch, run)
    runs, renames = {p.name for p in mesh.mesh_home().iterdir()}, len(fake.called("agent", "rename"))
    for argv in ROOT_ONLY:
        capsys.readouterr()
        assert mesh.main(argv) == 1, argv
        err = capsys.readouterr().err
        assert f"only a root runs `mesh {command_of(mesh, argv)}`" in err
        assert "MESH_HOME" in err and "MESH_CLAUDE" in err and "env -u MESH_RUN" in err  # the ways out
    refused = [e["command"] for e in mesh.read_log(run) if e["event"] == "refused"]
    assert refused == [command_of(mesh, argv) for argv in ROOT_ONLY]
    assert {p.name for p in mesh.mesh_home().iterdir()} == runs and not mesh.peers_path().exists()
    assert len(fake.called("agent", "rename")) == renames and not fake.called("pane", "split")
    assert not [e for e in mesh.read_log(run) if e["event"] in ("nudge", "alert")]
    # --as names someone else: the user's choice, as for `mesh stop`
    assert mesh.worker_caller(mesh.build_parser().parse_args(["ring", "x", "--as", "repo-lead"])) is None


@pytest.mark.parametrize("kind", ["agy", "claude"])
def test_a_worker_still_runs_its_own_commands(mesh, fake, run, monkeypatch, capsys, kind):
    with mesh.editing_run(run) as data:
        data["agents"]["repo-fix-typo"].update(kind=kind, session="s-1")
    fake.on("agent", "get", result={"agent": {"agent_status": "working"}})
    as_worker(monkeypatch, run)
    if kind == "claude":
        monkeypatch.setenv("CLAUDE_CODE_SESSION_ID", "s-1")
    for argv in WORKER_RUNS:
        assert mesh.main(argv) in (0, 2), argv  # 2: stored, the doorbell didn't ring
    assert stop_hook(mesh, monkeypatch, capsys, run) is None
    assert not [e for e in mesh.read_log(run) if e["event"] == "refused"]


def test_the_root_and_a_plain_shell_keep_every_command(mesh, fake, run, monkeypatch):
    for argv in ROOT_ONLY + WORKER_RUNS:
        mesh.check_worker(mesh.build_parser().parse_args(argv))  # the root, by its pane
    monkeypatch.delenv("HERDR_PANE_ID")
    for argv in ROOT_ONLY + WORKER_RUNS:
        mesh.check_worker(mesh.build_parser().parse_args(argv))


def test_a_pane_worker_herdr_restored_is_refused_too(mesh, fake, run, monkeypatch, capsys):
    restored(mesh, run, monkeypatch, fake)
    capsys.readouterr()
    assert mesh.main(["spawn", "--repo", "."]) == 1
    assert "repo-fix-typo is a worker of run mtest, and only a root runs `mesh spawn`" in capsys.readouterr().err


def test_a_worker_trying_mesh_out_under_a_temp_home_isnt_refused(mesh, fake, run, monkeypatch, tmp_path):
    as_worker(monkeypatch, run)
    monkeypatch.setenv("MESH_HOME", str(tmp_path / "sandbox"))
    args = mesh.build_parser().parse_args(["peers", "add", "x"])
    assert mesh.worker_caller(args) is None  # its run isn't in this MESH_HOME (test_peers.py runs `peers add` so)


def test_a_claude_session_that_inherited_a_workers_variables_isnt_refused(mesh, fake, run, monkeypatch):
    with mesh.editing_run(run) as data:
        data["agents"]["repo-fix-typo"].update(kind="claude", session="worker-session")
    as_worker(monkeypatch, run)
    args = mesh.build_parser().parse_args(["spawn"])
    monkeypatch.setenv("CLAUDE_CODE_SESSION_ID", "worker-session")
    assert mesh.worker_caller(args) == (run, "repo-fix-typo")
    monkeypatch.setenv("CLAUDE_CODE_SESSION_ID", "root-session")  # e.g. forked by the --bg daemon
    assert mesh.worker_caller(args) is None
    stopping = mesh.build_parser().parse_args(["stop", run])
    assert mesh.stopping_worker(stopping)[1] == ""  # nor is it refused `stop`, which asks the same


def test_only_a_root_stops_a_run(mesh, fake, run, monkeypatch, capsys):
    as_worker(monkeypatch, run)
    for argv in (["stop"], ["stop", run]):
        capsys.readouterr()
        assert mesh.main(argv) == 1
        assert "only a root stops a run" in capsys.readouterr().err
    assert (mesh.run_dir(run) / "run.json").exists() and not fake.called("pane", "close")
    refused = [e for e in mesh.read_log(run) if e["event"] == "refused"]
    assert [(e["name"], e["command"], e["run"]) for e in refused] == [("repo-fix-typo", "stop", run)] * 2
    assert not fake.called("pane", "process-info")  # no identity lookup for a named run
    # nor does a --run naming another run, or none, get a worker past it
    assert mesh.main(["stop", run, "--run", "mnone"]) == 1
    assert "names another run than the one to stop" in capsys.readouterr().err
    monkeypatch.setenv("MESH_RUN", "mnone")  # its own run named by the target only
    assert mesh.main(["stop", run]) == 1
    assert "only a root stops a run" in capsys.readouterr().err
    assert (mesh.run_dir(run) / "run.json").exists()
    # the user in a plain shell, with no identity at all
    for key in ("MESH_RUN", "MESH_SELF", "HERDR_PANE_ID"):
        monkeypatch.delenv(key)
    assert mesh.main(["stop", run]) == 0
    assert (mesh.mesh_home() / "archive" / run / "run.json").exists()


def test_spawn_creates_tab_then_splits(mesh, fake, run, repo, monkeypatch, capsys):
    fake.on("workspace", "list", result={"workspaces": [{"workspace_id": "w9", "label": "proj"}]})
    fake.on("tab", "create", result={"tab": {"tab_id": "w9:t4"}, "root_pane": {"pane_id": "w9:p10"}})
    fake.on("pane", "layout", result={"layout": {"panes": [{"pane_id": "w9:p10", "rect": {"width": 200, "height": 50}}]}})
    fake.on("pane", "split", result={"pane": {"pane_id": "w9:p11"}})
    fake.on("agent", "start", errors=["cli_error"], result={"agent": {}})  # shell not ready on the first try
    monkeypatch.setattr(mesh, "resolve_agy_model", lambda family, effort: f"gemini-3.8-{family}-{effort}")

    assert mesh.main(["spawn", "--kind", "agy", "--repo", str(repo), "--model", "flash", "--effort", "medium",
                      "--worktree", "Fix Reactions", "--task", "Do the thing."]) == 0
    first = json.loads(capsys.readouterr().out)
    assert first["name"] == "proj-fix-reactions"
    assert first["branch"] == "mesh/mtest/fix-reactions"
    assert first["model"] == "gemini-3.8-flash-medium"
    tab_call = fake.called("tab", "create")[0]
    assert "MESH_SELF=proj-fix-reactions" in tab_call and "MESH_PARENT=repo-lead" in tab_call
    start = fake.called("agent", "start")[-1]
    assert start[-3:] == ("--model", "gemini-3.8-flash-medium", "--dangerously-skip-permissions")
    assert first["delivery"]["rung"] is True

    assert mesh.main(["spawn", "--kind", "claude", "--repo", str(repo), "--name", "port"]) == 0
    second = json.loads(capsys.readouterr().out)
    assert second["pane"] == "w9:p11"
    assert second["model"] == "opus"
    assert fake.called("pane", "split", "w9:p10", "--direction", "right")
    assert mesh.load_run(run)["agents"]["proj-port"]["status"] == "running"
    assert started(fake)[-5:] == ("--model", "opus", "--effort", "high", "--dangerously-skip-permissions")
    assert "--settings" in fake.called("agent", "start")[-1]  # a Claude pane worker gets its hooks


def test_workers_open_beside_the_root_until_its_tab_is_full(mesh, fake, run, repo, monkeypatch, capsys):
    monkeypatch.setenv("MESH_SIDE_PANES", "2")
    monkeypatch.setattr(mesh, "resolve_agy_model", lambda family, effort: f"gemini-3.8-{family}-{effort}")
    fake.on("pane", "layout", result={"layout": {"panes": [{"pane_id": "w1:p1", "rect": {"width": 200, "height": 50}}]}})
    fake.on("pane", "split", result={"pane": {"pane_id": "w1:p2"}})
    stub_new_pane(fake)
    assert mesh.main(["spawn", "--repo", str(repo), "--name", "one", "--model", "flash"]) == 0
    first = json.loads(capsys.readouterr().out)
    assert (first["pane"], first["workspace"]) == ("w1:p2", "w1")
    split = fake.called("pane", "split")[-1]
    assert split[2:5] == ("w1:p1", "--direction", "right") and "MESH_SELF=proj-one" in split
    assert mesh.load_run(run)["agents"]["proj-one"]["tab"] == "w1:t1"

    # the next one splits the worker's pane, not the root's
    fake.on("pane", "layout", result={"layout": {"panes": [
        {"pane_id": "w1:p1", "rect": {"width": 100, "height": 50}},
        {"pane_id": "w1:p2", "rect": {"width": 99, "height": 50}}]}})
    fake.on("pane", "split", result={"pane": {"pane_id": "w1:p3"}})
    assert mesh.main(["spawn", "--repo", str(repo), "--name", "two", "--model", "flash"]) == 0
    assert fake.called("pane", "split")[-1][2:5] == ("w1:p2", "--direction", "down")
    assert not fake.called("tab", "create")

    # the root's tab is full: the third goes to the run's tab in the project's workspace
    fake.on("pane", "layout", result={"layout": {"panes": [
        {"pane_id": "w1:p1", "rect": {"width": 100, "height": 50}},
        {"pane_id": "w1:p2", "rect": {"width": 99, "height": 20}},
        {"pane_id": "w1:p3", "rect": {"width": 99, "height": 29}}]}})
    assert mesh.main(["spawn", "--repo", str(repo), "--name", "three", "--model", "flash"]) == 0
    entry = mesh.load_run(run)["agents"]["proj-three"]
    assert (entry["pane"], entry["tab"], entry["workspace"]) == ("w9:p10", "w9:t4", "w9")

    # a swap splits the worker's own pane, even though the tab is full and another worker's pane is bigger
    with mesh.editing_run(run) as data:
        data["agents"]["proj-one"].update(phase="plan", exec_model="flash", exec_effort="medium", session="c1")
    sent_plan(mesh, run, "proj-one")
    fake.on("pane", "split", result={"pane": {"pane_id": "w1:p4"}})
    assert mesh.main(["approve", "proj-one"]) == 0
    assert fake.called("pane", "split")[-1][2] == "w1:p2"
    entry = mesh.load_run(run)["agents"]["proj-one"]
    assert (entry["pane"], entry["tab"], entry["workspace"]) == ("w1:p4", "w1:t1", "w1")
    assert fake.called("pane", "close", "w1:p2")

    # a pane the user closed by hand frees its place
    fake.on("pane", "layout", result={"layout": {"panes": [
        {"pane_id": "w1:p1", "rect": {"width": 100, "height": 50}},
        {"pane_id": "w1:p4", "rect": {"width": 99, "height": 50}}]}})
    fake.on("pane", "split", result={"pane": {"pane_id": "w1:p5"}})
    assert mesh.main(["spawn", "--repo", str(repo), "--name", "four", "--model", "flash"]) == 0
    assert mesh.load_run(run)["agents"]["proj-four"]["pane"] == "w1:p5"

    # the root's tab is gone: a side worker's swap reopens it in its project's workspace, not the root's
    fake.on("pane", "layout", error="pane_not_found")
    fake.on("workspace", "list", result={"workspaces": [{"workspace_id": "w9", "label": "proj"}]})
    with mesh.editing_run(run) as data:
        data["agents"]["proj-four"].update(phase="plan", exec_model="flash", exec_effort="medium", session="c2")
        data.pop("tabs", None)
    sent_plan(mesh, run, "proj-four")
    fake.on("tab", "create", result={"tab": {"tab_id": "w9:t6"}, "root_pane": {"pane_id": "w9:p20"}})
    assert mesh.main(["approve", "proj-four"]) == 0
    entry = mesh.load_run(run)["agents"]["proj-four"]
    assert (entry["pane"], entry["tab"], entry["workspace"]) == ("w9:p20", "w9:t6", "w9")
    assert fake.called("tab", "create", "--workspace", "w9")


def test_spawn_defaults_to_auto_planning_then_flash(mesh, fake, run, repo, monkeypatch, capsys):
    fake.on("workspace", "list", result={"workspaces": [{"workspace_id": "w9", "label": "proj"}]})
    fake.on("tab", "create", result={"tab": {"tab_id": "w9:t4"}, "root_pane": {"pane_id": "w9:p10"}})
    with_quotas(mesh, monkeypatch)
    assert mesh.main(["spawn", "--repo", str(repo), "--worktree", "fix-reactions"]) == 0
    out = json.loads(capsys.readouterr().out)
    assert out["model"] == "claude-opus-5-5-high"  # Argon is at 0%
    assert out["notes"] == ["gemini-4-argon-high has 0% left (resets 2026-10-29T17:26:00Z)"]
    entry = mesh.load_run(run)["agents"]["proj-fix-reactions"]
    assert (entry["role"], entry["kind"], entry["effort"]) == ("worker", "agy", "high")
    assert (entry["phase"], entry["exec_model"], entry["exec_effort"]) == ("plan", "flash", "medium")
    assert entry["log"].endswith("/mtest/logs/proj-fix-reactions.log")
    assert fake.called("agent", "start")[-1][-5:] == (
        "--log-file", entry["log"], "--model", "claude-opus-5-5-high", "--dangerously-skip-permissions")
    assert "MESH_ROLE=worker" in fake.called("tab", "create")[0]

    fake.on("pane", "layout", result={"layout": {"panes": [{"pane_id": "w9:p10", "rect": {"width": 200, "height": 50}}]}})
    fake.on("pane", "split", result={"pane": {"pane_id": "w9:p11"}})
    assert mesh.main(["spawn", "--repo", str(repo), "--name", "one", "--model", "flash"]) == 0
    entry = mesh.load_run(run)["agents"]["proj-one"]
    assert (entry["model"], entry["exec_model"], entry["phase"]) == ("gemini-3.8-flash-high", None, None)
    assert (entry["effort"], entry["exec_effort"]) == ("high", None)


# ── trust dialogs ────────────────────────────────────────────────────────

AGY_TRUST = """Accessing workspace:
/home/u/.local/share/agent-mesh/worktrees/m1/x
Do you trust the contents of this project?
Antigravity CLI requires permission to read, edit, and execute files here.
> Yes, I trust this folder
  No, exit
  ↑/↓ Navigate · enter Confirm
"""

CLAUDE_TRUST = """ Accessing workspace:
 /tmp/scratch/mesh-test-a
 Quick safety check: Is this a project you created or one you trust?
 Claude Code'll be able to read, edit, and execute files here.
 Security guide
 ❯ No, exit
   Yes, I trust this folder
 Enter to confirm · Esc to cancel
"""


def test_trust_keys_agy_default_is_yes(mesh):
    assert mesh.trust_keys(AGY_TRUST) == ["enter"]


def test_trust_keys_claude_default_is_no(mesh):
    assert mesh.trust_keys(CLAUDE_TRUST) == ["down", "enter"]


def test_trust_keys_ignores_other_prompts(mesh):
    assert mesh.trust_keys("Select a theme\n❯ Dark\n  Light\n") is None


# Claude Code 2.1.310 in a repo whose .claude/settings.json has a malformed rule (ccswarm), as `pane read` shows it.
SETTINGS_WARNING = """ubuntu@host:~/research/orchestrators/ccswarm$ claude --dangerously-skip-permissions

────────────────────────────────────────────────────────────
  Settings Warning

  /home/ubuntu/research/orchestrators/ccswarm/.claude/settings.json
  └ permissions.deny: Invalid permission rule "Bash(rm -rf /)*" was skipped: Malformed Tool(content) rule.

  The values listed above were skipped; the rest of the file is in effect.

  ❯ 1. Continue
    2. Fix with Claude
    3. Exit and fix manually

  Enter to confirm · Esc to cancel
"""


def test_startup_screen_classifies_trust_known_dialogs_and_the_rest(mesh):
    assert mesh.startup_screen(AGY_TRUST) == ("trust", None)
    assert mesh.startup_screen(CLAUDE_TRUST) == ("trust", None)
    assert mesh.startup_screen(SETTINGS_WARNING) == ("dialog", "Settings Warning")
    imports = "Allow external CLAUDE.md file imports?\n❯ No, disable external imports\n  Yes, allow external imports\n"
    assert mesh.startup_screen(imports) == ("dialog", "external CLAUDE.md imports")
    # a heading quoted without its dialog's own options (a diff, a doc) isn't the dialog
    assert mesh.startup_screen("+  Settings Warning\n+ shown on a bad rule\n") == ("unknown", None)
    assert mesh.startup_screen("  Settings Warning\n  press Enter to confirm it\n") == ("unknown", None)
    assert mesh.startup_screen("Select a theme\n❯ Dark\n  Light\n") == ("unknown", None)


CLAUDE_TRUST_YES = CLAUDE_TRUST.replace("❯ No, exit\n   Yes", "  No, exit\n ❯ Yes")
WELCOME = "Claude Code v2.1.310\n────────────\n❯ \n────────────\n"


def test_trust_is_confirmed_only_once_the_screen_shows_yes_selected(mesh, fake):
    fake.screen("w9:p10", CLAUDE_TRUST_YES, CLAUDE_TRUST_YES, WELCOME)  # the dialog takes a read to go
    assert mesh.accept_trust_prompt("w9:p10", CLAUDE_TRUST) is True
    keys = [c[3] for c in fake.called("pane", "send-keys")]
    assert keys == ["down", "enter"]
    assert fake.called("agent", "wait", "w9:p10")[0][-3:] == ("blocked", "--timeout", "60000")


def test_a_trust_dialog_that_stays_after_enter_is_not_accepted(mesh, fake):
    fake.screen("w9:p10", AGY_TRUST)
    assert mesh.accept_trust_prompt("w9:p10", AGY_TRUST) is False
    assert [c[3] for c in fake.called("pane", "send-keys")] == ["enter"]  # never a second Enter
    assert not fake.called("agent", "wait")


def test_a_slow_start_after_trust_is_not_held(mesh, fake, monkeypatch):
    # The dialog reads `blocked`; herdr's state lags once it's gone, then the agent reads idle.
    fake.screen("w9:p10", CLAUDE_TRUST, CLAUDE_TRUST_YES, WELCOME)
    states = iter(["blocked", "blocked", "idle"])
    monkeypatch.setattr(mesh, "startup_state", lambda name, pane: next(states))
    assert mesh.clear_startup("proj-x", "w9:p10") == {}
    assert [c[3] for c in fake.called("pane", "send-keys")] == ["down", "enter"]


def test_a_cli_that_exits_at_startup_is_an_error(mesh, fake):
    fake.on("agent", "get", errors=["agent_not_found"] * 20)  # neither by name nor by pane, on every read
    fake.screen("w9:p10", "ubuntu@host:~/proj$ claude\nError: bad flag\nubuntu@host:~/proj$ ")
    with pytest.raises(mesh.MeshError, match=r"(?s)proj-x exited during startup; last lines of its pane:\n.*bad flag"):
        mesh.clear_startup("proj-x", "w9:p10")


def test_a_lost_arrow_never_confirms_no_exit(mesh, fake):
    fake.screen("w9:p10", CLAUDE_TRUST)  # the arrow never lands: "No, exit" stays selected
    assert mesh.accept_trust_prompt("w9:p10", CLAUDE_TRUST) is False
    assert [c[3] for c in fake.called("pane", "send-keys")] == ["down", "down"]  # tried twice, never Enter


def test_trust_then_a_settings_warning_holds_the_spawn_task(mesh, fake, run, repo, monkeypatch, capsys):
    fake.on("workspace", "list", result={"workspaces": [{"workspace_id": "w9", "label": "proj"}]})
    fake.on("tab", "create", result={"tab": {"tab_id": "w9:t4"}, "root_pane": {"pane_id": "w9:p10"}})
    fake.on("agent", "start", errors=["agent_not_ready"])
    fake.screen("w9:p10", CLAUDE_TRUST, CLAUDE_TRUST_YES, SETTINGS_WARNING)

    assert mesh.main(["spawn", "--kind", "claude", "--repo", str(repo), "--worktree", "fix", "--task", "Do it."]) == 4
    captured = capsys.readouterr()
    out = json.loads(captured.out)
    assert len(fake.called("agent", "start")) == 1  # not retried into a running CLI
    assert [c[3] for c in fake.called("pane", "send-keys")] == ["down", "enter"]
    assert not fake.called("agent", "prompt")  # nothing typed into the warning
    assert out["delivery"]["held"] == "Settings Warning" and out["delivery"]["rung"] is False
    [task] = mesh.unread(run, "proj-fix")
    assert "Do it." in task.read_text()
    assert captured.err.startswith("ALERT proj-fix is waiting at a startup screen (Settings Warning)")
    assert "Its task is stored" in captured.err
    alert, _, warning = captured.err.partition("\nwarning: ")
    assert alert.rstrip().endswith("Enter to confirm · Esc to cancel")
    assert warning.startswith("no `mesh watch` covers proj-fix")  # main's warning comes after the alert
    entry = mesh.load_run(run)["agents"]["proj-fix"]
    assert entry["status"] == "running" and entry["held"]["file"] == task.name


def test_a_timed_out_start_is_classified_not_retried(mesh, fake, run, repo, capsys):
    fake.on("workspace", "list", result={"workspaces": [{"workspace_id": "w9", "label": "proj"}]})
    fake.on("tab", "create", result={"tab": {"tab_id": "w9:t4"}, "root_pane": {"pane_id": "w9:p10"}})
    fake.on("agent", "start", errors=["timeout"])
    fake.on("agent", "get", result={"agent": {"agent_status": "idle"}})
    fake.screen("w9:p10", "welcome\n────────────\n❯ \n")  # a slow start that got there
    assert mesh.main(["spawn", "--kind", "claude", "--repo", str(repo), "--worktree", "slow", "--task", "Go."]) == 0
    assert len(fake.called("agent", "start")) == 1
    assert json.loads(capsys.readouterr().out)["delivery"]["rung"] is True


def test_an_unknown_startup_screen_is_held(mesh, fake):
    fake.on("agent", "get", result={"agent": {"agent_status": "blocked"}})
    fake.screen("w9:p10", "Select a theme\n❯ Dark\n  Light\n")
    held = mesh.clear_startup("proj-x", "w9:p10")
    assert held["held"] == "a screen mesh doesn't know" and held["tail"].endswith("    Light")
    assert not fake.called("pane", "send-keys")


def held_worker(mesh, run, mtype="task"):
    path = mesh.record(run, "repo-lead", "repo-fix-typo", mtype, "Do it.")
    with mesh.editing_run(run) as data:
        data["agents"]["repo-fix-typo"]["held"] = {"sender": "repo-lead", "type": mtype, "file": path.name,
                                                   "screen": "Settings Warning", "at": mesh.now_iso()}
    return path


def test_the_watch_rings_a_held_worker_once_its_startup_screen_is_gone(mesh, fake, run, monkeypatch, capsys):
    held_worker(mesh, run)
    states = iter(["blocked"] * 3 + ["idle", "idle"])
    monkeypatch.setattr(mesh, "agent_status", lambda name, *_: next(states))
    watch, other = mesh.Watch(run, "repo-fix-typo", 30), mesh.Watch(run, "repo-fix-typo", 30)
    assert [watch.tick(5) for _ in range(3)] == [None] * 3  # its start reported the screen: no exit 4 here
    assert not fake.called("agent", "prompt")
    assert watch.tick(5) is None and other.tick(5) is None  # two watches see it idle; one rings
    [prompt] = fake.called("agent", "prompt")  # rung once, with the doorbell that says who it is
    assert prompt[3].startswith("📬 [mesh] You are repo-fix-typo (worker) in agent-mesh run mtest")
    assert "held" not in mesh.load_run(run)["agents"]["repo-fix-typo"]


def test_a_held_worker_that_exits_is_gone(mesh, fake, run, monkeypatch):
    held_worker(mesh, run)
    monkeypatch.setattr(mesh, "agent_status", lambda name, *_: "gone")
    assert mesh.Watch(run, "repo-fix-typo", 30).tick(0)[0] == 5
    assert "held" not in mesh.load_run(run)["agents"]["repo-fix-typo"]


def test_mesh_ring_gives_a_held_worker_its_first_doorbell(mesh, fake, run, capsys):
    held_worker(mesh, run)
    assert mesh.main(["ring", "repo-fix-typo"]) == 0
    assert fake.called("agent", "prompt")[-1][3].startswith("📬 [mesh] You are repo-fix-typo (worker) in agent-mesh")
    assert "held" not in mesh.load_run(run)["agents"]["repo-fix-typo"]


def test_approve_holds_the_answer_when_the_reopened_pane_waits_at_a_startup_screen(mesh, fake, run, monkeypatch,
                                                                                    capsys):
    planning_worker(mesh, run, kind="claude", model="opus", session="s-1")
    stub_new_pane(fake)
    fake.on("agent", "start", errors=["agent_not_ready"])
    fake.screen("w9:p10", SETTINGS_WARNING)
    assert mesh.main(["approve", "repo-fix-typo"]) == 4
    assert not fake.called("agent", "prompt")
    entry = mesh.load_run(run)["agents"]["repo-fix-typo"]
    assert entry["held"]["type"] == "answer" and entry["phase"] == "exec" and entry["swapped"]
    [msg] = mesh.unread(run, "repo-fix-typo")
    assert msg.name.endswith("-answer.md")


def test_switch_holds_its_continue_task_at_a_startup_screen(mesh, fake, run, monkeypatch, capsys):
    planning_worker(mesh, run, model="gemini-4-argon-high")
    with_quotas(mesh, monkeypatch)
    stub_new_pane(fake)
    fake.on("agent", "start", errors=["agent_not_ready"])
    fake.screen("w9:p10", SETTINGS_WARNING)
    assert mesh.main(["switch", "repo-fix-typo"]) == 4
    assert not fake.called("agent", "prompt")
    assert mesh.load_run(run)["agents"]["repo-fix-typo"]["held"]["type"] == "task"
    assert "Continue exactly where you stopped" in mesh.unread(run, "repo-fix-typo")[0].read_text()
    assert capsys.readouterr().err.startswith("ALERT repo-fix-typo is waiting at a startup screen (Settings Warning)")


def test_resume_holds_its_task_at_a_startup_screen(mesh, fake, run, repo, capsys):
    archived_worker(mesh, repo, session="c1")
    stub_new_pane(fake)
    fake.on("agent", "start", errors=["agent_not_ready"])
    fake.screen("w9:p10", SETTINGS_WARNING)
    assert mesh.main(["resume", "proj-fix", "--task", "Apply the review."]) == 4
    assert not fake.called("agent", "prompt")
    entry = mesh.load_run(run)["agents"]["proj-fix"]
    assert entry["held"]["screen"] == "Settings Warning" and entry["status"] == "running"
    assert "Apply the review." in mesh.unread(run, "proj-fix")[0].read_text()


def test_a_message_to_a_held_worker_is_stored_not_typed(mesh, fake, run):
    fake.on("agent", "get", result={"agent": {"agent_status": "blocked"}})
    with mesh.editing_run(run) as data:  # spawned with no task, so nothing waits to be rung yet
        data["agents"]["repo-fix-typo"]["held"] = {"sender": None, "type": None, "file": None,
                                                   "screen": "Settings Warning", "at": mesh.now_iso()}
    delivery = mesh.deliver(run, "repo-lead", "repo-fix-typo", "task", "Do it.")
    assert delivery["held"] == "Settings Warning" and not delivery["rung"] and not fake.called("agent", "prompt")
    held = mesh.load_run(run)["agents"]["repo-fix-typo"]["held"]
    assert (held["sender"], held["type"], held["file"]) == ("repo-lead", "task", Path(delivery["stored"]).name)
    mesh.deliver(run, "repo-lead", "repo-fix-typo", "info", "Also this.")  # the first one stays the one rung
    assert mesh.load_run(run)["agents"]["repo-fix-typo"]["held"]["type"] == "task"


def test_a_stale_hold_is_dropped_and_the_message_gets_the_first_doorbell(mesh, fake, run):
    held_worker(mesh, run)
    fake.on("agent", "get", result={"agent": {"agent_status": "working"}})  # the user cleared it and prompted it
    delivery = mesh.deliver(run, "repo-lead", "repo-fix-typo", "answer", "Yes.")
    assert delivery["rung"] and "held" not in delivery
    assert fake.called("agent", "prompt")[-1][3].startswith("📬 [mesh] You are repo-fix-typo (worker) in agent-mesh")
    assert "held" not in mesh.load_run(run)["agents"]["repo-fix-typo"]


def test_reading_mail_drops_the_readers_hold_so_later_mail_rings(mesh, fake, run, monkeypatch):
    held_worker(mesh, run)
    as_worker(monkeypatch, run)
    assert mesh.main(["inbox", "--peek"]) == 0  # a peek proves nothing
    assert "held" in mesh.load_run(run)["agents"]["repo-fix-typo"]
    assert mesh.main(["inbox"]) == 0
    assert "held" not in mesh.load_run(run)["agents"]["repo-fix-typo"]
    fake.on("agent", "get", result={"agent": {"agent_status": "blocked"}})  # a later permission prompt
    delivery = mesh.deliver(run, "repo-lead", "repo-fix-typo", "answer", "Yes.")
    assert delivery["rung"] and "held" not in delivery and fake.called("agent", "prompt")


def test_a_failed_release_keeps_the_hold_for_a_retry(mesh, fake, run, monkeypatch):
    held_worker(mesh, run)
    monkeypatch.setattr(mesh, "agent_status", lambda name, *_: "idle")
    monkeypatch.setattr(mesh, "ring", lambda *a, **k: {"rung": False, "error": "agent_prompt_stalled", "via": None, "tries": 1})
    watch = mesh.Watch(run, "repo-fix-typo", 30)
    assert watch.tick(5) is None and watch.tick(5) is None  # quiet while it retries
    assert mesh.load_run(run)["agents"]["repo-fix-typo"]["held"]["tries"] == 2
    assert mesh.release_held(run, "repo-fix-typo", mesh.agent_entry(mesh.load_run(run), "repo-fix-typo"),
                             "idle") is False  # the third: the watch carries on, its nudge rings it
    assert mesh.load_run(run)["agents"]["repo-fix-typo"]["held"]["tries"] == 3  # `mesh ring` still sees it


def test_a_release_rings_for_any_unread_message(mesh, fake, run, monkeypatch):
    path = held_worker(mesh, run)
    mesh.mark_read(path)
    mesh.record(run, "repo-lead", "repo-fix-typo", "answer", "Later.")
    monkeypatch.setattr(mesh, "agent_status", lambda name, *_: "idle")
    assert mesh.Watch(run, "repo-fix-typo", 30).tick(5) is None
    assert fake.called("agent", "prompt")[-1][3].startswith("📬 [mesh] You are repo-fix-typo (worker) in agent-mesh")


def test_mesh_ring_clears_a_hold_with_nothing_to_ring(mesh, fake, run, capsys):
    with mesh.editing_run(run) as data:
        data["agents"]["repo-fix-typo"]["held"] = {"sender": None, "type": None, "file": None,
                                                   "screen": "Settings Warning", "at": mesh.now_iso()}
    assert mesh.main(["ring", "repo-fix-typo"]) == 0
    assert "startup hold is cleared" in capsys.readouterr().out
    assert "held" not in mesh.load_run(run)["agents"]["repo-fix-typo"]


def test_start_grace_counts_from_the_release(mesh, fake, run):
    held_worker(mesh, run)
    watch = mesh.Watch(run, "repo-fix-typo", 30)
    events = mesh.read_log(run)
    for e in events:
        if e.get("event") == "send":
            e["time"] = "2026-01-01T00:00:00+00:00"  # its task is long past start_grace
    assert watch.idle_from_start(events) is True
    events.append({"event": "held_rung", "name": "repo-fix-typo", "rung": False, "time": mesh.now_iso()})
    assert watch.idle_from_start(events) is True  # a ring that failed told it nothing
    events.append({"event": "held_rung", "name": "repo-fix-typo", "rung": True, "time": mesh.now_iso()})
    assert watch.idle_from_start(events) is False  # but it heard of it only now


def test_a_held_message_already_read_is_not_rung(mesh, fake, run, monkeypatch):
    path = held_worker(mesh, run)
    mesh.mark_read(path)
    monkeypatch.setattr(mesh, "agent_status", lambda name, *_: "idle")
    assert mesh.Watch(run, "repo-fix-typo", 30).tick(5) is None
    assert not fake.called("agent", "prompt") and "held" not in mesh.load_run(run)["agents"]["repo-fix-typo"]


def test_doorbell_confirms_delivery_and_retries_when_stalled(mesh, fake, run):
    fake.on("agent", "prompt", errors=["agent_prompt_stalled"])
    assert mesh.main(["send", "repo-fix-typo", "--type", "info", "--body", "hi"]) == 0
    calls = fake.called("agent", "prompt")
    assert len(calls) == 2
    assert calls[0][4:8] == ("--wait", "--until", "working", "--timeout")


BOX = "─" * 40


def screen(*above, typed=""):
    """An agent's pane: transcript lines, then its input box (with what is typed in it) and a footer."""
    return "\n".join([*above, BOX, f"❯ {typed}", BOX, "  esc to interrupt"])


def fixed_token(mesh, monkeypatch, token="abc123"):
    monkeypatch.setattr(mesh, "ring_token", lambda: token)
    return token


def test_a_doorbell_a_busy_agent_queued_is_not_typed_again(mesh, fake, run, monkeypatch):
    # Claude Code running a command queues the prompt, and herdr times out waiting for "working"
    fixed_token(mesh, monkeypatch)
    fake.on("agent", "prompt", errors=["timeout"] * 7)
    queued = "❯ 📬 [mesh] info from repo-lead (ref abc123): hi (run `mesh inbox`)"
    fake.screen("repo-fix-typo", screen("  ⎿  Running… (24s)"), screen("  ⎿  Running… (24s)"),
                screen("  ⎿  Running… (24s)", queued))
    assert mesh.main(["send", "repo-fix-typo", "--type", "info", "--body", "hi"]) == 0
    assert len(fake.called("agent", "prompt")) == 1
    rung = [e for e in mesh.read_log(run) if e["event"] == "rung"]
    assert [(e["to"], e["via"], e["tries"]) for e in rung] == [("repo-fix-typo", "pane", 1)]
    assert "type" not in rung[0]  # never counted as a send


def test_an_earlier_identical_doorbell_does_not_count(mesh, fake, run):
    fake.on("agent", "prompt", errors=["agent_prompt_stalled"])
    old = "> 📬 [mesh] info from repo-lead (ref 0ld0ld): hi (run `mesh inbox`)"
    fake.screen("repo-fix-typo", screen(old, "  Done."))  # the same doorbell from before, already handled
    assert mesh.main(["send", "repo-fix-typo", "--type", "info", "--body", "hi"]) == 0
    assert len(fake.called("agent", "prompt")) == 2


def test_an_earlier_doorbell_submitted_meanwhile_does_not_confirm_a_new_one(mesh, fake, run):
    # the old doorbell sat in the box and got submitted between the reads; the new one is still unsent
    fake.on("agent", "prompt", errors=["agent_prompt_stalled"])
    old = "📬 [mesh] info from repo-lead (ref 0ld0ld): hi (run `mesh inbox`)"
    fake.screen("repo-fix-typo", screen("  Done.", typed=old), screen("  Done.", typed=old),
                screen("  Done.", "❯ " + old))
    assert mesh.main(["send", "repo-fix-typo", "--type", "info", "--body", "hi"]) == 0
    assert len(fake.called("agent", "prompt")) == 2


def test_a_doorbell_left_in_the_input_box_is_typed_again(mesh, fake, run):
    fake.on("agent", "prompt", errors=["agent_prompt_stalled"])
    fake.screen("repo-fix-typo", screen("  Done."), screen("  Done.", typed="📬 [mesh] info from repo-lead: hi"))
    assert mesh.main(["send", "repo-fix-typo", "--type", "info", "--body", "hi"]) == 0
    assert len(fake.called("agent", "prompt")) == 2


def test_the_input_box_is_found_above_a_task_panel(mesh):
    # agy draws its background tasks under the input box, behind one more rule
    lines = ["> 📬 [mesh] hi", "", BOX, ">", BOX, "  ● sleep 45 running", BOX, "? for shortcuts"]
    assert mesh.above_input_box(lines) == ["> 📬 [mesh] hi", ""]
    # a panel row that starts with ">" is indented; only the input line starts at column 0
    lines = ["> 📬 [mesh] hi", BOX, ">", BOX, "  > npm test", BOX, "? for shortcuts"]
    assert mesh.above_input_box(lines) == ["> 📬 [mesh] hi"]
    # no input box found: nothing counts, so a doorbell can't be confirmed from what may be the box itself
    assert mesh.above_input_box(["no box here"]) == []


def test_a_pane_that_could_not_be_read_first_never_confirms_a_doorbell(mesh, fake, run, monkeypatch):
    fixed_token(mesh, monkeypatch)
    fake.on("agent", "prompt", errors=["agent_prompt_stalled"])
    new = "❯ 📬 [mesh] info from repo-lead (ref abc123): hi (run `mesh inbox`)"
    # the first read fails; a later one shows the doorbell, which may have been there before
    fake.screen("repo-fix-typo", None, screen(new))
    assert mesh.main(["send", "repo-fix-typo", "--type", "info", "--body", "hi"]) == 0
    assert len(fake.called("agent", "prompt")) == 2


def test_a_wrapped_doorbell_is_still_found(mesh, fake, run, monkeypatch):
    fixed_token(mesh, monkeypatch)
    fake.on("agent", "prompt", errors=["timeout"] * 7)
    wrapped = ["❯ 📬 [mesh] info", "  from repo-lead (ref abc", "  123): hi (run", "  `mesh inbox`)"]  # narrow pane
    fake.screen("repo-fix-typo", screen("  ⎿  Running…"), screen("  ⎿  Running…"), screen("  ⎿  Running…", *wrapped))
    assert mesh.main(["send", "repo-fix-typo", "--type", "info", "--body", "hi"]) == 0
    assert len(fake.called("agent", "prompt")) == 1


def test_a_message_read_meanwhile_stops_the_doorbell(mesh, fake, run, monkeypatch):
    fake.on("agent", "prompt", errors=["agent_prompt_stalled"] * 7)

    def read_it(seconds):  # the agent ran `mesh inbox` (after an earlier doorbell) during the first backoff
        for path in mesh.unread(run, "repo-fix-typo"):
            mesh.mark_read(path)

    monkeypatch.setattr(mesh.time, "sleep", read_it)
    assert mesh.main(["send", "repo-fix-typo", "--type", "info", "--body", "hi"]) == 0
    assert len(fake.called("agent", "prompt")) == 1
    rung = [e for e in mesh.read_log(run) if e["event"] == "rung"]
    assert [(e["via"], e["tries"]) for e in rung] == [("read", 1)]


# Rows captured with `herdr agent read --ansi` (herdr 0.8.2, Claude Code 2.1.310, agy 1.4.7; rules shortened).
CLAUDE_RULE = "\x1b[0m\x1b[38;2;136;136;136m" + "─" * 40 + "\x1b[0m"
CLAUDE_FOOTER = "  \x1b[0m\x1b[38;2;255;107;128m⏵⏵ bypass permissions on\x1b[0m"
CLAUDE_ECHO = ("\x1b[0m\x1b[38;2;153;153;153m\x1b[48;2;55;55;55m❯ \x1b[0m\x1b[38;2;255;255;255m"
               "\x1b[48;2;55;55;55msay hi\x1b[0m")
AGY_RULE = "\x1b[0m\x1b[38;5;8m" + "─" * 40 + "\x1b[0m"
AGY_FOOTER = "\x1b[0m\x1b[38;5;8m? for shortcuts         \x1b[0m\x1b[2mGemini 3.8 Flash · medium\x1b[0m"


def claude_pane(*box):
    return [CLAUDE_ECHO, "", CLAUDE_RULE, *box, CLAUDE_RULE, CLAUDE_FOOTER]


def agy_pane(*box):
    return ["\x1b[0m\x1b[1m\x1b[38;5;4m> say hi\x1b[0m", "", AGY_RULE, *box, AGY_RULE, AGY_FOOTER]


@pytest.mark.parametrize("lines, typed", [
    (claude_pane("❯\xa0"), ""),
    (claude_pane('❯\xa0\x1b[0m\x1b[2mTry "fix typecheck errors"\x1b[0m'), ""),  # ghost text is dim
    (claude_pane("❯\xa0hello draft"), "hello draft"),
    (claude_pane("❯\xa0\x1b[38;5;153m/model\x1b[0m"), "/model"),  # a coloured slash command is typed
    (claude_pane("❯\xa0word word", "  word", "  word"), "word word word word"),  # a wrapped draft
    (agy_pane("\x1b[0m\x1b[38;5;12m>\x1b[0m"), ""),
    (agy_pane("\x1b[0m\x1b[38;5;12m> \x1b[0mhello draft"), "hello draft"),
    (["no box here"], None),
    (claude_pane("❯\xa0hello draft")[:-2], None),  # no closing rule: what follows may be a footer
    (claude_pane("❯\xa0see \x1b]8;;https://x.io\x1b\\x.io\x1b]8;;\x1b\\"), "see x.io"),  # an OSC 8 link
    (claude_pane("❯\xa0\x1b[2Kodd"), None),  # an escape that isn't SGR or OSC: can't tell
])
def test_typed_text_reads_the_input_box(mesh, lines, typed):
    assert mesh.typed_text(lines) == typed


def test_only_a_human_draft_holds_a_doorbell(mesh):
    assert mesh.human_draft(claude_pane("❯\xa0hello draft"))
    assert not mesh.human_draft(claude_pane('❯\xa0\x1b[0m\x1b[2mTry "fix typecheck errors"\x1b[0m'))
    assert not mesh.human_draft(claude_pane("❯\xa0📬 [mesh] info from repo-lead: hi"))  # an earlier doorbell
    assert not mesh.human_draft(["no box here"])  # can't tell: type
    assert not mesh.human_draft(None)  # unreadable: type


def test_a_draft_holds_the_doorbell_until_it_is_sent(mesh, fake, run):
    busy_draft = "\n".join(agy_pane("\x1b[0m\x1b[38;5;12m> \x1b[0mbusy draft"))
    fake.screen("repo-fix-typo", busy_draft, busy_draft, "\n".join(agy_pane("\x1b[0m\x1b[38;5;12m>\x1b[0m")))
    assert mesh.main(["send", "repo-fix-typo", "--type", "info", "--body", "hi"]) == 0
    assert len(fake.called("agent", "prompt")) == 1  # typed only once the user had sent their draft


def test_a_draft_that_stays_ends_in_exit_2(mesh, fake, run, capsys):
    fake.screen("repo-fix-typo", "\n".join(claude_pane("❯\xa0half a sentence")))
    assert mesh.main(["send", "repo-fix-typo", "--type", "info", "--body", "hi"]) == 2
    assert not fake.called("agent", "prompt")
    failed = [e for e in mesh.read_log(run) if e["event"] == "doorbell_failed"]
    assert failed[0]["error"] == "human_draft"
    assert len(mesh.unread(run, "repo-fix-typo")) == 1


def test_rings_into_one_pane_take_turns(mesh, fake, run, monkeypatch):
    import fcntl
    path = mesh.mesh_home() / "locks" / "ring-w2_p5.lock"
    path.parent.mkdir(parents=True, exist_ok=True)
    order = []
    with open(path, "w") as other:  # another ring is typing into the same pane
        fcntl.flock(other, fcntl.LOCK_EX)
        tries = iter(range(3))

        def wait(seconds):
            order.append("waited")
            if next(tries, None) == 2:
                fcntl.flock(other, fcntl.LOCK_UN)

        monkeypatch.setattr(mesh.time, "sleep", wait)
        assert mesh.main(["send", "repo-fix-typo", "--type", "info", "--body", "hi"]) == 0
    assert order == ["waited"] * 3  # it typed only once the other ring let go
    assert len(fake.called("agent", "prompt")) == 1


def test_a_ring_lock_held_too_long_is_ignored(mesh, fake, run, monkeypatch):
    import fcntl
    monkeypatch.setattr(mesh, "RING_LOCK_WAIT", 1)
    path = mesh.mesh_home() / "locks" / "ring-w2_p5.lock"
    path.parent.mkdir(parents=True, exist_ok=True)
    with open(path, "w") as other:
        fcntl.flock(other, fcntl.LOCK_EX)
        assert mesh.main(["send", "repo-fix-typo", "--type", "info", "--body", "hi"]) == 0
    assert len(fake.called("agent", "prompt")) == 1


def test_a_doorbell_carries_its_token_and_keeps_the_searched_start(mesh, run):
    text = mesh.doorbell_text(mesh.load_run(run), "repo-lead", "repo-fix-typo", "task", "x", True, "abc123")
    assert text.startswith(f"📬 [mesh] {mesh.first_doorbell('repo-fix-typo')}")
    assert f"in agent-mesh run {run} (ref abc123);" in text  # early, where a shortened prompt keeps it
    assert mesh.doorbell_text(mesh.load_run(run), "repo-lead", "repo-fix-typo", "info", "hi", False, "abc123") \
        == "📬 [mesh] info from repo-lead (ref abc123): hi (run `mesh inbox`)"
    assert "ref" not in mesh.doorbell_text(mesh.load_run(run), "repo-lead", "repo-fix-typo", "info", "hi", False)


def test_the_preview_never_reads_as_shell(mesh):
    assert mesh.preview("Run `rm -rf $HOME` and \"quote\" it, don't") == "Run rm -rf HOME and quote it, don’t"


def test_agy_listing_falls_back_to_stale_cache(mesh, monkeypatch):
    cache = mesh.mesh_home() / "agy-models.txt"
    mesh.write_atomic(cache, AGY_MODELS)
    import os
    os.utime(cache, (0, 0))  # far older than the TTL

    def hang(*a, **k):
        raise mesh.subprocess.TimeoutExpired("agy models", 45)

    monkeypatch.setattr(mesh.subprocess, "run", hang)
    assert mesh.resolve_agy_model("flash", "high") == "gemini-3.8-flash-high"


def test_agy_listing_failure_without_cache_is_a_clean_error(mesh, monkeypatch, capsys):
    def hang(*a, **k):
        raise mesh.subprocess.TimeoutExpired("agy models", 45)

    monkeypatch.setattr(mesh.subprocess, "run", hang)
    assert mesh.main(["model", "--kind", "agy", "--model", "flash"]) == 1
    assert "no cached list" in capsys.readouterr().err
    assert mesh.resolve_agy_model("gemini-3.8-flash-low", "high") == "gemini-3.8-flash-low"


# ── names and workspaces ─────────────────────────────────────────────────


def test_agent_names_are_readable_valid_and_unique(mesh):
    assert mesh.agent_name("shop-api", "restocked-at", set()) == "shop-api-restocked-at"
    assert mesh.agent_name("shop-api", "restocked-at", {"shop-api-restocked-at"}) == "shop-api-restocked-at-2"
    long = mesh.agent_name("a-really-long-repository-name", "Fix the reaction handler bug", set())
    assert mesh.NAME_RE.match(long) and long.endswith("fix-the-reaction-han")
    assert mesh.NAME_RE.match(mesh.agent_name("3d-viewer", "lead", set()))


def test_init_keeps_a_root_name_you_chose(mesh, fake, monkeypatch, capsys):
    monkeypatch.setenv("HERDR_ENV", "1")
    monkeypatch.setenv("HERDR_PANE_ID", "w1:p1")
    fake.on("agent", "list", result={"agents": [{"pane_id": "w1:p1", "agent": "claude", "name": "boss", "cwd": "/x"}]})
    assert mesh.main(["init", "--mode", "claude", "--run-id", "mkeep"]) == 0
    assert mesh.load_run("mkeep")["root"] == "boss"
    assert not fake.called("agent", "rename")


def test_restart_in_a_worktree_reuses_its_slug(mesh, run):
    import argparse
    with mesh.editing_run(run) as data:
        data["worktrees"] = [{"path": "/wt/proj-fix-reactions", "repo": "/proj", "branch": "mesh/mtest/fix-reactions"}]
    args = argparse.Namespace(name=None, role="worker", worktree=None, cwd="/wt/proj-fix-reactions")
    suffix = mesh.spawn_suffix(args, mesh.load_run(run))
    assert mesh.agent_name("proj", suffix, {"proj-fix-reactions"}) == "proj-fix-reactions-2"


def test_tab_label_comes_from_the_request(mesh):
    assert mesh.tab_label({"id": "m1", "request": "add restockedAt to shop-api and use it in shop-web"}) == "mesh: add restockedAt to shop-api"
    assert mesh.tab_label({"id": "m1", "request": ""}) == "mesh: m1"


def test_workspace_found_by_label_then_by_pane_inside_repo(mesh, fake, tmp_path):
    repo = tmp_path / "shop-api"
    (repo / "src").mkdir(parents=True)
    fake.on("workspace", "list", result={"workspaces": [{"workspace_id": "w1", "label": "other"},
                                                        {"workspace_id": "w2", "label": "shop"}]})
    fake.on("pane", "list", "--workspace", "w2", result={"panes": [{"cwd": str(repo / "src")}]})
    assert mesh.find_or_create_workspace(repo) == ("w2", False)
    fake.on("workspace", "list", result={"workspaces": [{"workspace_id": "w3", "label": "Shop-API"}]})
    assert mesh.find_or_create_workspace(repo) == ("w3", False)
    assert not fake.called("workspace", "create")


def test_workspace_created_only_when_the_project_has_none(mesh, fake, tmp_path):
    repo = tmp_path / "newproj"
    repo.mkdir()
    fake.on("workspace", "list", result={"workspaces": [{"workspace_id": "w1", "label": "other"}]})
    fake.on("pane", "list", result={"panes": [{"cwd": str(tmp_path)}]})  # a parent dir is not a match
    fake.on("workspace", "create", result={"workspace": {"workspace_id": "w7"}})
    assert mesh.find_or_create_workspace(repo) == ("w7", True)
    assert fake.called("workspace", "create", "--cwd", str(repo), "--label", "newproj")


# ── past workers: sessions, agents, resume, open ─────────────────────────


def archived_worker(mesh, repo, kind="agy", session=None, run_id="mold"):
    """An archived run whose worker opened PR 7; `mesh stop` removed its worktree but kept the branch."""
    path = mesh.worktree_root() / run_id / "proj-fix"
    branch = f"mesh/{run_id}/fix"
    subprocess.run(["git", "branch", branch], cwd=repo, check=True)
    entry = {"role": "worker", "kind": kind, "model": "gemini-3.8-flash-high" if kind == "agy" else "opus",
             "effort": "high", "repo": str(repo), "cwd": str(path), "branch": branch, "pane": "w7:p1",
             "parent": "old-lead", "status": "running"}
    if session:
        entry["session"] = session
    mesh.write_atomic(mesh.mesh_home() / "agy-models.txt", AGY_MODELS)  # with_quotas: nothing runs agy
    mesh.agy_usage = lambda force=False: mesh.parse_usage(USAGE)
    folder = mesh.mesh_home() / "archive" / run_id
    (folder / "inbox" / "old-lead" / "read").mkdir(parents=True)
    (folder / "run.json").write_text(json.dumps({
        "id": run_id, "created": "2026-01-01T00:00:00+00:00", "mode": "agy", "root": "old-lead",
        "agents": {"old-lead": {"role": "root", "pane": "w7:p0"}, "proj-fix": entry},
        "worktrees": [{"path": str(path), "repo": str(repo), "branch": branch}],
    }))
    (folder / "inbox/old-lead/read/20260101T000000Z-proj-fix-done.md").write_text(
        f"---\nrun: {run_id}\nfrom: proj-fix\nto: old-lead\ntype: done\n---\n"
        "Opened https://github.com/acme/proj/pull/7, tests pass.\n"
    )
    return path, branch


def test_agy_conversation_comes_from_the_first_doorbell(mesh, tmp_path, monkeypatch):
    history = tmp_path / "history.jsonl"
    monkeypatch.setenv("MESH_AGY_HISTORY", str(history))
    wt = "/wt/proj-fix"
    lines = [
        {"display": "📬 [mesh] plan from proj-fix: You are proj-fix (…", "workspace": "/repo", "conversationId": "root"},
        {"display": "📬 [mesh] You are proj-fix (worker) in agent-mesh run mold; …", "workspace": wt, "conversationId": "c1"},
        {"display": "📬 [mesh] You are proj-other (worker) in agent-mesh run mold; …", "workspace": "/wt/o", "conversationId": "c2"},
        {"display": "📬 [mesh] You are proj-fix-2 (worker) in agent-mesh run mold; …", "workspace": wt, "conversationId": "c3"},
        {"display": "📬 [mesh] You are proj-new (worker) in agent-mesh run mold (ref 1a2b3c); …", "workspace": "/wt/n",
         "conversationId": "c4"},  # a rung first doorbell carries its token right after the run
    ]
    history.write_text("\n".join(json.dumps(line) for line in lines) + "\nnot json\n")
    assert mesh.agy_conversation("proj-fix", "mold", wt) == "c1"
    assert mesh.agy_conversation("proj-fix-2", "mold", wt) == "c3"
    assert mesh.agy_conversation("proj-fix", "mol", wt) == "c3"  # wrong run: only the cwd fallback is left
    assert mesh.agy_conversation("ghost", "mold", None) is None
    assert mesh.agy_conversation("proj-new", "mold", None) == "c4"


def test_agy_worker_in_a_main_checkout_never_borrows_the_users_session(mesh, tmp_path, monkeypatch):
    history = tmp_path / "history.jsonl"
    monkeypatch.setenv("MESH_AGY_HISTORY", str(history))
    history.write_text(json.dumps({"display": "fix the login bug", "workspace": "/repo", "conversationId": "mine"}))
    entry = {"kind": "agy", "repo": "/repo", "cwd": "/repo"}
    assert mesh.find_session("mold", "repo-planner", entry) is None
    assert mesh.find_session("mold", "repo-planner", {**entry, "cwd": "/wt/x", "session": "s"}) == "s"


# agy's conversation index, `CREATE TABLE` copied from the real ~/.gemini/antigravity-cli/conversation_summaries.db
AGY_SUMMARIES_SCHEMA = """CREATE TABLE `conversation_summaries` (`conversation_id` text,`title` text NOT NULL DEFAULT "",`preview` text NOT NULL DEFAULT "",`step_count` integer NOT NULL DEFAULT 0,`last_modified_time` datetime NOT NULL,`workspace_uris` text NOT NULL,`status` text NOT NULL DEFAULT "",`source` text NOT NULL DEFAULT "",`project_id` text NOT NULL DEFAULT "",`agent_name` text NOT NULL DEFAULT "",`parent_conversation_id` text NOT NULL DEFAULT "",`nesting_depth` integer NOT NULL DEFAULT 0,`battle_id` text NOT NULL DEFAULT "",`winning_conversation_id` text NOT NULL DEFAULT "",`not_fully_idle` numeric NOT NULL DEFAULT false,`killed` numeric NOT NULL DEFAULT false,`last_user_input_time` datetime NOT NULL,`last_user_input_step_index` integer NOT NULL DEFAULT -1,`app_data_dir` text NOT NULL DEFAULT "",`raw_summary` blob,`group_id` text NOT NULL DEFAULT "",PRIMARY KEY (`conversation_id`))"""


def agy_summaries_db(path, rows):
    """A conversation index like agy's; each row is (id, preview, workspace, modified[, steps, depth])."""
    import sqlite3
    path.parent.mkdir(parents=True, exist_ok=True)
    db = sqlite3.connect(path)
    db.execute("PRAGMA journal_mode=WAL")  # as agy keeps it
    db.execute(AGY_SUMMARIES_SCHEMA)
    for conversation, preview, workspace, modified, *rest in rows:
        steps, depth = (*rest, 12, 0)[:2]
        db.execute("INSERT INTO conversation_summaries (conversation_id, title, preview, step_count, "
                   "last_modified_time, workspace_uris, nesting_depth, last_user_input_time) VALUES (?,?,?,?,?,?,?,?)",
                   (conversation, "Agent Mesh Setup", preview, steps, modified,
                    json.dumps([Path(workspace).as_uri()]), depth, modified))
    db.commit()
    return db  # still open: its rows may sit in the WAL only, as agy's do


def test_agy_conversation_comes_from_agys_index_before_a_second_prompt(mesh, tmp_path, monkeypatch):
    wt, other = "/wt/proj fix", "/wt/proj-other"
    history = tmp_path / "agy" / "history.jsonl"
    history.parent.mkdir(parents=True)
    # agy writes a first prompt with no conversationId
    history.write_text(json.dumps({"display": "📬 [mesh] You are proj-fix (worker) in agent-mesh run mold; …",
                                   "workspace": wt}) + "\n")
    db = agy_summaries_db(tmp_path / "agy" / "conversation_summaries.db", [
        ("old", "Agent Mesh Setup", wt, "2026-10-01 10:00:00+00:00"),  # an older agy: preview is the title
        ("c1", "📬 [mesh] You are proj-fix (worker) in agent-mesh run mold; …", wt, "2026-10-02 10:00:00+00:00"),
        ("c2", "📬 [mesh] You are proj-other (worker) in agent-mesh run mold; …", other, "2026-10-02 11:00:00+00:00"),
        ("sub", "research the API", wt, "2026-10-02 12:00:00+00:00", 5, 1),  # an agy subagent
        ("slash", "", wt, "2026-10-02 13:00:00+00:00", 0, 0),  # /model only, no turn
    ])
    assert mesh.agy_conversation("proj-fix", "mold", wt) == "c1"
    assert mesh.agy_conversation("proj-fix", "mold", None) == "c1"
    assert mesh.agy_conversation("proj-other", "mold", other) == "c2"
    assert mesh.agy_conversation("proj-fix", "mol", wt) == "c1"  # wrong run: the newest in that worktree

    # a restart sharing the worktree is pinned by its own doorbell; the first one keeps its conversation
    db.execute("INSERT INTO conversation_summaries (conversation_id, preview, step_count, last_modified_time, "
               "workspace_uris, last_user_input_time) VALUES (?,?,?,?,?,?)",
               ("c3", "📬 [mesh] You are proj-fix-2 (worker) in agent-mesh run mold; …", 3,
                "2026-10-03 10:00:00+00:00", json.dumps([Path(wt).as_uri()]), "2026-10-03 10:00:00+00:00"))
    db.commit()
    assert mesh.agy_conversation("proj-fix-2", "mold", wt) == "c3"
    assert mesh.agy_conversation("proj-fix", "mold", wt) == "c1"
    assert mesh.agy_conversation("proj-fix", "mol", wt) == "c3"
    db.close()


def test_agy_conversation_falls_back_to_history_without_a_readable_index(mesh, tmp_path, monkeypatch):
    history = tmp_path / "agy" / "history.jsonl"
    history.parent.mkdir(parents=True)
    history.write_text(json.dumps({"display": "📬 [mesh] You are proj-fix (worker) in agent-mesh run mold; …",
                                   "workspace": "/wt/fix", "conversationId": "h1"}) + "\n")
    (tmp_path / "agy" / "conversation_summaries.db").write_text("not a database")
    assert mesh.agy_conversation("proj-fix", "mold", "/wt/fix") == "h1"
    # a pinned doorbell in history beats a mere worktree match in the index
    (tmp_path / "agy" / "conversation_summaries.db").unlink()
    agy_summaries_db(tmp_path / "agy" / "conversation_summaries.db",
                     [("i1", "Agent Mesh Setup", "/wt/fix", "2026-10-02 10:00:00+00:00")]).close()
    assert mesh.agy_conversation("proj-fix", "mold", "/wt/fix") == "h1"
    assert mesh.agy_conversation("proj-other", "mold", "/wt/fix") == "i1"


def test_claude_session_found_for_workers_spawned_before_session_ids(mesh, tmp_path, monkeypatch):
    monkeypatch.setenv("CLAUDE_CONFIG_DIR", str(tmp_path / "claude"))
    cwd = "/home/u/.local/share/agent-mesh/worktrees/mold/proj-fix"
    folder = tmp_path / "claude" / "projects" / "-home-u--local-share-agent-mesh-worktrees-mold-proj-fix"
    folder.mkdir(parents=True)
    (folder / "aaa.jsonl").write_text('{"message": "unrelated"}\n')
    (folder / "bbb.jsonl").write_text('{"message": "📬 [mesh] You are proj-fix (worker) in agent-mesh run mold"}\n')
    assert mesh.claude_session("proj-fix", cwd) == "bbb"
    assert mesh.find_session("mold", "proj-fix", {"kind": "claude", "cwd": cwd}) == "bbb"
    assert mesh.claude_session("proj-other", cwd) is None


def test_find_worker_by_name_branch_or_pr_newest_first(mesh, fake, run, repo):
    archived_worker(mesh, repo)
    with mesh.editing_run(run) as data:
        data["agents"]["proj-fix"] = {"role": "worker", "kind": "agy", "cwd": "/elsewhere", "pane": "w2:p9"}
    assert mesh.find_worker("proj-fix")[0]["id"] == run  # the live run is newer
    assert mesh.find_worker("proj-fix", "mold")[0]["id"] == "mold"
    assert mesh.find_worker("mesh/mold/fix")[0]["id"] == "mold"
    assert mesh.find_worker("https://github.com/acme/proj/pull/7/files#r1")[2] == "proj-fix"
    with pytest.raises(mesh.MeshError, match="mesh agents"):
        mesh.find_worker("repo-lead")  # roots are never resumed


def test_agents_lists_archived_workers_with_their_prs(mesh, fake, run, repo, capsys):
    archived_worker(mesh, repo, session="c1")
    capsys.readouterr()
    assert mesh.main(["agents", "pull/7", "--json"]) == 0
    [row] = json.loads(capsys.readouterr().out)
    assert (row["name"], row["run"], row["archived"]) == ("proj-fix", "mold", True)
    assert row["prs"] == "https://github.com/acme/proj/pull/7"
    assert (row["worktree"], row["state"], row["session"]) == ("removed", "gone", "found")
    assert mesh.main(["agents", "nothing-like-this"]) == 0
    assert "No workers match" in capsys.readouterr().out


def test_worker_prs_lists_a_pr_once_when_a_report_repeats_it(mesh, fake, run):
    pr, other = "https://github.com/acme/proj/pull/7", "https://github.com/acme/proj/pull/8"
    mesh.store_message(run, "repo-fix-typo", "repo-lead", "done", f"Opened {pr}.\n\nPR: {pr}, and see {other}")
    mesh.store_message(run, "repo-fix-typo", "repo-lead", "done", f"Updated {other}")
    assert mesh.worker_prs(mesh.run_dir(run), "repo-fix-typo") == [pr, other]


def test_agents_run_filter_keeps_workers_resumed_into_the_run(mesh, fake, run, repo, capsys):
    archived_worker(mesh, repo, session="c1")
    with mesh.editing_run(run) as data:  # resumed here: it keeps mesh/mold/fix
        data["agents"]["proj-fix"] = {"role": "worker", "kind": "agy", "repo": str(repo), "cwd": "/gone",
                                      "branch": "mesh/mold/fix", "resumed_from": {"run": "mold", "name": "proj-fix"}}
    capsys.readouterr()
    assert mesh.main(["agents", "--run", run, "--json"]) == 0
    rows = json.loads(capsys.readouterr().out)
    assert {(r["name"], r["run"]) for r in rows} == {("repo-fix-typo", run), ("proj-fix", run)}
    assert mesh.main(["agents", f"mesh/{run}", "--json"]) == 0
    assert "proj-fix" not in {r["name"] for r in json.loads(capsys.readouterr().out)}  # why --run exists
    assert mesh.main(["agents", "proj", "--run", "mold", "--json"]) == 0
    assert [(r["name"], r["run"]) for r in json.loads(capsys.readouterr().out)] == [("proj-fix", "mold")]
    assert mesh.main(["agents", "--run", "nope"]) == 0
    assert "No workers in run nope." in capsys.readouterr().out


def started(fake):
    """The CLI arguments of the last `herdr agent start`, without the `--settings` a Claude worker gets."""
    argv = list(fake.called("agent", "start")[-1])
    if "--settings" in argv:
        del argv[argv.index("--settings"):argv.index("--settings") + 2]
    return tuple(argv)


# ── status and log ───────────────────────────────────────────────────────


def test_a_torn_log_line_is_skipped_with_one_warning(mesh, fake, run, capsys):
    path = mesh.run_dir(run) / "log.jsonl"
    with open(path, "a") as fh:
        fh.write('{"time": "2026-10-25T10:00:00+00:00", "event": "se')  # a write cut short
    mesh.log_event(run, "send", sender="repo-lead", to="repo-fix-typo", type="task", file="t.md")
    capsys.readouterr()
    events = mesh.read_log(run)
    assert events[-1]["event"] == "send"  # sealed: the event after the fragment survives
    assert all(isinstance(e, dict) for e in events)
    assert mesh.read_log(run) == events
    err = capsys.readouterr().err
    assert err.count("skipped 1 unreadable line") == 1 and str(path) in err
    fake.on("agent", "get", result={"agent": {"agent_status": "working"}})
    assert mesh.main(["status", run, "--json"]) == 0  # every reader keeps working


def status_rows(mesh, run, capsys):
    capsys.readouterr()
    assert mesh.main(["status", run, "--json"]) == 0
    data = json.loads(capsys.readouterr().out)
    return data, {r["name"]: r for r in data["agents"]}


def add_worker(mesh, run, name, **entry):
    with mesh.editing_run(run) as data:
        data["agents"][name] = {"role": "worker", "kind": "claude", "pane": f"w2:{name}", "parent": "repo-lead",
                                "status": "running", **entry}


def test_status_derives_what_each_worker_owes_needing_the_root_first(mesh, fake, run, capsys):
    fake.on("agent", "get", result={"agent": {"agent_status": "idle"}})
    for name in ("repo-plan", "repo-ask", "repo-unread", "repo-busy", "repo-lost", "repo-idle"):
        add_worker(mesh, run, name)
    mesh.record(run, "repo-lead", "repo-fix-typo", "task", "Fix the typo")  # read, no report yet
    mesh.mark_read(mesh.unread(run, "repo-fix-typo")[0])
    for name in ("repo-plan", "repo-ask", "repo-busy"):
        mesh.mark_read(mesh.record(run, "repo-lead", name, "task", "Do it"))
    mesh.record(run, "repo-plan", "repo-lead", "plan", "My plan")
    mesh.mark_read(mesh.record(run, "repo-ask", "repo-lead", "question", "Which one?"))
    mesh.record(run, "repo-lead", "repo-unread", "task", "Do it")
    lost = mesh.record(run, "repo-lead", "repo-lost", "task", "Do it")
    mesh.log_event(run, "doorbell_failed", sender="repo-lead", to="repo-lost", file=lost.name, error="timeout")
    data, rows = status_rows(mesh, run, capsys)
    assert rows["repo-plan"]["waiting"].startswith("plan unanswered ") and rows["repo-plan"]["waiting"].endswith(", unread")
    assert rows["repo-ask"]["waiting"].startswith("question unanswered ") and "unread" not in rows["repo-ask"]["waiting"]
    assert rows["repo-unread"]["waiting"].startswith("task unread ") and rows["repo-unread"]["needs"] == "worker"
    assert rows["repo-lost"]["waiting"].endswith(", doorbell failed") and rows["repo-lost"]["needs"] == "root"
    assert rows["repo-busy"]["waiting"].startswith("on task ") and rows["repo-busy"]["needs"] == "worker"
    assert (rows["repo-idle"]["waiting"], rows["repo-idle"]["needs"]) == ("no task", None)
    assert (rows["repo-plan"]["report"], rows["repo-plan"]["needs"]) == ("plan", "root")
    assert set(data["needs_root"]) == {"repo-plan", "repo-ask", "repo-lost"}
    names = [r["name"] for r in data["agents"]]
    assert names[:3] == ["repo-plan", "repo-ask", "repo-lost"]  # in the order they started waiting
    assert names[-1] == "repo-lead"  # the root's own row last
    for key in ("name", "role", "kind", "model", "size", "view", "state", "unread", "parent", "branch"):
        assert key in rows["repo-plan"]  # what roots read before stays
    assert mesh.main(["status", run]) == 0
    out = capsys.readouterr().out
    assert "WAITING" in out and "PARENT" not in out
    assert out.index("repo-plan") < out.index("repo-busy") < out.index("repo-idle")


def test_status_shows_a_declared_wait(mesh, fake, run, capsys):
    fake.on("agent", "get", result={"agent": {"agent_status": "idle"}})
    mesh.mark_read(mesh.record(run, "repo-lead", "repo-fix-typo", "task", "Fix it"))
    until = mesh.datetime.now(mesh.timezone.utc) + mesh.timedelta(minutes=30)
    mesh.log_event(run, "send", sender="repo-fix-typo", to="repo-lead", type="wait", file="w.md",
                   until=until.isoformat(timespec="seconds"))
    _, rows = status_rows(mesh, run, capsys)
    row = rows["repo-fix-typo"]
    assert row["waiting"].endswith(f", waiting until {until.astimezone():%H:%M}") and row["needs"] == "worker"
    mesh.log_event(run, "wait_over", name="repo-fix-typo", file="w.md")  # it woke and stopped again
    assert "waiting until" not in status_rows(mesh, run, capsys)[1]["repo-fix-typo"]["waiting"]


def test_status_shows_a_worker_held_at_a_startup_screen_until_it_is_past_it(mesh, fake, run, capsys):
    fake.on("agent", "get", result={"agent": {"agent_status": "blocked"}})
    held_worker(mesh, run)
    at = mesh.load_run(run)["agents"]["repo-fix-typo"]["held"]["at"]
    data, rows = status_rows(mesh, run, capsys)
    row = rows["repo-fix-typo"]
    assert row["waiting"].startswith("held at a startup screen (Settings Warning) since "
                                     f"{mesh.datetime.fromisoformat(at).astimezone():%H:%M} (")
    assert row["waiting"].endswith(", task stored") and row["needs"] == "root" and row["since"] == at
    assert data["needs_root"] == ["repo-fix-typo"]
    fake.on("agent", "get", result={"agent": {"agent_status": "working"}})  # the user cleared it and prompted it
    _, rows = status_rows(mesh, run, capsys)
    assert rows["repo-fix-typo"]["waiting"].startswith("task unread ") and rows["repo-fix-typo"]["needs"] == "worker"
    fake.on("agent", "get", result={"agent": {"agent_status": "idle"}})  # past it, and no watch rang it
    _, rows = status_rows(mesh, run, capsys)
    assert rows["repo-fix-typo"]["waiting"].startswith("task unread ") and rows["repo-fix-typo"]["needs"] == "root"
    assert rows["repo-fix-typo"]["waiting"].endswith(", its first doorbell never rang: `mesh ring repo-fix-typo`")
    fake.on("agent", "get", result={"agent": {"agent_status": "blocked"}})
    with mesh.editing_run(run) as data:  # its doorbell rang (`release_held`, `mesh ring`): the hold is gone
        data["agents"]["repo-fix-typo"].pop("held")
    _, rows = status_rows(mesh, run, capsys)
    assert not rows["repo-fix-typo"]["waiting"].startswith("held")


def test_status_shows_a_hold_with_no_task_and_an_unknown_state(mesh, fake, run, capsys):
    fake.on("agent", "get", result={"agent": {"agent_status": "unknown"}})  # herdr lost it, the pane lives
    with mesh.editing_run(run) as data:  # spawned with no task
        data["agents"]["repo-fix-typo"]["held"] = {"sender": None, "type": None, "file": None,
                                                   "screen": "Settings Warning", "at": mesh.now_iso()}
    _, rows = status_rows(mesh, run, capsys)
    row = rows["repo-fix-typo"]
    assert row["waiting"].startswith("held at a startup screen (Settings Warning) since ") and row["needs"] == "root"
    assert "stored" not in row["waiting"]
    fake.on("agent", "get", result={"agent": {"agent_status": "idle"}})  # nothing to ring
    _, rows = status_rows(mesh, run, capsys)
    assert (rows["repo-fix-typo"]["waiting"], rows["repo-fix-typo"]["needs"]) == ("no task", None)


def test_status_ignores_a_stale_hold_the_worker_got_past(mesh, fake, run, capsys):
    fake.on("agent", "get", result={"agent": {"agent_status": "blocked"}})  # e.g. a permission prompt later on
    path = held_worker(mesh, run)
    mesh.mark_read(path)  # it read its task: past the screen, though no ring dropped the hold
    _, rows = status_rows(mesh, run, capsys)
    assert rows["repo-fix-typo"]["waiting"].startswith("on task ")
    mesh.record(run, "repo-fix-typo", "repo-lead", "plan", "My plan")
    _, rows = status_rows(mesh, run, capsys)
    row = rows["repo-fix-typo"]
    assert row["waiting"].startswith("plan unanswered ") and row["waiting"].endswith(", unread")
    assert row["report"] == "plan" and row["needs"] == "root"


def test_status_moves_a_worker_that_owes_a_report_but_is_gone_to_the_root(mesh, fake, run, capsys):
    fake.on("agent", "get", errors=["agent_not_found"] * 4)
    fake.on("agent", "get", "w1:p1", result={})  # the root's own pane, read for its session: herdr names none
    mesh.mark_read(mesh.record(run, "repo-lead", "repo-fix-typo", "task", "Fix it"))
    _, rows = status_rows(mesh, run, capsys)
    assert rows["repo-fix-typo"]["waiting"].startswith("on task ") and rows["repo-fix-typo"]["needs"] == "root"
    with mesh.editing_run(run) as data:
        data["agents"]["repo-fix-typo"]["status"] = "retired"
    _, rows = status_rows(mesh, run, capsys)
    assert (rows["repo-fix-typo"]["waiting"], rows["repo-fix-typo"]["needs"]) == ("retired before reporting", None)


def test_status_shows_a_done_with_its_pr_state_and_reviewers(mesh, fake, cli, run, capsys):
    fake.on("agent", "get", result={"agent": {"agent_status": "idle"}})
    pr = "https://github.com/acme/proj/pull/7"
    cli.on("pr", "view", stdout=json.dumps({"state": "OPEN", "isDraft": False, "statusCheckRollup": [
        {"status": "COMPLETED", "conclusion": "SUCCESS"}, {"status": "IN_PROGRESS", "conclusion": ""}]}))
    mesh.mark_read(mesh.record(run, "repo-lead", "repo-fix-typo", "task", "Fix it"))
    mesh.record(run, "repo-fix-typo", "repo-lead", "done", f"Fixed: {pr}")
    add_worker(mesh, run, "proj-review", reviews=[pr])
    mesh.mark_read(mesh.record(run, "repo-lead", "proj-review", "task", f"Review (read-only) of {pr}: review it."))
    data, rows = status_rows(mesh, run, capsys)
    done = rows["repo-fix-typo"]
    assert done["prs"] == [{"url": pr, "number": 7, "state": "open", "draft": False, "checks": "pending"}]
    assert done["waiting"].endswith(": PR #7 open, checks pending, in review (proj-review)")
    assert done["needs"] == "worker"  # its reviewers have the ball
    assert len(cli.called("pr", "view")) == 1
    mesh.record(run, "proj-review", "repo-lead", "done", f"No findings on {pr}.")
    data, rows = status_rows(mesh, run, capsys)
    assert len(cli.called("pr", "view")) == 1  # cached
    assert rows["repo-fix-typo"]["needs"] == "root" and "in review" not in rows["repo-fix-typo"]["waiting"]
    review = rows["proj-review"]
    assert review["waiting"].startswith("answered ") and review["needs"] == "root" and "prs" not in review
    with mesh.editing_run(run) as data:
        data["agents"]["repo-fix-typo"]["status"] = "retired"
    _, rows = status_rows(mesh, run, capsys)
    assert rows["repo-fix-typo"]["needs"] is None and "PR #7 open" in rows["repo-fix-typo"]["waiting"]


def test_a_pr_state_gh_cannot_read_is_unknown_or_the_last_one_seen(mesh, cli, run, monkeypatch):
    pr = "https://github.com/acme/proj/pull/7"
    (mesh.run_dir(run) / "pr-state.json").write_text("[]")  # not an object: read as empty
    cli.on("pr", "view", returncode=1, stderr="gh: not logged in")
    assert mesh.pr_states(run, [pr])[pr]["state"] == "unknown"
    assert mesh.pr_states(run, [pr])[pr]["state"] == "unknown"
    assert len(cli.called("pr", "view")) == 1  # a failure is cached too, for PR_RETRY_TTL
    assert cli.called("pr", "view")[0]["argv"][-1] == "state,isDraft,statusCheckRollup"
    monkeypatch.setattr(mesh, "PR_RETRY_TTL", -1)
    cli.on("pr", "view", stdout=json.dumps({"state": "MERGED", "isDraft": False, "statusCheckRollup": [
        {"__typename": "StatusContext", "state": "FAILURE"}]}))
    assert mesh.pr_states(run, [pr])[pr] == {"state": "merged", "draft": False, "checks": "failing"}
    monkeypatch.setattr(mesh, "PR_STATE_TTL", -1)
    cli.on("pr", "view", returncode=1)
    assert mesh.pr_states(run, [pr])[pr]["state"] == "merged"


def test_in_review_counts_only_the_pr_a_reviewer_reviews_now(mesh, fake, cli, run, capsys):
    fake.on("agent", "get", result={"agent": {"agent_status": "idle"}})
    old, new = "https://github.com/acme/proj/pull/7", "https://github.com/acme/proj/pull/8"
    cli.on("pr", "view", stdout=json.dumps({"state": "OPEN", "isDraft": False, "statusCheckRollup": []}))
    mesh.mark_read(mesh.record(run, "repo-lead", "repo-fix-typo", "task", "Fix it"))
    mesh.record(run, "repo-fix-typo", "repo-lead", "done", f"Fixed: {old}")
    add_worker(mesh, run, "proj-review", reviews=[old, new])  # reviewed #7 before, reviews #8 now
    mesh.mark_read(mesh.record(run, "repo-lead", "proj-review", "task", f"Review (read-only) of {new}: review it."))
    _, rows = status_rows(mesh, run, capsys)
    assert rows["repo-fix-typo"]["needs"] == "root" and "in review" not in rows["repo-fix-typo"]["waiting"]
    mesh.mark_read(mesh.record(run, "repo-lead", "proj-review", "task", "Question (read-only): how does X work?"))
    _, rows = status_rows(mesh, run, capsys)
    assert rows["repo-fix-typo"]["needs"] == "root"  # a follow-up is about #8, the review it follows


def test_in_review_reads_the_pr_from_a_task_that_names_its_head(mesh, fake, cli, run, capsys):
    # Every review task names its head ("… of <pr> at <sha>:"); a reviewer of two PRs asked about the first.
    fake.on("agent", "get", result={"agent": {"agent_status": "idle"}})
    first, second = "https://github.com/acme/proj/pull/7", "https://github.com/acme/proj/pull/8"
    cli.on("pr", "view", stdout=json.dumps({"state": "OPEN", "isDraft": False, "statusCheckRollup": []}))
    mesh.mark_read(mesh.record(run, "repo-lead", "repo-fix-typo", "task", "Fix it"))
    mesh.record(run, "repo-fix-typo", "repo-lead", "done", f"Fixed: {first}")
    add_worker(mesh, run, "proj-review", reviews=[first, second])
    mesh.mark_read(mesh.record(run, "repo-lead", "proj-review", "task",
                               f"Review (read-only) of {first} at 0123abc: review this pull request."))
    _, rows = status_rows(mesh, run, capsys)
    assert rows["repo-fix-typo"]["waiting"].endswith("in review (proj-review)")
    assert mesh.REVIEW_GIST_RE.match(f"Review (read-only) of {first} at 0123abc: x").group(1) == (
        f"Review (read-only) of {first} at 0123abc")  # the log's gist keeps the head


def test_a_follow_up_question_to_a_reviewer_keeps_the_pr_in_review(mesh, fake, cli, run, capsys):
    fake.on("agent", "get", result={"agent": {"agent_status": "idle"}})
    pr = "https://github.com/acme/proj/pull/7"
    cli.on("pr", "view", stdout=json.dumps({"state": "OPEN", "isDraft": False, "statusCheckRollup": []}))
    mesh.mark_read(mesh.record(run, "repo-lead", "repo-fix-typo", "task", "Fix it"))
    mesh.record(run, "repo-fix-typo", "repo-lead", "done", f"Fixed: {pr}")
    add_worker(mesh, run, "proj-review", reviews=[pr])
    mesh.mark_read(mesh.record(run, "repo-lead", "proj-review", "task", f"Review (read-only) of {pr}: review it."))
    mesh.record(run, "proj-review", "repo-lead", "done", "One finding.")
    _, rows = status_rows(mesh, run, capsys)
    assert "in review" not in rows["repo-fix-typo"]["waiting"]
    for question in ("Why is the finding blocking?", "And the other file?"):  # each follow-up, without --review
        mesh.mark_read(mesh.record(run, "repo-lead", "proj-review", "task", f"Question (read-only): {question}"))
        _, rows = status_rows(mesh, run, capsys)
        assert rows["repo-fix-typo"]["waiting"].endswith("in review (proj-review)")
        assert rows["repo-fix-typo"]["needs"] == "worker"
        mesh.record(run, "proj-review", "repo-lead", "done", "Because …")
        _, rows = status_rows(mesh, run, capsys)
        assert "in review" not in rows["repo-fix-typo"]["waiting"]
    assert mesh.load_run(run)["agents"]["proj-review"]["reviews"] == [pr]  # nothing new recorded
    # Turned into a change worker, a later question is about that change, not the old review.
    mesh.mark_read(mesh.record(run, "repo-lead", "proj-review", "task", "Fix the finding."))
    mesh.record(run, "proj-review", "repo-lead", "done", "Fixed.")
    mesh.mark_read(mesh.record(run, "repo-lead", "proj-review", "task", "Question (read-only): why so?"))
    _, rows = status_rows(mesh, run, capsys)
    assert "in review" not in rows["repo-fix-typo"]["waiting"]


def test_a_done_without_a_link_takes_only_an_earlier_dones_prs(mesh, fake, cli, run, capsys):
    fake.on("agent", "get", result={"agent": {"agent_status": "idle"}})
    plan_pr, done_pr = "https://github.com/acme/proj/pull/3", "https://github.com/acme/proj/pull/7"
    cli.on("pr", "view", stdout=json.dumps({"state": "OPEN", "isDraft": False, "statusCheckRollup": []}))
    mesh.mark_read(mesh.record(run, "repo-lead", "repo-fix-typo", "task", "Fix it"))
    mesh.record(run, "repo-fix-typo", "repo-lead", "plan", f"Like {plan_pr}, I will…")
    mesh.mark_read(mesh.record(run, "repo-lead", "repo-fix-typo", "answer", "approved"))
    mesh.record(run, "repo-fix-typo", "repo-lead", "done", f"Opened {done_pr}")
    mesh.mark_read(mesh.record(run, "repo-lead", "repo-fix-typo", "task", "Fix the review findings"))
    mesh.record(run, "repo-fix-typo", "repo-lead", "done", "Fixed both findings and pushed.")
    _, rows = status_rows(mesh, run, capsys)
    assert [p["url"] for p in rows["repo-fix-typo"]["prs"]] == [done_pr]


def test_a_doorbell_that_rang_later_is_no_longer_failed(mesh, fake, run, capsys):
    fake.on("agent", "get", result={"agent": {"agent_status": "idle"}})
    task = mesh.record(run, "repo-lead", "repo-fix-typo", "task", "Fix it")
    mesh.log_event(run, "doorbell_failed", sender="repo-lead", to="repo-fix-typo", file=task.name, error="timeout")
    _, rows = status_rows(mesh, run, capsys)
    assert rows["repo-fix-typo"]["needs"] == "root"
    mesh.log_event(run, "rung", sender="repo-lead", to="repo-fix-typo", via="pane")  # `mesh ring` worked
    _, rows = status_rows(mesh, run, capsys)
    row = rows["repo-fix-typo"]
    assert row["waiting"].startswith("task unread ") and "doorbell" not in row["waiting"] and row["needs"] == "worker"


def test_status_says_a_done_changed_nothing(mesh, fake, run, repo, capsys):
    fake.on("agent", "get", result={"agent": {"agent_status": "idle"}})
    path, branch, base = mesh.add_worktree(run, repo, "noop", None)
    add_worker(mesh, run, "proj-noop", repo=str(repo), cwd=str(path), branch=branch, base=base)
    mesh.mark_read(mesh.record(run, "repo-lead", "proj-noop", "task", "Fix the bug"))
    mesh.mark_read(mesh.record(run, "proj-noop", "repo-lead", "done", "Not a bug after all."))
    _, rows = status_rows(mesh, run, capsys)
    row = rows["proj-noop"]
    assert row["waiting"].endswith(": no commits, no PR") and row["needs"] == "root"
    assert (row["commits"], row["pushed"], row["dirty"]) == (0, False, False)
    subprocess.run(["git", "commit", "-q", "--allow-empty", "-m", "work"], cwd=path, check=True)
    (path / "wip.txt").write_text("more")
    _, rows = status_rows(mesh, run, capsys)
    assert rows["proj-noop"]["waiting"].endswith(": 1 commit(s), not pushed, no PR, worktree dirty")


def test_log_prints_a_timeline(mesh, fake, run, capsys):
    mesh.record(run, "repo-lead", "repo-fix-typo", "task", "Direct task (no plan): research…\n\n# Fix the typo in README")
    mesh.log_event(run, "nudge", name="repo-fix-typo", received=1)
    mesh.log_event(run, "swap", name="other", model="opus", effort="medium", why="approved", bg_id=None)
    capsys.readouterr()
    assert mesh.main(["log", run]) == 0
    out = capsys.readouterr().out.splitlines()
    assert out[0].startswith("── ") and any("init" in line for line in out)
    send = next(line for line in out if " send " in line)
    assert "repo-lead → repo-fix-typo  task  Fix the typo in README" in send
    nudge = next(line for line in out if " nudge " in line)
    assert nudge.endswith("repo-fix-typo  received=1")
    assert "bg_id" not in next(line for line in out if " swap " in line)
    assert mesh.main(["log", run, "--agent", "repo-fix-typo"]) == 0
    out = capsys.readouterr().out
    assert "nudge" in out and "swap" not in out
    assert mesh.main(["log", run, "--lines", "1"]) == 0
    [day, last] = capsys.readouterr().out.splitlines()
    assert day.startswith("── ") and " swap " in last


def test_log_follow_prints_new_events_until_the_run_stops(mesh, fake, run, monkeypatch, capsys):
    path = mesh.run_dir(run) / "log.jsonl"
    polls = iter(range(10))

    def sleep(_):
        n = next(polls)
        if n == 0:
            with open(path, "a") as fh:
                fh.write('{"time": "2026-10-25T10:00:00+00:00", "event": "retire", "na')  # still being written
        elif n == 1:
            with open(path, "a") as fh:
                fh.write('me": "repo-fix-typo"}\n')
        elif n == 2:
            mesh.log_event(run, "stop", closed=[], kept=[])

    monkeypatch.setattr(mesh.time, "sleep", sleep)
    capsys.readouterr()
    assert mesh.main(["log", run, "--follow"]) == 0
    out, err = capsys.readouterr()
    assert "retire" in out and "repo-fix-typo" in out and out.rstrip().splitlines()[-1].split()[1] == "stop"
    assert "unreadable" not in err  # a line being written is waited for, not torn


def test_log_reads_an_archived_run(mesh, fake, run, capsys):
    archive = mesh.mesh_home() / "archive"
    archive.mkdir(parents=True)
    os.replace(mesh.run_dir(run), archive / run)
    capsys.readouterr()
    assert mesh.main(["log", run]) == 0
    assert "init" in capsys.readouterr().out
    assert mesh.main(["log", run, "--agent", "repo-lead", "--follow"]) == 0  # archived: nothing more to follow
    assert mesh.main(["log", "nope"]) == 1
    assert mesh.main(["log", run, "--lines", "-1"]) == 1


def test_log_follow_for_one_agent_ends_on_the_stop_that_names_nobody(mesh, fake, run, capsys):
    mesh.log_event(run, "stop", closed=[], kept=[])
    capsys.readouterr()
    assert mesh.main(["log", run, "--agent", "repo-fix-typo", "--follow", "--lines", "1"]) == 0
    assert " stop " not in capsys.readouterr().out


def stub_new_pane(fake):
    fake.on("workspace", "list", result={"workspaces": [{"workspace_id": "w9", "label": "proj"}]})
    fake.on("tab", "create", result={"tab": {"tab_id": "w9:t4"}, "root_pane": {"pane_id": "w9:p10"}})


def test_resume_brings_the_worker_back_with_its_conversation(mesh, fake, run, repo, capsys):
    path, branch = archived_worker(mesh, repo, session="c1")
    stub_new_pane(fake)
    capsys.readouterr()
    assert mesh.main(["resume", "https://github.com/acme/proj/pull/7", "--task", "Apply the review."]) == 0
    out = json.loads(capsys.readouterr().out)
    assert (out["name"], out["worktree_recreated"], out["resumed_from"]) == ("proj-fix", True, {"run": "mold", "name": "proj-fix"})
    # the worktree is back at the same path (Claude finds sessions by path) on the same branch
    head = subprocess.run(["git", "branch", "--show-current"], cwd=path, capture_output=True, text=True).stdout.strip()
    assert head == branch
    start = fake.called("agent", "start")[-1]
    # It plans again, where a spawn on `auto` would: Argon is out (USAGE), so on Opus.
    assert start[-5:] == ("--conversation", "c1", "--model", "claude-opus-5-5-high", "--dangerously-skip-permissions")
    data = mesh.load_run(run)
    entry = data["agents"]["proj-fix"]
    assert (entry["parent"], entry["session"], entry["status"]) == ("repo-lead", "c1", "running")
    assert {"path": str(path), "repo": str(repo), "branch": branch} in data["worktrees"]  # this run's stop cleans it
    assert "MESH_SELF=proj-fix" in fake.called("tab", "create")[0]
    doorbell = fake.called("agent", "prompt")[-1][3]
    assert doorbell.startswith("📬 [mesh] You are proj-fix (worker) again, resumed into agent-mesh run mtest")
    assert "background tasks from before (`task-…` ids) are gone" in doorbell  # they died with the old process
    assert mesh.unread(run, "proj-fix")


def github_origin(tmp_path, repo, name="origin", set_head=True):
    """`repo` with a remote `name` that passes for GitHub's acme/proj (a clone at a `…/github.com/acme/proj` path),
    serving PR 7's head, a commit off main, at refs/pull/7/head. `<name>/HEAD` is set unless `set_head` is off."""
    origin = tmp_path / "gh" / "github.com" / "acme" / "proj"
    origin.parent.mkdir(parents=True)
    subprocess.run(["git", "clone", "-q", str(repo), str(origin)], check=True)
    run = lambda *a, cwd=origin: subprocess.run(["git", *a], cwd=cwd, check=True, capture_output=True, text=True).stdout.strip()
    run("config", "user.email", "t@t")
    run("config", "user.name", "t")
    run("update-ref", "refs/pull/7/head", run("commit-tree", "HEAD^{tree}", "-p", "HEAD", "-m", "PR 7's work"))
    run("remote", "add", name, str(origin), cwd=repo)
    run("fetch", "-q", name, cwd=repo)
    if not set_head:  # as before git 2.48, whose fetch sets <remote>/HEAD
        run("config", f"remote.{name}.followRemoteHEAD", "never", cwd=repo)
    subprocess.run(["git", "remote", "set-head", name, *(["main"] if set_head else ["-d"])], cwd=repo,
                   capture_output=True)  # -d fails where there's nothing to delete, which is fine
    return origin


@pytest.mark.parametrize("remote, set_head", [("origin", True), ("upstream", False)])
def test_resume_onto_a_merged_pr_starts_a_new_branch_from_the_default_one(mesh, fake, cli, run, repo, tmp_path,
                                                                           capsys, remote, set_head):
    origin = github_origin(tmp_path, repo, remote, set_head)  # upstream: the PR's remote isn't origin
    cli.on("repo", "view", stdout='{"defaultBranchRef": {"name": "main"}}')  # what <remote>/HEAD doesn't say
    path, branch = archived_worker(mesh, repo, session="c1")
    subprocess.run(["git", "commit", "-q", "--allow-empty", "-m", "squash-merged PR 7"], cwd=origin, check=True)
    merged = subprocess.run(["git", "rev-parse", "HEAD"], cwd=origin, capture_output=True, text=True).stdout.strip()
    pr = "https://github.com/acme/proj/pull/7"
    cli.on("pr", "list", stdout=json.dumps([{"number": 7, "url": pr, "state": "MERGED"}]))
    stub_new_pane(fake)
    capsys.readouterr()
    assert mesh.main(["resume", "proj-fix", "--task", "Direct task (no plan)\n\nAdd the follow-up."]) == 0
    out = json.loads(capsys.readouterr().out)
    assert (out["branch"], out["left_pr"]) == ("mesh/mtest/fix", pr)
    assert f"PR #7 ({pr}) from `{branch}` is merged" in out["notes"][0]
    [call] = cli.called("pr", "list")
    assert call["cwd"] == str(repo) and call["argv"][3:5] == ["--head", branch]
    git_out = lambda *a: subprocess.run(["git", *a], cwd=path, capture_output=True, text=True).stdout.strip()
    assert (git_out("branch", "--show-current"), git_out("rev-parse", "HEAD")) == ("mesh/mtest/fix", merged)
    assert git_out("rev-parse", "--verify", branch)  # the old branch is kept
    data = mesh.load_run(run)
    assert (data["agents"]["proj-fix"]["branch"], data["agents"]["proj-fix"]["base"]) == ("mesh/mtest/fix", merged)
    assert {"path": str(path), "repo": str(repo), "branch": "mesh/mtest/fix", "base": merged} in data["worktrees"]
    task = mesh.parse_message(mesh.unread(run, "proj-fix")[0])[1]
    assert task.startswith("Direct task (no plan)")  # the note goes last, so the task keeps its kind
    assert f"now on a new branch, `mesh/mtest/fix`, started from `{remote}/main`" in task.rstrip().splitlines()[-1]
    assert bool(cli.called("repo", "view", "acme/proj")) == (not set_head)


def test_resume_keeps_the_branch_for_an_open_pr_a_question_or_a_failing_gh(mesh, fake, cli, run, repo, tmp_path,
                                                                            capsys):
    github_origin(tmp_path, repo)
    path, branch = archived_worker(mesh, repo, session="c1")
    stub_new_pane(fake)
    cases = [
        ("Fix it.", json.dumps([{"number": 7, "url": "u7", "state": "MERGED"}, {"number": 8, "url": "u8", "state": "OPEN"}]), 0, None),
        ("Question (read-only)\n\nWhy?", "[]", 0, None),
        ("Direct task (no plan)\n\nFix it.", "", 1, "couldn't check whether mesh/mold/fix has a merged or closed PR (gh: gh: not logged in)"),
    ]
    for n, (task, stdout, code, note) in enumerate(cases):
        cli.calls.clear()
        cli.on("pr", "list", stdout=stdout, returncode=code, stderr="gh: not logged in" if code else "")
        if n:
            mesh.main(["retire", out["name"]])
        capsys.readouterr()
        assert mesh.main(["resume", "proj-fix", "--from-run", "mold", "--task", task]) == 0
        out = json.loads(capsys.readouterr().out)
        assert out["branch"] == branch and "left_pr" not in out
        assert bool(cli.called("pr", "list")) == (not task.startswith("Question"))
        assert (note in out["notes"][-1]) if note else not any("PR" in n for n in out["notes"])


def test_resume_onto_a_closed_pr_starts_a_new_branch_at_the_old_tip(mesh, fake, cli, run, repo, tmp_path, capsys):
    github_origin(tmp_path, repo)
    path, branch = archived_worker(mesh, repo, session="c1")
    mesh.ensure_worktree({"cwd": str(path), "repo": str(repo)}, branch)
    subprocess.run(["git", "commit", "-q", "--allow-empty", "-m", "the closed PR's work"], cwd=path, check=True)
    tip = subprocess.run(["git", "rev-parse", "HEAD"], cwd=path, capture_output=True, text=True).stdout.strip()
    (path / "wip.txt").write_text("unfinished")  # a new branch at the same commit keeps it
    cli.on("pr", "list", stdout=json.dumps([{"number": 7, "url": "u7", "state": "CLOSED", "headRefOid": tip}]))
    stub_new_pane(fake)
    capsys.readouterr()
    assert mesh.main(["resume", "proj-fix", "--task", "Fix it."]) == 0
    out = json.loads(capsys.readouterr().out)
    assert out["branch"] == "mesh/mtest/fix" and "its commits come along" in out["notes"][-1]
    git_out = lambda *a: subprocess.run(["git", *a], cwd=path, capture_output=True, text=True).stdout.strip()
    assert (git_out("branch", "--show-current"), git_out("rev-parse", "HEAD")) == ("mesh/mtest/fix", tip)
    assert (path / "wip.txt").exists()
    task = mesh.parse_message(mesh.unread(run, "proj-fix")[0])[1]
    assert "started from the tip of `mesh/mold/fix`, so its commits come along" in task


@pytest.mark.parametrize("fetchable", [True, False])
def test_a_closed_prs_head_is_fetched_when_its_branch_lost_it(mesh, fake, cli, run, repo, tmp_path, capsys,
                                                               fetchable):
    origin = github_origin(tmp_path, repo)
    path, branch = archived_worker(mesh, repo, session="c1")  # the branch was recreated at its base: no PR commits
    head = subprocess.run(["git", "rev-parse", "refs/pull/7/head"], cwd=origin, capture_output=True,
                          text=True).stdout.strip()
    oid = head if fetchable else "f" * 40
    cli.on("pr", "list", stdout=json.dumps([{"number": 7, "url": "https://github.com/acme/proj/pull/7",
                                              "state": "CLOSED", "headRefOid": oid}]))
    stub_new_pane(fake)
    capsys.readouterr()
    assert mesh.main(["resume", "proj-fix", "--task", "Fix it."]) == 0
    out = json.loads(capsys.readouterr().out)
    tip = subprocess.run(["git", "rev-parse", "HEAD"], cwd=path, capture_output=True, text=True).stdout.strip()
    task = mesh.parse_message(mesh.unread(run, "proj-fix")[0])[1]
    assert out["branch"] == "mesh/mtest/fix"
    if fetchable:
        assert tip == head and f"the closed PR's head ({head[:12]}, fetched from GitHub)" in task
    else:
        base = subprocess.run(["git", "rev-parse", branch], cwd=repo, capture_output=True, text=True).stdout.strip()
        assert tip == base and "the closed PR's commits aren't here" in out["notes"][-1]
        assert "couldn't be fetched" in task and "commits come along" not in task


def test_resume_onto_a_merged_pr_with_uncommitted_work_tells_the_worker(mesh, fake, cli, run, repo, tmp_path,
                                                                         capsys):
    github_origin(tmp_path, repo)
    path, branch = archived_worker(mesh, repo, session="c1")
    mesh.ensure_worktree({"cwd": str(path), "repo": str(repo)}, branch)
    (path / "wip.txt").write_text("unfinished")
    cli.on("pr", "list", stdout=json.dumps([{"number": 7, "url": "u7", "state": "MERGED"}]))
    stub_new_pane(fake)
    capsys.readouterr()
    assert mesh.main(["resume", "proj-fix", "--task", "Fix it."]) == 0
    out = json.loads(capsys.readouterr().out)
    assert out["branch"] == branch and "the worktree has uncommitted changes, so it stays" in out["notes"][-1]
    head = subprocess.run(["git", "branch", "--show-current"], cwd=path, capture_output=True, text=True).stdout.strip()
    assert head == branch
    task = mesh.parse_message(mesh.unread(run, "proj-fix")[0])[1]
    assert "is merged. Don't push to that branch: start a new one from the repo's default branch" in task


def test_a_finished_pr_counts_only_from_this_branch_and_only_with_a_task(mesh, fake, cli, run, repo, tmp_path):
    github_origin(tmp_path, repo)
    _, branch = archived_worker(mesh, repo, session="c1")
    subprocess.run(["git", "switch", "-q", "-c", "elsewhere"], cwd=repo, check=True)
    subprocess.run(["git", "commit", "-q", "--allow-empty", "-m", "an older PR's head"], cwd=repo, check=True)
    old_head = subprocess.run(["git", "rev-parse", "HEAD"], cwd=repo, capture_output=True, text=True).stdout.strip()
    subprocess.run(["git", "switch", "-q", "main"], cwd=repo, check=True)
    cli.on("pr", "list", stdout=json.dumps([{"number": 3, "url": "u3", "state": "MERGED", "headRefOid": old_head}]))
    assert mesh.finished_pr(repo, branch) == (None, None)  # a reused branch name: that PR's head isn't in it
    cli.on("pr", "list", stdout=json.dumps([{"number": 3, "url": "u3", "state": "MERGED", "headRefOid": "f" * 40}]))
    assert mesh.finished_pr(repo, branch)[0]["number"] == 3  # never fetched: it still counts
    # a PR whose head is in the commit the worker started from isn't its own, even once merged into main
    subprocess.run(["git", "branch", "-f", branch, old_head], cwd=repo, check=True)
    cli.on("pr", "list", stdout=json.dumps([{"number": 3, "url": "u3", "state": "MERGED", "headRefOid": old_head}]))
    assert mesh.finished_pr(repo, branch)[0]["number"] == 3
    assert mesh.finished_pr(repo, branch, base=old_head) == (None, None)
    cli.calls.clear()
    entry = {"repo": str(repo), "cwd": str(repo), "branch": branch}
    ns = mesh.argparse.Namespace()
    assert mesh.leave_finished_pr(ns, run, entry, branch, None) == {}
    assert mesh.leave_finished_pr(ns, run, entry, branch, "\n  Question (read-only)\n\nWhy?") == {}
    assert not cli.called("pr", "list")  # without a task the worker would never hear of a new branch
    # a resumed branch's suffix is renumbered, not stacked
    path, again, _ = mesh.add_worktree(run, repo, "fix-2", None)
    cli.on("pr", "list", stdout=json.dumps([{"number": 9, "url": "u9", "state": "CLOSED"}]))
    moved = mesh.leave_finished_pr(ns, run, {"repo": str(repo), "cwd": str(path)}, again, "Fix it.")
    assert (again, moved["branch"]) == ("mesh/mtest/fix-2", "mesh/mtest/fix")


def test_a_head_branch_match_needs_the_prs_repo_and_skips_only_live_reviewers(mesh, run, repo):
    pr = "https://github.com/acme/proj/pull/9"
    entry = {"repo": str(repo), "branch": "feat/x", "reviews": [pr]}
    assert mesh.github_repos(repo) is None
    assert mesh.opened_pr(pr, "feat/x", {}, entry)  # no remotes to tell by; it reviewed its own PR
    subprocess.run(["git", "remote", "add", "origin", "https://gitlab.com/acme/proj.git"], cwd=repo, check=True)
    assert mesh.github_repos(repo) == set()
    assert not mesh.opened_pr(pr, "feat/x", {}, entry)  # a GitLab repo opened no GitHub PR
    subprocess.run(["git", "remote", "add", "github", "git@github.com:Acme/Proj.git"], cwd=repo, check=True)
    assert mesh.opened_pr(pr, "feat/x", {}, entry)


def test_a_resumed_worker_given_a_planned_task_plans_on_the_plan_chain_then_moves_on(mesh, fake, run, repo,
                                                                                     monkeypatch, capsys):
    with_quotas(mesh, monkeypatch)
    archived_worker(mesh, repo, session="c1")  # it ran on gemini-3.8-flash-high
    stub_new_pane(fake)
    assert mesh.main(["resume", "proj-fix", "--task", "Fix the review findings; send a plan first."]) == 0
    entry = mesh.load_run(run)["agents"]["proj-fix"]
    assert (entry["model"], entry["effort"]) == ("claude-opus-5-5-high", "high")  # Argon is out of quota; not Flash
    assert fake.called("agent", "start")[-1][-3:] == ("--model", "claude-opus-5-5-high", "--dangerously-skip-permissions")
    assert (entry["phase"], entry["exec_model"], entry["exec_effort"]) == ("plan", "flash", "medium")
    fake.on("pane", "layout", result={"layout": {"panes": [{"pane_id": "w9:p10", "rect": {"width": 200, "height": 50}}]}})
    fake.on("pane", "split", result={"pane": {"pane_id": "w9:p11"}})
    sent_plan(mesh, run, "proj-fix")
    capsys.readouterr()
    assert mesh.main(["approve", "proj-fix"]) == 0
    assert json.loads(capsys.readouterr().out)["model"] == "gemini-3.8-flash-medium"
    assert fake.called("agent", "start")[-1][-5:] == (
        "--conversation", "c1", "--model", "gemini-3.8-flash-medium", "--dangerously-skip-permissions")

    # a direct task, a question or a named effort has no plan phase
    for extra in (["--task", "Direct task (no plan)\n\nBump it."], ["--task", "Plan it.", "--effort", "high"]):
        mesh.main(["retire", "proj-fix"])
        assert mesh.main(["resume", "proj-fix", "--from-run", "mold", *extra]) == 0
    later = [e for n, e in mesh.load_run(run)["agents"].items() if n.startswith("proj-fix-")]
    assert len(later) == 2 and all(e["phase"] is None and e["exec_model"] is None for e in later)
    assert [e["model"] for e in later] == ["gemini-3.8-flash-high"] * 2  # they keep the model they had


def test_resume_of_a_claude_worker_whose_name_is_taken(mesh, fake, run, repo, capsys):
    archived_worker(mesh, repo, kind="claude", session="s-1")
    with mesh.editing_run(run) as data:
        data["agents"]["proj-fix"] = {"role": "worker", "kind": "agy", "cwd": "/elsewhere", "pane": "w2:p9"}
    stub_new_pane(fake)
    capsys.readouterr()
    assert mesh.main(["resume", "mesh/mold/fix"]) == 0
    assert json.loads(capsys.readouterr().out)["name"] == "proj-fix-2"
    assert started(fake)[-7:] == (
        "--resume", "s-1", "--model", "opus", "--effort", "high", "--dangerously-skip-permissions")
    assert "--settings" in fake.called("agent", "start")[-1]


def test_resume_refuses_a_worker_that_is_still_running(mesh, fake, run, repo, capsys):
    path, _ = archived_worker(mesh, repo, session="c1")
    fake.on("agent", "list", result={"agents": [{"pane_id": "w5:p2", "cwd": str(path / "src")}]})
    assert mesh.main(["resume", "proj-fix", "--from-run", "mold"]) == 1
    assert "still running in pane w5:p2" in capsys.readouterr().err
    assert not fake.called("agent", "start") and not path.exists()

def test_resume_refuses_while_a_claude_process_works_in_the_worktree(mesh, fake, run, repo, monkeypatch, capsys):
    # herdr lists no agent there (it lost the registration), but the process table shows one
    path, _ = archived_worker(mesh, repo, kind="claude", session="c1")
    monkeypatch.setattr(mesh, "agent_processes", lambda: [{"pid": 4242, "argv": ["claude", "-c"], "cwd": str(path / "src")}])
    capsys.readouterr()
    assert mesh.main(["resume", "proj-fix", "--from-run", "mold"]) == 1
    err = capsys.readouterr().err
    assert "may still be open in process 4242" in err and "ps -o pid,args -p 4242" in err
    assert "which herdr doesn't list" in err
    assert not fake.called("agent", "start")


def test_process_holder_matches_the_worktree_or_the_session(mesh, tmp_path):
    wt, repo = tmp_path / "wt", tmp_path / "repo"
    entry = {"kind": "claude", "cwd": str(wt), "repo": str(repo), "session": "s1"}
    procs = [{"pid": 1, "argv": ["agy"], "cwd": str(wt)}, {"pid": 2, "argv": ["claude"], "cwd": str(repo)}]
    assert mesh.live_holder("w", entry, agents=[], procs=procs) is None  # an agy there, a Claude elsewhere
    procs.append({"pid": 3, "argv": ["claude"], "cwd": str(wt / "src")})
    assert mesh.live_holder("w", entry, agents=[], procs=procs)["process"] == 3
    # a main checkout is shared with the user's own sessions: only one naming its session counts
    shared = {**entry, "cwd": str(repo)}
    assert mesh.live_holder("w", shared, agents=[], procs=procs) is None
    procs.append({"pid": 4, "argv": ["claude", "--resume", "s1"], "cwd": str(repo)})
    assert mesh.live_holder("w", shared, agents=[], procs=procs)["process"] == 4
    # a Copilot asker holds its session through its pid claim, never through some process in its worktree
    assert mesh.live_holder("w", {**entry, "kind": "copilot"}, agents=[], procs=procs) is None


def test_live_holder_outside_herdr_still_reads_the_processes(mesh, monkeypatch, tmp_path):
    def no_herdr(*args, timeout=None):
        raise mesh.MeshError("herdr not found (herdr); mesh must run inside herdr")

    monkeypatch.setattr(mesh, "herdr", no_herdr)
    entry = {"kind": "claude", "cwd": str(tmp_path / "wt"), "repo": str(tmp_path / "repo"), "session": "s1"}
    monkeypatch.setattr(mesh, "agent_processes", lambda: [{"pid": 7, "argv": ["claude"], "cwd": entry["cwd"]}])
    assert mesh.live_holder("w", entry)["process"] == 7
    # herdr is there but failed (its server down): the process table still answers
    monkeypatch.setattr(mesh, "herdr", lambda *a, timeout=None: (_ for _ in ()).throw(mesh.HerdrError("cli_error", "x")))
    assert mesh.live_holder("w", entry)["process"] == 7
    monkeypatch.setattr(mesh, "has_proc", lambda: False)
    with pytest.raises(mesh.HerdrError):  # no process table either (macOS): no guessing
        mesh.live_holder("w", entry)
    monkeypatch.setattr(mesh, "herdr", no_herdr)
    with pytest.raises(mesh.MeshError, match="herdr not found"):
        mesh.live_holder("w", entry)


def test_agent_processes_leaves_out_this_commands_ancestors():
    real = load_mesh()
    mine, pid = set(), os.getpid()
    while pid > 1:
        mine.add(pid)
        pid = real.parent_pid(pid)
    procs = real.agent_processes()
    assert not {p["pid"] for p in procs} & mine
    assert all(real.agent_binary(p["argv"]) for p in procs)


def test_resume_without_a_conversation_suggests_a_fresh_worker(mesh, fake, run, repo, monkeypatch, capsys):
    monkeypatch.setenv("MESH_AGY_HISTORY", str(repo / "none.jsonl"))
    archived_worker(mesh, repo)
    assert mesh.main(["resume", "proj-fix"]) == 1
    assert "mesh spawn --cwd" in capsys.readouterr().err


def test_open_reopens_the_conversation_here_outside_the_mesh(mesh, fake, repo, monkeypatch, capsys):
    path, _ = archived_worker(mesh, repo, kind="claude", session="s-1")
    monkeypatch.setenv("MESH_RUN", "mtest")
    calls = {}
    monkeypatch.setattr(mesh.os, "chdir", lambda p: calls.setdefault("cwd", p))
    monkeypatch.setattr(mesh.os, "execvpe", lambda f, argv, env: calls.update(argv=argv, env=env))
    assert mesh.main(["open", "proj-fix", "--", "--dangerously-skip-permissions"]) == 0
    assert calls["argv"] == ["claude", "--resume", "s-1", "--model", "opus", "--effort", "high",
                             "--dangerously-skip-permissions"]
    assert calls["cwd"] == str(path) and path.exists()
    assert not any(key.startswith("MESH_") for key in calls["env"])
    assert "git worktree remove" in capsys.readouterr().err


def test_open_without_herdr_refuses_a_conversation_a_process_holds(mesh, fake, repo, monkeypatch, capsys):
    # herdr is installed but its server isn't running, so only the process table can show the worker live
    path, _ = archived_worker(mesh, repo, kind="claude", session="s-1")
    fake.on("agent", "list", errors=["cli_error"] * 9)
    opened = []
    monkeypatch.setattr(mesh.os, "chdir", lambda p: None)
    monkeypatch.setattr(mesh.os, "execvpe", lambda f, argv, env: opened.append(argv))
    monkeypatch.setattr(mesh, "agent_processes", lambda: [{"pid": 4242, "argv": ["claude"], "cwd": str(path / "src")}])
    capsys.readouterr()
    assert mesh.main(["open", "proj-fix"]) == 1
    err = capsys.readouterr().err
    assert "may still be open in process 4242" in err and not opened
    assert "while herdr can't be asked: herdr cli_error" in err and "which herdr doesn't list" not in err
    monkeypatch.setattr(mesh, "agent_processes", lambda: [])
    assert mesh.main(["open", "proj-fix"]) == 0
    assert opened == [["claude", "--resume", "s-1", "--model", "opus", "--effort", "high"]]
    assert len(fake.called("agent", "list")) == 2  # herdr failed both times, asked once per open


def test_open_without_herdr_refuses_a_process_naming_the_session_in_a_shared_checkout(mesh, fake, repo, monkeypatch,
                                                                                      capsys):
    archived_worker(mesh, repo, kind="claude", session="s-1")
    path = mesh.mesh_home() / "archive" / "mold" / "run.json"
    data = json.loads(path.read_text())
    data["agents"]["proj-fix"]["cwd"] = str(repo)  # it worked in the main checkout, shared with other sessions
    path.write_text(json.dumps(data))
    fake.on("agent", "list", errors=["cli_error"] * 9)
    monkeypatch.setattr(mesh.os, "execvpe", lambda f, argv, env: pytest.fail("opened a held conversation"))
    monkeypatch.setattr(mesh, "agent_processes", lambda: [
        {"pid": 5, "argv": ["claude"], "cwd": str(repo)},  # the user's own session there: not the worker's
        {"pid": 6, "argv": ["claude", "--resume", "s-1"], "cwd": str(repo)}])
    capsys.readouterr()
    assert mesh.main(["open", "proj-fix"]) == 1
    assert "may still be open in process 6" in capsys.readouterr().err


def test_open_without_herdr_or_proc_warns_it_cant_tell(mesh, fake, repo, monkeypatch, capsys):
    path, _ = archived_worker(mesh, repo, kind="claude", session="s-1")
    fake.on("agent", "list", errors=["cli_error"] * 9)
    monkeypatch.setattr(mesh, "has_proc", lambda: False)
    opened = []
    monkeypatch.setattr(mesh.os, "chdir", lambda p: None)
    monkeypatch.setattr(mesh.os, "execvpe", lambda f, argv, env: opened.append(argv))
    capsys.readouterr()
    assert mesh.main(["open", "proj-fix"]) == 0 and opened
    assert "can't tell whether proj-fix is still running" in capsys.readouterr().err


# ── models per phase: approve, quota hits, switch ────────────────────────


def planning_worker(mesh, run, kind="agy", model="claude-opus-5-5-high", session="c1", phase="plan"):
    log = mesh.run_dir(run) / "logs" / "repo-fix-typo.log"
    log.parent.mkdir(parents=True, exist_ok=True)
    log.write_text("I1024 started\n")
    with mesh.editing_run(run) as data:
        data["agents"]["repo-fix-typo"].update(
            kind=kind, model=model, effort="high", repo="/repo", cwd="/wt/fix", session=session,
            workspace="w9", phase=phase, exec_model="flash" if kind == "agy" else "opus",
            log=str(log) if kind == "agy" else None,
            models=[{"model": model, "at": "t", "why": "spawn"}])
    sent_plan(mesh, run)
    return log


def test_approve_moves_the_worker_to_flash_in_the_same_conversation(mesh, fake, run, monkeypatch, capsys):
    log = planning_worker(mesh, run)
    with_quotas(mesh, monkeypatch)
    stub_new_pane(fake)
    capsys.readouterr()
    assert mesh.main(["approve", "repo-fix-typo", "--body", "Mind the migration."]) == 0
    out = json.loads(capsys.readouterr().out)
    assert out["model"] == "gemini-3.8-flash-medium"
    assert fake.called("pane", "close", "w2:p5")  # the planning pane goes
    assert fake.called("agent", "start")[-1][-7:] == (
        "--log-file", str(log), "--conversation", "c1", "--model", "gemini-3.8-flash-medium",
        "--dangerously-skip-permissions")
    entry = mesh.load_run(run)["agents"]["repo-fix-typo"]
    assert (entry["phase"], entry["pane"], entry["status"]) == ("exec", "w9:p10", "running")
    assert entry["effort"] == "medium" and entry["models"][-1]["effort"] == "medium"
    assert "swapped" not in entry and entry["models"][-1]["why"] == "plan approved"
    doorbell = fake.called("agent", "prompt")[-1][3]
    assert bare(doorbell).startswith("📬 [mesh] You are repo-fix-typo (worker) again in agent-mesh run mtest, "
                               "now on gemini-3.8-flash-medium at medium effort")
    [msg] = mesh.unread(run, "repo-fix-typo")
    assert msg.name.endswith("-answer.md") and "approved\n\nMind the migration." in msg.read_text()

    # a later approval (after a revision) doesn't switch again
    sent_plan(mesh, run)
    assert mesh.main(["approve", "repo-fix-typo"]) == 0
    assert len(fake.called("agent", "start")) == 1


def test_approve_a_claude_worker_resumes_it_on_opus_at_medium(mesh, fake, run, capsys):
    planning_worker(mesh, run, kind="claude", model="opus", session="s-1")
    stub_new_pane(fake)
    assert mesh.main(["approve", "repo-fix-typo"]) == 0
    assert started(fake)[-7:] == (
        "--resume", "s-1", "--model", "opus", "--effort", "medium", "--dangerously-skip-permissions")
    argv = fake.called("agent", "start")[-1]
    hooks = json.loads(argv[argv.index("--settings") + 1])["hooks"]  # reopened with its hooks again
    assert "hook stop --run mtest --as repo-fix-typo" in hooks["Stop"][0]["hooks"][0]["command"]


DIRECT_FIX = "Direct task (no plan)\n\nFix it."


def read_only_asker(mesh, run, **extra):
    with mesh.editing_run(run) as data:
        data["agents"]["repo-fix-typo"].update({
            "kind": "claude", "model": "opus", "effort": "high", "repo": "/repo", "cwd": "/wt/fix", "session": "s-1",
            "workspace": "w9", "read_only": True, "models": [{"model": "opus", "at": "t", "why": "spawn"}], **extra})


def test_a_change_task_reopens_a_read_only_asker_without_its_deny_list(mesh, fake, run, capsys):
    read_only_asker(mesh, run)
    stub_new_pane(fake)
    assert mesh.main(["send", "repo-fix-typo", "--type", "task", "--body", "Direct task (no plan)\n\nFix it."]) == 0
    captured = capsys.readouterr()
    out = json.loads(captured.out)
    assert out["read_only_lifted"] is True and out["rung"] is True
    assert "without its deny list" in captured.err
    argv = started(fake)
    assert argv[-7:] == ("--resume", "s-1", "--model", "opus", "--effort", "high", "--dangerously-skip-permissions")
    assert not deny_rules(argv)
    entry = mesh.load_run(run)["agents"]["repo-fix-typo"]
    assert (entry["read_only"], entry["models"][-1]["why"]) == (False, "read-only lifted")
    assert "again in agent-mesh run mtest" in fake.called("agent", "prompt")[-1][3]
    # Once lifted, the next task is an ordinary send.
    starts = len(fake.called("agent", "start"))
    assert mesh.main(["send", "repo-fix-typo", "--type", "task", "--body", "Also the footer."]) == 0
    assert len(fake.called("agent", "start")) == starts


def test_a_worker_sends_only_to_its_parent(mesh, fake, run, monkeypatch, capsys):
    read_only_asker(mesh, run)
    with mesh.editing_run(run) as data:
        data["agents"]["repo-other"] = {"role": "worker", "kind": "agy", "pane": "w2:p6", "parent": "repo-lead",
                                        "status": "running"}
    as_worker(monkeypatch, run)
    monkeypatch.setenv("CLAUDE_CODE_SESSION_ID", "s-1")
    for to in ("repo-fix-typo", "repo-other"):  # a direct task to itself would lift its own deny list
        capsys.readouterr()
        assert mesh.main(["send", to, "--type", "task", "--body", "Direct task (no plan)\n\nFix it."]) == 1
        assert "a worker sends only to its parent (repo-lead)" in capsys.readouterr().err
    assert not fake.called("agent", "start") and mesh.load_run(run)["agents"]["repo-fix-typo"]["read_only"]
    assert not mesh.unread(run, "repo-other") and not mesh.unread(run, "repo-fix-typo")
    refused = [(e["command"], e["to"]) for e in mesh.read_log(run) if e["event"] == "refused"]
    assert refused == [("send", "repo-fix-typo"), ("send", "repo-other")]
    assert mesh.main(["send", "repo-lead", "--type", "info", "--body", "FYI."]) in (0, 2)  # by name too


def test_a_read_only_follow_up_leaves_the_asker_as_it_is(mesh, fake, run, capsys):
    read_only_asker(mesh, run)
    assert mesh.main(["send", "repo-fix-typo", "--type", "task", "--body", "Question (read-only): and why?"]) == 0
    assert not fake.called("agent", "start") and mesh.load_run(run)["agents"]["repo-fix-typo"]["read_only"]


def test_a_busy_read_only_asker_is_sent_no_change(mesh, fake, run, capsys):
    read_only_asker(mesh, run)
    fake.on("agent", "wait", error="timeout")
    assert mesh.main(["send", "repo-fix-typo", "--type", "task", "--body", DIRECT_FIX]) == 1
    assert "Nothing was sent" in capsys.readouterr().err
    assert not mesh.unread(run, "repo-fix-typo")
    assert mesh.load_run(run)["agents"]["repo-fix-typo"]["read_only"] is True


def test_a_background_asker_is_lifted_by_a_copy_launched_without_the_deny_list(mesh, fake, cli, run, capsys):
    claude_root(mesh, run)
    read_only_asker(mesh, run, view="bg", bg_id="old1", pane=None)
    bg_cli(cli, rows=[{"id": "old1", "sessionId": "s-1", "status": "idle", "pid": 5},
                      {"id": "ab12cd34", "sessionId": "copy-s", "status": "busy", "pid": 6}])
    assert mesh.main(["send", "repo-fix-typo", "--type", "task", "--body", DIRECT_FIX]) == 0
    out = json.loads(capsys.readouterr().out)
    assert out["launched"] is True and out["read_only_lifted"] is True
    launch = cli.called("--bg")[-1]
    assert launch["argv"][1:4] == ["--bg", "--resume", "s-1"] and not deny_rules(launch["argv"])
    assert mesh.load_run(run)["agents"]["repo-fix-typo"]["read_only"] is False


def test_a_retired_asker_is_given_a_change_by_resume_not_send(mesh, fake, run, repo, capsys):
    read_only_asker(mesh, run, status="retired", cwd=str(repo), repo=str(repo))
    stub_new_pane(fake)
    assert mesh.main(["send", "repo-fix-typo", "--type", "task", "--body", DIRECT_FIX]) == 1
    assert "mesh resume repo-fix-typo --task-file" in capsys.readouterr().err
    assert not mesh.unread(run, "repo-fix-typo") and not fake.called("agent", "start")
    # `mesh resume` with the change reopens it without its deny list.
    fake.on("agent", "get", error="agent_not_found")
    assert mesh.main(["resume", "repo-fix-typo", "--task", "Direct task (no plan)\n\nFix it."]) == 0
    argv = fake.called("agent", "start")[-1]
    assert "--resume" in argv and not deny_rules(argv)
    assert mesh.load_run(run)["agents"]["repo-fix-typo"]["read_only"] is False


def test_a_failed_background_lift_keeps_one_task_and_points_to_resume(mesh, fake, cli, run, tmp_path, capsys):
    claude_root(mesh, run)
    read_only_asker(mesh, run, view="bg", bg_id="old1", pane=None, cwd=str(tmp_path), repo=str(tmp_path))
    bg_cli(cli, rows=[{"id": "old1", "sessionId": "s-1", "status": "idle", "pid": 5}])
    cli.on("--bg", returncode=1, stdout="boom")
    assert mesh.main(["send", "repo-fix-typo", "--type", "task", "--body", DIRECT_FIX]) == 1
    err = capsys.readouterr().err
    assert "don't send it again" in err and "`mesh resume repo-fix-typo` (no task)" in err
    assert len(mesh.unread(run, "repo-fix-typo")) == 1
    # The resume it points to reopens it without its deny list, and adds no second task.
    bg_cli(cli, rows=[{"id": "ab12cd34", "sessionId": "copy-s", "status": "busy", "pid": 6}])
    assert mesh.main(["resume", "repo-fix-typo"]) == 0
    launch = cli.called("--bg")[-1]
    assert launch["argv"][1:4] == ["--bg", "--resume", "s-1"] and not deny_rules(launch["argv"])
    assert len(mesh.unread(run, "repo-fix-typo")) == 1


def stub_panes(fake):
    """New panes for several starts: the first opens a tab, the rest split it."""
    stub_new_pane(fake)
    fake.on("pane", "layout", result={"layout": {"panes": [{"pane_id": "w9:p10", "rect": {"width": 200, "height": 50}}]}})
    fake.on("pane", "split", result={"pane": {"pane_id": "w9:p11"}})


def claude_planner(mesh, run, **extra):
    """A Claude worker planning under its deny list, with its plan waiting."""
    planning_worker(mesh, run, kind="claude", model="opus", session="s-1")
    with mesh.editing_run(run) as data:
        data["agents"]["repo-fix-typo"].update(planning=True, **extra)


def test_a_large_claude_worker_plans_under_the_deny_list(mesh, fake, cli, run, repo, capsys):
    stub_panes(fake)
    assert mesh.main(["spawn", "--kind", "claude", "--repo", str(repo), "--worktree", "feat",
                      "--task", "Add the feature."]) == 0
    out = json.loads(capsys.readouterr().out)
    rules = deny_rules(started(fake))
    assert f"Edit(/{Path(out['cwd']).resolve()}/**)" in rules and "Bash(git push:*)" in rules
    assert not any(r.startswith("Bash(uv") or r.startswith("Bash(pnpm") for r in rules)  # set-up still runs
    entry = mesh.load_run(run)["agents"]["proj-feat"]
    assert (entry["planning"], entry["read_only"], entry["phase"]) == (True, False, "plan")
    # One with a named model has no exec phase but plans all the same; one with no task yet will plan.
    assert mesh.main(["spawn", "--kind", "claude", "--repo", str(repo), "--name", "pinned", "--model", "opus",
                      "--task", "Add another."]) == 0
    assert deny_rules(started(fake)) and mesh.load_run(run)["agents"]["proj-pinned"]["planning"] is True
    assert mesh.main(["spawn", "--kind", "claude", "--repo", str(repo), "--name", "later"]) == 0
    assert deny_rules(started(fake)) and mesh.load_run(run)["agents"]["proj-later"]["planning"] is True
    # A user-reviewed plan is in plan mode already, and a direct task has no plan: no list.
    assert mesh.main(["spawn", "--kind", "claude", "--repo", str(repo), "--name", "user", "--plan-review", "user",
                      "--task", "Add a third."]) == 0
    assert not deny_rules(started(fake)) and mesh.load_run(run)["agents"]["proj-user"]["planning"] is False
    # agy has no deny list: its planner is held by the prompt alone.
    assert mesh.claude_deny({"kind": "agy", "planning": True, "cwd": "/wt"}) == []


def launch_argv(fake):
    """The command line the last `herdr agent start` runs: `claude` and everything after `--`."""
    call = list(fake.called("agent", "start")[-1])
    return ["claude", *call[call.index("--") + 1:]]


def test_every_way_mesh_starts_a_claude_pane_worker_is_recognised_as_its(mesh, fake, cli, run, repo, capsys):
    stub_panes(fake)
    assert mesh.main(["spawn", "--kind", "claude", "--repo", str(repo), "--worktree", "feat",
                      "--task", "Add the feature."]) == 0
    name = json.loads(capsys.readouterr().out)["name"]

    def launched(run_id, who):
        return mesh.mesh_launched(launch_argv(fake), run_id, who)

    assert launched(run, name) and mesh.load_run(run)["agents"][name]["planning"]  # with its deny list
    assert not launched(run, "repo-fix-typo")  # names this worker of this run
    assert not launched("mold", name)
    assert mesh.main(["spawn", "--kind", "claude", "--repo", str(repo), "--name", "user", "--plan-review", "user",
                      "--task", "Add a third."]) == 0
    assert launched(run, "proj-user")
    with mesh.editing_run(run) as data:  # a swap: `approve` reopening a planner (and `switch`, which shares it)
        data["agents"][name].update(planning=True, phase="plan", exec_model="opus", exec_effort="medium")
    sent_plan(mesh, run, name)
    assert mesh.main(["approve", name]) == 0
    assert fake.called("pane", "close") and launched(run, name)
    archived_worker(mesh, repo, kind="claude", session="c1")  # a resume (a peer's ask spawns or resumes too)
    assert mesh.main(["resume", "https://github.com/acme/proj/pull/7", "--task", "Apply the review."]) == 0
    assert launched(run, "proj-fix")
    assert not mesh.mesh_launched(["claude", "--resume", "c1"], run, "proj-fix")  # what a herdr restore runs
    link = repo.parent / "mesh-link"
    link.symlink_to(MESH_SCRIPT)  # the installed `mesh` is a symlink to the checkout
    assert mesh.mesh_launched(mesh_argv(run, "proj-fix", "c1", script=str(link)), run, "proj-fix")


def test_a_pane_worker_herdr_relaunched_is_reopened_with_its_flags_before_its_doorbell(mesh, fake, run, capsys):
    claude_planner(mesh, run)
    claude_in_pane(fake, "w2:p5", "claude", "--resume", "s-1")  # a herdr restore: none of mesh's flags
    stub_new_pane(fake)
    assert mesh.main(["send", "repo-fix-typo", "--type", "answer", "--body", "Use ISO dates."]) == 0
    argv = launch_argv(fake)
    assert mesh.mesh_launched(argv, run, "repo-fix-typo") and "Bash(git push:*)" in deny_rules(argv)
    assert started(fake)[-7:] == ("--resume", "s-1", "--model", "opus", "--effort", "high",
                                  "--dangerously-skip-permissions")
    assert fake.called("pane", "close", "w2:p5")
    assert bare(fake.called("agent", "prompt")[-1][3]).startswith(
        "📬 [mesh] You are repo-fix-typo (worker) again in agent-mesh run mtest, reopened with mesh's settings")
    [swap] = [e for e in mesh.read_log(run) if e["event"] == "swap"]
    assert swap["why"] == "relaunched"
    entry = mesh.load_run(run)["agents"]["repo-fix-typo"]
    assert (entry["pane"], entry["planning"], entry["model"]) == ("w9:p10", True, "opus")
    assert "relaunching" not in entry  # its claim is released


@pytest.mark.parametrize("case", ["mesh's own", "herdr can't say", "no claude there", "no leader, one claude",
                                  "no leader, two claudes", "agy", "bg", "another worker's", "another run's",
                                  "a prompt hook", "another checkout's mesh", "an older deny list"])
def test_only_a_claude_mesh_didnt_start_in_a_workers_pane_is_reopened(mesh, fake, run, capsys, case):
    claude_planner(mesh, run)
    if case in ("agy", "bg"):
        with mesh.editing_run(run) as data:
            data["agents"]["repo-fix-typo"].update(kind="agy") if case == "agy" else \
                data["agents"]["repo-fix-typo"].update(view="bg", bg_id="ab12")
    deny = mesh.claude_deny(mesh.load_run(run)["agents"]["repo-fix-typo"])[1:]
    prompt_hook = {"hooks": {"Stop": [{"hooks": [{"type": "prompt", "prompt": "Check your work."}]}]}}
    argv = {"mesh's own": mesh_argv(session="s-1", deny=deny), "another worker's": mesh_argv(name="repo-other"),
            "another run's": mesh_argv(run="mold"), "no claude there": ["bash"],
            "a prompt hook": ["claude", "--resume", "s-1", "--settings", json.dumps(prompt_hook)],
            # Neither an upgrade nor another checkout's `bin/mesh` delivering takes it for a relaunched one.
            "another checkout's mesh": mesh_argv(session="s-1", deny=deny, script="/elsewhere/bin/mesh"),
            "an older deny list": mesh_argv(session="s-1", deny=deny[:-1])}.get(case, ["claude", "--resume", "s-1"])
    claude_in_pane(fake, "w2:p5", *argv)
    if case == "herdr can't say":
        fake.on("pane", "process-info", "--pane", "w2:p5", error="cli_error")
    if case.startswith("no leader"):  # herdr gives the processes but no group: the pane's one Claude, if one
        restore = {"pid": 9, "argv": ["claude", "--resume", "s-1"]}
        fake.on("pane", "process-info", "--pane", "w2:p5", result={"process_info": {"foreground_processes": [
            restore, *([{"pid": 12, "argv": ["claude", "-p", "x"]}] if case.endswith("two claudes") else [])]}})
    stub_new_pane(fake)
    mesh.main(["send", "repo-fix-typo", "--type", "answer", "--body", "Use ISO dates."])
    reopened = bool(fake.called("agent", "start"))
    assert reopened == (case in ("another worker's", "another run's", "a prompt hook", "no leader, one claude"))
    if case in ("agy", "bg"):
        assert not fake.called("pane", "process-info")


@pytest.mark.parametrize("native", [False, True])
def test_a_report_to_a_claude_root_is_never_taken_for_a_relaunch(mesh, fake, run, monkeypatch, capsys, native):
    # The root's Claude is the user's own: no mesh `--settings`, and it must still hear its workers.
    if native:
        claude_root(mesh, run)
    with mesh.editing_run(run) as data:
        data["agents"]["repo-fix-typo"].update(kind="claude", session="s1")
    claude_in_pane(fake, "w1:p1", "claude")
    as_worker(monkeypatch, run)
    capsys.readouterr()
    assert mesh.main(["send", "parent", "--type", "done", "--body", "Done."]) == 0
    out = json.loads(capsys.readouterr().out)
    assert ("send_message" in out) if native else out["rung"]
    assert not fake.called("agent", "start") and not fake.called("pane", "process-info", "--pane", "w1:p1")


@pytest.mark.parametrize("claim", ["live", "dead"])
def test_one_delivery_at_a_time_reopens_a_relaunched_worker(mesh, fake, run, capsys, claim):
    import os
    claude_planner(mesh, run)
    claude_in_pane(fake, "w2:p5", "claude", "--resume", "s-1")
    stub_new_pane(fake)
    mark = ({"pid": os.getpid(), "start": mesh.proc_start(os.getpid())} if claim == "live"
            else {"pid": 2 ** 22 + 9, "start": "x"})  # a reopen another delivery is doing, or one that died
    with mesh.editing_run(run) as data:
        data["agents"]["repo-fix-typo"]["relaunching"] = mark
    capsys.readouterr()
    code = mesh.main(["send", "repo-fix-typo", "--type", "answer", "--body", "Use ISO dates."])
    out = json.loads(capsys.readouterr().out)
    if claim == "live":  # stored for the doorbell of the reopen in progress, never a second `claude --resume`
        assert code == 2 and "being reopened" in out["error"] and not fake.called("agent", "start")
        assert "if that fails, the next message retries" in out["error"]
        assert mesh.load_run(run)["agents"]["repo-fix-typo"]["relaunching"] == mark
    else:
        assert code == 0 and fake.called("agent", "start")
        assert "relaunching" not in mesh.load_run(run)["agents"]["repo-fix-typo"]


@pytest.mark.parametrize("state", ["starting", "held", "running"])
def test_a_delivery_that_finds_the_worker_already_reopened_rings_it_once_it_runs(mesh, fake, run, state):
    claude_planner(mesh, run)
    entry = mesh.load_run(run)["agents"]["repo-fix-typo"]  # as this delivery saw it: relaunched in w2:p5
    path = mesh.record(run, "repo-lead", "repo-fix-typo", "answer", "Use ISO dates.")
    with mesh.editing_run(run) as data:  # meanwhile another delivery's reopen moved it, and released its claim
        data["agents"]["repo-fix-typo"].update(pane="w9:p10", status="starting" if state == "starting" else "running",
                                               **({"held": {"screen": "trust"}} if state == "held" else {}))
    out = mesh.reopen_relaunched(run, "repo-lead", "repo-fix-typo", "answer", "Use ISO dates.", entry, path)
    if state == "running":
        assert out is None  # the caller rings the new pane
    else:  # never typed into a pane at a startup screen: the reopen's own doorbell covers it
        assert not out["rung"] and "being reopened" in out["error"]
        assert "relaunching" not in mesh.load_run(run)["agents"]["repo-fix-typo"]
    assert not fake.called("agent", "start") and not fake.called("agent", "prompt")


def test_a_relaunched_worker_herdr_lost_is_not_reopened_blind(mesh, fake, run, capsys):
    claude_planner(mesh, run)
    claude_in_pane(fake, "w2:p5", "claude", "--resume", "s-1")
    fake.on("agent", "wait", error="agent_not_found")  # its turn can't be waited for
    capsys.readouterr()
    assert mesh.main(["send", "repo-fix-typo", "--type", "answer", "--body", "Use ISO dates."]) == 2
    assert "herdr doesn't know repo-fix-typo" in json.loads(capsys.readouterr().out)["error"]
    assert not fake.called("pane", "close") and not fake.called("agent", "start")


def test_ring_refuses_a_relaunched_worker(mesh, fake, run, capsys):
    claude_planner(mesh, run)
    mesh.record(run, "repo-lead", "repo-fix-typo", "answer", "Use ISO dates.")
    claude_in_pane(fake, "w2:p5", "claude", "--resume", "s-1")
    capsys.readouterr()
    assert mesh.main(["ring", "repo-fix-typo"]) == 1
    assert "`mesh send repo-fix-typo …` reopens it" in capsys.readouterr().err
    assert not fake.called("agent", "prompt")


def test_a_relaunched_worker_in_a_turn_is_not_rung_without_its_flags(mesh, fake, run, capsys):
    claude_planner(mesh, run)
    claude_in_pane(fake, "w2:p5", "claude", "--resume", "s-1")
    fake.on("agent", "wait", error="timeout")  # the user typed into it, and its turn goes on
    capsys.readouterr()
    assert mesh.main(["send", "repo-fix-typo", "--type", "answer", "--body", "Use ISO dates."]) == 2
    out = json.loads(capsys.readouterr().out)
    assert not out["rung"] and "runs a Claude mesh didn't start" in out["error"] and "mesh retire" in out["error"]
    assert not fake.called("agent", "prompt") and not fake.called("pane", "close")
    assert [m.name for m in mesh.unread(run, "repo-fix-typo")]  # stored: the next message retries the reopen
    assert mesh.load_run(run)["agents"]["repo-fix-typo"]["pane"] == "w2:p5"


def test_a_large_background_claude_worker_plans_under_the_deny_list(mesh, fake, cli, run, repo, capsys):
    claude_root(mesh, run)
    bg_cli(cli)
    assert mesh.main(["spawn", "--kind", "claude", "--repo", str(repo), "--worktree", "feat", "--view", "bg",
                      "--task", "Add the feature."]) == 0
    assert "Bash(git push:*)" in deny_rules(cli.called("--bg")[-1]["argv"])
    assert mesh.load_run(run)["agents"]["proj-feat"]["planning"] is True


def test_approve_reopens_a_claude_planner_without_its_deny_list(mesh, fake, run, capsys):
    claude_planner(mesh, run)
    stub_panes(fake)
    mesh.swap_model(run, "repo-fix-typo", "opus", "high", "quota")  # a switch keeps it
    assert "Edit(//wt/fix/**)" in deny_rules(started(fake))
    starts = len(fake.called("agent", "start"))
    assert mesh.main(["send", "repo-fix-typo", "--type", "answer", "--body", "changes: also X"]) == 0
    assert len(fake.called("agent", "start")) == starts  # an answer never reopens it
    sent_plan(mesh, run)
    assert mesh.main(["approve", "repo-fix-typo"]) == 0
    argv = started(fake)
    assert not deny_rules(argv) and argv[-7:] == (
        "--resume", "s-1", "--model", "opus", "--effort", "medium", "--dangerously-skip-permissions")
    entry = mesh.load_run(run)["agents"]["repo-fix-typo"]
    assert (entry["planning"], entry["phase"]) == (False, "exec")


def test_approve_keep_model_reopens_a_claude_planner_on_the_same_model(mesh, fake, run, capsys):
    claude_planner(mesh, run)
    stub_new_pane(fake)
    assert mesh.main(["approve", "repo-fix-typo", "--keep-model"]) == 0
    out = json.loads(capsys.readouterr().out)
    argv = started(fake)
    assert out["kept_model"] is True and not deny_rules(argv)
    assert argv[-7:] == ("--resume", "s-1", "--model", "opus", "--effort", "high", "--dangerously-skip-permissions")
    entry = mesh.load_run(run)["agents"]["repo-fix-typo"]
    assert (entry["planning"], entry["phase"], entry["effort"]) == (False, "exec", "high")
    assert [m["why"] for m in entry["models"]].count("plan approved") == 1
    assert [e["model"] for e in mesh.read_log(run) if e["event"] == "approve_kept_model"] == ["opus"]
    assert "approved" in mesh.unread(run, "repo-fix-typo")[0].read_text()


def test_approve_keep_model_logs_nothing_when_a_planners_reopen_cannot_start(mesh, fake, run, capsys):
    claude_planner(mesh, run)
    fake.on("agent", "wait", error="timeout")
    assert mesh.main(["approve", "repo-fix-typo", "--keep-model"]) == 1
    assert "only reopening its conversation lifts that" in capsys.readouterr().err
    assert not [e for e in mesh.read_log(run) if e["event"] == "approve_kept_model"]
    assert mesh.load_run(run)["agents"]["repo-fix-typo"]["phase"] == "plan"


def test_approve_lifts_a_pinned_claude_planner_and_a_busy_one_is_told_to_retry(mesh, fake, run, capsys):
    claude_planner(mesh, run, phase=None, exec_model=None, pinned=True)
    fake.on("agent", "wait", error="timeout")
    assert mesh.main(["approve", "repo-fix-typo"]) == 1
    err = capsys.readouterr().err
    assert "only reopening its conversation lifts that" in err and "--keep-model" not in err
    assert mesh.unread(run, "repo-fix-typo") == []
    fake.on("agent", "wait", result={})
    stub_new_pane(fake)
    assert mesh.main(["approve", "repo-fix-typo"]) == 0
    argv = started(fake)
    assert not deny_rules(argv) and argv[-3:] == ("--effort", "high", "--dangerously-skip-permissions")
    assert mesh.load_run(run)["agents"]["repo-fix-typo"]["planning"] is False


def test_a_failed_approval_of_a_pinned_planner_can_be_retried(mesh, fake, cli, run, capsys):
    claude_root(mesh, run)
    claude_planner(mesh, run, phase=None, exec_model=None, pinned=True, view="bg", bg_id="old1", pane=None)
    bg_cli(cli, rows=[{"id": "old1", "sessionId": "s-1", "status": "idle", "pid": 5}])
    cli.on("--bg", returncode=1, stdout="boom")
    assert mesh.main(["approve", "repo-fix-typo"]) == 1
    entry = mesh.load_run(run)["agents"]["repo-fix-typo"]
    assert (entry["status"], entry["planning"]) == ("swap_failed", True)
    bg_cli(cli, rows=[{"id": "old1", "sessionId": "s-1", "status": "idle", "pid": 5},
                      {"id": "ab12cd34", "sessionId": "copy-s", "status": "busy", "pid": 6}])
    assert mesh.main(["approve", "repo-fix-typo"]) == 0
    assert not deny_rules(cli.called("--bg")[-1]["argv"])
    assert mesh.load_run(run)["agents"]["repo-fix-typo"]["planning"] is False


def test_a_failed_background_lift_of_a_planner_points_to_a_resume_without_the_list(mesh, fake, cli, run, tmp_path,
                                                                                    capsys):
    claude_root(mesh, run)
    claude_planner(mesh, run, view="bg", bg_id="old1", pane=None, cwd=str(tmp_path), repo=str(tmp_path))
    bg_cli(cli, rows=[{"id": "old1", "sessionId": "s-1", "status": "idle", "pid": 5}])
    cli.on("--bg", returncode=1, stdout="boom")
    assert mesh.main(["send", "repo-fix-typo", "--type", "task", "--body", DIRECT_FIX]) == 1
    assert "`mesh resume repo-fix-typo` (no task)" in capsys.readouterr().err
    assert mesh.load_run(run)["agents"]["repo-fix-typo"]["planning"] is False
    bg_cli(cli, rows=[{"id": "ab12cd34", "sessionId": "copy-s", "status": "busy", "pid": 6}])
    assert mesh.main(["resume", "repo-fix-typo"]) == 0
    assert not deny_rules(cli.called("--bg")[-1]["argv"])
    assert len(mesh.unread(run, "repo-fix-typo")) == 1


def test_a_retired_planner_is_given_a_direct_task_by_resume_not_send(mesh, fake, cli, run, tmp_path, capsys):
    claude_root(mesh, run)
    claude_planner(mesh, run, view="bg", bg_id="old1", pane=None, status="retired", cwd=str(tmp_path),
                   repo=str(tmp_path))
    bg_cli(cli, rows=[{"id": "ab12cd34", "sessionId": "copy-s", "status": "busy", "pid": 6}])
    assert mesh.main(["send", "repo-fix-typo", "--type", "task", "--body", DIRECT_FIX]) == 1
    assert "mesh resume repo-fix-typo --task-file" in capsys.readouterr().err
    assert not cli.called("--bg") and not mesh.unread(run, "repo-fix-typo")
    assert mesh.load_run(run)["agents"]["repo-fix-typo"]["status"] == "retired"


def test_an_askers_planned_task_keeps_its_deny_list_until_approval(mesh, fake, run, capsys):
    read_only_asker(mesh, run, phase="plan", exec_model="opus")
    assert mesh.main(["send", "repo-fix-typo", "--type", "task", "--body", "Add the feature."]) == 0
    captured = capsys.readouterr()
    assert not fake.called("agent", "start") and "deny_lifted" not in captured.out
    assert "plans this task under the same deny list" in captured.err
    entry = mesh.load_run(run)["agents"]["repo-fix-typo"]
    assert (entry["read_only"], entry["planning"]) == (False, True)
    assert "Edit(//wt/fix/**)" in mesh.claude_deny(entry)
    assert "mesh denies edits" in mesh.worker_phase(entry, mesh.read_log(run), "repo-fix-typo", "Add the feature.")
    # A planner re-scoped to a direct task is reopened without the list.
    stub_new_pane(fake)
    assert mesh.main(["send", "repo-fix-typo", "--type", "task", "--body", DIRECT_FIX]) == 0
    assert json.loads(capsys.readouterr().out)["deny_lifted"] is True
    entry = mesh.load_run(run)["agents"]["repo-fix-typo"]
    assert not deny_rules(started(fake)) and (entry["planning"], entry["phase"]) == (False, None)


def test_resume_plans_under_the_deny_list_and_a_direct_task_gets_none(mesh, fake, run, repo, capsys):
    archived_worker(mesh, repo, kind="claude", session="s-1")
    stub_new_pane(fake)
    assert mesh.main(["resume", "mesh/mold/fix", "--task", "Add the feature."]) == 0
    name = json.loads(capsys.readouterr().out)["name"]
    assert "Bash(git push:*)" in deny_rules(started(fake))
    assert mesh.load_run(run)["agents"][name]["planning"] is True
    # Retired mid-plan and resumed with no task: it is still planning.
    with mesh.editing_run(run) as data:
        data["agents"][name]["status"] = "retired"
    assert mesh.main(["resume", name]) == 0
    assert deny_rules(started(fake)) and mesh.load_run(run)["agents"][name]["planning"] is True
    with mesh.editing_run(run) as data:
        data["agents"][name]["status"] = "retired"
    assert mesh.main(["resume", name, "--task", DIRECT_FIX]) == 0
    assert not deny_rules(started(fake)) and mesh.load_run(run)["agents"][name]["planning"] is False


def test_a_finished_worker_plans_a_sent_task_unguarded_and_a_resume_mid_plan_guards_it(mesh, fake, run, repo,
                                                                                       monkeypatch, capsys):
    archived_worker(mesh, repo, kind="claude", session="s-1")
    stub_new_pane(fake)
    assert mesh.main(["resume", "mesh/mold/fix", "--task", DIRECT_FIX]) == 0
    name = json.loads(capsys.readouterr().out)["name"]
    starts = len(fake.called("agent", "start"))
    # Finished, then sent a task it plans: `send` never reopens it to add the list (ORCHESTRATE: retire + resume).
    assert mesh.main(["send", name, "--type", "task", "--body", "# Task: add retries"]) == 0
    assert len(fake.called("agent", "start")) == starts
    monkeypatch.setenv("MESH_RUN", run)
    monkeypatch.setenv("MESH_SELF", name)
    assert mesh.main(["inbox"]) == 0
    assert mesh.main(["send", "parent", "--type", "plan", "--body", "# Plan: retries"]) == 0
    entry = mesh.load_run(run)["agents"][name]
    assert (entry["phase"], entry["planning"]) == ("plan", False)
    monkeypatch.delenv("MESH_RUN")
    monkeypatch.delenv("MESH_SELF")
    # Retired mid-plan and resumed with no task: it comes back a planner, under the list, on its model.
    with mesh.editing_run(run) as data:
        data["agents"][name]["status"] = "retired"
    assert mesh.main(["resume", name]) == 0
    assert "Bash(git push:*)" in deny_rules(started(fake))
    entry = mesh.load_run(run)["agents"][name]
    assert (entry["planning"], entry["phase"], entry["exec_effort"], entry["effort"]) == (
        True, "plan", "medium", "high")
    sent_plan(mesh, run, name)
    capsys.readouterr()
    assert mesh.main(["approve", name]) == 0  # lifts it, and moves it to its execution effort
    assert not deny_rules(started(fake)) and started(fake)[-6:-1] == ("s-1", "--model", "opus", "--effort", "medium")
    entry = mesh.load_run(run)["agents"][name]
    assert (entry["planning"], entry["phase"]) == (False, "exec")


def test_a_named_model_on_a_resume_mid_plan_keeps_the_list_but_not_the_phase(mesh, fake, run, repo, capsys):
    archived_worker(mesh, repo, kind="claude", session="s-1")
    stub_new_pane(fake)
    assert mesh.main(["resume", "mesh/mold/fix", "--task", "Add the feature."]) == 0
    name = json.loads(capsys.readouterr().out)["name"]
    with mesh.editing_run(run) as data:
        data["agents"][name].update(status="retired", planning=False)  # its plan phase came from a sent plan
    assert mesh.main(["resume", name, "--model", "sonnet"]) == 0
    entry = mesh.load_run(run)["agents"][name]
    assert deny_rules(started(fake)) and (entry["planning"], entry["phase"], entry["pinned"]) == (True, None, True)


def test_a_direct_task_ends_a_plan_phase_with_no_deny_list_too(mesh, fake, run, capsys):
    for kind, model in (("claude", "opus"), ("agy", "claude-opus-5-5-high")):
        planning_worker(mesh, run, kind=kind, model=model)  # a sent plan's phase: no `planning`, no list
        assert mesh.main(["send", "repo-fix-typo", "--type", "task", "--body", "Add more."]) == 0
        assert mesh.load_run(run)["agents"]["repo-fix-typo"]["phase"] == "plan"  # a task it plans keeps it
        assert mesh.main(["send", "repo-fix-typo", "--type", "task", "--body", DIRECT_FIX]) == 0
        entry = mesh.load_run(run)["agents"]["repo-fix-typo"]
        assert entry["phase"] is None and not entry.get("planning") and not fake.called("agent", "start")
    assert [e["name"] for e in mesh.read_log(run) if e["event"] == "plan_phase_ended"] == ["repo-fix-typo"] * 2


def test_a_resume_mid_plan_keeps_an_agy_planner_and_not_one_that_finished(mesh, fake, run, repo, capsys):
    archived_worker(mesh, repo, kind="agy", session="c1")
    stub_new_pane(fake)
    assert mesh.main(["resume", "mesh/mold/fix", "--task", "Add the feature."]) == 0
    name = json.loads(capsys.readouterr().out)["name"]
    planned = mesh.load_run(run)["agents"][name]
    with mesh.editing_run(run) as data:
        data["agents"][name]["status"] = "retired"
    assert mesh.main(["resume", name]) == 0
    entry = mesh.load_run(run)["agents"][name]
    assert (entry["phase"], entry["model"], entry["exec_model"]) == ("plan", planned["model"], "flash")
    # It reported `done` without an approval (the fix was already in): it no longer owes a plan.
    mesh.record(run, name, "repo-lead", "done", "Already fixed on main.")
    with mesh.editing_run(run) as data:
        data["agents"][name]["status"] = "retired"
    assert mesh.main(["resume", name]) == 0
    assert mesh.load_run(run)["agents"][name]["phase"] is None


def test_a_claude_worker_that_finished_mid_plan_resumes_without_the_list(mesh, fake, run, repo, capsys):
    archived_worker(mesh, repo, kind="claude", session="s-1")
    stub_new_pane(fake)
    assert mesh.main(["resume", "mesh/mold/fix", "--task", DIRECT_FIX]) == 0
    name = json.loads(capsys.readouterr().out)["name"]
    with mesh.editing_run(run) as data:  # a plan phase from a sent plan, then a `done` with no approval
        data["agents"][name].update(status="retired", phase="plan", exec_model="opus", exec_effort="medium")
    mesh.record(run, name, "repo-lead", "done", "Nothing to change.")
    assert mesh.main(["resume", name]) == 0
    entry = mesh.load_run(run)["agents"][name]
    assert not deny_rules(started(fake)) and (entry["planning"], entry["phase"]) == (False, None)


def test_a_no_task_resume_keeps_an_asker_read_only(mesh, fake, run, repo, capsys):
    archived_worker(mesh, repo, kind="claude", session="s-1")
    stub_new_pane(fake)
    assert mesh.main(["resume", "mesh/mold/fix", "--task", "Question (read-only)\n\nWhy?"]) == 0
    name = json.loads(capsys.readouterr().out)["name"]
    with mesh.editing_run(run) as data:
        data["agents"][name]["status"] = "retired"
    assert mesh.main(["resume", name]) == 0  # a follow-up question sent while it was stopped
    entry = mesh.load_run(run)["agents"][name]
    assert "Bash(git push:*)" in deny_rules(started(fake)) and (entry["read_only"], entry["planning"]) == (True, False)


def test_resume_of_a_live_worker_with_a_task_it_plans_says_retire_first(mesh, fake, run, repo, capsys):
    with mesh.editing_run(run) as data:
        data["agents"]["repo-fix-typo"].update(kind="claude", cwd=str(repo), repo=str(repo), session="s-1")
    fake.on("agent", "list", result={"agents": [{"pane_id": "w9:p3", "name": "repo-fix-typo", "cwd": str(repo)}]})
    capsys.readouterr()
    assert mesh.main(["resume", "repo-fix-typo", "--task", "Add the feature."]) == 1
    assert "`mesh retire repo-fix-typo` once its `done` is in" in capsys.readouterr().err
    assert mesh.main(["resume", "repo-fix-typo", "--task", DIRECT_FIX]) == 1
    assert "send it the task: `mesh send repo-fix-typo --type task`" in capsys.readouterr().err


def test_a_model_swap_keeps_a_read_only_askers_deny_list(mesh, fake, run, capsys):
    read_only_asker(mesh, run)
    stub_new_pane(fake)
    mesh.swap_model(run, "repo-fix-typo", "sonnet", "high", "quota")
    assert "Edit(//wt/fix/**)" in deny_rules(fake.called("agent", "start")[-1])
    assert mesh.load_run(run)["agents"]["repo-fix-typo"]["read_only"] is True


def test_approve_with_the_same_model_and_effort_keeps_the_pane_of_a_worker_with_no_deny_list(mesh, fake, run,
                                                                                           capsys):
    # An entry from before planners got the deny list (or one whose list is off): nothing to lift, no reopen.
    planning_worker(mesh, run, kind="claude", model="opus", session="s-1")
    with mesh.editing_run(run) as data:  # `spawn --kind claude --effort high`: both phases match
        data["agents"]["repo-fix-typo"]["exec_effort"] = "high"
    assert mesh.main(["approve", "repo-fix-typo"]) == 0
    assert not fake.called("agent", "start") and not fake.called("pane", "close")
    assert mesh.load_run(run)["agents"]["repo-fix-typo"]["phase"] == "exec"
    assert bare(fake.called("agent", "prompt")[-1][3]).startswith("📬 [mesh] answer from repo-lead: approved")


def test_approve_with_the_same_model_and_effort_still_reopens_a_planner_to_lift_its_list(mesh, fake, run, capsys):
    claude_planner(mesh, run, exec_effort="high")  # `spawn --kind claude --effort high`: both phases match
    stub_new_pane(fake)
    assert mesh.main(["approve", "repo-fix-typo"]) == 0
    argv = started(fake)
    assert fake.called("pane", "close") and not deny_rules(argv)
    assert argv[-7:] == ("--resume", "s-1", "--model", "opus", "--effort", "high", "--dangerously-skip-permissions")
    entry = mesh.load_run(run)["agents"]["repo-fix-typo"]
    assert (entry["phase"], entry["planning"]) == ("exec", False)


def test_approve_without_an_exec_model_is_a_plain_answer(mesh, fake, run, capsys):
    sent_plan(mesh, run)
    assert mesh.main(["approve", "repo-fix-typo"]) == 0
    assert not fake.called("agent", "start") and not fake.called("pane", "close")
    assert bare(fake.called("agent", "prompt")[-1][3]).startswith("📬 [mesh] answer from repo-lead: approved")


def test_a_worker_never_approves_itself(mesh, fake, run, monkeypatch, capsys):
    sent_plan(mesh, run)
    as_worker(monkeypatch, run)
    capsys.readouterr()
    assert mesh.main(["approve", "repo-fix-typo"]) == 1
    assert "only a root runs `mesh approve`" in capsys.readouterr().err
    assert mesh.unread(run, "repo-fix-typo") == []
    [refused] = [e for e in mesh.read_log(run) if e["event"] == "refused"]
    assert (refused["name"], refused["command"]) == ("repo-fix-typo", "approve")
    # the user, from another terminal, acts for the parent
    monkeypatch.delenv("MESH_RUN")
    monkeypatch.delenv("MESH_SELF")
    monkeypatch.delenv("HERDR_PANE_ID")
    assert mesh.main(["approve", "repo-fix-typo", "--run", run, "--as", "repo-lead"]) == 0


def test_another_worker_cannot_approve_switch_or_retire(mesh, fake, run, monkeypatch, capsys):
    sent_plan(mesh, run)
    with mesh.editing_run(run) as data:
        data["agents"]["repo-other"] = {"role": "worker", "kind": "agy", "pane": "w2:p6", "parent": "repo-lead",
                                        "status": "running"}
    monkeypatch.setenv("MESH_RUN", run)
    monkeypatch.setenv("MESH_SELF", "repo-other")
    for command in (["approve"], ["switch", "--model", "flash"], ["retire"]):
        capsys.readouterr()
        assert mesh.main([command[0], "repo-fix-typo", *command[1:]]) == 1
        assert f"repo-other is a worker of run {run}, and only a root runs `mesh {command[0]}`" in capsys.readouterr().err
    assert not fake.called("pane", "close") and not fake.called("agent", "start")
    assert mesh.load_run(run)["agents"]["repo-fix-typo"]["status"] == "running"
    # nor does the root aim them at itself
    monkeypatch.delenv("MESH_RUN")
    monkeypatch.delenv("MESH_SELF")
    for command in (["approve"], ["switch", "--model", "flash"], ["retire"]):
        capsys.readouterr()
        assert mesh.main([command[0], "repo-lead", *command[1:]]) == 1
        assert f"repo-lead is the run's root, and `mesh {command[0]}` acts on a worker" in capsys.readouterr().err
    assert not fake.called("agent", "wait")


def test_approve_needs_a_plan_waiting(mesh, fake, run, capsys):
    capsys.readouterr()
    assert mesh.main(["approve", "repo-fix-typo"]) == 1
    assert "hasn't sent one" in capsys.readouterr().err
    mesh.record(run, "repo-fix-typo", "repo-lead", "plan", "Plan.")
    mesh.record(run, "repo-fix-typo", "repo-lead", "question", "Or keep _db.py?")
    assert mesh.main(["approve", "repo-fix-typo"]) == 1
    err = capsys.readouterr().err
    assert "newest report" in err and "is a question" in err and "mesh send repo-fix-typo --type answer" in err
    assert "approve the plan it then sends again" in err
    # an answer asking for changes needs a new plan
    assert mesh.main(["send", "repo-fix-typo", "--type", "answer", "--body", "changes: keep it"]) == 0
    capsys.readouterr()
    assert mesh.main(["approve", "repo-fix-typo"]) == 1
    assert "hasn't reported since the answer it got from repo-lead" in capsys.readouterr().err
    mesh.record(run, "repo-lead", "repo-fix-typo", "task", "Do it differently.")
    assert mesh.main(["approve", "repo-fix-typo"]) == 1
    err = capsys.readouterr().err
    assert "since the task it got" in err and "mesh ring" not in err  # only an answer can be a lost approval
    mesh.record(run, "repo-fix-typo", "repo-lead", "plan", "Plan 2.")
    assert mesh.main(["approve", "repo-fix-typo"]) == 0
    capsys.readouterr()
    assert mesh.main(["approve", "repo-fix-typo"]) == 1  # approved already
    assert "mesh ring repo-fix-typo" in capsys.readouterr().err
    mesh.record(run, "repo-fix-typo", "repo-lead", "done", "Done.")
    assert mesh.main(["approve", "repo-fix-typo"]) == 1
    assert "is a done; there is nothing to approve" in capsys.readouterr().err


def test_watch_alerts_on_a_new_quota_hit(mesh, fake, run, monkeypatch, capsys):
    log = planning_worker(mesh, run)
    log.write_text(log.read_text() + "E1023 RESOURCE_EXHAUSTED (code 429): an old hit. Resets in 1h.\n")
    fake.on("agent", "get", result={"agent": {"agent_status": "working"}})
    polls = iter(range(10))

    def sleep(_):
        if next(polls) == 1:
            with open(log, "a") as fh:
                fh.write("E1024 errorreport.go:224] generating and executing: RESOURCE_EXHAUSTED (code 429): "
                         "Individual quota reached. Please upgrade your subscription. Resets in 138h17m29s.\n")

    monkeypatch.setattr(mesh.time, "sleep", sleep)
    assert mesh.main(["watch", "repo-fix-typo", "--run-id", run]) == 7
    out = capsys.readouterr().out
    assert "hit its quota on claude-opus-5-5-high (resets in 138h17m29s)" in out
    assert "mesh switch repo-fix-typo" in out


def test_switch_moves_a_planning_worker_down_the_chain(mesh, fake, run, monkeypatch, capsys):
    planning_worker(mesh, run, model="gemini-4-argon-high")
    with_quotas(mesh, monkeypatch)
    stub_new_pane(fake)
    capsys.readouterr()
    assert mesh.main(["switch", "repo-fix-typo"]) == 0
    out = json.loads(capsys.readouterr().out)
    assert (out["previous"], out["model"]) == ("gemini-4-argon-high", "claude-opus-5-5-high")
    entry = mesh.load_run(run)["agents"]["repo-fix-typo"]
    assert entry["phase"] == "plan" and entry["models"][-1]["why"] == "quota"
    assert "Continue exactly where you stopped" in mesh.unread(run, "repo-fix-typo")[0].read_text()


def test_switch_with_flash_exhausted_while_executing_changes_nothing(mesh, fake, run, monkeypatch, capsys):
    planning_worker(mesh, run, model="gemini-3.8-flash-high", phase="exec")
    with_quotas(mesh, monkeypatch, USAGE.replace("52%", "0%"))
    assert mesh.main(["switch", "repo-fix-typo"]) == 1
    assert "no model with quota left" in capsys.readouterr().err
    assert not fake.called("agent", "start") and not fake.called("pane", "close")


def test_switch_a_claude_worker_needs_a_model(mesh, fake, run, capsys):
    planning_worker(mesh, run, kind="claude", model="opus", session="s-1")
    assert mesh.main(["switch", "repo-fix-typo"]) == 1
    assert "pass --model" in capsys.readouterr().err


def test_watch_carries_on_while_the_worker_is_reopened(mesh, fake, run, monkeypatch, capsys):
    planning_worker(mesh, run)
    states = iter(["working", "gone", "working"])

    def get(*args, timeout=None):
        fake.calls.append(args)
        if args[:2] == ("agent", "get"):
            state = next(states, None)
            if state == "gone":
                with mesh.editing_run(run) as data:
                    data["agents"]["repo-fix-typo"]["status"] = "starting"
                raise mesh.HerdrError("agent_not_found", "stubbed")
            if state is None:
                mesh.log_event(run, "send", sender="repo-fix-typo", to="repo-lead", type="done", file="x.md")
                state = "idle"
            return {"agent": {"agent_status": state}}
        return {}

    monkeypatch.setattr(mesh, "herdr", get)
    assert mesh.main(["watch", "repo-fix-typo", "--run-id", run]) == 0
    assert "reported done" in capsys.readouterr().out


# ── review fixes: failed swaps, log offsets, switch chain, kept doorbell ──


def stub_split(fake):
    """A second worker pane beside the first, for tests that swap a worker twice."""
    fake.on("pane", "layout", result={"layout": {"panes": [{"pane_id": "w9:p10", "rect": {"width": 200, "height": 50}}]}})
    fake.on("pane", "split", result={"pane": {"pane_id": "w9:p11"}})


def test_failed_swap_marks_the_worker_and_the_watch_alerts(mesh, fake, run, monkeypatch, capsys):
    planning_worker(mesh, run)
    with_quotas(mesh, monkeypatch)
    stub_new_pane(fake)
    fake.on("agent", "start", errors=["cli_error"] * 10)
    capsys.readouterr()
    assert mesh.main(["approve", "repo-fix-typo"]) == 1
    assert "was not reopened on gemini-3.8-flash-medium" in capsys.readouterr().err
    entry = mesh.load_run(run)["agents"]["repo-fix-typo"]
    # the recorded model is still the planning one, so approving again retries the swap
    assert (entry["status"], entry["model"], entry["effort"], entry["phase"]) == (
        "swap_failed", "claude-opus-5-5-high", "high", "plan")
    assert "swapped" not in entry and len(entry["models"]) == 1

    fake.on("agent", "get", errors=["agent_not_found"] * 5)
    assert mesh.main(["watch", "repo-fix-typo", "--run-id", run]) == 5
    assert "is gone" in capsys.readouterr().out


def test_a_failed_switch_while_planning_is_no_approval_to_retry(mesh, fake, run, monkeypatch, capsys):
    planning_worker(mesh, run, model="gemini-4-argon-high")
    mesh.record(run, "repo-fix-typo", "repo-lead", "question", "Keep _db.py?")
    with_quotas(mesh, monkeypatch)
    stub_new_pane(fake)
    fake.on("agent", "start", errors=["cli_error"] * 20)
    fake.on("pane", "layout", result={"layout": {"panes": [{"pane_id": "w9:p10", "rect": {"width": 200, "height": 50}}]}})
    fake.on("pane", "split", result={"pane": {"pane_id": "w9:p11"}})
    for answered in (False, True):
        if answered:
            mesh.record(run, "repo-lead", "repo-fix-typo", "answer", "Yes.")
        assert mesh.main(["switch", "repo-fix-typo"]) == 1
        entry = mesh.load_run(run)["agents"]["repo-fix-typo"]
        assert (entry["status"], entry["phase"]) == ("swap_failed", "plan")
        capsys.readouterr()
        assert mesh.main(["approve", "repo-fix-typo"]) == 1
        assert "has no plan waiting" in capsys.readouterr().err
    assert mesh.read_log(run)[-1]["why"] == "quota"


def test_watch_stops_excusing_a_worker_that_never_comes_back(mesh, fake, run, capsys):
    planning_worker(mesh, run)
    with mesh.editing_run(run) as data:
        data["agents"]["repo-fix-typo"]["status"] = "starting"
    fake.on("agent", "get", errors=["agent_not_found"] * 10)
    assert mesh.main(["watch", "repo-fix-typo", "--run-id", run, "--interval", "100"]) == 5
    assert len(fake.called("agent", "get", "repo-fix-typo")) == 4  # 3 polls excused (300s), the 4th alerts


def test_quota_hit_reads_only_new_bytes_and_ignores_plain_rate_limits(mesh, tmp_path):
    log = tmp_path / "w.log"
    log.write_text("E1 RESOURCE_EXHAUSTED (code 429): too many requests, retry in 5s\n")
    reset, offset = mesh.quota_hit(str(log), 0)
    assert reset is None and offset == log.stat().st_size
    with open(log, "a") as fh:
        fh.write("E2 RESOURCE_EXHAUSTED (code 429): Individual quota reached. Resets in 2h.\n")
    assert mesh.quota_hit(str(log), offset)[0] == "2h"
    assert mesh.quota_hit(str(log), log.stat().st_size)[0] is None  # already seen
    assert mesh.quota_hit(None, 7) == (None, 7)


def test_watch_follows_a_log_that_restarts_shorter(mesh, fake, run, monkeypatch, capsys):
    log = planning_worker(mesh, run)
    log.write_text("x" * 500 + "\n")
    fake.on("agent", "get", result={"agent": {"agent_status": "working"}})
    polls = iter(range(10))

    def sleep(_):
        n = next(polls)
        if n == 0:
            log.write_text("fresh log\n")  # a reopened agy truncated it
        elif n == 1:
            with open(log, "a") as fh:
                fh.write("E1 RESOURCE_EXHAUSTED (code 429): Individual quota reached. Resets in 3h.\n")

    monkeypatch.setattr(mesh.time, "sleep", sleep)
    assert mesh.main(["watch", "repo-fix-typo", "--run-id", run]) == 7
    assert "resets in 3h" in capsys.readouterr().out


def test_switch_never_returns_to_a_model_that_already_ran_out(mesh, fake, run, monkeypatch, capsys):
    planning_worker(mesh, run, model="gemini-4-argon-high")
    with_quotas(mesh, monkeypatch)
    monkeypatch.setattr(mesh, "agy_usage", lambda force=False: None)  # quotas unreadable: everything looks free
    stub_new_pane(fake)
    stub_split(fake)
    capsys.readouterr()
    assert mesh.main(["switch", "repo-fix-typo"]) == 0
    assert json.loads(capsys.readouterr().out)["model"] == "claude-opus-5-5-high"
    assert mesh.main(["switch", "repo-fix-typo"]) == 0  # Opus ran out too: not back to Argon
    assert json.loads(capsys.readouterr().out)["model"] == "gemini-3.8-flash-high"


def test_switch_goes_ahead_when_the_stalled_worker_never_goes_idle(mesh, fake, run, monkeypatch, capsys):
    planning_worker(mesh, run, model="gemini-4-argon-high")
    with_quotas(mesh, monkeypatch)
    stub_new_pane(fake)
    fake.on("agent", "wait", error="timeout")
    assert mesh.main(["switch", "repo-fix-typo"]) == 0
    assert fake.called("agent", "wait")[0][-2:] == ("--timeout", "60000")
    assert fake.called("agent", "start")


def test_approve_does_not_cut_a_busy_worker_short(mesh, fake, run, monkeypatch, capsys):
    planning_worker(mesh, run)
    with_quotas(mesh, monkeypatch)
    fake.on("agent", "wait", error="timeout")
    assert mesh.main(["approve", "repo-fix-typo"]) == 1
    assert not fake.called("pane", "close") and not fake.called("agent", "start")
    assert mesh.load_run(run)["agents"]["repo-fix-typo"]["phase"] == "plan"


def test_approve_suggests_keep_model_when_the_swap_cannot_start(mesh, fake, run, monkeypatch, capsys):
    planning_worker(mesh, run, session=None)  # agy has no conversation for it on record
    with_quotas(mesh, monkeypatch)
    assert mesh.main(["approve", "repo-fix-typo"]) == 1
    err = capsys.readouterr().err
    assert "can't find repo-fix-typo's agy conversation" in err
    assert "mesh approve repo-fix-typo --keep-model" in err and "mesh send" not in err
    entry = mesh.load_run(run)["agents"]["repo-fix-typo"]
    assert entry["phase"] == "plan" and mesh.unread(run, "repo-fix-typo") == []

    fake.on("agent", "wait", error="timeout")  # a busy worker: same hint
    planning_worker(mesh, run)
    assert mesh.main(["approve", "repo-fix-typo"]) == 1
    assert "--keep-model" in capsys.readouterr().err


def test_approve_does_not_suggest_keep_model_once_the_pane_is_gone(mesh, fake, run, monkeypatch, capsys):
    planning_worker(mesh, run)
    with_quotas(mesh, monkeypatch)
    stub_new_pane(fake)
    fake.on("agent", "start", errors=["cli_error"] * 10)
    assert mesh.main(["approve", "repo-fix-typo"]) == 1
    err = capsys.readouterr().err
    assert "then retry" in err and "--keep-model" not in err
    assert mesh.load_run(run)["agents"]["repo-fix-typo"]["status"] == "swap_failed"


def test_approve_keep_model_executes_where_it_planned(mesh, fake, run, monkeypatch, capsys):
    planning_worker(mesh, run, model="gemini-4-argon-high", session=None)
    with_quotas(mesh, monkeypatch)
    capsys.readouterr()
    assert mesh.main(["approve", "repo-fix-typo", "--keep-model"]) == 0
    out = json.loads(capsys.readouterr().out)
    assert out["kept_model"] is True and out["model"] == "gemini-4-argon-high"
    assert not fake.called("agent", "start") and not fake.called("pane", "close")
    assert bare(fake.called("agent", "prompt")[-1][3]).startswith("📬 [mesh] answer from repo-lead: approved")
    entry = mesh.load_run(run)["agents"]["repo-fix-typo"]
    assert (entry["phase"], entry["model"], entry["effort"]) == ("exec", "gemini-4-argon-high", "high")
    assert entry["models"][-1]["why"] == "plan approved" and entry["models"][-1]["kept"] is True

    # executing, so a quota hit moves it to the exec model, not down the plan chain
    with mesh.editing_run(run) as data:
        data["agents"]["repo-fix-typo"]["session"] = "c1"
    stub_new_pane(fake)
    assert mesh.main(["switch", "repo-fix-typo"]) == 0
    assert json.loads(capsys.readouterr().out)["model"] == "gemini-3.8-flash-medium"


def small_asker(mesh, run, **extra):
    with mesh.editing_run(run) as data:  # one model, no plan phase
        data["agents"]["repo-fix-typo"].update({
            "kind": "agy", "size": "small", "model": "gemini-3.8-flash-low", "effort": "low", "phase": None,
            "exec_model": None,
            "exec_effort": None, "session": "c1", "cwd": "/wt/fix", "repo": "/repo", "workspace": "w9",
            "models": [{"model": "gemini-3.8-flash-low", "why": "spawn"}], **extra})


def test_a_worker_with_no_plan_phase_enters_one_when_it_sends_a_plan(mesh, fake, run, monkeypatch, capsys):
    small_asker(mesh, run)
    # the root's task changes nothing: a fix the worker just does keeps its phase
    assert mesh.main(["send", "repo-fix-typo", "--type", "task", "--body", "# Task: add retries"]) == 0
    assert mesh.load_run(run)["agents"]["repo-fix-typo"]["phase"] is None
    as_worker(monkeypatch, run)
    assert mesh.main(["inbox"]) == 0
    for mtype in ("question", "done"):
        assert mesh.main(["send", "parent", "--type", mtype, "--body", "x"]) == 0
        assert mesh.load_run(run)["agents"]["repo-fix-typo"]["phase"] is None
    assert mesh.main(["send", "parent", "--type", "plan", "--body", "# Plan: retries"]) == 0
    entry = mesh.load_run(run)["agents"]["repo-fix-typo"]
    assert (entry["phase"], entry["exec_model"], entry["exec_effort"]) == ("plan", "flash", "medium")
    assert entry["model"] == "gemini-3.8-flash-low" and entry["models"][-1]["why"] == "planned task"
    assert len(mesh.unread(run, "repo-lead")) == 3  # stored first

    monkeypatch.delenv("MESH_RUN")
    monkeypatch.delenv("MESH_SELF")
    with_quotas(mesh, monkeypatch)
    stub_new_pane(fake)
    capsys.readouterr()
    assert mesh.main(["approve", "repo-fix-typo"]) == 0
    assert json.loads(capsys.readouterr().out)["model"] == "gemini-3.8-flash-medium"
    assert mesh.load_run(run)["agents"]["repo-fix-typo"]["phase"] == "exec"


def test_a_pinned_or_user_reviewed_worker_never_gets_an_exec_phase(mesh, fake, run, monkeypatch, capsys):
    as_worker(monkeypatch, run)
    for extra in ({"pinned": True}, {"plan_review": "user", "kind": "claude", "model": "opus"}):
        small_asker(mesh, run, **extra)
        assert mesh.main(["send", "parent", "--type", "plan", "--body", "# Plan"]) == 0
        entry = mesh.load_run(run)["agents"]["repo-fix-typo"]
        assert (entry["phase"], entry["exec_model"]) == (None, None)


def test_an_entry_from_before_pinned_is_inferred(mesh):
    worker = {"role": "worker", "kind": "agy"}
    assert not mesh.has_phases({**worker, "size": "large", "exec_model": None})  # spawned with a named --model
    assert mesh.has_phases({**worker, "size": "large", "exec_model": "flash"})  # auto
    assert mesh.has_phases({**worker, "size": "small", "exec_model": None})  # its size's defaults
    assert mesh.has_phases({**worker, "size": "large", "exec_model": None, "resumed_from": {"run": "m1"}})
    assert mesh.has_phases({**worker, "size": "large", "exec_model": None, "pinned": False})
    assert not mesh.has_phases({**worker, "size": "small", "pinned": True})


def test_the_plan_phase_is_recorded_before_the_doorbell_rings(mesh, fake, run, monkeypatch, capsys):
    small_asker(mesh, run)
    as_worker(monkeypatch, run)
    seen = []

    def ring(targets, text, token, read=(), delays=()):
        seen.append(mesh.load_run(run)["agents"]["repo-fix-typo"]["phase"])
        return {"rung": True, "error": None, "via": "working", "tries": 1}

    monkeypatch.setattr(mesh, "ring", ring)
    assert mesh.main(["send", "parent", "--type", "plan", "--body", "# Plan"]) == 0
    assert seen == ["plan"]


def test_spawn_records_a_named_model_or_effort(mesh, fake, run, repo, monkeypatch, capsys):
    with_quotas(mesh, monkeypatch)
    stub_new_pane(fake)
    stub_split(fake)
    assert mesh.main(["spawn", "--kind", "agy", "--model", "argon", "--repo", str(repo), "--worktree", "a"]) == 0
    assert mesh.main(["spawn", "--kind", "agy", "--repo", str(repo), "--worktree", "b"]) == 0
    assert mesh.main(["spawn", "--kind", "claude", "--size", "small", "--effort", "high", "--repo", str(repo),
                      "--worktree", "c"]) == 0
    assert mesh.main(["spawn", "--kind", "claude", "--size", "small", "--repo", str(repo), "--worktree", "d"]) == 0
    agents = mesh.load_run(run)["agents"]
    assert [agents[f"proj-{w}"]["pinned"] for w in "abcd"] == [True, False, True, False]


def test_resume_with_a_named_effort_pins_the_worker(mesh, fake, run, repo, monkeypatch, capsys):
    archived_worker(mesh, repo, session="c1")
    with_quotas(mesh, monkeypatch)
    stub_new_pane(fake)
    assert mesh.main(["resume", "proj-fix", "--effort", "high", "--task", "Apply the review."]) == 0
    entry = mesh.load_run(run)["agents"]["proj-fix"]
    assert (entry["pinned"], entry["phase"]) == (True, None)


def test_a_sized_reviewers_own_effort_is_not_a_named_one(mesh, fake, run, repo, tmp_path, capsys):
    pr_origin(tmp_path, repo)
    stub_new_pane(fake)
    pr = "https://github.com/acme/proj/pull/7"
    assert mesh.main(["ask", "--repo", str(repo), "--kind", "claude", "--size", "small", "--review", pr,
                      "--no-wait"]) == 0
    name = json.loads(capsys.readouterr().out)["name"]
    entry = mesh.load_run(run)["agents"][name]
    assert entry["effort"] == "high" and entry["pinned"] is False


def test_switch_while_executing_uses_the_exec_effort(mesh, fake, run, monkeypatch, capsys):
    planning_worker(mesh, run, model="gemini-4-argon-high", phase="exec")
    with mesh.editing_run(run) as data:
        data["agents"]["repo-fix-typo"]["exec_effort"] = "medium"
    with_quotas(mesh, monkeypatch)
    stub_new_pane(fake)
    capsys.readouterr()
    assert mesh.main(["switch", "repo-fix-typo"]) == 0
    assert json.loads(capsys.readouterr().out)["model"] == "gemini-3.8-flash-medium"


def test_an_agy_workers_conversation_is_recorded_with_its_first_message(mesh, fake, run, tmp_path, monkeypatch):
    with mesh.editing_run(run) as data:
        data["agents"]["repo-fix-typo"].update(kind="agy", cwd="/wt/fix", session=None)
    db = agy_summaries_db(tmp_path / "agy" / "conversation_summaries.db", [
        ("other", "Agent Mesh Setup", "/wt/fix", "2026-10-02 09:00:00+00:00"),
    ])
    as_worker(monkeypatch, run)
    assert mesh.main(["send", "parent", "--type", "question", "--body", "Which file?"]) == 0
    assert mesh.load_run(run)["agents"]["repo-fix-typo"]["session"] is None  # a worktree match isn't enough
    db.execute("INSERT INTO conversation_summaries (conversation_id, preview, step_count, last_modified_time, "
               "workspace_uris, last_user_input_time) VALUES (?,?,?,?,?,?)",
               ("mine", "📬 [mesh] You are repo-fix-typo (worker) in agent-mesh run mtest; …", 3,
                "2026-10-02 10:00:00+00:00", json.dumps([Path("/wt/fix").as_uri()]), "2026-10-02 10:00:00+00:00"))
    db.commit()
    assert mesh.main(["send", "parent", "--type", "plan", "--body", "# Plan"]) == 0
    assert mesh.load_run(run)["agents"]["repo-fix-typo"]["session"] == "mine"
    db.execute("UPDATE conversation_summaries SET preview = 'Agent Mesh Setup'")  # agy's title replaces it later
    db.commit()
    assert mesh.find_session(run, "repo-fix-typo", mesh.load_run(run)["agents"]["repo-fix-typo"]) == "mine"
    db.close()


def test_a_herdr_failure_while_waiting_is_not_a_busy_worker(mesh, fake, run, monkeypatch, capsys):
    planning_worker(mesh, run)
    with_quotas(mesh, monkeypatch)
    fake.on("agent", "wait", error="cli_error")
    assert mesh.main(["approve", "repo-fix-typo"]) == 1
    err = capsys.readouterr().err
    assert "herdr cli_error" in err and "--keep-model" not in err


def test_agy_index_lookup_survives_a_relative_path_and_no_sqlite(mesh, tmp_path, monkeypatch):
    agy_summaries_db(tmp_path / "agy" / "conversation_summaries.db", [
        ("c1", "📬 [mesh] You are proj-fix (worker) in agent-mesh run mold; …", "/wt/fix", "2026-10-02 10:00:00+00:00"),
    ]).close()
    monkeypatch.chdir(tmp_path)
    monkeypatch.setenv("MESH_AGY_SUMMARIES", "agy/conversation_summaries.db")
    assert mesh.agy_conversation("proj-fix", "mold", None) == "c1"
    mesh.agy_summaries.cache_clear()
    monkeypatch.setitem(__import__("sys").modules, "sqlite3", None)  # a Python built without it
    assert mesh.agy_conversation("proj-fix", "mold", None) is None


def test_the_new_model_doorbell_survives_a_failed_ring(mesh, fake, run, monkeypatch, capsys):
    planning_worker(mesh, run)
    with_quotas(mesh, monkeypatch)
    stub_new_pane(fake)
    fake.on("agent", "prompt", errors=["agent_blocked"] * 7)  # every attempt of the first ring
    assert mesh.main(["approve", "repo-fix-typo"]) == 2
    assert mesh.load_run(run)["agents"]["repo-fix-typo"]["swapped"] is True
    assert mesh.main(["send", "repo-fix-typo", "--type", "info", "--body", "ping"]) == 0
    assert bare(fake.called("agent", "prompt")[-1][3]).startswith(
        "📬 [mesh] You are repo-fix-typo (worker) again in agent-mesh run mtest, now on gemini-3.8-flash-medium at medium effort")
    assert "swapped" not in mesh.load_run(run)["agents"]["repo-fix-typo"]


def test_a_failed_usage_read_is_not_retried_for_a_minute(mesh, monkeypatch):
    calls = []

    def fail(argv, **kwargs):
        calls.append(argv)
        return subprocess.CompletedProcess(argv, 1, stdout="", stderr="offline")

    monkeypatch.setattr(mesh.subprocess, "run", fail)
    assert mesh.agy_usage() is None and mesh.agy_usage() is None
    assert len(calls) == 1
    assert mesh.agy_usage(force=True) is None and len(calls) == 2


# ── review fixes: effort overrides, exec model checked at spawn ──────────


def test_a_named_effort_covers_both_phases(mesh, fake, run, repo, monkeypatch, capsys):
    fake.on("workspace", "list", result={"workspaces": [{"workspace_id": "w9", "label": "proj"}]})
    fake.on("tab", "create", result={"tab": {"tab_id": "w9:t4"}, "root_pane": {"pane_id": "w9:p10"}})
    with_quotas(mesh, monkeypatch)
    assert mesh.main(["spawn", "--kind", "claude", "--repo", str(repo), "--name", "a", "--effort", "max"]) == 0
    entry = mesh.load_run(run)["agents"]["proj-a"]
    assert (entry["effort"], entry["exec_effort"]) == ("max", "max")
    stub_split(fake)
    assert mesh.main(["spawn", "--kind", "claude", "--repo", str(repo), "--name", "b",
                      "--effort", "max", "--exec-effort", "low"]) == 0
    assert mesh.load_run(run)["agents"]["proj-b"]["exec_effort"] == "low"


def test_spawn_checks_the_exec_model_before_any_side_effect(mesh, fake, run, repo, monkeypatch, capsys):
    with_quotas(mesh, monkeypatch)
    agents = set(mesh.load_run(run)["agents"])
    capsys.readouterr()
    assert mesh.main(["spawn", "--repo", str(repo), "--worktree", "w", "--exec-effort", "max"]) == 1
    assert "effort=max" in capsys.readouterr().err
    assert not fake.called("tab", "create") and set(mesh.load_run(run)["agents"]) == agents
    assert mesh.main(["spawn", "--repo", str(repo), "--model", "argon", "--exec-effort", "low"]) == 1
    assert "needs an execution phase" in capsys.readouterr().err
    assert not fake.called("tab", "create")


def test_switch_to_an_exact_id_records_its_own_effort(mesh, fake, run, monkeypatch, capsys):
    planning_worker(mesh, run, model="gemini-3.8-flash-medium", phase="exec")
    with mesh.editing_run(run) as data:
        data["agents"]["repo-fix-typo"]["effort"] = "medium"
    with_quotas(mesh, monkeypatch)
    stub_new_pane(fake)
    assert mesh.main(["switch", "repo-fix-typo", "--model", "gemini-3.8-flash-high"]) == 0
    entry = mesh.load_run(run)["agents"]["repo-fix-typo"]
    assert (entry["model"], entry["effort"]) == ("gemini-3.8-flash-high", "high")


def test_effort_flags_reject_unknown_values(mesh, capsys):
    with pytest.raises(SystemExit):
        mesh.main(["usage", "--exec-effort", "med"])


# ── ask: questions and reviews ───────────────────────────────────────────


def answer_on_sleep(mesh, monkeypatch, run, worker, body="restockedAt is set in stock.py.", mtype="done"):
    """The worker answers the first time anything waits."""
    def sleep(_):
        if worker in mesh.load_run(run)["agents"] and not mesh.sends_from(run, worker, mesh.FINAL_TYPES):
            mesh.deliver(run, worker, "repo-lead", mtype, body)

    monkeypatch.setattr(mesh.time, "sleep", sleep)


def test_ask_slug_keeps_the_telling_words(mesh):
    assert mesh.ask_slug("How is restockedAt computed?") == "ask-restockedat"
    assert mesh.ask_slug("What does the webhook route return on 404") == "ask-webhook-route"
    assert mesh.ask_slug("???") == "ask"


def test_ask_starts_a_fresh_worker_and_prints_its_answer(mesh, fake, run, repo, monkeypatch, capsys):
    stub_new_pane(fake)
    fake.on("agent", "get", result={"agent": {"agent_status": "working"}})
    with_quotas(mesh, monkeypatch)
    answer_on_sleep(mesh, monkeypatch, run, "proj-ask-restockedat")
    assert mesh.main(["ask", "--repo", str(repo), "--body", "How is restockedAt computed?"]) == 0
    captured = capsys.readouterr()
    assert "── done from proj-ask-restockedat ──\nrestockedAt is set in stock.py." in captured.out
    assert "mesh ask proj-ask-restockedat" in captured.out
    assert "started proj-ask-restockedat" in captured.err
    entry = mesh.load_run(run)["agents"]["proj-ask-restockedat"]
    # an ordinary worker on auto: a later change task plans, and approve moves it to Flash
    assert (entry["role"], entry["branch"], entry["phase"], entry["exec_model"]) == (
        "worker", "mesh/mtest/ask-restockedat", "plan", "flash")
    [task] = (mesh.inbox_dir(run, "proj-ask-restockedat")).glob("*-task.md")
    assert mesh.parse_message(task)[1].startswith(mesh.ASK_PREFIX + "\n\nHow is restockedAt computed?")
    doorbell = fake.called("agent", "prompt")[0][3]
    assert doorbell.startswith("📬 [mesh] You are proj-ask-restockedat (worker) in agent-mesh run mtest")
    assert not mesh.unread(run, "repo-lead")  # the answer was printed and marked read


def test_ask_closes_the_agent_it_opened_once_it_answers(mesh, fake, run, repo, monkeypatch, capsys):
    stub_new_pane(fake)
    fake.on("agent", "get", result={"agent": {"agent_status": "working"}})
    with_quotas(mesh, monkeypatch)
    answer_on_sleep(mesh, monkeypatch, run, "proj-ask-restockedat")
    assert mesh.main(["ask", "--repo", str(repo), "--body", "How is restockedAt computed?"]) == 0
    assert "proj-ask-restockedat is closed" in capsys.readouterr().out
    entry = mesh.load_run(run)["agents"]["proj-ask-restockedat"]
    assert entry["status"] == "retired" and fake.called("pane", "close", entry["pane"])

    # asking again reopens the same conversation in this run, under the same name
    with mesh.editing_run(run) as data:
        data["agents"]["proj-ask-restockedat"]["session"] = "c9"
    assert mesh.main(["ask", "proj-ask-restockedat", "--body", "When?", "--keep", "--no-wait"]) == 0
    assert json.loads(capsys.readouterr().out)["name"] == "proj-ask-restockedat"
    assert fake.called("agent", "start")[-1][-5:-2] == ("--conversation", "c9", "--model")


def test_retire_lets_the_turn_end_and_copes_with_a_pane_that_never_opened(mesh, fake, run, capsys):
    assert mesh.main(["retire", "repo-fix-typo"]) == 0
    wait = fake.called("agent", "wait", "repo-fix-typo")
    assert wait and fake.calls.index(wait[0]) < fake.calls.index(fake.called("pane", "close", "w2:p5")[0])
    with mesh.editing_run(run) as data:
        data["agents"]["proj-broken"] = {"role": "worker", "kind": "agy", "pane": None, "parent": "repo-lead",
                                         "status": "starting"}
    assert mesh.main(["retire", "proj-broken"]) == 0
    assert mesh.load_run(run)["agents"]["proj-broken"]["status"] == "retired"


def test_ask_leaves_a_worker_that_was_already_open(mesh, fake, run, monkeypatch, capsys):
    fake.on("agent", "get", result={"agent": {"agent_status": "idle"}})
    answer_on_sleep(mesh, monkeypatch, run, "repo-fix-typo", body="Because.")
    assert mesh.main(["ask", "repo-fix-typo", "--body", "Why ISO dates?"]) == 0
    assert "Because." in capsys.readouterr().out
    assert mesh.load_run(run)["agents"]["repo-fix-typo"]["status"] != "retired"
    assert not fake.called("pane", "close")


def test_ask_a_live_worker_sends_it_the_question(mesh, fake, run, monkeypatch, capsys):
    fake.on("agent", "get", result={"agent": {"agent_status": "idle"}})
    assert mesh.main(["ask", "repo-fix-typo", "--body", "Why ISO dates?", "--no-wait"]) == 0
    assert json.loads(capsys.readouterr().out)["name"] == "repo-fix-typo"
    assert not fake.called("tab", "create") and not fake.called("agent", "start")
    [task] = mesh.unread(run, "repo-fix-typo")
    assert mesh.parse_message(task)[1].startswith("Question (read-only)")
    assert bare(fake.called("agent", "prompt")[-1][3]).startswith("📬 [mesh] task from repo-lead: Question (read-only)")


def test_ask_a_past_worker_resumes_its_conversation(mesh, fake, run, repo, monkeypatch, capsys):
    archived_worker(mesh, repo, session="c1")
    stub_new_pane(fake)
    fake.on("agent", "get", result={"agent": {"agent_status": "working"}})
    answer_on_sleep(mesh, monkeypatch, run, "proj-fix", body="Because the API spec says so.")
    assert mesh.main(["ask", "https://github.com/acme/proj/pull/7", "--body", "Why ISO dates?"]) == 0
    assert "Because the API spec says so." in capsys.readouterr().out
    assert fake.called("agent", "start")[-1][-5:-2] == ("--conversation", "c1", "--model")
    assert mesh.load_run(run)["agents"]["proj-fix"]["resumed_from"] == {"run": "mold", "name": "proj-fix"}


def test_a_past_claude_worker_is_held_read_only_for_a_question_but_not_for_a_change(mesh, fake, run, repo,
                                                                                      monkeypatch, capsys):
    archived_worker(mesh, repo, kind="claude", session="c1")
    stub_new_pane(fake)
    assert mesh.main(["ask", "proj-fix", "--body", "Why ISO dates?", "--no-wait"]) == 0
    assert "Bash(git push:*)" in deny_rules(fake.called("agent", "start")[-1])
    assert mesh.load_run(run)["agents"]["proj-fix"]["read_only"] is True
    with mesh.editing_run(run) as data:
        data["agents"]["proj-fix"]["status"] = "retired"
    fake.on("agent", "get", error="agent_not_found")
    assert mesh.main(["resume", "proj-fix", "--task", "Direct task (no plan)\n\nUse ISO dates."]) == 0
    assert not deny_rules(fake.called("agent", "start")[-1])
    assert mesh.load_run(run)["agents"]["proj-fix"]["read_only"] is False


def test_ask_without_a_run_starts_one(mesh, fake, repo, monkeypatch, capsys):
    monkeypatch.setenv("HERDR_ENV", "1")
    monkeypatch.setenv("HERDR_PANE_ID", "w1:p1")
    fake.on("agent", "list", result={"agents": [{"pane_id": "w1:p1", "agent": "claude", "cwd": "/repo"}]})
    stub_new_pane(fake)
    with_quotas(mesh, monkeypatch)
    assert mesh.main(["ask", "--repo", str(repo), "--kind", "claude", "--body", "Where is auth?", "--no-wait"]) == 0
    captured = capsys.readouterr()
    run_id = json.loads(captured.out)["delivery"]["stored"].split("/state/")[1].split("/")[0]
    assert f"started run {run_id}" in captured.err
    data = mesh.load_run(run_id)
    assert (data["mode"], data["request"]) == ("claude", "Where is auth?")
    assert data["agents"]["proj-ask-auth"]["kind"] == "claude"


def test_ask_needs_one_target_and_a_question(mesh, fake, run, repo, capsys):
    assert mesh.main(["ask", "--body", "x"]) == 1
    assert mesh.main(["ask", "repo-fix-typo", "--repo", str(repo), "--body", "x"]) == 1
    assert mesh.main(["ask", "repo-fix-typo"]) == 1
    assert mesh.main(["ask", "repo-fix-typo", "--kind", "claude", "--body", "x"]) == 1
    assert "empty question" in capsys.readouterr().err
    assert mesh.main(["ask", "--repo", str(repo), "--review", "7"]) == 1  # no GitHub remote to read a number against
    assert "not on GitHub" in capsys.readouterr().err
    assert not fake.called("agent", "start")


def pr_origin(tmp_path, repo):
    """An origin for `repo` with a pull request head at refs/pull/7/head, as GitHub serves it."""
    origin = tmp_path / "origin"
    subprocess.run(["git", "clone", "-q", str(repo), str(origin)], check=True)
    for cmd in (["config", "user.email", "t@t"], ["config", "user.name", "t"],
                ["commit", "-q", "--allow-empty", "-m", "the change under review"]):
        subprocess.run(["git", *cmd], cwd=origin, check=True)
    head = subprocess.run(["git", "rev-parse", "HEAD"], cwd=origin, capture_output=True, text=True).stdout.strip()
    subprocess.run(["git", "update-ref", "refs/pull/7/head", head], cwd=origin, check=True)
    subprocess.run(["git", "remote", "add", "origin", str(origin)], cwd=repo, check=True)
    return head


def test_ask_for_a_review_checks_out_the_pr_head(mesh, fake, run, repo, tmp_path, monkeypatch, capsys):
    head = pr_origin(tmp_path, repo)
    stub_new_pane(fake)
    with_quotas(mesh, monkeypatch)
    pr = "https://github.com/acme/proj/pull/7"
    assert mesh.main(["ask", "--repo", str(repo), "--review", pr, "--no-wait"]) == 0
    out = json.loads(capsys.readouterr().out)
    assert out["name"] == "proj-review-7-agy"
    wt_head = subprocess.run(["git", "rev-parse", "HEAD"], cwd=out["cwd"], capture_output=True, text=True).stdout
    assert wt_head.strip() == head
    entry = mesh.load_run(run)["agents"]["proj-review-7-agy"]
    assert (entry["reviews"], entry["base"]) == ([pr], head)
    [task] = mesh.unread(run, "proj-review-7-agy")
    body = mesh.parse_message(task)[1]
    assert body.startswith(f"Review (read-only) of {pr} at {head}:") and "checked out in your worktree" in body
    assert "**blocking**" in body and "**advisory**" in body and "/code-review" not in body  # an agy reviewer
    assert entry["review_heads"] == {pr: head}


def test_a_second_fresh_reviewer_of_a_pr_gets_its_own_branch(mesh, fake, run, repo, tmp_path, capsys):
    pr_origin(tmp_path, repo)
    stub_new_pane(fake)
    pr = "https://github.com/acme/proj/pull/7"
    assert mesh.main(["ask", "--repo", str(repo), "--kind", "claude", "--review", pr, "--no-wait"]) == 0
    first = json.loads(capsys.readouterr().out)
    fake.on("pane", "layout", result={"layout": {"panes": [{"pane_id": "w9:p10", "rect": {"width": 200, "height": 50}}]}})
    fake.on("pane", "split", result={"pane": {"pane_id": "w9:p11"}})
    assert mesh.main(["ask", "--repo", str(repo), "--kind", "claude", "--review", pr, "--no-wait"]) == 0
    second = json.loads(capsys.readouterr().out)  # the first one's branch is still there, as a retired one's is
    assert (first["branch"], second["branch"]) == ("mesh/mtest/review-7-claude", "mesh/mtest/review-7-claude-2")
    assert second["name"] != first["name"]


def test_a_named_reviewer_whose_branch_is_taken_gets_the_next_one(mesh, fake, run, repo, tmp_path, capsys):
    pr_origin(tmp_path, repo)
    stub_new_pane(fake)
    pr = "https://github.com/acme/proj/pull/7"
    named = ["ask", "--repo", str(repo), "--kind", "claude", "--review", pr, "--name", "review-7-claude", "--no-wait"]
    assert mesh.main(named) == 0  # /delegate-review always passes --name
    capsys.readouterr()
    fake.on("pane", "layout", result={"layout": {"panes": [{"pane_id": "w9:p10", "rect": {"width": 200, "height": 50}}]}})
    fake.on("pane", "split", result={"pane": {"pane_id": "w9:p11"}})
    assert mesh.main(named) == 0
    out = json.loads(capsys.readouterr().out)
    assert (out["branch"], out["name"]) == ("mesh/mtest/review-7-claude-2", "proj-review-7-claude-2")


def test_reviewers_started_at_the_same_moment_get_different_slugs(mesh, fake, run, repo, monkeypatch):
    import threading
    real, started = mesh.git, threading.Barrier(2)

    def git(*args, **kw):  # both look before either has made its branch
        if args[:2] == ("rev-parse", "--verify") and started.n_waiting < 2 and not started.broken:
            try:
                started.wait(timeout=0.5)
            except threading.BrokenBarrierError:
                pass
        return real(*args, **kw)

    monkeypatch.setattr(mesh, "git", git)
    slugs = []
    threads = [threading.Thread(target=lambda: slugs.append(mesh.free_slug(run, repo, "review-7-claude")))
               for _ in range(2)]
    for t in threads:
        t.start()
    for t in threads:
        t.join()
    assert sorted(slugs) == ["review-7-claude", "review-7-claude-2"]
    assert mesh.load_run(run)["slugs"] == {str(repo): ["review-7-claude", "review-7-claude-2"]}


def test_a_long_slugs_suffix_survives_into_the_agent_name(mesh, fake, run, repo):
    first, second = (mesh.free_slug(run, repo, "review-12345-copilot") for _ in range(2))
    assert second == "review-12345-copil-2"
    assert mesh.agent_name("agent-mesh", first, set()) != mesh.agent_name("agent-mesh", second, set())


def test_a_fresh_reviewer_gets_high_effort_at_any_size(mesh, fake, run, repo, tmp_path, capsys):
    pr_origin(tmp_path, repo)
    stub_new_pane(fake)
    pr = "https://github.com/acme/proj/pull/7"
    assert mesh.main(["ask", "--repo", str(repo), "--kind", "claude", "--size", "small", "--review", pr,
                      "--no-wait"]) == 0
    assert json.loads(capsys.readouterr().out)["effort"] == "high"
    assert started(fake)[-5:-1] == ("--model", "opus", "--effort", "high")
    fake.on("pane", "layout", result={"layout": {"panes": [{"pane_id": "w9:p10", "rect": {"width": 200, "height": 50}}]}})
    fake.on("pane", "split", result={"pane": {"pane_id": "w9:p11"}})
    assert mesh.main(["ask", "--repo", str(repo), "--kind", "claude", "--size", "small", "--review", pr,
                      "--effort", "medium", "--name", "review-again", "--no-wait"]) == 0
    assert json.loads(capsys.readouterr().out)["effort"] == "medium"  # a named effort wins
    assert mesh.main(["ask", "--repo", str(repo), "--kind", "claude", "--size", "small", "--body", "Where is auth?",
                      "--no-wait"]) == 0
    assert json.loads(capsys.readouterr().out)["effort"] == "medium"  # a question keeps its size's effort


def test_a_trivial_reviewer_gets_high_effort_on_either_cli(mesh, fake, run, repo, tmp_path, monkeypatch, capsys):
    pr_origin(tmp_path, repo)
    stub_new_pane(fake)
    monkeypatch.setattr(mesh, "resolve_agy_model", lambda family, effort: f"gemini-3.8-{family}-{effort}")
    pr = "https://github.com/acme/proj/pull/7"
    assert mesh.main(["ask", "--repo", str(repo), "--kind", "agy", "--size", "trivial", "--review", pr,
                      "--name", "review-agy", "--no-wait"]) == 0
    assert json.loads(capsys.readouterr().out)["model"] == "gemini-3.8-flash-high"  # not its size's low
    fake.on("pane", "layout", result={"layout": {"panes": [{"pane_id": "w9:p10", "rect": {"width": 200, "height": 50}}]}})
    fake.on("pane", "split", result={"pane": {"pane_id": "w9:p11"}})
    assert mesh.main(["ask", "--repo", str(repo), "--kind", "claude", "--size", "trivial", "--review", pr,
                      "--name", "review-claude", "--no-wait"]) == 0
    out = json.loads(capsys.readouterr().out)
    assert (out["model"], out["effort"]) == ("opus", "high")


def test_a_large_reviewer_plans_high_and_still_executes_a_later_fix_at_medium(mesh, fake, run, repo, tmp_path,
                                                                              monkeypatch, capsys):
    pr_origin(tmp_path, repo)
    stub_new_pane(fake)
    with_quotas(mesh, monkeypatch)
    pr = "https://github.com/acme/proj/pull/7"
    for kind in ("claude", "agy"):
        if kind == "agy":
            fake.on("pane", "layout", result={"layout": {"panes": [{"pane_id": "w9:p10", "rect": {"width": 200, "height": 50}}]}})
            fake.on("pane", "split", result={"pane": {"pane_id": "w9:p11"}})
        assert mesh.main(["ask", "--repo", str(repo), "--kind", kind, "--review", pr, "--name", f"review-{kind}",
                          "--no-wait"]) == 0
        entry = mesh.load_run(run)["agents"][json.loads(capsys.readouterr().out)["name"]]
        assert (entry["effort"], entry["phase"], entry["exec_effort"]) == ("high", "plan", "medium"), kind


def test_a_review_task_carries_the_diff_unless_it_is_too_long(mesh, fake, run, capsys):
    fake.on("agent", "get", result={"agent": {"agent_status": "idle"}})
    pr = "https://github.com/acme/other/pull/3"
    mesh.run_cli.on("pr", "diff", stdout="--- a/x\n+++ b/x\n+fixed\n")
    assert mesh.main(["ask", "repo-fix-typo", "--review", pr, "--body", "Focus on auth.", "--no-wait"]) == 0
    body = mesh.parse_message(mesh.unread(run, "repo-fix-typo")[-1])[1]
    assert mesh.run_cli.called("pr", "diff", pr)
    assert "Focus on auth.\n\nThe change (`gh pr diff" in body and body.rstrip().endswith("+fixed\n````")

    mesh.run_cli.on("pr", "diff", stdout="+x\n" * mesh.REVIEW_DIFF_MAX)
    assert mesh.main(["ask", "repo-fix-typo", "--review", pr, "--no-wait"]) == 0
    body = mesh.parse_message(mesh.unread(run, "repo-fix-typo")[-1])[1]
    assert "too long to include" in body and "+x" not in body


def pr_on_a_branch(tmp_path, repo):
    """An origin for `repo` whose PR 7 changes fix.txt on a branch, while main has moved on (other.txt) since."""
    origin = tmp_path / "origin"
    subprocess.run(["git", "clone", "-q", str(repo), str(origin)], check=True)

    def run_git(*cmd):
        return subprocess.run(["git", *cmd], cwd=origin, check=True, capture_output=True, text=True).stdout.strip()
    run_git("config", "user.email", "t@t")
    run_git("config", "user.name", "t")
    run_git("checkout", "-q", "-b", "fix")
    (origin / "fix.txt").write_text("fixed\n")
    run_git("add", "fix.txt")
    run_git("commit", "-q", "-m", "the change under review")
    head = run_git("rev-parse", "HEAD")
    run_git("update-ref", "refs/pull/7/head", head)
    run_git("checkout", "-q", "main")
    (origin / "other.txt").write_text("not this PR's\n")
    run_git("add", "other.txt")
    run_git("commit", "-q", "-m", "main moves on")
    subprocess.run(["git", "remote", "add", "origin", str(origin)], cwd=repo, check=True)
    return origin, head


def test_a_review_diff_is_of_the_head_the_task_names(mesh, fake, cli, run, repo, tmp_path, monkeypatch, capsys):
    _, head = pr_on_a_branch(tmp_path, repo)
    stub_new_pane(fake)
    with_quotas(mesh, monkeypatch)
    pr = "https://github.com/acme/proj/pull/7"
    cli.on("pr", "view", stdout=json.dumps({"baseRefName": "main"}))
    cli.on("pr", "diff", stdout="+a newer push\n")  # gh shows the PR as it is now
    assert mesh.main(["ask", "--repo", str(repo), "--review", pr, "--no-wait"]) == 0
    name = json.loads(capsys.readouterr().out)["name"]
    body = mesh.parse_message(mesh.unread(run, name)[-1])[1]
    base = subprocess.run(["git", "rev-parse", "main"], cwd=repo, capture_output=True, text=True).stdout.strip()
    assert f"The change (`git diff {base}..{head}`):" in body  # from the merge base, not main's tip
    assert f"`git diff {base}..{head}` shows the change" in body  # the task's own words name it too
    assert "+fixed" in body and "other.txt" not in body and "newer push" not in body
    assert not cli.called("pr", "diff")


def test_a_review_diff_falls_back_to_gh_when_git_cannot_build_it(mesh, cli, repo, tmp_path, monkeypatch):
    _, head = pr_on_a_branch(tmp_path, repo)
    pr = "https://github.com/acme/proj/pull/7"
    assert mesh.pr_head(repo, pr, 7) == head
    cli.on("pr", "diff", stdout="+from gh\n")
    for view in ({"stdout": json.dumps({"baseRefName": "gone"})}, {"returncode": 1, "stderr": "not logged in"}):
        cli.on("pr", "view", **view)  # a base branch that can't be fetched, then gh can't name it
        diff = mesh.pr_diff(pr, head, mesh.head_diff(repo, pr, head))
        assert diff.startswith(f"The change (`gh pr diff {pr}`, the PR as it is now, which may be past {head}):")
        assert diff.rstrip().endswith("+from gh\n````")
    assert mesh.pr_diff(pr).startswith(f"The change (`gh pr diff {pr}`):")  # no head: gh, as before

    cli.on("pr", "view", stdout=json.dumps({"baseRefName": "main"}))
    monkeypatch.setattr(mesh, "REVIEW_DIFF_MAX", 3)  # the cap holds for a diff from git too
    diff = mesh.pr_diff(pr, head, mesh.head_diff(repo, pr, head))
    merge_base = mesh.git("merge-base", "main", head, cwd=repo)
    assert "too long to include" in diff and diff.endswith(f"read it with `git diff {merge_base}..{head}`.")


def test_a_merged_pr_falls_back_to_gh_for_its_diff(mesh, cli, repo, tmp_path):
    # A merge commit puts the head in its base: the merge base is the head itself, and git's diff is empty.
    origin, head = pr_on_a_branch(tmp_path, repo)
    subprocess.run(["git", "merge", "-q", "--no-ff", "-m", "merge the PR", "fix"], cwd=origin, check=True)
    pr = "https://github.com/acme/proj/pull/7"
    assert mesh.pr_head(repo, pr, 7) == head
    cli.on("pr", "view", stdout=json.dumps({"baseRefName": "main"}))
    cli.on("pr", "diff", stdout="+fixed\n")
    assert mesh.head_diff(repo, pr, head) is None
    task = mesh.ask_task("", pr, 7, fresh=True, head=head, kind="claude", repo=repo)
    assert f"The change (`gh pr diff {pr}`, the PR as it is now" in task and task.rstrip().endswith("+fixed\n````")
    assert "`/code-review high 7`" in task and f"{head}..{head}" not in task  # its remote is origin: the number


def test_fetching_a_pr_leaves_fetch_head_and_no_refs_behind(mesh, repo, tmp_path):
    _, head = pr_on_a_branch(tmp_path, repo)
    fetch_head = repo / ".git" / "FETCH_HEAD"
    fetch_head.write_text("the user's own\n")
    assert mesh.pr_head(repo, "https://github.com/acme/proj/pull/7", 7) == head
    assert fetch_head.read_text() == "the user's own\n"  # parallel asks can't read each other's fetch either
    assert not mesh.git("for-each-ref", "refs/mesh", cwd=repo)

def test_a_fork_review_diff_fetches_the_base_from_upstream(mesh, cli, repo, tmp_path, monkeypatch):
    _, head = pr_on_a_branch(tmp_path, repo)
    subprocess.run(["git", "remote", "rename", "origin", "upstream"], cwd=repo, check=True)
    subprocess.run(["git", "remote", "add", "origin", "git@github.com:me/proj.git"], cwd=repo, check=True)
    real_git = mesh.git

    def git(*args, cwd):  # upstream is a local stand-in for github.com/acme/proj
        if args == ("remote", "get-url", "upstream"):
            return "https://github.com/acme/proj.git"
        return real_git(*args, cwd=cwd)

    monkeypatch.setattr(mesh, "git", git)
    pr = "https://github.com/acme/proj/pull/7"
    assert mesh.pr_head(repo, pr, 7) == head
    cli.on("pr", "view", stdout=json.dumps({"baseRefName": "main"}))
    diff, span = mesh.head_diff(repo, pr, head)
    assert "+fixed" in diff and "other.txt" not in diff
    assert span == f"{mesh.git('merge-base', 'main', head, cwd=repo)}..{head}"


def test_a_live_worker_reviews_without_switching_its_branch(mesh, fake, run, monkeypatch, capsys):
    fake.on("agent", "get", result={"agent": {"agent_status": "idle"}})
    pr = "https://github.com/acme/other/pull/3"
    assert mesh.main(["ask", "repo-fix-typo", "--review", pr, "--body", "Focus on auth.", "--no-wait"]) == 0
    body = mesh.parse_message(mesh.unread(run, "repo-fix-typo")[0])[1]
    assert "Don't switch your branch" in body and "pull/3/head" in body and body.rstrip().endswith("Focus on auth.")
    assert mesh.load_run(run)["agents"]["repo-fix-typo"]["reviews"] == [pr]


def test_a_reviewer_asked_again_reviews_what_changed_since_the_head_it_read(mesh, fake, run, repo, tmp_path,
                                                                             monkeypatch, capsys):
    head = pr_origin(tmp_path, repo)
    fake.on("agent", "get", result={"agent": {"agent_status": "idle"}})
    pr = "https://github.com/acme/proj/pull/7"
    with mesh.editing_run(run) as data:
        data["agents"]["repo-fix-typo"].update(repo=str(repo), reviews=[pr], review_heads={pr: "0ld"})
    assert mesh.main(["ask", "repo-fix-typo", "--review", pr, "--no-wait"]) == 0
    body = mesh.parse_message(mesh.unread(run, "repo-fix-typo")[0])[1]
    assert body.startswith(f"Review (read-only) of {pr} at {head}:")
    assert "You last reviewed it at 0ld" in body and f"`git diff 0ld {head}`" in body
    assert mesh.load_run(run)["agents"]["repo-fix-typo"]["review_heads"] == {pr: head}


def fresh_reviewer_and_a_fix(mesh, fake, run, repo, tmp_path, monkeypatch, capsys):
    """A fresh reviewer of PR 7, then a fix pushed to the PR: (its name, its worktree, the old head, the new one)."""
    last = pr_origin(tmp_path, repo)
    stub_new_pane(fake)
    with_quotas(mesh, monkeypatch)
    pr = "https://github.com/acme/proj/pull/7"
    assert mesh.main(["ask", "--repo", str(repo), "--review", pr, "--no-wait"]) == 0
    out = json.loads(capsys.readouterr().out)
    origin = tmp_path / "origin"
    subprocess.run(["git", "commit", "-q", "--allow-empty", "-m", "the fix"], cwd=origin, check=True)
    head = subprocess.run(["git", "rev-parse", "HEAD"], cwd=origin, capture_output=True, text=True).stdout.strip()
    subprocess.run(["git", "update-ref", "refs/pull/7/head", head], cwd=origin, check=True)
    for path in mesh.unread(run, out["name"]):
        mesh.mark_read(path)
    return out["name"], Path(out["cwd"]), last, head


def worktree_head(cwd):
    return subprocess.run(["git", "rev-parse", "HEAD"], cwd=cwd, capture_output=True, text=True).stdout.strip()


def live_in_its_worktree(fake, name, cwd, status):
    """herdr shows the reviewer running in its worktree, in `status`."""
    fake.on("agent", "list", result={"agents": [{"name": name, "cwd": str(cwd)}]})
    fake.on("agent", "get", result={"agent": {"agent_status": status}})


@pytest.mark.parametrize("live", [False, True])
def test_a_fresh_reviewer_asked_again_moves_to_the_new_head(mesh, fake, run, repo, tmp_path, monkeypatch, capsys,
                                                            live):
    name, cwd, last, head = fresh_reviewer_and_a_fix(mesh, fake, run, repo, tmp_path, monkeypatch, capsys)
    fake.on("agent", "get", result={"agent": {"agent_status": "idle"}})  # herdr lists no agent there: none runs
    if live:  # idle, and its last task answered
        live_in_its_worktree(fake, name, cwd, "idle")
        mesh.record(run, name, "repo-lead", "done", "No findings.")
    pr = "https://github.com/acme/proj/pull/7"
    assert mesh.main(["ask", name, "--review", pr, "--no-wait"]) == 0
    assert worktree_head(cwd) == head
    entry = mesh.load_run(run)["agents"][name]
    assert entry["base"] == head and entry["review_heads"] == {pr: head} and entry["review_checkout"] == pr
    assert next(w for w in mesh.load_run(run)["worktrees"] if w["path"] == str(cwd))["base"] == head
    body = mesh.parse_message(mesh.unread(run, name)[-1])[1]
    assert "checked out in your worktree" in body and f"You last reviewed it at {last}" in body
    assert "Don't switch your branch" not in body and "still at" not in body


@pytest.mark.parametrize("why", ["dirty", "busy", "owes", "committed"])
def test_a_fresh_reviewer_asked_again_stays_when_moving_isnt_safe(mesh, fake, run, repo, tmp_path, monkeypatch,
                                                                   capsys, why):
    name, cwd, last, head = fresh_reviewer_and_a_fix(mesh, fake, run, repo, tmp_path, monkeypatch, capsys)
    fake.on("agent", "get", result={"agent": {"agent_status": "idle"}})
    if why in ("busy", "owes"):  # working, or idle with its review not reported (a declared wait, say)
        live_in_its_worktree(fake, name, cwd, "working" if why == "busy" else "idle")
    if why == "dirty":
        (cwd / "notes.txt").write_text("the reviewer's scratch")
    if why == "committed":
        subprocess.run(["git", "commit", "-q", "--allow-empty", "-m", "its own"], cwd=cwd, check=True)
    at = worktree_head(cwd)
    pr = "https://github.com/acme/proj/pull/7"
    assert mesh.main(["ask", name, "--review", pr, "--no-wait"]) == 0
    assert worktree_head(cwd) == at and mesh.load_run(run)["agents"][name]["base"] == last
    assert (cwd / "notes.txt").exists() == (why == "dirty")
    body = mesh.parse_message(mesh.unread(run, name)[-1])[1]
    reason = {"dirty": "uncommitted changes", "busy": "you were busy", "owes": "hadn't reported",
              "committed": "moved off the head"}[why]
    assert f"Your worktree is still at {last}, not the new head, because" in body and reason in body
    assert "Don't switch your branch" in body and f"`git show {head}:<path>`" in body
    assert f"stays at {last}" in capsys.readouterr().err


def test_a_reviewer_that_couldnt_move_moves_on_a_later_ask(mesh, fake, run, repo, tmp_path, monkeypatch, capsys):
    name, cwd, last, head = fresh_reviewer_and_a_fix(mesh, fake, run, repo, tmp_path, monkeypatch, capsys)
    fake.on("agent", "get", result={"agent": {"agent_status": "idle"}})
    pr = "https://github.com/acme/proj/pull/7"
    (cwd / "notes.txt").write_text("the reviewer's scratch")
    assert mesh.main(["ask", name, "--review", pr, "--no-wait"]) == 0
    entry = mesh.load_run(run)["agents"][name]
    assert (entry["base"], entry["review_heads"][pr]) == (last, head)  # it read the new head without moving
    (cwd / "notes.txt").unlink()
    capsys.readouterr()
    assert mesh.main(["ask", name, "--review", pr, "--no-wait"]) == 0  # same head: still moved, from its base
    assert worktree_head(cwd) == head and mesh.load_run(run)["agents"][name]["base"] == head
    assert "checked out in your worktree" in mesh.parse_message(mesh.unread(run, name)[-1])[1]


def test_a_failed_ask_moves_the_reviewer_back(mesh, fake, run, repo, tmp_path, monkeypatch, capsys):
    name, cwd, last, head = fresh_reviewer_and_a_fix(mesh, fake, run, repo, tmp_path, monkeypatch, capsys)

    def refuse(*args, **kwargs):
        assert worktree_head(cwd) == head  # moved before the reach, which may reopen it
        raise mesh.MeshError("it is still running")

    monkeypatch.setattr(mesh, "reach_worker", refuse)
    assert mesh.main(["ask", name, "--review", "https://github.com/acme/proj/pull/7", "--no-wait"]) == 1
    assert worktree_head(cwd) == last and mesh.load_run(run)["agents"][name]["base"] == last


def test_a_reviewer_running_in_another_run_never_moves(mesh, fake, run, repo, tmp_path, monkeypatch, capsys):
    name, cwd, last, head = fresh_reviewer_and_a_fix(mesh, fake, run, repo, tmp_path, monkeypatch, capsys)
    live_in_its_worktree(fake, name, cwd, "idle")
    data = mesh.load_run(run)
    target = ({**data, "id": "other"}, None, name, data["agents"][name])  # found in a run that was never stopped
    assert mesh.plan_move(run, target, "https://github.com/acme/proj/pull/7", head) == (
        None, f"you were still running in run other")


def test_a_stopped_fresh_reviewer_comes_back_at_the_new_head(mesh, fake, run, repo, tmp_path, monkeypatch, capsys):
    name, cwd, last, head = fresh_reviewer_and_a_fix(mesh, fake, run, repo, tmp_path, monkeypatch, capsys)
    pr = "https://github.com/acme/proj/pull/7"
    entry = {**mesh.load_run(run)["agents"][name], "status": "retired"}
    mesh.pr_head(repo, pr, 7)
    subprocess.run(["git", "worktree", "remove", str(cwd)], cwd=repo, check=True)  # as `mesh stop` leaves it
    target = (mesh.load_run(run), None, name, entry)
    move, why = mesh.plan_move(run, target, pr, head)
    assert move and not why
    mesh.apply_move(move)  # the branch kept: moved to the head
    assert mesh.git("rev-parse", entry["branch"], cwd=repo) == head and entry["base"] == head
    mesh.apply_move(move, undo=True)
    mesh.git("branch", "-D", entry["branch"], cwd=repo)  # untouched, so `mesh stop` deleted it
    move, why = mesh.plan_move(run, target, pr, head)
    mesh.apply_move(move)
    assert mesh.load_run(run)["agents"][name]["base"] == head  # where a resume recreates it
    assert mesh.ensure_worktree(entry, entry["branch"]) and worktree_head(cwd) == head


def test_an_author_reviewing_another_pr_never_moves(mesh, fake, run, repo, tmp_path, monkeypatch, capsys):
    pr_origin(tmp_path, repo)
    pr = "https://github.com/acme/proj/pull/7"
    entry = {"repo": str(repo), "cwd": str(repo), "base": "0ld", "branch": "main", "review_heads": {pr: "0ld"}}
    target = (mesh.load_run(run), None, "repo-fix-typo", entry)
    assert mesh.plan_move(run, target, pr, "new") == (None, "")  # its own checkout
    entry.update(cwd=str(tmp_path / "wt"), base="abc")
    assert mesh.plan_move(run, target, pr, "new") == (None, "")  # its base isn't a head it reviewed
    entry.update(base="0ld", peer="proj")
    assert mesh.plan_move(run, target, pr, "new") == (None, "")


@pytest.mark.parametrize("size", ["small", "large", None])
def test_a_fresh_claude_reviewer_runs_code_review_high_at_any_size(mesh, fake, run, repo, tmp_path, capsys, size):
    pr_origin(tmp_path, repo)
    stub_new_pane(fake)
    pr = "https://github.com/acme/proj/pull/7"
    argv = ["ask", "--repo", str(repo), "--kind", "claude", "--review", pr, "--no-wait"]
    assert mesh.main(argv + (["--size", size] if size else [])) == 0
    name = json.loads(capsys.readouterr().out)["name"]
    body = mesh.parse_message(mesh.unread(run, name)[0])[1]
    assert body.startswith(f"Review (read-only) of {pr} at ") and "`/code-review high 7`" in body  # no base: origin's
    assert "never with `--fix`, `--comment` or `ultra`" in body and "--type wait" in body
    assert "commits since your last review" not in body and "FETCH_HEAD" not in body


def test_code_review_targets_the_range_from_the_merge_base_in_a_fork_too(mesh, fake, cli, run, repo, tmp_path,
                                                                         monkeypatch, capsys):
    # A bare number is resolved by gh against the worktree's default repo, which in a fork may be `origin`.
    _, head = pr_on_a_branch(tmp_path, repo)
    subprocess.run(["git", "remote", "rename", "origin", "upstream"], cwd=repo, check=True)
    subprocess.run(["git", "remote", "add", "origin", "git@github.com:me/proj.git"], cwd=repo, check=True)
    real_git = mesh.git

    def git(*args, cwd):  # upstream is a local stand-in for github.com/acme/proj
        if args == ("remote", "get-url", "upstream"):
            return "https://github.com/acme/proj.git"
        return real_git(*args, cwd=cwd)

    monkeypatch.setattr(mesh, "git", git)
    stub_new_pane(fake)
    cli.on("pr", "view", stdout=json.dumps({"baseRefName": "main"}))
    pr = "https://github.com/acme/proj/pull/7"
    assert mesh.main(["ask", "--repo", str(repo), "--kind", "claude", "--review", pr, "--no-wait"]) == 0
    name = json.loads(capsys.readouterr().out)["name"]
    body = mesh.parse_message(mesh.unread(run, name)[0])[1]
    merge_base = real_git("merge-base", "main", head, cwd=repo)
    assert f"`/code-review high {merge_base}..{head}`" in body and f"`git diff {merge_base}..{head}`" in body
    assert "/code-review high 7" not in body
    cli.on("pr", "view", returncode=1)  # no base: the PR's link, since a fork's bare number may name `origin`'s
    task = mesh.ask_task("", pr, 7, fresh=True, head=head, kind="claude", repo=repo)
    assert f"`/code-review high {pr}`" in task


def test_a_fresh_reviewer_in_a_claude_run_runs_code_review(mesh, fake, run, repo, tmp_path, capsys):
    pr_origin(tmp_path, repo)
    stub_new_pane(fake)
    with mesh.editing_run(run) as data:
        data["mode"] = "claude"  # no --kind: the reviewer is of the run's mode
    pr = "https://github.com/acme/proj/pull/7"
    assert mesh.main(["ask", "--repo", str(repo), "--review", pr, "--size", "small", "--no-wait"]) == 0
    name = json.loads(capsys.readouterr().out)["name"]
    assert "`/code-review high 7`" in mesh.parse_message(mesh.unread(run, name)[0])[1]


def test_a_claude_reviewer_asked_again_runs_code_review_on_the_new_commits(mesh, fake, run, repo, tmp_path, capsys):
    head = pr_origin(tmp_path, repo)
    last = subprocess.run(["git", "rev-parse", "HEAD"], cwd=repo, capture_output=True, text=True).stdout.strip()
    fake.on("agent", "get", result={"agent": {"agent_status": "idle"}})
    pr = "https://github.com/acme/proj/pull/7"
    with mesh.editing_run(run) as data:
        data["agents"]["repo-fix-typo"].update(kind="claude", size="large", repo=str(repo), reviews=[pr],
                                               review_heads={pr: last})
    assert mesh.main(["ask", "repo-fix-typo", "--review", pr, "--no-wait"]) == 0
    body = mesh.parse_message(mesh.unread(run, "repo-fix-typo")[0])[1]
    assert f"`/code-review high {last}..{head}`" in body and "commits since your last review" in body
    assert f"`git diff {last} {head}`" in body and "review the whole PR again" not in body
    # An existing worker isn't on the PR's branch; the head is in the shared object store, unlike FETCH_HEAD.
    assert f"`git show {head}:<path>`" in body and "FETCH_HEAD" not in body


def test_a_rebased_pr_is_reviewed_again_whole(mesh, fake, run, repo, tmp_path, capsys):
    head = pr_origin(tmp_path, repo)
    subprocess.run(["git", "commit", "-q", "--allow-empty", "-m", "the old head, since rebased away"], cwd=repo,
                   check=True)
    last = subprocess.run(["git", "rev-parse", "HEAD"], cwd=repo, capture_output=True, text=True).stdout.strip()
    fake.on("agent", "get", result={"agent": {"agent_status": "idle"}})
    pr = "https://github.com/acme/proj/pull/7"
    with mesh.editing_run(run) as data:
        data["agents"]["repo-fix-typo"].update(kind="claude", repo=str(repo), reviews=[pr], review_heads={pr: last})
    assert mesh.main(["ask", "repo-fix-typo", "--review", pr, "--no-wait"]) == 0
    body = mesh.parse_message(mesh.unread(run, "repo-fix-typo")[0])[1]
    assert "`/code-review high 7`" in body and f"{last}..{head}" not in body
    assert "review the whole PR again" in body and "commits since your last review" not in body


def test_a_claude_reviewer_without_the_pr_head_reviews_by_hand(mesh, fake, run, capsys):
    # Another repo's PR (or one whose head can't be fetched): a bare number would name a PR of the worker's repo.
    fake.on("agent", "get", result={"agent": {"agent_status": "idle"}})
    pr = "https://github.com/acme/other/pull/3"
    with mesh.editing_run(run) as data:
        data["agents"]["repo-fix-typo"].update(kind="claude", repo="/nowhere")
    assert mesh.main(["ask", "repo-fix-typo", "--review", pr, "--no-wait"]) == 0
    body = mesh.parse_message(mesh.unread(run, "repo-fix-typo")[0])[1]
    assert body.startswith(f"Review (read-only) of {pr}:") and "/code-review" not in body
    assert "`git show FETCH_HEAD:<path>`" in body


def test_a_review_whose_head_cant_be_fetched_still_goes_out(mesh, fake, run, monkeypatch, capsys):
    fake.on("agent", "get", result={"agent": {"agent_status": "idle"}})
    pr = "https://github.com/acme/other/pull/3"
    with mesh.editing_run(run) as data:
        data["agents"]["repo-fix-typo"]["repo"] = "/nowhere"
    assert mesh.main(["ask", "repo-fix-typo", "--review", pr, "--no-wait"]) == 0
    body = mesh.parse_message(mesh.unread(run, "repo-fix-typo")[0])[1]
    assert body.startswith(f"Review (read-only) of {pr}:") and "You last reviewed" not in body
    assert "review_heads" not in mesh.load_run(run)["agents"]["repo-fix-typo"]


def test_a_pr_link_finds_its_author_not_its_reviewer(mesh, fake, run, repo, capsys):
    archived_worker(mesh, repo, session="c1")
    pr = "https://github.com/acme/proj/pull/7"
    with mesh.editing_run(run) as data:
        data["agents"]["proj-review-7"] = {"role": "worker", "kind": "agy", "repo": str(repo), "cwd": "/wt",
                                           "pane": "w2:p9", "reviews": [pr]}
    mesh.store_message(run, "proj-review-7", "repo-lead", "done", f"Reviewed {pr}: two findings.")
    assert mesh.find_worker(pr)[2] == "proj-fix"
    capsys.readouterr()
    assert mesh.main(["agents", "pull/7", "--json"]) == 0
    rows = {r["name"]: r for r in json.loads(capsys.readouterr().out)}
    assert (rows["proj-review-7"]["prs"], rows["proj-review-7"]["reviews"]) == ("", pr)
    assert rows["proj-fix"]["prs"] == pr


def test_a_pr_link_finds_its_author_by_the_prs_head_branch(mesh, fake, cli, run, repo, capsys):
    archived_worker(mesh, repo, session="c1")  # its report quotes PR 7, never PR 9
    pr = "https://github.com/acme/proj/pull/9"
    cli.on("pr", "view", stdout='{"headRefName": "mesh/mold/fix"}')
    assert mesh.find_worker(f"{pr}/files")[2] == "proj-fix"
    assert cli.called("pr", "view", pr)
    capsys.readouterr()
    assert mesh.main(["agents", pr, "--json"]) == 0
    assert [r["prs"] for r in json.loads(capsys.readouterr().out)] == [f"https://github.com/acme/proj/pull/7 {pr}"]
    # the branch its worktree is on counts too: a worker may follow its repo's branch naming
    path, _, _ = mesh.add_worktree(run, repo, "own", None)
    subprocess.run(["git", "switch", "-q", "-c", "feat/own"], cwd=path, check=True)
    with mesh.editing_run(run) as data:
        data["agents"]["proj-own"] = {"role": "worker", "kind": "agy", "repo": str(repo), "cwd": str(path),
                                      "branch": "mesh/mtest/own", "pane": "w2:p9"}
    cli.on("pr", "view", stdout='{"headRefName": "feat/own"}')
    assert mesh.find_worker(pr)[2] == "proj-own"
    with mesh.editing_run(run) as data:  # a reviewer on the PR's branch isn't its author
        data["agents"]["proj-own"]["reviews"] = [pr]
    with pytest.raises(mesh.MeshError, match="no worker matches"):
        mesh.find_worker(pr)
    # another repo's branch of the same name isn't the author; the message links are the fallback
    subprocess.run(["git", "remote", "add", "origin", "git@github.com:acme/other.git"], cwd=repo, check=True)
    cli.on("pr", "view", stdout='{"headRefName": "mesh/mold/fix"}')
    with pytest.raises(mesh.MeshError, match="no worker matches"):
        mesh.find_worker(pr)
    cli.on("pr", "view", returncode=1, stderr="gh: not logged in")
    assert mesh.find_worker("https://github.com/acme/proj/pull/7")[2] == "proj-fix"


def test_ask_reports_a_worker_that_stops_without_answering(mesh, fake, run, monkeypatch, capsys):
    states_then(mesh, monkeypatch, "idle", "working", "idle")
    assert mesh.main(["ask", "repo-fix-typo", "--body", "Why?"]) == 3
    assert "without sending a message (nudged once" in capsys.readouterr().out


def test_stop_drops_untouched_branches_and_resume_brings_them_back(mesh, fake, run, repo, tmp_path, capsys):
    pr_origin(tmp_path, repo)
    path, branch, base = mesh.add_worktree(run, repo, "ask-x", None)
    pushed, pushed_branch, _ = mesh.add_worktree(run, repo, "pushed", None)
    subprocess.run(["git", "push", "-q", "-u", "origin", pushed_branch], cwd=pushed, check=True)
    capsys.readouterr()
    assert mesh.main(["stop"]) == 0
    out = json.loads(capsys.readouterr().out)
    assert out["branches_deleted"] == [branch]
    assert out["branches_kept"] == [pushed_branch]
    branches = subprocess.run(["git", "branch"], cwd=repo, capture_output=True, text=True).stdout
    assert branch not in branches and pushed_branch in branches
    # resuming the asker later starts its branch again at the same commit
    assert mesh.ensure_worktree({"cwd": str(path), "repo": str(repo), "base": base}, branch) is True
    assert subprocess.run(["git", "rev-parse", "HEAD"], cwd=path, capture_output=True, text=True).stdout.strip() == base
    assert subprocess.run(["git", "branch", "--show-current"], cwd=path, capture_output=True, text=True).stdout.strip() == branch


def test_pr_numbers_resolve_against_the_github_remote(mesh, repo):
    subprocess.run(["git", "remote", "add", "origin", "git@github.com:acme/proj.git"], cwd=repo, check=True)
    assert mesh.pr_ref("7", repo) == ("https://github.com/acme/proj/pull/7", 7)
    assert mesh.pr_ref("#7", repo) == ("https://github.com/acme/proj/pull/7", 7)
    assert mesh.pr_ref("https://github.com/acme/proj/pull/7/files", repo) == ("https://github.com/acme/proj/pull/7", 7)
    with pytest.raises(mesh.MeshError, match="not a PR of proj"):
        mesh.pr_head(repo, "https://github.com/acme/other/pull/3", 3)


def test_ask_a_busy_worker_does_not_take_its_other_report_for_the_answer(mesh, fake, run, capsys):
    fake.on("agent", "get", result={"agent": {"agent_status": "working"}})
    assert mesh.main(["ask", "repo-fix-typo", "--body", "Why?"]) == 0
    captured = capsys.readouterr()
    assert "is busy" in captured.err and json.loads(captured.out)["delivery"]["rung"] is True


def test_ask_prints_the_answer_even_if_the_root_already_read_it(mesh, fake, run, monkeypatch, capsys):
    fake.on("agent", "get", result={"agent": {"agent_status": "idle"}})

    def sleep(_):  # the worker answers, and its doorbell has the root run `mesh inbox` first
        if not mesh.sends_from(run, "repo-fix-typo", mesh.FINAL_TYPES):
            mesh.deliver(run, "repo-fix-typo", "repo-lead", "done", "Because of the spec.")
            for path in mesh.unread(run, "repo-lead"):
                mesh.mark_read(path)

    monkeypatch.setattr(mesh.time, "sleep", sleep)
    monkeypatch.setattr(mesh, "watch_agent", lambda *a, **k: (sleep(0), (0, "reported"))[1])
    assert mesh.main(["ask", "repo-fix-typo", "--body", "Why?"]) == 0
    assert "── done from repo-fix-typo ──\nBecause of the spec." in capsys.readouterr().out


def test_only_the_root_asks(mesh, fake, run, repo, monkeypatch, capsys):
    as_worker(monkeypatch, run)
    assert mesh.main(["ask", "--repo", str(repo), "--body", "x"]) == 1
    assert "only a root runs `mesh ask`" in capsys.readouterr().err
    assert not fake.called("agent", "start")


def test_a_fork_review_fetches_from_the_remote_that_owns_the_pr(mesh, repo, tmp_path, monkeypatch):
    head = pr_origin(tmp_path, repo)
    subprocess.run(["git", "remote", "rename", "origin", "upstream"], cwd=repo, check=True)
    subprocess.run(["git", "remote", "add", "origin", "git@github.com:me/proj.git"], cwd=repo, check=True)
    real_git = mesh.git

    def git(*args, cwd):  # upstream is a local stand-in for github.com/acme/proj
        if args == ("remote", "get-url", "upstream"):
            return "https://github.com/acme/proj.git"
        return real_git(*args, cwd=cwd)

    monkeypatch.setattr(mesh, "git", git)
    assert mesh.pr_head(repo, "https://github.com/acme/proj/pull/7", 7) == head
    with pytest.raises(mesh.MeshError, match="remotes: "):
        mesh.pr_head(repo, "https://github.com/other/thing/pull/7", 7)


# ── 0.7: sizes, background workers, native transport, compaction, conflicts ──


def claude_root(mesh, run):
    """Give the run's root a live Claude socket, as `mesh init` records under Claude Code."""
    import os
    from pathlib import Path
    socks = Path(os.environ["MESH_CC_SOCKS"])
    socks.mkdir(parents=True, exist_ok=True)
    (socks / "4242.sock").write_text("")
    address = f"uds:{socks}/4242.sock"
    with mesh.editing_run(run) as data:
        data["agents"]["repo-lead"]["address"] = address
    return address


def bg_cli(cli, rows=None, short="ab12cd34"):
    """`claude --bg` prints its short id; `claude agents --json` lists the sessions in `rows`."""
    rows = rows if rows is not None else [{"id": short, "sessionId": f"{short}-s", "status": "busy", "pid": 777}]
    cli.on("--bg", stdout=f"Starting background service…\nbackgrounded · {short} · x\n")
    cli.on("agents", stdout=lambda: json.dumps(rows))
    return rows


def launch_settings_env(launch):
    """The environment a `claude --bg` launch gives its session, through `--settings`."""
    argv = launch["argv"]
    return json.loads(argv[argv.index("--settings") + 1])["env"]


def bg_worker(mesh, run, session="old-s", bg_id="old1", **extra):
    with mesh.editing_run(run) as data:
        data["agents"]["repo-fix-typo"].update(kind="claude", view="bg", session=session, bg_id=bg_id, pane=None,
                                                model="opus", effort="high", cwd="/wt", parent="repo-lead", **extra)


def test_claude_address_is_the_socket_of_an_ancestor(mesh, monkeypatch):
    import os
    from pathlib import Path
    assert mesh.claude_address() is None
    socks = Path(os.environ["MESH_CC_SOCKS"])
    socks.mkdir()
    (socks / f"{os.getpid()}.sock").write_text("")
    assert mesh.claude_address() == f"uds:{socks}/{os.getpid()}.sock"


def test_small_claude_task_runs_in_the_background_with_no_plan(mesh, fake, cli, run, repo, capsys):
    claude_root(mesh, run)
    bg_cli(cli)
    assert mesh.main(["spawn", "--kind", "claude", "--size", "small", "--repo", str(repo), "--worktree", "copy",
                      "--task", "Fix the typo in the footer."]) == 0
    out = json.loads(capsys.readouterr().out)
    assert (out["view"], out["model"], out["effort"], out["exec_model"]) == ("bg", "opus", "medium", None)
    launch = cli.called("--bg")[0]
    argv = launch["argv"]
    assert argv[:8] == ["claude", "--bg", "-n", "proj-copy", "--model", "opus", "--effort", "medium"]
    assert argv[8] == "--settings" and argv[10] == "--dangerously-skip-permissions"
    assert argv[11].startswith("📬 [mesh] You are proj-copy (worker) in agent-mesh run mtest")
    assert launch["cwd"] == mesh.worktree_root() / "mtest" / "proj-copy"
    assert launch_settings_env(launch)["MESH_SELF"] == "proj-copy"
    entry = mesh.load_run(run)["agents"]["proj-copy"]
    assert (entry["bg_id"], entry["session"], entry["phase"], entry["status"]) == ("ab12cd34", "ab12cd34-s", None, "running")
    assert not fake.called("agent", "start")
    task = mesh.parse_message(mesh.unread(run, "proj-copy")[0])[1]
    assert task.startswith("Direct task (no plan)") and task.rstrip().endswith("Fix the typo in the footer.")
    assert out["delivery"]["launched"] is True


def test_a_claude_worker_gets_its_hooks_and_agy_none(mesh, fake, run):
    env = mesh.mesh_env(run, "proj-x", "repo-lead", "worker")
    assert mesh.settings_argv("agy", env) == []
    flag, settings = mesh.settings_argv("claude", env)
    settings = json.loads(settings)
    assert flag == "--settings" and "env" not in settings  # a pane has its environment
    command = settings["hooks"]["Stop"][0]["hooks"][0]["command"]
    assert command.split() == [str(Path(mesh.__file__).absolute()), "hook", "stop", "--run", run, "--as", "proj-x"]
    starts = settings["hooks"]["SessionStart"]
    assert [g["matcher"] for g in starts] == ["compact", "clear", "resume"]
    assert starts[1]["hooks"][0]["command"].split()[1:4] == ["whoami", "--reminder", "clear"]
    bg = json.loads(mesh.worker_settings(env, bg=True))
    assert bg["hooks"] == settings["hooks"] and bg["env"]["MESH_SELF"] == "proj-x"


def test_a_bg_launch_gives_its_identity_and_config_by_settings_only(mesh, fake, cli, run, repo, monkeypatch, capsys):
    # A spare process keeps the environment of whoever started Claude Code's daemon, so the launch's own
    # environment carries no identity, and settings set every MESH_* variable and blank the herdr ids.
    claude_root(mesh, run)
    bg_cli(cli)
    monkeypatch.setenv("MESH_MIN_QUOTA", "30")
    monkeypatch.setenv("MESH_SELF", "repo-lead")
    monkeypatch.setenv("CLAUDE_CODE_SESSION_ID", "root-s")  # the root's own session, not the worker's
    monkeypatch.setenv("CLAUDE_CODE_MESSAGING_TOKEN", "t")
    monkeypatch.setenv("CLAUDE_CONFIG_DIR", "/cfg")
    assert mesh.main(["spawn", "--kind", "claude", "--size", "small", "--repo", str(repo), "--worktree", "copy",
                      "--task", "Fix it."]) == 0
    launch = cli.called("--bg")[0]
    assert not any(k.startswith("MESH_") or k in mesh.CLAUDE_SESSION_VARS or k in mesh.HERDR_IDS
                   for k in launch["env"])
    assert launch["env"]["CLAUDE_CONFIG_DIR"] == "/cfg"  # configuration of Claude Code itself stays
    env = launch_settings_env(launch)
    assert (env["MESH_RUN"], env["MESH_SELF"], env["MESH_PARENT"], env["MESH_ROLE"]) == \
        ("mtest", "proj-copy", "repo-lead", "worker")
    assert env["MESH_HOME"] == str(mesh.mesh_home()) and env["MESH_MIN_QUOTA"] == "30"
    assert env["MESH_COMPACT_AT"] == "" and env["MESH_CLAUDE"] == ""  # unset here, so a spare's value can't stay
    assert env["HERDR_PANE_ID"] == env["HERDR_TAB_ID"] == env["HERDR_WORKSPACE_ID"] == ""
    hooks = json.loads(launch["argv"][launch["argv"].index("--settings") + 1])["hooks"]
    assert hooks["Stop"][0]["hooks"][0]["command"].endswith("hook stop --run mtest --as proj-copy")
    # The `claude agents` lookups after the launch can start the daemon too.
    assert all(not any(k in c["env"] for k in (*mesh.MESH_IDS, "HERDR_PANE_ID", "CLAUDE_CODE_SESSION_ID"))
               for c in cli.called("agents"))
    capsys.readouterr()


def test_an_empty_config_variable_counts_as_unset(mesh, monkeypatch):
    for key in mesh.MESH_CONFIG:
        monkeypatch.setenv(key, "")
    assert (mesh.claude_bin(), mesh.side_panes(), mesh.min_quota()) == ("claude", mesh.SIDE_PANES, 10)
    assert mesh.mesh_home() == Path.home() / ".local/state/agent-mesh"
    assert mesh.mesh_env("r", "n", "p", "worker") == {"MESH_RUN": "r", "MESH_SELF": "n", "MESH_PARENT": "p",
                                                      "MESH_ROLE": "worker"}


def test_background_falls_back_to_a_pane_without_a_claude_root(mesh, fake, cli, run, repo, capsys):
    stub_new_pane(fake)
    assert mesh.main(["spawn", "--kind", "claude", "--size", "trivial", "--repo", str(repo), "--worktree", "cfg"]) == 0
    out = json.loads(capsys.readouterr().out)
    assert out["view"] == "pane" and "starting a pane" in out["notes"][0]
    assert started(fake)[-5:] == ("--model", "opus", "--effort", "medium",
                                                      "--dangerously-skip-permissions")
    assert not cli.called("--bg")


def test_an_untrusted_folder_falls_back_to_a_pane_with_the_task_kept(mesh, fake, cli, run, repo, capsys):
    claude_root(mesh, run)
    stub_new_pane(fake)
    cli.on("--bg", returncode=1, stdout="Workspace not trusted. Run `claude` in /x once")
    assert mesh.main(["spawn", "--kind", "claude", "--size", "small", "--repo", str(repo), "--worktree", "ui",
                      "--task", "Tweak the button."]) == 0
    out = json.loads(capsys.readouterr().out)
    assert out["view"] == "pane" and "doesn't trust" in out["notes"][0]
    assert len(mesh.unread(run, "proj-ui")) == 1  # stored once, before the launch that failed
    assert out["delivery"]["rung"] is True
    assert "You are proj-ui (worker) in agent-mesh run mtest" in fake.called("agent", "prompt")[-1][3]
    assert [(e["to"], e["via"]) for e in mesh.read_log(run) if e["event"] == "rung"] == [("proj-ui", "working")]


def test_small_and_trivial_agy_tasks_are_flash_at_high_and_low_in_a_pane(mesh, fake, run, repo, monkeypatch, capsys):
    stub_new_pane(fake)
    monkeypatch.setattr(mesh, "resolve_agy_model", lambda family, effort: f"gemini-3.8-{family}-{effort}")
    assert mesh.main(["spawn", "--size", "small", "--repo", str(repo), "--worktree", "fix"]) == 0
    out = json.loads(capsys.readouterr().out)
    assert (out["view"], out["model"], out["exec_model"]) == ("pane", "gemini-3.8-flash-high", None)
    fake.on("pane", "layout", result={"layout": {"panes": [{"pane_id": "w9:p10", "rect": {"width": 200, "height": 50}}]}})
    fake.on("pane", "split", result={"pane": {"pane_id": "w9:p11"}})
    assert mesh.main(["spawn", "--size", "trivial", "--repo", str(repo), "--worktree", "bump"]) == 0
    assert json.loads(capsys.readouterr().out)["model"] == "gemini-3.8-flash-low"
    assert mesh.main(["spawn", "--size", "small", "--view", "bg", "--repo", str(repo), "--worktree", "no"]) == 1
    assert "for Claude workers" in capsys.readouterr().err


def test_a_size_has_no_execution_phase_to_override(mesh, fake, run, repo, capsys):
    assert mesh.main(["spawn", "--kind", "claude", "--size", "small", "--exec-model", "opus", "--repo", str(repo)]) == 1
    assert "no execution phase" in capsys.readouterr().err


def test_user_reviewed_plan_runs_opus_in_plan_mode(mesh, fake, run, repo, capsys):
    stub_new_pane(fake)
    assert mesh.main(["spawn", "--kind", "claude", "--plan-review", "user", "--repo", str(repo), "--worktree", "feat",
                      "--task", "Build it."]) == 0
    start = started(fake)
    assert start[start.index("--model") + 1] == "opus" and start[-3:] == ("--permission-mode", "plan", "--allow-dangerously-skip-permissions")
    assert "--dangerously-skip-permissions" not in start
    entry = mesh.load_run(run)["agents"]["proj-feat"]
    assert (entry["plan_review"], entry["phase"], entry["view"]) == ("user", None, "pane")
    assert mesh.parse_message(mesh.unread(run, "proj-feat")[0])[1].startswith("Plan review by the user")


def test_watch_lets_a_user_reviewed_worker_wait_at_its_plan_dialog(mesh, fake, run, monkeypatch, capsys):
    with mesh.editing_run(run) as data:
        data["agents"]["repo-fix-typo"]["plan_review"] = "user"
    states = iter(["blocked", "blocked", "blocked"])

    def status(name, *_):
        state = next(states, None)
        if state is None:
            mesh.log_event(run, "send", sender=name, to="repo-lead", type="done", file="x.md")
            return "idle"
        return state

    monkeypatch.setattr(mesh, "agent_status", status)
    assert mesh.main(["watch", "repo-fix-typo"]) == 0


def test_a_claude_worker_reports_to_a_claude_root_by_send_message(mesh, fake, run, monkeypatch, capsys):
    address = claude_root(mesh, run)
    with mesh.editing_run(run) as data:
        data["agents"]["repo-fix-typo"]["kind"] = "claude"
    as_worker(monkeypatch, run)
    assert mesh.main(["send", "parent", "--type", "done", "--body", "PR: https://github.com/a/b/pull/1"]) == 0
    captured = capsys.readouterr()
    out = json.loads(captured.out)
    assert out["send_message"]["to"] == address and "done from repo-fix-typo" in out["send_message"]["message"]
    assert "SendMessage" in captured.err
    assert not fake.called("agent", "prompt")  # nothing typed into the user's pane
    assert len(mesh.unread(run, "repo-lead")) == 1  # the mailbox is still the record
    # an agy worker can't SendMessage: it rings, as before
    with mesh.editing_run(run) as data:
        data["agents"]["repo-fix-typo"]["kind"] = "agy"
    assert mesh.main(["send", "parent", "--type", "info", "--body", "x"]) == 0
    assert fake.called("agent", "prompt")


def test_a_dead_root_socket_falls_back_to_the_doorbell(mesh, fake, run, monkeypatch, capsys):
    from pathlib import Path
    address = claude_root(mesh, run)
    Path(address[4:]).unlink()
    with mesh.editing_run(run) as data:
        data["agents"]["repo-fix-typo"]["kind"] = "claude"
    as_worker(monkeypatch, run)
    assert mesh.main(["send", "parent", "--type", "info", "--body", "x"]) == 0
    assert "send_message" not in json.loads(capsys.readouterr().out) and fake.called("agent", "prompt")


def test_a_reopened_claude_root_rebinds_its_socket_on_its_next_command(mesh, fake, run, monkeypatch, capsys):
    import os
    from pathlib import Path
    old = claude_root(mesh, run)
    Path(old[4:]).unlink()
    (Path(os.environ["MESH_CC_SOCKS"]) / f"{os.getpid()}.sock").write_text("")
    assert mesh.main(["inbox"]) == 0
    assert mesh.load_run(run)["agents"]["repo-lead"]["address"] == mesh.claude_address() != old
    with mesh.editing_run(run) as data:
        data["agents"]["repo-fix-typo"]["kind"] = "claude"
    as_worker(monkeypatch, run)
    capsys.readouterr()
    assert mesh.main(["send", "parent", "--type", "info", "--body", "x"]) == 0
    assert json.loads(capsys.readouterr().out)["send_message"]["to"] == mesh.claude_address()


@pytest.mark.parametrize("where", ["another pane", "no pane", "nested in its pane"])
def test_a_session_acting_as_the_root_leaves_its_address(mesh, fake, run, monkeypatch, capsys, where):
    import os
    from pathlib import Path
    old = claude_root(mesh, run)
    as_claude(monkeypatch, "other-s", 5000)
    (Path(os.environ["MESH_CC_SOCKS"]) / "5000.sock").write_text("")
    if where == "another pane":
        monkeypatch.setenv("HERDR_PANE_ID", "w3:p9")
    if where == "no pane":
        monkeypatch.delenv("HERDR_PANE_ID")
    in_pane(fake, "w1:p1", 4242)  # the root's own Claude leads its pane
    assert mesh.main(["inbox", "--run", run, "--as", "repo-lead"]) == 0
    assert mesh.load_run(run)["agents"]["repo-lead"]["address"] == old
    assert not [e for e in mesh.read_log(run) if e["event"] == "rebind"]


@pytest.mark.parametrize("case", ["its pane gone", "its pane's Claude exited", "a `claude -c` beside it",
                                  "another session"])
def test_a_root_reopened_in_another_pane_is_known_by_its_conversation(mesh, fake, run, monkeypatch, capsys, case):
    import os
    from pathlib import Path
    old = claude_root(mesh, run)
    with mesh.editing_run(run) as data:
        data["agents"]["repo-lead"]["session"] = "root-s"  # as `mesh init` records it
    # `claude --resume root-s` in a new pane, then `--as`; or another session
    as_claude(monkeypatch, "other-s" if case == "another session" else "root-s", 5000)
    (Path(os.environ["MESH_CC_SOCKS"]) / "5000.sock").write_text("")
    monkeypatch.setenv("HERDR_PANE_ID", "w3:p9")
    if case == "its pane gone":
        fake.on("pane", "process-info", "--pane", "w1:p1", error="pane_not_found")
    elif case == "its pane's Claude exited":
        claude_in_pane(fake, "w1:p1", "bash")
    else:  # the root still runs in its pane
        claude_in_pane(fake, "w1:p1", "claude")
    assert mesh.main(["inbox", "--run", run, "--as", "repo-lead"]) == 0
    address = mesh.load_run(run)["agents"]["repo-lead"]["address"]
    healed = case in ("its pane gone", "its pane's Claude exited")
    assert (address == mesh.claude_address() != old) if healed else address == old


def test_the_root_keeps_its_recorded_conversation_current_from_its_pane(mesh, fake, run, monkeypatch, capsys):
    with mesh.editing_run(run) as data:
        data["agents"]["repo-lead"]["session"] = "root-s"
    as_claude(monkeypatch, "cleared-s", 5000)  # a /clear: the same process, a new session
    in_pane(fake, "w1:p1", 5000)
    assert mesh.main(["inbox"]) == 0
    assert mesh.load_run(run)["agents"]["repo-lead"]["session"] == "cleared-s"
    in_pane(fake, "w1:p1", 4242)  # a nested `claude -p` in its pane: not the root's process
    as_claude(monkeypatch, "nested-s", 7000)
    assert mesh.main(["inbox", "--run", run, "--as", "repo-lead"]) == 0
    assert mesh.load_run(run)["agents"]["repo-lead"]["session"] == "cleared-s"


@pytest.mark.parametrize("herdr_says", ["leader", "can't say"])
def test_a_reopened_root_in_its_own_pane_records_its_new_address(mesh, fake, run, monkeypatch, capsys, herdr_says):
    import os
    from pathlib import Path
    old = claude_root(mesh, run)
    as_claude(monkeypatch, "root-s", 5000)  # the root reopened: a new process, a new socket
    (Path(os.environ["MESH_CC_SOCKS"]) / "5000.sock").write_text("")
    if herdr_says == "leader":
        in_pane(fake, "w1:p1", 5000)
    assert mesh.main(["inbox"]) == 0
    address = mesh.load_run(run)["agents"]["repo-lead"]["address"]
    assert address == mesh.claude_address() != old and address.endswith("/5000.sock")
    assert [e["address"] for e in mesh.read_log(run) if e["event"] == "rebind"] == [address]
    # its session too, so the next command doesn't ask herdr again for it
    assert mesh.load_run(run)["agents"]["repo-lead"]["session"] == "root-s"


def test_a_message_to_a_background_worker_goes_by_its_socket(mesh, fake, cli, run, capsys):
    import os
    from pathlib import Path
    claude_root(mesh, run)
    bg_worker(mesh, run)
    rows = bg_cli(cli, rows=[{"id": "old1", "sessionId": "old-s", "status": "idle", "pid": 777}])
    (Path(os.environ["MESH_CC_SOCKS"]) / "777.sock").write_text("")
    assert mesh.main(["send", "repo-fix-typo", "--type", "answer", "--body", "Use ISO dates."]) == 0
    assert json.loads(capsys.readouterr().out)["send_message"]["to"].endswith("/777.sock")
    rows[0].pop("status")  # it exited
    assert mesh.main(["send", "repo-fix-typo", "--type", "answer", "--body", "Again."]) == 2
    assert "is not running" in json.loads(capsys.readouterr().out)["error"]
    assert not fake.called("agent", "prompt")


def test_bg_status_speaks_herdr(mesh):
    entry = {"view": "bg", "session": "s"}
    for status, state in (("busy", "working"), ("idle", "idle"), ("waiting", "blocked"), (None, "gone")):
        assert mesh.bg_status(entry, [{"sessionId": "s", "status": status}]) == state
    assert mesh.bg_status(entry, []) == "gone"
    assert mesh.agent_status("x", entry, [{"sessionId": "s", "status": "busy"}]) == "working"


def test_approve_moves_a_bg_worker_to_a_copy_on_its_exec_model(mesh, fake, cli, run, capsys):
    claude_root(mesh, run)
    bg_worker(mesh, run, phase="plan", exec_model="opus", exec_effort="medium")
    sent_plan(mesh, run)
    bg_cli(cli, rows=[{"id": "old1", "sessionId": "old-s", "status": "idle", "pid": 1},
                      {"id": "abcdef12", "sessionId": "new-s", "status": "busy", "pid": 2}], short="abcdef12")
    unread_at_launch = []
    cli.on("--bg", stdout="backgrounded · abcdef12 · repo-fix-typo\n",
           effect=lambda argv: unread_at_launch.append(len(mesh.unread(run, "repo-fix-typo"))))
    assert mesh.main(["approve", "repo-fix-typo", "--body", "Mind the migration."]) == 0
    out = json.loads(capsys.readouterr().out)
    assert cli.called("stop", "old1")
    argv = cli.called("--bg")[0]["argv"]
    assert argv[1:4] == ["--bg", "--resume", "old-s"] and argv[6:10] == ["--model", "opus", "--effort", "medium"]
    assert "now on opus at medium effort" in argv[-1]
    assert unread_at_launch == [1]  # the approval waits in the mailbox before the copy starts
    entry = mesh.load_run(run)["agents"]["repo-fix-typo"]
    assert (entry["session"], entry["bg_id"], entry["sessions"], entry["phase"]) == ("new-s", "abcdef12", ["old-s"], "exec")
    assert "swapped" not in entry and out["delivery"]["launched"] is True


def test_stop_and_retire_stop_background_workers(mesh, fake, cli, run, capsys):
    bg_worker(mesh, run)
    assert mesh.main(["retire", "repo-fix-typo"]) == 0
    assert cli.called("stop", "old1") and not fake.called("pane", "close")
    with mesh.editing_run(run) as data:
        data["agents"]["repo-fix-typo"]["status"] = "running"
    capsys.readouterr()
    assert mesh.main(["stop"]) == 0
    assert "repo-fix-typo" in json.loads(capsys.readouterr().out)["closed"]
    assert len(cli.called("stop", "old1")) == 2


def test_open_attaches_to_a_running_background_worker(mesh, fake, cli, run, monkeypatch, capsys):
    bg_worker(mesh, run)
    bg_cli(cli, rows=[{"id": "old1", "sessionId": "old-s", "status": "busy", "pid": 1}])
    calls = {}
    monkeypatch.setattr(mesh.os, "execvp", lambda f, argv: calls.update(argv=argv))
    mesh.main(["open", "repo-fix-typo"])
    assert calls["argv"] == ["claude", "attach", "old1"]


def session_file(mesh, monkeypatch, tmp_path, cwd, session, lines):
    monkeypatch.setenv("CLAUDE_CONFIG_DIR", str(tmp_path / "claude"))
    folder = mesh.claude_sessions_dir(cwd)
    folder.mkdir(parents=True, exist_ok=True)
    (folder / f"{session}.jsonl").write_text("".join(json.dumps(line) + "\n" for line in lines))


def usage(n):
    return {"type": "assistant", "message": {"usage": {"input_tokens": 2, "cache_read_input_tokens": n,
                                                       "cache_creation_input_tokens": 10}}}


def test_context_tokens_counts_the_last_prompt_since_compaction(mesh, monkeypatch, tmp_path):
    entry = {"kind": "claude", "cwd": "/wt/proj-fix"}
    session_file(mesh, monkeypatch, tmp_path, entry["cwd"], "s", [usage(1000), usage(5000)])
    assert mesh.context_tokens(entry, "s") == 5012
    talk = {"type": "user", "message": {"content": "what is a compact_boundary?", "usage": {}}}
    session_file(mesh, monkeypatch, tmp_path, entry["cwd"], "s",
                 [usage(5000), {"type": "system", "subtype": "compact_boundary"}, talk])
    assert mesh.context_tokens(entry, "s") == 0
    assert mesh.context_tokens({"kind": "agy", "cwd": "/x"}, "s") is None
    assert mesh.context_tokens(entry, "missing") is None


def pb_varint(n):
    out = bytearray()
    while True:
        out.append(n & 0x7F | (0x80 if n > 0x7F else 0))
        n >>= 7
        if not n:
            return bytes(out)


def pb(*fields):
    """A protobuf message from (field, value) pairs: an int is a varint, bytes are length-delimited."""
    out = b""
    for field, value in fields:
        if isinstance(value, int):
            out += pb_varint(field << 3) + pb_varint(value)
        else:
            out += pb_varint(field << 3 | 2) + pb_varint(len(value)) + value
    return out


def agy_call(fresh, cached=0, out=50):
    """A model call's step metadata as agy writes it: usage in field 9, among fields mesh ignores."""
    usage = pb((1, 1320), (2, fresh), (3, out), *([(5, cached)] if cached else []), (6, 24),
               (7, b"bot-167b372f"), (8, pb((1, b"sessionID"), (2, b"-375"))))
    return pb((1, pb((1, 1792958697), (2, 540058032))), (3, 2), (9, usage), (11, 1320), (12, b"a39c9314"))


def agy_conversation_file(mesh, session, steps):
    """agy's conversation file: `steps` are (step_type, metadata[, status]) in order, done unless a status is given."""
    import sqlite3
    folder = Path(os.environ["MESH_AGY_CONVERSATIONS"])
    folder.mkdir(parents=True, exist_ok=True)
    (folder / f"{session}.db").unlink(missing_ok=True)
    db = sqlite3.connect(folder / f"{session}.db")
    db.execute("CREATE TABLE steps (idx integer PRIMARY KEY, step_type integer NOT NULL DEFAULT 0, "
               "status integer NOT NULL DEFAULT 0, metadata blob, step_payload blob)")
    db.executemany("INSERT INTO steps (idx, step_type, status, metadata) VALUES (?, ?, ?, ?)",
                   [(i, step[0], step[2] if len(step) > 2 else 3, step[1]) for i, step in enumerate(steps)])
    db.commit()
    db.close()


def test_agy_context_is_the_last_calls_prompt_since_its_last_compaction(mesh):
    user, tool = (14, pb((1, pb((1, 5))))), (132, b"\xff\xff")  # a tool step that isn't protobuf is skipped
    agy_conversation_file(mesh, "c1", [user, (15, agy_call(11_904)), tool, (15, agy_call(2_083, cached=82_756)), tool])
    assert mesh.context_tokens({"kind": "agy"}, "c1") == 84_839  # fresh plus cached input, not the output
    agy_conversation_file(mesh, "c1", [(15, agy_call(248_334)), (23, pb((1, b"<summary>")))])
    assert mesh.context_tokens({"kind": "agy"}, "c1") == 0  # compacted (by `/compact`, or by agy itself)
    agy_conversation_file(mesh, "c1", [(15, agy_call(248_334)), (23, b""), user, (15, agy_call(18_134))])
    assert mesh.agy_context_tokens("c1") == 18_134
    agy_conversation_file(mesh, "c1", [(15, agy_call(248_334)), (23, b"", 2)])
    assert mesh.agy_context_tokens("c1") == 248_334  # a compaction that didn't finish compacted nothing
    agy_conversation_file(mesh, "c1", [(15, agy_call(20_000)), (15, "not bytes"), (15, 7)])
    assert mesh.agy_context_tokens("c1") == 20_000  # metadata that isn't a blob is skipped, not a crash
    agy_conversation_file(mesh, "c1", [user])
    assert mesh.agy_context_tokens("c1") is None  # no model call yet
    assert mesh.agy_context_tokens("missing") is None
    (Path(os.environ["MESH_AGY_CONVERSATIONS"]) / "junk.db").write_text("not sqlite")
    assert mesh.agy_context_tokens("junk") is None


def test_protobuf_fields_reads_every_wire_type_and_refuses_a_truncated_message(mesh):
    blob = pb((1, 300), (2, b"hi")) + bytes([0x1D]) + b"\x01\x02\x03\x04" + bytes([0x21]) + bytes(8)
    assert mesh.protobuf_fields(blob) == [(1, 300), (2, b"hi"), (3, b"\x01\x02\x03\x04"), (4, bytes(8))]
    with pytest.raises(ValueError):
        mesh.protobuf_fields(pb((2, b"hello"))[:-2])
    with pytest.raises(IndexError):
        mesh.protobuf_fields(bytes([0x08, 0x80]))


def test_when_auto_compaction_compacts(mesh, monkeypatch):
    claude, agy = {"kind": "claude"}, {"kind": "agy"}
    assert mesh.should_compact("auto", claude, 450_000, False, True)
    assert not mesh.should_compact("auto", claude, 250_000, False, True)
    assert not mesh.should_compact("auto", claude, 450_000, True, True)  # a question doesn't repay it
    assert mesh.should_compact("auto", claude, 650_000, True, False)
    assert not mesh.should_compact("auto", claude, None, False, True)
    assert mesh.should_compact("yes", claude, 10, False, False) and not mesh.should_compact("no", claude, 10**6, False, True)
    assert mesh.should_compact("auto", agy, 150_000, False, False)  # its own threshold, whatever run it's from
    assert not mesh.should_compact("auto", agy, 50_000, False, True)
    assert not mesh.should_compact("auto", agy, 150_000, True, True) and mesh.should_compact("auto", agy, 250_000, True, True)
    # its size can't be read: compact what comes back from an earlier run, unless it's a question
    assert mesh.should_compact("auto", agy, None, False, True)
    assert not mesh.should_compact("auto", agy, None, False, False) and not mesh.should_compact("auto", agy, None, True, True)
    monkeypatch.setenv("MESH_COMPACT_AT", "1000")
    assert mesh.should_compact("auto", claude, 1500, False, False)
    assert not mesh.should_compact("auto", agy, 50_000, False, False)
    monkeypatch.setenv("MESH_AGY_COMPACT_AT", "40000")
    assert mesh.should_compact("auto", agy, 50_000, False, False)


def test_resume_compacts_a_large_conversation_before_reopening_it(mesh, fake, cli, run, repo, monkeypatch,
                                                                  tmp_path, capsys):
    path, _ = archived_worker(mesh, repo, kind="claude", session="s-1")
    session_file(mesh, monkeypatch, tmp_path, str(path), "s-1", [usage(450_000)])
    stub_new_pane(fake)
    started = []
    cli.on("-p", effect=lambda argv: started.append(bool(fake.called("agent", "start"))))
    capsys.readouterr()
    assert mesh.main(["resume", "proj-fix", "--task", "Apply the review.\nDetails…"]) == 0
    out = json.loads(capsys.readouterr().out)
    argv = cli.called("-p")[0]["argv"]
    assert argv[:7] == ["claude", "-p", "--resume", "s-1", "--model", "opus", "--dangerously-skip-permissions"]
    assert argv[7].startswith("/compact Keep what the next task needs: Apply the review.")
    assert started == [False]  # compacted before the pane opens
    assert "MESH_SELF" not in cli.called("-p")[0]["env"]
    assert out["compaction"]["compacted"] is True and out["compaction"]["context_tokens"] == 450_012


def test_a_failed_compaction_still_resumes(mesh, fake, cli, run, repo, monkeypatch, tmp_path, capsys):
    path, _ = archived_worker(mesh, repo, kind="claude", session="s-1")
    session_file(mesh, monkeypatch, tmp_path, str(path), "s-1", [usage(450_000)])
    stub_new_pane(fake)
    cli.on("-p", returncode=1, stderr="boom")
    capsys.readouterr()
    assert mesh.main(["resume", "proj-fix", "--task", "Go.", "--focus", "the API contract"]) == 0
    out = json.loads(capsys.readouterr().out)
    assert cli.called("-p")[0]["argv"][-1] == "/compact the API contract"
    assert out["compaction"]["compacted"] is False and "boom" in out["notes"][0]
    assert fake.called("agent", "start")


def test_agy_compacts_when_it_comes_back_from_another_run(mesh, fake, cli, run, repo, capsys):
    archived_worker(mesh, repo, session="c1")
    stub_new_pane(fake)
    assert mesh.main(["resume", "proj-fix", "--task", "Apply the review."]) == 0
    argv = cli.calls[0]["argv"]
    assert argv[:4] == ["agy", "--conversation", "c1", "--model"] and argv[-2] == "-p"
    assert argv[-1].startswith("/compact ")
    capsys.readouterr()


def test_a_small_agy_conversation_from_another_run_is_not_compacted(mesh, fake, cli, run, repo, capsys):
    archived_worker(mesh, repo, session="c1")
    agy_conversation_file(mesh, "c1", [(15, agy_call(3_000, cached=40_000))])
    stub_new_pane(fake)
    capsys.readouterr()
    assert mesh.main(["resume", "proj-fix", "--task", "Apply the review."]) == 0
    assert not cli.called("--conversation")
    assert json.loads(capsys.readouterr().out)["compaction"] == {"context_tokens": 43_000, "compacted": False}


def test_compact_no_skips_it(mesh, fake, cli, run, repo, capsys):
    archived_worker(mesh, repo, session="c1")
    stub_new_pane(fake)
    assert mesh.main(["resume", "proj-fix", "--compact", "no"]) == 0
    assert not cli.calls
    capsys.readouterr()


def test_resume_of_a_bg_worker_starts_a_copy_with_the_new_runs_identity(mesh, fake, cli, run, repo, capsys):
    claude_root(mesh, run)
    path, _ = archived_worker(mesh, repo, kind="claude", session="s-1")
    with open(mesh.mesh_home() / "archive" / "mold" / "run.json") as fh:
        data = json.load(fh)
    data["agents"]["proj-fix"].update(view="bg", bg_id="s1short")
    (mesh.mesh_home() / "archive" / "mold" / "run.json").write_text(json.dumps(data))
    bg_cli(cli, rows=[{"id": "c0ffee01", "sessionId": "copy-s", "status": "busy", "pid": 5}], short="c0ffee01")
    capsys.readouterr()
    assert mesh.main(["resume", "proj-fix", "--task", "Address the review."]) == 0
    out = json.loads(capsys.readouterr().out)
    launch = cli.called("--bg")[0]
    assert launch["argv"][1:4] == ["--bg", "--resume", "s-1"] and launch_settings_env(launch)["MESH_RUN"] == "mtest"
    assert "again, resumed into agent-mesh run mtest" in launch["argv"][-1]
    entry = mesh.load_run(run)["agents"]["proj-fix"]
    assert (entry["view"], entry["session"], entry["sessions"]) == ("bg", "copy-s", ["s-1"])
    assert out["view"] == "bg" and not fake.called("agent", "start")


def test_ask_under_a_claude_root_returns_once_delivered(mesh, fake, run, monkeypatch, capsys):
    claude_root(mesh, run)
    with mesh.editing_run(run) as data:
        data["agents"]["repo-fix-typo"]["kind"] = "claude"
    fake.on("agent", "get", result={"agent": {"agent_status": "idle"}})
    monkeypatch.setattr(mesh, "watch_agent", lambda *a, **k: pytest.fail("the answer comes by SendMessage"))
    assert mesh.main(["ask", "repo-fix-typo", "--body", "Why ISO dates?"]) == 0
    assert json.loads(capsys.readouterr().out)["delivery"]["rung"] is True


def test_a_small_fresh_asker_gets_the_question_not_a_direct_task(mesh, fake, cli, run, repo, capsys):
    claude_root(mesh, run)
    bg_cli(cli)
    assert mesh.main(["ask", "--repo", str(repo), "--kind", "claude", "--size", "small",
                      "--body", "How is restockedAt computed?"]) == 0
    out = json.loads(capsys.readouterr().out)
    assert out["view"] == "bg"
    task = mesh.parse_message(mesh.unread(run, out["name"])[0])[1]
    assert task.startswith("Question (read-only)") and "Direct task" not in task


def deny_rules(argv):
    """The rules after `--disallowedTools`, up to the next flag."""
    argv = list(argv)
    if "--disallowedTools" not in argv:
        return []
    rest = argv[argv.index("--disallowedTools") + 1:]
    return rest[:next((i for i, a in enumerate(rest) if a.startswith("--")), len(rest))]


def test_a_claude_asker_can_neither_edit_nor_publish(mesh, fake, cli, run, repo, capsys):
    claude_root(mesh, run)
    bg_cli(cli)
    assert mesh.main(["ask", "--repo", str(repo), "--kind", "claude", "--size", "small",
                      "--body", "How is restockedAt computed?", "--no-wait"]) == 0
    out = json.loads(capsys.readouterr().out)
    rules = deny_rules(cli.called("--bg")[0]["argv"])
    assert rules == [f"Edit(/{Path(out['cwd']).resolve()}/**)", f"Edit(/{repo.resolve()}/**)",
                     *(f"Bash({cmd}:*)" for cmd in mesh.PUBLISH_COMMANDS)]
    assert "Bash(git push:*)" in rules and rules[0].startswith("Edit(//")
    assert mesh.load_run(run)["agents"][out["name"]]["read_only"] is True
    # A change task gets no deny list.
    assert mesh.main(["spawn", "--kind", "claude", "--size", "small", "--repo", str(repo), "--worktree", "copy",
                      "--task", "Fix the typo in the footer."]) == 0
    assert not deny_rules(cli.called("--bg")[-1]["argv"])
    assert mesh.load_run(run)["agents"]["proj-copy"]["read_only"] is False


def test_a_claude_reviewer_in_a_pane_gets_the_deny_list_and_agy_none(mesh, fake, cli, run, repo, tmp_path,
                                                                             monkeypatch, capsys):
    pr_origin(tmp_path, repo)
    stub_new_pane(fake)
    with_quotas(mesh, monkeypatch)
    pr = "https://github.com/acme/proj/pull/7"
    assert mesh.main(["ask", "--repo", str(repo), "--kind", "claude", "--review", pr, "--no-wait"]) == 0
    argv = started(fake)
    assert "Bash(gh pr comment:*)" in deny_rules(argv) and argv[-1] == "--dangerously-skip-permissions"
    # agy has no deny list: its askers are held by the prompt alone.
    assert mesh.claude_deny({"kind": "agy", "read_only": True, "cwd": "/wt"}) == []


def test_a_fresh_reviewer_opens_in_a_pane_where_a_question_would_not(mesh, fake, cli, run, repo, tmp_path, capsys):
    pr_origin(tmp_path, repo)
    claude_root(mesh, run)
    bg_cli(cli)
    stub_new_pane(fake)
    pr = "https://github.com/acme/proj/pull/7"
    assert mesh.main(["ask", "--repo", str(repo), "--kind", "claude", "--size", "small", "--review", pr]) == 0
    out = json.loads(capsys.readouterr().out)
    assert out["view"] == "pane" and fake.called("agent", "start") and not cli.called("--bg")
    # A named view still wins.
    assert mesh.main(["ask", "--repo", str(repo), "--kind", "claude", "--size", "small", "--review", pr,
                      "--view", "bg", "--name", "review-bg"]) == 0
    assert json.loads(capsys.readouterr().out)["view"] == "bg"


def test_watch_any_returns_the_first_event_and_alerts_only_keeps_going(mesh, fake, run, monkeypatch, capsys):
    with mesh.editing_run(run) as data:
        data["agents"]["repo-two"] = {"role": "worker", "kind": "agy", "pane": "w2:p6", "parent": "repo-lead",
                                      "status": "running"}
    polls = {"n": 0}

    def status(name, *_):
        if name == "repo-two":
            polls["n"] += 1
            if polls["n"] == 2:
                mesh.log_event(run, "send", sender="repo-two", to="repo-lead", type="done", file="x.md")
        return "working"

    monkeypatch.setattr(mesh, "agent_status", status)
    assert mesh.main(["watch", "--any"]) == 0
    assert "repo-two reported done" in capsys.readouterr().out

    def status2(name, *_):
        if name == "repo-two":
            polls["n"] += 1
            if polls["n"] == 5:
                mesh.log_event(run, "send", sender="repo-two", to="repo-lead", type="done", file="y.md")
            return "working"
        return "gone" if polls["n"] > 6 else "working"

    monkeypatch.setattr(mesh, "agent_status", status2)
    assert mesh.main(["watch", "--any", "--alerts"]) == 5
    assert "ALERT repo-fix-typo is gone" in capsys.readouterr().out
    assert mesh.main(["watch"]) == 1


def test_a_running_watch_holds_its_workers_lock_and_root_commands_warn_without_one(mesh, fake, run, monkeypatch,
                                                                                    capsys):
    assert mesh.unwatched(run) == []  # no task yet: it owes nothing
    mesh.deliver(run, "repo-lead", "repo-fix-typo", "task", "Fix the typo.")
    assert mesh.unwatched(run) == ["repo-fix-typo"]
    held = mesh.hold_watch(run, ["repo-fix-typo"])
    assert mesh.watched(run, "repo-fix-typo") and mesh.unwatched(run) == []
    for handle in held:
        handle.close()  # as when the watch process dies
    assert not mesh.watched(run, "repo-fix-typo")
    monkeypatch.setattr(mesh, "spawn_worker", lambda args, run_id, me: {"name": "repo-other"})
    capsys.readouterr()
    assert mesh.main(["spawn", "--kind", "agy", "--repo", ".", "--task", "x"]) == 0
    assert "warning: no `mesh watch` covers repo-fix-typo (it owes a report)" in capsys.readouterr().err
    seen = {}

    def tick(self, *args, **kwargs):
        seen["watched"] = mesh.watched(run, "repo-fix-typo")
        return 0, "done"

    monkeypatch.setattr(mesh.Watch, "tick", tick)
    assert mesh.watch_agents(run, ["repo-fix-typo"]) == (0, "done")
    assert seen["watched"] and not mesh.watched(run, "repo-fix-typo")
    assert mesh.unwatched(run, skip=("repo-fix-typo",)) == []
    mesh.deliver(run, "repo-fix-typo", "repo-lead", "done", "Fixed.")
    assert mesh.unwatched(run) == []


def test_a_gone_worker_gets_its_own_advice_and_none_once_alerted_gone(mesh, fake, run, monkeypatch, capsys):
    mesh.deliver(run, "repo-lead", "repo-fix-typo", "task", "Fix the typo.")
    monkeypatch.setattr(mesh, "agent_status", lambda name, *_: "gone")
    mesh.warn_unwatched(run)
    err = capsys.readouterr().err
    assert "repo-fix-typo looks gone without a report: `mesh watch repo-fix-typo` (exit 5)" in err
    assert "--any" not in err  # it would leave a gone worker out
    assert mesh.main(["watch", "repo-fix-typo"]) == 5
    assert [(e["kind"], e["received"]) for e in mesh.read_log(run) if e["event"] == "alert"] == [("gone", 1)]
    capsys.readouterr()
    mesh.warn_unwatched(run)
    assert capsys.readouterr().err == ""  # the root was told
    mesh.deliver(run, "repo-lead", "repo-fix-typo", "task", "Again, resumed.")  # a new task re-arms it
    assert mesh.unwatched(run) == ["repo-fix-typo"]


def commit_file(path, name, text, msg):
    (path / name).write_text(text)
    subprocess.run(["git", "add", name], cwd=path, check=True)
    subprocess.run(["git", "commit", "-q", "-m", msg], cwd=path, check=True)


def test_conflicts_finds_overlapping_branches_and_skips_stacked_ones(mesh, fake, run, repo, capsys):
    commit_file(repo, "a.txt", "1\n", "base")
    b1, _, _ = mesh.add_worktree(run, repo, "b1", None)
    commit_file(b1, "a.txt", "2\n", "b1")
    b2, _, _ = mesh.add_worktree(run, repo, "b2", None)
    commit_file(b2, "a.txt", "3\n", "b2")
    b3, _, _ = mesh.add_worktree(run, repo, "b3", None)
    commit_file(b3, "c.txt", "x\n", "b3")
    b4, _, _ = mesh.add_worktree(run, repo, "b4", "mesh/mtest/b1")  # stacked on b1
    commit_file(b4, "a.txt", "4\n", "b4")
    mesh.add_worktree(run, repo, "b5", None)  # no commits: not checked
    capsys.readouterr()
    assert mesh.main(["conflicts", "--json"]) == 0
    rows = json.loads(capsys.readouterr().out)
    pairs = {frozenset((r["branch"].rsplit("/", 1)[1], r["against"].rsplit("/", 1)[1])) for r in rows}
    assert pairs == {frozenset(("b1", "b2")), frozenset(("b2", "b4"))}
    assert all(r["files"] == ["a.txt"] and r["kind"] == "conflict" for r in rows)
    assert mesh.main(["conflicts"]) == 0
    out = capsys.readouterr().out
    assert "mesh/mtest/b2" in out and "Warning" not in out


def test_conflicts_warns_about_overlapping_work_before_it_is_committed(mesh, fake, run, repo, capsys):
    commit_file(repo, "a.txt", "1\n", "base")
    commit_file(repo, "r.txt", "r\n", "base")
    b1, _, _ = mesh.add_worktree(run, repo, "b1", None)
    (b1 / "a.txt").write_text("2\n")  # unstaged
    (b1 / "new.txt").write_text("n\n")  # untracked
    b2, _, _ = mesh.add_worktree(run, repo, "b2", None)
    commit_file(b2, "a.txt", "3\n", "b2")  # committed, so merge-tree has nothing to compare it with
    b3, _, _ = mesh.add_worktree(run, repo, "b3", None)
    (b3 / "new.txt").write_text("m\n")
    subprocess.run(["git", "mv", "r.txt", "s.txt"], cwd=b3, check=True)  # staged rename
    b4, _, _ = mesh.add_worktree(run, repo, "b4", None)
    (b4 / "r.txt").write_text("changed\n")
    b5, _, _ = mesh.add_worktree(run, repo, "b5", "mesh/mtest/b2")  # stacked on b2
    (b5 / "a.txt").write_text("5\n")
    capsys.readouterr()
    assert mesh.main(["conflicts", "--json"]) == 0
    rows = json.loads(capsys.readouterr().out)
    assert all(r["kind"] == "overlap" for r in rows)
    found = {frozenset((r["branch"].rsplit("/", 1)[1], r["against"].rsplit("/", 1)[1])): r["files"] for r in rows}
    assert found == {frozenset(("b1", "b2")): ["a.txt"], frozenset(("b1", "b3")): ["new.txt"],
                     frozenset(("b3", "b4")): ["r.txt"], frozenset(("b1", "b5")): ["a.txt"]}
    assert mesh.main(["conflicts"]) == 0
    out = capsys.readouterr().out
    assert out.startswith("No conflicts among 1 branch(es) with commits.")
    assert "Warning" in out and "new.txt" in out


def overlap_pairs(mesh, capsys):
    capsys.readouterr()
    assert mesh.main(["conflicts", "--json"]) == 0
    return {frozenset(r[k].rsplit("/", 1)[1] for k in ("branch", "against")): r["files"]
            for r in json.loads(capsys.readouterr().out) if r["kind"] == "overlap"}


def test_conflicts_counts_only_a_workers_own_commits(mesh, fake, run, repo, capsys):
    commit_file(repo, "a.txt", "1\n", "base")
    b2, _, _ = mesh.add_worktree(run, repo, "b2", None)
    commit_file(b2, "a.txt", "2\n", "b2")
    b5, _, _ = mesh.add_worktree(run, repo, "b5", "mesh/mtest/b2")  # stacked on b2
    commit_file(b2, "x.txt", "x\n", "b2 again")  # b2 is no longer an ancestor of b5
    assert overlap_pairs(mesh, capsys) == {}
    (b5 / "a.txt").write_text("5\n")  # still stacked: overlapping b2 is the point
    commit_file(b5, "y.txt", "y\n", "b5")
    assert overlap_pairs(mesh, capsys) == {}
    # Two workers on one non-default base share its files without touching them.
    c1, _, _ = mesh.add_worktree(run, repo, "c1", "mesh/mtest/b2")
    c2, _, _ = mesh.add_worktree(run, repo, "c2", "mesh/mtest/b2")
    assert overlap_pairs(mesh, capsys) == {}
    (c1 / "x.txt").write_text("c1\n")
    (c2 / "x.txt").write_text("c2\n")
    assert overlap_pairs(mesh, capsys) == {frozenset(("c1", "c2")): ["x.txt"]}


def test_conflicts_sees_overlap_with_a_worker_started_after_a_rebase(mesh, fake, run, repo, capsys):
    commit_file(repo, "a.txt", "1\n", "base")
    b, _, _ = mesh.add_worktree(run, repo, "b", None)
    commit_file(b, "a.txt", "b\n", "b")
    x, _, _ = mesh.add_worktree(run, repo, "x", None)
    commit_file(repo, "m.txt", "m\n", "main moves on")
    subprocess.run(["git", "rebase", "-q", "main"], cwd=b, check=True)
    subprocess.run(["git", "merge", "-q", "--ff-only", "main"], cwd=x, check=True)  # no commits of its own
    a, _, _ = mesh.add_worktree(run, repo, "a", None)  # from main, which b and x now build on
    (a / "a.txt").write_text("a\n")
    y, _, _ = mesh.add_worktree(run, repo, "y", None)
    for path in (x, y):
        (path / "c.txt").write_text(path.name)
    assert overlap_pairs(mesh, capsys) == {frozenset(("a", "b")): ["a.txt"], frozenset(("x", "y")): ["c.txt"]}


def test_conflicts_skips_a_worker_whose_base_is_gone(mesh, fake, run, repo, capsys):
    commit_file(repo, "a.txt", "1\n", "base")
    for slug in ("b1", "b2", "b3"):
        path, _, _ = mesh.add_worktree(run, repo, slug, None)
        (path / "a.txt").write_text(slug)
    with mesh.editing_run(run) as data:
        next(wt for wt in data["worktrees"] if wt["branch"].endswith("/b3"))["base"] = "0" * 40
    assert overlap_pairs(mesh, capsys) == {frozenset(("b1", "b2")): ["a.txt"]}
    assert mesh.main(["conflicts"]) == 0
    assert "no overlap check for mesh/mtest/b3" in capsys.readouterr().err


def test_conflicts_reads_a_rename_in_the_worktree(mesh, fake, run, repo, capsys):
    commit_file(repo, "r.txt", "".join(f"line {i}\n" for i in range(20)), "base")
    b1, _, _ = mesh.add_worktree(run, repo, "b1", None)
    (b1 / "r.txt").rename(b1 / "s.txt")
    subprocess.run(["git", "add", "-N", "s.txt"], cwd=b1, check=True)  # status shows " R s.txt <- r.txt"
    b2, _, _ = mesh.add_worktree(run, repo, "b2", None)
    (b2 / "r.txt").write_text("changed\n")
    (b2 / "s.txt").write_text("new\n")
    assert overlap_pairs(mesh, capsys) == {frozenset(("b1", "b2")): ["r.txt", "s.txt"]}


def test_conflicts_matches_committed_renames_and_non_ascii_paths(mesh, fake, run, repo, capsys):
    commit_file(repo, "r.txt", "".join(f"line {i}\n" for i in range(20)), "base")
    commit_file(repo, "café.txt", "1\n", "base")
    b1, _, _ = mesh.add_worktree(run, repo, "b1", None)
    subprocess.run(["git", "mv", "r.txt", "s.txt"], cwd=b1, check=True)
    commit_file(b1, "café.txt", "b1\n", "rename r.txt, edit café.txt")
    b2, _, _ = mesh.add_worktree(run, repo, "b2", None)
    (b2 / "r.txt").write_text("changed\n")
    (b2 / "café.txt").write_text("b2\n")
    assert overlap_pairs(mesh, capsys) == {frozenset(("b1", "b2")): ["café.txt", "r.txt"]}


def test_a_bg_approve_whose_launch_failed_can_be_run_again(mesh, fake, cli, run, capsys):
    claude_root(mesh, run)
    bg_worker(mesh, run, phase="plan", exec_model="opus", exec_effort="medium")
    sent_plan(mesh, run)
    cli.on("--bg", returncode=1, stdout="boom")
    assert mesh.main(["approve", "repo-fix-typo"]) == 1
    entry = mesh.load_run(run)["agents"]["repo-fix-typo"]
    assert (entry["status"], entry["phase"]) == ("swap_failed", "plan")
    assert len(mesh.unread(run, "repo-fix-typo")) == 1  # its approval went in before the launch
    bg_cli(cli, rows=[{"id": "abcdef12", "sessionId": "new-s", "status": "busy", "pid": 2}], short="abcdef12")
    assert mesh.main(["approve", "repo-fix-typo"]) == 0
    assert mesh.load_run(run)["agents"]["repo-fix-typo"]["phase"] == "exec"
    capsys.readouterr()


def test_a_bg_swap_is_marked_starting_before_its_session_stops(mesh, fake, cli, run, capsys):
    claude_root(mesh, run)
    bg_worker(mesh, run, phase="plan", exec_model="opus", exec_effort="medium")
    sent_plan(mesh, run)
    bg_cli(cli, rows=[{"id": "abcdef12", "sessionId": "new-s", "status": "busy", "pid": 2}], short="abcdef12")
    at_stop = []
    cli.on("stop", effect=lambda argv: at_stop.append(mesh.load_run(run)["agents"]["repo-fix-typo"]["status"]))
    assert mesh.main(["approve", "repo-fix-typo"]) == 0
    assert at_stop == ["starting"]  # a watch polling during the stop excuses the gap
    capsys.readouterr()


def test_a_bg_session_that_never_lists_is_stopped_and_the_spawn_takes_a_pane(mesh, fake, cli, run, repo, capsys):
    claude_root(mesh, run)
    stub_new_pane(fake)
    bg_cli(cli, rows=[])
    assert mesh.main(["spawn", "--kind", "claude", "--size", "small", "--repo", str(repo), "--worktree", "ui",
                      "--task", "Tweak the button."]) == 0
    out = json.loads(capsys.readouterr().out)
    assert cli.called("stop", "ab12cd34")  # no stray session left to do the task too
    assert out["view"] == "pane" and "doesn't list it" in out["notes"][0]
    assert mesh.load_run(run)["agents"]["proj-ui"]["status"] == "running"
    assert len(mesh.unread(run, "proj-ui")) == 1 and out["delivery"]["rung"] is True


def test_a_bg_resume_in_an_untrusted_folder_reopens_in_a_pane(mesh, fake, cli, run, repo, capsys):
    claude_root(mesh, run)
    archived_worker(mesh, repo, kind="claude", session="s-1")
    with open(mesh.mesh_home() / "archive" / "mold" / "run.json") as fh:
        data = json.load(fh)
    data["agents"]["proj-fix"].update(view="bg", bg_id="s1short")
    (mesh.mesh_home() / "archive" / "mold" / "run.json").write_text(json.dumps(data))
    stub_new_pane(fake)
    cli.on("--bg", returncode=1, stdout="Workspace not trusted. Run `claude` in /x once")
    capsys.readouterr()
    assert mesh.main(["resume", "proj-fix", "--task", "Address the review."]) == 0
    out = json.loads(capsys.readouterr().out)
    entry = mesh.load_run(run)["agents"]["proj-fix"]
    assert (out["view"], entry["view"], entry["status"]) == ("pane", "pane", "running")
    assert fake.called("agent", "start") and "doesn't trust" in out["notes"][-1]
    assert len(mesh.unread(run, "proj-fix")) == 1 and out["delivery"]["rung"] is True


def test_an_alerts_watch_ends_once_every_worker_reported_and_ended(mesh, fake, run, monkeypatch, capsys):
    polls = {"n": 0}

    def status(name, *_):
        polls["n"] += 1
        if polls["n"] == 2:
            mesh.log_event(run, "send", sender="repo-fix-typo", to="repo-lead", type="done", file="x.md")
        return "working" if polls["n"] < 4 else "gone"

    monkeypatch.setattr(mesh, "agent_status", status)
    assert mesh.main(["watch", "--any", "--alerts"]) == 0
    assert "nothing left to watch" in capsys.readouterr().out


def test_a_watch_lets_a_worker_go_that_the_root_retired(mesh, fake, run, monkeypatch, capsys):
    with mesh.editing_run(run) as data:
        data["agents"]["repo-two"] = {"role": "worker", "kind": "agy", "pane": "w2:p6", "parent": "repo-lead",
                                      "status": "running"}
    closed = set()

    def status(name, *_):
        if name == "repo-fix-typo" and not closed:  # retired mid-watch, without a report
            mesh.retire_worker(run, name, mesh.agent_entry(mesh.load_run(run), name))
            closed.add(name)
        return "gone" if name in closed else "working"

    monkeypatch.setattr(mesh, "agent_status", status)
    clock = iter(range(0, 10**6, 100))
    monkeypatch.setattr(mesh.time, "monotonic", lambda: next(clock))
    assert mesh.main(["watch", "--any", "--alerts", "--timeout", "150"]) == 6
    out = capsys.readouterr().out
    assert out.startswith("ALERT repo-two has not reported") and "repo-fix-typo" not in out

    closed.clear()
    with mesh.editing_run(run) as data:
        data["agents"]["repo-fix-typo"]["status"] = "running"
    assert mesh.main(["watch", "repo-fix-typo"]) == 0  # without --alerts too, rather than waiting forever
    assert "retired repo-fix-typo; nothing left to watch" in capsys.readouterr().out


def test_retire_marks_the_worker_before_closing_it_and_undoes_that_on_failure(mesh, fake, run, monkeypatch):
    seen = []
    fake.on("pane", "close", error="cli_error")

    def herdr(*args, **kw):
        if args[:2] == ("pane", "close"):
            seen.append(mesh.load_run(run)["agents"]["repo-fix-typo"]["status"])
        return fake(*args, **kw)

    monkeypatch.setattr(mesh, "herdr", herdr)
    with pytest.raises(mesh.HerdrError):
        mesh.retire_worker(run, "repo-fix-typo", mesh.agent_entry(mesh.load_run(run), "repo-fix-typo"))
    assert seen == ["retiring"]  # until its pane is closed: status and pane counts still see it
    assert mesh.load_run(run)["agents"]["repo-fix-typo"]["status"] == "running"


def test_a_watch_takes_back_a_worker_whose_retire_failed(mesh, fake, run, monkeypatch):
    monkeypatch.setattr(mesh, "agent_status", lambda *_: "working")
    watch = mesh.Watch(run, "repo-fix-typo", grace=30)
    with mesh.editing_run(run) as data:
        data["agents"]["repo-fix-typo"]["status"] = "retiring"
    assert watch.tick(5) is None and watch.finished
    with mesh.editing_run(run) as data:  # the close failed: retire_worker put the status back
        data["agents"]["repo-fix-typo"]["status"] = "running"
    monkeypatch.setattr(mesh, "agent_status", lambda *_: "gone")
    assert watch.tick(5)[0] == 5 and not watch.finished


def test_a_watch_says_reported_for_a_worker_retired_after_its_report(mesh, fake, run, monkeypatch):
    # a Copilot asker files its done, then its entry is retired
    mesh.log_event(run, "send", sender="repo-fix-typo", to="repo-lead", type="done", file="x.md")
    with mesh.editing_run(run) as data:
        data["agents"]["repo-fix-typo"]["status"] = "retired"
    monkeypatch.setattr(mesh, "agent_status", lambda *_: "gone")
    status, line = mesh.watch_agents(run, ["repo-fix-typo"], finals_at_start=0, alerts_only=True)
    assert status == 0 and line.startswith("every watched worker reported")


def test_an_alerts_watch_timeout_names_only_the_workers_that_have_not_reported(mesh, fake, run, monkeypatch):
    with mesh.editing_run(run) as data:
        data["agents"]["repo-two"] = {"role": "worker", "kind": "agy", "pane": "w2:p6", "parent": "repo-lead",
                                      "status": "running"}
    mesh.log_event(run, "send", sender="repo-fix-typo", to="repo-lead", type="info", file="x.md")
    monkeypatch.setattr(mesh, "agent_status", lambda *_: "working")
    clock = iter(range(0, 10**6, 100))
    monkeypatch.setattr(mesh.time, "monotonic", lambda: next(clock))
    watches = []
    real = mesh.Watch.tick

    def tick(self, *a):
        if self.name == "repo-fix-typo" and not self.reported:
            mesh.log_event(run, "send", sender="repo-fix-typo", to="repo-lead", type="done", file="y.md")
        watches.append(self)
        return real(self, *a)

    monkeypatch.setattr(mesh.Watch, "tick", tick)
    status, line = mesh.watch_agents(run, ["repo-fix-typo", "repo-two"], timeout=150, alerts_only=True)
    assert status == 6 and line.startswith("ALERT repo-two has not reported") and "repo-fix-typo" not in line


def test_the_compaction_focus_skips_the_prefixes(mesh):
    assert mesh.task_gist(f"{mesh.DIRECT_PREFIX}\n\n# Fix reactions\n\nDetails") == "Fix reactions"
    assert mesh.task_gist(f"{mesh.ASK_PREFIX}\n\nHow is restockedAt computed?") == "How is restockedAt computed?"
    assert mesh.task_gist("Apply the review.\nDetails…") == "Apply the review."
    assert mesh.task_gist(None) == ""


def test_the_compaction_turn_has_no_identity_at_all(mesh, fake, cli, run, repo, monkeypatch, tmp_path, capsys):
    path, _ = archived_worker(mesh, repo, kind="claude", session="s-1")
    session_file(mesh, monkeypatch, tmp_path, str(path), "s-1", [usage(450_000)])
    stub_new_pane(fake)
    monkeypatch.setenv("HERDR_PANE_ID", "w1:p1")  # the root's pane, which by-pane maps to the root
    capsys.readouterr()
    assert mesh.main(["resume", "proj-fix", "--task", "Go."]) == 0
    env = cli.called("-p")[0]["env"]
    assert "HERDR_PANE_ID" not in env and not any(k in env for k in mesh.MESH_IDS)
    assert env["MESH_HOME"] == str(mesh.mesh_home())  # configuration stays
    events = [e["event"] for e in mesh.read_log(run) if e.get("event") in ("resuming", "resume")]
    assert events == ["resuming", "resume"]  # logged before the compaction, for `mesh watch --any`
    capsys.readouterr()


def test_model_auto_on_a_small_task_means_its_default(mesh, fake, cli, run, repo, capsys):
    claude_root(mesh, run)
    bg_cli(cli)
    assert mesh.main(["spawn", "--kind", "claude", "--size", "small", "--model", "auto", "--repo", str(repo),
                      "--worktree", "copy"]) == 0
    assert json.loads(capsys.readouterr().out)["model"] == "opus"
    assert cli.called("--bg")[0]["argv"][4:6] == ["--model", "opus"]


def test_watch_any_reads_claude_agents_once_per_poll(mesh, fake, cli, run, capsys):
    bg_worker(mesh, run, session="s1", bg_id="b1")
    with mesh.editing_run(run) as data:
        data["agents"]["repo-two"] = {**data["agents"]["repo-fix-typo"], "session": "s2", "bg_id": "b2"}
    rows = [{"id": "b1", "sessionId": "s1", "status": "busy"}, {"id": "b2", "sessionId": "s2", "status": "busy"}]

    def agents_call(argv):
        if len(cli.called("agents")) == 2:  # read by the next poll's log read, before its `claude agents`
            mesh.log_event(run, "send", sender="repo-two", to="repo-lead", type="done", file="x.md")

    cli.on("agents", stdout=lambda: json.dumps(rows), effect=agents_call)
    assert mesh.main(["watch", "--any"]) == 0
    assert "repo-two reported done" in capsys.readouterr().out
    assert len(cli.called("agents")) == 3  # one to list the live workers, then one per poll


def put_worker(mesh, run, name, spawned, status="running"):
    with mesh.editing_run(run) as data:
        data["agents"][name] = {"role": "worker", "kind": "agy", "pane": "w2:p6", "parent": "repo-lead",
                                "status": status, "spawned": spawned}


def test_watch_any_takes_on_a_worker_spawned_after_it_started(mesh, fake, run, monkeypatch, capsys):
    monkeypatch.setattr(mesh, "agent_status", lambda *_: "working")
    polls, seen = {"n": 0}, {}
    real = mesh.Watch.tick

    def tick(self, *a):
        polls["n"] += 1
        if polls["n"] == 2:
            put_worker(mesh, run, "repo-two", "2026-10-26T00:00:00+00:00")
        if self.name == "repo-two":
            seen["covered"] = mesh.watched(run, "repo-two") and mesh.watched(run, mesh.ANY_WATCH)
            mesh.log_event(run, "send", sender="repo-two", to="repo-lead", type="done", file="x.md")
        return real(self, *a)

    monkeypatch.setattr(mesh.Watch, "tick", tick)
    assert mesh.main(["watch", "--any"]) == 0
    assert "repo-two reported done" in capsys.readouterr().out and seen["covered"]
    assert not mesh.watched(run, "repo-two") and not mesh.watched(run, mesh.ANY_WATCH)


def test_watch_any_lets_a_retired_worker_go_and_takes_it_back_when_resumed(mesh, fake, run, monkeypatch, capsys):
    put_worker(mesh, run, "repo-two", "2026-10-26T00:00:00+00:00")
    monkeypatch.setattr(mesh, "agent_status", lambda *_: "working")
    polls, seen, watches = {"n": 0}, [], []
    real = mesh.Watch.tick

    def tick(self, *a):
        if self.name == "repo-fix-typo":
            polls["n"] += 1
            if polls["n"] == 2:
                with mesh.editing_run(run) as data:
                    data["agents"]["repo-two"]["status"] = "retired"
                mesh.log_event(run, "retire", name="repo-two")
            if polls["n"] == 3:
                seen.append(mesh.watched(run, "repo-two"))
                put_worker(mesh, run, "repo-two", "2026-10-26T01:00:00+00:00", status="starting")
            if polls["n"] == 5:
                mesh.log_event(run, "send", sender="repo-two", to="repo-lead", type="done", file="x.md")
        elif self not in watches:
            watches.append(self)
        return real(self, *a)

    monkeypatch.setattr(mesh.Watch, "tick", tick)
    assert mesh.main(["watch", "--any"]) == 0
    assert "repo-two reported done" in capsys.readouterr().out
    assert seen == [False]  # it left, its lock released
    assert len(watches) == 2  # a fresh Watch once resumed


def test_watch_any_waits_for_a_retired_workers_resume_before_saying_nothing_is_left(mesh, fake, run, monkeypatch):
    state = {"now": "working"}
    monkeypatch.setattr(mesh, "agent_status", lambda *_: state["now"])
    old = "2026-01-01T00:00:00+00:00"  # past RETIRE_SETTLE: only the pending resume holds the watch
    polls, first = {"n": 0}, []
    real = mesh.Watch.tick

    def tick(self, *a):
        polls["n"] += 1
        if polls["n"] == 1:  # a group's task 1 is done: the root retires the worker, and its resume compacts
            mesh.log_event(run, "send", sender="repo-fix-typo", to="repo-lead", type="done", file="x.md")
            with mesh.editing_run(run) as data:
                data["agents"]["repo-fix-typo"]["status"] = "retired"
            mesh.log_event(run, "retire", name="repo-fix-typo", time=old)
            mesh.log_event(run, "resuming", name="repo-fix-typo", from_run=run)
            first.append(self)
        elif self not in first:  # the resumed worker reports task 2, then ends
            if not self.reported:
                mesh.log_event(run, "send", sender="repo-fix-typo", to="repo-lead", type="done", file="y.md")
            else:
                state["now"] = "gone"
        return real(self, *a)

    monkeypatch.setattr(mesh.Watch, "tick", tick)
    calls = {"n": 0}
    settling = mesh.Roster.settling

    def settle(self, watches):
        calls["n"] += 1
        if calls["n"] == 4:  # compaction done: the entry comes back, and the task is delivered
            put_worker(mesh, run, "repo-fix-typo", "2026-10-26T01:00:00+00:00")
            mesh.log_event(run, "resume", name="repo-fix-typo", from_run=run, from_name="repo-fix-typo")
            mesh.log_event(run, "send", sender="repo-lead", to="repo-fix-typo", type="task", file="t.md")
            return True
        return settling(self, watches)

    monkeypatch.setattr(mesh.Roster, "settling", settle)
    status, line = mesh.watch_agents(run, ["repo-fix-typo"], alerts_only=True, every=mesh.load_run(run))
    assert (status, line) == (0, "every watched worker reported and has ended; nothing left to watch")
    assert calls["n"] >= 4 and len(first) == 1  # it held through the resume instead of exiting after the retire


def test_roster_settles_after_a_retire_and_during_a_resume(mesh, fake, run):
    roster = mesh.Roster(run, 30, 0, {}, mesh.load_run(run), [])
    old = "2026-01-01T00:00:00+00:00"
    path = mesh.run_dir(run) / "log.jsonl"
    mesh.log_event(run, "retire", name="a", time=old)
    assert not roster.settling([])
    mesh.log_event(run, "retire", name="a")
    assert roster.settling([])
    mesh.log_event(run, "resuming", name="a", from_run=run)
    mesh.log_event(run, "resume", name="a", from_run=run, from_name="a")
    assert not roster.settling([])  # a retire it was resumed from no longer holds it
    path.write_text("")
    mesh.log_event(run, "resuming", name="a", from_run=run, time=old)  # one that never ended holds it only so long
    assert not roster.settling([])
    mesh.log_event(run, "resuming", name="a", from_run=run)
    assert roster.settling([])
    mesh.log_event(run, "resume_failed", from_name="a", from_run=run)
    assert not roster.settling([])
    with mesh.editing_run(run) as data:  # retire_worker waits for its turn to end before the `retire` event
        data["agents"]["repo-fix-typo"]["status"] = "retiring"
    assert roster.settling([])
    for handle in roster.held.values():
        handle.close()


def test_watch_any_holds_while_a_worker_is_retiring(mesh, fake, run, monkeypatch):
    monkeypatch.setattr(mesh, "agent_status", lambda *_: "working")
    real = mesh.Watch.tick

    def tick(self, *a):
        mesh.log_event(run, "send", sender="repo-fix-typo", to="repo-lead", type="done", file="x.md")
        with mesh.editing_run(run) as data:
            data["agents"]["repo-fix-typo"]["status"] = "retiring"
        monkeypatch.setattr(mesh.Watch, "tick", real)
        return real(self, *a)

    monkeypatch.setattr(mesh.Watch, "tick", tick)
    calls = {"n": 0}
    settling = mesh.Roster.settling

    def settle(self, watches):
        calls["n"] += 1
        if calls["n"] == 3:  # the herdr wait is over: retired, and no resume follows
            with mesh.editing_run(run) as data:
                data["agents"]["repo-fix-typo"]["status"] = "retired"
            mesh.log_event(run, "retire", name="repo-fix-typo", time="2026-01-01T00:00:00+00:00")
        return settling(self, watches)

    monkeypatch.setattr(mesh.Roster, "settling", settle)
    status, line = mesh.watch_agents(run, ["repo-fix-typo"], alerts_only=True, every=mesh.load_run(run))
    assert calls["n"] >= 3 and (status, line) == (0, "every watched worker reported and has ended; nothing left "
                                                      "to watch")


def test_watch_any_started_mid_resume_waits_for_the_worker(mesh, fake, run, monkeypatch, capsys):
    with mesh.editing_run(run) as data:
        data["agents"]["repo-fix-typo"].update(status="retired", spawned="2026-10-26T00:00:00+00:00")
    mesh.log_event(run, "resuming", name="repo-fix-typo", from_run=run)
    monkeypatch.setattr(mesh, "agent_status", lambda *_: "working")
    calls = {"n": 0}
    settling = mesh.Roster.settling

    def settle(self, watches):
        calls["n"] += 1
        if calls["n"] == 2:  # compaction done: the entry is back, and its task delivered
            put_worker(mesh, run, "repo-fix-typo", "2026-10-26T01:00:00+00:00")
            mesh.log_event(run, "resume", name="repo-fix-typo", from_run=run, from_name="repo-fix-typo")
        return settling(self, watches)

    monkeypatch.setattr(mesh.Roster, "settling", settle)
    real = mesh.Watch.tick

    def tick(self, *a):
        mesh.log_event(run, "send", sender="repo-fix-typo", to="repo-lead", type="done", file="x.md")
        return real(self, *a)

    monkeypatch.setattr(mesh.Watch, "tick", tick)
    assert mesh.main(["watch", "--any"]) == 0
    assert "repo-fix-typo reported done" in capsys.readouterr().out


def test_watch_any_takes_on_a_starting_worker_it_read_as_gone(mesh, fake, run, monkeypatch, capsys):
    with mesh.editing_run(run) as data:  # mid-spawn or mid-swap: no pane yet
        data["agents"]["repo-fix-typo"].update(status="starting", pane=None)
    states = iter(["gone", "working"])
    monkeypatch.setattr(mesh, "agent_status", lambda *_: next(states, "working"))
    real = mesh.Watch.tick

    def tick(self, *a):
        mesh.log_event(run, "send", sender="repo-fix-typo", to="repo-lead", type="done", file="x.md")
        return real(self, *a)

    monkeypatch.setattr(mesh.Watch, "tick", tick)
    assert mesh.main(["watch", "--any"]) == 0
    assert "repo-fix-typo reported done" in capsys.readouterr().out


def test_watch_any_raises_a_gone_worker_that_owes_a_report_once(mesh, fake, run, monkeypatch, capsys):
    mesh.deliver(run, "repo-lead", "repo-fix-typo", "task", "Fix the typo.")
    monkeypatch.setattr(mesh, "agent_status", lambda *_: "gone")
    capsys.readouterr()
    assert mesh.main(["watch", "--any", "--alerts"]) == 5
    assert "ALERT repo-fix-typo is gone" in capsys.readouterr().out
    assert mesh.main(["watch", "--any", "--alerts"]) == 0  # raised for this task already: a restart leaves it out
    assert capsys.readouterr().out.startswith("no live workers to watch")


def test_a_failed_resume_logs_resume_failed(mesh, fake, run, monkeypatch):
    def reopen(args, run_id, me, started):
        mesh.log_event(run_id, "resuming", name="proj-fix", from_run="old")
        started.append({"name": "proj-fix", "from_run": "old"})
        raise mesh.MeshError("compaction blew up")

    monkeypatch.setattr(mesh, "reopen_worker", reopen)
    with pytest.raises(mesh.MeshError):
        mesh.resume_worker(None, run, "repo-lead")
    assert [e for e in mesh.read_log(run) if e["event"] == "resume_failed"][0]["from_name"] == "proj-fix"
    roster = mesh.Roster(run, 30, 0, {}, mesh.load_run(run), [])
    assert not roster.settling([])
    for handle in roster.held.values():
        handle.close()


def test_watch_any_exit_lines_count_retired_workers(mesh, fake, run, monkeypatch):
    put_worker(mesh, run, "repo-two", "2026-10-26T00:00:00+00:00")
    monkeypatch.setattr(mesh, "agent_status", lambda *_: "working")
    monkeypatch.setattr(mesh.Roster, "settling", lambda self, watches: False)
    real = mesh.Watch.tick

    def scenario(retire_reporter):
        def tick(self, *a):
            with mesh.editing_run(run) as data:
                if self.name == "repo-fix-typo":
                    if not self.reported:
                        mesh.log_event(run, "send", sender="repo-fix-typo", to="repo-lead", type="done", file="x.md")
                    elif retire_reporter:
                        data["agents"]["repo-fix-typo"]["status"] = "retired"
                else:  # retired without a report
                    data["agents"]["repo-two"]["status"] = "retired"
            return real(self, *a)
        monkeypatch.setattr(mesh.Watch, "tick", tick)
        with mesh.editing_run(run) as data:
            for name in ("repo-fix-typo", "repo-two"):
                data["agents"][name]["status"] = "running"
        return mesh.watch_agents(run, ["repo-fix-typo", "repo-two"], alerts_only=True, every=mesh.load_run(run),
                                 timeout=150)

    clock = iter(range(0, 10**6, 100))
    monkeypatch.setattr(mesh.time, "monotonic", lambda: next(clock))
    assert scenario(False) == (0, "every watched worker reported or was retired within 150s")
    monkeypatch.setattr(mesh.time, "monotonic", lambda: 0)
    assert scenario(True) == (0, "retired repo-two; the rest reported and have ended; nothing left to watch")


def test_brief_says_a_running_any_watch_takes_on_an_unwatched_worker(mesh, fake, run, capsys):
    mesh.deliver(run, "repo-lead", "repo-fix-typo", "task", "Fix the typo.")
    held = mesh.hold_watch(run, [mesh.ANY_WATCH])
    fake.on("agent", "get", result={"agent": {"agent_status": "working"}})
    capsys.readouterr()
    assert mesh.main(["brief"]) == 0
    out = capsys.readouterr().out
    assert "not watched yet: repo-fix-typo; the running `mesh watch --any` takes them on" in out
    for handle in held:
        handle.close()


def copilot_jsonl(text, model="gpt-5.6-luna"):
    """What `copilot -p --output-format json` prints: a tool turn with no text, then the answer."""
    events = [{"type": "session.auto_mode_resolved", "data": {"chosenModel": model}},
              {"type": "assistant.message", "data": {"model": model, "content": "", "toolRequests": [{"name": "bash"}]}},
              {"type": "assistant.message", "data": {"model": model, "content": text}},
              {"type": "result", "sessionId": "s", "exitCode": 0}]
    return "".join(json.dumps(e) + "\n" for e in events)


def test_ask_copilot_reviews_headless_and_resumes_for_a_follow_up(mesh, fake, cli, run, repo, tmp_path, capsys):
    head = pr_origin(tmp_path, repo)
    pr = "https://github.com/acme/proj/pull/7"
    cli.on("--output-format", stdout=copilot_jsonl("1. x.py:3 crashes on empty input."))
    assert mesh.main(["ask", "--repo", str(repo), "--review", pr, "--kind", "copilot"]) == 0
    captured = capsys.readouterr()
    assert "── done from proj-review-7-copilot ──\n1. x.py:3 crashes" in captured.out and "apply" not in captured.err
    entry = mesh.load_run(run)["agents"]["proj-review-7-copilot"]
    assert (entry["kind"], entry["view"], entry["status"], entry["model"], entry["reviews"], entry["base"]) == (
        "copilot", "headless", "retired", "gpt-5.6-luna", [pr], head)
    [call] = cli.called("--output-format")
    argv = call["argv"]
    assert "-p" not in argv  # the prompt goes on stdin, since a diff can outgrow one argument
    assert call["input"].startswith(f"Review (read-only) of {pr} at {head}:") and "checked out in your worktree" in call["input"]
    assert entry["review_heads"] == {pr: head}
    assert call["input"].endswith(mesh.COPILOT_NOTE) and "/code-review" not in call["input"]
    assert call["cwd"] == entry["cwd"] and "MESH_RUN" not in call["env"] and "pid" not in entry
    denied = argv[argv.index("--deny-tool") + 1: argv.index("--no-ask-user")]
    assert denied[0] == "write" and "shell(git push:*)" in denied and "shell(gh pr comment:*)" in denied
    assert argv[argv.index("--session-id") + 1] == entry["session"]
    assert argv[argv.index("--model") + 1] == "auto" and "--auto-tier" in argv and "--reasoning-effort" not in argv
    assert not fake.called("agent", "start") and not mesh.unread(run, "proj-review-7-copilot")

    cli.on("--output-format", stdout=copilot_jsonl("Because the list can be empty."))
    assert mesh.main(["ask", "proj-review-7-copilot", "--body", "Why?"]) == 0
    assert "Because the list can be empty." in capsys.readouterr().out
    follow = cli.called("--output-format")[-1]
    # The same --session-id resumes the session it started.
    assert follow["argv"][follow["argv"].index("--session-id") + 1] == entry["session"]
    assert "--resume" not in follow["argv"] and follow["cwd"] == entry["cwd"]


def test_ask_copilot_reports_a_failed_run(mesh, fake, cli, run, repo, capsys):
    cli.on("--output-format", returncode=1, stderr='Error: Model "gpt-x" from --model flag is not available.\n')
    assert mesh.main(["ask", "--repo", str(repo), "--kind", "copilot", "--model", "gpt-x", "--size", "large",
                      "--body", "Where is auth?"]) == 1
    out = capsys.readouterr().out
    assert "── failed from proj-ask-auth ──" in out and "not available" in out
    argv = cli.called("--output-format")[0]["argv"]
    assert argv[argv.index("--reasoning-effort") + 1] == "high" and "--auto-tier" not in argv
    entry = mesh.load_run(run)["agents"]["proj-ask-auth"]
    assert (entry["status"], "pid" in entry) == ("retired", False)

    cli.on("--output-format", stdout="")  # exit 0, nothing said
    assert mesh.main(["ask", "proj-ask-auth", "--body", "Again?"]) == 1
    assert "copilot gave no answer (exit 0)" in capsys.readouterr().out


def test_a_copilot_ask_starts_its_run_in_the_roots_own_mode(mesh, fake, cli, repo, monkeypatch, capsys):
    monkeypatch.setenv("HERDR_ENV", "1")
    monkeypatch.setenv("HERDR_PANE_ID", "w1:p1")
    fake.on("agent", "list", result={"agents": [{"pane_id": "w1:p1", "agent": "claude", "cwd": "/repo"}]})
    cli.on("--output-format", stdout=copilot_jsonl("In auth.py."))
    assert mesh.main(["ask", "--repo", str(repo), "--kind", "copilot", "--body", "Where is auth?"]) == 0
    run_id = capsys.readouterr().err.split("started run ")[1].split()[0]
    assert mesh.load_run(run_id)["mode"] == "claude"


def test_copilot_askers_only_answer_and_stop_leaves_them_be(mesh, fake, cli, run, repo, capsys):
    cli.on("--output-format", stdout=copilot_jsonl("In auth.py."))
    assert mesh.main(["ask", "--repo", str(repo), "--kind", "copilot", "--body", "Where is auth?"]) == 0
    cwd = mesh.load_run(run)["agents"]["proj-ask-auth"]["cwd"]
    with pytest.raises(SystemExit):
        mesh.main(["spawn", "--kind", "copilot", "--repo", str(repo)])
    capsys.readouterr()
    assert mesh.main(["resume", "proj-ask-auth", "--task", "Fix it."]) == 1
    assert "only answers" in capsys.readouterr().err
    assert mesh.resume_argv("copilot", "s1", "auto", "medium") == ["--resume", "s1", "--model", "auto",
                                                                   "--auto-tier", "intelligence"]
    assert mesh.main(["stop"]) == 0
    out = json.loads(capsys.readouterr().out)
    assert "proj-ask-auth" not in out["closed"] and cwd in [w["path"] for w in out["worktrees_removed"]]


def test_run_cli_turns_a_launch_error_into_a_failed_run(monkeypatch):
    module = load_mesh()  # the real run_cli; the `mesh` fixture replaces it with FakeCli

    def too_long(*args, **kwargs):
        raise OSError(7, "Argument list too long")
    monkeypatch.setattr(module.subprocess, "Popen", too_long)
    result = module.run_cli(["copilot", "x"])
    assert result.returncode == 126 and "Argument list too long" in result.stderr


def test_copilot_answer_skips_lines_that_are_not_events(mesh):
    out = '[1, 2]\n"x"\n3\nnot json\n' + copilot_jsonl("Fine.")
    assert mesh.copilot_answer(out) == ("Fine.", "gpt-5.6-luna")


def test_a_busy_copilot_asker_is_not_asked_or_opened_twice(mesh, fake, cli, run, repo, capsys):
    cli.on("--output-format", stdout=copilot_jsonl("In auth.py."))
    assert mesh.main(["ask", "--repo", str(repo), "--kind", "copilot", "--body", "Where is auth?"]) == 0
    with mesh.editing_run(run) as data:
        data["agents"]["proj-ask-auth"].update(status="running", pid=os.getpid())
    capsys.readouterr()
    assert mesh.main(["ask", "proj-ask-auth", "--body", "And sessions?"]) == 1
    assert mesh.main(["open", "proj-ask-auth"]) == 1
    assert capsys.readouterr().err.count("still answering in mesh process") == 2
    assert len(cli.called("--output-format")) == 1

    with mesh.editing_run(run) as data:  # a `running` left by a killed `mesh ask`
        data["agents"]["proj-ask-auth"]["pid"] = 2 ** 22 + 7
    assert mesh.main(["ask", "proj-ask-auth", "--body", "And sessions?"]) == 0
    assert len(cli.called("--output-format")) == 2


def test_only_mesh_ask_reaches_a_copilot_asker(mesh, fake, cli, run, repo, capsys):
    cli.on("--output-format", stdout=copilot_jsonl("In auth.py."))
    assert mesh.main(["ask", "--repo", str(repo), "--kind", "copilot", "--body", "Where is auth?"]) == 0
    before = len(list(mesh.inbox_dir(run, "proj-ask-auth").rglob("*.md")))
    capsys.readouterr()
    for argv in (["send", "proj-ask-auth", "--type", "task", "--body", "Fix it."], ["ring", "proj-ask-auth"],
                 ["switch", "proj-ask-auth", "--model", "auto"], ["approve", "proj-ask-auth"]):
        assert mesh.main(argv) == 1, argv
        assert "headless Copilot asker" in capsys.readouterr().err
    assert len(list(mesh.inbox_dir(run, "proj-ask-auth").rglob("*.md"))) == before
    assert not fake.called("pane", "close")


def test_a_copilot_asker_counts_as_working_until_its_answer_is_filed(mesh, fake, cli, run, repo, capsys):
    seen = []

    def during(argv):
        entry = mesh.load_run(run)["agents"]["proj-ask-auth"]
        seen.append((mesh.agent_status("proj-ask-auth", entry), entry["pid"]))
    cli.on("--output-format", stdout=copilot_jsonl("In auth.py."), effect=during)
    assert mesh.main(["ask", "--repo", str(repo), "--kind", "copilot", "--body", "Where is auth?"]) == 0
    assert seen == [("working", os.getpid())]
    entry = mesh.load_run(run)["agents"]["proj-ask-auth"]
    assert mesh.agent_status("proj-ask-auth", entry) == "gone"
    # The `done` was filed before the entry stopped counting as working, so a watch on it ends with the report.
    events = [e["event"] for e in mesh.read_log(run) if e.get("name") == "proj-ask-auth" or e.get("sender") == "proj-ask-auth"]
    assert events[-1] == "send"


def test_stop_waits_for_a_copilot_asker_that_is_answering(mesh, fake, cli, run, repo, capsys):
    cli.on("--output-format", stdout=copilot_jsonl("In auth.py."))
    assert mesh.main(["ask", "--repo", str(repo), "--kind", "copilot", "--body", "Where is auth?"]) == 0
    cwd = mesh.load_run(run)["agents"]["proj-ask-auth"]["cwd"]
    with mesh.editing_run(run) as data:
        data["agents"]["proj-ask-auth"].update(status="running", pid=os.getpid())
    capsys.readouterr()
    assert mesh.main(["stop"]) == 1
    assert "still answering in mesh process" in capsys.readouterr().err
    assert mesh.run_dir(run).exists() and Path(cwd).exists()
    with mesh.editing_run(run) as data:  # its `mesh ask` was killed: stale, and its peek pane left open
        data["agents"]["proj-ask-auth"].update(pid=2 ** 22 + 7, pane="w1:p7")
    assert mesh.main(["stop"]) == 0 and not Path(cwd).exists()
    assert fake.called("pane", "close", "w1:p7")


def test_a_copilot_left_by_a_killed_ask_keeps_its_asker_busy(mesh, fake, cli, run, repo, capsys):
    seen = []
    cli.on("--output-format", stdout=copilot_jsonl("In auth.py."),
           effect=lambda argv: seen.append(dict(mesh.load_run(run)["agents"]["proj-ask-auth"])))
    assert mesh.main(["ask", "--repo", str(repo), "--kind", "copilot", "--body", "Where is auth?"]) == 0
    assert (seen[0]["pid"], seen[0]["copilot_pid"]) == (os.getpid(), cli.pid)  # recorded while copilot runs
    assert seen[0]["pid_start"] == mesh.proc_start(os.getpid())
    entry = mesh.load_run(run)["agents"]["proj-ask-auth"]
    assert not any(key in entry for key in mesh.PROCESS_KEYS)

    me = os.getpid()  # stands in for the orphaned copilot: alive, though its `mesh ask` is gone
    with mesh.editing_run(run) as data:
        data["agents"]["proj-ask-auth"].update(status="running", pid=2 ** 22 + 7, copilot_pid=me,
                                               copilot_pid_start=mesh.proc_start(me))
    capsys.readouterr()
    assert mesh.main(["ask", "proj-ask-auth", "--body", "And sessions?"]) == 1
    assert mesh.main(["stop"]) == 1
    err = capsys.readouterr().err
    assert err.count(f"still answering in copilot process {me}") == 2 and f"kill -- -{me}" in err
    assert len(cli.called("--output-format")) == 1

    with mesh.editing_run(run) as data:  # a later process at the same pid isn't that copilot
        data["agents"]["proj-ask-auth"]["copilot_pid_start"] = "1"
    assert mesh.agent_status("proj-ask-auth", mesh.load_run(run)["agents"]["proj-ask-auth"]) == "gone"
    assert mesh.main(["ask", "proj-ask-auth", "--body", "And sessions?"]) == 0


def copilot_turn_events(task, answer="", shutdown=True, model="gpt-5.6-luna", content=None):
    """One ask as Copilot writes it to its session's events.jsonl: the prompt, a tool turn, the answer, the exit."""
    events = [{"type": "session.resume", "data": {}},
              {"type": "user.message", "data": {"content": content or f"{task}\n\nYou are not in an agent-mesh session"}},
              {"type": "assistant.message", "data": {"model": model, "content": "", "toolRequests": [{"name": "bash"}]}}]
    if answer:
        events.append({"type": "assistant.message", "data": {"model": model, "content": answer, "phase": "final_answer"}})
    if shutdown:
        events.append({"type": "session.shutdown", "data": {"shutdownType": "routine"}})
    return events


def write_events(path, events):
    path.parent.mkdir(parents=True, exist_ok=True)
    with path.open("a") as f:
        f.writelines(json.dumps(e) + "\n" for e in events)


def orphan_copilot(mesh, run, name, task, events, copilot_pid=2 ** 22 + 8, before=()):
    """`name` as a SIGKILLed `mesh ask` leaves it: claimed after its session holds `before`, `task` recorded and
    unanswered, the entry `running`, and its copilot (dead unless `copilot_pid` says otherwise) having written
    `events` to its session since."""
    write_events(mesh.copilot_events(mesh.load_run(run)["agents"][name]), before)
    with mesh.editing_run(run) as data:
        mesh.claim_copilot(data["agents"][name])
        data["agents"][name].update(pid=2 ** 22 + 7, pid_start=None, copilot_pid=copilot_pid,
                                    copilot_pid_start=mesh.proc_start(copilot_pid))
    root = mesh.load_run(run)["root"]
    mesh.mark_read(mesh.record(run, root, name, "task", task))
    write_events(Path(mesh.load_run(run)["agents"][name]["events"]), events)


def sends_of(mesh, run, name, mtype="done"):
    return [mesh.message_body(run, e).strip() for e in mesh.read_log(run)
            if e.get("event") == "send" and e.get("sender") == name and e.get("type") == mtype]


def test_the_answer_of_a_copilot_left_by_a_killed_ask_is_filed_once(mesh, fake, cli, run, repo, capsys):
    cli.on("--output-format", stdout=copilot_jsonl("In auth.py."))
    assert mesh.main(["ask", "--repo", str(repo), "--kind", "copilot", "--body", "Where is auth?"]) == 0
    orphan_copilot(mesh, run, "proj-ask-auth", "And sessions?",
                   copilot_turn_events("And sessions?", "In sessions.py.", model="gpt-9"))
    capsys.readouterr()
    # `mesh status` derives, never files: it says the answer waits.
    fake.on("agent", "get", result={"agent": {"agent_status": "working"}})  # repo-fix-typo's pane
    assert mesh.main(["status", "--json"]) == 0
    row = next(r for r in json.loads(capsys.readouterr().out)["agents"] if r["name"] == "proj-ask-auth")
    assert row["waiting"] == "report waiting: `mesh inbox` files it" and row["needs"] == "root"
    assert sends_of(mesh, run, "proj-ask-auth") == ["In auth.py."]
    # A watch on it files it and ends with the report instead of taking it for gone.
    assert mesh.watch_agent(run, "proj-ask-auth", interval=0.01) == (
        0, "proj-ask-auth reported done: read it with `mesh inbox`")
    assert "filed proj-ask-auth's `done`" in capsys.readouterr().err
    assert sends_of(mesh, run, "proj-ask-auth") == ["In auth.py.", "In sessions.py."]
    entry = mesh.load_run(run)["agents"]["proj-ask-auth"]
    assert entry["status"] == "retired" and entry["model"] == "gpt-9"
    assert not any(key in entry for key in mesh.CLAIM_KEYS)
    # Whoever comes next finds nothing left to file.
    assert mesh.file_orphan(run, "proj-ask-auth") is None and mesh.main(["inbox"]) == 0
    assert "In sessions.py." in capsys.readouterr().out
    assert sends_of(mesh, run, "proj-ask-auth") == ["In auth.py.", "In sessions.py."]
    assert [e["type"] for e in mesh.read_log(run) if e["event"] == "copilot_filed"] == ["done"]


def test_the_roots_inbox_and_the_next_ask_file_an_orphaned_copilot_answer(mesh, fake, cli, run, repo, capsys):
    cli.on("--output-format", stdout=copilot_jsonl("In auth.py."))
    assert mesh.main(["ask", "--repo", str(repo), "--kind", "copilot", "--body", "Where is auth?"]) == 0
    orphan_copilot(mesh, run, "proj-ask-auth", "And sessions?", copilot_turn_events("And sessions?", "In sessions.py."))
    capsys.readouterr()
    assert mesh.main(["inbox", "--peek"]) == 0  # a peek changes nothing
    assert mesh.load_run(run)["agents"]["proj-ask-auth"]["status"] == "running"
    assert mesh.main(["inbox"]) == 0
    assert "In sessions.py." in capsys.readouterr().out

    orphan_copilot(mesh, run, "proj-ask-auth", "And tokens?", copilot_turn_events("And tokens?", "In tokens.py."))
    cli.on("--output-format", stdout=copilot_jsonl("In cookies.py."))
    assert mesh.main(["ask", "proj-ask-auth", "--body", "And cookies?"]) == 0
    captured = capsys.readouterr()
    assert "filed proj-ask-auth's `done`" in captured.err and "── done from proj-ask-auth ──\nIn cookies.py." in captured.out
    assert sends_of(mesh, run, "proj-ask-auth") == ["In auth.py.", "In sessions.py.", "In tokens.py.", "In cookies.py."]


@pytest.mark.parametrize("before, events, failure", [
    ([], copilot_turn_events("And sessions?", "In sessions.py.", shutdown=False), "stopped before it finished answering"),
    ([], copilot_turn_events("And sessions?", shutdown=True), "gave no answer"),
    ([], copilot_turn_events("Where is auth?", "In auth.py."), "never got the task"),  # another question only
    ([], [], "never got the task"),
    # The same question asked and answered before this ask's claim: not this ask's answer.
    (copilot_turn_events("And sessions?", "In sessions.py."), [{"type": "session.resume", "data": {}}],
     "never got the task"),
    # ...nor when another ask followed it, whose answer isn't this ask's either.
    (copilot_turn_events("And sessions?", "In sessions.py.") + copilot_turn_events("And tokens?", "In tokens.py."), [],
     "never got the task"),
])
def test_an_orphaned_copilot_that_did_not_answer_files_failed(mesh, fake, cli, run, repo, before, events, failure):
    cli.on("--output-format", stdout=copilot_jsonl("In auth.py."))
    assert mesh.main(["ask", "--repo", str(repo), "--kind", "copilot", "--body", "Where is auth?"]) == 0
    orphan_copilot(mesh, run, "proj-ask-auth", "And sessions?", events, before=before)
    assert mesh.file_orphan(run, "proj-ask-auth") == "failed"
    assert failure in sends_of(mesh, run, "proj-ask-auth", "failed")[0]
    assert mesh.load_run(run)["agents"]["proj-ask-auth"]["status"] == "retired"


def test_an_orphaned_copilot_is_filed_only_once_dead_and_only_when_owed(mesh, fake, cli, run, repo, capsys):
    cli.on("--output-format", stdout=copilot_jsonl("In auth.py."))
    assert mesh.main(["ask", "--repo", str(repo), "--kind", "copilot", "--body", "Where is auth?"]) == 0
    # Still answering: busy as before, nothing filed.
    orphan_copilot(mesh, run, "proj-ask-auth", "And sessions?", copilot_turn_events("And sessions?", "In sessions.py."),
                   copilot_pid=os.getpid())
    assert mesh.file_orphan(run, "proj-ask-auth") is None and mesh.main(["inbox"]) == 0
    assert mesh.load_run(run)["agents"]["proj-ask-auth"]["status"] == "running"
    assert sends_of(mesh, run, "proj-ask-auth") == ["In auth.py."]
    # Its `mesh ask` died after filing the answer: only released.
    mesh.record(run, "proj-ask-auth", mesh.load_run(run)["root"], "done", "In sessions.py.")
    with mesh.editing_run(run) as data:
        data["agents"]["proj-ask-auth"]["copilot_pid_start"] = "1"
    assert mesh.file_orphan(run, "proj-ask-auth") is None
    assert mesh.load_run(run)["agents"]["proj-ask-auth"]["status"] == "retired"
    assert sends_of(mesh, run, "proj-ask-auth") == ["In auth.py.", "In sessions.py."]


def test_an_orphaned_copilot_answer_is_read_where_its_ask_had_copilot_write_it(mesh, fake, cli, run, repo,
                                                                               tmp_path, monkeypatch):
    cli.on("--output-format", stdout=copilot_jsonl("In auth.py."))
    assert mesh.main(["ask", "--repo", str(repo), "--kind", "copilot", "--body", "Where is auth?"]) == 0
    orphan_copilot(mesh, run, "proj-ask-auth", "And sessions?",
                   copilot_turn_events("And sessions?", "In sessions.py.")
                   + [{"type": "session.resume", "data": {}},  # past its end: not part of the turn
                      {"type": "assistant.message", "data": {"content": "Something later."}}])
    monkeypatch.setenv("COPILOT_HOME", str(tmp_path / "elsewhere"))  # the root's inbox, run from another shell
    assert mesh.file_orphan(run, "proj-ask-auth") == "done"
    assert sends_of(mesh, run, "proj-ask-auth")[-1] == "In sessions.py."


def test_looking_for_an_orphan_takes_no_lock_while_there_is_none(mesh, fake, cli, run, repo, monkeypatch):
    cli.on("--output-format", stdout=copilot_jsonl("In auth.py."))
    assert mesh.main(["ask", "--repo", str(repo), "--kind", "copilot", "--body", "Where is auth?"]) == 0
    saves = []
    save_run = mesh.save_run
    monkeypatch.setattr(mesh, "save_run", lambda *a: (saves.append(a), save_run(*a)))
    watch = mesh.Watch(run, "proj-ask-auth", grace=60)
    watch.tick(0.01)  # retired
    assert saves == []
    with mesh.editing_run(run) as data:  # answering
        data["agents"]["proj-ask-auth"].update(status="running", pid=os.getpid(), pid_start=mesh.proc_start(os.getpid()))
    saves.clear()
    watch.tick(0.01)
    assert mesh.file_orphan(run, "proj-ask-auth") is None and saves == []


def test_an_interrupted_copilot_ask_files_failed_and_releases_its_asker(mesh, fake, cli, run, repo, capsys):
    def interrupted(argv):
        raise KeyboardInterrupt
    cli.on("--output-format", stdout=copilot_jsonl("In auth.py."), effect=interrupted)
    with pytest.raises(KeyboardInterrupt):
        mesh.main(["ask", "--repo", str(repo), "--kind", "copilot", "--body", "Where is auth?"])
    entry = mesh.load_run(run)["agents"]["proj-ask-auth"]
    assert entry["status"] == "retired" and not any(key in entry for key in mesh.CLAIM_KEYS)
    assert sends_of(mesh, run, "proj-ask-auth", "failed") == [
        "interrupted: its `mesh ask` was stopped before copilot answered"]
    assert mesh.file_orphan(run, "proj-ask-auth") is None


def test_stop_files_an_orphaned_copilot_answer_into_the_archived_run(mesh, fake, cli, run, repo, capsys):
    cli.on("--output-format", stdout=copilot_jsonl("In auth.py."))
    assert mesh.main(["ask", "--repo", str(repo), "--kind", "copilot", "--body", "Where is auth?"]) == 0
    orphan_copilot(mesh, run, "proj-ask-auth", "And sessions?", copilot_turn_events("And sessions?", "In sessions.py."))
    assert mesh.main(["stop"]) == 0
    archive = mesh.mesh_home() / "archive" / run
    assert any("In sessions.py." in p.read_text() for p in archive.rglob("*-proj-ask-auth-done.md"))


def test_pid_alive_tells_a_reused_pid_from_the_process_recorded(mesh):
    me = os.getpid()
    start = mesh.proc_start(me)
    assert start and mesh.proc_start(me) == start and mesh.proc_start(2 ** 22 + 7) is None
    assert mesh.pid_alive(me) and mesh.pid_alive(me, start) and not mesh.pid_alive(me, start + "0")
    assert not mesh.pid_alive(2 ** 22 + 7) and not mesh.pid_alive(None)


def test_run_cli_starts_a_recorded_cli_in_its_own_session_and_kills_its_group():
    module = load_mesh()  # the real run_cli
    sessions = []
    result = module.run_cli(["sh", "-c", "cat"], input="hi", started=lambda pid: sessions.append((pid, os.getsid(pid))))
    assert result.stdout == "hi" and sessions[0][0] == sessions[0][1]
    started = []
    result = module.run_cli(["sh", "-c", "sleep 30 & wait"], timeout=0.5, started=started.append)
    assert result.returncode == 124 and group_gone(started[0])  # its `sleep` went with it


def group_gone(pgid, within=3.0):
    """Whether process group `pgid` empties within a few seconds; zombies (not reaped yet) don't count."""
    import time
    deadline = time.monotonic() + within
    while time.monotonic() < deadline:
        try:
            os.killpg(pgid, 0)
        except ProcessLookupError:
            return True
        live = []
        for stat in Path("/proc").glob("[0-9]*/stat"):
            try:
                fields = stat.read_text().rsplit(")", 1)[1].split()
            except (OSError, IndexError):
                continue
            if fields[2] == str(pgid) and fields[0] != "Z":
                live.append(stat)
        if Path("/proc/self").exists() and not live:
            return True
        time.sleep(0.05)
    return False


def test_run_cli_timeout_does_not_wait_for_a_grandchild_holding_its_pipes():
    import time
    module = load_mesh()
    for started in (None, lambda pid: None):  # `setsid`: out of the group a recorded CLI's kill reaches
        begun = time.monotonic()
        result = module.run_cli(["sh", "-c", "setsid sleep 5 & sleep 5"], timeout=1, started=started)
        assert result.returncode == 124 and time.monotonic() - begun < 3


def test_a_sigterm_to_mesh_ask_takes_its_copilot_group_with_it():
    import signal
    module = load_mesh()
    before = signal.getsignal(signal.SIGTERM)
    started = []

    def record_and_get_killed(pid):
        started.append(pid)
        os.kill(os.getpid(), signal.SIGTERM)  # someone kills `mesh ask` while copilot answers
    with pytest.raises(SystemExit) as exc:
        with module.signals_raise(signal.SIGTERM, signal.SIGHUP):
            module.run_cli(["sh", "-c", "sleep 30 & wait"], started=record_and_get_killed)
    assert exc.value.code == 128 + signal.SIGTERM and group_gone(started[0])
    assert signal.getsignal(signal.SIGTERM) == before


def test_peek_shows_what_a_copilot_asker_is_doing(mesh, fake, cli, run, repo, tmp_path, monkeypatch, capsys):
    monkeypatch.setenv("COPILOT_HOME", str(tmp_path / "copilot"))
    cli.on("--output-format", stdout=copilot_jsonl("In auth.py."))
    assert mesh.main(["ask", "--repo", str(repo), "--kind", "copilot", "--body", "Where is auth?"]) == 0
    session = mesh.load_run(run)["agents"]["proj-ask-auth"]["session"]
    capsys.readouterr()
    assert mesh.main(["peek", "proj-ask-auth"]) == 1
    assert "no Copilot session yet" in capsys.readouterr().err
    events = tmp_path / "copilot" / "session-state" / session / "events.jsonl"
    events.parent.mkdir(parents=True)
    events.write_text("".join(json.dumps(e) + "\n" for e in [
        {"type": "tool.execution_start", "timestamp": "2026-10-25T06:35:16.000Z",
         "data": {"toolName": "bash", "arguments": {"command": "uvx pytest -q", "description": "Run the tests"}}},
        {"type": "tool.execution_start", "timestamp": "2026-10-25T06:35:20.000Z",
         "data": {"toolName": "view", "arguments": {"path": "/wt/bin/mesh"}}},
        {"type": "assistant.message", "timestamp": "2026-10-25T06:35:49.000Z", "data": {"content": "done\n\n1. x"}},
        [1, 2],
    ]) + '{"type": "assistant.mess')  # a line Copilot is still writing
    assert mesh.main(["peek", "proj-ask-auth", "--lines", "2"]) == 0
    assert capsys.readouterr().out.splitlines() == ["06:35:20 view: /wt/bin/mesh", "06:35:49 says: done"]
    assert mesh.main(["peek", "proj-ask-auth", "--follow"]) == 0  # it isn't answering, so --follow ends at once
    assert "is not answering now" in capsys.readouterr().out
    assert mesh.main(["peek", "repo-fix-typo"]) == 1
    assert "herdr agent read repo-fix-typo" in capsys.readouterr().err


def test_a_copilot_asker_gets_a_peek_pane_beside_the_root_while_it_answers(mesh, fake, cli, run, repo, monkeypatch,
                                                                         capsys):
    monkeypatch.setenv("MESH_SIDE_PANES", "2")
    fake.on("pane", "layout", result={"layout": {"panes": [{"pane_id": "w1:p1", "rect": {"width": 200, "height": 50}}]}})
    fake.on("pane", "split", result={"pane": {"pane_id": "w1:p2"}})
    seen = []

    def during(argv):
        seen.append((mesh.load_run(run)["agents"]["proj-ask-auth"]["pane"], bool(fake.called("pane", "close"))))
    cli.on("--output-format", stdout=copilot_jsonl("In auth.py."), effect=during)
    assert mesh.main(["ask", "--repo", str(repo), "--kind", "copilot", "--body", "Where is auth?"]) == 0
    assert "follow it in pane w1:p2" in capsys.readouterr().err
    assert seen == [("w1:p2", False)]  # open while it answers
    [ran] = fake.called("pane", "run")
    assert ran == ("pane", "run", "w1:p2", "mesh peek proj-ask-auth --from-run mtest --follow")
    assert fake.called("pane", "split")[-1][2] == "w1:p1" and fake.called("pane", "rename", "w1:p2")
    entry = mesh.load_run(run)["agents"]["proj-ask-auth"]
    assert fake.called("pane", "close", "w1:p2") and (entry["pane"], entry["tab"]) == (None, None)

    # herdr refuses the command: the split is closed again and the hint is the way to follow it
    fake.on("pane", "run", error="pane_not_found")
    fake.on("pane", "split", result={"pane": {"pane_id": "w1:p3"}})
    assert mesh.main(["ask", "proj-ask-auth", "--body", "And sessions?"]) == 0
    assert "follow it with `mesh peek proj-ask-auth --follow`" in capsys.readouterr().err
    assert fake.called("pane", "close", "w1:p3")

    # no room beside the root: no pane anywhere else
    monkeypatch.setenv("MESH_SIDE_PANES", "0")
    splits = len(fake.called("pane", "split"))
    assert mesh.main(["ask", "proj-ask-auth", "--body", "And tokens?"]) == 0
    assert len(fake.called("pane", "split")) == splits and not fake.called("tab", "create")


def test_a_peek_pane_is_closed_when_copilot_fails_and_a_stale_one_when_it_is_asked_again(mesh, fake, cli, run, repo,
                                                                                         monkeypatch, capsys):
    monkeypatch.setenv("MESH_SIDE_PANES", "3")
    fake.on("pane", "layout", result={"layout": {"panes": [{"pane_id": "w1:p1", "rect": {"width": 200, "height": 50}}]}})
    fake.on("pane", "split", result={"pane": {"pane_id": "w1:p2"}})
    cli.on("--output-format", stdout=copilot_jsonl("In auth.py."))
    assert mesh.main(["ask", "--repo", str(repo), "--kind", "copilot", "--body", "Where is auth?"]) == 0

    def boom(*args, **kwargs):
        raise mesh.MeshError("copilot vanished")
    real = mesh.copilot_ask
    monkeypatch.setattr(mesh, "copilot_ask", boom)
    fake.on("pane", "split", result={"pane": {"pane_id": "w1:p3"}})
    assert mesh.main(["ask", "proj-ask-auth", "--body", "And sessions?"]) == 1
    assert fake.called("pane", "close", "w1:p3")  # the finally path
    monkeypatch.setattr(mesh, "copilot_ask", real)

    with mesh.editing_run(run) as data:  # a `mesh ask` killed mid-answer left its pane and a dead pid
        data["agents"]["proj-ask-auth"].update(status="running", pid=2 ** 22 + 7, pane="w1:p7", tab="w1:t1",
                                               workspace="w1")
    fake.on("pane", "split", result={"pane": {"pane_id": "w1:p8"}})
    assert mesh.main(["ask", "proj-ask-auth", "--body", "And tokens?"]) == 0
    closed = [c[2] for c in fake.called("pane", "close")]
    assert closed[-2:] == ["w1:p7", "w1:p8"]  # the stale one before the new one opened, the new one after
    assert mesh.load_run(run)["agents"]["proj-ask-auth"]["pane"] is None
    capsys.readouterr()


def test_panes_beside_the_root_are_counted_split_and_recorded_under_one_lock(mesh, fake, run, monkeypatch):
    import fcntl
    monkeypatch.setenv("MESH_SIDE_PANES", "2")
    fake.on("pane", "layout", result={"layout": {"panes": [{"pane_id": "w1:p1", "rect": {"width": 200, "height": 50}}]}})
    held = []

    def split_at(target, cwd, env):
        with open(mesh.run_dir(run) / ".panes.lock", "w") as fh:  # what a parallel `mesh` call would try
            try:
                fcntl.flock(fh, fcntl.LOCK_EX | fcntl.LOCK_NB)
                held.append(False)
            except BlockingIOError:
                held.append(True)
        assert mesh.load_run(run)["agents"]["repo-fix-typo"]["pane"] == "w2:p5"  # not recorded yet
        return "w1:p2"
    monkeypatch.setattr(mesh, "split_at", split_at)
    assert mesh.beside_root(run, "repo-fix-typo", Path("/x"), {}) == ("w1:p2", "w1:t1", "w1")
    assert held == [True]
    # recorded before the lock is let go, so the next placement counts it
    assert mesh.load_run(run)["agents"]["repo-fix-typo"]["pane"] == "w1:p2"


def test_peek_follow_waits_for_the_copilot_session_to_start(mesh, fake, cli, run, repo, tmp_path, monkeypatch, capsys):
    monkeypatch.setenv("COPILOT_HOME", str(tmp_path / "copilot"))
    cli.on("--output-format", stdout=copilot_jsonl("In auth.py."))
    assert mesh.main(["ask", "--repo", str(repo), "--kind", "copilot", "--body", "Where is auth?"]) == 0
    with mesh.editing_run(run) as data:
        data["agents"]["proj-ask-auth"].update(status="running", pid=os.getpid())
    session = mesh.load_run(run)["agents"]["proj-ask-auth"]["session"]
    events = tmp_path / "copilot" / "session-state" / session / "events.jsonl"
    sleeps = []

    def sleep(_):
        sleeps.append(events.exists())
        if not events.exists():  # Copilot starts the session
            events.parent.mkdir(parents=True)
            events.write_text(json.dumps({"type": "assistant.message", "timestamp": "2026-10-25T06:35:49.000Z",
                                          "data": {"content": "Looking."}}) + "\n")
        else:  # and answers
            with mesh.editing_run(run) as data:
                data["agents"]["proj-ask-auth"]["status"] = "retired"
    monkeypatch.setattr(mesh.time, "sleep", sleep)
    capsys.readouterr()
    assert mesh.main(["peek", "proj-ask-auth", "--follow"]) == 0
    assert sleeps == [False, True]
    assert capsys.readouterr().out.splitlines() == ["06:35:49 says: Looking.",
                                                    "(proj-ask-auth is not answering now: its answer is what "
                                                    "`mesh ask` printed, or `mesh inbox`)"]


# ── root context: measuring, resetting and briefing the root ─────────────


def root_transcript_file(mesh, monkeypatch, tokens, idle=60, session="root-s"):
    """A Claude root session file whose last reply had `tokens` of prompt, last written `idle` seconds ago."""
    import os
    import time
    monkeypatch.setenv("CLAUDE_CODE_SESSION_ID", session)
    path = mesh.claude_sessions_dir("/repo") / f"{session}.jsonl"
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(usage(tokens - 12)) + "\n")
    os.utime(path, (time.time() - idle, time.time() - idle))
    return path


def test_transcript_tokens_reads_backwards_across_chunks(mesh, tmp_path):
    path = tmp_path / "s.jsonl"
    filler = {"type": "user", "message": {"content": "x" * 300}}
    lines = [usage(1000), filler, {"type": "system", "subtype": "compact_boundary"}, filler, usage(7000),
             *[filler] * 5]
    path.write_text("".join(json.dumps(line) + "\n" for line in lines))
    assert mesh.transcript_tokens(path, chunk=64) == 7012  # the newest reply, though cut across many chunks
    path.write_text("".join(json.dumps(line) + "\n" for line in lines[:4]))
    assert mesh.transcript_tokens(path, chunk=64) == 0  # compacted since its last reply
    path.write_text(json.dumps(filler) + "\n")
    assert mesh.transcript_tokens(path, chunk=64) is None
    assert mesh.transcript_tokens(tmp_path / "missing.jsonl") is None


def test_root_context_finds_a_session_file_outside_its_cwd(mesh, run, monkeypatch):
    monkeypatch.setenv("CLAUDE_CODE_SESSION_ID", "elsewhere")
    path = mesh.claude_sessions_dir("/somewhere/else") / "elsewhere.jsonl"
    path.parent.mkdir(parents=True)
    path.write_text(json.dumps(usage(988)) + "\n")
    context = mesh.root_context(mesh.load_run(run), as_root=True)
    assert (context["tokens"], context["transcript"], context["cold"]) == (1000, str(path), False)
    monkeypatch.delenv("CLAUDE_CODE_SESSION_ID")
    assert mesh.root_context(mesh.load_run(run), as_root=True) is None  # no session anywhere


def test_root_line_weighs_size_against_cache_state(mesh, run, monkeypatch):
    root_transcript_file(mesh, monkeypatch, 400_000, idle=60)
    assert mesh.root_line(run)[0] is None  # warm and under twice the threshold: not worth a reset yet
    line = mesh.root_line(run, about_to_wait=True)[0]
    assert "root context 400k, cache warm" in line and "about to wait" in line and "mesh reset --next" in line
    root_transcript_file(mesh, monkeypatch, 400_000, idle=4000)
    line = mesh.root_line(run)[0]
    assert "cache cold since" in line and "re-reads all of it" in line
    root_transcript_file(mesh, monkeypatch, 700_000, idle=60)
    assert "next quiet point" in mesh.root_line(run)[0]
    root_transcript_file(mesh, monkeypatch, 200_000, idle=4000)
    assert mesh.root_line(run)[0] is None  # small: cold or not, a reset saves little
    monkeypatch.setenv("MESH_ROOT_RESET_AT", "100000")
    assert "cache cold" in mesh.root_line(run)[0]


def test_root_line_waits_for_what_waits_on_the_root(mesh, run, monkeypatch):
    mesh.store_message(run, "repo-fix-typo", "repo-lead", "plan", "my plan")
    root_transcript_file(mesh, monkeypatch, 400_000, idle=60)
    assert mesh.root_line(run, about_to_wait=True)[0] is None  # a plan to review first: not about to wait
    root_transcript_file(mesh, monkeypatch, 400_000, idle=4000)
    assert "once you've handled a plan from repo-fix-typo" in mesh.root_line(run)[0]


def test_root_line_shows_a_failed_reset_and_nothing_while_one_is_pending(mesh, run, monkeypatch):
    import fcntl
    mesh.log_event(run, "reset_failed", name="repo-lead", reason="gave up after waiting 30m: a draft is in its input box")
    assert "the last `mesh reset` failed" in mesh.root_line(run)[0] and "a draft" in mesh.root_line(run)[0]
    with mesh.reset_lock_path(run).open("a") as handle:
        fcntl.flock(handle, fcntl.LOCK_EX)
        assert mesh.root_line(run)[0] is None


def test_status_header_and_root_commands_show_the_root_context(mesh, fake, run, monkeypatch, capsys):
    fake.on("agent", "get", result={"agent": {"agent_status": "idle"}})
    root_transcript_file(mesh, monkeypatch, 400_000, idle=4000)
    assert mesh.main(["status"]) == 0
    out = capsys.readouterr()
    assert "root context 400k, cache cold since" in out.out.splitlines()[0]
    assert "mesh reset" in out.err
    assert mesh.main(["status", "--json"]) == 0
    assert json.loads(capsys.readouterr().out)["root_context"]["tokens"] == 400_000
    assert mesh.main(["inbox"]) == 0
    assert "mesh reset" in capsys.readouterr().err
    mesh.warn_unwatched(run)
    assert "cache cold" in capsys.readouterr().err
    assert mesh.main(["inbox", "--as", "repo-fix-typo"]) == 0
    assert "mesh reset" not in capsys.readouterr().err  # a worker's command says nothing of the root


def test_warn_unwatched_says_a_big_warm_root_is_about_to_wait(mesh, run, monkeypatch, capsys):
    root_transcript_file(mesh, monkeypatch, 400_000, idle=60)
    mesh.warn_unwatched(run)
    assert "about to wait on workers" in capsys.readouterr().err


class FakePopen:
    started: list = []

    def __init__(self, argv, **kwargs):
        self.pid = 4321
        FakePopen.started.append((argv, kwargs))


def test_reset_starts_a_detached_process_holding_the_lock(mesh, run, monkeypatch, capsys):
    FakePopen.started = []
    monkeypatch.setattr(mesh.subprocess, "Popen", FakePopen)
    assert mesh.main(["reset", "--next", "review PR 31"]) == 0
    out = json.loads(capsys.readouterr().out)
    assert out["pending"] and out["pid"] == 4321 and "end your turn" in out["then"]
    (argv, kwargs), = FakePopen.started
    assert argv[2:6] == ["reset", "--run", run, "--as"] and "--detached" in argv
    assert argv[argv.index("--next") + 1] == "review PR 31" and "--compact" not in argv
    assert kwargs["start_new_session"] and kwargs["pass_fds"]
    assert "CLAUDE_CODE_SESSION_ID" not in kwargs["env"]
    event = mesh.read_log(run)[-1]
    assert (event["event"], event["via"], event["next"]) == ("reset_requested", "command", "review PR 31")
    assert mesh.main(["reset", "--compact", "keep the PR list"]) == 0
    argv = FakePopen.started[-1][0]
    assert argv[argv.index("--compact") + 1] == "keep the PR list"


def test_reset_refuses_a_worker_a_waiting_plan_and_a_pending_reset(mesh, run, monkeypatch, capsys):
    import fcntl
    monkeypatch.setattr(mesh.subprocess, "Popen", FakePopen)
    assert mesh.main(["reset", "--as", "repo-fix-typo"]) == 1
    assert "only a run's root" in capsys.readouterr().err
    path = mesh.store_message(run, "repo-fix-typo", "repo-lead", "question", "which field?")
    assert mesh.main(["reset"]) == 1
    assert "a question from repo-fix-typo waits on you" in capsys.readouterr().err
    mesh.mark_read(path)
    with mesh.reset_lock_path(run).open("a") as handle:
        fcntl.flock(handle, fcntl.LOCK_EX)
        assert mesh.main(["reset"]) == 1
        assert "already pending" in capsys.readouterr().err


IDLE_BOX = "\n".join(claude_pane("❯\xa0"))


def scripted_root(mesh, fake, monkeypatch, working=0, clears=True, status="idle", kind="claude"):
    """herdr's view of the root's pane: `working` reads of working, then `status`; its session turns from
    `old` to `new` once /clear has been typed into it (when `clears`)."""
    reads = {"n": 0}

    def agent(pane):
        reads["n"] += 1
        cleared = clears and any(c[:4] == ("agent", "prompt", pane, "/clear") for c in fake.calls)
        return {"agent": kind, "agent_status": "working" if reads["n"] <= working else status,
                "agent_session": {"value": "new" if cleared else "old"}}

    monkeypatch.setattr(mesh, "pane_agent", agent)
    fake.screen("w1:p1", IDLE_BOX)
    return reads


def prompts(fake):
    return [c[3] for c in fake.called("agent", "prompt")]


def test_reset_root_waits_for_the_turn_to_end_then_clears_and_bootstraps(mesh, fake, run, monkeypatch):
    scripted_root(mesh, fake, monkeypatch, working=3)
    assert mesh.reset_root(run, "repo-lead", "review PR 31", None) == 0
    clear, bootstrap = prompts(fake)
    assert clear == "/clear"
    assert bootstrap.startswith("📬 [mesh] (ref ") and "You are repo-lead, the root of agent-mesh run mtest" in bootstrap
    assert "no parent to report to" in bootstrap and "Run `mesh brief`" in bootstrap
    assert bootstrap.endswith("Then: review PR 31")
    assert "--wait" not in fake.called("agent", "prompt")[0]  # a clear is no turn: herdr would call it stalled
    event = mesh.read_log(run)[-1]
    assert (event["event"], event["old_session"], event["new_session"]) == ("reset", "old", "new")


def test_reset_root_never_types_into_a_working_root(mesh, fake, run, monkeypatch):
    monkeypatch.setenv("MESH_RESET_WAIT", "0")
    scripted_root(mesh, fake, monkeypatch, working=10 ** 6)
    assert mesh.reset_root(run, "repo-lead", None, None) == 1
    assert prompts(fake) == []
    event = mesh.read_log(run)[-1]
    assert event["event"] == "reset_failed" and "it reads working" in event["reason"]


def test_reset_root_holds_on_a_draft_but_not_on_ghost_text(mesh, fake, run, monkeypatch):
    monkeypatch.setenv("MESH_RESET_WAIT", "0")
    scripted_root(mesh, fake, monkeypatch)
    fake.screen("w1:p1", "\n".join(claude_pane("❯\xa0hello draft")))
    assert mesh.reset_root(run, "repo-lead", None, None) == 1
    assert prompts(fake) == [] and "a draft is in its input box" in mesh.read_log(run)[-1]["reason"]
    # Claude's prompt suggestion after a turn is dim ghost text, like its "Try …" placeholder: an empty box
    # (the row as captured from a live root, Claude Code 2.1.310).
    monkeypatch.delenv("MESH_RESET_WAIT")
    fake.screen("w1:p1", "\n".join(claude_pane("❯\xa0\x1b[0m\x1b[2mrespawn repo-tw and ask for its report\x1b[0m")))
    assert mesh.reset_root(run, "repo-lead", None, None) == 0
    assert prompts(fake)[0] == "/clear"


def test_reset_root_holds_on_an_unreadable_box(mesh, fake, run, monkeypatch):
    monkeypatch.setenv("MESH_RESET_WAIT", "0")
    scripted_root(mesh, fake, monkeypatch)
    fake.screen("w1:p1", "no input box here")
    assert mesh.reset_root(run, "repo-lead", None, None) == 1
    assert prompts(fake) == [] and "can't be read" in mesh.read_log(run)[-1]["reason"]


def test_reset_root_is_cancelled_by_a_plan_that_arrives(mesh, fake, run, monkeypatch):
    scripted_root(mesh, fake, monkeypatch)
    mesh.store_message(run, "repo-fix-typo", "repo-lead", "plan", "my plan")
    assert mesh.reset_root(run, "repo-lead", None, None) == 1
    assert prompts(fake) == []
    event = mesh.read_log(run)[-1]
    assert (event["event"], event["reason"]) == ("reset_cancelled", "a plan from repo-fix-typo arrived")


def test_reset_root_types_no_bootstrap_when_the_clear_doesnt_show(mesh, fake, run, monkeypatch):
    scripted_root(mesh, fake, monkeypatch, clears=False)
    assert mesh.reset_root(run, "repo-lead", None, None) == 1
    assert prompts(fake) == ["/clear"]
    assert "didn't show up as a new session" in mesh.read_log(run)[-1]["reason"]


def agy_index(mesh, rows):
    """agy's conversation index, as `agy_summaries` reads it: (id, first prompt) rows."""
    import sqlite3
    path = mesh.Path(mesh.os.environ["MESH_AGY_SUMMARIES"])
    path.parent.mkdir(parents=True, exist_ok=True)
    if path.exists():
        path.unlink()
    db = sqlite3.connect(path)
    db.execute("CREATE TABLE conversation_summaries (conversation_id, preview, workspace_uris, nesting_depth, "
               "step_count, last_modified_time)")
    for n, (conversation, preview) in enumerate(rows):
        db.execute("INSERT INTO conversation_summaries VALUES (?, ?, '[]', 0, 1, ?)", (conversation, preview, n))
    db.commit()
    db.close()


def index_on_bootstrap(mesh, monkeypatch, root_conversation, others=()):
    """agy indexes the root's new conversation once the bootstrap is its first prompt; `others` are there anyway."""
    agy_index(mesh, [(c, "someone else") for c in others])
    real_ring = mesh.ring

    def ring(targets, text, token, **kw):
        result = real_ring(targets, text, token, **kw)
        agy_index(mesh, [*((c, "someone else") for c in others),
                         (root_conversation, f"📬 [mesh] (ref {token}) You are repo-lead…")])
        return result

    monkeypatch.setattr(mesh, "ring", ring)


def test_reset_root_of_an_agy_root_confirms_by_a_new_conversation(mesh, fake, run, monkeypatch):
    with mesh.editing_run(run) as data:
        data["agents"]["repo-lead"]["kind"] = "agy"
    scripted_root(mesh, fake, monkeypatch, clears=False, kind="agy")
    folder = mesh.agy_conversation_file("x").parent
    folder.mkdir(parents=True)
    (folder / "before.db").write_text("")
    real = mesh.agy_conversations
    monkeypatch.setattr(mesh, "agy_conversations", lambda: real() | ({"after"} if prompts(fake) else set()))
    index_on_bootstrap(mesh, monkeypatch, "after")
    assert mesh.reset_root(run, "repo-lead", None, None) == 0
    assert mesh.load_run(run)["agents"]["repo-lead"]["session"] == "after"  # measured from now on
    assert "Then: carry on from the next step in your notes" in prompts(fake)[1]


def test_reset_root_of_an_agy_root_doesnt_take_a_lone_new_conversation_on_trust(mesh, fake, run, monkeypatch):
    agy_root(mesh, run)
    scripted_root(mesh, fake, monkeypatch, clears=False, kind="agy")
    # Another agy's conversation shows first; the root's own is indexed only after its bootstrap.
    monkeypatch.setattr(mesh, "agy_conversations", lambda: {"a", "other"} if prompts(fake) else {"a"})
    index_on_bootstrap(mesh, monkeypatch, "mine", others=("other",))
    assert mesh.reset_root(run, "repo-lead", None, None) == 0
    assert mesh.load_run(run)["agents"]["repo-lead"]["session"] == "mine"


def test_reset_root_of_an_agy_root_records_nothing_when_the_lone_new_one_isnt_its_own(mesh, fake, run,
                                                                                       monkeypatch):
    agy_root(mesh, run)
    scripted_root(mesh, fake, monkeypatch, clears=False, kind="agy")
    monkeypatch.setattr(mesh, "agy_conversations", lambda: {"a", "other"} if prompts(fake) else {"a"})
    agy_index(mesh, [("other", "someone else")])  # the root's own never gets indexed in time
    assert mesh.reset_root(run, "repo-lead", None, None) == 0
    assert mesh.load_run(run)["agents"]["repo-lead"]["session"] is None  # the lone one wasn't the root's
    assert "couldn't tell" in mesh.read_log(run)[-1]["note"]


def test_reset_root_with_compact_waits_for_the_compaction(mesh, fake, run, monkeypatch):
    scripted_root(mesh, fake, monkeypatch, clears=False)
    assert mesh.reset_root(run, "repo-lead", None, "keep the PR list") == 0
    compact = fake.called("agent", "prompt")[0]
    assert compact[3] == "/compact keep the PR list" and "--until" in compact
    assert fake.called("agent", "wait")
    assert "compacted your session" in prompts(fake)[1]


def test_reset_root_reports_a_bootstrap_that_doesnt_get_through(mesh, fake, run, monkeypatch):
    scripted_root(mesh, fake, monkeypatch)
    real = fake.__call__

    def herdr(*args, timeout=None):
        if args[:2] == ("agent", "prompt") and args[3] != "/clear":
            raise mesh.HerdrError("agent_not_found", "gone")
        return real(*args, timeout=timeout)

    monkeypatch.setattr(mesh, "herdr", herdr)
    assert mesh.reset_root(run, "repo-lead", None, None) == 1
    assert "didn't get through" in mesh.read_log(run)[-1]["reason"]


def autoreset_root(mesh, fake, monkeypatch, tokens=400_000, idle=4000, status="idle"):
    import os
    import time
    path = mesh.claude_sessions_dir("/repo") / "root-s.jsonl"
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(usage(tokens - 12)) + "\n")
    os.utime(path, (time.time() - idle, time.time() - idle))
    monkeypatch.setattr(mesh, "pane_agent", lambda pane: {"agent": "claude", "agent_status": status,
                                                          "agent_session": {"value": "root-s"}})
    started = []
    monkeypatch.setattr(mesh, "start_reset", lambda *a, **k: started.append((a, k)) or 1)
    return started


def test_autoreset_resets_a_big_idle_cold_root_owed_nothing(mesh, fake, run, monkeypatch):
    started = autoreset_root(mesh, fake, monkeypatch)
    assert mesh.maybe_autoreset(run, mesh.load_run(run), mesh.read_log(run))
    (args, kwargs), = started
    assert args[1] == "repo-lead" and kwargs["via"] == "watch"
    mesh.log_event(run, "reset_requested", name="repo-lead", via="watch")
    assert not mesh.maybe_autoreset(run, mesh.load_run(run), mesh.read_log(run))  # once per cache lifetime


@pytest.mark.parametrize("change", ["warm", "small", "working", "mail"])
def test_autoreset_leaves_a_root_alone(mesh, fake, run, monkeypatch, change):
    started = autoreset_root(mesh, fake, monkeypatch, idle=60 if change == "warm" else 4000,
                             tokens=100_000 if change == "small" else 400_000,
                             status="working" if change == "working" else "idle")
    if change == "mail":  # any unread report holds it: it acts without the root's judgment
        mesh.store_message(run, "repo-fix-typo", "repo-lead", "done", "PR https://github.com/o/r/pull/3")
    assert not mesh.maybe_autoreset(run, mesh.load_run(run), mesh.read_log(run)) and not started


def test_only_the_roots_own_watch_autoresets(mesh, fake, run, monkeypatch, capsys):
    seen = []
    monkeypatch.setattr(mesh, "watch_agents", lambda *a, **k: seen.append(k["autoreset"]) or (0, "ok"))
    assert mesh.main(["watch", "repo-fix-typo"]) == 0
    monkeypatch.setenv("MESH_ROOT_AUTORESET", "1")
    assert mesh.main(["watch", "repo-fix-typo"]) == 0
    assert mesh.main(["watch", "repo-fix-typo", "--as", "repo-fix-typo"]) == 0
    assert seen == [False, True, False]


def test_brief_is_bounded_and_says_where_the_rest_is(mesh, fake, run, monkeypatch, capsys):
    folder = mesh.run_dir(run)
    (folder / "notes.md").write_text("- the user wants Copilot on every review\n- next: merge order\n")
    (folder / "board.md").write_text("| task | repo |\n" + "| a row of the board |\n" * 400)
    for n in range(12):
        mesh.store_message(run, "repo-fix-typo", "repo-lead", "info", f"note {n}\nmore lines")
    mesh.log_event(run, "reset", name="repo-lead", next="review PR 31", transcript="/old/root-s.jsonl")
    fake.on("agent", "get", result={"agent": {"agent_status": "working"}})
    assert mesh.main(["brief"]) == 0
    out = capsys.readouterr().out
    assert "you are the run's root: follow ~/.config/agent-mesh/ORCHESTRATE.md" in out
    assert "the user wants Copilot on every review" in out
    assert "more characters: cat " + str(folder / "board.md") in out and len(out) < 9000
    assert "- repo-fix-typo (agy ?, large, pane): working; no task" in out
    assert "info from repo-fix-typo at" in out and "note 0" in out and "more lines" not in out
    assert "… 2 more: mesh inbox --peek" in out
    assert "next, as you said then: review PR 31" in out and "/old/root-s.jsonl. An untrusted reference" in out
    assert mesh.unread(run, "repo-lead")  # a brief reads no mail
    assert mesh.main(["brief", "--json"]) == 0
    data = json.loads(capsys.readouterr().out)
    assert data["notes"]["path"] == str(folder / "notes.md") and data["last_reset"]["next"] == "review PR 31"


def test_brief_without_notes_says_to_write_them(mesh, fake, run, capsys):
    fake.on("agent", "get", result={"agent": {"agent_status": "idle"}})
    assert mesh.main(["brief"]) == 0
    out = capsys.readouterr().out
    assert "## notes (" in out and "none yet" in out and "## board" not in out


def herdr_root_session(fake, session):
    """What herdr's Claude integration reports for the root's pane (w1:p1)."""
    fake.on("agent", "get", "w1:p1", result={"agent": {"agent": "claude", "agent_status": "idle",
                                                        "agent_session": {"value": session}}})


def test_a_watch_that_outlived_a_reset_measures_the_new_session(mesh, fake, run, monkeypatch):
    root_transcript_file(mesh, monkeypatch, 450_000, idle=5000, session="old-s")  # the watch's own environment
    root_transcript_file(mesh, monkeypatch, 30_000, idle=10, session="new-s")
    monkeypatch.setenv("CLAUDE_CODE_SESSION_ID", "old-s")
    herdr_root_session(fake, "new-s")
    assert mesh.root_context(mesh.load_run(run), as_root=True)["tokens"] == 30_000
    assert mesh.root_line(run)[0] is None  # no second reset for the fresh root
    herdr_root_session(fake, "old-s")  # herdr agrees: the environment's session is the root's
    assert mesh.root_context(mesh.load_run(run), as_root=True)["tokens"] == 450_000


def test_status_from_another_session_or_a_terminal_shows_the_roots_context(mesh, fake, run, monkeypatch, capsys):
    root_transcript_file(mesh, monkeypatch, 620_000, idle=30, session="worker-s")
    root_transcript_file(mesh, monkeypatch, 80_000, idle=30, session="root-s")
    fake.on("agent", "get", result={"agent": {"agent_status": "idle"}})
    herdr_root_session(fake, "root-s")
    monkeypatch.setenv("CLAUDE_CODE_SESSION_ID", "worker-s")
    assert mesh.main(["status", "--as", "repo-fix-typo"]) == 0  # a Claude worker asking
    assert "root context 80k" in capsys.readouterr().out
    monkeypatch.delenv("CLAUDE_CODE_SESSION_ID")
    monkeypatch.delenv("HERDR_PANE_ID")  # a plain terminal: not in the run at all
    assert mesh.main(["status", run, "--json"]) == 0
    assert json.loads(capsys.readouterr().out)["root_context"]["tokens"] == 80_000


def test_caller_names_only_an_agent_of_the_named_run(mesh, fake, run, monkeypatch):
    other = mesh.run_dir("mold")
    other.mkdir(parents=True)
    mesh.save_run("mold", {"id": "mold", "mode": "agy", "root": "repo-lead", "request": "",
                           "agents": {"repo-lead": {"role": "root", "kind": "claude", "pane": "w9:p9"}}})
    before = (other / "run.json").read_text()
    assert mesh.caller(argparse_ns(), "mold") == ("mold", None)  # the pane is mtest's root, not mold's
    assert mesh.caller(argparse_ns(), run) == (run, "repo-lead")
    assert (other / "run.json").read_text() == before


def argparse_ns(**kw):
    import argparse
    return argparse.Namespace(run=None, as_=None, **kw)


def test_a_failed_reset_is_shown_once(mesh, fake, run, monkeypatch, capsys):
    mesh.log_event(run, "reset_cancelled", name="repo-lead", reason="a plan from repo-fix-typo arrived")
    mesh.root_hint(run)
    assert "was cancelled" in capsys.readouterr().err
    mesh.root_hint(run)
    assert capsys.readouterr().err == ""
    assert mesh.read_log(run)[-1]["event"] == "reset_noted"


def agy_root(mesh, run):
    with mesh.editing_run(run) as data:
        data["agents"]["repo-lead"].update(kind="agy", session="agy-old")


def test_reset_root_of_an_agy_root_tells_the_roots_conversation_by_its_bootstrap(mesh, fake, run, monkeypatch):
    agy_root(mesh, run)
    scripted_root(mesh, fake, monkeypatch, clears=False, kind="agy")
    monkeypatch.setattr(mesh, "agy_conversations", lambda: {"a", "b", "z"} if prompts(fake) else {"a"})
    index_on_bootstrap(mesh, monkeypatch, "b", others=("z",))
    assert mesh.reset_root(run, "repo-lead", None, None) == 0
    assert mesh.load_run(run)["agents"]["repo-lead"]["session"] == "b"  # not "z", which sorts last
    event = mesh.read_log(run)[-1]
    assert (event["old_session"], event["new_session"], "note" in event) == ("agy-old", "b", False)


def test_reset_root_of_an_agy_root_measures_nothing_rather_than_guess(mesh, fake, run, monkeypatch):
    agy_root(mesh, run)
    scripted_root(mesh, fake, monkeypatch, clears=False, kind="agy")
    monkeypatch.setattr(mesh, "agy_conversations", lambda: {"a", "b", "z"} if prompts(fake) else {"a"})
    assert mesh.reset_root(run, "repo-lead", None, None) == 0  # it was reset; only its measuring is lost
    assert mesh.load_run(run)["agents"]["repo-lead"]["session"] is None
    assert "couldn't tell which new agy conversation" in mesh.read_log(run)[-1]["note"]


def test_reset_root_compacting_an_agy_root_keeps_its_conversation(mesh, fake, run, monkeypatch):
    agy_root(mesh, run)
    scripted_root(mesh, fake, monkeypatch, clears=False, kind="agy")
    assert mesh.reset_root(run, "repo-lead", None, "keep the PR list") == 0
    assert mesh.load_run(run)["agents"]["repo-lead"]["session"] == "agy-old"
    event = mesh.read_log(run)[-1]
    assert (event["old_session"], event["new_session"]) == ("agy-old", "agy-old")


def test_a_watch_reset_reminds_the_root_of_a_question_to_the_user(mesh):
    text = mesh.bootstrap_text("mtest", "repo-lead", "carry on supervising", False, "abc123", via="watch")
    assert "waiting on an answer from the user" in text and "notes.md" in text
    assert "user" not in mesh.bootstrap_text("mtest", "repo-lead", "go on", False, "abc123")


def test_brief_caps_the_request_and_tells_a_non_root_whose_view_it_is(mesh, fake, run, capsys):
    fake.on("agent", "get", result={"agent": {"agent_status": "idle"}})
    with mesh.editing_run(run) as data:
        data["request"] = "do it\n" * 1000
    assert mesh.main(["brief", "--json"]) == 0
    data = json.loads(capsys.readouterr().out)
    assert len(data["request"]) < 1300 and "more characters" in data["request"]
    assert mesh.main(["brief", "--as", "repo-fix-typo"]) == 0
    assert "you are repo-fix-typo, not this run's root (repo-lead)" in capsys.readouterr().out


def test_root_session_prefers_the_file_written_last_when_herdr_and_the_command_disagree(mesh, fake, run,
                                                                                        monkeypatch):
    # A nested `claude -c -p` in the root's pane made herdr report its session; the root's own turn is live.
    root_transcript_file(mesh, monkeypatch, 20_000, idle=3000, session="nested-s")
    root_transcript_file(mesh, monkeypatch, 400_000, idle=1, session="root-s")
    herdr_root_session(fake, "nested-s")
    entry = mesh.load_run(run)["agents"]["repo-lead"]
    assert mesh.root_session(entry, as_root=True) == "root-s"
    # A watch that outlived a reset: its own session is the still one, herdr's the live one.
    root_transcript_file(mesh, monkeypatch, 400_000, idle=3000, session="old-s")
    root_transcript_file(mesh, monkeypatch, 30_000, idle=1, session="new-s")
    monkeypatch.setenv("CLAUDE_CODE_SESSION_ID", "old-s")
    herdr_root_session(fake, "new-s")
    assert mesh.root_session(entry, as_root=True) == "new-s"
    assert mesh.root_session(entry) == "new-s"  # not the root's own command: herdr's word


def claude_process(mesh, monkeypatch, pid, session, started="77", status="idle", since=60):
    """The file Claude Code keeps for a running process (`sessions/<pid>.json`); the process started at `77`,
    and its `status` was last set `since` seconds ago."""
    import time
    monkeypatch.setattr(mesh, "proc_start", lambda p: "77")
    path = mesh.Path(mesh.os.environ["CLAUDE_CONFIG_DIR"]) / "sessions" / f"{pid}.json"
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps({"pid": pid, "sessionId": session, "procStart": started, "kind": "interactive",
                                "status": status, "statusUpdatedAt": int((time.time() - since) * 1000)}))


def test_root_session_is_the_session_of_the_process_leading_the_pane(mesh, fake, run, monkeypatch):
    # A nested `claude -p` in the root's pane is writing its transcript right now, and made herdr report it.
    root_transcript_file(mesh, monkeypatch, 400_000, idle=30, session="root-s")
    root_transcript_file(mesh, monkeypatch, 20_000, idle=0, session="nested-s")
    in_pane(fake, "w1:p1", 4000, 4001, session="nested-s")
    claude_process(mesh, monkeypatch, 4000, "root-s")
    claude_process(mesh, monkeypatch, 4100, "nested-s")  # its own process, in a group of its own
    entry = mesh.load_run(run)["agents"]["repo-lead"]
    monkeypatch.setenv("CLAUDE_CODE_SESSION_ID", "root-s")
    assert mesh.root_session(entry, as_root=True) == "root-s"  # the newest file isn't the root's
    assert mesh.root_session(entry) == "root-s"  # nor is herdr's word, for the watch or a reset
    monkeypatch.setenv("CLAUDE_CODE_SESSION_ID", "old-s")  # a watch that outlived a /clear: the process moved on
    assert mesh.root_session(entry, as_root=True) == "root-s"
    # A file an earlier process with the same pid left says nothing: herdr's word again.
    claude_process(mesh, monkeypatch, 4000, "gone-s", started="12")
    assert mesh.root_session(entry) == "nested-s"


def test_a_nested_claude_doesnt_fool_the_watch_into_a_reset(mesh, fake, run, monkeypatch):
    # The root's last reset confirmed root-s; since then a nested `claude -c -p` in its pane made herdr report
    # its big, cold session, while the root's own (small) had a turn after it.
    started = autoreset_root(mesh, fake, monkeypatch)
    monkeypatch.setattr(mesh, "pane_agent", lambda pane: {"agent": "claude", "agent_status": "idle",
                                                          "agent_session": {"value": "nested-s"}})
    root_transcript_file(mesh, monkeypatch, 450_000, idle=5000, session="nested-s")
    root_transcript_file(mesh, monkeypatch, 30_000, idle=4000, session="root-s")
    monkeypatch.delenv("CLAUDE_CODE_SESSION_ID")
    mesh.log_event(run, "reset", name="repo-lead", via="command", old_session="older-s", new_session="root-s")
    assert mesh.root_context(mesh.load_run(run))["session"] == "root-s"
    assert not mesh.maybe_autoreset(run, mesh.load_run(run), mesh.read_log(run)) and not started
    # The user cleared the root by hand since: herdr's session is the one written last, and is measured.
    root_transcript_file(mesh, monkeypatch, 450_000, idle=4000, session="nested-s")
    root_transcript_file(mesh, monkeypatch, 30_000, idle=5000, session="root-s")
    monkeypatch.delenv("CLAUDE_CODE_SESSION_ID")
    assert mesh.root_context(mesh.load_run(run))["session"] == "nested-s"


def test_reset_root_confirms_the_clear_by_the_roots_own_process(mesh, fake, run, monkeypatch):
    # herdr keeps reporting a nested Claude's session; the root's process moves from root-s to fresh-s.
    scripted_root(mesh, fake, monkeypatch, clears=False)
    in_pane(fake, "w1:p1", 4000)
    claude_process(mesh, monkeypatch, 4000, "root-s")
    real = fake.__call__

    def herdr(*args, timeout=None):
        result = real(*args, timeout=timeout)
        if args[:4] == ("agent", "prompt", "w1:p1", "/clear"):
            claude_process(mesh, monkeypatch, 4000, "fresh-s")
        return result

    monkeypatch.setattr(mesh, "herdr", herdr)
    assert mesh.reset_root(run, "repo-lead", None, None) == 0
    event = mesh.read_log(run)[-1]
    assert (event["event"], event["old_session"], event["new_session"]) == ("reset", "root-s", "fresh-s")


def test_reset_root_takes_herdrs_clear_when_the_process_file_is_slow_to_follow(mesh, fake, run, monkeypatch):
    # The root's process file keeps root-s; herdr reports the clear (scripted_root: old → new).
    scripted_root(mesh, fake, monkeypatch)
    in_pane(fake, "w1:p1", 4000)
    claude_process(mesh, monkeypatch, 4000, "root-s")
    slept = []
    monkeypatch.setattr(mesh.time, "sleep", slept.append)
    assert mesh.reset_root(run, "repo-lead", None, None) == 0
    event = mesh.read_log(run)[-1]
    assert (event["event"], event["old_session"], event["new_session"]) == ("reset", "root-s", "new")
    # The file had its chance first: herdr's word counts only CLEAR_FILE_GRACE seconds after it showed.
    assert sum(1 for s in slept if s == 1) >= mesh.CLEAR_FILE_GRACE
    assert prompts(fake)[1].startswith("📬 [mesh] (ref ")


def lost_root(mesh, fake, monkeypatch, status="idle", since=60, leader=True):
    """A root herdr reads `unknown`; its Claude (pid 4000, session root-s, its own `status`) leads the pane
    when `leader`, and moves to fresh-s once /clear is typed."""
    reads = scripted_root(mesh, fake, monkeypatch, status="unknown", clears=False)
    if leader:
        in_pane(fake, "w1:p1", 4000)
        claude_process(mesh, monkeypatch, 4000, "root-s", status=status, since=since)
        real = fake.__call__

        def herdr(*args, timeout=None):
            result = real(*args, timeout=timeout)
            if args[:4] == ("agent", "prompt", "w1:p1", "/clear"):
                claude_process(mesh, monkeypatch, 4000, "fresh-s")
            return result

        monkeypatch.setattr(mesh, "herdr", herdr)
    return reads


def clock(mesh, monkeypatch):
    """A monotonic clock that `time.sleep` moves on, so waits in seconds pass at once."""
    now = [1000.0]
    monkeypatch.setattr(mesh.time, "monotonic", lambda: now[0])
    monkeypatch.setattr(mesh.time, "sleep", lambda s: now.__setitem__(0, now[0] + s))
    return now


@pytest.mark.parametrize("status", ["idle", "shell"])  # shell: idle, with a background task (its watch)
def test_reset_root_resets_a_root_herdr_lost_once_its_process_reads_the_turn_over(mesh, fake, run, monkeypatch,
                                                                                   status):
    lost_root(mesh, fake, monkeypatch, status=status)
    assert mesh.reset_root(run, "repo-lead", "review PR 31", None) == 0
    assert prompts(fake)[0] == "/clear" and prompts(fake)[1].endswith("Then: review PR 31")
    event = mesh.read_log(run)[-1]
    assert (event["event"], event["old_session"], event["new_session"]) == ("reset", "root-s", "fresh-s")


@pytest.mark.parametrize("status, since", [("busy", 60), ("waiting", 60), ("idle", 1)])
def test_reset_root_never_clears_a_lost_root_mid_turn(mesh, fake, run, monkeypatch, status, since):
    # busy: a turn (a foreground tool call too); waiting: a dialog the /clear would answer; idle a moment ago:
    # the next step may be on its way.
    monkeypatch.setenv("MESH_RESET_WAIT", "0")
    lost_root(mesh, fake, monkeypatch, status=status, since=since)
    assert mesh.reset_root(run, "repo-lead", None, None) == 1
    assert prompts(fake) == []
    assert f"its process reads {status}" in mesh.read_log(run)[-1]["reason"]


@pytest.mark.parametrize("case", ["no leader", "compact"])
def test_reset_root_gives_up_on_a_lost_root_it_cant_judge_once_herdr_stays_lost(mesh, fake, run, monkeypatch,
                                                                                 case):
    lost_root(mesh, fake, monkeypatch, leader=case != "no leader")
    now = clock(mesh, monkeypatch)
    start = now[0]
    assert mesh.reset_root(run, "repo-lead", None, "keep it" if case == "compact" else None) == 1
    assert prompts(fake) == []
    assert mesh.LOST_AFTER <= now[0] - start < mesh.LOST_AFTER + 2 * mesh.RESET_POLL  # not MESH_RESET_WAIT
    event = mesh.read_log(run)[-1]
    assert event["event"] == "reset_failed" and "herdr has lost track of this root's pane" in event["reason"]
    assert "/clear by hand" in event["reason"]


def test_reset_root_waits_out_a_passing_unknown(mesh, fake, run, monkeypatch):
    # herdr reads the root unknown for a few polls (re-detecting it), then idle again.
    reads = scripted_root(mesh, fake, monkeypatch)
    real = mesh.pane_agent

    def agent(pane):
        result = real(pane)
        return {**result, "agent_status": "unknown"} if reads["n"] <= 5 else result

    monkeypatch.setattr(mesh, "pane_agent", agent)
    clock(mesh, monkeypatch)
    assert mesh.reset_root(run, "repo-lead", None, None) == 0
    assert prompts(fake)[0] == "/clear"


def test_cleared_session_keeps_herdrs_first_report_while_it_flickers(mesh, fake, run, monkeypatch):
    # The process file stays on root-s; herdr reports the new session once, then loses it.
    in_pane(fake, "w1:p1", 4000)
    claude_process(mesh, monkeypatch, 4000, "root-s")
    answers = iter([{"agent_session": {"value": "nested-s"}}, {"agent_session": {"value": "new-s"}}])
    monkeypatch.setattr(mesh, "pane_agent", lambda pane: next(answers, {}))
    entry = mesh.load_run(run)["agents"]["repo-lead"]
    assert mesh.cleared_session(entry, "root-s", set(), "nested-s") == {"new-s"}
    # Never anything new: it gives up after CLEAR_CONFIRM, with herdr flickering or not.
    monkeypatch.setattr(mesh, "pane_agent", lambda pane: {})
    assert mesh.cleared_session(entry, "root-s", set(), "nested-s") == set()


def test_cleared_session_never_takes_the_old_session_for_a_new_one(mesh, fake, run, monkeypatch):
    entry = mesh.load_run(run)["agents"]["repo-lead"]
    # Before the clear, the process file couldn't be read: herdr reported a nested Claude's session. After it,
    # the file reads the root's unchanged session: that is no clear.
    in_pane(fake, "w1:p1", 4000)
    claude_process(mesh, monkeypatch, 4000, "root-s")
    monkeypatch.setattr(mesh, "pane_agent", lambda pane: {"agent_session": {"value": "nested-s"}})
    assert mesh.cleared_session(entry, None, set(), "nested-s") == set()
    # herdr's report moving back to the root's old session isn't a clear either.
    monkeypatch.setattr(mesh, "pane_agent", lambda pane: {"agent_session": {"value": "root-s"}})
    assert mesh.cleared_session(entry, "root-s", set(), "nested-s") == set()


def test_root_context_reads_the_log_only_when_the_roots_process_cant_say(mesh, fake, run, monkeypatch):
    root_transcript_file(mesh, monkeypatch, 40_000, session="root-s")
    in_pane(fake, "w1:p1", 4000)
    claude_process(mesh, monkeypatch, 4000, "root-s")
    monkeypatch.setattr(mesh, "reset_session", lambda *a: pytest.fail("read the log"))
    assert mesh.root_context(mesh.load_run(run))["session"] == "root-s"


@pytest.mark.parametrize("process, reset", [("idle", True), ("shell", True), ("busy", False), (None, False)])
def test_autoreset_judges_a_root_herdr_lost_by_its_own_process(mesh, fake, run, monkeypatch, process, reset):
    started = autoreset_root(mesh, fake, monkeypatch, status="unknown")
    if process:  # its Claude leads the pane and says how its turn stands; None: nothing can tell
        in_pane(fake, "w1:p1", 4000)
        claude_process(mesh, monkeypatch, 4000, "root-s", status=process)
    assert mesh.maybe_autoreset(run, mesh.load_run(run), mesh.read_log(run)) is reset
    assert bool(started) is reset
