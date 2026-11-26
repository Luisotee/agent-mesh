#!/usr/bin/env bash
# Install (or --uninstall) agent-mesh for the current user: builds the daemon (meshd, mesh-term and the `mesh` CLI)
# into ~/.local/lib/agent-mesh, installs and starts its systemd user units, links `mesh`, the protocol and the
# skills into place, and adds a marked block to the Claude Code and agy global rule files. Safe to re-run: it
# upgrades the daemon, refreshes the PATH its units hold, and re-points the links (an install of the old,
# herdr-based `bin/mesh` is replaced the same way).
# Claude Code gets the skills from the agent-mesh plugin instead when it is installed at user scope, or with
# --plugin (before installing it): then they are not linked into ~/.claude/skills, and our old links there are
# removed.
set -euo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BIN_DIR="$HOME/.local/bin"
CONFIG_LINK="$HOME/.config/agent-mesh"
SKILL_ROOTS=("$HOME/.claude/skills" "$HOME/.gemini/config/skills")
RULE_FILES=("$HOME/.claude/CLAUDE.md" "$HOME/.gemini/AGENTS.md")
ROOT_CLIS=(claude agy)  # the CLI each SKILL_ROOTS / RULE_FILES entry belongs to: installed only when it is on PATH
# Files an earlier install put the rules block in for CLIs the daemon has no adapter for yet (Copilot, OpenCode):
# the block is taken out again, since an agent of those CLIs can't be a daemon agent. OpenCode reads the first of
# ~/.config/opencode/AGENTS.md and ~/.claude/CLAUDE.md, so a file left with nothing but the block is removed (an
# empty one would hide the user's CLAUDE.md from it).
STALE_RULE_FILES=("${COPILOT_HOME:-$HOME/.copilot}/copilot-instructions.md")
OPENCODE_RULES="${XDG_CONFIG_HOME:-$HOME/.config}/opencode/AGENTS.md"
SKILLS=(delegate delegate-review check-plan self-review find-peers harvest-peers make-concise)
RETIRED_SKILLS=(delegate-claude-agy delegate-claude delegate-agy)  # the bin/mesh era's; unlinked on install and uninstall
INSTALLED_PLUGINS="$HOME/.claude/plugins/installed_plugins.json"  # Claude Code's record of installed plugins
HERDR_PLUGIN_ID="local.mesh-notify"  # the retired herdr plugin: unlinked if an old install left it
DAEMON_LIB="$HOME/.local/lib/agent-mesh"  # bin/ (meshd, mesh-term, mesh) and share/ (protocol, skills), copied, not linked
UNIT_DIR="${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user"
DAEMON_UNITS=(agent-mesh.service agent-mesh-term.service)
DAEMON_BINS=(meshd mesh-term mesh)  # built from daemon/cmd/<name>
BEGIN="<!-- agent-mesh:begin (managed by agent-mesh/install.sh) -->"
END="<!-- agent-mesh:end -->"

link() { # link <target> <link-path>: replace an old symlink, refuse to clobber a real file
  local target="$1" path="$2"
  mkdir -p "$(dirname "$path")"
  if [[ -L "$path" ]]; then
    rm "$path"
  elif [[ -e "$path" ]]; then
    echo "skip: $path exists and is not a symlink" >&2
    return
  fi
  ln -s "$target" "$path"
  echo "linked $path -> $target"
}

has() { command -v "$1" >/dev/null; }

# Rewrite a rule file without the marked block, optionally appending a fresh one.
set_block() { # set_block <file> <add: 0|1>
  local file="$1" add="$2"
  [[ -f "$file" ]] || { [[ "$add" == 1 ]] || return 0; mkdir -p "$(dirname "$file")"; touch "$file"; }
  if grep -qF "$BEGIN" "$file" || [[ "$add" == 1 ]]; then
    cp "$file" "$file.bak-agent-mesh"
  fi
  python3 - "$file" "$add" "$REPO/rules/global-snippet.md" "$BEGIN" "$END" <<'PY'
import sys
from pathlib import Path
file, add, snippet, begin, end = sys.argv[1:]
text = Path(file).read_text()
if begin in text and end in text:
    head, _, rest = text.partition(begin)
    _, _, tail = rest.partition(end)
    text = head.rstrip("\n") + ("\n" + tail.lstrip("\n") if tail.strip() else "\n")
if add == "1":
    text = text.rstrip("\n") + "\n\n" + begin + "\n" + Path(snippet).read_text().strip() + "\n" + end + "\n"
Path(file).write_text(text)
PY
  echo "$( [[ "$add" == 1 ]] && echo updated || echo cleaned ) $file"
}

claude_plugin_installed() { # the agent-mesh Claude Code plugin, from any marketplace, at user scope
  [[ -f "$INSTALLED_PLUGINS" ]] || return 1
  python3 - "$INSTALLED_PLUGINS" <<'PY'
import json, sys
try:
    plugins = json.load(open(sys.argv[1])).get("plugins") or {}
    found = any(key.split("@")[0] == "agent-mesh" and isinstance(entries, list)
                and any(isinstance(e, dict) and e.get("scope") == "user" for e in entries)
                for key, entries in plugins.items())
except (OSError, ValueError, AttributeError):
    found = False
sys.exit(0 if found else 1)
PY
}

# ── the daemon ──
# Builds daemon/ into $DAEMON_LIB/{bin,share}, copies the two systemd user units beside a drop-in holding the
# installing shell's PATH (so the agents' CLIs are found), and starts meshd. It is versioned by install, not by
# the checked-out branch. State is in ~/.local/state/agent-mesh-daemon.

find_go() { # the go on PATH, else the one a user-local install leaves
  if command -v go >/dev/null; then command -v go
  elif [[ -x "$HOME/.local/go/bin/go" ]]; then echo "$HOME/.local/go/bin/go"
  else return 1; fi
}

unlink_if_into() { # unlink_if_into <path> <dir>: remove a symlink that points at <dir> or into it
  local path="$1" dir="$2"
  if [[ -L "$path" && ( "$(readlink "$path")" == "$dir" || "$(readlink "$path")" == "$dir"/* ) ]]; then
    rm "$path"
    echo "removed $path"
  fi
}

same() { # same <a> <b>: identical files (without cmp, nothing counts as identical: at worst an extra note)
  has cmp && cmp -s "$1" "$2"
}

unit_path_dropin() { # a unit's drop-in: the PATH of this shell (systemd's own is too short to find claude or agy)
  local path_value="$PATH"
  path_value="${path_value//\\/\\\\}"  # backslash, % and " are escapes inside a unit's quoted Environment=
  path_value="${path_value//%/%%}"
  path_value="${path_value//\"/\\\"}"
  printf '[Service]\nEnvironment="PATH=%s"\n' "$path_value"
}

daemon_install() {
  local go stage unit created=() d
  go="$(find_go)" || { echo "install.sh: needs Go (go on PATH, or ~/.local/go/bin/go)" >&2; return 1; }
  [[ -d "$REPO/daemon/cmd" ]] || { echo "install.sh: $REPO/daemon/cmd is missing" >&2; return 1; }
  has claude || has agy || echo "warning: neither Claude Code (claude) nor agy is on PATH; the daemon needs at least one to run agents (and its units get this PATH)" >&2
  # The folders this run creates are removed again if it fails before installing anything.
  d="$DAEMON_LIB"
  while [[ ! -d "$d" ]]; do created+=("$d"); d="$(dirname "$d")"; done
  mkdir -p "$DAEMON_LIB"
  stage="$(mktemp -d "$DAEMON_LIB/.stage.XXXXXX")"
  trap 'rm -rf "$stage"; for d in "${created[@]}"; do rmdir "$d" 2>/dev/null || true; done' RETURN
  # Everything is built before anything is replaced: a failed build leaves the installed daemon as it was.
  echo "building the daemon (go build)..."
  mkdir -p "$stage/bin"
  local pkgs=()
  for b in "${DAEMON_BINS[@]}"; do pkgs+=("./cmd/$b"); done
  (cd "$REPO/daemon" && "$go" build -trimpath -o "$stage/bin/" "${pkgs[@]}") \
    || { echo "install.sh: the build failed; nothing was changed" >&2; return 1; }
  for b in "${DAEMON_BINS[@]}"; do
    [[ -x "$stage/bin/$b" ]] || { echo "install.sh: the build made no $b; nothing was changed" >&2; return 1; }
  done
  # Every fallible copy is made before the first replacement; what follows is renames.
  [[ ! -d "$REPO/daemon/share" ]] || cp -R "$REPO/daemon/share" "$stage/share" \
    || { echo "install.sh: copying daemon/share failed; nothing was changed" >&2; return 1; }
  mkdir -p "$DAEMON_LIB/bin"
  # One rename per file: a running meshd or mesh-term keeps the old inode, so an upgrade never breaks it.
  local term_changed=0 had_term=0
  [[ ! -e "$DAEMON_LIB/bin/mesh-term" ]] || had_term=1
  same "$stage/bin/mesh-term" "$DAEMON_LIB/bin/mesh-term" || term_changed=1
  for b in "${DAEMON_BINS[@]}"; do mv -f "$stage/bin/$b" "$DAEMON_LIB/bin/$b"; done
  echo "installed ${DAEMON_BINS[*]} in $DAEMON_LIB/bin"
  if [[ -d "$REPO/daemon/share" ]]; then
    rm -rf "$DAEMON_LIB/share.old"
    [[ ! -e "$DAEMON_LIB/share" ]] || mv "$DAEMON_LIB/share" "$DAEMON_LIB/share.old"
    mv "$stage/share" "$DAEMON_LIB/share"
    rm -rf "$DAEMON_LIB/share.old"
    echo "installed $DAEMON_LIB/share"
  else
    rm -rf "$DAEMON_LIB/share"  # an earlier install's share/ is stale once the checkout has none
  fi
  mkdir -p "$UNIT_DIR"
  for unit in "${DAEMON_UNITS[@]}"; do
    [[ "$unit" != agent-mesh-term.service ]] || same "$REPO/daemon/packaging/systemd/$unit" "$UNIT_DIR/$unit" || term_changed=1
    cp "$REPO/daemon/packaging/systemd/$unit" "$UNIT_DIR/$unit.tmp" && mv -f "$UNIT_DIR/$unit.tmp" "$UNIT_DIR/$unit"
    mkdir -p "$UNIT_DIR/$unit.d"
    unit_path_dropin >"$UNIT_DIR/$unit.d/path.conf.new"
    if [[ "$unit" == agent-mesh-term.service ]]; then
      same "$UNIT_DIR/$unit.d/path.conf.new" "$UNIT_DIR/$unit.d/path.conf" || term_changed=1
    fi
    mv -f "$UNIT_DIR/$unit.d/path.conf.new" "$UNIT_DIR/$unit.d/path.conf"
    echo "installed $UNIT_DIR/$unit (and its PATH drop-in)"
  done
  if has systemctl; then
    # restart, not just start: an upgrade runs the new meshd (the terminal host stays up, and its terminals with it)
    if systemctl --user daemon-reload && systemctl --user enable agent-mesh.service >/dev/null \
        && systemctl --user restart agent-mesh.service; then
      echo "meshd is running (systemctl --user status agent-mesh.service); state is in ~/.local/state/agent-mesh-daemon"
    else
      echo "warning: systemctl --user could not start agent-mesh.service (no user session?): the files are installed; start it with 'systemctl --user enable --now agent-mesh.service'" >&2
    fi
  else
    echo "note: systemctl is not on PATH: start meshd yourself (meshd --state ~/.local/state/agent-mesh-daemon)" >&2
  fi
  if [[ "$term_changed" == 1 && "$had_term" == 1 ]]; then
    echo "note: the terminal host keeps running with its old binary and PATH, so the agents' terminals survive this upgrade;" \
      "apply them with 'systemctl --user restart agent-mesh-term.service' (it ends every running agent's terminal)" >&2
  fi
  echo "daemon installed. Re-run install.sh to upgrade it or to refresh the PATH its units hold."
}


# The files an install of the bin/mesh era (or of a CLI the daemon has no adapter for) left behind: links into the
# old checkout (dangling once it is pulled), the retired skills, the Copilot and OpenCode rules blocks, `meshctl`
# and the herdr plugin. Nothing of the old run state (~/.local/state/agent-mesh, ~/.config/agent-mesh.json) is read
# or removed.
clean_old_world() {
  local root skill opencode_had_block=0 file
  for root in "${SKILL_ROOTS[@]}"; do
    for skill in "${RETIRED_SKILLS[@]}" "${SKILLS[@]}"; do unlink_if_into "$root/$skill" "$REPO/skills"; done
    for skill in "${RETIRED_SKILLS[@]}"; do unlink_if_into "$root/$skill" "$DAEMON_LIB"; done
  done
  unlink_if_into "$BIN_DIR/mesh" "$REPO/bin"
  unlink_if_into "$CONFIG_LINK" "$REPO/protocol"
  unlink_if_into "$BIN_DIR/meshctl" "$DAEMON_LIB"
  grep -qF "$BEGIN" "$OPENCODE_RULES" 2>/dev/null && opencode_had_block=1
  for file in "${STALE_RULE_FILES[@]}" "$OPENCODE_RULES"; do set_block "$file" 0; done
  if [[ "$opencode_had_block" == 1 && -f "$OPENCODE_RULES" ]] && ! grep -q '[^[:space:]]' "$OPENCODE_RULES"; then
    rm -f "$OPENCODE_RULES" && echo "removed $OPENCODE_RULES (only the block was in it)"
  fi
  if has herdr && herdr plugin list 2>/dev/null | grep -qF "$HERDR_PLUGIN_ID"; then
    herdr plugin unlink "$HERDR_PLUGIN_ID" >/dev/null && echo "unlinked herdr plugin $HERDR_PLUGIN_ID"
  fi
}

daemon_uninstall() {
  local unit b
  if has systemctl; then
    systemctl --user disable --now "${DAEMON_UNITS[@]}" >/dev/null 2>&1 || true
  fi
  for unit in "${DAEMON_UNITS[@]}"; do
    rm -f "$UNIT_DIR/$unit" "$UNIT_DIR/$unit.d/path.conf"
    rmdir "$UNIT_DIR/$unit.d" 2>/dev/null || true
  done
  if has systemctl; then systemctl --user daemon-reload || true; fi
  unlink_if_into "$BIN_DIR/meshctl" "$DAEMON_LIB"
  for b in "${DAEMON_BINS[@]}"; do rm -f "$DAEMON_LIB/bin/$b"; done
  rmdir "$DAEMON_LIB/bin" 2>/dev/null || true
  rm -rf "$DAEMON_LIB/share" "$DAEMON_LIB"/.stage.* "$DAEMON_LIB/share.old"
  rmdir "$DAEMON_LIB" 2>/dev/null || true
  echo "daemon uninstalled (its state in ~/.local/state/agent-mesh-daemon is kept)"
}

USAGE="usage: install.sh [--plugin | --uninstall]"
PLUGIN=0 UNINSTALL=0
for arg in "$@"; do
  case "$arg" in
    --plugin) PLUGIN=1 ;;
    --uninstall) UNINSTALL=1 ;;
    --daemon) ;;  # the daemon is the install now: accepted so an old command line still works
    -h|--help) echo "$USAGE"; exit 0 ;;
    *) echo "install.sh: unknown argument: $arg" >&2; echo "$USAGE" >&2; exit 2 ;;
  esac
done
if [[ "$PLUGIN" == 1 && "$UNINSTALL" == 1 ]]; then
  echo "install.sh: --plugin and --uninstall don't go together" >&2; echo "$USAGE" >&2; exit 2
fi
if [[ "$UNINSTALL" == 0 && "$PLUGIN" == 0 ]] && claude_plugin_installed; then
  PLUGIN=1
  echo "the agent-mesh Claude Code plugin is installed: its skills stay out of ~/.claude/skills"
fi

if [[ "$UNINSTALL" == 1 ]]; then
  for root in "${SKILL_ROOTS[@]}"; do
    for skill in "${SKILLS[@]}"; do unlink_if_into "$root/$skill" "$DAEMON_LIB"; done
  done
  unlink_if_into "$BIN_DIR/mesh" "$DAEMON_LIB"
  unlink_if_into "$CONFIG_LINK" "$DAEMON_LIB"
  clean_old_world
  for file in "${RULE_FILES[@]}"; do set_block "$file" 0; done
  daemon_uninstall
  echo "agent-mesh uninstalled (run state in ~/.local/state/agent-mesh-daemon, and the bin/mesh era's in ~/.local/state/agent-mesh, is kept)"
  exit 0
fi

has claude || has agy || echo "warning: neither Claude Code (claude) nor agy is on PATH; mesh needs at least one" >&2
# The daemon is built and installed first: a failed build or copy leaves every link as it was, so an install of the
# old mesh keeps working. Then the links move to it.
daemon_install || exit $?
link "$DAEMON_LIB/bin/mesh" "$BIN_DIR/mesh"
link "$DAEMON_LIB/share/protocol" "$CONFIG_LINK"
# Each CLI's skills and rules go in only when it is installed. A missing CLI only means no new links: what an earlier
# install put in place stays (the CLI may be on another shell's PATH); only --uninstall removes it. Re-run this after
# installing the other CLI. The skills are the installed copy's (daemon/share/skills), not the checkout's, so
# switching a branch here changes no agent.
for i in "${!SKILL_ROOTS[@]}"; do
  root="${SKILL_ROOTS[$i]}"
  if has "${ROOT_CLIS[$i]}"; then
    set_block "${RULE_FILES[$i]}" 1
  else
    echo "skip: ${ROOT_CLIS[$i]} is not on PATH, so nothing new goes into $root or ${RULE_FILES[$i]}"
  fi
  if [[ "$PLUGIN" == 1 && "$root" == "$HOME/.claude/skills" ]]; then
    for skill in "${SKILLS[@]}"; do unlink_if_into "$root/$skill" "$DAEMON_LIB"; done  # the plugin has them
    continue
  fi
  if has "${ROOT_CLIS[$i]}"; then
    for skill in "${SKILLS[@]}"; do link "$DAEMON_LIB/share/skills/$skill" "$root/$skill"; done
  fi
done
clean_old_world
echo "agent-mesh installed. Try: mesh version"
