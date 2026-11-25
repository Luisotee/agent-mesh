#!/usr/bin/env bash
# Regenerates app/src/gen from ../proto (run after a .proto change); needs buf (scripts/env.sh finds it).
set -euo pipefail
cd "$(dirname "$0")/.."
source scripts/env.sh
./gradlew -q --no-daemon writeCodegenTools
(cd ../proto && buf generate --template ../android/buf.gen.yaml --exclude-path agentmesh/adapter)
