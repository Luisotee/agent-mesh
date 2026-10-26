#!/usr/bin/env bash
# Install (or --uninstall) agent-mesh for the current user by symlinking this
# checkout into place and adding a marked block to both global rule files.
set -euo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BIN_DIR="$HOME/.local/bin"
CONFIG_LINK="$HOME/.config/agent-mesh"
SKILL_ROOTS=("$HOME/.claude/skills" "$HOME/.gemini/config/skills")
RULE_FILES=("$HOME/.claude/CLAUDE.md" "$HOME/.gemini/AGENTS.md")
SKILLS=(delegate-claude delegate-agy delegate-review check-plan find-peers harvest-peers)
RETIRED_SKILLS=(delegate-claude-agy)  # removed in 0.3; unlinked on install and uninstall
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

if [[ "${1:-}" == "--uninstall" ]]; then
  unlink_if_ours "$BIN_DIR/mesh"
  unlink_if_ours "$CONFIG_LINK"
  for root in "${SKILL_ROOTS[@]}"; do
    for skill in "${SKILLS[@]}" "${RETIRED_SKILLS[@]}"; do unlink_if_ours "$root/$skill"; done
  done
  for file in "${RULE_FILES[@]}"; do set_block "$file" 0; done
  echo "agent-mesh uninstalled (run state in ~/.local/state/agent-mesh is kept)"
  exit 0
fi

command -v herdr >/dev/null || echo "warning: herdr is not on PATH; mesh needs it at runtime" >&2
chmod +x "$REPO/bin/mesh"
link "$REPO/bin/mesh" "$BIN_DIR/mesh"
link "$REPO/protocol" "$CONFIG_LINK"
for root in "${SKILL_ROOTS[@]}"; do
  for skill in "${RETIRED_SKILLS[@]}"; do unlink_if_ours "$root/$skill"; done
  for skill in "${SKILLS[@]}"; do link "$REPO/skills/$skill" "$root/$skill"; done
done
for file in "${RULE_FILES[@]}"; do set_block "$file" 1; done
echo "agent-mesh installed. Try: mesh --version"
