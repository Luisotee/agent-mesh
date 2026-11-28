"""scripts/gate-slot.sh: the host-wide queue the three check scripts pass before their heavy part.

The memory numbers come from fake /proc/meminfo and /proc/pressure/memory files (GATE_MEMINFO, GATE_PSI) a test edits
while gates wait. Every part needs 1000 MB here, and a running gate holds back about that much (its tree is a few MB).
"""

import os
import queue
import signal
import subprocess
import threading
import time
from pathlib import Path

import pytest

REPO = Path(__file__).resolve().parent.parent
HELPER = REPO / "scripts" / "gate-slot.sh"
CHECKS = ["daemon/scripts/check.sh", "desktop/scripts/check.sh", "android/scripts/check.sh"]


class Host:
    """The fake host: what MemAvailable and the memory pressure say, and the gates started on it."""

    def __init__(self, tmp_path):
        self.tmp = tmp_path
        self.meminfo = tmp_path / "meminfo"
        self.psi = tmp_path / "psi"
        self.order = tmp_path / "order.txt"
        self.env = {
            **{k: v for k, v in os.environ.items() if not k.startswith(("GATE_", "AGENT_MESH_"))},
            "GATE_DIR": str(tmp_path / "gates"),
            "GATE_POLL": "0.2",
            "GATE_MEMINFO": str(self.meminfo),
            "GATE_PSI": str(self.psi),
            "GATE_NEED_MB_DAEMON": "1000",
            "GATE_NEED_MB_DESKTOP": "1000",
            "GATE_NEED_MB_ANDROID": "1000",
        }
        self.env.pop("XDG_RUNTIME_DIR", None)
        self.free(100000)
        self.pressure(0)
        self.procs = []
        self.queues = {}

    def free(self, mb):
        self.meminfo.write_text(f"MemTotal: 16000000 kB\nMemAvailable: {mb * 1024} kB\n")

    def pressure(self, full_avg10):
        self.psi.write_text(
            "some avg10=1.00 avg60=1.00 avg300=1.00 total=1\n"
            f"full avg10={full_avg10:.2f} avg60=0.00 avg300=0.00 total=1\n"
        )

    def watch(self, p):
        q = self.queues[p.pid] = queue.Queue()

        def pump():
            for ln in p.stdout:
                q.put(ln)

        threading.Thread(target=pump, daemon=True).start()
        self.procs.append(p)
        return p

    def start(self, part="daemon", extra=None, hold=60, tag=None, pre=""):
        """A gate that takes a slot, says `got`, and holds it for `hold` seconds. `tag` is logged to order.txt on `got`."""
        tag = tag or part
        script = f'{pre} source "{HELPER}"; gate_slot {part}; echo got; echo {tag} >> "{self.order}"; sleep {hold}'
        p = subprocess.Popen(
            ["bash", "-c", script],
            env={**self.env, **(extra or {})}, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True,
            start_new_session=True,
        )
        return self.watch(p)

    def line(self, p, timeout=5):
        """The next line the process prints, or None if it prints none in `timeout` seconds."""
        try:
            return self.queues[p.pid].get(timeout=timeout)
        except queue.Empty:
            return None

    def until(self, p, text, timeout=8):
        """Everything the process prints until a line holds `text` (or the time is up)."""
        seen, deadline = [], time.time() + timeout
        while time.time() < deadline:
            got = self.line(p, 0.5)
            if got:
                seen.append(got)
                if text in got:
                    break
        return "".join(seen)

    def got(self, p, timeout=5):
        return "got" in self.until(p, "got", timeout)

    def kill(self, p):
        try:
            os.killpg(p.pid, signal.SIGKILL)
        except ProcessLookupError:
            pass
        p.wait()

    def order_list(self):
        return self.order.read_text().split() if self.order.exists() else []

    def status(self):
        return subprocess.run([str(HELPER), "status"], env=self.env, capture_output=True, text=True, timeout=10).stdout


@pytest.fixture
def host(tmp_path):
    h = Host(tmp_path)
    yield h
    for p in h.procs:
        h.kill(p)


def test_the_first_gate_is_admitted_even_with_no_room(host):
    host.free(10)
    host.pressure(60)
    a = host.start("android")
    assert host.got(a), "a gate that runs alone must never wait"


def test_a_second_gate_is_admitted_with_room_and_queued_without_it(host):
    a = host.start("daemon")
    assert host.got(a)
    b = host.start("desktop")  # 100 GB available: room
    assert host.got(b)
    host.free(1500)  # 1500 less the ~2000 MB the two gates still expect to grow by: no room
    c = host.start("android")
    out = host.until(c, "checks again")
    assert "waiting for a gate slot" in out and "MB available" in out and "needs 1000 MB" in out, out
    assert "daemon@" in out and "desktop@" in out, out
    # room returns: the queued gate takes its slot
    host.free(100000)
    assert host.got(c, 8)


def test_the_memory_pressure_brake(host):
    a = host.start("daemon")
    assert host.got(a)
    host.pressure(8.5)
    b = host.start("daemon")
    out = host.until(b, "checks again")
    assert "memory pressure 8.50% (the limit is 5%)" in out, out
    host.pressure(0.3)
    assert host.got(b, 8)


def test_a_backstop_caps_the_gates_whatever_the_memory(host):
    extra = {"GATE_SLOTS_MAX": "2"}
    a, b = host.start("daemon", extra), host.start("desktop", extra)
    assert host.got(a) and host.got(b)
    c = host.start("daemon", extra)
    out = host.until(c, "checks again")
    assert "all 2 slots busy" in out, out
    host.kill(a)
    assert host.got(c, 8)


def test_waiters_are_served_in_the_order_they_came(host):
    host.start("daemon", tag="holder")
    assert host.until(host.procs[0], "got")
    host.free(1500)
    for tag in ("w1", "w2", "w3"):
        w = host.start("daemon", hold=1, tag=tag)
        assert "waiting for a gate slot" in host.until(w, "checks again"), tag
    host.free(100000)  # room for all three at once: the order must still be the queue's
    deadline = time.time() + 15
    while time.time() < deadline and len(host.order_list()) < 4:
        time.sleep(0.2)
    assert host.order_list() == ["holder", "w1", "w2", "w3"], host.order_list()


def test_a_killed_waiters_ticket_does_not_block_the_next(host):
    a = host.start("daemon")
    assert host.got(a)
    host.free(1500)
    w1 = host.start("daemon", tag="w1")
    assert "waiting" in host.until(w1, "checks again")
    w2 = host.start("daemon", tag="w2")
    assert "waiting" in host.until(w2, "checks again")
    host.kill(w1)  # the oldest ticket goes with its process
    host.free(100000)
    assert host.got(w2, 8)
    assert [t for t in (host.tmp / "gates").glob("wait.*")] == []


def test_a_waiting_gate_declares_its_wait_once(host):
    log = host.tmp / "mesh.log"
    bindir = host.tmp / "bin"
    bindir.mkdir()
    stub = bindir / "mesh"
    stub.write_text(f'#!/bin/sh\necho "$* token=$AGENT_MESH_TOKEN" >> "{log}"\n')
    stub.chmod(0o755)
    env = {"PATH": f"{bindir}:{os.environ['PATH']}", "AGENT_MESH_TOKEN": "tok", "GATE_POLL": "0.1"}
    a = host.start("daemon")
    assert host.got(a)
    host.free(1500)
    # the way daemon/scripts/check.sh calls it: sourced first, the identity unset, the gate taken later
    pre = f'source "{HELPER}"; for n in $(compgen -e | grep ^AGENT_MESH_); do unset "$n"; done;'
    b = host.start("daemon", env, pre=pre)
    assert "checks again" in host.until(b, "checks again")
    time.sleep(1)  # several polls
    host.free(100000)
    assert host.got(b, 8)
    calls = log.read_text().splitlines()
    assert len(calls) == 1, calls
    assert calls[0].startswith("send parent --type wait --until 60m --body daemon gate waiting")
    assert calls[0].endswith("token=tok"), "the wait is declared as the agent, not as nobody"


def test_a_gate_without_an_identity_declares_nothing(host):
    log = host.tmp / "mesh.log"
    bindir = host.tmp / "bin"
    bindir.mkdir()
    stub = bindir / "mesh"
    stub.write_text(f'#!/bin/sh\necho "$*" >> "{log}"\n')
    stub.chmod(0o755)
    a = host.start("daemon")
    assert host.got(a)
    host.free(1500)
    b = host.start("daemon", {"PATH": f"{bindir}:{os.environ['PATH']}"})
    assert "checks again" in host.until(b, "checks again")
    assert not log.exists()


def test_a_killed_gate_never_leaves_its_slot_held(host):
    e = {"GATE_SLOTS_MAX": "1"}
    a = host.start("daemon", e)
    assert host.got(a)
    host.kill(a)  # no trap runs, the kernel lets the lock go
    b = host.start("daemon", e)
    assert host.got(b)


def test_slots_max_zero_turns_the_queue_off(host):
    a = host.start("daemon", {"GATE_SLOTS_MAX": "1"})
    assert host.got(a)
    off = host.start("daemon", {"GATE_SLOTS_MAX": "0"})
    assert host.got(off)


def test_a_gate_that_already_holds_a_slot_does_not_wait_for_a_second(host):
    e = {"GATE_SLOTS_MAX": "1"}
    a = host.start("daemon", e)
    assert host.got(a)
    nested = host.start("daemon", {**e, "GATE_SLOT_HELD": "1"})
    assert host.got(nested)


def test_gate_wait_max_gives_up_with_75(host):
    e = {"GATE_SLOTS_MAX": "1"}
    a = host.start("daemon", e)
    assert host.got(a)
    r = subprocess.run(
        ["bash", "-c", f'source "{HELPER}"; gate_slot late; echo "rc=$?"'],
        env={**host.env, **e, "GATE_WAIT_MAX": "1"}, capture_output=True, text=True, timeout=20,
    )
    assert "rc=75" in r.stdout and "giving up" in r.stderr, (r.stdout, r.stderr)
    assert list((host.tmp / "gates").glob("wait.*")) == [], "a gate that gave up leaves no ticket"


def test_a_term_to_a_waiting_gate_ends_it_at_once(host):
    e = {"GATE_SLOTS_MAX": "1", "GATE_POLL": "30"}
    a = host.start("daemon", e)
    assert host.got(a)
    w = subprocess.Popen(
        ["bash", "-c", f'trap "exit 143" TERM; source "{HELPER}"; gate_slot daemon; echo never'],
        env={**host.env, **e}, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True, start_new_session=True,
    )
    host.watch(w)
    assert "waiting for a gate slot" in host.until(w, "checks again")
    t0 = time.time()
    w.send_signal(signal.SIGTERM)
    w.wait(timeout=5)
    assert time.time() - t0 < 3, "the TERM waited for the poll's sleep"
    assert w.returncode == 143


def test_status_lists_holders_waiters_and_the_memory(host):
    a = host.start("daemon")
    assert host.got(a)
    host.free(1500)
    w = host.start("android")
    assert "checks again" in host.until(w, "checks again")
    out = host.status()
    assert "running: slot 0 daemon@" in out and "needs 1000 MB" in out, out
    assert "waiting: android@" in out, out
    assert "1500 MB available" in out and "pressure 0" in out, out
    host.kill(w)
    host.kill(a)
    out = host.status()
    assert "running: none" in out and "waiting: none" in out, out


def test_a_whole_number_is_required_for_the_settings():
    env = {k: v for k, v in os.environ.items() if not k.startswith("GATE_")}
    r = subprocess.run(
        ["bash", "-c", f'source "{HELPER}"; gate_slots_max; GATE_SLOTS_MAX=oops gate_slots_max; GATE_NEED_MB_ANDROID=big gate_need_mb android'],
        env=env, capture_output=True, text=True, timeout=10,
    )
    assert r.stdout.split() == ["6", "6", "2500"], r
    assert r.stderr.count("is not a whole number") == 2


@pytest.mark.parametrize("script", CHECKS)
def test_every_check_script_takes_a_slot_after_its_cheap_steps(script):
    text = (REPO / script).read_text()
    assert "scripts/gate-slot.sh" in text and "gate_slot " in text, script
    # the slot comes before the part that is heavy, and after the checks that are cheap
    first_heavy = {"daemon": 'step "go vet"', "desktop": 'step "typecheck"', "android": "./gradlew --no-daemon testDebugUnitTest"}[
        script.split("/")[0]
    ]
    assert text.index("gate_slot ") < text.index(first_heavy), script
    assert subprocess.run(["bash", "-n", str(REPO / script)]).returncode == 0


def test_the_daemon_gate_keeps_its_identity_for_the_wait():
    text = (REPO / "daemon/scripts/check.sh").read_text()
    assert text.index('source "$repo/scripts/gate-slot.sh"') < text.index("unset \"$name\""), (
        "the helper must be sourced before AGENT_MESH_* is unset, or it has no identity to declare the wait with"
    )
