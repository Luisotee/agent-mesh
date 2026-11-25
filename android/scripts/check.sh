#!/usr/bin/env bash
# The CI equivalent for android/ (the repo runs no hosted CI): generated code up to date, JVM unit tests (the
# Compose screens under Robolectric, and the end-to-end tests against a real meshd, skipped when Go isn't found),
# lint, and a debug APK. No emulator is needed.
#
#   android/scripts/check.sh
#   SKIP_E2E=1 android/scripts/check.sh   # without the meshd end-to-end tests
#
# Toolchains come from scripts/setup.sh (the SDK, a JDK and Gradle under ~/.local/opt); nothing is global.
set -euo pipefail
cd "$(dirname "$0")/.."
repo=$(cd .. && pwd)
source scripts/env.sh
[ -d "$ANDROID_HOME/platforms" ] && command -v javac >/dev/null || { echo "run android/scripts/setup.sh first" >&2; exit 1; }

step() { printf '\n== %s\n' "$*"; }

step "generated code is current"
scripts/gen.sh
# Regenerating must change nothing against what is staged or committed, and add no file.
if ! git -C "$repo" diff --quiet -- android/app/src/gen || [ -n "$(git -C "$repo" ls-files --others --exclude-standard -- android/app/src/gen)" ]; then
  git -C "$repo" status --short -- android/app/src/gen
  echo "android/app/src/gen is stale: commit the output of android/scripts/gen.sh" >&2
  exit 1
fi

tests=(testDebugUnitTest)
[ -n "${SKIP_E2E:-}" ] && tests=(testDebugUnitTest --tests '*Test' --tests '!*DaemonE2ETest')

step "unit tests, lint, debug APK"
./gradlew --no-daemon "${tests[@]}" lintDebug assembleDebug
ls -l app/build/outputs/apk/debug/app-debug.apk

echo
echo "android/check.sh: all passed"
