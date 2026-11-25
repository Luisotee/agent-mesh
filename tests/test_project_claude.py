"""The repo's own Claude Code project settings (`.claude/settings.json`), which every Claude session in a checkout or
worktree of this repo loads, mesh agents included: the generated-code guard hook."""
import json
import os
import subprocess
import sys
from pathlib import Path

import pytest

ROOT = Path(__file__).resolve().parent.parent
GUARD = ROOT / ".claude" / "hooks" / "guard_generated.py"
SETTINGS = json.loads((ROOT / ".claude" / "settings.json").read_text())
GENERATED = ("daemon/gen", "desktop/src/gen", "android/app/src/gen")


def guard(payload, command=None) -> subprocess.CompletedProcess:
    """Run the hook as Claude Code does: the payload on stdin, the command through a shell when given."""
    text = payload if isinstance(payload, str) else json.dumps(payload)
    argv = command if command else [sys.executable, str(GUARD)]
    return subprocess.run(argv, input=text, capture_output=True, text=True, timeout=30, shell=bool(command),
                          env={**os.environ, "CLAUDE_PROJECT_DIR": str(ROOT)})


def edit(path: str, tool: str = "Edit", cwd: Path = ROOT) -> dict:
    return {"session_id": "s", "hook_event_name": "PreToolUse", "cwd": str(cwd), "tool_name": tool,
            "tool_input": {"file_path": path, "old_string": "a", "new_string": "b"}}


def denial(result: subprocess.CompletedProcess) -> str:
    assert result.returncode == 0, result.stderr
    out = json.loads(result.stdout)["hookSpecificOutput"]
    assert out["hookEventName"] == "PreToolUse" and out["permissionDecision"] == "deny"
    return out["permissionDecisionReason"]


# ── the generated-code guard ─────────────────────────────────────────────


@pytest.mark.parametrize("folder, regenerate", [("daemon/gen", "cd proto && buf generate"),
                                                ("desktop/src/gen", "cd desktop && pnpm gen"),
                                                ("android/app/src/gen", "android/scripts/gen.sh")])
@pytest.mark.parametrize("tool", ["Edit", "Write", "MultiEdit"])
def test_an_edit_to_generated_code_is_denied_with_the_command_that_regenerates_it(folder, regenerate, tool):
    reason = denial(guard(edit(str(ROOT / folder / "agentmesh" / "v1" / "x.go"), tool)))
    assert f"{folder}/" in reason and regenerate in reason
    assert regenerate in denial(guard(edit(f"{folder}/x", tool)))  # a path relative to the payload's cwd


def test_a_relative_path_counts_from_the_sessions_cwd():
    assert "pnpm gen" in denial(guard(edit("src/gen/x.ts", cwd=ROOT / "desktop")))
    assert "buf generate" in denial(guard(edit("../daemon/gen/x.go", cwd=ROOT / "proto")))
    assert guard(edit("src/gen/x.ts", cwd=ROOT)).stdout == ""  # src/gen at the root is no generated folder


@pytest.mark.parametrize("path", ["daemon/CLAUDE.md", "daemon/internal/daemon/agents.go", "desktop/src/store.ts",
                                  "android/app/src/main/kotlin/x.kt", "proto/agentmesh/v1/mesh.proto",
                                  "daemon/generated.go", "desktop/src/gen.ts", "/tmp/daemon/gen/x.go"])
def test_any_other_edit_passes_silently(path):
    result = guard(edit(path))
    assert result.returncode == 0 and result.stdout == "" and result.stderr == ""


@pytest.mark.parametrize("payload", ["", "not json", "[]", "null", json.dumps({"tool_name": "Write"}),
                                     json.dumps({"tool_input": {"file_path": 7}}),
                                     json.dumps({"tool_input": None, "cwd": None})])
def test_a_payload_it_cant_read_passes_silently(payload):
    result = guard(payload)
    assert result.returncode == 0 and result.stdout == "" and result.stderr == ""


def test_the_project_settings_run_the_guard_on_every_editing_tool():
    groups = SETTINGS["hooks"]["PreToolUse"]
    assert len(groups) == 1 and set(groups[0]["matcher"].split("|")) == {"Edit", "Write", "MultiEdit"}
    [hook] = groups[0]["hooks"]
    assert hook["type"] == "command" and ".claude/hooks/guard_generated.py" in hook["command"]
    # The command exactly as Claude Code runs it, from a folder other than the repo's.
    reason = denial(guard(edit(str(ROOT / "daemon/gen/x.go")), command=f"cd / && {hook['command']}"))
    assert "buf generate" in reason
    assert guard(edit(str(ROOT / "daemon/x.go")), command=hook["command"]).stdout == ""


def test_the_project_hooks_add_to_a_mesh_workers_hooks_and_leave_its_inbound_setting_alone(mesh, run, tmp_path):
    """mesh launches a Claude worker with its own hooks in `--settings` (`worker_hooks`): Claude Code adds those to
    the project's, and the two use different events, so neither replaces the other. The project file sets no
    `crossSessionInbound` and turns no hooks off, so a worker here hears from mesh as one in a bare repo does."""
    hooks = mesh.worker_hooks(run, "repo-fix-typo")
    assert set(hooks) & set(SETTINGS["hooks"]) == set()
    worker = json.loads(mesh.worker_settings(mesh.mesh_env(run, "repo-fix-typo", "repo-lead", "worker"), False))
    assert set(worker["hooks"]) == set(hooks)
    assert not {"disableAllHooks", "allowManagedHooksOnly", "crossSessionInbound"} & set(SETTINGS)
    assert mesh.settings_inbound(ROOT / ".claude" / "settings.json") is None
    assert mesh.inbound_setting(ROOT, "accept") == mesh.inbound_setting(tmp_path, "accept")
    assert mesh.worker_accepts(ROOT) == mesh.worker_accepts(tmp_path)
