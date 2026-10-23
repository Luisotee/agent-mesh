import json
import subprocess

import pytest


def as_executor(monkeypatch, run):
    monkeypatch.setenv("MESH_RUN", run)
    monkeypatch.setenv("MESH_SELF", "repo-fix-typo")


# ── init / identity ──────────────────────────────────────────────────────


def test_init_names_root_and_maps_pane(mesh, fake, run, capsys):
    assert fake.called("agent", "rename", "w1:p1", "repo-lead")
    data = mesh.load_run(run)
    assert data["root"] == "repo-lead"
    assert data["agents"]["repo-lead"]["kind"] == "claude"
    # the root has no MESH_* env, so its identity comes from the pane map
    import argparse
    assert mesh.identity(argparse.Namespace()) == ("mtest", "repo-lead")


def test_init_outside_herdr_fails(mesh, fake, monkeypatch, capsys):
    monkeypatch.delenv("HERDR_ENV", raising=False)
    assert mesh.main(["init", "--mode", "agy"]) == 1
    assert "inside a herdr pane" in capsys.readouterr().err


def test_identity_without_run_fails(mesh, fake, monkeypatch, capsys):
    monkeypatch.delenv("HERDR_PANE_ID", raising=False)
    assert mesh.main(["inbox"]) == 1
    assert "not in a mesh run" in capsys.readouterr().err


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


def test_send_to_parent_and_inbox_marks_read(mesh, fake, run, monkeypatch, capsys):
    as_executor(monkeypatch, run)
    assert mesh.main(["send", "parent", "--type", "question", "--body", "Include _db.py?"]) == 0
    doorbell = fake.called("agent", "prompt")[-1]
    assert doorbell[2] == "repo-lead"
    assert doorbell[3].startswith("📬 [mesh] question from repo-fix-typo: Include _db.py?")

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


def test_send_rejects_unknown_agent_and_empty_body(mesh, fake, run, capsys):
    assert mesh.main(["send", "nobody", "--type", "info", "--body", "x"]) == 1
    assert mesh.main(["send", "repo-fix-typo", "--type", "info", "--body", "  "]) == 1


def test_first_message_introduces_the_agent(mesh, fake, run):
    mesh.deliver(run, "repo-lead", "repo-fix-typo", "task", "Do X.", first=True)
    text = fake.called("agent", "prompt")[-1][3]
    assert "You are repo-fix-typo (executor)" in text
    assert "your parent is repo-lead" in text
    assert "PROTOCOL.md" in text


# ── models ───────────────────────────────────────────────────────────────

AGY_MODELS = """gemini-4-argon-medium\tGemini 4 Argon (Medium)
gemini-4-argon-high\tGemini 4 Argon (High)
gemini-3.8-flash-high\tGemini 3.8 Flash (High)
gemini-3.8-flash-medium\tGemini 3.8 Flash (Medium)
gemini-3.10-flash-medium\tGemini 3.10 Flash (Medium)
claude-opus-5-5-high\tClaude Opus 5.5 (High)
"""


def test_pick_agy_model_newest_family_and_effort(mesh):
    assert mesh.pick_agy_model(AGY_MODELS, "flash", "medium") == "gemini-3.10-flash-medium"
    assert mesh.pick_agy_model(AGY_MODELS, "flash", "high") == "gemini-3.8-flash-high"
    assert mesh.pick_agy_model(AGY_MODELS, "argon", "high") == "gemini-4-argon-high"
    assert mesh.pick_agy_model(AGY_MODELS, "gemini-3.8-flash-high", "low") == "gemini-3.8-flash-high"
    with pytest.raises(mesh.MeshError):
        mesh.pick_agy_model(AGY_MODELS, "flash", "low")


def test_claude_argv_keeps_alias(mesh):
    argv, model = mesh.agent_argv("claude", "opus", "high")
    assert model == "opus"
    assert argv == ["--model", "opus", "--effort", "high", "--dangerously-skip-permissions"]


# ── watch ────────────────────────────────────────────────────────────────


def test_watch_returns_when_child_reports_done(mesh, fake, run, monkeypatch):
    fake.on("agent", "get", result={"agent": {"agent_status": "working"}})
    as_executor(monkeypatch, run)

    def sleep(_):
        if not mesh.sends_from(run, "repo-fix-typo", ("done",)):
            mesh.deliver(run, "repo-fix-typo", "repo-lead", "done", "All green.")

    monkeypatch.setattr(mesh.time, "sleep", sleep)
    assert mesh.main(["watch", "repo-fix-typo"]) == 0


def test_watch_alerts_on_silent_stop(mesh, fake, run, monkeypatch, capsys):
    states = iter(["working", "working", "idle"])
    monkeypatch.setattr(mesh, "agent_status", lambda name: next(states))
    assert mesh.main(["watch", "repo-fix-typo"]) == 3
    assert "without sending a message" in capsys.readouterr().out


def test_watch_question_then_idle_is_not_an_alert(mesh, fake, run, monkeypatch):
    states = iter(["working", "idle", "idle", "gone"])

    def status(name):
        state = next(states)
        if state == "idle" and not mesh.sends_from(run, name):
            mesh.deliver(run, name, "repo-lead", "question", "Which base?")
        return state

    monkeypatch.setattr(mesh, "agent_status", status)
    # the question keeps the watch going; it ends only when the agent disappears
    assert mesh.main(["watch", "repo-fix-typo"]) == 5


def test_watch_alerts_when_blocked(mesh, fake, run, monkeypatch):
    monkeypatch.setattr(mesh, "agent_status", lambda name: "blocked")
    assert mesh.main(["watch", "repo-fix-typo"]) == 4


# ── worktrees / stop ─────────────────────────────────────────────────────


@pytest.fixture
def repo(tmp_path):
    path = tmp_path / "proj"
    path.mkdir()
    for cmd in (["init", "-q", "-b", "main"], ["config", "user.email", "t@t"], ["config", "user.name", "t"],
                ["commit", "-q", "--allow-empty", "-m", "init"]):
        subprocess.run(["git", *cmd], cwd=path, check=True)
    return path


def test_stop_removes_clean_worktrees_and_keeps_dirty(mesh, fake, run, repo, capsys):
    clean, _ = mesh.add_worktree(run, repo, "clean", None)
    dirty, branch = mesh.add_worktree(run, repo, "dirty", None)
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
    # branches survive the worktree removal
    assert "mesh/mtest/clean" in subprocess.run(["git", "branch"], cwd=repo, capture_output=True, text=True).stdout


def test_spawn_creates_tab_then_splits(mesh, fake, run, repo, monkeypatch, capsys):
    fake.on("workspace", "list", result={"workspaces": [{"workspace_id": "w9", "label": "proj"}]})
    fake.on("tab", "create", result={"tab": {"tab_id": "w9:t4"}, "root_pane": {"pane_id": "w9:p10"}})
    fake.on("pane", "layout", result={"layout": {"panes": [{"pane_id": "w9:p10", "rect": {"width": 200, "height": 50}}]}})
    fake.on("pane", "split", result={"pane": {"pane_id": "w9:p11"}})
    fake.on("agent", "start", errors=["cli_error"], result={"agent": {}})  # shell not ready on the first try
    monkeypatch.setattr(mesh, "resolve_agy_model", lambda family, effort: f"gemini-3.8-{family}-{effort}")

    assert mesh.main(["spawn", "--kind", "agy", "--role", "executor", "--repo", str(repo), "--model", "flash",
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

    assert mesh.main(["spawn", "--kind", "claude", "--role", "planner", "--repo", str(repo), "--model", "opus",
                      "--effort", "high"]) == 0
    second = json.loads(capsys.readouterr().out)
    assert second["pane"] == "w9:p11"
    assert fake.called("pane", "split", "w9:p10", "--direction", "right")
    assert mesh.load_run(run)["agents"]["proj-planner"]["status"] == "running"


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


def test_doorbell_confirms_delivery_and_retries_when_stalled(mesh, fake, run):
    fake.on("agent", "prompt", errors=["agent_prompt_stalled"])
    assert mesh.main(["send", "repo-fix-typo", "--type", "info", "--body", "hi"]) == 0
    calls = fake.called("agent", "prompt")
    assert len(calls) == 2
    assert calls[0][4:8] == ("--wait", "--until", "working", "--timeout")


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
    assert mesh.agent_name("curupira-bot", "planner", set()) == "curupira-bot-planner"
    assert mesh.agent_name("curupira-bot", "planner", {"curupira-bot-planner"}) == "curupira-bot-planner-2"
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


def test_escalation_executor_reuses_the_worktree_slug(mesh, run):
    import argparse
    with mesh.editing_run(run) as data:
        data["worktrees"] = [{"path": "/wt/proj-fix-reactions", "repo": "/proj", "branch": "mesh/mtest/fix-reactions"}]
    args = argparse.Namespace(name=None, role="executor", worktree=None, cwd="/wt/proj-fix-reactions")
    suffix = mesh.spawn_suffix(args, mesh.load_run(run))
    assert mesh.agent_name("proj", suffix, {"proj-fix-reactions"}) == "proj-fix-reactions-2"


def test_tab_label_comes_from_the_request(mesh):
    assert mesh.tab_label({"id": "m1", "request": "fix the reaction bug in curupira and port it"}) == "mesh: fix the reaction bug in"
    assert mesh.tab_label({"id": "m1", "request": ""}) == "mesh: m1"


def test_workspace_found_by_label_then_by_pane_inside_repo(mesh, fake, tmp_path):
    repo = tmp_path / "city-tour-app"
    (repo / "src").mkdir(parents=True)
    fake.on("workspace", "list", result={"workspaces": [{"workspace_id": "w1", "label": "other"},
                                                        {"workspace_id": "w2", "label": "walklore"}]})
    fake.on("pane", "list", "--workspace", "w2", result={"panes": [{"cwd": str(repo / "src")}]})
    assert mesh.find_or_create_workspace(repo) == ("w2", False)
    fake.on("workspace", "list", result={"workspaces": [{"workspace_id": "w3", "label": "City-Tour-App"}]})
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
