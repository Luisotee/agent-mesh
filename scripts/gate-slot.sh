#!/usr/bin/env bash
# A host-wide queue for the gates (daemon/, desktop/ and android/ `scripts/check.sh`): at most GATE_SLOTS of them do
# their heavy part at once, however many workers and roots start one. Eleven gates at once (load 25 on 12 cores, memory
# pressure over the daemon's `memory_pressure_max`) were each slower than the same eleven run two at a time, and made
# vitest and gradle time out and the daemon refuse to start agents.
#
# A check script sources this file and calls `gate_slot <name>` after its cheap steps (generated code, formatting: a gate
# that would fail on those never waits) and before the heavy ones. The slot is a `flock` on a file of the slot dir, held
# on fd 9 until the script's process, and anything it started that still holds the fd, is gone: the kernel lets it go
# when the script ends, however it ends (SIGKILL included), so a slot can't go stale.
#
#   source "$repo/scripts/gate-slot.sh"; gate_slot daemon
#   scripts/gate-slot.sh status        # who holds which slot
#
# Why a script and not a daemon RPC: a gate is run by hand, by a worker's shell, by a root and by a reviewer, with or
# without meshd (a plain checkout, a dev meshd, a machine with none), and the kernel's lock is released by any exit, a
# SIGKILL included: a daemon-owned queue would need leases, heartbeats and a way for a killed gate to give its slot back,
# and the gates would need a daemon to run at all.
#
# Settings (environment):
#   GATE_SLOTS     how many gates run at once; default the smaller of cores/6 and GB of RAM/6, at least 1 (2 on 12 cores
#                  and 16 GB); 0 turns the queue off
#   GATE_DIR       the slot dir; default $XDG_RUNTIME_DIR/agent-mesh-gates, else /tmp/agent-mesh-gates-<uid>
#   GATE_WAIT_MAX  seconds to wait for a slot before giving up with exit 75; default 0 (as long as it takes)
#   GATE_POLL      seconds between tries; default 5 (a test knob)
# A gate that waits longer than a few minutes is a long job its worker started: declare it
# (`mesh send parent --type wait --until 45m`, PROTOCOL.md "Waiting on purpose"), or the daemon nudges the worker.

gate_dir() {
  local d=${GATE_DIR:-}
  if [ -z "$d" ]; then
    if [ -n "${XDG_RUNTIME_DIR:-}" ] && [ -d "$XDG_RUNTIME_DIR" ]; then
      d=$XDG_RUNTIME_DIR/agent-mesh-gates
    else
      d=/tmp/agent-mesh-gates-$(id -u)
    fi
  fi
  printf '%s\n' "$d"
}

gate_slots() {
  local n=${GATE_SLOTS:-} cores mem
  case $n in
    '') ;;
    *[!0-9]*) echo "GATE_SLOTS=$n is not a whole number: using the default" >&2; n= ;;
  esac
  if [ -z "$n" ]; then
    cores=$(getconf _NPROCESSORS_ONLN 2>/dev/null || echo 6) # not nproc: an agent is pinned to a few cores
    mem=$(awk '/^MemTotal:/ { print int($2 / 1048576) }' /proc/meminfo 2>/dev/null || true)
    mem=${mem:-12}
    n=$((cores / 6))
    [ "$((mem / 6))" -lt "$n" ] && n=$((mem / 6))
    [ "$n" -ge 1 ] || n=1
  fi
  printf '%s\n' "$((10#$n))"
}

# gate_holders prints who holds each slot of the dir, one per line: "slot name since-seconds".
gate_holders() {
  local dir=$1 slots=$2 i f line now
  now=$(date +%s)
  for ((i = 0; i < slots; i++)); do
    f=$dir/slot.$i
    [ -s "$f" ] || continue
    # A slot nobody holds keeps its last holder's line: it is free when the lock can be taken.
    if flock -n "$f" true 2>/dev/null; then continue; fi
    line=$(head -n1 "$f" 2>/dev/null) || continue
    # line: <epoch> <name> <worktree>
    set -- $line
    printf '%s %s %s\n' "$i" "${2:-?}@$(basename "${3:-?}")" "$((now - ${1:-$now}))"
  done
}

gate_slot() {
  local name=${1:-gate} slots dir poll wait_max i f start now last_msg=0 waited holders
  [ -z "${GATE_SLOT_HELD:-}" ] || return 0 # a gate run by a gate (it already has its slot)
  slots=$(gate_slots)
  [ "$slots" -gt 0 ] || return 0
  dir=$(gate_dir)
  poll=${GATE_POLL:-5}
  wait_max=${GATE_WAIT_MAX:-0}
  mkdir -p "$dir" || { echo "gate slot: cannot make $dir: running without a slot" >&2; return 0; }
  start=$(date +%s)
  while :; do
    for ((i = 0; i < slots; i++)); do
      f=$dir/slot.$i
      exec 9>>"$f" || continue
      if flock -n 9; then
        # the holder (when, which gate, whose worktree), for the others' messages
        printf '%s %s %s\n' "$(date +%s)" "$name" "$(git rev-parse --show-toplevel 2>/dev/null || echo "$PWD")" >"$f"
        export GATE_SLOT_HELD=$f
        waited=$(($(date +%s) - start))
        if [ "$waited" -ge 2 ]; then
          printf '\n== gate slot %d of %d taken after waiting %ds\n' "$((i + 1))" "$slots" "$waited"
        fi
        return 0
      fi
    done
    exec 9>&- # this try got none: let the descriptor go
    now=$(date +%s)
    if [ "$wait_max" -gt 0 ] && [ "$((now - start))" -ge "$wait_max" ]; then
      echo "gate slot: no slot of $slots in ${wait_max}s (GATE_WAIT_MAX): giving up" >&2
      return 75
    fi
    if [ "$last_msg" -eq 0 ] || [ "$((now - last_msg))" -ge 120 ]; then
      holders=$(gate_holders "$dir" "$slots" | awk '{ printf "%s%s for %dm", sep, $2, $3 / 60; sep = ", " }')
      printf '\n== waiting for a gate slot (all %d busy: %s); %s checks again every %ss.\n' \
        "$slots" "${holders:-held by gates that just ended}" "$name" "$poll"
      printf '   A worker: declare this wait (mesh send parent --type wait --until 45m) so the daemon does not nudge you.\n'
      last_msg=$now
    fi
    sleep "$poll" &
    wait $! || true # `wait`, not a foreground sleep: a TERM to the script then acts at once
  done
}

# Run, not sourced: `gate-slot.sh status`.
if [ "${BASH_SOURCE[0]}" = "$0" ]; then
  case ${1:-status} in
    status)
      slots=$(gate_slots)
      dir=$(gate_dir)
      echo "gate slots: $slots (GATE_SLOTS), dir $dir"
      holders=$(gate_holders "$dir" "$slots")
      [ -z "$holders" ] || awk '{ printf "  slot %d: %s for %dm\n", $1, $2, $3 / 60 }' <<<"$holders"
      [ -n "$holders" ] || echo "  all free"
      ;;
    *) echo "usage: gate-slot.sh status" >&2; exit 2 ;;
  esac
fi
