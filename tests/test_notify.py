"""The herdr mesh-notify plugin (herdr/mesh-notify/notify.py): which agent states notify and which are mesh noise."""
import importlib.util
import json
import os
from pathlib import Path

import pytest

ROOT = Path(__file__).resolve().parent.parent


@pytest.fixture
def notify(tmp_path, monkeypatch):
    spec = importlib.util.spec_from_file_location("mesh_notify", ROOT / "herdr/mesh-notify/notify.py")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    monkeypatch.setattr(module, "MESH_HOME", tmp_path / "mesh")
    monkeypatch.setattr(module, "STATE", tmp_path / "state")
    monkeypatch.setattr(module, "DELAY", 0)
    module.sent = []
    monkeypatch.setattr(module, "toast", lambda title, body: module.sent.append(title))
    run = tmp_path / "mesh/mx"
    run.mkdir(parents=True)
    (run / "run.json").write_text(json.dumps({"id": "mx", "root": "repo-lead", "agents": {
        "repo-lead": {"role": "root", "pane": "w1:p1", "status": "running", "session": "S-root"},
        "other-fix": {"role": "worker", "pane": "w1:p2", "status": "running", "session": "S-w"},
        "old-fix": {"role": "worker", "pane": "w1:p3", "status": "retired"}}}))
    (run / "log.jsonl").write_text("")
    return module


TASK = {"event": "send", "sender": "repo-lead", "to": "other-fix", "type": "task"}
DONE = {"event": "send", "sender": "other-fix", "to": "repo-lead", "type": "done"}


def fire(notify, monkeypatch, pane_id, status, session=None, log=None, now=None, focused=False):
    if log is not None:
        (notify.MESH_HOME / "mx/log.jsonl").write_text("".join(json.dumps(e) + "\n" for e in log))
    pane = {"agent_status": now or status, "focused": focused, "cwd": "/x/repo", "terminal_title_stripped": "t",
            "agent_session": {"value": session} if session else None}
    monkeypatch.setattr(notify, "pane_info", lambda p: pane)
    monkeypatch.setenv("HERDR_PLUGIN_EVENT_JSON", json.dumps({"data": {"pane_id": pane_id, "agent_status": status}}))
    notify.sent.clear()
    notify.main()
    return list(notify.sent)


@pytest.mark.parametrize("status", ["done", "blocked"])
def test_a_worker_never_notifies(notify, monkeypatch, status):
    assert fire(notify, monkeypatch, "w1:p2", status, "S-w") == []
    assert fire(notify, monkeypatch, "w9:p9", status, "S-w") == []  # its session in another pane


def test_a_reused_pane_or_a_retired_worker_notifies(notify, monkeypatch):
    assert fire(notify, monkeypatch, "w1:p2", "done", "S-other") == ["repo finished"]
    assert fire(notify, monkeypatch, "w1:p3", "done") == ["repo finished"]


def test_a_root_is_done_only_once_no_worker_owes_a_report(notify, monkeypatch):
    assert fire(notify, monkeypatch, "w1:p1", "done", "S-root", log=[TASK]) == []
    assert fire(notify, monkeypatch, "w1:p1", "blocked", "S-root") == ["repo-lead needs input"]
    assert fire(notify, monkeypatch, "w1:p1", "done", "S-root", log=[TASK, DONE]) == ["repo-lead finished"]
    assert fire(notify, monkeypatch, "w1:p1", "done", "S-cleared", log=[TASK]) == []  # a reset root is still the root


def test_a_stale_run_is_ignored(notify, monkeypatch):
    log = notify.MESH_HOME / "mx/log.jsonl"
    log.write_text("")
    os.utime(log, (0, 0))
    assert fire(notify, monkeypatch, "w1:p2", "done", "S-w") == ["repo finished"]


def test_plain_sessions_notify_unless_focused_or_moved_on(notify, monkeypatch):
    assert fire(notify, monkeypatch, "w5:p1", "done") == ["repo finished"]
    assert fire(notify, monkeypatch, "w5:p1", "working") == []
    assert fire(notify, monkeypatch, "w5:p1", "done", focused=True) == []
    assert fire(notify, monkeypatch, "w5:p1", "done", now="working") == []


def test_the_report_types_match_mesh(notify, mesh):
    assert notify.RECEIVED_TYPES == mesh.RECEIVED_TYPES
    assert notify.REPORT_TYPES == mesh.REPORT_TYPES
