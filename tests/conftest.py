import importlib.machinery
import importlib.util
import json
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
        """What `herdr agent read <target>` prints: one text per read, the last one repeated."""
        self.screens[target] = list(texts)

    def text(self, *args):
        self.calls.append(args)
        if args[:2] == ("agent", "read") and args[2] in self.screens:
            texts = self.screens[args[2]]
            return texts.pop(0) if len(texts) > 1 else texts[0]
        return ""

    def on(self, *prefix, result=None, error=None, errors=None):
        """Answer calls starting with `prefix`: a result, one error, or a list of errors then the result."""
        queue = list(errors or ([error] if error else []))
        self.handlers[prefix] = (queue, result)

    def __call__(self, *args, timeout=None):
        self.calls.append(args)
        for prefix, (queue, result) in sorted(self.handlers.items(), key=lambda kv: -len(kv[0])):
            if args[: len(prefix)] == prefix:
                if queue:
                    raise self.mesh.HerdrError(queue.pop(0), "stubbed")
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

    def on(self, *prefix, returncode=0, stdout="", stderr="", effect=None):
        self.handlers[prefix] = (returncode, stdout, stderr, effect)

    def __call__(self, argv, cwd=None, env=None, timeout=120, input=None):
        import subprocess
        self.calls.append({"argv": list(argv), "cwd": cwd, "env": env, "input": input})
        for prefix, (code, out, err, effect) in sorted(self.handlers.items(), key=lambda kv: -len(kv[0])):
            if tuple(argv[1: 1 + len(prefix)]) == prefix:
                if effect:
                    effect(argv)
                return subprocess.CompletedProcess(argv, code, out() if callable(out) else out, err)
        return subprocess.CompletedProcess(argv, 0, "", "")

    def called(self, *prefix):
        return [c for c in self.calls if tuple(c["argv"][1: 1 + len(prefix)]) == prefix]


@pytest.fixture
def mesh(tmp_path, monkeypatch):
    monkeypatch.setenv("MESH_HOME", str(tmp_path / "state"))
    monkeypatch.setenv("MESH_WORKTREES", str(tmp_path / "worktrees"))
    # No real Claude socket: a test that wants a Claude root creates one here (see claude_root).
    monkeypatch.setenv("MESH_CC_SOCKS", str(tmp_path / "socks"))
    # Most tests check the run-tab placement; the side-pane tests turn it back on.
    monkeypatch.setenv("MESH_SIDE_PANES", "0")
    # Never agy's real conversation index, prompt history or conversations: a test that wants one writes its own.
    monkeypatch.setenv("MESH_AGY_SUMMARIES", str(tmp_path / "agy" / "conversation_summaries.db"))
    monkeypatch.setenv("MESH_AGY_HISTORY", str(tmp_path / "agy" / "history.jsonl"))
    monkeypatch.setenv("MESH_AGY_CONVERSATIONS", str(tmp_path / "agy" / "conversations"))
    # CLAUDE_CODE_SESSION_ID, CLAUDE_PID: the suite may run inside Claude Code, whose session would meet
    # `inherited_identity` and whose process `claude_address`.
    for key in ("MESH_RUN", "MESH_SELF", "MESH_PARENT", "MESH_ROLE", "MESH_COMPACT_AT", "MESH_AGY_COMPACT_AT",
                "CLAUDE_CODE_SESSION_ID", "CLAUDE_PID"):
        monkeypatch.delenv(key, raising=False)
    module = load_mesh()
    monkeypatch.setattr(module.time, "sleep", lambda s: None)
    monkeypatch.setattr(module, "run_cli", FakeCli())
    return module


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
