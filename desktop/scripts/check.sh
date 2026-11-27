#!/usr/bin/env bash
# The CI equivalent for desktop/ (the repo runs no hosted CI): locked install, generated TypeScript current,
# typecheck, unit tests, build, and the end-to-end tests (the built app, driven over CDP, against a real meshd).
#
#   desktop/scripts/check.sh              # everything
#   SKIP_E2E=1 desktop/scripts/check.sh   # without the end-to-end tests
#
# The e2e tests open a real Electron window: on a virtual display when xvfb-run is installed, so nothing pops
# up on your screen (E2E_VISIBLE=1 shows them), else on DISPLAY (scripts/e2e.sh). They build meshd,
# mesh-term, mesh and the fake claude from daemon/ into a temp dir and give every run a state dir, HOME and
# user-data dir of its own under the temp dir: nothing of the user's is read or written.
set -euo pipefail

desktop_dir=$(cd "$(dirname "$0")/.." && pwd)
export PATH="$PATH:$HOME/.local/go/bin:$HOME/.local/opt/buf/bin"

step() { printf '\n== %s\n' "$*"; }
cd "$desktop_dir"

step "pnpm install (locked)"
pnpm install --frozen-lockfile

step "generated TypeScript is current"
before=$(mktemp -d)
trap 'rm -rf "$before"' EXIT
cp -r src/gen/. "$before"
pnpm gen
if ! diff -r "$before" src/gen >/dev/null; then
  diff -rq "$before" src/gen || true
  echo "desktop/src/gen is stale: commit the output of 'pnpm gen' (in desktop/)" >&2
  exit 1
fi

# The heavy part (typecheck, tests, build, e2e) waits for a host-wide gate slot (scripts/gate-slot.sh).
source "$desktop_dir/../scripts/gate-slot.sh"
gate_slot desktop

step "typecheck"
pnpm typecheck

step "unit tests"
pnpm test

step "build"
pnpm build

if [ -z "${SKIP_E2E:-}" ]; then
  step "end-to-end tests"
  pnpm e2e
fi

echo
echo "desktop/scripts/check.sh: all passed"
