#!/usr/bin/env bash
# The CI equivalent for proto/ and daemon/ (the repo runs no hosted CI): buf lint, buf breaking against main,
# generated code up to date, gofmt, go vet, go test -race, and the Python suite from the repo root.
#
#   daemon/scripts/check.sh            # everything
#   SKIP_PYTEST=1 daemon/scripts/check.sh
#
# Toolchains are looked for on PATH, then in the user-local spots docs name: ~/.local/go, ~/.local/opt/buf,
# and ~/.local/opt/zigcc/cc as the C compiler the race detector needs when the machine has none. Its scratch dirs are
# removed when it ends, however it ends (not by a SIGKILL).
set -euo pipefail

daemon_dir=$(cd "$(dirname "$0")/.." && pwd)
repo=$(cd "$daemon_dir/.." && pwd)
export PATH="$PATH:$HOME/.local/go/bin:$HOME/.local/opt/buf/bin"
if [ -z "${CC:-}" ] && ! command -v cc >/dev/null 2>&1 && [ -x "$HOME/.local/opt/zigcc/cc" ]; then
  export CC="$HOME/.local/opt/zigcc/cc"
fi
export CGO_ENABLED=1
# An agent of the daemon runs this with its own identity in the environment (AGENT_MESH_TOKEN, AGENT_MESH_STATE, …):
# the tests start daemons of their own, which would refuse that token ("unknown or revoked token").
for name in $(compgen -e | grep '^AGENT_MESH_' || true); do unset "$name"; done

# The tests and the Go toolchain write scratch dirs (state dirs, built binaries, go-build trees). They go into dirs of
# this run, removed when it ends, also when it is killed (TERM, INT) or a test run times out: /tmp is a tmpfs, and a
# gate that leaves its dirs there fills it for every other one. TMPDIR keeps the user's own value as the parent (a long
# one is the test of the suite; unset, /var/tmp); the short dir is for unix sockets (shortdir), which cannot take a long path.
# The big scratch (go-build trees, built test binaries) goes on disk, not on the /tmp tmpfs that every gate on the host
# shares: $TMPDIR when set, else /var/tmp, else /tmp.
scratch_base=${TMPDIR:-/var/tmp}
[ -d "$scratch_base" ] && [ -w "$scratch_base" ] || scratch_base=/tmp
scratch=$(mktemp -d "$scratch_base/ck.XXXXXX")
short=$(mktemp -d /tmp/cs.XXXXXX 2>/dev/null || echo "$scratch")
export TMPDIR="$scratch" GOTMPDIR="$scratch" AGENT_MESH_SHORT_TMP="$short"
# The long steps run as a job of their own session, waited for: a TERM or INT to this script (a `pkill`, a Ctrl-C)
# then stops at once instead of when `go test` ends, and takes the job's whole process group with it, tests' children
# too, before the scratch dirs go.
child=
cleanup() {
  if [ -n "$child" ]; then
    kill -TERM -- "-$child" 2>/dev/null || true
    sleep 1
    kill -KILL -- "-$child" 2>/dev/null || true
  fi
  rm -rf "$scratch" "$short"
}
trap cleanup EXIT
trap 'exit 143' TERM
trap 'exit 130' INT
run() {
  setsid "$@" &
  child=$!
  local rc=0
  wait "$child" || rc=$?
  child=
  return $rc
}

step() { printf '\n== %s\n' "$*"; }

step "buf lint"
(cd "$repo/proto" && buf lint)

step "buf breaking"
base=$(git -C "$repo" rev-parse --verify -q origin/main || git -C "$repo" rev-parse --verify -q main || true)
if [ -n "$base" ] && [ -n "$(git -C "$repo" ls-tree -d "$base" proto)" ]; then
  gitdir=$(cd "$repo" && cd "$(git rev-parse --git-common-dir)" && pwd)
  (cd "$repo/proto" && buf breaking --against "$gitdir#ref=$base,subdir=proto")
else
  echo "skipped: main has no proto/ yet"
fi

step "generated code is current"
(cd "$daemon_dir" && go tool protoc-gen-go --version >/dev/null && go tool protoc-gen-connect-go --version >/dev/null)
(cd "$repo/proto" && buf generate)
if [ -n "$(git -C "$repo" status --porcelain -- daemon/gen)" ]; then
  git -C "$repo" status --short -- daemon/gen
  echo "daemon/gen is stale: commit the output of 'cd proto && buf generate'" >&2
  exit 1
fi

step "gofmt"
unformatted=$(cd "$daemon_dir" && gofmt -l cmd internal e2e)
if [ -n "$unformatted" ]; then
  echo "$unformatted"
  echo "not gofmt'd: run 'gofmt -w' on these" >&2
  exit 1
fi

step "go vet"
(cd "$daemon_dir" && go vet ./...)

step "go test -race"
# The daemon package alone runs about 8 to 10 minutes under -race (more on a loaded host): past go test's default 10-minute timeout.
run go -C "$daemon_dir" test -race -count=1 -timeout 30m ./...

if [ -z "${SKIP_PYTEST:-}" ]; then
  step "python suite"
  run bash -c 'cd "$1" && exec uvx pytest -q tests' _ "$repo"
fi

echo
echo "check.sh: all passed"
