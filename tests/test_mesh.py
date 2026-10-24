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
gemini-3.10-flash-medium\tGemini 3.10 Flash (Medium)
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
    assert mesh.pick_agy_model(AGY_MODELS, "flash", "medium") == "gemini-3.10-flash-medium"
    assert mesh.pick_agy_model(AGY_MODELS, "flash", "high") == "gemini-3.8-flash-high"
    assert mesh.pick_agy_model(AGY_MODELS, "argon", "high") == "gemini-4-argon-high"
    assert mesh.pick_agy_model(AGY_MODELS, "gemini-3.8-flash-high", "low") == "gemini-3.8-flash-high"
    with pytest.raises(mesh.MeshError):
        mesh.pick_agy_model(AGY_MODELS, "flash", "low")


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


def test_watch_plan_then_idle_waits_for_review(mesh, fake, run, monkeypatch):
    states = iter(["working", "idle", "idle", "gone"])

    def status(name):
        state = next(states)
        if state == "idle" and not mesh.sends_from(run, name):
            mesh.deliver(run, name, "repo-lead", "plan", "1. Patch reactions.py")
        return state

    monkeypatch.setattr(mesh, "agent_status", status)
    # a plan awaiting review is neither a stall nor a final report
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
    assert (entry["phase"], entry["exec_model"]) == ("plan", "flash")
    assert entry["log"].endswith("/mtest/logs/proj-fix-reactions.log")
    assert fake.called("agent", "start")[-1][-5:] == (
        "--log-file", entry["log"], "--model", "claude-opus-5-5-high", "--dangerously-skip-permissions")
    assert "MESH_ROLE=worker" in fake.called("tab", "create")[0]

    fake.on("pane", "layout", result={"layout": {"panes": [{"pane_id": "w9:p10", "rect": {"width": 200, "height": 50}}]}})
    fake.on("pane", "split", result={"pane": {"pane_id": "w9:p11"}})
    assert mesh.main(["spawn", "--repo", str(repo), "--name", "one", "--model", "flash"]) == 0
    entry = mesh.load_run(run)["agents"]["proj-one"]
    assert (entry["model"], entry["exec_model"], entry["phase"]) == ("gemini-3.8-flash-high", None, None)


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
            workspace="w9", phase=phase, exec_model="flash" if kind == "agy" else "sonnet",
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
    assert out["model"] == "gemini-3.8-flash-high"
    assert fake.called("pane", "close", "w2:p5")  # the planning pane goes
    assert fake.called("agent", "start")[-1][-7:] == (
        "--log-file", str(log), "--conversation", "c1", "--model", "gemini-3.8-flash-high",
        "--dangerously-skip-permissions")
    entry = mesh.load_run(run)["agents"]["repo-fix-typo"]
    assert (entry["phase"], entry["pane"], entry["status"]) == ("exec", "w9:p10", "running")
    assert "swapped" not in entry and entry["models"][-1]["why"] == "plan approved"
    doorbell = fake.called("agent", "prompt")[-1][3]
    assert doorbell.startswith("📬 [mesh] You are repo-fix-typo (worker) again in agent-mesh run mtest, "
                               "now on gemini-3.8-flash-high")
    [msg] = mesh.unread(run, "repo-fix-typo")
    assert msg.name.endswith("-answer.md") and "approved\n\nMind the migration." in msg.read_text()

    # a later approval (after a revision) doesn't switch again
    assert mesh.main(["approve", "repo-fix-typo"]) == 0
    assert len(fake.called("agent", "start")) == 1


def test_approve_a_claude_worker_resumes_it_on_sonnet(mesh, fake, run, capsys):
    planning_worker(mesh, run, kind="claude", model="opus", session="s-1")
    stub_new_pane(fake)
    assert mesh.main(["approve", "repo-fix-typo"]) == 0
    assert fake.called("agent", "start")[-1][-7:] == (
        "--resume", "s-1", "--model", "sonnet", "--effort", "high", "--dangerously-skip-permissions")


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
    assert "was not reopened on gemini-3.8-flash-high" in capsys.readouterr().err
    entry = mesh.load_run(run)["agents"]["repo-fix-typo"]
    # the recorded model is still the planning one, so approving again retries the swap
    assert (entry["status"], entry["model"], entry["phase"]) == ("swap_failed", "claude-opus-5-5-high", "plan")
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
        "📬 [mesh] You are repo-fix-typo (worker) again in agent-mesh run mtest, now on gemini-3.8-flash-high")
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
