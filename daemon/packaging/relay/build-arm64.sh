#!/usr/bin/env bash
# Cross-compile a static linux/arm64 mesh-relay binary for the Oracle ARM VPS.
set -euo pipefail

daemon_dir=$(cd "$(dirname "$0")/../.." && pwd)
export PATH="$PATH:$HOME/.local/go/bin"

mkdir -p "$daemon_dir/bin"
out="$daemon_dir/bin/mesh-relay-linux-arm64"
(cd "$daemon_dir" && CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags="-s -w" -o "$out" ./cmd/mesh-relay)
echo "built $out"
