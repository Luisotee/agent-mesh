#!/usr/bin/env bash
# The CI equivalent for desktop/ (the repo runs no hosted CI): locked install, generated TypeScript current,
# typecheck, unit tests, build, and the end-to-end tests (the built app, driven over CDP, against a real meshd).
#
#   desktop/scripts/check.sh              # everything
#   SKIP_E2E=1 desktop/scripts/check.sh   # without the end-to-end tests
#
# The e2e tests open a real Electron window, so they need a display (WSLg has one). They build meshd,
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

step "typecheck"
pnpm typecheck

step "unit tests"
pnpm test

step "build"
pnpm build

if [ -z "${SKIP_E2E:-}" ]; then
  step "end-to-end tests"
  if [ -z "${DISPLAY:-}${WAYLAND_DISPLAY:-}" ]; then
    if command -v xvfb-run >/dev/null 2>&1; then
      xvfb-run -a pnpm e2e
    else
      echo "no display for Electron: set DISPLAY, install xvfb, or SKIP_E2E=1" >&2
      exit 1
    fi
  else
    pnpm e2e
  fi
fi

echo
echo "desktop/scripts/check.sh: all passed"
