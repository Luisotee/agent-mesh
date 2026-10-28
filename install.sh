#!/usr/bin/env bash
# Install (or --uninstall) agent-mesh for the current user by symlinking this
# checkout into place and adding a marked block to both global rule files.
# Claude Code gets the skills from the agent-mesh plugin instead when it is
# installed at user scope, or with --plugin (before installing it): then they
# are not linked into ~/.claude/skills, and our old links there are removed.
set -euo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BIN_DIR="$HOME/.local/bin"
CONFIG_LINK="$HOME/.config/agent-mesh"
SKILL_ROOTS=("$HOME/.claude/skills" "$HOME/.gemini/config/skills")
RULE_FILES=("$HOME/.claude/CLAUDE.md" "$HOME/.gemini/AGENTS.md")
SKILLS=(delegate-claude delegate-agy delegate-review check-plan find-peers harvest-peers make-concise)
RETIRED_SKILLS=(delegate-claude-agy)  # removed in 0.3; unlinked on install and uninstall
INSTALLED_PLUGINS="$HOME/.claude/plugins/installed_plugins.json"  # Claude Code's record of installed plugins
PLUGIN_ID="local.mesh-notify"  # herdr/mesh-notify: desktop notifications without the workers' noise
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
  [[ -f "$file" ]] || { [[ "$add" == 1 ]] || return 0; touch "$file"; }
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

USAGE="usage: install.sh [--plugin | --uninstall]"
PLUGIN=0 UNINSTALL=0
for arg in "$@"; do
  case "$arg" in
    --plugin) PLUGIN=1 ;;
    --uninstall) UNINSTALL=1 ;;
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
  unlink_if_ours "$BIN_DIR/mesh"
  unlink_if_ours "$CONFIG_LINK"
  for root in "${SKILL_ROOTS[@]}"; do
    for skill in "${SKILLS[@]}" "${RETIRED_SKILLS[@]}"; do unlink_if_ours "$root/$skill"; done
  done
  for file in "${RULE_FILES[@]}"; do set_block "$file" 0; done
  if command -v herdr >/dev/null && herdr plugin list 2>/dev/null | grep -qF "$PLUGIN_ID"; then
    herdr plugin unlink "$PLUGIN_ID" >/dev/null && echo "unlinked herdr plugin $PLUGIN_ID"
  fi
  echo "agent-mesh uninstalled (run state in ~/.local/state/agent-mesh is kept)"
  exit 0
fi

command -v herdr >/dev/null || echo "warning: herdr is not on PATH; mesh needs it at runtime" >&2
chmod +x "$REPO/bin/mesh"
link "$REPO/bin/mesh" "$BIN_DIR/mesh"
link "$REPO/protocol" "$CONFIG_LINK"
for root in "${SKILL_ROOTS[@]}"; do
  for skill in "${RETIRED_SKILLS[@]}"; do unlink_if_ours "$root/$skill"; done
  if [[ "$PLUGIN" == 1 && "$root" == "$HOME/.claude/skills" ]]; then
    for skill in "${SKILLS[@]}"; do unlink_if_ours "$root/$skill"; done  # the plugin has them
    continue
  fi
  for skill in "${SKILLS[@]}"; do link "$REPO/skills/$skill" "$root/$skill"; done
done
for file in "${RULE_FILES[@]}"; do set_block "$file" 1; done
link_plugin
echo "agent-mesh installed. Try: mesh --version"
