#!/usr/bin/env bash
# Captures the hook payloads of a real Claude Code, scrubbed, as the Claude adapter's fixtures:
# internal/adapter/claude/testdata/<claude version>/. Run it BY HAND when a new Claude Code version should be
# verified; never from a test or CI. It runs the real `claude` through a meshd and mesh-term of its own, on a
# temp state dir, in a temp git repo, for a handful of short turns on the light tier (Haiku), so it uses the
# account of whoever runs it and leaves a transcript under ~/.claude/projects/. No settings file is touched: the
# hooks come per launch, through --settings, as in production.
#
#   daemon/scripts/capture-claude-hooks.sh [--claude <path>]
#
# Read the diff of the fixtures before committing them: the scrubber refuses a payload that still names the
# user, the host or an email, but it can't know what else a prompt of yours revealed.
set -euo pipefail

daemon_dir=$(cd "$(dirname "$0")/.." && pwd)
export PATH="$PATH:$HOME/.local/go/bin"
claude=claude
if [ "${1:-}" = "--claude" ]; then claude=$2; fi
command -v "$claude" >/dev/null || { echo "no claude on PATH (or --claude <path>)" >&2; exit 1; }
claude_path=$(command -v "$claude")

root=$(mktemp -d /tmp/capture-XXXXXX)
meshd_pid=
cleanup() {
  [ -n "$meshd_pid" ] && kill "$meshd_pid" 2>/dev/null || true
  # The terminal host goes with its own --max-lifetime; ask it to stop now when it is still there.
  pkill -f "$root/state" 2>/dev/null || true
  rm -rf "$root"
}
trap cleanup EXIT

echo "building into $root/bin"
(cd "$daemon_dir" && CGO_ENABLED=0 go build -o "$root/bin/" ./cmd/meshd ./cmd/mesh-term ./cmd/meshctl ./internal/testutil/capturehooks)

repo="$root/repo"
mkdir -p "$repo" "$root/capture"
git -C "$repo" init -q -b main
printf '# Capture repo\n\nA repository for recording a Claude Code session.\n' > "$repo/README.md"
git -C "$repo" add README.md
git -C "$repo" -c user.name=capture -c user.email=capture@example.invalid commit -q -m "init"

# The account's own settings may bypass permissions (defaultMode), and then nothing asks: the agent runs through a
# wrapper that passes --permission-mode default, which the command line wins over the settings with, so the
# permission prompt this records is the one every other user sees.
cat > "$root/bin/claude-asks" <<WRAP
#!/usr/bin/env bash
exec "$claude_path" --permission-mode default "\$@"
WRAP
chmod +x "$root/bin/claude-asks"

state="$root/state"
"$root/bin/meshd" --state "$state" --term-launch detached --max-lifetime 30m \
  --meshctl "$root/bin/meshctl" --term-binary "$root/bin/mesh-term" --cli "claude=$root/bin/claude-asks" \
  --agent-env "AGENT_MESH_HOOK_CAPTURE=$root/capture" >"$root/meshd.log" 2>&1 &
meshd_pid=$!
for _ in $(seq 1 100); do [ -f "$state/meshd.json" ] && break; sleep 0.1; done
[ -f "$state/meshd.json" ] || { cat "$root/meshd.log" >&2; echo "meshd did not start" >&2; exit 1; }

"$root/bin/capturehooks" --state "$state" --capture "$root/capture" --repo "$repo" \
  --out "$daemon_dir/internal/adapter/claude/testdata" --claude "$claude_path" || { echo "--- meshd log" >&2; tail -20 "$root/meshd.log" >&2; exit 1; }
