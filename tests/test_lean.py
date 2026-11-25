"""Lean Claude sessions, tools on demand, the root's relaunch at a reset, the root keeper and the compactions at
approve and before a follow-up (design §11, §12, §15)."""

import json
import os
import shutil
import stat
import subprocess
import threading
import time
from datetime import datetime, timedelta, timezone
from pathlib import Path

import pytest

from conftest import with_config
from test_mesh import (IDLE_BOX, archived_worker, bg_cli, claude_planner, claude_root, claude_worker,  # noqa: F401
                       deny_rules, prompts, scripted_root, sent_plan, started, stub_new_pane, stub_panes, usage)


@pytest.fixture
def lean(mesh, monkeypatch, tmp_path):
    """Lean launches on, with a user config to cut from: MCP servers at user scope and for the repo, an approved
    and an unapproved `.mcp.json` server, and enabled plugins."""
    monkeypatch.setenv("MESH_CLAUDE_LEAN", "1")
    repo = tmp_path / "repo"
    repo.mkdir()
    (repo / ".mcp.json").write_text(json.dumps({"mcpServers": {"approved": {"command": "a"},
                                                               "unapproved": {"command": "u"}}}))
    home = Path(os.environ["CLAUDE_CONFIG_DIR"])
    home.mkdir(parents=True, exist_ok=True)
    (home / ".claude.json").write_text(json.dumps({
        "mcpServers": {"todoist": {"command": "t", "env": {"TOKEN": "secret"}}, "flow": {"command": "f"}},
        "projects": {str(repo): {"mcpServers": {"sentry": {"command": "s"}},
                                 "enabledMcpjsonServers": ["approved"]}}}))
    (home / "settings.json").write_text(json.dumps({"enabledPlugins": {
        "pr-review-toolkit@official": True, "frontend-design@official": True, "feature-dev@official": False}}))
    return repo


def entry_for(repo, **extra):
    return {"role": "worker", "kind": "claude", "repo": str(repo), "cwd": str(repo), **extra}


def mcp_servers(path):
    return set(json.loads(Path(path).read_text())["mcpServers"])


# ── lean argv and settings ───────────────────────────────────────────────


def test_lean_off_keeps_the_deny_list_alone(mesh, run, tmp_path):
    entry = entry_for(tmp_path, planning=True)
    assert mesh.claude_launch_argv(run, "repo-fix-typo", entry) == mesh.claude_deny(entry)
    assert mesh.claude_launch_argv(run, "repo-fix-typo", entry_for(tmp_path)) == []
    settings = json.loads(mesh.worker_settings(mesh.mesh_env(run, "repo-fix-typo", "repo-lead", "worker"), False))
    assert set(settings) <= {"hooks", "crossSessionInbound", "promptCacheTtl"}


def test_a_lean_launch_merges_one_deny_list_and_loads_only_the_repos_mcp_servers(mesh, run, lean):
    entry = entry_for(lean, planning=True)
    argv = mesh.claude_launch_argv(run, "repo-fix-typo", entry)
    assert argv.count("--disallowedTools") == 1
    rules = deny_rules(argv)
    assert any(r.startswith("Edit(") for r in rules) and {"Artifact", "Workflow", "EnterWorktree"} <= set(rules)
    path = argv[argv.index("--mcp-config") + 1]
    assert "--strict-mcp-config" in argv and "--system-prompt-snapshot" not in argv
    assert mcp_servers(path) == {"sentry", "approved"}  # not the user's todoist/flow, nor the unapproved one
    assert stat.S_IMODE(os.stat(path).st_mode) == 0o600  # a server's config may carry secrets
    with_config({"claude": {"mcp_keep": ["flow"]}})
    mesh.claude_launch_argv(run, "repo-fix-typo", entry)
    assert mcp_servers(path) == {"sentry", "approved", "flow"}
    with_config({"claude": {"mcp": "none", "mcp_keep": ["flow"]}})
    mesh.claude_launch_argv(run, "repo-fix-typo", entry)
    assert mcp_servers(path) == {"flow"}
    with_config({"claude": {"mcp": "inherit"}})
    assert "--strict-mcp-config" not in mesh.claude_launch_argv(run, "repo-fix-typo", entry)


def test_lean_settings_ride_in_the_one_settings_json_beside_the_hooks(mesh, run, lean):
    claude_worker(mesh, run, repo=str(lean), cwd=str(lean))
    env = mesh.mesh_env(run, "repo-fix-typo", "repo-lead", "worker")
    for bg in (False, True):
        settings = json.loads(mesh.worker_settings(env, bg))
        assert settings["skillOverrides"]["dataviz"] == "user-invocable-only"
        assert settings["skillOverrides"]["delegate-claude"] == "user-invocable-only"  # a worker's
        assert settings["syncClaudeAiSkills"] is False
        assert settings["enabledPlugins"] == {"pr-review-toolkit@official": False}  # enabled and listed only
        assert settings["env"]["ENABLE_CLAUDEAI_MCP_SERVERS"] == "false"
        assert bg == ("MESH_RUN" in settings["env"])
        argv = ["claude", "--settings", json.dumps(settings)]
        assert mesh.mesh_launched(argv, run, "repo-fix-typo")  # still mesh's own launch
    root = mesh.lean_settings({"role": "root", "kind": "claude"})
    assert "delegate-claude" not in root["skillOverrides"]  # the root delegates


def test_what_an_agent_added_back_stays_in_and_a_tool_turns_the_snapshot_off(mesh, run, lean):
    entry = entry_for(lean, added=["Artifact", "dataviz", "pr-review-toolkit"], mcp=["todoist"], tools_added=True)
    argv = mesh.claude_launch_argv(run, "repo-fix-typo", entry)
    assert "Artifact" not in deny_rules(argv) and argv[-2:] == ["--system-prompt-snapshot", "off"]
    assert "todoist" in mcp_servers(argv[argv.index("--mcp-config") + 1])
    settings = mesh.lean_settings(entry)
    assert "dataviz" not in settings["skillOverrides"] and "enabledPlugins" not in settings
    user_review = entry_for(lean, plan_review="user")
    assert "AskUserQuestion" not in deny_rules(mesh.claude_launch_argv(run, "repo-fix-typo", user_review))


def test_the_compaction_turn_runs_lean_too(mesh, run, lean, cli):
    argv = mesh.CLIS["claude"].compact_argv("s-1", "opus", "/compact x", entry_for(lean))
    assert "Workflow" in deny_rules(argv)
    settings = json.loads(argv[argv.index("--settings") + 1])
    assert settings["promptCacheTtl"] == "5m" and settings["syncClaudeAiSkills"] is False
    assert "--mcp-config" not in argv and argv[-1] == "/compact x"
    # a stopped worker's compaction: its own MCP servers, so the turn reads the prefix it cached
    mesh.compact_stopped(run, "repo-fix-typo", entry_for(lean, model="opus"), "s-1", "keep X", "approve")
    argv = cli.calls[-1]["argv"]
    assert "--strict-mcp-config" in argv and mcp_servers(argv[argv.index("--mcp-config") + 1]) == {"sentry", "approved"}
    assert json.loads(argv[argv.index("--settings") + 1])["env"] == {"ENABLE_CLAUDEAI_MCP_SERVERS": "false"}


def test_approve_reopens_a_planner_lean_without_its_edit_rules(mesh, fake, run, lean, capsys):
    claude_planner(mesh, run)
    with mesh.editing_run(run) as data:
        data["agents"]["repo-fix-typo"].update(repo=str(lean), cwd=str(lean))
    stub_new_pane(fake)
    sent_plan(mesh, run)
    assert mesh.main(["approve", "repo-fix-typo"]) == 0
    argv = fake.called("agent", "start")[-1]
    rules = deny_rules(argv)
    assert "Artifact" in rules and not any(r.startswith("Edit(") for r in rules)
    assert "--strict-mcp-config" in argv and argv.count("--settings") == 1
    assert "promptCacheTtl" not in json.loads(argv[argv.index("--settings") + 1])  # executing: 1 h


def test_a_resumed_worker_keeps_what_it_added(mesh):
    assert set(mesh.ADDED_KEYS) == {"added", "tools_added", "mcp_all", "mcp"}


def test_retire_and_stop_remove_the_private_files(mesh, fake, run, lean):
    mesh.claude_launch_argv(run, "repo-fix-typo", entry_for(lean))
    path = mesh.mcp_file(run, "repo-fix-typo")
    root = mesh.root_files("w1:p1") / "settings.json"
    mesh.write_private(root, "{}")
    assert path.exists()
    mesh.retire_worker(run, "repo-fix-typo", mesh.load_run(run)["agents"]["repo-fix-typo"])
    assert not path.exists()
    mesh.claude_launch_argv(run, "repo-two", entry_for(lean))
    mesh.remove_private(run)
    assert not list((mesh.run_dir(run) / "mcp").glob("*.json"))
    assert root.exists()  # the root's Claude runs on it past the run, and the next run's reset reads it


# ── tools on demand ──────────────────────────────────────────────────────


def test_tools_lists_what_a_session_runs_without(mesh, run, lean, capsys):
    claude_worker(mesh, run, repo=str(lean), cwd=str(lean))
    assert mesh.main(["tools", "--as", "repo-fix-typo"]) == 0
    out = json.loads(capsys.readouterr().out)
    assert "Artifact" in out["tools"] and out["mcp_loaded"] == ["approved", "sentry"]
    assert set(out["mcp_cut"]) == {"todoist", "flow"} and out["plugins"] == ["pr-review-toolkit"]


def test_a_worker_adds_tools_back_and_mesh_reopens_it_after_its_turn(mesh, run, lean, monkeypatch, capsys):
    claude_worker(mesh, run, repo=str(lean), cwd=str(lean))
    reopens = []
    monkeypatch.setattr(mesh, "start_tools_reopen", lambda *a: reopens.append(a) or 77)
    assert mesh.main(["tools", "add", "Artifact", "flow", "--next", "publish the page", "--as", "repo-fix-typo"]) == 0
    entry = mesh.load_run(run)["agents"]["repo-fix-typo"]
    assert entry["added"] == ["Artifact"] and entry["mcp"] == ["flow"] and entry["tools_added"] is True  # a server is `mcp`
    assert reopens == [(run, "repo-fix-typo", "publish the page")]
    assert mesh.live_wait(mesh.read_log(run), "repo-fix-typo")  # its Stop hook lets this turn end
    assert "end your turn now" in json.loads(capsys.readouterr().out)["then"]


def test_a_tools_add_that_cant_start_its_reopen_records_nothing(mesh, run, lean, monkeypatch, capsys):
    claude_worker(mesh, run, repo=str(lean), cwd=str(lean))

    def busy(*a):
        raise mesh.MeshError("a reopen of repo-fix-typo with added tools is already pending")
    monkeypatch.setattr(mesh, "start_tools_reopen", busy)
    assert mesh.main(["tools", "add", "Artifact", "--as", "repo-fix-typo"]) == 1
    entry = mesh.load_run(run)["agents"]["repo-fix-typo"]
    assert "added" not in entry and "tools_added" not in entry
    assert not mesh.live_wait(mesh.read_log(run), "repo-fix-typo")


def test_tools_add_refuses_what_isnt_cut(mesh, run, lean, capsys):
    claude_worker(mesh, run, repo=str(lean), cwd=str(lean))
    assert mesh.main(["tools", "add", "Bash", "--as", "repo-fix-typo"]) == 1
    err = capsys.readouterr().err
    assert "Bash isn't something this session runs without" in err and "todoist" in err
    assert "added" not in mesh.load_run(run)["agents"]["repo-fix-typo"]


def test_a_worker_runs_mesh_tools_only_for_itself(mesh, run, lean, monkeypatch, capsys):
    claude_worker(mesh, run, repo=str(lean), cwd=str(lean))
    reopens = []
    monkeypatch.setattr(mesh, "start_tools_reopen", lambda *a: reopens.append(a) or 77)
    monkeypatch.setattr(mesh, "start_reset", lambda *a, **k: reopens.append(a) or 78)
    monkeypatch.setenv("MESH_RUN", run)
    monkeypatch.setenv("MESH_SELF", "repo-fix-typo")
    for argv in (["tools", "add", "Artifact", "--as", "repo-lead"],  # the root's entry
                 ["tools", "add", "Artifact", "--run", "other", "--as", "repo-fix-typo"],  # another run's
                 ["tools", "--apply", "--as", "repo-fix-typo"],  # the reopen itself, which the detached child runs
                 ["tools", "--apply"]):
        assert mesh.main(argv) == 1
        assert "only" in capsys.readouterr().err
    assert not reopens and "added" not in mesh.load_run(run)["agents"]["repo-lead"]
    assert [e for e in mesh.read_log(run) if e["event"] == "refused" and e["command"] == "tools"]
    # its own session is still its own: by flag and by environment
    assert mesh.main(["tools", "add", "Artifact", "--as", "repo-fix-typo"]) == 0
    assert mesh.main(["tools", "add", "flow"]) == 0
    assert len(reopens) == 2
    # the detached child runs without MESH_*, so --apply works for it
    for var in ("MESH_RUN", "MESH_SELF"):
        monkeypatch.delenv(var)
    monkeypatch.setattr(mesh, "tools_reopen", lambda *a: 0)
    assert mesh.main(["tools", "--apply", "--run", run, "--as", "repo-fix-typo"]) == 0


def test_tools_lists_nothing_cut_when_lean_is_off(mesh, run, lean, monkeypatch, capsys):
    claude_worker(mesh, run, repo=str(lean), cwd=str(lean))
    monkeypatch.setenv("MESH_CLAUDE_LEAN", "0")
    assert mesh.main(["tools", "--as", "repo-fix-typo"]) == 0
    out = json.loads(capsys.readouterr().out)
    assert out["lean"] is False and not (out["tools"] or out["skills"] or out["plugins"] or out["mcp_cut"])
    assert out["claudeai_skills"] is None and out["claudeai_connectors"] is None


def test_the_root_loses_the_agent_tool_until_it_adds_it_back(mesh, run, lean):
    assert {"Agent", "ReportFindings"} <= set(mesh.setting("claude.root_deny_tools"))
    root = {"role": "root", "kind": "claude", "repo": str(lean), "cwd": str(lean)}
    assert {"Agent", "ReportFindings"} <= set(mesh.lean_tools(root))
    assert "Agent" not in mesh.lean_tools({**root, "added": ["Agent"]})


def test_claude_ai_connectors_bring_the_users_whole_mcp_config(mesh, run, lean, monkeypatch):
    claude_worker(mesh, run, repo=str(lean), cwd=str(lean))
    monkeypatch.setattr(mesh, "start_tools_reopen", lambda *a: 77)
    assert mesh.main(["tools", "add", "claude.ai Gmail", "--as", "repo-fix-typo"]) == 0
    entry = mesh.load_run(run)["agents"]["repo-fix-typo"]
    assert entry["mcp_all"] and "--strict-mcp-config" not in mesh.claude_launch_argv(run, "repo-fix-typo", entry)


def test_the_root_adds_tools_back_through_a_reopening_reset(mesh, run, lean, monkeypatch):
    monkeypatch.setenv("MESH_ROOT_RELAUNCH", "1")
    with mesh.editing_run(run) as data:
        data["agents"]["repo-lead"].update(kind="claude", cwd=str(lean))
    resets = []
    monkeypatch.setattr(mesh, "start_reset", lambda *a, **k: resets.append((a, k)) or 5)
    assert mesh.main(["tools", "add", "Artifact", "--next", "publish it"]) == 0
    (args, kwargs), = resets
    assert args[:3] == (run, "repo-lead", "publish it") and kwargs == {"via": "tools", "reopen": True}
    monkeypatch.setenv("MESH_ROOT_RELAUNCH", "0")
    assert mesh.main(["tools", "add", "Workflow"]) == 1  # nothing could restart it


def test_tools_reopen_swaps_the_same_conversation_and_rings_it_on(mesh, run, monkeypatch):
    claude_worker(mesh, run, model="opus", effort="high", added=["flow"])
    swaps = []
    monkeypatch.setattr(mesh, "swap_model", lambda *a, **k: swaps.append((a, k)) or {
        "delivery": {"stored": "x", "rung": True, "error": None}})
    assert mesh.tools_reopen(run, "repo-fix-typo", "go on with the deploy") == 0
    (args, kwargs), = swaps
    assert args[1:5] == ("repo-fix-typo", "opus", "high", "tools added")
    sender, mtype, body = kwargs["message"]
    assert (sender, mtype) == ("repo-lead", "info") and "with flow" in body and body.endswith("deploy.")
    assert mesh.read_log(run)[-1]["event"] == "tools_reopened"


# ── the root's relaunch at a reset ───────────────────────────────────────


def test_parse_claude_argv_drops_what_picks_a_conversation_and_a_prompt(mesh):
    parsed = mesh.parse_claude_argv([
        "node", "/usr/lib/claude", "--resume", "abc", "--model", "opus", "--settings", "s.json",
        "--disallowedTools", "A,B", "C", "--add-dir", "x", "y", "-c", "--strict-mcp-config",
        "--effort=high", "fix the bug"])
    assert parsed["plain"] == ["--model", "opus", "--settings", "s.json", "--disallowedTools", "A,B", "C",
                               "--add-dir", "x", "y", "--strict-mcp-config", "--effort=high"]
    assert parsed["kept"] == ["--model", "opus", "--add-dir", "x", "y", "--effort=high"]
    assert parsed["settings"] == ["s.json"] and parsed["deny"] == ["A", "B", "C"]
    assert mesh.parse_claude_argv(["claude", "-r", "--verbose"])["plain"] == ["--verbose"]


def test_parse_claude_argv_takes_the_users_mcp_config_out_of_the_kept_flags(mesh):
    parsed = mesh.parse_claude_argv(["claude", "--mcp-config", "a.json", "b.json", "--model", "opus",
                                     "--mcp-config=c.json"])
    assert parsed["mcp"] == ["a.json", "b.json", "c.json"] and parsed["kept"] == ["--model", "opus"]
    assert parsed["plain"][:3] == ["--mcp-config", "a.json", "b.json"]  # the fallback keeps the old line whole


def lean_root(mesh, run, monkeypatch, tmp_path, argv=None, env=None):
    monkeypatch.setenv("MESH_ROOT_RELAUNCH", "1")
    with mesh.editing_run(run) as data:
        data["agents"]["repo-lead"].update(kind="claude", cwd=str(tmp_path), session="old")
    (tmp_path / "user.json").write_text(json.dumps({"model": "opus", "env": {"A": "1"},
                                                     "enabledPlugins": {"x@y": True}}))
    claude = {"pid": 100, "start": "9", "ppid": 50, "cwd": str(tmp_path),
              "argv": argv or ["claude", "--resume", "old", "--settings", "user.json", "--disallowedTools", "Bash(rm:*)"],
              "env": env or {"PATH": "/bin", "ALIAS_SET": "yes", "CLAUDE_CODE_ENTRYPOINT": "cli"},
              "shell_env": {"PATH": "/bin"}}
    monkeypatch.setattr(mesh, "root_claude", lambda pane: (claude, ""))
    monkeypatch.setattr(mesh, "leader_session", lambda pane: "old")
    return claude


def test_relaunch_plan_merges_the_users_flags_into_one_settings_file(mesh, run, lean, monkeypatch, tmp_path):
    lean_root(mesh, run, monkeypatch, tmp_path)
    plan, why = mesh.relaunch_plan(run, "repo-lead", mesh.load_run(run)["agents"]["repo-lead"], None)
    argv = plan["argv"]
    assert "--resume" not in argv and argv.count("--settings") == 1 and argv.count("--disallowedTools") == 1
    assert deny_rules(argv)[0] == "Bash(rm:*)" and "Artifact" in deny_rules(argv) and "AskUserQuestion" not in argv
    path = mesh.root_files("w1:p1") / "settings.json"
    settings = json.loads(path.read_text())
    assert settings["model"] == "opus" and settings["env"] == {"ALIAS_SET": "yes", "A": "1",
                                                               "ENABLE_CLAUDEAI_MCP_SERVERS": "false"}
    assert settings["enabledPlugins"] == {"x@y": True, "pr-review-toolkit@official": False}
    assert stat.S_IMODE(path.stat().st_mode) == 0o600
    assert plan["fallback"] == ["--settings", "user.json", "--disallowedTools", "Bash(rm:*)", "--resume", "old"]
    reopen, _ = mesh.relaunch_plan(run, "repo-lead", mesh.load_run(run)["agents"]["repo-lead"], "old")
    assert reopen["argv"][-2:] == ["--resume", "old"]


def test_a_root_relaunched_before_gets_back_what_it_added(mesh, run, lean, monkeypatch, tmp_path):
    claude = lean_root(mesh, run, monkeypatch, tmp_path)
    plan, _ = mesh.relaunch_plan(run, "repo-lead", mesh.load_run(run)["agents"]["repo-lead"], None)
    claude.update(argv=["claude", *plan["argv"]], env={"PATH": "/bin"})  # now runs mesh's flags, not the alias
    with mesh.editing_run(run) as data:
        data["agents"]["repo-lead"].update(added=["Artifact", "dataviz"], tools_added=True)
    plan, _ = mesh.relaunch_plan(run, "repo-lead", mesh.load_run(run)["agents"]["repo-lead"], "new1")
    rules = deny_rules(plan["argv"])
    assert "Artifact" not in rules and "Bash(rm:*)" in rules and "Workflow" in rules
    settings = json.loads((mesh.root_files("w1:p1") / "settings.json").read_text())
    assert "dataviz" not in settings["skillOverrides"] and settings["model"] == "opus"
    assert settings["env"]["ALIAS_SET"] == "yes"  # what the alias set, kept from the first relaunch
    # a later run (its own run dir) still knows it, and without the origin record the merged file stands in
    (mesh.root_files("w1:p1") / "origin.json").unlink()
    plan, why = mesh.relaunch_plan("mother", "repo-lead", mesh.load_run(run)["agents"]["repo-lead"], "new2")
    assert plan and "Bash(rm:*)" in deny_rules(plan["argv"]) and "Artifact" not in deny_rules(plan["argv"])
    assert json.loads((mesh.root_files("w1:p1") / "settings.json").read_text())["env"]["ALIAS_SET"] == "yes"


@pytest.mark.parametrize("mode, user_kept", [("repo", False), ("inherit", True)])
def test_each_lean_flag_appears_once_however_many_reopens_follow(mesh, run, lean, monkeypatch, tmp_path, mode,
                                                                 user_kept):
    with_config({"claude": {"mcp": mode}})
    claude = lean_root(mesh, run, monkeypatch, tmp_path, argv=[
        "claude", "--resume", "old", "--settings", "user.json", "--mcp-config", "u.json",
        "--disallowedTools", "Bash(rm:*)"])
    for n in range(1, 4):
        if n > 1:
            with mesh.editing_run(run) as data:
                data["agents"]["repo-lead"].update(added=["Artifact"], tools_added=True)
        plan, why = mesh.relaunch_plan(run, "repo-lead", mesh.load_run(run)["agents"]["repo-lead"], f"s{n}")
        argv = plan["argv"]
        assert plan, why
        for flag in ("--settings", "--disallowedTools", "--resume", "--system-prompt-snapshot"):
            assert argv.count(flag) <= 1, (n, flag)
        assert argv.count("--mcp-config") == 1 and argv.count("--strict-mcp-config") == (0 if user_kept else 1)
        mcp_path = argv[argv.index("--mcp-config") + 1]
        assert (mcp_path == "u.json") is user_kept and (user_kept or "u.json" not in argv)
        claude["argv"] = ["claude", *argv]  # the next reopen sees the process this one started
    assert argv[-4:] == ["--resume", "s3", "--system-prompt-snapshot", "off"]


def test_a_root_already_running_lean_is_cleared_not_relaunched(mesh, run, lean, monkeypatch, tmp_path):
    claude = lean_root(mesh, run, monkeypatch, tmp_path)
    plan, _ = mesh.relaunch_plan(run, "repo-lead", mesh.load_run(run)["agents"]["repo-lead"], None)
    claude["argv"] = ["claude", *plan["argv"]]
    assert mesh.relaunch_plan(run, "repo-lead", mesh.load_run(run)["agents"]["repo-lead"], None) == (None,
                                                                                                    "already lean")


def relaunching(mesh, fake, monkeypatch, starts_ok=(True,), exits=True):
    """The pane's Claude ends at /exit (when `exits`); each `herdr agent start` takes (per `starts_ok`) and a new
    Claude then leads the pane."""
    state = {"alive": True}
    monkeypatch.setattr(mesh, "pid_alive", lambda pid, start=None: state["alive"] and not (
        exits and any(c[:4] == ("agent", "prompt", "w1:p1", "/exit") for c in fake.calls)))
    results = [None if ok else "cli_error" for ok in starts_ok]

    def start(*args, timeout=None):
        fake.calls.append(args)
        code = results.pop(0) if len(results) > 1 else results[0]
        if code:
            raise mesh.HerdrError(code, "stubbed")
        state["started"] = state.get("started", 0) + 1
        return {}

    real = fake.__call__

    def herdr(*args, timeout=None):
        if args[:2] == ("agent", "start"):
            return start(*args, timeout=timeout)
        return real(*args, timeout=timeout)

    monkeypatch.setattr(mesh, "herdr", herdr)
    monkeypatch.setattr(mesh, "leader_record", lambda pane: {
        "pid": 200 + state["started"], "procStart": "7", "sessionId": f"new{state['started']}",
        "messagingSocketPath": "/s/200.sock"} if state.get("started") else None)
    monkeypatch.setattr(mesh, "pane_process_info", lambda pane: {"foreground_process_group_id": 50,
                                                                 "shell_pid": 50})
    return state


def test_reset_relaunches_the_root_lean_then_bootstraps_it(mesh, fake, run, lean, monkeypatch, tmp_path):
    lean_root(mesh, run, monkeypatch, tmp_path)
    scripted_root(mesh, fake, monkeypatch)
    relaunching(mesh, fake, monkeypatch)
    assert mesh.reset_root(run, "repo-lead", "review PR 31", None) == 0
    exit_, bootstrap = prompts(fake)
    assert exit_ == "/exit" and "/clear" not in prompts(fake) and bootstrap.endswith("Then: review PR 31")
    start = fake.called("agent", "start")[-1]
    assert start[:7] == ("agent", "start", "repo-lead", "--kind", "claude", "--pane", "w1:p1")
    root = mesh.load_run(run)["agents"]["repo-lead"]
    assert (root["pid"], root["session"], root["address"]) == (201, "new1", "uds:/s/200.sock")
    event = mesh.read_log(run)[-1]
    assert (event["event"], event["relaunched"], event["new_session"]) == ("reset", True, "new1")


def test_a_failed_lean_relaunch_brings_the_old_root_back(mesh, fake, run, lean, monkeypatch, tmp_path):
    lean_root(mesh, run, monkeypatch, tmp_path)
    scripted_root(mesh, fake, monkeypatch)
    relaunching(mesh, fake, monkeypatch, starts_ok=(False, True))
    assert mesh.reset_root(run, "repo-lead", None, None) == 1
    first, second = fake.called("agent", "start")
    assert "--resume" not in first and second[-2:] == ("--resume", "old")
    assert "old command line was started again" in mesh.read_log(run)[-1]["reason"]
    assert mesh.load_run(run)["agents"]["repo-lead"]["session"] == "new1"  # the old conversation, back


def test_a_claude_that_leads_the_pane_late_is_rebound_and_bootstrapped(mesh, fake, run, lean, monkeypatch, tmp_path):
    lean_root(mesh, run, monkeypatch, tmp_path)
    scripted_root(mesh, fake, monkeypatch)
    relaunching(mesh, fake, monkeypatch)
    started_ = {"n": 0}
    real = mesh.herdr

    def herdr(*args, **kw):
        if args[:2] == ("agent", "start"):
            started_["n"] += 1
        return real(*args, **kw)
    monkeypatch.setattr(mesh, "herdr", herdr)
    monkeypatch.setattr(mesh, "RELAUNCH_START", 0)  # the first look finds no new record in time
    monkeypatch.setattr(mesh, "pane_process_info", lambda pane: {"foreground_process_group_id": 201,
                                                                 "shell_pid": 50})
    late = {"pid": 201, "procStart": "7", "messagingSocketPath": "/s/201.sock"}  # up, but no session recorded yet
    monkeypatch.setattr(mesh, "leader_record", lambda pane: late)
    assert mesh.reset_root(run, "repo-lead", "carry on", None) == 0
    assert started_["n"] == 1  # no second start of the old command line
    root = mesh.load_run(run)["agents"]["repo-lead"]
    assert (root["pid"], root["address"]) == (201, "uds:/s/201.sock")
    assert prompts(fake)[-1].endswith("Then: carry on")
    assert mesh.read_log(run)[-1]["relaunched"] is True


def test_a_root_that_doesnt_exit_gets_nothing_more_typed(mesh, fake, run, lean, monkeypatch, tmp_path):
    lean_root(mesh, run, monkeypatch, tmp_path)
    scripted_root(mesh, fake, monkeypatch)
    relaunching(mesh, fake, monkeypatch, exits=False)
    monkeypatch.setattr(mesh, "RELAUNCH_EXIT", 0)  # no waiting: its idle screen is judged at once
    assert mesh.reset_root(run, "repo-lead", None, None) == 1
    assert prompts(fake) == ["/exit"] and not fake.called("agent", "start")
    assert "still runs" in mesh.read_log(run)[-1]["reason"]


def slow_exit(mesh, fake, monkeypatch, waits, screen_gone=True):
    """A root whose /exit takes longer: `wait_gone` answers from `waits` in turn (the 60 s look, the late wait,
    each signal's wait), and after the /exit its screen reads gone (no input box; herdr may still say working)."""
    sent = []
    replies = list(waits)
    monkeypatch.setattr(mesh, "wait_gone", lambda plan, seconds: replies.pop(0))
    monkeypatch.setattr(mesh.os, "kill", lambda pid, sig: sent.append(sig))
    real_lines = mesh.pane_lines
    typed = lambda: any(c[:4] == ("agent", "prompt", "w1:p1", "/exit") for c in fake.calls)
    monkeypatch.setattr(mesh, "pane_lines", lambda *a, **k: ["Goodbye!", "Resume this session with:"] if
                        screen_gone and typed() else real_lines(*a, **k))
    real_agent = mesh.pane_agent  # and herdr keeps reading it working after the /exit: the screen decides
    monkeypatch.setattr(mesh, "pane_agent", lambda pane: {**real_agent(pane), "agent_status": "working"} if
                        screen_gone and typed() else real_agent(pane))
    return sent


def test_a_root_slow_to_exit_with_its_screen_gone_is_waited_for_then_relaunched(mesh, fake, run, lean, monkeypatch,
                                                                              tmp_path, capsys):
    """Run mfb6a: /exit outlived the old 20 s check and the reset gave up, leaving the user no root."""
    lean_root(mesh, run, monkeypatch, tmp_path)
    scripted_root(mesh, fake, monkeypatch)
    relaunching(mesh, fake, monkeypatch, exits=False)
    sent = slow_exit(mesh, fake, monkeypatch, [False, True])  # gone by itself, in the late wait
    assert mesh.reset_root(run, "repo-lead", "carry on", None) == 0
    assert sent == [] and fake.called("agent", "start")
    assert mesh.load_run(run)["agents"]["repo-lead"]["pid"] == 201
    assert mesh.read_log(run)[-1]["relaunched"] is True


def test_a_root_that_never_finishes_exiting_is_terminated_then_killed_and_relaunched(mesh, fake, run, lean,
                                                                                    monkeypatch, tmp_path, capsys):
    lean_root(mesh, run, monkeypatch, tmp_path)
    scripted_root(mesh, fake, monkeypatch)
    relaunching(mesh, fake, monkeypatch, exits=False)
    sent = slow_exit(mesh, fake, monkeypatch, [False, False, False, True])  # TERM changed nothing, KILL did
    monkeypatch.setattr(mesh, "pid_alive", lambda pid, start=None: True)
    assert mesh.reset_root(run, "repo-lead", None, None) == 0
    assert sent == [mesh.signal.SIGTERM, mesh.signal.SIGKILL] and fake.called("agent", "start")
    assert "outlived its /exit: SIGTERM" in capsys.readouterr().out


def test_a_root_that_survives_sigkill_fails_the_reset_without_starting_a_second(mesh, fake, run, lean, monkeypatch,
                                                                              tmp_path):
    lean_root(mesh, run, monkeypatch, tmp_path)
    scripted_root(mesh, fake, monkeypatch)
    relaunching(mesh, fake, monkeypatch, exits=False)
    slow_exit(mesh, fake, monkeypatch, [False, False, False, False])
    monkeypatch.setattr(mesh, "pid_alive", lambda pid, start=None: True)
    assert mesh.reset_root(run, "repo-lead", None, None) == 1
    assert prompts(fake) == ["/exit"] and not fake.called("agent", "start")
    assert "SIGKILL" in mesh.read_log(run)[-1]["reason"]


@pytest.mark.parametrize("screen, fg, signalled", [
    (None, 201, False),  # herdr can't read the pane
    (["Do you trust the files in this folder?", " 1. Yes, proceed", " 2. No, exit"], 201, False),  # a dialog
    (["some", "screen", "nobody recognises"], 201, False),
    (None, 50, True),  # unreadable, but the pane's foreground is its shell again: it let go of the terminal
    (["a dialog"], 50, True),
])
def test_a_root_is_signalled_only_on_positive_evidence_that_it_is_exiting(mesh, fake, run, lean, monkeypatch, tmp_path,
                                                                       screen, fg, signalled):
    lean_root(mesh, run, monkeypatch, tmp_path)
    scripted_root(mesh, fake, monkeypatch)
    relaunching(mesh, fake, monkeypatch, exits=False)
    sent = slow_exit(mesh, fake, monkeypatch, [False, False, False, True])
    monkeypatch.setattr(mesh, "pane_lines", lambda *a, **k: screen)
    monkeypatch.setattr(mesh, "pane_process_info", lambda pane: {"foreground_process_group_id": fg, "shell_pid": 50})
    monkeypatch.setattr(mesh, "pid_alive", lambda pid, start=None: True)
    why = mesh.end_root_claude("w1:p1", {"pid": 99, "start": "1"})
    if signalled:
        assert why == "" and sent[:1] == [mesh.signal.SIGTERM]
    else:
        assert sent == [] and "was not signalled" in why and "still runs" in why
        assert ("could not be read" in why) == (screen is None)


def test_a_reset_of_a_root_nobody_can_read_fails_and_leaves_it_running(mesh, fake, run, lean, monkeypatch, tmp_path):
    lean_root(mesh, run, monkeypatch, tmp_path)
    scripted_root(mesh, fake, monkeypatch)
    relaunching(mesh, fake, monkeypatch, exits=False)
    sent = slow_exit(mesh, fake, monkeypatch, [False, False])
    before_exit = mesh.pane_lines  # slow_exit's: the root's real screen until /exit is typed
    monkeypatch.setattr(mesh, "pane_lines", lambda *a, **k: None if any(
        c[:4] == ("agent", "prompt", "w1:p1", "/exit") for c in fake.calls) else before_exit(*a, **k))
    monkeypatch.setattr(mesh, "pane_process_info", lambda pane: {"foreground_process_group_id": 201, "shell_pid": 50})
    monkeypatch.setattr(mesh, "pid_alive", lambda pid, start=None: True)
    assert mesh.reset_root(run, "repo-lead", None, None) == 1
    assert sent == [] and not fake.called("agent", "start")
    assert "was not signalled" in mesh.read_log(run)[-1]["reason"]


def test_a_farewell_left_by_an_earlier_reset_is_not_evidence_the_root_is_exiting(mesh, fake, run, lean, monkeypatch,
                                                                               tmp_path):
    """A second reset in the pane: the first one's `Resume this session with` is still in the scrollback."""
    lean_root(mesh, run, monkeypatch, tmp_path)
    scripted_root(mesh, fake, monkeypatch)
    relaunching(mesh, fake, monkeypatch, exits=False)
    sent = slow_exit(mesh, fake, monkeypatch, [False, False])
    stale = ["Resume this session with:", "claude --resume old", "(a dialog, no input box)"]
    monkeypatch.setattr(mesh, "pane_lines", lambda *a, **k: stale)  # before the /exit and after it
    monkeypatch.setattr(mesh, "pane_process_info", lambda pane: {"foreground_process_group_id": 201, "shell_pid": 50})
    monkeypatch.setattr(mesh, "pid_alive", lambda pid, start=None: True)
    assert mesh.relaunch_root(run, "repo-lead", mesh.load_run(run)["agents"]["repo-lead"],
                              {"pid": 99, "start": "1", "argv": [], "fallback": []})[1].count("was not signalled") == 1
    assert sent == []
    # a new farewell, after the /exit, is evidence
    assert mesh.farewells(stale) == 1 and mesh.exiting("w1:p1", stale + stale, 1) is True
    assert mesh.exiting("w1:p1", stale, 1) is False and mesh.exiting("w1:p1", stale, None) is False


def test_a_root_still_answering_after_the_exit_is_left_running_not_killed(mesh, fake, run, lean, monkeypatch,
                                                                         tmp_path):
    lean_root(mesh, run, monkeypatch, tmp_path)
    scripted_root(mesh, fake, monkeypatch, status="working")  # it took no /exit: it is working
    relaunching(mesh, fake, monkeypatch, exits=False)
    sent = slow_exit(mesh, fake, monkeypatch, [False], screen_gone=False)
    assert mesh.end_root_claude("w1:p1", {"pid": 99, "start": "1"}).startswith("it was sent /exit, but its Claude still")
    assert sent == []


def test_a_root_that_cant_be_relaunched_is_cleared(mesh, fake, run, lean, monkeypatch, tmp_path):
    monkeypatch.setenv("MESH_ROOT_RELAUNCH", "1")
    with mesh.editing_run(run) as data:
        data["agents"]["repo-lead"].update(kind="claude")
    monkeypatch.setattr(mesh, "root_claude", lambda pane: (None, "its Claude wasn't started from the pane's shell"))
    scripted_root(mesh, fake, monkeypatch)
    assert mesh.reset_root(run, "repo-lead", None, None) == 0
    assert prompts(fake)[0] == "/clear"
    # ...but a reopen for added tools has nothing to fall back on
    assert mesh.reset_root(run, "repo-lead", None, None, via="tools", reopen=True) == 1
    assert "couldn't reopen it" in mesh.read_log(run)[-1]["reason"]


def test_a_reopen_resumes_the_roots_conversation_and_says_what_it_added(mesh, fake, run, lean, monkeypatch,
                                                                       tmp_path):
    lean_root(mesh, run, monkeypatch, tmp_path)
    with mesh.editing_run(run) as data:
        data["agents"]["repo-lead"].update(added=["Artifact"], tools_added=True)
    scripted_root(mesh, fake, monkeypatch)
    relaunching(mesh, fake, monkeypatch)
    assert mesh.reset_root(run, "repo-lead", "publish it", None, via="tools", reopen=True) == 0
    start = fake.called("agent", "start")[-1]
    assert start[-4:] == ("--resume", "old", "--system-prompt-snapshot", "off") and "Artifact" not in start
    assert "reopened your conversation with Artifact" in prompts(fake)[-1]


# ── the root keeper and the idle clock ───────────────────────────────────


def test_root_commands_start_the_keeper(mesh, fake, run, capsys):
    assert mesh.main(["inbox"]) == 0
    assert mesh.keepers == [run]
    assert mesh.main(["inbox"]) == 0
    assert mesh.keepers == [run, run]  # the fake starter holds no lock; the real one runs once
    assert mesh.main(["inbox", "--as", "repo-fix-typo"]) == 0
    assert mesh.keepers == [run, run]  # a worker's command doesn't


def test_the_keeper_runs_until_it_starts_a_reset_or_the_run_stops(mesh, run, monkeypatch):
    calls = []
    monkeypatch.setattr(mesh, "maybe_autoreset", lambda *a: calls.append(1) or len(calls) == 3)
    assert mesh.run_root_keeper(run, sleep=lambda s: None) == 0 and len(calls) == 3
    monkeypatch.setenv("MESH_ROOT_AUTORESET", "0")
    calls.clear()
    assert mesh.run_root_keeper(run, sleep=lambda s: None) == 0 and not calls
    assert mesh.ensure_root_keeper(run) is None


def test_the_keeper_ends_once_its_roots_pane_is_gone(mesh, fake, run, monkeypatch):
    monkeypatch.setattr(mesh, "maybe_autoreset", lambda *a: pytest.fail("no root to reset"))
    fake.on("pane", "process-info", error="pane_not_found")
    assert mesh.run_root_keeper(run, sleep=lambda s: None) == 0


def test_the_keeper_holds_its_lock(mesh, run):
    import fcntl
    with mesh.keeper_lock_path(run).open("a") as handle:
        fcntl.flock(handle, fcntl.LOCK_EX)
        assert mesh.ensure_root_keeper(run) is None
    assert mesh.ensure_root_keeper(run) == 4243


def test_idle_counts_from_the_last_turn_not_claude_codes_bookkeeping(mesh, fake, run, monkeypatch):
    """An `away_summary` written 3 minutes after the turn moved the file's mtime: idle is from the turn."""
    path = mesh.claude_sessions_dir("/repo") / "root-s.jsonl"
    path.parent.mkdir(parents=True, exist_ok=True)
    now = datetime.now(timezone.utc)
    turn = {**usage(399_988), "timestamp": (now - timedelta(seconds=400)).isoformat()}
    away = {"type": "system", "subtype": "away_summary", "timestamp": (now - timedelta(seconds=220)).isoformat()}
    path.write_text(json.dumps(turn) + "\n" + json.dumps(away) + "\n")
    os.utime(path, (time.time() - 220, time.time() - 220))
    monkeypatch.setattr(mesh, "pane_agent", lambda pane: {"agent": "claude", "agent_status": "idle",
                                                          "agent_session": {"value": "root-s"}})
    context = mesh.root_context(mesh.load_run(run))
    assert 395 <= context["idle"] <= 410 and not context["cold"]
    assert mesh.last_turn_time(path) == pytest.approx((now - timedelta(seconds=400)).timestamp(), abs=1)


# ── compaction at approve and before a follow-up ─────────────────────────


def transcript(mesh, cwd, session, tokens):
    path = mesh.claude_sessions_dir(cwd) / f"{session}.jsonl"
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(usage(tokens - 12)) + "\n")
    return path


@pytest.mark.parametrize("tokens, exec_model, compacts", [
    (260_000, "opus", True), (240_000, "opus", False),  # same model: compact.approve (250k)
    (210_000, "sonnet", True), (190_000, "sonnet", False)])  # it changes: compact.approve_switch (200k)
def test_approve_compacts_a_big_planner_before_the_swap(mesh, fake, run, monkeypatch, tokens, exec_model, compacts):
    claude_planner(mesh, run, exec_model=exec_model)
    transcript(mesh, "/wt/fix", "s-1", tokens)
    stub_new_pane(fake)
    sent_plan(mesh, run)
    seen = []
    monkeypatch.setattr(mesh, "compact_live_pane", lambda *a: seen.append(a) or {"compacted": True})
    assert mesh.main(["approve", "repo-fix-typo"]) == 0
    assert bool(seen) is compacts
    if compacts:
        assert "plans/repo-fix-typo.md" in seen[0][4] and seen[0][5] == "approve"
        assert seen[0][2]["model"] == "opus"  # on the planning model, before the new pane opens
    with_config({"compact": {"approve": 0, "approve_switch": 0}})
    assert mesh.approve_compaction(run, "repo-fix-typo", {"kind": "claude", "model": "opus", "session": "s-1",
                                                          "cwd": "/wt/fix"}, exec_model) is None


def test_a_live_compaction_types_compact_into_the_idle_pane(mesh, fake, run, monkeypatch):
    claude_worker(mesh, run, cwd="/wt/fix", session="s-1")
    transcript(mesh, "/wt/fix", "s-1", 300_000)
    monkeypatch.setattr(mesh, "pane_agent", lambda pane: {"agent": "claude", "agent_status": "idle"})
    fake.screen("w2:p5", IDLE_BOX)
    entry = mesh.load_run(run)["agents"]["repo-fix-typo"]
    result = mesh.compact_live_pane(run, "repo-fix-typo", entry, "s-1", "keep X", "followup")
    assert fake.called("agent", "prompt", "w2:p5", "/compact keep X")
    assert fake.called("agent", "wait", "w2:p5")
    assert result["compacted"] is False and "didn't shrink" in result["note"]  # the fake file stayed as it was
    event = mesh.read_log(run)[-1]
    assert (event["event"], event["why"], event["tokens"]) == ("compacted", "followup", 300_000)


def test_a_live_compaction_never_types_into_a_draft(mesh, fake, run, monkeypatch):
    claude_worker(mesh, run, cwd="/wt/fix", session="s-1")
    monkeypatch.setattr(mesh, "pane_agent", lambda pane: {"agent": "claude", "agent_status": "working"})
    monkeypatch.setattr(mesh, "COMPACT_QUIET_WAIT", 0)
    entry = mesh.load_run(run)["agents"]["repo-fix-typo"]
    result = mesh.compact_live_pane(run, "repo-fix-typo", entry, "s-1", "keep X", "followup")
    assert result["compacted"] is False and not fake.called("agent", "prompt")


def test_a_live_compaction_skips_when_herdr_lost_the_pane_and_the_follow_up_still_goes(mesh, fake, run,
                                                                                       monkeypatch):
    claude_worker(mesh, run, cwd="/wt/fix", session="s-1", view="pane", model="opus", effort="high")
    path = transcript(mesh, "/wt/fix", "s-1", 300_000)
    os.utime(path, (time.time() - 60, time.time() - 60))
    mesh.log_event(run, "send", sender="repo-lead", to="repo-fix-typo", type="task", file="t.md")
    mesh.log_event(run, "send", sender="repo-fix-typo", to="repo-lead", type="done", file="r.md")
    tries = []

    def lost(pane, entry=None, lost_ok=True):
        tries.append(pane)
        raise mesh.LostRoot("herdr has lost track of this root's pane")
    monkeypatch.setattr(mesh, "quiet_box", lost)
    monkeypatch.setattr(mesh, "COMPACT_QUIET_WAIT", 0)
    delivered = []
    monkeypatch.setattr(mesh, "deliver", lambda *a, **k: delivered.append(a) or {"stored": "x", "rung": True,
                                                                                 "error": None})
    assert mesh.main(["send", "repo-fix-typo", "--type", "answer", "--body", "Fix it."]) == 0
    assert len(tries) == 1 and not fake.called("agent", "prompt") and delivered
    event = [e for e in mesh.read_log(run) if e["event"] == "compacted"][-1]
    assert not event["ok"] and "lost track" in event["note"]


def test_a_follow_up_whose_background_copy_fails_is_stored_for_a_resume(mesh, fake, run, monkeypatch, capsys):
    claude_worker(mesh, run, cwd="/wt/fix", session="s-1", view="bg", bg_id="b1", model="opus", effort="high")
    transcript(mesh, "/wt/fix", "s-1", 300_000)
    mesh.log_event(run, "send", sender="repo-lead", to="repo-fix-typo", type="task", file="t.md")
    mesh.log_event(run, "send", sender="repo-fix-typo", to="repo-lead", type="done", file="r.md")

    def swap(*a, **k):
        err = mesh.MeshError("repo-fix-typo was not reopened on opus (`claude --bg` did not start it)")
        err.stored = mesh.record(run, "repo-lead", "repo-fix-typo", "answer", "x")
        raise err
    monkeypatch.setattr(mesh, "swap_model", swap)
    assert mesh.main(["send", "repo-fix-typo", "--type", "answer", "--body", "Fix it."]) == 2
    assert "mesh resume repo-fix-typo" in json.loads(capsys.readouterr().out)["error"]


@pytest.mark.parametrize("tokens, read_only, report, compacts", [
    (300_000, False, "done", True), (200_000, False, "done", False), (300_000, True, "done", False),
    (300_000, False, "plan", False)])
def test_a_follow_up_to_a_big_live_worker_compacts_it_first(mesh, fake, run, monkeypatch, tokens, read_only,
                                                           report, compacts):
    claude_worker(mesh, run, cwd="/wt/fix", session="s-1", view="pane", read_only=read_only, model="opus",
                  effort="high")
    path = transcript(mesh, "/wt/fix", "s-1", tokens)
    os.utime(path, (time.time() - 60, time.time() - 60))  # warm
    mesh.log_event(run, "send", sender="repo-lead", to="repo-fix-typo", type="task", file="t.md")
    mesh.log_event(run, "send", sender="repo-fix-typo", to="repo-lead", type=report, file="r.md")
    seen = []
    monkeypatch.setattr(mesh, "compact_live_pane", lambda *a: seen.append(a) or {"compacted": True})
    monkeypatch.setattr(mesh, "deliver", lambda *a, **k: {"stored": "x", "rung": True, "error": None})
    assert mesh.main(["send", "repo-fix-typo", "--type", "answer", "--body", "Fix the review findings."]) == 0
    assert bool(seen) is compacts
    if compacts:
        assert "Fix the review findings" in seen[0][4]


# ── the user's MCP servers, picked per task (`--mcp`, `mesh tools servers`; design §11) ──

USER_SERVERS = {
    "todoist": {"command": "t", "args": ["--token", "secret-arg"], "env": {"TOKEN": "secret"}},
    "playwright": {"type": "http", "url": "http://localhost:8931/mcp"},
    "context7": {"type": "http", "url": "https://mcp.context7.com/mcp", "headers": {"KEY": "secret-header"}},
    "flow": {"command": "/usr/bin/flow", "args": ["serve"]},
}


@pytest.fixture
def gitrepo(tmp_path):
    path = tmp_path / "proj"
    path.mkdir()
    for cmd in (["init", "-q", "-b", "main"], ["config", "user.email", "t@t"], ["config", "user.name", "t"],
                ["commit", "-q", "--allow-empty", "-m", "init"]):
        subprocess.run(["git", *cmd], cwd=path, check=True)
    return path


@pytest.fixture
def users(mesh, monkeypatch):
    """A user config with the personal servers, Playwright's one shared browser and Context7."""
    monkeypatch.setenv("MESH_CLAUDE_LEAN", "1")
    home = Path(os.environ["CLAUDE_CONFIG_DIR"])
    home.mkdir(parents=True, exist_ok=True)
    (home / ".claude.json").write_text(json.dumps({"mcpServers": USER_SERVERS}))
    return USER_SERVERS


def spawn_claude(mesh, repo, slug, *flags, task="Add the feature."):
    return mesh.main(["spawn", "--tier", "strong", "--kind", "claude", "--repo", str(repo), "--worktree", slug,
                      "--task", task, *flags])


def launched_servers(mesh, run, name):
    return mcp_servers(mesh.mcp_file(run, name))


def test_context7_comes_with_no_config_of_the_users(mesh):
    assert mesh.setting("claude.mcp_keep") == ["context7"]


def test_mcp_names_the_users_servers_a_spawned_worker_starts_with(mesh, fake, cli, run, gitrepo, users, capsys):
    stub_panes(fake)
    assert spawn_claude(mesh, gitrepo, "plain") == 0
    assert launched_servers(mesh, run, "proj-plain") == {"context7"}  # none by default
    assert "mcp" not in mesh.load_run(run)["agents"]["proj-plain"]
    assert spawn_claude(mesh, gitrepo, "shot", "--mcp", "playwright") == 0
    assert launched_servers(mesh, run, "proj-shot") == {"context7", "playwright"}  # not todoist, not flow
    assert mesh.load_run(run)["agents"]["proj-shot"]["mcp"] == ["playwright"]
    path = mesh.mcp_file(run, "proj-shot")
    assert stat.S_IMODE(os.stat(path).st_mode) == 0o600


def test_an_unknown_server_or_a_cli_without_lean_sessions_refuses_before_anything_starts(mesh, fake, run, gitrepo,
                                                                                         users, monkeypatch, capsys):
    stub_panes(fake)
    capsys.readouterr()
    assert spawn_claude(mesh, gitrepo, "x", "--mcp", "nope") == 1
    err = capsys.readouterr().err
    assert "nope" in err and "playwright" in err  # names what it can pass
    assert mesh.main(["spawn", "--tier", "strong", "--kind", "agy", "--repo", str(gitrepo), "--worktree", "y",
                      "--task", "Add it.", "--mcp", "playwright"]) == 1
    assert "Claude" in capsys.readouterr().err
    monkeypatch.setenv("MESH_CLAUDE_LEAN", "0")
    assert spawn_claude(mesh, gitrepo, "z", "--mcp", "playwright") == 1
    assert "claude.lean" in capsys.readouterr().err
    monkeypatch.setenv("MESH_CLAUDE_LEAN", "1")
    with_config({"claude": {"mcp": "inherit"}})
    assert spawn_claude(mesh, gitrepo, "w", "--mcp", "playwright") == 1
    assert "inherit" in capsys.readouterr().err
    assert not [a for a in mesh.load_run(run)["agents"] if a.startswith("proj-")]
    assert not list(mesh.worktree_root().glob("*/proj-*"))  # no worktree was left behind


def test_a_server_the_root_passed_doesnt_unhide_a_plugin_or_skill_of_that_name(mesh, run, lean):
    home = Path(os.environ["CLAUDE_CONFIG_DIR"])
    state = json.loads((home / ".claude.json").read_text())
    state["mcpServers"].update({"pr-review-toolkit": {"command": "p"}, "dataviz": {"command": "d"}})
    (home / ".claude.json").write_text(json.dumps(state))
    entry = entry_for(lean, mcp=["pr-review-toolkit", "dataviz"])
    argv = mesh.claude_launch_argv(run, "repo-fix-typo", entry)
    assert {"pr-review-toolkit", "dataviz"} <= mcp_servers(argv[argv.index("--mcp-config") + 1])
    settings = mesh.lean_settings(entry)
    assert settings["enabledPlugins"] == {"pr-review-toolkit@official": False}
    assert settings["skillOverrides"]["dataviz"] == "user-invocable-only"
    assert "--system-prompt-snapshot" not in argv  # present from the first prompt: nothing to un-snapshot


def test_a_resume_keeps_replaces_or_clears_the_servers_it_had(mesh, fake, run, gitrepo, users, capsys):
    stub_new_pane(fake)
    cases = (([], {"context7", "playwright"}, ["playwright"]), (["--mcp", "flow"], {"context7", "flow"}, ["flow"]),
             (["--mcp", "none"], {"context7"}, None))
    for n, (flags, servers, recorded) in enumerate(cases):
        run_id = f"mold{n}"
        archived_worker(mesh, gitrepo, kind="claude", session="c1", run_id=run_id)
        archive = mesh.mesh_home() / "archive" / run_id / "run.json"
        data = json.loads(archive.read_text())
        data["agents"]["proj-fix"].update(mcp=["playwright"], status="retired")
        archive.write_text(json.dumps(data))
        capsys.readouterr()
        assert mesh.main(["resume", "--tier", "strong", "--from-run", run_id, "proj-fix", "--task", "Go on.",
                          *flags]) == 0, flags
        name = json.loads(capsys.readouterr().out)["name"]
        assert launched_servers(mesh, run, name) == servers, flags
        assert mesh.load_run(run)["agents"][name].get("mcp") == recorded, flags
        with mesh.editing_run(run) as data:  # free the name and the browser for the next case
            data["agents"][name]["status"] = "retired"


def test_an_approved_plan_reopens_with_the_servers_it_started_with(mesh, fake, run, users, gitrepo):
    claude_planner(mesh, run, mcp=["playwright"], repo=str(gitrepo), cwd=str(gitrepo))
    stub_new_pane(fake)
    sent_plan(mesh, run)
    assert mesh.main(["approve", "repo-fix-typo"]) == 0
    assert launched_servers(mesh, run, "repo-fix-typo") == {"context7", "playwright"}


def test_a_background_worker_gets_them_too(mesh, cli, run, gitrepo, users, capsys):
    claude_root(mesh, run)
    bg_cli(cli)
    assert mesh.main(["spawn", "--tier", "strong", "--kind", "claude", "--repo", str(gitrepo), "--worktree", "bgw",
                      "--size", "small", "--task", "Fix the typo.", "--mcp", "flow"]) == 0
    assert launched_servers(mesh, run, json.loads(capsys.readouterr().out)["name"]) == {"context7", "flow"}
    assert "--bg" in cli.calls[-1]["argv"] or cli.called("--bg")


def lease_entry(**extra):
    return {"role": "worker", "kind": "claude", "status": "running", "pane": "w1:p9", **extra}


def other_run(mesh, run_id, agents, archived=False):
    folder = mesh.mesh_home() / ("archive" if archived else "") / run_id
    folder.mkdir(parents=True)
    (folder / "run.json").write_text(json.dumps({"id": run_id, "root": "x-lead", "agents": agents}))


def test_one_agent_at_a_time_holds_a_shared_browser(mesh, run, users):
    with mesh.editing_run(run) as data:
        data["agents"]["repo-fix-typo"].update(kind="claude", mcp=["playwright"])
    assert mesh.setting("claude.mcp_exclusive") == ["playwright"]
    assert mesh.mcp_holder("playwright") == (run, "repo-fix-typo")
    assert mesh.mcp_holder("flow") is None and mesh.mcp_holder("playwright", skip=(run, "repo-fix-typo")) is None
    with pytest.raises(mesh.MeshError, match=r"repo-fix-typo.*retire"):
        mesh.check_mcp_lease(["playwright"])
    mesh.check_mcp_lease(["flow"])  # not shared: any number may hold it
    with mesh.editing_run(run) as data:
        data["agents"]["repo-fix-typo"]["status"] = "retired"
    assert mesh.mcp_holder("playwright") is None
    mesh.check_mcp_lease(["playwright"])


def test_the_browser_is_held_across_runs_until_that_run_stops(mesh, run, users):
    other_run(mesh, "m2", {"w-shot": lease_entry(mcp=["playwright"])})
    assert mesh.mcp_holder("playwright") == ("m2", "w-shot")
    with pytest.raises(mesh.MeshError, match=r"w-shot.*run m2"):
        mesh.check_mcp_lease(["playwright"])
    other_run(mesh, "m3", {"w-old": lease_entry(mcp=["playwright"])}, archived=True)  # a stopped run holds nothing
    shutil.rmtree(mesh.mesh_home() / "m2")
    assert mesh.mcp_holder("playwright") is None
    with_config({"claude": {"mcp_exclusive": []}})
    other_run(mesh, "m4", {"w-shot": lease_entry(mcp=["playwright"])})
    mesh.check_mcp_lease(["playwright"])  # the user's config says it isn't shared


def test_a_worker_that_added_the_browser_back_holds_it_too(mesh, run, users, monkeypatch, capsys):
    claude_worker(mesh, run)
    with mesh.editing_run(run) as data:
        data["agents"]["repo-lead"]["kind"] = "claude"
    monkeypatch.setattr(mesh, "start_tools_reopen", lambda *a: 77)
    assert mesh.main(["tools", "add", "playwright", "--as", "repo-fix-typo"]) == 0
    assert mesh.mcp_holder("playwright") == (run, "repo-fix-typo")
    entry = mesh.load_run(run)["agents"]["repo-fix-typo"]
    assert entry["mcp"] == ["playwright"] and "added" not in entry  # so no resume can keep it past `--mcp none`
    monkeypatch.setattr(mesh, "start_reset", lambda *a, **k: 5)
    monkeypatch.setenv("MESH_ROOT_RELAUNCH", "1")
    assert mesh.main(["tools", "add", "playwright"]) == 1  # the root can't take it from the worker
    assert "repo-fix-typo" in capsys.readouterr().err
    assert "playwright" not in (mesh.load_run(run)["agents"]["repo-lead"].get("added") or [])


def test_a_plugin_or_skill_added_back_doesnt_hold_the_browser(mesh, run, users):
    claude_worker(mesh, run, added=["playwright"])  # a plugin or skill of that name, not the server
    assert mesh.mcp_holder("playwright") is None


def test_a_send_that_reopens_a_worker_leaves_out_a_server_someone_else_holds(mesh, fake, run, gitrepo, users, capsys):
    stub_new_pane(fake)
    archived_worker(mesh, gitrepo, kind="claude", session="c1", run_id="mold")
    archive = mesh.mesh_home() / "archive" / "mold" / "run.json"
    data = json.loads(archive.read_text())
    data["agents"]["proj-fix"].update(mcp=["playwright", "flow"], status="retired")
    archive.write_text(json.dumps(data))
    other_run(mesh, "m2", {"w-shot": lease_entry(mcp=["playwright"])})
    capsys.readouterr()
    assert mesh.main(["resume", "--tier", "strong", "--from-run", "mold", "proj-fix", "--task", "Go on."]) == 0
    out = json.loads(capsys.readouterr().out)
    assert launched_servers(mesh, run, out["name"]) == {"context7", "flow"}
    assert any("playwright is held" in n for n in out["notes"])
    assert mesh.load_run(run)["agents"][out["name"]]["mcp"] == ["flow"]
    capsys.readouterr()  # asked for by name, it is refused
    assert mesh.main(["resume", "--tier", "strong", "--from-run", "mold", "proj-fix", "--task", "Go on.",
                      "--mcp", "playwright"]) == 1
    assert "w-shot" in capsys.readouterr().err


def test_a_second_spawn_on_the_browser_is_refused_naming_the_holder(mesh, fake, run, gitrepo, users, capsys):
    stub_panes(fake)
    assert spawn_claude(mesh, gitrepo, "one", "--mcp", "playwright") == 0
    capsys.readouterr()
    assert spawn_claude(mesh, gitrepo, "two", "--mcp", "playwright") == 1
    assert "proj-one" in capsys.readouterr().err
    assert "proj-two" not in mesh.load_run(run)["agents"] and not (mesh.worktree_root() / run / "proj-two").exists()
    assert spawn_claude(mesh, gitrepo, "three", "--mcp", "flow") == 0  # a server nobody shares
    mesh.retire_worker(run, "proj-one", mesh.load_run(run)["agents"]["proj-one"])
    assert spawn_claude(mesh, gitrepo, "four", "--mcp", "playwright") == 0  # the holder is retired


def test_two_spawns_at_once_cant_both_take_the_browser(mesh, run, users):
    barrier = threading.Barrier(2)
    results = []

    def claim(name):
        barrier.wait()
        try:
            with mesh.mcp_lease(["playwright"]):
                time.sleep(0.05)  # long enough for the other to be at the lock
                with mesh.editing_run(run) as data:
                    data["agents"][name] = lease_entry(mcp=["playwright"])
            results.append("won")
        except mesh.MeshError:
            results.append("refused")

    threads = [threading.Thread(target=claim, args=(n,)) for n in ("a", "b")]
    for t in threads:
        t.start()
    for t in threads:
        t.join()
    assert sorted(results) == ["refused", "won"]


def test_a_resume_of_a_worker_holding_the_browser_doesnt_count_it_against_itself(mesh, run, users):
    with mesh.editing_run(run) as data:
        data["agents"]["repo-fix-typo"].update(kind="claude", mcp=["playwright"])
    with mesh.mcp_lease(["playwright"], skip=(run, "repo-fix-typo")):
        pass


def test_tools_servers_tells_the_root_what_it_can_pass(mesh, run, users, capsys):
    other_run(mesh, "m2", {"w-shot": lease_entry(mcp=["playwright"])})
    with_config({"claude": {"mcp_about": {"flow": "the user's flow notes"}}})
    assert mesh.main(["tools", "servers", "--json"]) == 0
    rows = {r["name"]: r for r in json.loads(capsys.readouterr().out)}
    assert set(rows) == {"todoist", "playwright", "context7", "flow"}
    assert rows["flow"]["about"] == "the user's flow notes"
    assert rows["playwright"]["about"] == "http localhost:8931" and rows["playwright"]["exclusive"] is True
    assert rows["playwright"]["held_by"] == "w-shot (run m2)" and rows["todoist"]["held_by"] is None
    assert rows["context7"]["always"] is True and rows["flow"]["always"] is False
    assert rows["todoist"]["about"] == "stdio t" and rows["context7"]["about"] == "http mcp.context7.com"
    assert mesh.main(["tools", "servers"]) == 0
    text = capsys.readouterr().out
    assert "playwright" in text and "exclusive" in text and "held by w-shot" in text
    for secret in ("secret", "secret-arg", "secret-header", "--token"):  # config and arguments stay out of it
        assert secret not in text
        assert secret not in json.dumps(rows)


def test_tools_servers_works_for_a_root_that_isnt_claude(mesh, run, users, capsys):
    with mesh.editing_run(run) as data:
        data["agents"]["repo-lead"]["kind"] = "agy"
    assert mesh.main(["tools", "servers", "--json"]) == 0
    assert len(json.loads(capsys.readouterr().out)) == 4


def test_the_policy_leaves_check_their_types(mesh, tmp_path, monkeypatch):
    assert mesh.setting("claude.mcp_about") == {} and mesh.setting("claude.mcp_exclusive") == ["playwright"]
    with_config({"claude": {"mcp_about": {"flow": 3}}})
    with pytest.raises(mesh.MeshError, match="mcp_about"):
        mesh.setting("claude.mcp_about")
    with_config({"claude": {"mcp_about": {"flow": "notes"}}})
    assert mesh.setting("claude.mcp_about") == {"flow": "notes"}
    assert "mcp_about" in mesh.render_routing(mesh.routing())


GUARDED = (("a planner", {"planning": True}), ("an executor", {}), ("a read-only asker", {"read_only": True}),
           ("an agent with a task list", {"mcp": ["playwright"]}), ("a reviewer", {"read_only": True,
                                                                                    "mcp": ["flow"]}))


@pytest.mark.parametrize("who,extra", GUARDED, ids=[g[0] for g in GUARDED])
def test_every_lean_agent_keeps_context7_and_web_search(mesh, run, users, who, extra):
    entry = entry_for(Path("/nonexistent"), **extra)
    argv = mesh.claude_launch_argv(run, "repo-fix-typo", entry)
    assert "context7" in mcp_servers(argv[argv.index("--mcp-config") + 1])
    assert not {"WebSearch", "WebFetch"} & set(deny_rules(argv))
    assert not {"WebSearch", "WebFetch"} & set(mesh.lean_tools(entry))
    root = {**entry, "role": "root"}
    assert not {"WebSearch", "WebFetch"} & set(mesh.lean_tools(root))


def test_a_resume_that_clears_or_drops_a_server_added_back_launches_without_it(mesh, fake, run, gitrepo, users,
                                                                              monkeypatch, capsys):
    stub_new_pane(fake)
    monkeypatch.setattr(mesh, "start_tools_reopen", lambda *a: 77)
    for n, flags in enumerate((["--mcp", "none"], [])):  # cleared by the root; dropped as another holds it
        run_id = f"mold{n}"
        archived_worker(mesh, gitrepo, kind="claude", session="c1", run_id=run_id)
        archive = mesh.mesh_home() / "archive" / run_id / "run.json"
        data = json.loads(archive.read_text())
        data["agents"]["proj-fix"].update(mcp=["playwright"], status="retired")
        archive.write_text(json.dumps(data))
        if not flags:
            other_run(mesh, "m2", {"w-shot": lease_entry(mcp=["playwright"])})
        capsys.readouterr()
        assert mesh.main(["resume", "--tier", "strong", "--from-run", run_id, "proj-fix", "--task", "Go.", *flags]) == 0
        name = json.loads(capsys.readouterr().out)["name"]
        assert launched_servers(mesh, run, name) == {"context7"}, flags
        assert "playwright" not in str(mesh.load_run(run)["agents"][name].get("added"))
        with mesh.editing_run(run) as data:
            data["agents"][name]["status"] = "retired"


def test_a_failed_spawn_or_resume_doesnt_hold_the_server(mesh, fake, run, gitrepo, users, monkeypatch, capsys):
    stub_panes(fake)
    real = mesh.place_pane

    def broken(*a, **k):
        raise mesh.MeshError("no pane")
    monkeypatch.setattr(mesh, "place_pane", broken)
    assert spawn_claude(mesh, gitrepo, "one", "--mcp", "playwright") == 1
    assert mesh.mcp_holder("playwright") is None and not mesh.mcp_file(run, "proj-one").exists()
    monkeypatch.setattr(mesh, "place_pane", real)
    assert spawn_claude(mesh, gitrepo, "two", "--mcp", "playwright") == 0  # the failed one didn't hold it
    mesh.retire_worker(run, "proj-two", mesh.load_run(run)["agents"]["proj-two"])
    archived_worker(mesh, gitrepo, kind="claude", session="c1", run_id="mold")
    archive = mesh.mesh_home() / "archive" / "mold" / "run.json"
    data = json.loads(archive.read_text())
    data["agents"]["proj-fix"].update(mcp=["playwright"], status="retired")
    archive.write_text(json.dumps(data))
    monkeypatch.setattr(mesh, "place_pane", broken)
    assert mesh.main(["resume", "--tier", "strong", "--from-run", "mold", "proj-fix", "--task", "Go."]) == 1
    assert mesh.mcp_holder("playwright") is None


def test_an_entry_whose_start_died_holds_nothing_past_the_grace(mesh, run, users):
    old = (datetime.now(timezone.utc) - timedelta(seconds=mesh.STARTING_GRACE + 5)).isoformat(timespec="seconds")
    other_run(mesh, "m2", {"w-dead": lease_entry(mcp=["playwright"], status="starting", pane=None, spawned=old)})
    assert mesh.mcp_holder("playwright") is None
    other_run(mesh, "m3", {"w-new": lease_entry(mcp=["playwright"], status="starting", pane=None,
                                                spawned=datetime.now(timezone.utc).isoformat(timespec="seconds"))})
    assert mesh.mcp_holder("playwright") == ("m3", "w-new")  # a start in progress does


def test_claude_ai_connectors_hold_every_shared_server(mesh, run, users, monkeypatch, capsys):
    other_run(mesh, "m2", {"w-all": lease_entry(mcp_all=True)})
    assert mesh.mcp_holder("playwright") == ("m2", "w-all") and mesh.mcp_holder("flow") is None
    shutil.rmtree(mesh.mesh_home() / "m2")
    claude_worker(mesh, run)
    other_run(mesh, "m3", {"w-shot": lease_entry(mcp=["playwright"])})
    monkeypatch.setattr(mesh, "start_tools_reopen", lambda *a: 77)
    assert mesh.main(["tools", "add", "claude.ai Gmail", "--as", "repo-fix-typo"]) == 1  # it would load the browser
    assert "w-shot" in capsys.readouterr().err and "mcp_all" not in mesh.load_run(run)["agents"]["repo-fix-typo"]


def test_the_compaction_before_a_resume_runs_with_the_servers_it_will_reopen_with(mesh, fake, run, gitrepo, users,
                                                                                  monkeypatch, capsys):
    stub_new_pane(fake)
    seen = []
    real = mesh.maybe_compact
    monkeypatch.setattr(mesh, "maybe_compact", lambda a, entry, *r, **k: seen.append(entry["mcp"]) or real(a, entry, *r, **k))
    archived_worker(mesh, gitrepo, kind="claude", session="c1", run_id="mold")
    archive = mesh.mesh_home() / "archive" / "mold" / "run.json"
    data = json.loads(archive.read_text())
    data["agents"]["proj-fix"].update(mcp=["playwright"], status="retired")
    archive.write_text(json.dumps(data))
    assert mesh.main(["resume", "--tier", "strong", "--from-run", "mold", "proj-fix", "--task", "Go.",
                      "--mcp", "flow"]) == 0
    assert seen == [["flow"]]


def test_tools_servers_needs_no_run(mesh, users, capsys):
    assert mesh.main(["tools", "servers", "--json"]) == 0
    assert len(json.loads(capsys.readouterr().out)) == 4
