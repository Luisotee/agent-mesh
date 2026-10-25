import json
import subprocess

import pytest


def as_worker(monkeypatch, run):
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
    as_worker(monkeypatch, run)
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
    as_worker(monkeypatch, run)

    def sleep(_):
        if not mesh.sends_from(run, "repo-fix-typo", ("done",)):
            mesh.deliver(run, "repo-fix-typo", "repo-lead", "done", "All green.")

    monkeypatch.setattr(mesh.time, "sleep", sleep)
    assert mesh.main(["watch", "repo-fix-typo"]) == 0


def test_watch_alerts_on_silent_stop(mesh, fake, run, monkeypatch, capsys):
    states = iter(["working", "working", "idle"])
    monkeypatch.setattr(mesh, "agent_status", lambda name, *_: next(states))
    assert mesh.main(["watch", "repo-fix-typo"]) == 3
    assert "without sending a message" in capsys.readouterr().out


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


def test_watch_alerts_when_blocked(mesh, fake, run, monkeypatch):
    monkeypatch.setattr(mesh, "agent_status", lambda name, *_: "blocked")
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
    assert fake.called("agent", "start")[-1][-5:] == ("--model", "opus", "--effort", "high", "--dangerously-skip-permissions")


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
    ]
    history.write_text("\n".join(json.dumps(line) for line in lines) + "\nnot json\n")
    assert mesh.agy_conversation("proj-fix", "mold", wt) == "c1"
    assert mesh.agy_conversation("proj-fix-2", "mold", wt) == "c3"
    assert mesh.agy_conversation("proj-fix", "mol", wt) == "c3"  # wrong run: only the cwd fallback is left
    assert mesh.agy_conversation("ghost", "mold", None) is None


def test_agy_worker_in_a_main_checkout_never_borrows_the_users_session(mesh, tmp_path, monkeypatch):
    history = tmp_path / "history.jsonl"
    monkeypatch.setenv("MESH_AGY_HISTORY", str(history))
    history.write_text(json.dumps({"display": "fix the login bug", "workspace": "/repo", "conversationId": "mine"}))
    entry = {"kind": "agy", "repo": "/repo", "cwd": "/repo"}
    assert mesh.find_session("mold", "repo-planner", entry) is None
    assert mesh.find_session("mold", "repo-planner", {**entry, "cwd": "/wt/x", "session": "s"}) == "s"


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
    assert start[-5:] == ("--conversation", "c1", "--model", "gemini-3.8-flash-high", "--dangerously-skip-permissions")
    data = mesh.load_run(run)
    entry = data["agents"]["proj-fix"]
    assert (entry["parent"], entry["session"], entry["status"]) == ("repo-lead", "c1", "running")
    assert {"path": str(path), "repo": str(repo), "branch": branch} in data["worktrees"]  # this run's stop cleans it
    assert "MESH_SELF=proj-fix" in fake.called("tab", "create")[0]
    doorbell = fake.called("agent", "prompt")[-1][3]
    assert doorbell.startswith("📬 [mesh] You are proj-fix (worker) again, resumed into agent-mesh run mtest")
    assert mesh.unread(run, "proj-fix")


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
    assert fake.called("agent", "start")[-1][-7:] == (
        "--resume", "s-1", "--model", "opus", "--effort", "high", "--dangerously-skip-permissions")


def test_resume_refuses_a_worker_that_is_still_running(mesh, fake, run, repo, capsys):
    path, _ = archived_worker(mesh, repo, session="c1")
    fake.on("agent", "list", result={"agents": [{"pane_id": "w5:p2", "cwd": str(path / "src")}]})
    assert mesh.main(["resume", "proj-fix", "--from-run", "mold"]) == 1
    assert "still running in pane w5:p2" in capsys.readouterr().err
    assert not fake.called("agent", "start") and not path.exists()


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
    assert doorbell.startswith("📬 [mesh] You are repo-fix-typo (worker) again in agent-mesh run mtest, "
                               "now on gemini-3.8-flash-medium at medium effort")
    [msg] = mesh.unread(run, "repo-fix-typo")
    assert msg.name.endswith("-answer.md") and "approved\n\nMind the migration." in msg.read_text()

    # a later approval (after a revision) doesn't switch again
    assert mesh.main(["approve", "repo-fix-typo"]) == 0
    assert len(fake.called("agent", "start")) == 1


def test_approve_a_claude_worker_resumes_it_on_opus_at_medium(mesh, fake, run, capsys):
    planning_worker(mesh, run, kind="claude", model="opus", session="s-1")
    stub_new_pane(fake)
    assert mesh.main(["approve", "repo-fix-typo"]) == 0
    assert fake.called("agent", "start")[-1][-7:] == (
        "--resume", "s-1", "--model", "opus", "--effort", "medium", "--dangerously-skip-permissions")


def test_approve_with_the_same_model_and_effort_keeps_the_pane(mesh, fake, run, capsys):
    planning_worker(mesh, run, kind="claude", model="opus", session="s-1")
    with mesh.editing_run(run) as data:  # `spawn --kind claude --effort high`: both phases match
        data["agents"]["repo-fix-typo"]["exec_effort"] = "high"
    assert mesh.main(["approve", "repo-fix-typo"]) == 0
    assert not fake.called("agent", "start") and not fake.called("pane", "close")
    assert mesh.load_run(run)["agents"]["repo-fix-typo"]["phase"] == "exec"
    assert fake.called("agent", "prompt")[-1][3].startswith("📬 [mesh] answer from repo-lead: approved")


def test_approve_without_an_exec_model_is_a_plain_answer(mesh, fake, run, capsys):
    assert mesh.main(["approve", "repo-fix-typo"]) == 0
    assert not fake.called("agent", "start") and not fake.called("pane", "close")
    assert fake.called("agent", "prompt")[-1][3].startswith("📬 [mesh] answer from repo-lead: approved")


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


def test_watch_stops_excusing_a_worker_that_never_comes_back(mesh, fake, run, capsys):
    planning_worker(mesh, run)
    with mesh.editing_run(run) as data:
        data["agents"]["repo-fix-typo"]["status"] = "starting"
    fake.on("agent", "get", errors=["agent_not_found"] * 10)
    assert mesh.main(["watch", "repo-fix-typo", "--run-id", run, "--interval", "100"]) == 5
    assert len(fake.called("agent", "get")) == 4  # 3 polls excused (300s), the 4th alerts


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


def test_the_new_model_doorbell_survives_a_failed_ring(mesh, fake, run, monkeypatch, capsys):
    planning_worker(mesh, run)
    with_quotas(mesh, monkeypatch)
    stub_new_pane(fake)
    fake.on("agent", "prompt", errors=["agent_blocked"] * 7)  # every attempt of the first ring
    assert mesh.main(["approve", "repo-fix-typo"]) == 2
    assert mesh.load_run(run)["agents"]["repo-fix-typo"]["swapped"] is True
    assert mesh.main(["send", "repo-fix-typo", "--type", "info", "--body", "ping"]) == 0
    assert fake.called("agent", "prompt")[-1][3].startswith(
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
    assert fake.called("agent", "prompt")[-1][3].startswith("📬 [mesh] task from repo-lead: Question (read-only)")


def test_ask_a_past_worker_resumes_its_conversation(mesh, fake, run, repo, monkeypatch, capsys):
    archived_worker(mesh, repo, session="c1")
    stub_new_pane(fake)
    fake.on("agent", "get", result={"agent": {"agent_status": "working"}})
    answer_on_sleep(mesh, monkeypatch, run, "proj-fix", body="Because the API spec says so.")
    assert mesh.main(["ask", "https://github.com/acme/proj/pull/7", "--body", "Why ISO dates?"]) == 0
    assert "Because the API spec says so." in capsys.readouterr().out
    assert fake.called("agent", "start")[-1][-5:-2] == ("--conversation", "c1", "--model")
    assert mesh.load_run(run)["agents"]["proj-fix"]["resumed_from"] == {"run": "mold", "name": "proj-fix"}


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
    assert out["name"] == "proj-review-7"
    wt_head = subprocess.run(["git", "rev-parse", "HEAD"], cwd=out["cwd"], capture_output=True, text=True).stdout
    assert wt_head.strip() == head
    entry = mesh.load_run(run)["agents"]["proj-review-7"]
    assert (entry["reviews"], entry["base"]) == ([pr], head)
    [task] = mesh.unread(run, "proj-review-7")
    body = mesh.parse_message(task)[1]
    assert body.startswith(f"Review (read-only) of {pr}:") and "checked out in your worktree" in body


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


def test_a_live_worker_reviews_without_switching_its_branch(mesh, fake, run, monkeypatch, capsys):
    fake.on("agent", "get", result={"agent": {"agent_status": "idle"}})
    pr = "https://github.com/acme/other/pull/3"
    assert mesh.main(["ask", "repo-fix-typo", "--review", pr, "--body", "Focus on auth.", "--no-wait"]) == 0
    body = mesh.parse_message(mesh.unread(run, "repo-fix-typo")[0])[1]
    assert "Don't switch your branch" in body and "pull/3/head" in body and body.rstrip().endswith("Focus on auth.")
    assert mesh.load_run(run)["agents"]["repo-fix-typo"]["reviews"] == [pr]


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


def test_ask_reports_a_worker_that_stops_without_answering(mesh, fake, run, monkeypatch, capsys):
    states = iter(["idle", "working", "idle"])
    monkeypatch.setattr(mesh, "agent_status", lambda name, *_: next(states))
    assert mesh.main(["ask", "repo-fix-typo", "--body", "Why?"]) == 3
    assert "without sending a message" in capsys.readouterr().out


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
    assert "only the root asks" in capsys.readouterr().err
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
    assert (out["view"], out["model"], out["effort"], out["exec_model"]) == ("bg", "sonnet", "medium", None)
    launch = cli.called("--bg")[0]
    argv = launch["argv"]
    assert argv[:9] == ["claude", "--bg", "-n", "proj-copy", "--model", "sonnet", "--effort", "medium",
                        "--dangerously-skip-permissions"]
    assert argv[9].startswith("📬 [mesh] You are proj-copy (worker) in agent-mesh run mtest")
    assert launch["cwd"] == mesh.worktree_root() / "mtest" / "proj-copy"
    assert launch["env"]["MESH_SELF"] == "proj-copy" and "HERDR_PANE_ID" not in launch["env"]
    entry = mesh.load_run(run)["agents"]["proj-copy"]
    assert (entry["bg_id"], entry["session"], entry["phase"], entry["status"]) == ("ab12cd34", "ab12cd34-s", None, "running")
    assert not fake.called("agent", "start")
    task = mesh.parse_message(mesh.unread(run, "proj-copy")[0])[1]
    assert task.startswith("Direct task (no plan)") and task.rstrip().endswith("Fix the typo in the footer.")
    assert out["delivery"]["launched"] is True


def test_background_falls_back_to_a_pane_without_a_claude_root(mesh, fake, cli, run, repo, capsys):
    stub_new_pane(fake)
    assert mesh.main(["spawn", "--kind", "claude", "--size", "trivial", "--repo", str(repo), "--worktree", "cfg"]) == 0
    out = json.loads(capsys.readouterr().out)
    assert out["view"] == "pane" and "starting a pane" in out["notes"][0]
    assert fake.called("agent", "start")[-1][-5:] == ("--model", "sonnet", "--effort", "low",
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


def test_small_and_trivial_agy_tasks_are_flash_at_high_and_low_in_a_pane(mesh, fake, run, repo, monkeypatch, capsys):
    stub_new_pane(fake)
    monkeypatch.setattr(mesh, "resolve_agy_model", lambda family, effort: f"gemini-3.8-{family}-{effort}")
    assert mesh.main(["spawn", "--size", "small", "--repo", str(repo), "--worktree", "fix"]) == 0
    out = json.loads(capsys.readouterr().out)
    assert (out["view"], out["model"], out["exec_model"]) == ("pane", "gemini-3.8-flash-high", None)
    assert mesh.main(["spawn", "--size", "trivial", "--repo", str(repo), "--worktree", "bump"]) == 0
    assert json.loads(capsys.readouterr().out)["model"] == "gemini-3.8-flash-low"
    assert mesh.main(["spawn", "--size", "small", "--view", "bg", "--repo", str(repo), "--worktree", "no"]) == 1
    assert "for Claude workers" in capsys.readouterr().err


def test_a_size_has_no_execution_phase_to_override(mesh, fake, run, repo, capsys):
    assert mesh.main(["spawn", "--kind", "claude", "--size", "small", "--exec-model", "opus", "--repo", str(repo)]) == 1
    assert "no execution phase" in capsys.readouterr().err


def test_user_reviewed_plan_runs_opusplan_in_plan_mode(mesh, fake, run, repo, capsys):
    stub_new_pane(fake)
    assert mesh.main(["spawn", "--kind", "claude", "--plan-review", "user", "--repo", str(repo), "--worktree", "feat",
                      "--task", "Build it."]) == 0
    start = fake.called("agent", "start")[-1]
    assert "opusplan" in start and start[-3:] == ("--permission-mode", "plan", "--allow-dangerously-skip-permissions")
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


def test_when_auto_compaction_compacts(mesh, monkeypatch):
    claude, agy = {"kind": "claude"}, {"kind": "agy"}
    assert mesh.should_compact("auto", claude, 150_000, False, True)
    assert not mesh.should_compact("auto", claude, 150_000, True, True)  # a question doesn't repay it
    assert mesh.should_compact("auto", claude, 250_000, True, False)
    assert not mesh.should_compact("auto", claude, None, False, True)
    assert mesh.should_compact("yes", claude, 10, False, False) and not mesh.should_compact("no", claude, 10**6, False, True)
    assert mesh.should_compact("auto", agy, None, False, True)
    assert not mesh.should_compact("auto", agy, None, False, False) and not mesh.should_compact("auto", agy, None, True, True)
    monkeypatch.setenv("MESH_COMPACT_AT", "1000")
    assert mesh.should_compact("auto", claude, 1500, False, False)


def test_resume_compacts_a_large_conversation_before_reopening_it(mesh, fake, cli, run, repo, monkeypatch,
                                                                  tmp_path, capsys):
    path, _ = archived_worker(mesh, repo, kind="claude", session="s-1")
    session_file(mesh, monkeypatch, tmp_path, str(path), "s-1", [usage(150_000)])
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
    assert out["compaction"]["compacted"] is True and out["compaction"]["context_tokens"] == 150_012


def test_a_failed_compaction_still_resumes(mesh, fake, cli, run, repo, monkeypatch, tmp_path, capsys):
    path, _ = archived_worker(mesh, repo, kind="claude", session="s-1")
    session_file(mesh, monkeypatch, tmp_path, str(path), "s-1", [usage(150_000)])
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
    assert launch["argv"][1:4] == ["--bg", "--resume", "s-1"] and launch["env"]["MESH_RUN"] == "mtest"
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
    assert all(r["files"] == ["a.txt"] for r in rows)
    assert mesh.main(["conflicts"]) == 0
    assert "mesh/mtest/b2" in capsys.readouterr().out


def test_a_bg_swap_is_marked_starting_before_its_session_stops(mesh, fake, cli, run, capsys):
    claude_root(mesh, run)
    bg_worker(mesh, run, phase="plan", exec_model="opus", exec_effort="medium")
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
    session_file(mesh, monkeypatch, tmp_path, str(path), "s-1", [usage(150_000)])
    stub_new_pane(fake)
    monkeypatch.setenv("HERDR_PANE_ID", "w1:p1")  # the root's pane, which by-pane maps to the root
    capsys.readouterr()
    assert mesh.main(["resume", "proj-fix", "--task", "Go."]) == 0
    env = cli.called("-p")[0]["env"]
    assert "HERDR_PANE_ID" not in env and not any(k in env for k in mesh.MESH_IDS)
    assert env["MESH_HOME"] == str(mesh.mesh_home())  # configuration stays
    capsys.readouterr()


def test_model_auto_on_a_small_task_means_its_default(mesh, fake, cli, run, repo, capsys):
    claude_root(mesh, run)
    bg_cli(cli)
    assert mesh.main(["spawn", "--kind", "claude", "--size", "small", "--model", "auto", "--repo", str(repo),
                      "--worktree", "copy"]) == 0
    assert json.loads(capsys.readouterr().out)["model"] == "sonnet"
    assert cli.called("--bg")[0]["argv"][4:6] == ["--model", "sonnet"]


def test_watch_any_reads_claude_agents_once_per_poll(mesh, fake, cli, run, capsys):
    bg_worker(mesh, run, session="s1", bg_id="b1")
    with mesh.editing_run(run) as data:
        data["agents"]["repo-two"] = {**data["agents"]["repo-fix-typo"], "session": "s2", "bg_id": "b2"}
    rows = [{"id": "b1", "sessionId": "s1", "status": "busy"}, {"id": "b2", "sessionId": "s2", "status": "busy"}]

    def agents_call(argv):
        if len(cli.called("agents")) == 3:
            mesh.log_event(run, "send", sender="repo-two", to="repo-lead", type="done", file="x.md")

    cli.on("agents", stdout=lambda: json.dumps(rows), effect=agents_call)
    assert mesh.main(["watch", "--any"]) == 0
    assert "repo-two reported done" in capsys.readouterr().out
    assert len(cli.called("agents")) == 3  # one to list the live workers, then one per poll
