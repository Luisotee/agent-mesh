#!/usr/bin/env bash
# Install (or --uninstall) agent-mesh for the current user by symlinking this
# checkout into place and adding a marked block to both global rule files.
# `--daemon` is the separate, opt-in install of the Go daemon beside it (see daemon_install below).
# Claude Code gets the skills from the agent-mesh plugin instead when it is
# installed at user scope, or with --plugin (before installing it): then they
# are not linked into ~/.claude/skills, and our old links there are removed.
set -euo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BIN_DIR="$HOME/.local/bin"
CONFIG_LINK="$HOME/.config/agent-mesh"
SKILL_ROOTS=("$HOME/.claude/skills" "$HOME/.gemini/config/skills")
RULE_FILES=("$HOME/.claude/CLAUDE.md" "$HOME/.gemini/AGENTS.md")
ROOT_CLIS=(claude agy)  # the CLI each SKILL_ROOTS / RULE_FILES entry belongs to: installed only when it is on PATH
# CLIs that run workers but no root (no skills): only the rules block, so a pane worker knows a doorbell (design §22).
WORKER_RULE_FILES=("${COPILOT_HOME:-$HOME/.copilot}/copilot-instructions.md")  # where Copilot reads them
WORKER_CLIS=(copilot)
# OpenCode reads the first of ~/.config/opencode/AGENTS.md and ~/.claude/CLAUDE.md: the block goes into its own file
# only when that exists or CLAUDE.md lacks the block, since creating it would hide the user's CLAUDE.md from OpenCode.
OPENCODE_RULES="${XDG_CONFIG_HOME:-$HOME/.config}/opencode/AGENTS.md"
SKILLS=(delegate-claude delegate-agy delegate-review check-plan self-review find-peers harvest-peers make-concise)
RETIRED_SKILLS=(delegate-claude-agy)  # removed in 0.3; unlinked on install and uninstall
INSTALLED_PLUGINS="$HOME/.claude/plugins/installed_plugins.json"  # Claude Code's record of installed plugins
PLUGIN_ID="local.mesh-notify"  # herdr/mesh-notify: desktop notifications without the workers' noise
DAEMON_LIB="$HOME/.local/lib/agent-mesh"  # the daemon world: bin/ (meshd, mesh-term, mesh) and share/, copied, not linked
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

link_plugin() { # (re)link herdr/mesh-notify into herdr; herdr records the path, so relink after a move
  command -v herdr >/dev/null || return 0
  herdr plugin unlink "$PLUGIN_ID" >/dev/null 2>&1 || true
  if herdr plugin link "$REPO/herdr/mesh-notify" >/dev/null; then
    echo "linked herdr plugin $PLUGIN_ID -> $REPO/herdr/mesh-notify"
    grep -qE '^delivery *= *"off"' "$HOME/.config/herdr/config.toml" 2>/dev/null \
      || echo "note: set [ui.toast] delivery = \"off\" (and [ui.sound] enabled = false) in herdr's config.toml, or herdr notifies for workers too" >&2
  fi
}

skill_needs() { # skill_needs <skill>: the CLI its workers run on, or nothing (works with either)
  case "$1" in
    delegate-agy) echo agy ;;
    delegate-claude) echo claude ;;
  esac
}

has() { command -v "$1" >/dev/null; }

unlink_if_ours() {
  local path="$1"
  if [[ -L "$path" && "$(readlink "$path")" == "$REPO"/* ]]; then
    rm "$path"
    echo "removed $path"
  fi
}

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

# ── the daemon (opt-in: `install.sh --daemon`) ──
# Builds daemon/ into $DAEMON_LIB/{bin,share}, copies the two systemd user units beside a drop-in holding the
# installing shell's PATH (so the agents' CLIs are found), and starts meshd. It is versioned by install, not by
# the checked-out branch, and touches nothing of bin/mesh's world: not ~/.local/bin/mesh, ~/.config/agent-mesh,
# ~/.claude or ~/.local/state/agent-mesh. The Go CLI is also linked as ~/.local/bin/meshctl until bin/mesh is retired.

find_go() { # the go on PATH, else the one a user-local install leaves
  if command -v go >/dev/null; then command -v go
  elif [[ -x "$HOME/.local/go/bin/go" ]]; then echo "$HOME/.local/go/bin/go"
  else return 1; fi
}

unlink_if_into() { # unlink_if_into <path> <dir>: remove a symlink that points into <dir>
  local path="$1" dir="$2"
  if [[ -L "$path" && "$(readlink "$path")" == "$dir"/* ]]; then
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
  go="$(find_go)" || { echo "install.sh: --daemon needs Go (go on PATH, or ~/.local/go/bin/go)" >&2; return 1; }
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
  link "$DAEMON_LIB/bin/mesh" "$BIN_DIR/meshctl"
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
  echo "daemon installed. Re-run \`install.sh --daemon\` to upgrade it or to refresh the PATH its units hold."
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

USAGE="usage: install.sh [--plugin | --uninstall] | install.sh --daemon [--uninstall]"
PLUGIN=0 UNINSTALL=0 DAEMON=0
for arg in "$@"; do
  case "$arg" in
    --plugin) PLUGIN=1 ;;
    --uninstall) UNINSTALL=1 ;;
    --daemon) DAEMON=1 ;;
    -h|--help) echo "$USAGE"; exit 0 ;;
    *) echo "install.sh: unknown argument: $arg" >&2; echo "$USAGE" >&2; exit 2 ;;
  esac
done
if [[ "$PLUGIN" == 1 && "$UNINSTALL" == 1 ]]; then
  echo "install.sh: --plugin and --uninstall don't go together" >&2; echo "$USAGE" >&2; exit 2
fi
if [[ "$DAEMON" == 1 && "$PLUGIN" == 1 ]]; then
  echo "install.sh: --daemon and --plugin don't go together" >&2; echo "$USAGE" >&2; exit 2
fi
if [[ "$DAEMON" == 1 ]]; then  # the daemon alone: nothing of the live mesh is touched
  if [[ "$UNINSTALL" == 1 ]]; then daemon_uninstall; else daemon_install; fi
  exit $?
fi
if [[ "$UNINSTALL" == 0 && "$PLUGIN" == 0 ]] && claude_plugin_installed; then
  PLUGIN=1
  echo "the agent-mesh Claude Code plugin is installed: its skills stay out of ~/.claude/skills"
fi

if [[ "$UNINSTALL" == 1 ]]; then
  unlink_if_ours "$BIN_DIR/mesh"
  unlink_if_ours "$CONFIG_LINK"
  for root in "${SKILL_ROOTS[@]}"; do
    for skill in "${SKILLS[@]}" "${RETIRED_SKILLS[@]}"; do unlink_if_ours "$root/$skill"; done
  done
  opencode_had_block=0
  grep -qF "$BEGIN" "$OPENCODE_RULES" 2>/dev/null && opencode_had_block=1
  for file in "${RULE_FILES[@]}" "${WORKER_RULE_FILES[@]}" "$OPENCODE_RULES"; do set_block "$file" 0; done
  # An OpenCode file left with nothing but the block would, empty, still hide ~/.claude/CLAUDE.md from OpenCode
  # (one the user left empty themselves stays).
  if [[ "$opencode_had_block" == 1 && -f "$OPENCODE_RULES" ]] && ! grep -q '[^[:space:]]' "$OPENCODE_RULES"; then
    rm -f "$OPENCODE_RULES" && echo "removed $OPENCODE_RULES (only the block was in it)"
  fi
  if command -v herdr >/dev/null && herdr plugin list 2>/dev/null | grep -qF "$PLUGIN_ID"; then
    herdr plugin unlink "$PLUGIN_ID" >/dev/null && echo "unlinked herdr plugin $PLUGIN_ID"
  fi
  echo "agent-mesh uninstalled (run state in ~/.local/state/agent-mesh is kept)"
  exit 0
fi

command -v herdr >/dev/null || echo "warning: herdr is not on PATH; mesh needs it at runtime" >&2
has claude || has agy || echo "warning: neither Claude Code (claude) nor agy is on PATH; mesh needs at least one" >&2
chmod +x "$REPO/bin/mesh"
link "$REPO/bin/mesh" "$BIN_DIR/mesh"
link "$REPO/protocol" "$CONFIG_LINK"
# Each CLI's skills and rules go in only when it is installed, and a skill whose workers' CLI is missing is left
# out. A missing CLI only means no new links: what an earlier install put in place stays (the CLI may be on another
# shell's PATH); only --uninstall removes it. Re-run this after installing the other CLI.
for i in "${!SKILL_ROOTS[@]}"; do
  root="${SKILL_ROOTS[$i]}"
  for skill in "${RETIRED_SKILLS[@]}"; do unlink_if_ours "$root/$skill"; done
  if has "${ROOT_CLIS[$i]}"; then
    set_block "${RULE_FILES[$i]}" 1
  else
    echo "skip: ${ROOT_CLIS[$i]} is not on PATH, so nothing new goes into $root or ${RULE_FILES[$i]}"
  fi
  if [[ "$PLUGIN" == 1 && "$root" == "$HOME/.claude/skills" ]]; then
    for skill in "${SKILLS[@]}"; do unlink_if_ours "$root/$skill"; done  # the plugin has them
    continue
  fi
  for skill in "${SKILLS[@]}"; do
    need="$(skill_needs "$skill")"
    if has "${ROOT_CLIS[$i]}" && { [[ -z "$need" ]] || has "$need"; }; then
      link "$REPO/skills/$skill" "$root/$skill"
    fi
  done
done
for i in "${!WORKER_RULE_FILES[@]}"; do
  if has "${WORKER_CLIS[$i]}"; then
    set_block "${WORKER_RULE_FILES[$i]}" 1
  else
    echo "skip: ${WORKER_CLIS[$i]} is not on PATH, so nothing new goes into ${WORKER_RULE_FILES[$i]}"
  fi
done
if has opencode; then
  if [[ -f "$OPENCODE_RULES" ]] || ! grep -qF "$BEGIN" "$HOME/.claude/CLAUDE.md" 2>/dev/null; then
    set_block "$OPENCODE_RULES" 1
  else
    echo "OpenCode reads the block from ~/.claude/CLAUDE.md (creating $OPENCODE_RULES would hide that file from it)"
  fi
else
  echo "skip: opencode is not on PATH, so nothing new goes into $OPENCODE_RULES"
fi
link_plugin
echo "agent-mesh installed. Try: mesh --version"
