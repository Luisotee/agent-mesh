#!/usr/bin/env bash
# A host-wide queue for the gates (daemon/, desktop/ and android/ `scripts/check.sh`): a gate starts its heavy part at
# once when the host has the memory for it, and waits when it has not, however many workers and roots start one. Eleven
# gates at once (load 25 on 12 cores, memory pressure over the daemon's `memory_pressure_max`) made vitest and gradle
# time out and the daemon refuse to start agents; but the same suites on the same 3 pinned cores finish no sooner two at
# a time than four at a time while memory has room: they wait on I/O and sleeps more than they compute. So the queue
# limits memory, not the number of gates.
#
# A check script sources this file and calls `gate_slot <part>` after its cheap steps (generated code, formatting: a
# gate that would fail on those never waits) and before the heavy ones. The slot is a `flock` on a file of the slot dir,
# held on fd 9 until the script's process, and anything it started that still holds the fd, is gone: the kernel lets it
# go when the script ends, however it ends (SIGKILL included), so a slot can't go stale.
#
#   source "$repo/scripts/gate-slot.sh"; gate_slot daemon
#   scripts/gate-slot.sh status        # who runs, who waits, and the memory the next admission would use
#
# A gate is admitted when no gate runs, or when all of these hold: it is the oldest waiter (first come, first served: a big
# Android gate is not overtaken forever by small ones); fewer than GATE_SLOTS_MAX gates run; the memory available, less
# what the running gates are still expected to grow by (their need minus what their process tree holds now), is at least
# its own need; and the host's memory pressure (`/proc/pressure/memory`, `full avg10`) is under GATE_PSI_MAX. The daemon's
# launch guard (daemon/internal/daemon/guard.go) reads the same two signals.
#
# Why a script and not a daemon RPC: a gate is run by hand, by a worker's shell, by a root and by a reviewer, with or
# without meshd (a plain checkout, a dev meshd, a machine with none), and the kernel's lock is released by any exit, a
# SIGKILL included: a daemon-owned queue would need leases, heartbeats and a way for a killed gate to give its slot back,
# and the gates would need a daemon to run at all.
#
# When a gate starts to wait and the caller is an agent of the daemon (AGENT_MESH_* in the environment when this file is
# sourced: daemon/scripts/check.sh sources it before it unsets them), it declares the wait to its parent once
# (`mesh send parent --type wait --until 60m`, PROTOCOL.md "Waiting on purpose"), so the daemon does not nudge the worker.
#
# Settings (environment):
#   GATE_SLOTS_MAX   most gates running at once, a backstop; default 6; 0 turns the queue off
#   GATE_NEED_MB_<PART>  memory a gate of the part (DAEMON, DESKTOP, ANDROID) is expected to peak at, in MB; defaults
#                    1500, 1500 and 2500 (measured: a daemon gate about 0.8 GB per test run, the largest process 0.6 GB;
#                    an Android gate 1.0 to 2.2 GB of Gradle and test JVMs; desktop estimated)
#   GATE_PSI_MAX     the memory pressure (percent, `full avg10`) from which no further gate is admitted; default 5
#   GATE_DIR         the slot dir; default $XDG_RUNTIME_DIR/agent-mesh-gates, else /tmp/agent-mesh-gates-<uid>
#   GATE_WAIT_MAX    seconds to wait before giving up with exit 75; default 0 (as long as it takes)
#   GATE_POLL        seconds between tries; default 5 (a test knob)
#   GATE_MEMINFO, GATE_PSI   the files read for the memory numbers (test knobs; /proc/meminfo, /proc/pressure/memory)

# The agent's identity, kept for the wait message: a gate script unsets AGENT_MESH_* for its tests.
if [ -z "${_GATE_MESH_CAPTURED:-}" ]; then
  _GATE_MESH_CAPTURED=1
  _GATE_MESH_ENV=()
  while IFS= read -r _gate_n; do
    [ -n "$_gate_n" ] && _GATE_MESH_ENV+=("$_gate_n=${!_gate_n}")
  done < <(compgen -e | grep '^AGENT_MESH_' || true)
  unset _gate_n
fi

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

# gate_whole NAME DEFAULT: the value of the variable NAME as a decimal whole number, else DEFAULT (with a warning).
gate_whole() {
  local v=${!1:-}
  case $v in
    '') v=$2 ;;
    *[!0-9]*) echo "$1=$v is not a whole number: using $2" >&2; v=$2 ;;
  esac
  printf '%s\n' "$((10#$v))"
}

gate_slots_max() { gate_whole GATE_SLOTS_MAX 6; }

gate_need_mb() {
  local def=1500 var
  [ "$1" != android ] || def=2500
  var=GATE_NEED_MB_$(printf '%s' "$1" | tr '[:lower:]-' '[:upper:]_')
  gate_whole "$var" "$def"
}

# gate_avail_mb prints MemAvailable in MB; nothing when it can't be read.
gate_avail_mb() {
  awk '/^MemAvailable:/ { print int($2 / 1024) }' "${GATE_MEMINFO:-/proc/meminfo}" 2>/dev/null || true
}

# gate_psi prints the `full avg10` of the memory pressure (0 when it can't be read).
gate_psi() {
  awk '/^full/ { for (i = 1; i <= NF; i++) if ($i ~ /^avg10=/) { sub("avg10=", "", $i); print $i; found = 1 } }
       END { if (!found) print 0 }' "${GATE_PSI:-/proc/pressure/memory}" 2>/dev/null || echo 0
}

# gate_starttime PID prints the start time (clock ticks since boot) of a live process: with the PID it names it, so a
# reused PID is not taken for the one that wrote a slot or a ticket.
gate_starttime() {
  local s
  s=$(cat "/proc/$1/stat" 2>/dev/null) || return 1
  s=${s##*) } # after the command name, which may hold spaces
  set -- $s
  printf '%s\n' "${20:-}"
}

# gate_holders DIR MAX prints the gates that hold a slot, one per line: "slot pid need seconds-running part@worktree".
gate_holders() {
  local dir=$1 max=$2 i f now epoch pid need name wt
  now=$(date +%s)
  for ((i = 0; i < max; i++)); do
    f=$dir/slot.$i
    [ -s "$f" ] || continue
    # A slot nobody holds keeps its last holder's line: it is free when the lock can be taken.
    if flock -n "$f" true 2>/dev/null; then continue; fi
    read -r epoch pid need name wt <"$f" || continue
    printf '%s %s %s %s %s\n' "$i" "${pid:-0}" "${need:-0}" "$((now - ${epoch:-$now}))" "${name:-?}@$(basename "${wt:-?}")"
  done
}

# gate_tickets DIR prints the live waiters, oldest first: "ticket pid seconds-waiting part@worktree". A ticket whose
# process is gone (a killed waiter) is removed.
gate_tickets() {
  local dir=$1 f ns pid st name wt now
  now=$(date +%s)
  for f in "$dir"/wait.*; do
    [ -e "$f" ] || continue
    read -r ns pid st name wt <"$f" || continue
    if [ -z "${pid:-}" ] || [ "$(gate_starttime "$pid" 2>/dev/null)" != "${st:-x}" ]; then
      rm -f "$f"
      continue
    fi
    printf '%s %s %s %s\n' "$ns" "$pid" "$((now - ns / 1000000000))" "${name:-?}@$(basename "${wt:-?}")"
  done | sort -k1,1n -k2,2n
}

# gate_reserve_mb HOLDERS prints how much the running gates are still expected to grow by: for each, its need less the
# resident memory of its process tree now (one `ps`), never below 0.
gate_reserve_mb() {
  local spec
  spec=$(awk '{ printf "%s:%s ", $2, $3 }' <<<"$1")
  { ps -e -o pid=,ppid=,rss= 2>/dev/null || true; } | awk -v spec="$spec" '
    { ppid[$1] = $2; rss[$1] = $3 }
    END {
      n = split(spec, h, " "); total = 0
      for (k = 1; k <= n; k++) {
        split(h[k], a, ":"); sum = 0
        for (p in rss) {
          q = p
          for (d = 0; q != "" && q != 0 && d < 64; d++) { if (q == a[1]) { sum += rss[p]; break } q = ppid[q] }
        }
        left = a[2] - sum / 1024
        if (left > 0) total += left
      }
      printf "%d\n", total
    }'
}

# gate_try_locked DIR MAX NEED PART: with the admission lock held, takes a slot (on fd 9) or sets gate_why and fails.
gate_try_locked() {
  local dir=$1 max=$2 need=$3 part=$4 holders nheld free i avail reserve psi ahead
  gate_why=
  # Nothing started here keeps the admission lock (fd 8) open: a helper that hangs must not stall every gate.
  ahead=$(gate_tickets "$dir" 8>&- | awk -v me="$$" '$2 == me { exit } { n++ } END { print n + 0 }' || true)
  if [ "$ahead" -gt 0 ]; then
    gate_why="$ahead waiting before it"
    return 1
  fi
  holders=$(gate_holders "$dir" "$max" 8>&- || true)
  nheld=0
  [ -z "$holders" ] || nheld=$(grep -c . <<<"$holders")
  free=-1
  for ((i = 0; i < max; i++)); do
    if ! awk -v s="$i" '$1 == s { f = 1 } END { exit !f }' <<<"$holders"; then free=$i; break; fi
  done
  if [ "$nheld" -gt 0 ]; then # the first gate is always admitted
    if [ "$free" -lt 0 ]; then
      gate_why="all $max slots busy"
      return 1
    fi
    psi=$(gate_psi 8>&-)
    if ! awk -v a="$psi" -v m="$(gate_whole GATE_PSI_MAX 5)" 'BEGIN { exit !(a + 0 < m + 0) }'; then
      gate_why="memory pressure ${psi}% (the limit is $(gate_whole GATE_PSI_MAX 5)%)"
      return 1
    fi
    avail=$(gate_avail_mb 8>&-)
    if [ -n "$avail" ]; then
      reserve=$(gate_reserve_mb "$holders" 8>&-)
      if [ "$((avail - reserve))" -lt "$need" ]; then
        gate_why="${avail} MB available less ${reserve} MB the running gates still need; this gate needs ${need} MB"
        return 1
      fi
    fi
  fi
  exec 9>>"$dir/slot.$free" || return 1
  if ! flock -n 9; then
    exec 9>&-
    gate_why="slot $free was taken"
    return 1
  fi
  # the holder, for the others' messages and the reserve: when, its PID, its need, which gate, whose worktree
  printf '%s %s %s %s %s\n' "$(date +%s)" "$$" "$need" "$part" "$(git rev-parse --show-toplevel 2>/dev/null || echo "$PWD")" >"$dir/slot.$free"
  gate_slot_index=$free
  return 0
}

# gate_try DIR MAX NEED PART: one try, serialized with the other gates' tries so two don't both take the last room.
gate_try() {
  local rc
  exec 8>>"$1/admit.lock" || { gate_why="cannot open the admission lock"; return 1; }
  if ! flock -w 30 8; then
    exec 8>&-
    gate_why="the admission lock is busy"
    return 1
  fi
  gate_try_locked "$@" && rc=0 || rc=$?
  exec 8>&-
  return "$rc"
}

# gate_declare_wait PART: tell the parent this agent waits for a long job, once. Nothing when the caller has no identity.
gate_declare_wait() {
  [ "${#_GATE_MESH_ENV[@]}" -gt 0 ] || return 0
  command -v mesh >/dev/null 2>&1 || return 0
  env "${_GATE_MESH_ENV[@]}" timeout 20 mesh send parent --type wait --until 60m \
    --body "$1 gate waiting for a gate slot (scripts/gate-slot.sh status)" >/dev/null 2>&1 || true
}

gate_slot() {
  local part=${1:-gate} max dir poll wait_max need start now last_msg=0 waited holders ticket declared=0 st
  [ -z "${GATE_SLOT_HELD:-}" ] || return 0 # a gate run by a gate (it already has its slot)
  max=$(gate_slots_max)
  [ "$max" -gt 0 ] || return 0
  dir=$(gate_dir)
  poll=${GATE_POLL:-5}
  wait_max=${GATE_WAIT_MAX:-0}
  mkdir -p "$dir" || { echo "gate slot: cannot make $dir: running without a slot" >&2; return 0; }
  need=$(gate_need_mb "$part")
  start=$(date +%s)
  # The ticket gives the order: the oldest live one may take a slot. Written whole, then moved into place.
  ticket=$dir/wait.$$
  st=$(gate_starttime "$$" || true)
  printf '%s %s %s %s %s\n' "$(date +%s%N)" "$$" "$st" "$part" "$(git rev-parse --show-toplevel 2>/dev/null || echo "$PWD")" >"$dir/.wait.$$"
  mv -f "$dir/.wait.$$" "$ticket"
  while :; do
    if gate_try "$dir" "$max" "$need" "$part"; then
      rm -f "$ticket"
      waited=$(($(date +%s) - start))
      export GATE_SLOT_HELD=$dir/slot.$gate_slot_index
      if [ "$waited" -ge 2 ]; then
        printf '\n== gate slot taken after waiting %ds\n' "$waited"
      fi
      return 0
    fi
    now=$(date +%s)
    if [ "$wait_max" -gt 0 ] && [ "$((now - start))" -ge "$wait_max" ]; then
      rm -f "$ticket"
      echo "gate slot: no room in ${wait_max}s (GATE_WAIT_MAX): giving up" >&2
      return 75
    fi
    if [ "$last_msg" -eq 0 ] || [ "$((now - last_msg))" -ge 120 ]; then
      holders=$(gate_holders "$dir" "$max" | awk '{ printf "%s%s for %dm", sep, $5, $4 / 60; sep = ", " }')
      printf '\n== waiting for a gate slot (%s); running: %s. %s checks again every %ss.\n' \
        "${gate_why:-no reason}" "${holders:-none}" "$part" "$poll"
      last_msg=$now
    fi
    if [ "$declared" -eq 0 ]; then
      declared=1
      gate_declare_wait "$part"
    fi
    sleep "$poll" &
    wait $! || true # `wait`, not a foreground sleep: a TERM to the script then acts at once
  done
}

# Run, not sourced: `gate-slot.sh status`.
if [ "${BASH_SOURCE[0]}" = "$0" ]; then
  case ${1:-status} in
    status)
      max=$(gate_slots_max)
      dir=$(gate_dir)
      echo "gate queue: up to $max at once (GATE_SLOTS_MAX), dir $dir"
      holders=$(gate_holders "$dir" "$max")
      if [ -n "$holders" ]; then
        awk '{ printf "  running: slot %d %s for %dm (needs %d MB)\n", $1, $5, $4 / 60, $3 }' <<<"$holders"
      else
        echo "  running: none"
      fi
      tickets=$(gate_tickets "$dir")
      if [ -n "$tickets" ]; then
        awk '{ printf "  waiting: %s for %dm\n", $4, $3 / 60 }' <<<"$tickets"
      else
        echo "  waiting: none"
      fi
      reserve=0
      [ -z "$holders" ] || reserve=$(gate_reserve_mb "$holders")
      echo "  memory: $(gate_avail_mb) MB available, $reserve MB still expected from the running gates, pressure $(gate_psi)% (limit $(gate_whole GATE_PSI_MAX 5)%)"
      ;;
    *) echo "usage: gate-slot.sh status" >&2; exit 2 ;;
  esac
fi
