#!/usr/bin/env bash
# Captures the hook payloads of a real agy CLI session, scrubbed, as the agy adapter's fixtures:
# internal/adapter/agy/testdata/<agy version>/. Run it BY HAND when a new agy version should be verified;
# never from a test or CI. It runs the real `agy` through its own meshd and mesh-term, on a temp state dir,
# in a temp git repo, for short turns on the light tier (flash).
#
#   daemon/scripts/capture-agy-hooks.sh [--agy <path>]
#
# Read the diff of the fixtures before committing them: the scrubber refuses a payload that still names the
# user, the host or an email.
set -euo pipefail

daemon_dir=$(cd "$(dirname "$0")/.." && pwd)
export PATH="$PATH:$HOME/.local/go/bin"
agy=agy
if [ "${1:-}" = "--agy" ]; then agy=$2; fi
command -v "$agy" >/dev/null || { echo "no agy on PATH (or --agy <path>)" >&2; exit 1; }
agy_path=$(command -v "$agy")

root=$(mktemp -d /tmp/capture-agy-XXXXXX)
meshd_pid=
cleanup() {
  [ -n "$meshd_pid" ] && kill "$meshd_pid" 2>/dev/null || true
  pkill -f "$root/state" 2>/dev/null || true
  rm -rf "$root"
}
trap cleanup EXIT

echo "building into $root/bin"
(cd "$daemon_dir" && CGO_ENABLED=0 go build -o "$root/bin/" ./cmd/meshd ./cmd/mesh-term ./cmd/mesh ./internal/testutil/capturehooks)

repo="$root/repo"
mkdir -p "$repo" "$root/capture"
git -C "$repo" init -q -b main
printf '# Capture repo\n\nA repository for recording an agy session.\n' > "$repo/README.md"
git -C "$repo" add README.md
git -C "$repo" -c user.name=capture -c user.email=capture@example.invalid commit -q -m "init"

state="$root/state"
"$root/bin/meshd" --state "$state" --term-launch detached --max-lifetime 30m --skip-permissions \
  --mesh "$root/bin/mesh" --term-binary "$root/bin/mesh-term" --cli "agy=$agy_path" \
  --agent-env "PATH=$root/bin:$PATH" --agent-env "AGENT_MESH_HOOK_CAPTURE=$root/capture" >"$root/meshd.log" 2>&1 &
meshd_pid=$!
for _ in $(seq 1 100); do [ -f "$state/meshd.json" ] && break; sleep 0.1; done
[ -f "$state/meshd.json" ] || { cat "$root/meshd.log" >&2; echo "meshd did not start" >&2; exit 1; }

"$root/bin/capturehooks" --kind agy --state "$state" --capture "$root/capture" --repo "$repo" \
  --out "$daemon_dir/internal/adapter/agy/testdata" --agy "$agy_path" || { echo "--- meshd log" >&2; tail -20 "$root/meshd.log" >&2; exit 1; }
