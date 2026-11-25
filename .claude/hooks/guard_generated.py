#!/usr/bin/env python3
"""PreToolUse guard for Edit, Write and MultiEdit: deny a hand edit to generated code, naming the command that
regenerates it. Silent otherwise, and on a payload it can't read (it guards against a mistake, never blocks work;
the settings' `|| true` keeps a missing script or a crash from blocking every edit, as exit 2 would).
Stdlib only; tested by tests/test_project_claude.py."""
import json
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent.parent  # the checkout (or worktree) this file is in

GENERATED = {  # folder under ROOT -> how to regenerate it
    "daemon/gen": "change the .proto under proto/, then run `cd proto && buf generate` (see proto/CLAUDE.md)",
    "desktop/src/gen": "change the .proto under proto/, then run `cd desktop && pnpm gen` (see proto/CLAUDE.md)",
    "android/app/src/gen": "change the .proto under proto/, then run `android/scripts/gen.sh` (see proto/CLAUDE.md)",
    "desktop/electron/icons.ts": "change desktop/scripts/make-icons.mjs, then run `node scripts/make-icons.mjs` in desktop/",
}


def reason(payload: dict) -> str | None:
    """Why this tool call must not run, or None to let it through."""
    tool_input = payload.get("tool_input")
    raw = tool_input.get("file_path") if isinstance(tool_input, dict) else None
    if not isinstance(raw, str) or not raw:
        return None
    path = Path(raw)
    if not path.is_absolute():
        path = Path(payload.get("cwd") or ".") / path
    path = path.resolve()
    for folder, how in GENERATED.items():
        if path.is_relative_to((ROOT / folder).resolve()):
            return f"{folder}{'' if folder.endswith('.ts') else '/'} is generated code: don't edit it by hand; {how}."
    return None


def main() -> int:
    try:
        payload = json.load(sys.stdin)
        why = reason(payload) if isinstance(payload, dict) else None
    except (ValueError, OSError, TypeError):
        return 0
    if why:
        json.dump({"hookSpecificOutput": {"hookEventName": "PreToolUse", "permissionDecision": "deny",
                                          "permissionDecisionReason": why}}, sys.stdout)
    return 0


if __name__ == "__main__":
    sys.exit(main())
