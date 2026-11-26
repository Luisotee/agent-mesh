"""A desktop notification (a Windows toast under WSL, else notify-send) when a herdr agent pane goes `done` or `blocked`, except agent-mesh workers.

Run by herdr on `pane.agent_status_changed` (HERDR_PLUGIN_EVENT_JSON). Skips:
- a pane that is a live agent-mesh worker (its pane or session is a non-retired worker in a run.json);
- a mesh root going `done` while one of its workers still owes a report (the root only handled a report);
- the focused pane, and a state that changed again within DELAY seconds.
`blocked` (needs input) always notifies for a root or a plain session.
"""
import base64
import shutil
import json
import os
import subprocess
import sys
import time
from pathlib import Path

DELAY = 1.5  # seconds a state must hold before it notifies, like herdr's ui.toast.delay_seconds
STALE_RUN = 6 * 3600  # a run whose log hasn't changed in this long is ignored (its root likely died)
RECEIVED_TYPES = ("task", "answer")  # mirror bin/mesh: what a worker acts on
REPORT_TYPES = ("plan", "question", "done", "failed")  # mirror bin/mesh: what ends a worker's turn
POWERSHELL = shutil.which("powershell.exe") or "/mnt/c/WINDOWS/System32/WindowsPowerShell/v1.0/powershell.exe"
APP_ID = r"{1AC14E77-02E7-4E5D-B744-2EB1AE5198B7}\WindowsPowerShell\v1.0\powershell.exe"

STATE = Path(os.environ.get("HERDR_PLUGIN_STATE_DIR") or Path.home() / ".local/state/mesh-notify")
MESH_HOME = Path(os.environ.get("MESH_HOME") or Path.home() / ".local/state/agent-mesh")
HERDR = os.environ.get("HERDR_BIN_PATH") or "herdr"


def log(line: str) -> None:
    try:
        STATE.mkdir(parents=True, exist_ok=True)
        with open(STATE / "notify.log", "a", encoding="utf-8") as f:
            f.write(f"{time.strftime('%Y-%m-%dT%H:%M:%S')} {line}\n")
    except OSError:
        pass


def pane_info(pane_id: str) -> dict:
    out = subprocess.run([HERDR, "pane", "get", pane_id], capture_output=True, text=True, timeout=5).stdout
    try:
        return json.loads(out)["result"]["pane"]
    except (ValueError, KeyError, TypeError):
        return {}


def live_runs():
    """(run.json, log events) of each run whose log changed within STALE_RUN."""
    now = time.time()
    for run_file in MESH_HOME.glob("*/run.json"):
        log_file = run_file.with_name("log.jsonl")
        try:
            if now - log_file.stat().st_mtime > STALE_RUN:
                continue
            run = json.loads(run_file.read_text())
        except (OSError, ValueError):
            continue
        yield run, log_file


def read_events(log_file: Path) -> list[dict]:
    events = []
    for line in log_file.read_text(errors="replace").splitlines():
        try:
            events.append(json.loads(line))
        except ValueError:
            pass
    return events


def owes_report(events: list[dict], name: str) -> bool:
    """True when `name`'s newest task or answer has no report after it (it is working for the root)."""
    owes = False
    for e in events:
        if e.get("event") != "send":
            continue
        if e.get("to") == name and e.get("type") in RECEIVED_TYPES:
            owes = True
        elif e.get("sender") == name and e.get("type") in REPORT_TYPES:
            owes = False
    return owes


def mesh_role(pane_id: str, session: str | None):
    """(role, run, name, log file) or None for the agent in this pane."""
    for run, log_file in live_runs():
        for name, entry in run.get("agents", {}).items():
            if entry.get("status") == "retired":
                continue
            same_pane = entry.get("pane") == pane_id
            same_session = bool(session) and entry.get("session") == session
            if same_pane and entry.get("role") != "root" and session and entry.get("session") and not same_session:
                continue  # herdr reused the pane id for another session (a root's changes on reset or /clear)
            if same_pane or same_session:
                return entry.get("role"), run, name, log_file
    return None


def busy_workers(run: dict, log_file: Path) -> list[str]:
    events = read_events(log_file)
    return [name for name, entry in run.get("agents", {}).items()
            if entry.get("role") != "root" and entry.get("status") != "retired" and owes_report(events, name)]


def ps_quote(text: str) -> str:
    return "'" + text.replace("'", "''") + "'"


def toast(title: str, body: str) -> None:
    if not Path(POWERSHELL).exists():
        if shutil.which("notify-send"):
            subprocess.run(["notify-send", "--app-name=herdr", title, body], capture_output=True, timeout=10)
        else:
            log("no notifier: neither powershell.exe nor notify-send")
        return
    xml_escape = lambda s: s.replace("&", "&amp;").replace("<", "&lt;").replace(">", "&gt;")
    xml = (f"<toast><visual><binding template='ToastGeneric'><text>{xml_escape(title)}</text>"
           f"<text>{xml_escape(body)}</text></binding></visual></toast>")
    script = (
        "[Windows.UI.Notifications.ToastNotificationManager, Windows.UI.Notifications, ContentType = WindowsRuntime] | Out-Null;"
        "[Windows.Data.Xml.Dom.XmlDocument, Windows.Data.Xml.Dom.XmlDocument, ContentType = WindowsRuntime] | Out-Null;"
        f"$x = New-Object Windows.Data.Xml.Dom.XmlDocument; $x.LoadXml({ps_quote(xml)});"
        f"[Windows.UI.Notifications.ToastNotificationManager]::CreateToastNotifier({ps_quote(APP_ID)})"
        ".Show([Windows.UI.Notifications.ToastNotification]::new($x))"
    )
    encoded = base64.b64encode(script.encode("utf-16-le")).decode()
    subprocess.run([POWERSHELL, "-NoProfile", "-NonInteractive", "-EncodedCommand", encoded],
                   capture_output=True, timeout=20)


def main() -> None:
    if "--test" in sys.argv:
        toast("herdr", "Mesh Notify test")
        return
    try:
        event = json.loads(os.environ.get("HERDR_PLUGIN_EVENT_JSON") or "{}")
    except ValueError:
        event = {}
    try:
        (STATE / "last-event.json").write_text(json.dumps(event))  # for debugging the payload shape
    except OSError:
        pass
    data = event.get("data") or {}
    status = str(data.get("agent_status") or "").lower()
    pane_id = data.get("pane_id") or os.environ.get("HERDR_PANE_ID")
    if status not in ("done", "blocked") or not pane_id:
        return

    time.sleep(DELAY)
    pane = pane_info(pane_id)
    if str(pane.get("agent_status") or "").lower() != status:
        return  # moved on already
    if pane.get("focused"):
        return
    session = (pane.get("agent_session") or {}).get("value")
    role = mesh_role(pane_id, session)
    who = Path(pane.get("cwd") or "").name or pane.get("agent") or "agent"
    if role:
        kind, run, name, log_file = role
        if kind != "root":
            log(f"skip worker {name} {pane_id} {status}")
            return
        if status == "done" and (busy := busy_workers(run, log_file)):
            log(f"skip root {name} done, busy: {', '.join(busy)}")
            return
        who = name
    title = f"{who} needs input" if status == "blocked" else f"{who} finished"
    body = pane.get("terminal_title_stripped") or pane_id
    log(f"toast {pane_id} {title} | {body}")
    toast(title, body)


if __name__ == "__main__":
    try:
        main()
    except Exception as exc:  # never fail herdr's hook loudly
        log(f"error {type(exc).__name__}: {exc}")
