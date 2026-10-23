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


@pytest.fixture
def mesh(tmp_path, monkeypatch):
    monkeypatch.setenv("MESH_HOME", str(tmp_path / "state"))
    monkeypatch.setenv("MESH_WORKTREES", str(tmp_path / "worktrees"))
    for key in ("MESH_RUN", "MESH_SELF", "MESH_PARENT", "MESH_ROLE"):
        monkeypatch.delenv(key, raising=False)
    module = load_mesh()
    monkeypatch.setattr(module.time, "sleep", lambda s: None)
    return module


@pytest.fixture
def fake(mesh, monkeypatch):
    stub = FakeHerdr(mesh)
    monkeypatch.setattr(mesh, "herdr", stub)
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
