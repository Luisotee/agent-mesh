"""The per-CLI adapters (`Cli`, `CLIS`): each one's outputs pinned to what mesh did before they existed, so moving
code into them changes no behaviour (design §22)."""

import pytest


def test_registry_keeps_the_kind_tables(mesh):
    assert mesh.KINDS == mesh.ASK_KINDS == ("claude", "agy", "copilot", "opencode")  # every CLI a worker
    assert mesh.AGENT_BINARIES == ("claude", "agy", "copilot", "opencode", "opencode.exe")
    assert mesh.CLI_NAMES == {"claude": "Claude Code", "agy": "agy", "copilot": "Copilot", "opencode": "OpenCode"}
    assert mesh.SHORT_NAMES == {"claude": "Claude", "agy": "agy", "copilot": "Copilot", "opencode": "OpenCode"}
    assert all(c.kind == kind for kind, c in mesh.CLIS.items())
    assert set(mesh.MODES) <= set(mesh.KINDS)  # a root's CLI is a worker's too


def test_cli_defaults_to_claude_for_an_entry_without_a_kind(mesh):
    assert mesh.cli(None) is mesh.cli("") is mesh.CLIS["claude"]
    assert mesh.cli("agy") is mesh.CLIS["agy"]
    assert mesh.cli_has(None, "stop_hook")  # the same default as cli()
    assert mesh.cli_has("claude", "stop_hook") and not mesh.cli_has("agy", "stop_hook")


def test_an_unknown_kind_is_an_error_not_claude(mesh, run):
    """A run a newer mesh wrote (a kind this checkout doesn't know) must not get Claude's argv."""
    with pytest.raises(mesh.MeshError, match="unknown agent kind 'zed'"):
        mesh.cli("zed")
    with pytest.raises(mesh.MeshError, match="unknown agent kind"):
        mesh.resume_argv("zed", "ses_1", "zed-1", "high")
    assert not mesh.cli_has("zed", "stop_hook")  # a reading: no such capability here


def test_capabilities_per_kind(mesh):
    flags = ("stop_hook", "native_inbox", "enforces_read_only", "plan_review_user", "code_review",
             "worker_log", "records_session_on_send", "measures_cache", "compacts_unmeasured", "counts_cost")
    have = {kind: {f for f in flags if getattr(c, f)} for kind, c in mesh.CLIS.items()}
    assert have == {
        "claude": {"stop_hook", "native_inbox", "enforces_read_only", "plan_review_user", "code_review",
                   "measures_cache", "counts_cost"},
        "agy": {"worker_log", "records_session_on_send", "compacts_unmeasured", "counts_cost"},
        "copilot": {"enforces_read_only"},
        "opencode": {"enforces_read_only", "records_session_on_send", "counts_cost"},
    }
    assert {kind: c.views for kind, c in mesh.CLIS.items()} == {"claude": ("pane", "bg"), "agy": ("pane",),
                                                                "copilot": ("pane",), "opencode": ("pane",)}


def test_binaries(mesh, monkeypatch):
    monkeypatch.setenv("MESH_CLAUDE", "/opt/claude")
    assert [mesh.cli_binary(k) for k in mesh.ASK_KINDS] == ["/opt/claude", "agy", "copilot", "opencode"]


def test_launch_argv(mesh, monkeypatch):
    monkeypatch.setattr(mesh, "resolve_agy_model", lambda family, effort: f"gemini-4-{family}-{effort}")
    argv, model, session = mesh.agent_argv("claude", "opus", "medium")
    assert argv == ["--session-id", session, "--model", "opus", "--effort", "medium",
                    "--dangerously-skip-permissions"] and model == "opus" and len(session) == 36
    assert mesh.agent_argv("agy", "hydrogen", "low", "/tmp/w.log") == (
        ["--log-file", "/tmp/w.log", "--model", "gemini-4-hydrogen-low", "--dangerously-skip-permissions"],
        "gemini-4-hydrogen-low", None)
    assert mesh.resolve_model("claude", "opus", "high") == "opus"
    assert mesh.resolve_model("agy", "argon", "high") == "gemini-4-argon-high"


def test_resume_argv(mesh):
    assert mesh.resume_argv("claude", "s1", "opus", "medium") == ["--resume", "s1", "--model", "opus", "--effort",
                                                                  "medium"]
    assert mesh.resume_argv("agy", "c1", "gemini-4-hydrogen-medium", "medium") == [
        "--conversation", "c1", "--model", "gemini-4-hydrogen-medium"]
    assert mesh.resume_argv("copilot", "u1", "auto", "high") == ["--session-id", "u1", "--model", "auto",
                                                                 "--auto-tier", "intelligence"]
    assert mesh.resume_argv("copilot", "u1", "gpt-5", "high") == ["--session-id", "u1", "--model", "gpt-5",
                                                                  "--reasoning-effort", "high"]


def test_deny_and_settings_argv(mesh, tmp_path):
    entry = {"kind": "claude", "read_only": True, "cwd": str(tmp_path / "wt"), "repo": str(tmp_path / "repo")}
    deny = mesh.deny_argv(entry)
    assert deny[0] == "--disallowedTools" and f"Edit(/{tmp_path / 'wt'}/**)" in deny and "Bash(git push:*)" in deny
    assert mesh.deny_argv({**entry, "read_only": False}) == []
    assert mesh.deny_argv({**entry, "kind": "agy"}) == []
    env = {"MESH_RUN": "r1", "MESH_SELF": "w", "MESH_PARENT": "root", "MESH_ROLE": "worker"}
    assert mesh.settings_argv("claude", env)[0] == "--settings"
    assert mesh.settings_argv("agy", env) == []


def test_resumed_model(mesh, monkeypatch):
    monkeypatch.setattr(mesh, "resolve_agy_model", lambda family, effort: f"gemini-4-{family}-{effort}")
    assert mesh.resumed_model({"kind": "claude", "model": "opus", "effort": "medium"}, None, None) == ("opus", "medium")
    assert mesh.resumed_model({"kind": "copilot", "requested_model": "gpt-5"}, None, "low") == ("gpt-5", "low")
    assert mesh.resumed_model({"kind": "copilot", "tier": "mid"}, None, "low") == ("auto", "low")  # its tier's
    with pytest.raises(mesh.MeshError, match="no tier or model on record"):  # never a default (design §9)
        mesh.resumed_model({"kind": "copilot"}, None, "low")
    exact = {"kind": "agy", "model": "gemini-4-hydrogen-medium", "effort": "medium"}
    assert mesh.resumed_model(exact, None, None) == ("gemini-4-hydrogen-medium", "medium")
    assert mesh.resumed_model(exact, None, "high") == ("gemini-4-hydrogen-high", "high")


def test_find_session(mesh, monkeypatch, tmp_path):
    asked = []
    monkeypatch.setattr(mesh, "claude_session", lambda name, cwd: asked.append(("claude", cwd)) or "s-claude")
    monkeypatch.setattr(mesh, "agy_conversation",
                        lambda name, run_id, cwd, pinned_only=False: asked.append(("agy", cwd)) or "c-agy")
    assert mesh.find_session("r1", "w", {"kind": "claude", "session": "rec"}) == "rec"
    assert mesh.find_session("r1", "w", {"kind": "claude", "cwd": "/wt"}) == "s-claude"
    assert mesh.find_session("r1", "w", {"kind": "agy", "cwd": "/wt", "repo": "/repo"}) == "c-agy"
    # A main checkout is shared with the user's own sessions: only the doorbell counts there.
    assert mesh.find_session("r1", "w", {"kind": "agy", "cwd": str(tmp_path), "repo": str(tmp_path)}) == "c-agy"
    assert asked == [("claude", "/wt"), ("agy", "/wt"), ("agy", None)]
    assert mesh.find_session("r1", "w", {"kind": "copilot"}) is None


def test_compaction_leaves(mesh, monkeypatch):
    monkeypatch.setenv("MESH_COMPACT_AT", "111")
    monkeypatch.setenv("MESH_AGY_COMPACT_AT", "222")
    assert [mesh.compact_at(k) for k in (None, "claude", "agy", "copilot")] == [111, 111, 222, 111]


def test_switch_model(mesh, monkeypatch):
    with pytest.raises(mesh.MeshError, match="pass --model for a claude worker"):
        mesh.cli("claude").switch_model({"kind": "claude", "model": "opus"}, "high")

    def spent(*a, **k):
        raise mesh.MeshError("no model over the reserve")
    monkeypatch.setattr(mesh, "choose_agy_model", spent)
    monkeypatch.setattr(mesh, "agy_at_reserve", lambda model, effort=None: (0, "2026-11-20T00:00:00+00:00"))
    with pytest.raises(mesh.NoModelLeft) as caught:
        mesh.cli("agy").switch_model({"kind": "agy", "model": "gemini-4-argon-high", "phase": "plan"}, "high")
    assert caught.value.reset == mesh.epoch("2026-11-20T00:00:00+00:00")
    assert mesh.cli("agy").effort_of("gemini-4-hydrogen-low") == "low"
    assert mesh.cli("claude").effort_of("opus") is None


def test_reserve_and_reviewers(mesh):
    assert mesh.reserve_hit("copilot", "auto") is None
    assert mesh.reserve_hit(None, "opus") is None
    assert mesh.reviewer_available("claude", "small") == (True, "")
    assert mesh.reviewer_available("copilot", "small") == (True, "")


def test_agent_binary_names_the_kind(mesh):
    assert mesh.agent_binary(["node", "/usr/lib/claude", "--resume", "x"]) == "claude"
    assert mesh.agent_binary(["/home/u/.local/bin/agy", "--conversation", "c"]) == "agy"
    # Copilot's npm loader runs under node, and starts its native binary, also named `copilot`.
    assert mesh.agent_binary(["node", "/usr/lib/node_modules/.bin/copilot", "--session-id", "u"]) == "copilot"
    assert mesh.agent_binary(["/x/@github/copilot-linux-x64/copilot", "--session-id", "u"]) == "copilot"
    assert mesh.agent_binary(["bash"]) is None


def test_a_root_recorded_with_herdrs_own_kind_still_gets_its_reports(mesh, fake, run):
    """A root started from a pane herdr names another agent in (codex, …) has that kind recorded: reports to it
    are rung into its pane, as before, rather than refused as an unknown kind."""
    with mesh.editing_run(run) as data:
        data["agents"]["repo-lead"]["kind"] = "codex"
    assert not mesh.cli_has("codex", "native_inbox")  # the readings: False, as for any CLI without it
    with pytest.raises(mesh.MeshError, match="unknown agent kind 'codex'"):
        mesh.cli("codex")  # only argv building refuses it
    result = mesh.deliver(run, "repo-fix-typo", "repo-lead", "done", "Fixed.")
    assert result["stored"] and fake.called("agent", "prompt")
    assert mesh.find_session(run, "repo-lead", {"kind": "codex"}) is None


def test_ask_waits_for_an_agy_worker_even_when_the_root_takes_frames(mesh, fake, run, monkeypatch, capsys):
    """native_route to the root doesn't look at the sender: only a worker that itself answers by the inbox
    socket lets `ask` return at once; an agy worker's ask is watched (so its quota alerts are seen)."""
    fake.on("agent", "get", result={"agent": {"agent_status": "idle"}})
    monkeypatch.setattr(mesh, "native_route",
                        lambda run, sender, to, *a, **k: {"socket": "uds:/x"} if to == "repo-lead" else None)
    watched = []

    def watch(run_id, name, *a, **k):
        watched.append(name)
        mesh.record(run_id, name, "repo-lead", "done", "Because of the spec.")
        return 0, f"{name} reported done"

    monkeypatch.setattr(mesh, "watch_agent", watch)
    assert mesh.main(["ask", "repo-fix-typo", "--body", "Why?", "--keep"]) == 0
    assert watched == ["repo-fix-typo"]
    assert "── done from repo-fix-typo ──\nBecause of the spec." in capsys.readouterr().out


def test_the_readings_have_nothing_for_an_unknown_kind_and_the_actions_name_it(mesh, run):
    entry = {"kind": "zed", "session": "ses_1", "cwd": "/wt"}
    assert mesh.progress_files(entry, "ses_1") == [] and mesh.context_tokens(entry, "ses_1") is None
    assert mesh.still_limited(entry) is False
    with mesh.editing_run(run) as data:
        data["agents"]["repo-new"] = {"role": "worker", "kind": "zed", "parent": "repo-lead", "status": "running"}
    with pytest.raises(mesh.MeshError, match="unknown agent kind 'zed'"):
        mesh.swap_model(run, "repo-new", "x", "high", "test")
