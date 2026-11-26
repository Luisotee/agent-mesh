#!/usr/bin/env bash
# Runs the end-to-end tests (`pnpm e2e` runs this). Their Electron windows go to a virtual display when
# xvfb-run is installed, so they never pop up on the user's screen (under WSLg, DISPLAY is the Windows
# desktop). E2E_VISIBLE=1 uses the real display instead, to watch a run; without xvfb-run the real
# display is the only one.
set -euo pipefail
cd "$(dirname "$0")/.."
run=(pnpm exec vitest run --config vitest.e2e.config.ts "$@")
if [ -z "${E2E_VISIBLE:-}" ] && command -v xvfb-run >/dev/null 2>&1; then
  exec env -u WAYLAND_DISPLAY xvfb-run -a "${run[@]}"
fi
if [ -z "${DISPLAY:-}${WAYLAND_DISPLAY:-}" ]; then
  echo "no display for Electron: install xvfb (sudo apt-get install -y xvfb), set DISPLAY, or SKIP_E2E=1" >&2
  exit 1
fi
if [ -z "${E2E_VISIBLE:-}" ]; then
  echo "e2e: xvfb-run is not installed, so the test windows open on your display (sudo apt-get install -y xvfb)" >&2
fi
exec "${run[@]}"
