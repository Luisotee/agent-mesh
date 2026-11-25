"""A reset root knows the protocol (design §15): the root card, `mesh protocol`, the reminder, the watch defaults
and the delivery nets that a lost doorbell needs. Fakes only."""
import json
import re
import shlex
from pathlib import Path

import pytest

from test_lean import lean, lean_root  # noqa: F401
from test_mesh import as_worker, claude_root, stop_hook  # noqa: F401

CARD = Path(__file__).resolve().parent.parent / "protocol" / "ROOT_CARD.md"
FILL = {"tier": "mid", "size": "small", "kind": "claude"}


def card_commands():
    """Every `mesh …` code span of the card, with its placeholders filled in."""
    out = []
    for span in re.findall(r"`(mesh [^`]+)`", CARD.read_text()):
        text = re.sub(r"<(\w+)>", lambda m: FILL.get(m.group(1), "x"), span)
        text = text.replace("$(mesh dir)", "/tmp/run")
        out.append(shlex.split(text)[1:])
    return out


# ── the card ─────────────────────────────────────────────────────────────


def test_every_command_the_card_names_parses_with_the_real_parser(mesh):
    parser = mesh.build_parser()
    commands = card_commands()
    assert len(commands) > 15
    commands_known = parser._subparsers._group_actions[0].choices
    for argv in commands:
        if len(argv) == 1:  # a bare mention of a command
            assert argv[0] in commands_known, argv
            continue
        try:
            parser.parse_args(argv)
        except SystemExit:
            pytest.fail(f"`mesh {' '.join(argv)}` doesn't parse")


def test_every_protocol_part_the_card_points_to_exists(mesh):
    parts = mesh.protocol_sections()
    named = [argv[1] for argv in card_commands() if argv[0] == "protocol" and len(argv) > 1 and argv[1] != "x"]
    assert named and all(name in parts for name in named), [n for n in named if n not in parts]


def test_the_card_stays_short_and_the_brief_prints_it_for_the_root(mesh, fake, run, capsys):
    fake.on("agent", "get", result={"agent": {"agent_status": "idle"}})
    assert CARD.stat().st_size < 6000
    assert mesh.main(["brief"]) == 0
    out = capsys.readouterr().out
    assert CARD.read_text().strip() in out
    assert out.index("## Root card") < out.index("## workers")


def test_a_non_roots_brief_has_no_card(mesh, fake, run, capsys):
    fake.on("agent", "get", result={"agent": {"agent_status": "idle"}})
    assert mesh.main(["brief", "--as", "repo-fix-typo"]) == 0
    assert "## Root card" not in capsys.readouterr().out


def test_the_brief_points_at_the_section_for_what_waits(mesh, fake, run, capsys):
    fake.on("agent", "get", result={"agent": {"agent_status": "idle"}})
    mesh.deliver(run, "repo-lead", "repo-fix-typo", "task", "Fix it. Send your plan for review first.")
    mesh.deliver(run, "repo-fix-typo", "repo-lead", "plan", "# Plan")
    assert mesh.main(["brief"]) == 0
    out = capsys.readouterr().out
    assert "NOW: repo-fix-typo's plan waits: before you approve or answer it, `mesh protocol root.6`" in out
    mesh.deliver(run, "repo-fix-typo", "repo-lead", "done", "PR: https://github.com/a/b/pull/1")
    assert mesh.main(["brief"]) == 0
    out = capsys.readouterr().out
    now = [line for line in out.splitlines() if line.startswith("NOW:")]
    assert len(now) == 1 and "`mesh protocol root.8`" in now[0] and "root.6" not in now[0]


def test_the_reset_prompt_sends_the_root_to_the_card(mesh):
    text = mesh.bootstrap_text("mtest", "repo-lead", None, False, "tok")
    assert "act by the root card" in text


# ── mesh protocol ────────────────────────────────────────────────────────


def test_mesh_protocol_prints_one_part(mesh, capsys):
    assert mesh.main(["protocol"]) == 0
    listing = capsys.readouterr().out
    for name in ("root", "root.6", "your-own-context", "protocol", "rules-for-everyone", "root-card"):
        assert re.search(rf"^  {re.escape(name)}\s+\d+ chars$", listing, re.M), name
    assert mesh.main(["protocol", "root.6"]) == 0
    out = capsys.readouterr().out
    assert out.startswith("## Root, step 6") and "Review every plan" in out and "Check every `done`" not in out
    assert mesh.main(["protocol", "your-own-context"]) == 0
    assert "mesh reset --next" in capsys.readouterr().out
    assert mesh.main(["protocol", "protocol"]) == 0
    whole = capsys.readouterr().out
    assert whole.startswith("# agent-mesh protocol") and "## If you are a worker" in whole


def test_an_unknown_protocol_part_lists_the_names(mesh, capsys):
    assert mesh.main(["protocol", "nope"]) == 1
    assert re.search(r"no protocol part named nope; the names are: .*root\.6", capsys.readouterr().err)


def test_a_worker_may_read_the_protocol(mesh, fake, run, monkeypatch, capsys):
    as_worker(monkeypatch, run)
    assert mesh.main(["protocol", "worker"]) == 0
    assert "Worker" in capsys.readouterr().out


# ── the reminder a relaunched root carries ───────────────────────────────


def test_a_roots_reminder_sends_it_to_the_brief(mesh, fake, run, capsys):
    assert mesh.main(["whoami", "--reminder", "compact"]) == 0
    out = capsys.readouterr().out
    assert "you are repo-lead, the root of run mtest" in out and "run `mesh brief` now" in out
    assert "root card" in out and "worker" not in out.split("run `mesh brief`")[0]


def test_a_roots_reminder_says_nothing_once_its_run_is_over(mesh, fake, run, monkeypatch, capsys):
    def gone(args):
        raise mesh.MeshError("no run here")
    monkeypatch.setattr(mesh, "identity", gone)
    assert mesh.main(["whoami", "--reminder", "resume"]) == 0
    captured = capsys.readouterr()
    assert captured.out == "" and captured.err == ""
    assert mesh.main(["whoami", "--reminder", "resume", "--run", "mtest", "--as", "repo-lead"]) == 0  # a worker's names it
    assert "mesh whoami --reminder" in capsys.readouterr().err


def test_relaunch_settings_carry_the_root_hooks_and_keep_the_users(mesh, run, lean, monkeypatch, tmp_path):
    lean_root(mesh, run, monkeypatch, tmp_path)
    user = {"model": "opus", "hooks": {"SessionStart": [{"hooks": [{"type": "command", "command": "echo hi"}]}],
                                       "Stop": [{"hooks": [{"type": "command", "command": "echo stop"}]}]}}
    (tmp_path / "user.json").write_text(json.dumps(user))
    for _ in range(1):
        plan, why = mesh.relaunch_plan(run, "repo-lead", mesh.load_run(run)["agents"]["repo-lead"], None)
        assert plan, why
        hooks = json.loads((mesh.root_files("w1:p1") / "settings.json").read_text())["hooks"]
        start = hooks["SessionStart"]
        assert start[0] == user["hooks"]["SessionStart"][0] and hooks["Stop"] == user["hooks"]["Stop"]
        assert [g["matcher"] for g in start[1:]] == ["compact", "clear", "resume"]
        command = start[1]["hooks"][0]["command"]
        assert shlex.split(command)[1:] == ["whoami", "--reminder", "compact"]  # no run: found by its pane


def test_merged_settings_appends_hook_lists_instead_of_replacing_them(mesh):
    a, b = {"hooks": {"Stop": [{"x": 1}]}}, {"hooks": {"Stop": [{"x": 2}], "SessionStart": [{"y": 1}]}}
    assert mesh.merged_settings(a, b)["hooks"] == {"Stop": [{"x": 1}, {"x": 2}], "SessionStart": [{"y": 1}]}
    assert mesh.merged_settings({}, b)["hooks"] == b["hooks"]
    assert mesh.merged_settings(a, {"hooks": {"Stop": [{"x": 1}]}})["hooks"] == {"Stop": [{"x": 1}]}


# ── safe defaults ────────────────────────────────────────────────────────


@pytest.fixture
def watched(mesh, monkeypatch):
    seen = {}

    def watch_agents(run_id, names, timeout, interval, grace, **kw):
        seen.update(timeout=timeout, alerts=kw.get("alerts_only"))
        return 6, "timeout"
    monkeypatch.setattr(mesh, "watch_agents", watch_agents)
    return seen


def test_a_roots_any_watch_is_alerts_only_with_the_configured_timeout(mesh, fake, run, watched):
    fake.on("agent", "get", result={"agent": {"agent_status": "idle"}})
    assert mesh.main(["watch", "--any"]) == 6
    assert watched == {"timeout": 7200, "alerts": True}
    assert mesh.main(["watch", "--any", "--timeout", "30", "--reports"]) == 6
    assert watched == {"timeout": 30, "alerts": False}
    assert mesh.main(["watch", "--any", "--timeout", "0"]) == 6
    assert watched["timeout"] == 0


def test_the_watch_timeout_is_a_policy_leaf(mesh, fake, run, watched, monkeypatch):
    from conftest import with_config
    fake.on("agent", "get", result={"agent": {"agent_status": "idle"}})
    with_config({"watch": {"timeout": 600}})
    assert mesh.main(["watch", "--any"]) == 6 and watched["timeout"] == 600
    monkeypatch.setenv("MESH_WATCH_TIMEOUT", "90")
    assert mesh.main(["watch", "--any"]) == 6 and watched["timeout"] == 90
    assert mesh.main(["watch", "repo-fix-typo"]) == 6 and watched["timeout"] == 0  # a single worker: none


def test_a_ring_refused_for_a_draft_tells_the_root_not_to_type(mesh, fake, run, monkeypatch, capsys):
    mesh.deliver(run, "repo-lead", "repo-fix-typo", "task", "Fix it.")
    monkeypatch.setattr(mesh, "ring_stored", lambda *a, **k: {"rung": False, "error": "human_draft", "via": None})
    assert mesh.main(["ring", "repo-fix-typo"]) == 2
    err = capsys.readouterr().err
    assert "Tell the user; don't type into the pane yourself" in err


# ── delivery nets ────────────────────────────────────────────────────────


def native_report(mesh, run, mtype="done", address="uds:/x/1.sock"):
    """A worker's report delivered the `native` way: stored for the root, and handed back to the worker."""
    path = mesh.store_message(run, "repo-fix-typo", "repo-lead", mtype, "Fixed.")
    message = f"📬 [mesh] {mtype} from repo-fix-typo: Fixed. (run `mesh inbox`)"
    mesh.log_event(run, "send", sender="repo-fix-typo", to="repo-lead", type=mtype, file=path.name)
    mesh.log_event(run, "native", sender="repo-fix-typo", to="repo-lead", address=address, file=path.name,
                   type=mtype, message=message)
    return path, message


def transcript(tmp_path, *blocks):
    path = tmp_path / "worker.jsonl"
    lines = [json.dumps({"type": "assistant", "timestamp": "2999-01-01T00:00:00.000Z",
                         "message": {"content": [{"type": "tool_use", "name": name, "input": inp}]}})
             for name, inp in blocks]
    path.write_text("\n".join(lines) + "\n")
    return str(path)


def test_a_workers_stop_hook_blocks_once_for_a_send_message_it_owes(mesh, fake, run, monkeypatch, capsys, tmp_path):
    path, message = native_report(mesh, run)
    silent = transcript(tmp_path, ("Bash", {"command": "mesh send parent"}))
    reason = stop_hook(mesh, monkeypatch, capsys, run, transcript_path=silent)
    given = json.dumps({"to": "uds:/x/1.sock", "message": message}, ensure_ascii=False)
    assert given in reason and "SendMessage" in reason and "'" not in reason.split("(JSON): ")[1]
    assert [e["file"] for e in mesh.read_log(run) if e["event"] == "hook_send"] == [path.name]
    assert stop_hook(mesh, monkeypatch, capsys, run, transcript_path=silent) is None  # once per report


def test_no_block_once_the_send_message_is_in_its_transcript_or_the_report_is_read(mesh, fake, run, monkeypatch,
                                                                                capsys, tmp_path):
    path, message = native_report(mesh, run)
    sent = transcript(tmp_path, ("SendMessage", {"to": "uds:/x/other.sock", "message": "x"}),
                      ("SendMessage", {"to": "uds:/x/1.sock", "message": message}))
    assert stop_hook(mesh, monkeypatch, capsys, run, transcript_path=sent) is None
    assert not [e for e in mesh.read_log(run) if e["event"] == "hook_send"]
    other = transcript(tmp_path, ("SendMessage", {"to": "uds:/x/other.sock", "message": "x"}))
    mesh.mark_read(path)  # the root read it anyway
    assert stop_hook(mesh, monkeypatch, capsys, run, transcript_path=other) is None
    assert stop_hook(mesh, monkeypatch, capsys, run, transcript_path=str(tmp_path / "none.jsonl")) is None  # can't tell


def test_an_old_send_message_doesnt_count_for_a_new_report(mesh, fake, run, monkeypatch, capsys, tmp_path):
    native_report(mesh, run)
    old = tmp_path / "old.jsonl"
    old.write_text(json.dumps({"type": "assistant", "timestamp": "2020-01-01T00:00:00.000Z", "message": {"content": [
        {"type": "tool_use", "name": "SendMessage", "input": {"to": "uds:/x/1.sock"}}]}}) + "\n")
    assert "SendMessage" in stop_hook(mesh, monkeypatch, capsys, run, transcript_path=str(old))


def test_mesh_send_ends_with_the_send_message_to_make(mesh, fake, run, monkeypatch, capsys):
    address = claude_root(mesh, run)
    with mesh.editing_run(run) as data:
        data["agents"]["repo-fix-typo"]["kind"] = "claude"
    as_worker(monkeypatch, run)
    assert mesh.main(["send", "parent", "--type", "done", "--body", "PR: https://github.com/a/b/pull/1"]) == 0
    captured = capsys.readouterr()
    *_, hint, last = captured.err.strip().splitlines()
    assert "SendMessage" in hint
    given = json.loads(last)  # one JSON line: no Python quoting for the model to copy
    assert given["to"] == address and "done from repo-fix-typo" in given["message"]
    event = [e for e in mesh.read_log(run) if e["event"] == "native"][-1]
    assert event["type"] == "done" and "done from repo-fix-typo" in event["message"]


def lost(mesh, run, monkeypatch, after=0):
    monkeypatch.setattr(mesh, "REPORT_LOST_AFTER", after)
    watch = mesh.Watch(run, "repo-fix-typo", 5)
    watch.unread_report = None  # the first-tick check is its own test
    return watch


def test_a_report_unread_with_no_doorbell_rung_ends_the_watch_once(mesh, fake, run, monkeypatch):
    path, _ = native_report(mesh, run, "plan")
    watch = lost(mesh, run, monkeypatch)
    status, line = watch.report_event(mesh.read_log(run), True)  # alerts only: it still counts
    assert status == 0 and "plan has sat unread" in line and "mesh inbox" in line
    assert watch.report_event(mesh.read_log(run), True) is None  # once per file


def test_the_net_stays_quiet_for_a_rung_a_read_or_a_fresh_report(mesh, fake, run, monkeypatch):
    path, _ = native_report(mesh, run, "question")
    mesh.log_event(run, "rung", sender="repo-fix-typo", to="repo-lead", file=path.name, via="socket", tries=1)
    assert lost(mesh, run, monkeypatch).report_event(mesh.read_log(run), True) is None  # a doorbell rang
    path, _ = native_report(mesh, run, "done")
    mesh.log_event(run, "collected", sender="repo-fix-typo", to="repo-lead", file=path.name)
    assert lost(mesh, run, monkeypatch).report_event(mesh.read_log(run), True) is None  # a fan-out collects it
    path, _ = native_report(mesh, run, "done")
    assert lost(mesh, run, monkeypatch, after=3600).report_event(mesh.read_log(run), True) is None  # too new
    mesh.mark_read(path)
    assert lost(mesh, run, monkeypatch).report_event(mesh.read_log(run), True) is None  # read
    ask = mesh.Watch(run, "repo-fix-typo", 5, finals_at_start=0)  # `mesh ask` reads its reply itself
    native_report(mesh, run, "done")
    assert ask.report_event(mesh.read_log(run), True) is None


def test_the_net_stays_quiet_when_the_workers_transcript_shows_its_send_message(mesh, fake, run, monkeypatch,
                                                                               tmp_path):
    path, message = native_report(mesh, run, "done", address="uds:/x/1.sock")
    log = transcript(tmp_path, ("SendMessage", {"to": "uds:/x/1.sock", "message": message}))
    with mesh.editing_run(run) as data:
        data["agents"]["repo-fix-typo"].update(session="s1", cwd=str(tmp_path))
    monkeypatch.setattr(mesh, "claude_transcript", lambda entry, session: Path(log))
    assert lost(mesh, run, monkeypatch).report_event(mesh.read_log(run), True) is None  # the root is only busy
    monkeypatch.setattr(mesh, "claude_transcript", lambda entry, session: Path(transcript(tmp_path)))
    assert lost(mesh, run, monkeypatch).report_event(mesh.read_log(run), True)[0] == 0  # no SendMessage: lost


def test_a_send_message_record_with_no_readable_time_still_counts(mesh, tmp_path):
    path = tmp_path / "t.jsonl"
    path.write_text(json.dumps({"type": "assistant", "message": {"content": [
        {"type": "tool_use", "name": "SendMessage", "input": {"to": "uds:/a"}}]}}) + "\n")
    assert mesh.sent_message(str(path), "uds:/a", 1.0) is True


def test_a_message_with_both_kinds_of_quote_is_given_as_json_to_copy(mesh, fake, run, monkeypatch, capsys, tmp_path):
    tricky = "📬 [mesh] done from repo-fix-typo: it's \"fixed\" (run `mesh inbox`)"
    path = mesh.store_message(run, "repo-fix-typo", "repo-lead", "done", "x")
    mesh.log_event(run, "native", sender="repo-fix-typo", to="repo-lead", address="uds:/x/1.sock", file=path.name,
                   type="done", message=tricky)
    reason = stop_hook(mesh, monkeypatch, capsys, run, transcript_path=transcript(tmp_path))
    assert json.loads(reason.split("(JSON): ")[1]) == {"to": "uds:/x/1.sock", "message": tricky}


def test_the_lost_report_note_is_logged_so_a_restarted_watch_doesnt_repeat_it(mesh, fake, run, monkeypatch):
    path, _ = native_report(mesh, run, "plan")
    assert lost(mesh, run, monkeypatch).report_event(mesh.read_log(run), True)[0] == 0
    assert [e["file"] for e in mesh.read_log(run) if e["event"] == "lost_noted"] == [path.name]
    assert lost(mesh, run, monkeypatch).report_event(mesh.read_log(run), True) is None  # a new `mesh watch --any`


def test_a_tail_without_the_send_message_is_unknown_not_never_sent(mesh, tmp_path, monkeypatch):
    sent = transcript(tmp_path, ("SendMessage", {"to": "uds:/x/1.sock"}), *[("Bash", {"command": "x" * 200})] * 50)
    assert mesh.sent_message(sent, "uds:/x/1.sock", 1.0) is True
    monkeypatch.setattr(mesh, "SENT_TAIL", 2000)  # the SendMessage is before what is read
    assert mesh.sent_message(sent, "uds:/x/1.sock", 1.0) is None
    assert mesh.sent_message(transcript(tmp_path, ("Bash", {"command": "x"})), "uds:/x/1.sock", 1.0) is False  # all read


def test_the_stop_hook_doesnt_block_when_the_send_message_may_be_before_the_tail(mesh, fake, run, monkeypatch, capsys,
                                                                                tmp_path):
    native_report(mesh, run)
    sent = transcript(tmp_path, ("SendMessage", {"to": "uds:/x/1.sock"}), *[("Bash", {"command": "x" * 200})] * 50)
    monkeypatch.setattr(mesh, "SENT_TAIL", 2000)
    assert stop_hook(mesh, monkeypatch, capsys, run, transcript_path=sent) is None


def test_an_info_after_a_native_plan_doesnt_hide_the_plan_from_the_stop_hook(mesh, fake, run, monkeypatch, capsys,
                                                                            tmp_path):
    path, message = native_report(mesh, run, "plan")
    native_report(mesh, run, "info")
    reason = stop_hook(mesh, monkeypatch, capsys, run, transcript_path=transcript(tmp_path))
    assert "your plan to repo-lead" in reason and [e["file"] for e in mesh.read_log(run)
                                                    if e["event"] == "hook_send"] == [path.name]


def test_a_roots_reminder_is_silent_while_a_reset_is_about_to_type_its_own(mesh, fake, run, monkeypatch, capsys):
    monkeypatch.setattr(mesh, "reset_pending", lambda run_id: True)  # the bootstrap says `mesh brief` itself
    assert mesh.main(["whoami", "--reminder", "clear"]) == 0
    assert capsys.readouterr().out == "" and not [e for e in mesh.read_log(run) if e["event"] == "reminder"]
    monkeypatch.setattr(mesh, "reset_pending", lambda run_id: False)
    assert mesh.main(["whoami", "--reminder", "clear"]) == 0
    assert "mesh brief" in capsys.readouterr().out
