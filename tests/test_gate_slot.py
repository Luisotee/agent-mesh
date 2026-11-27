"""scripts/gate-slot.sh: the host-wide queue the three check scripts take a slot from before their heavy part."""

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


@pytest.fixture
def gates(tmp_path):
    env = {
        **os.environ,
        "GATE_DIR": str(tmp_path / "gates"),
        "GATE_SLOTS": "2",
        "GATE_POLL": "0.2",
    }
    for k in ("GATE_SLOT_HELD", "GATE_WAIT_MAX", "XDG_RUNTIME_DIR"):
        env.pop(k, None)
    procs = []
    queues = {}

    def pump(p, q):
        for ln in p.stdout:
            q.put(ln)

    def watch(p):
        q = queues[p.pid] = queue.Queue()
        threading.Thread(target=pump, args=(p, q), daemon=True).start()
        return p

    def start(name, extra=None, hold=60):
        """A gate that takes a slot, says `got`, and holds it for `hold` seconds."""
        e = {**env, **(extra or {})}
        p = subprocess.Popen(
            ["bash", "-c", f'source "{HELPER}"; gate_slot {name}; echo got; sleep {hold}'],
            env=e, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True, start_new_session=True,
        )
        procs.append(p)
        return watch(p)

    def line(p, timeout=5):
        """The next line the process prints, or None if it prints none in `timeout` seconds."""
        try:
            return queues[p.pid].get(timeout=timeout)
        except queue.Empty:
            return None

    line.watch = watch

    yield env, start, line
    for p in procs:
        try:
            os.killpg(p.pid, signal.SIGKILL)
        except ProcessLookupError:
            pass
        p.wait()


def test_two_gates_get_slots_and_a_third_waits_until_one_ends(gates):
    env, start, line = gates
    a, b = start("one"), start("two")
    assert line(a).strip() == "got" and line(b).strip() == "got"
    c = start("three")
    out = ""
    # The third gate says it waits, and who holds the slots, and has no slot.
    seen = []
    deadline = time.time() + 5
    while time.time() < deadline and "declare this wait" not in "".join(seen):
        got = line(c, 1)
        if got:
            seen.append(got)
    out = "".join(seen)
    assert "waiting for a gate slot (all 2 busy" in out, out
    assert "one@" in out or "two@" in out, out
    assert "mesh send parent --type wait" in out
    assert "got" not in [s.strip() for s in seen]
    # One ends: the waiting gate takes its slot.
    a.kill()
    os.killpg(a.pid, signal.SIGKILL)
    a.wait()
    rest = []
    deadline = time.time() + 10
    while time.time() < deadline and "got" not in [s.strip() for s in rest]:
        got = line(c, 1)
        if got:
            rest.append(got)
    assert "got" in [s.strip() for s in rest], rest


def test_a_killed_gate_never_leaves_its_slot_held(gates):
    env, start, line = gates
    e = {"GATE_SLOTS": "1"}
    a = start("holder", e)
    assert line(a).strip() == "got"
    os.killpg(a.pid, signal.SIGKILL)  # no trap runs, the kernel lets the lock go
    a.wait()
    b = start("next", e)
    assert line(b, 5).strip() == "got"


def test_slots_zero_turns_the_queue_off(gates):
    env, start, line = gates
    held = [start(f"g{i}", {"GATE_SLOTS": "1"}) for i in range(1)]
    assert line(held[0]).strip() == "got"
    off = start("off", {"GATE_SLOTS": "0"})
    assert line(off).strip() == "got"


def test_a_gate_that_already_holds_a_slot_does_not_wait_for_a_second(gates):
    env, start, line = gates
    e = {"GATE_SLOTS": "1"}
    a = start("outer", e)
    assert line(a).strip() == "got"
    nested = start("inner", {**e, "GATE_SLOT_HELD": "1"})
    assert line(nested).strip() == "got"


def test_gate_wait_max_gives_up_with_75(gates):
    env, start, line = gates
    e = {"GATE_SLOTS": "1"}
    a = start("holder", e)
    assert line(a).strip() == "got"
    r = subprocess.run(
        ["bash", "-c", f'source "{HELPER}"; gate_slot late; echo "rc=$?"'],
        env={**env, **e, "GATE_WAIT_MAX": "1"}, capture_output=True, text=True, timeout=20,
    )
    assert "rc=75" in r.stdout and "giving up" in r.stderr, (r.stdout, r.stderr)


def test_a_term_to_a_waiting_gate_ends_it_at_once(gates):
    env, start, line = gates
    e = {"GATE_SLOTS": "1", "GATE_POLL": "30"}
    a = start("holder", e)
    assert line(a).strip() == "got"
    w = subprocess.Popen(
        ["bash", "-c", f'trap "exit 143" TERM; source "{HELPER}"; gate_slot late; echo never'],
        env={**env, **e}, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True, start_new_session=True,
    )
    line.watch(w)
    try:
        seen = [line(w, 5), line(w, 5)]
        assert any("waiting for a gate slot" in (x or "") for x in seen), seen
        t0 = time.time()
        w.send_signal(signal.SIGTERM)
        w.wait(timeout=5)
        assert time.time() - t0 < 3, "the TERM waited for the poll's sleep"
        assert w.returncode == 143
    finally:
        try:
            os.killpg(w.pid, signal.SIGKILL)
        except ProcessLookupError:
            pass


def test_status_lists_the_holders(gates):
    env, start, line = gates
    a = start("daemon")
    assert line(a).strip() == "got"
    r = subprocess.run([str(HELPER), "status"], env=env, capture_output=True, text=True, timeout=10)
    assert "gate slots: 2" in r.stdout and "daemon@" in r.stdout, r.stdout
    os.killpg(a.pid, signal.SIGKILL)
    a.wait()
    r = subprocess.run([str(HELPER), "status"], env=env, capture_output=True, text=True, timeout=10)
    assert "all free" in r.stdout, r.stdout


def test_the_default_is_derived_from_the_host_and_at_least_one(tmp_path):
    env = {k: v for k, v in os.environ.items() if k != "GATE_SLOTS"}
    r = subprocess.run(
        ["bash", "-c", f'source "{HELPER}"; gate_slots'], env=env, capture_output=True, text=True, timeout=10
    )
    assert int(r.stdout.strip()) >= 1
    r = subprocess.run(
        ["bash", "-c", f'source "{HELPER}"; GATE_SLOTS=oops gate_slots'], env=env, capture_output=True, text=True, timeout=10
    )
    assert int(r.stdout.strip()) >= 1 and "not a whole number" in r.stderr


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
