#!/usr/bin/env bash
# The CI equivalent for proto/ and daemon/ (the repo runs no hosted CI): buf lint, buf breaking against main,
# generated code up to date, gofmt, go vet, go test -race, and the Python suite from the repo root.
#
#   daemon/scripts/check.sh            # everything
#   SKIP_PYTEST=1 daemon/scripts/check.sh
#
# Toolchains are looked for on PATH, then in the user-local spots docs name: ~/.local/go, ~/.local/opt/buf,
# and ~/.local/opt/zigcc/cc as the C compiler the race detector needs when the machine has none.
set -euo pipefail

daemon_dir=$(cd "$(dirname "$0")/.." && pwd)
repo=$(cd "$daemon_dir/.." && pwd)
export PATH="$PATH:$HOME/.local/go/bin:$HOME/.local/opt/buf/bin"
if [ -z "${CC:-}" ] && ! command -v cc >/dev/null 2>&1 && [ -x "$HOME/.local/opt/zigcc/cc" ]; then
  export CC="$HOME/.local/opt/zigcc/cc"
fi
export CGO_ENABLED=1

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
unformatted=$(cd "$daemon_dir" && gofmt -l cmd internal)
if [ -n "$unformatted" ]; then
  echo "$unformatted"
  echo "not gofmt'd: run 'gofmt -w' on these" >&2
  exit 1
fi

step "go vet"
(cd "$daemon_dir" && go vet ./...)

step "go test -race"
(cd "$daemon_dir" && go test -race -count=1 ./...)

if [ -z "${SKIP_PYTEST:-}" ]; then
  step "python suite"
  (cd "$repo" && uvx pytest -q tests)
fi

echo
echo "check.sh: all passed"
