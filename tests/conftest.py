import importlib.machinery
import importlib.util
import json
import os
import shutil
import subprocess
from pathlib import Path

import pytest

MESH_PATH = Path(__file__).resolve().parent.parent / "bin" / "mesh"


def load_mesh():
    loader = importlib.machinery.SourceFileLoader("mesh", str(MESH_PATH))
    spec = importlib.util.spec_from_loader("mesh", loader)
    module = importlib.util.module_from_spec(spec)
    loader.exec_module(module)
    return module


class FakeHerdr:
    """Records herdr calls; answers from a queue of per-subcommand handlers."""

    def __init__(self, mesh):
        self.mesh = mesh
        self.calls: list[tuple[str, ...]] = []
        self.handlers = {}
        self.screens: dict[str, list[str]] = {}

    def screen(self, target, *texts):
        """What `herdr agent read <target>` (or `pane read`) prints: one text per read, the last one repeated."""
        self.screens[target] = list(texts)

    def text(self, *args):
        self.calls.append(args)
        if args[:2] in (("agent", "read"), ("pane", "read")) and args[2] in self.screens:
            texts = self.screens[args[2]]
            return texts.pop(0) if len(texts) > 1 else texts[0]
        return ""

    def on(self, *prefix, result=None, error=None, errors=None, results=None):
        """Answer calls starting with `prefix`: a result, one error, a list of errors then the result, or a list
        of results, one per call, the last one repeated."""
        queue = list(errors or ([error] if error else []))
        self.handlers[prefix] = (queue, list(results) if results else result)

    def __call__(self, *args, timeout=None):
        self.calls.append(args)
        for prefix, (queue, result) in sorted(self.handlers.items(), key=lambda kv: -len(kv[0])):
            if args[: len(prefix)] == prefix:
                if queue:
                    raise self.mesh.HerdrError(queue.pop(0), "stubbed")
                if isinstance(result, list):
                    return result.pop(0) if len(result) > 1 else result[0]
                return result if result is not None else {}
        return {}

    def called(self, *prefix):
        return [c for c in self.calls if c[: len(prefix)] == prefix]


class FakeCli:
    """Stands in for `run_cli`, so tests never start a real `claude` or `agy`.

    Answers by argv prefix after the binary (`fake_cli.on("agents", stdout=...)`); records every call.
    """

    def __init__(self):
        self.calls: list[dict] = []
        self.handlers = {}
        self.pid = 2 ** 22 + 9  # what a `started` callback gets: a pid no process has

    def on(self, *prefix, returncode=0, stdout="", stderr="", effect=None):
        self.handlers[prefix] = (returncode, stdout, stderr, effect)

    def __call__(self, argv, cwd=None, env=None, timeout=120, input=None, started=None):
        self.calls.append({"argv": list(argv), "cwd": cwd, "env": env, "input": input})
        if started:
            started(self.pid)
        for prefix, (code, out, err, effect) in sorted(self.handlers.items(), key=lambda kv: -len(kv[0])):
            if tuple(argv[1: 1 + len(prefix)]) == prefix:
                if effect:
                    effect(argv)
                return subprocess.CompletedProcess(argv, code, out() if callable(out) else out, err)
        return subprocess.CompletedProcess(argv, 0, "", "")

    def called(self, *prefix):
        return [c for c in self.calls if tuple(c["argv"][1: 1 + len(prefix)]) == prefix]


class RefuseAgentClis(subprocess.Popen):
    """subprocess.Popen (under subprocess.run too) for anything but an agent CLI or herdr, which fail the test."""

    def __init__(self, args, *rest, **kwargs):
        binary = Path(args[0] if isinstance(args, (list, tuple)) else str(args).split()[0]).name
        assert binary not in ("agy", "claude", "copilot", "herdr"), f"a test started a real {binary}: {args}"
        super().__init__(args, *rest, **kwargs)


def write_meminfo(path, available, swap_free, total=16 << 20, swap_total=32 << 20):
    """A /proc/meminfo with these fields, in kB."""
    path.write_text(f"MemTotal:       {total} kB\nMemFree:        {available // 2} kB\n"
                    f"MemAvailable:   {available} kB\nSwapTotal:      {swap_total} kB\n"
                    f"SwapFree:       {swap_free} kB\n")


@pytest.fixture
def mesh(tmp_path, monkeypatch):
    monkeypatch.setenv("MESH_HOME", str(tmp_path / "state"))
    monkeypatch.setenv("MESH_WORKTREES", str(tmp_path / "worktrees"))
    # No real Claude socket: a test that wants a Claude root creates one here (see claude_root).
    monkeypatch.setenv("MESH_CC_SOCKS", str(tmp_path / "socks"))
    # Most tests check the run-tab placement; the side-pane tests turn it back on.
    monkeypatch.setenv("MESH_SIDE_PANES", "0")
    # Never Copilot's real session events: a test that wants some writes them here (see copilot_events).
    monkeypatch.setenv("COPILOT_HOME", str(tmp_path / "copilot"))
    # Never agy's real conversation index, prompt history or conversations: a test that wants one writes its own.
    monkeypatch.setenv("MESH_AGY_SUMMARIES", str(tmp_path / "agy" / "conversation_summaries.db"))
    monkeypatch.setenv("MESH_AGY_HISTORY", str(tmp_path / "agy" / "history.jsonl"))
    monkeypatch.setenv("MESH_AGY_CONVERSATIONS", str(tmp_path / "agy" / "conversations"))
    # CLAUDE_CODE_SESSION_ID, CLAUDE_PID, CLAUDE_CODE_MESSAGING_SOCKET: the suite may run inside Claude Code, whose
    # session would meet `inherited_identity` and whose socket `claude_address`.
    for key in ("MESH_RUN", "MESH_SELF", "MESH_PARENT", "MESH_ROLE", "MESH_COMPACT_AT", "MESH_AGY_COMPACT_AT",
                "MESH_ROOT_RESET_AT", "MESH_ROOT_AUTORESET", "MESH_CACHE_TTL", "MESH_RESET_WAIT",
                "MESH_QUOTA_STOP", "MESH_MIN_QUOTA", "MESH_CLAUDE_USAGE",
                "CLAUDE_CODE_SESSION_ID", "CLAUDE_PID", "CLAUDE_CODE_MESSAGING_SOCKET"):
        monkeypatch.delenv(key, raising=False)
    # Never this machine's Claude session files either (a root's is looked up across every project folder).
    monkeypatch.setenv("CLAUDE_CONFIG_DIR", str(tmp_path / "claude-config"))
    # Never this machine's memory: plenty free, so the memory guard passes (see low_memory for a short machine).
    write_meminfo(tmp_path / "meminfo", available=16 << 20, swap_free=16 << 20)
    monkeypatch.setenv("MESH_MEMINFO", str(tmp_path / "meminfo"))
    monkeypatch.delenv("MESH_MIN_FREE_MB", raising=False)
    # Never this machine's policy file or folders (a test that wants a config writes one here, see with_config).
    monkeypatch.setenv("MESH_CONFIG_FILE", str(tmp_path / "agent-mesh.json"))
    for key in ("MESH_REPOS_DIR", "MESH_PEERS_DIR", "MESH_COPILOT_MODEL", "MESH_CLAUDE"):
        monkeypatch.delenv(key, raising=False)
    # Every CLI counts as installed, whatever this machine has; `clis()` narrows it.
    real_which = shutil.which
    monkeypatch.setattr(shutil, "which", lambda cmd, *a, **k: f"/bin/{cmd}" if cmd in ("agy", "claude", "copilot")
                        else real_which(cmd, *a, **k))
    # A path that bypasses the fakes below fails instead of reaching a real herdr pane or agent CLI.
    monkeypatch.setenv("MESH_HERDR", str(tmp_path / "no-herdr"))
    module = load_mesh()
    monkeypatch.setattr(module.subprocess, "Popen", RefuseAgentClis)
    monkeypatch.setattr(module.time, "sleep", lambda s: None)
    monkeypatch.setattr(module, "run_cli", FakeCli())
    # Never this machine's managed Claude settings: a test that wants a policy writes one here (inbound_setting).
    monkeypatch.setattr(module, "CLAUDE_MANAGED", tmp_path / "claude-managed")
    # Never this machine's real claude/agy processes (live_holder): a test that wants some lists its own.
    monkeypatch.setattr(module, "agent_processes", lambda: [])
    # ...nor any other of its processes (the sweep), and never a real signal: a test that wants either fakes it.
    monkeypatch.setattr(module, "process_table", lambda: [])
    monkeypatch.setattr(module, "kill_process", refuse_kill)
    # Never Claude's real usage endpoint: a test that wants a reading sets one (see claude_windows).
    module.real_fetch_claude_usage = module.fetch_claude_usage
    monkeypatch.setattr(module, "fetch_claude_usage", lambda: None)
    # ...nor agy's quotas (the reserve reads them on every agy spawn): the tests of agy_usage use real_agy_usage.
    module.real_agy_usage = module.agy_usage
    monkeypatch.setattr(module, "agy_usage", lambda force=False: None)
    # Never a real detached retirer (a worker's `done` starts one): `module.retirers` lists what would have started.
    module.retirers = []
    module.real_start_retirer = module.start_retirer
    monkeypatch.setattr(module, "start_retirer", lambda run_id, handle: module.retirers.append(run_id) or 4242)
    return module


def refuse_kill(pid, sig):
    raise AssertionError(f"a test sent a real signal {sig} to {pid}")


class Inboxes:
    """Real Claude inbox sockets, `<pid>.sock` in a short folder set as MESH_CC_SOCKS (a unix socket's path holds
    about 108 bytes, more than a test's tmp_path can leave): `listen(pid)` binds one, `frames(pid)` reads back the
    frames written to it so far, as mesh writes them."""

    def __init__(self, folder):
        self.folder, self.servers, self.read = folder, {}, {}

    def listen(self, pid):
        import socket
        server = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        server.bind(str(self.folder / f"{pid}.sock"))
        server.listen(16)
        server.setblocking(False)
        self.servers[pid] = server
        return f"uds:{self.folder / f'{pid}.sock'}"

    def frames(self, pid):
        server, found = self.servers[pid], self.read.setdefault(pid, [])
        while True:
            try:
                conn, _ = server.accept()
            except BlockingIOError:
                return found
            conn.setblocking(True)
            with conn:
                data = b"".join(iter(lambda: conn.recv(65536), b""))
            found.extend(json.loads(line) for line in data.decode().splitlines() if line)

    def close(self):
        for server in self.servers.values():
            server.close()


@pytest.fixture
def inbox(mesh, monkeypatch):
    import shutil
    import tempfile
    folder = Path(tempfile.mkdtemp(prefix="cc", dir="/tmp"))
    monkeypatch.setenv("MESH_CC_SOCKS", str(folder))
    inboxes = Inboxes(folder)
    yield inboxes
    inboxes.close()
    shutil.rmtree(folder, ignore_errors=True)


@pytest.fixture
def cli(mesh):
    return mesh.run_cli


@pytest.fixture
def fake(mesh, monkeypatch):
    stub = FakeHerdr(mesh)
    monkeypatch.setattr(mesh, "herdr", stub)
    monkeypatch.setattr(mesh, "herdr_text", stub.text)
    return stub


@pytest.fixture
def run(mesh, fake, monkeypatch):
    """An initialised run whose root is pane w1:p1, plus one worker."""
    monkeypatch.setenv("HERDR_ENV", "1")
    monkeypatch.setenv("HERDR_PANE_ID", "w1:p1")
    fake.on("agent", "list", result={"agents": [{"pane_id": "w1:p1", "agent": "claude", "tab_id": "w1:t1",
                                                  "workspace_id": "w1", "cwd": "/repo"}]})
    assert mesh.main(["init", "--mode", "agy", "--run-id", "mtest"]) == 0
    with mesh.editing_run("mtest") as data:
        data["agents"]["repo-fix-typo"] = {"role": "worker", "kind": "agy", "pane": "w2:p5", "parent": "repo-lead",
                                           "status": "running", "tab": "w2:t3"}
    return "mtest"


def read_json(path):
    return json.loads(Path(path).read_text())


def with_config(data) -> Path:
    """Write the policy file the `mesh` fixture points MESH_CONFIG_FILE at."""
    path = Path(os.environ["MESH_CONFIG_FILE"])
    path.write_text(json.dumps(data))
    return path


def clis(monkeypatch, *kinds: str) -> None:
    """Only these agent CLIs count as installed (claude, agy, copilot)."""
    real = shutil.which
    monkeypatch.setattr(shutil, "which", lambda cmd, *a, **k: (f"/bin/{cmd}" if cmd in kinds else None)
                        if cmd in ("agy", "claude", "copilot") else real(cmd, *a, **k))
