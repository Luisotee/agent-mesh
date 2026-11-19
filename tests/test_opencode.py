"""OpenCode as a worker kind (design §22): models and the `free` family, its pane config, its session db, its
limits and `mesh switch`'s rotation, its input box. Shapes are OpenCode 1.19.6's, seen in a scratch pane."""

import json
import os
import sqlite3
import time
from pathlib import Path

import pytest

from test_mesh import repo, started, stub_new_pane  # noqa: F401 (repo is a fixture)

# `opencode models --verbose`: each model's line, then its JSON object (trimmed).
LISTING = """opencode/beam-free
{
  "id": "beam-free", "providerID": "opencode", "status": "active",
  "cost": {"input": 0, "output": 0, "cache": {"read": 0, "write": 0}},
  "capabilities": {"toolcall": true}, "release_date": "2026-10-05",
  "variants": {"low": {"reasoningEffort": "low"}, "high": {"reasoningEffort": "high"}}
}
opencode/big-pickle
{
  "id": "big-pickle", "providerID": "opencode", "status": "active",
  "cost": {"input": 0, "output": 0}, "capabilities": {"toolcall": true}, "release_date": "2026-11-01"
}
opencode/gpt-paid
{
  "id": "gpt-paid", "providerID": "opencode", "cost": {"input": 1.25, "output": 10}, "release_date": "2026-11-10"
}
anthropic/claude-x
{
  "id": "claude-x", "providerID": "anthropic", "cost": {"input": 0, "output": 0}, "release_date": "2026-11-12"
}
"""


def seed_listing(mesh, text=LISTING):
    models = mesh.parse_opencode_models(text)
    mesh.write_atomic(mesh.mesh_home() / "opencode-models.json", json.dumps(models))
    return models


def opencode_db(mesh, sessions=(), parts=(), messages=()):
    """An OpenCode db with these rows: session (id, directory, time_updated ms), part (session, data, time),
    message (session, data, time)."""
    path = Path(os.environ["MESH_OPENCODE_DB"])
    path.parent.mkdir(parents=True, exist_ok=True)
    db = sqlite3.connect(path)
    db.executescript("create table if not exists session (id text, directory text, parent_id text, time_created int,"
                     " time_updated int); create table if not exists part (session_id text, data text, "
                     "time_created int); create table if not exists message (session_id text, data text, "
                     "time_created int);")
    for n, (sid, directory, updated) in enumerate(sessions):
        db.execute("insert into session values (?, ?, null, ?, ?)", (sid, directory, n, updated))
    for sid, data, at in parts:
        db.execute("insert into part values (?, ?, ?)", (sid, json.dumps(data), at))
    for sid, data, at in messages:
        db.execute("insert into message values (?, ?, ?)", (sid, json.dumps(data), at))
    db.commit()
    db.close()


def test_the_free_family_is_opencodes_own_costless_models_newest_first(mesh):
    models = seed_listing(mesh)
    assert set(models) == {"opencode/beam-free", "opencode/big-pickle", "opencode/gpt-paid", "anthropic/claude-x"}
    assert mesh.opencode_free(models) == ["opencode/big-pickle", "opencode/beam-free"]  # not a paid or other one
    assert mesh.opencode_pick(["free"]) == ("opencode/big-pickle", [])
    assert mesh.opencode_pick(["anthropic/claude-y", "free"]) == ("anthropic/claude-y", [])  # a provider's, as given
    assert mesh.opencode_variants("opencode/beam-free") == {"low", "high"}
    assert mesh.cli("opencode").plan_model("high", None) == ("opencode/big-pickle", [])


def test_an_opencode_worker_starts_in_a_pane_with_its_own_agent_config(mesh, fake, run, repo):
    seed_listing(mesh)
    stub_new_pane(fake)
    assert mesh.main(["spawn", "--kind", "opencode", "--repo", str(repo), "--worktree", "feat",
                      "--model", "opencode/beam-free", "--effort", "high", "--task", "Add a dark mode."]) == 0
    entry = mesh.load_run(run)["agents"]["proj-feat"]
    agent = mesh.opencode_agent("opencode/beam-free", "high")
    assert started(fake)[-5:] == ("--", "--agent", agent, "-m", "opencode/beam-free", "--auto")[-5:]
    assert (entry["kind"], entry["session"], entry["planning"]) == ("opencode", None, True)
    tab = " ".join(fake.called("tab", "create")[0])
    config = json.loads(tab.split("OPENCODE_CONFIG_CONTENT=")[1].split(" --env")[0])
    assert config == {"agent": {agent: {"mode": "primary", "model": "opencode/beam-free", "variant": "high"}}}
    assert "OPENCODE_DISABLE_AUTOUPDATE=1" in tab and "OPENCODE_PERMISSION=" in tab  # planning: under the deny rules


def test_read_only_rules_deny_edits_in_the_worktree_and_main_checkout_and_publishing(mesh, tmp_path):
    rules = mesh.opencode_permission({"cwd": str(tmp_path / "wt" / "x"), "repo": str(tmp_path / "proj")})
    assert rules["edit"] == {"*": "deny", "../*": "allow", "../../proj/*": "deny"}  # the last match wins
    assert rules["bash"]["git push*"] == "deny" and rules["bash"]["gh pr comment*"] == "deny"
    env = mesh.cli("opencode").launch_env({"model": "opencode/big-pickle", "effort": "medium", "read_only": False})
    assert "OPENCODE_PERMISSION" not in env  # a change task: no deny rules
    assert "variant" not in json.loads(env["OPENCODE_CONFIG_CONTENT"])["agent"][
        mesh.opencode_agent("opencode/big-pickle", "medium")]  # no such variant listed for it


def test_a_reopen_on_another_model_gets_another_agent_name(mesh):
    """The TUI reopening a session takes its last message's model while that message's agent is configured, so
    each (model, effort) has its own agent: a swap's `-m` then holds (checked live on 1.19.6)."""
    oc = mesh.cli("opencode")
    a = oc.resume_argv("ses_1", "opencode/big-pickle", "high")
    b = oc.resume_argv("ses_1", "opencode/hy4-flash-free", "high")
    assert a[-2:] == b[-2:] == ["-s", "ses_1"] and a[1] != b[1] and a[1].startswith("mesh-")
    assert oc.resume_argv("ses_1", "opencode/big-pickle", "high") == a  # the same launch, the same name


def test_an_opencode_session_is_found_by_its_first_doorbell_then_its_folder(mesh, tmp_path):
    wt = tmp_path / "wt"
    wt.mkdir()
    doorbell = "📬 [mesh] You are proj-feat (worker) in agent-mesh run mtest (ref ab12); read the protocol"
    opencode_db(mesh, sessions=[("ses_old", str(wt), 1), ("ses_mine", "/elsewhere", 2), ("ses_new", str(wt), 3)],
                parts=[("ses_mine", {"type": "text", "text": doorbell}, 10),
                       ("ses_new", {"type": "text", "text": "📬 [mesh] You are proj-feat (worker) in agent-mesh run "
                                                            "other (ref x)"}, 11)])
    entry = {"kind": "opencode", "cwd": str(wt), "repo": str(tmp_path / "proj")}
    assert mesh.find_session("mtest", "proj-feat", entry) == "ses_mine"  # pinned by its doorbell and run
    assert mesh.cli("opencode").lookup_session("mtest", "proj-feat", entry, pinned_only=True) == "ses_mine"
    assert mesh.find_session("mzzz", "proj-feat", entry) == "ses_new"  # else the newest in its folder
    assert mesh.find_session("mzzz", "proj-feat", {**entry, "repo": str(wt)}) is None  # not in a shared checkout


def test_progress_context_and_cost_come_from_the_session_rows(mesh, tmp_path):
    opencode_db(mesh, sessions=[("ses_1", str(tmp_path), 1795000000000)], messages=[
        ("ses_1", {"role": "user", "time": {"created": 1795000000000}}, 1),
        ("ses_1", {"role": "assistant", "id": "msg_1", "time": {"created": 1795000001000},
                   "tokens": {"input": 1200, "output": 40, "reasoning": 10, "cache": {"read": 30000, "write": 0}}}, 2)])
    entry = {"kind": "opencode", "cwd": str(tmp_path)}
    assert mesh.last_progress(entry, "ses_1") == 1795000000.0
    assert mesh.context_tokens(entry, "ses_1") == 31200
    [call] = mesh.opencode_calls("ses_1")
    assert (call["t"], call["input"], call["read"], call["output"]) == (1795000001.0, 1200, 30000, 50)
    assert mesh.cost_kind("proj-x", {"kind": "opencode", "size": "small"}, False) == "opencode worker small pane"


def test_opencodes_free_limit_reads_as_a_quota_and_switch_rotates(mesh, fake, run, tmp_path, capsys):
    seed_listing(mesh)
    with mesh.editing_run(run) as data:
        data["agents"]["repo-fix-typo"].update(kind="opencode", model="opencode/big-pickle", effort="high",
                                               phase="plan", cwd=str(tmp_path),
                                               models=[{"model": "opencode/big-pickle", "why": "spawn"}])
    mesh.deliver(run, "repo-lead", "repo-fix-typo", "task", "Fix the typo.")
    fake.screen("repo-fix-typo", "  ┃  Free usage exceeded, subscribe to Go [retrying in 4h 12m attempt #1]\n")
    watch = mesh.Watch(run, "repo-fix-typo", grace=1)
    status, line = watch.quota_event()
    assert status == 7 and "resets in 4h 12m" in line and "mesh switch repo-fix-typo" in line
    assert watch.quota_event() is None  # once per task
    limit = mesh.model_limit("opencode", "opencode/big-pickle")
    assert limit and abs(limit["until"] - time.time() - (4 * 3600 + 12 * 60)) < 5
    entry = mesh.load_run(run)["agents"]["repo-fix-typo"]
    model, notes = mesh.cli("opencode").switch_model(entry, "high")
    assert model == "opencode/beam-free"  # the next free one: it ran on big-pickle, which is spent too
    assert "big-pickle is at its limit" in mesh.cli("opencode").plan_model("high", None)[1][0]  # for a new worker
    mesh.note_model_limit("opencode", "opencode/beam-free", "free", time.time() + 600)
    with pytest.raises(mesh.NoModelLeft, match="no OpenCode model left") as left:
        mesh.cli("opencode").switch_model(entry, "high")
    assert left.value.reset and left.value.reset - time.time() < 700  # the soonest reset


def now_ms(offset=0.0):
    return int((time.time() + offset) * 1000)


def reply(model, error=None):
    provider, _, name = model.partition("/")
    return {"role": "assistant", "providerID": provider, "modelID": name, **({"error": error} if error else {})}


def test_a_429_reply_and_a_passing_rate_limit(mesh, fake, run, tmp_path):
    entry = {"kind": "opencode", "pane": "w2:p5", "session": "ses_1", "model": "opencode/big-pickle"}
    watch = mesh.Watch(run, "repo-fix-typo", grace=1)
    opencode_db(mesh, messages=[("ses_1", reply("opencode/big-pickle"), now_ms())])  # the reply it is retrying
    fake.screen("repo-fix-typo", "  ┃  Too Many Requests [retrying in 8s attempt #1]\n")
    assert mesh.cli("opencode").quota_hit(watch, entry) is None  # a passing rate limit retries away
    fake.screen("repo-fix-typo", "  ┃  Too Many Requests [retrying in 30s attempt #4]\n")
    assert mesh.cli("opencode").quota_hit(watch, entry) == ""  # its retry delay isn't the limit's reset
    limit = mesh.model_limit("opencode", "opencode/big-pickle")
    assert limit and abs(limit["until"] - time.time() - mesh.OPENCODE_RATE_HOLD) < 5  # a rate limit's own hold
    fake.screen("repo-fix-typo", "  ┃  API Error: 429 [retrying in 2s attempt #12]\n")
    assert mesh.cli("opencode").quota_hit(watch, entry) == ""
    (mesh.mesh_home() / "model-limits.json").unlink()
    fake.screen("repo-fix-typo", "  ┃\n")
    opencode_db(mesh, messages=[("ses_1", reply("opencode/big-pickle", {"name": "APIError", "data": {
        "statusCode": 429, "message": "Rate limit", "responseBody": '{"error":"FreeUsageLimitError"}'}}), now_ms(1))])
    watch.session = "ses_1"
    assert mesh.cli("opencode").quota_hit(watch, entry) == ""


def test_after_a_switch_the_old_models_stored_429_isnt_the_new_ones(mesh, fake, run):
    entry = {"kind": "opencode", "pane": "w2:p5", "session": "ses_1", "model": "opencode/beam-free"}
    watch = mesh.Watch(run, "repo-fix-typo", grace=1)
    watch.session = "ses_1"
    fake.screen("repo-fix-typo", "  ┃\n")
    error = {"name": "APIError", "data": {"statusCode": 429, "message": "Rate limit"}}
    opencode_db(mesh, messages=[("ses_1", reply("opencode/big-pickle", error), now_ms(-5)),
                                ("ses_1", {"role": "user"}, now_ms(-4))])
    mesh.deliver(run, "repo-lead", "repo-fix-typo", "task", "Go on.")  # the switch's task
    assert mesh.cli("opencode").quota_hit(watch, entry) is None  # the new model hasn't answered yet
    # A limit line still on the pane is the old model's too, until the new one has started a reply.
    fake.screen("repo-fix-typo", "  ┃  Too Many Requests [retrying in 30s attempt #4]\n")
    assert mesh.cli("opencode").quota_hit(watch, entry) is None
    assert mesh.model_limit("opencode", "opencode/beam-free") is None
    fake.screen("repo-fix-typo", "  ┃\n")
    opencode_db(mesh, messages=[("ses_1", reply("opencode/beam-free", error), now_ms(1))])
    assert mesh.cli("opencode").quota_hit(watch, entry) == ""  # its own


def test_a_hold_that_ran_out_isnt_set_again_by_the_continue(mesh, fake, run, monkeypatch):
    entry = {"kind": "opencode", "pane": "w2:p5", "session": "ses_1", "model": "opencode/big-pickle"}
    watch = mesh.Watch(run, "repo-fix-typo", grace=1)
    watch.session = "ses_1"
    fake.screen("repo-fix-typo", "  ┃\n")
    mesh.deliver(run, "repo-lead", "repo-fix-typo", "task", "Fix the typo.")
    error = {"name": "APIError", "data": {"statusCode": 429, "message": "Free",
                                          "responseBody": '{"error":"FreeUsageLimitError"}'}}
    opencode_db(mesh, messages=[("ses_1", reply("opencode/big-pickle", error), now_ms())])
    assert mesh.cli("opencode").quota_hit(watch, entry) == ""
    assert mesh.model_limit("opencode", "opencode/big-pickle")
    mesh.note_model_limit("opencode", "opencode/big-pickle", "free", time.time() - 1)  # the hold ran out
    later = mesh.datetime.now(mesh.timezone.utc) + mesh.timedelta(seconds=5)  # the log keeps whole seconds
    monkeypatch.setattr(mesh, "now_iso", lambda: later.isoformat(timespec="seconds"))
    mesh.deliver(run, "repo-lead", "repo-fix-typo", "task", "continue")  # continue_worker's, at the reset
    assert mesh.cli("opencode").quota_hit(watch, entry) is None
    assert mesh.model_limit("opencode", "opencode/big-pickle") is None


def test_a_limit_with_no_reset_is_held_from_its_first_sighting(mesh, fake, run, monkeypatch):
    entry = {"kind": "opencode", "pane": "w2:p5", "model": "opencode/big-pickle"}
    watch = mesh.Watch(run, "repo-fix-typo", grace=1)
    fake.screen("repo-fix-typo", "  ┃  Free limit reached\n")
    start = time.time()
    assert mesh.cli("opencode").quota_hit(watch, entry) == ""
    monkeypatch.setattr(mesh.time, "time", lambda: start + 3600)  # the next poll, an hour on
    assert mesh.cli("opencode").quota_hit(watch, entry) == ""
    assert abs(mesh.model_limit("opencode", "opencode/big-pickle")["until"] - start - mesh.OPENCODE_LIMIT_HOLD) < 5


def test_a_line_quoting_a_limit_mid_sentence_isnt_one(mesh):
    for line in ['# OpenCode Go\'s "<name> usage limit reached. It will reset in 3 hours …") or a 429 it is',
                 "  The docs say a model's usage limit reached. It will reset in 3 hours when it does",
                 "  so a 429 shows as Too Many Requests [retrying in 2s attempt #3] in the pane",
                 "  ┃  Too Many Requests [retrying in 8s attempt #2]"]:
        assert not mesh.OPENCODE_LIMIT_RE.search(line), line
    for line in ["  ┃  Kimi K2 usage limit reached. It will reset in 3 hours.",
                 "  ┃  API Error: 429 [retrying in 2s attempt #10]",
                 "  ┃  Free usage exceeded, subscribe to Go [retrying in 4h attempt #1]",
                 "  ┃  Rate limit reached for gpt-4o in organization org-abc on tokens per min (TPM): Limit 30000, "
                 "Used 29000 [retrying in 2s attempt #3]",
                 "  ┃  Error: Too Many Requests [retrying in 5s attempt #3]",
                 "  ┃  opencode/kimi-k2 usage limit reached. It will reset in 3 hours.",
                 "  ┃  Kimi K2 (Go) usage limit reached. It will reset in 3 hours."]:
        assert mesh.OPENCODE_LIMIT_RE.search(line), line
    assert mesh.OPENCODE_RESET_RE.search("Kimi K2 usage limit reached. It will reset in 3 hours.").group(1) == "3 hours"
    assert not mesh.OPENCODE_RESET_RE.search("Too Many Requests [retrying in 30s attempt #4]")


def test_open_reopens_an_opencode_worker_with_its_agent_config(mesh, fake, repo, monkeypatch):
    from test_mesh import archived_worker
    seed_listing(mesh)
    archived_worker(mesh, repo, kind="opencode", session="ses_9")
    folder = mesh.mesh_home() / "archive" / "mold"
    data = json.loads((folder / "run.json").read_text())
    data["agents"]["proj-fix"]["model"] = "opencode/big-pickle"
    (folder / "run.json").write_text(json.dumps(data))
    calls = {}
    monkeypatch.setattr(mesh.os, "chdir", lambda p: None)
    monkeypatch.setattr(mesh.os, "execvpe", lambda f, argv, env: calls.update(argv=argv, env=env))
    assert mesh.main(["open", "proj-fix"]) == 0
    agent = calls["argv"][calls["argv"].index("--agent") + 1]
    assert agent in json.loads(calls["env"]["OPENCODE_CONFIG_CONTENT"])["agent"]


# OpenCode's box (ANSI as read): a `┃` gutter, the typed rows, a blank row, the agent and model, then `╹▀▀`.
GUT = "\x1b[0m\x1b[38;2;255;255;255m\x1b[48;2;10;10;10m  \x1b[0m\x1b[38;2;245;167;66m\x1b[48;2;10;10;10m┃\x1b[0m"
BOTTOM = "\x1b[0m\x1b[38;2;255;255;255m\x1b[48;2;10;10;10m  \x1b[0m\x1b[38;2;245;167;66m╹\x1b[0m\x1b[38;2;30;30;30m▀▀▀▀\x1b[0m"


def oc_box(*rows):
    status = f"{GUT}\x1b[48;2;30;30;30m  \x1b[0m\x1b[38;2;245;167;66mMesh-Ab12\x1b[0m \x1b[38;2;128;128;128mauto\x1b[0m"
    return ["  previous output", f"{GUT}  ", *(f"{GUT}\x1b[48;2;30;30;30m  \x1b[0m{r}" for r in rows),
            f"{GUT}  ", status, BOTTOM, "  /tmp/wt  ctrl+p commands"]


def test_opencodes_input_box_is_read_with_its_placeholder_and_status_left_out(mesh):
    placeholder = '\x1b[38;2;128;128;128m\x1b[48;2;30;30;30mAsk anything… "Fix a TODO in the codebase"\x1b[0m'
    assert mesh.typed_text(oc_box(placeholder), "opencode") == ""
    typed = ["\x1b[38;2;238;238;238mhello draft one two\x1b[0m", "\x1b[38;2;238;238;238mthree four\x1b[0m"]
    assert mesh.typed_text(oc_box(*typed), "opencode") == "hello draft one two three four"
    assert mesh.human_draft(oc_box(*typed), "opencode") and not mesh.human_draft(oc_box(placeholder), "opencode")
    assert mesh.typed_text(["no box"], "opencode") is None
    plain = [mesh.strip_sgr(line) for line in oc_box("📬 [mesh] info (ref k9)")]
    assert mesh.doorbells_shown(plain, "refk9)", "opencode") == 0  # in the box: not yet sent
    assert mesh.doorbells_shown(["  📬 [mesh] info (ref k9)", *plain[1:]], "refk9)", "opencode") == 1


def test_copilots_empty_box_reads_empty(mesh):
    rows = ["\x1b[0m\x1b[38;2;129;139;152m╻\x1b[0m\x1b[38;2;20;27;34m▄▄▄▄\x1b[0m",
            "\x1b[0m\x1b[38;2;129;139;152m┃\x1b[0m\x1b[48;2;20;27;34m          \x1b[0m",
            "\x1b[0m\x1b[38;2;129;139;152m╹\x1b[0m\x1b[38;2;20;27;34m▀▀▀▀\x1b[0m", " ← open sidebar · Interactive"]
    assert mesh.gutter_typed(rows, mesh.cli("copilot").ghost_styles, 0) == ""
    rows[1] = "\x1b[0m\x1b[38;2;129;139;152m┃\x1b[0m\x1b[48;2;20;27;34m  half a thought\x1b[0m"
    # A draft in its box wasn't seen live, so mesh doesn't claim to read one: unclear, as for any unread box.
    assert mesh.typed_text(rows, "copilot") is None and not mesh.human_draft(rows, "copilot")


def test_a_near_grey_reads_as_ghost_text(mesh):
    for colour, ghost in (("128;128;128", True), ("120;128;136", True), ("110;120;130", True),
                          ("200;200;200", False), ("128;60;60", False), ("40;40;40", False)):
        assert mesh.visible_chars(f"\x1b[38;2;{colour}mx", ("rgb-grey",))[0][1] is ghost, colour


def test_opencode_compacts_itself(mesh):
    assert not mesh.should_compact("auto", {"kind": "opencode"}, 10**6, False, True)
    assert mesh.compact_conversation({"kind": "opencode", "cwd": "/x"}, "ses_1", "m", "f")[0] is False


def test_durations(mesh):
    assert mesh.parse_duration("4h 12m") == 4 * 3600 + 12 * 60
    assert mesh.parse_duration("3 hours 20 minutes") == 3 * 3600 + 20 * 60
    assert mesh.parse_duration("2 days") == 2 * 86400 and mesh.parse_duration("12m30s") == 750
