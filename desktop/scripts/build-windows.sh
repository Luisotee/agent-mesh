#!/usr/bin/env bash
# Builds the Windows installer and zip on the Windows side of a WSL machine, natively: nothing is installed system
# wide and no admin prompt is needed. A portable Node (checked against nodejs.org's SHA-256) and pnpm go under
# %USERPROFILE%\agent-mesh-build\tools, the committed source of desktop/ is copied to a folder there (never built from
# a \\wsl$ path), `pnpm run pack` runs there, and the results are copied to the Downloads folder.
#
#   desktop/scripts/build-windows.sh            # from the repo, in WSL: builds what is committed (HEAD)
#
# NODE_VERSION (default v24.20.0) picks the Node release; SOURCE_REPO builds another checkout's HEAD. Not code-signed: SmartScreen warns on the first run.
set -euo pipefail

NODE_VERSION="${NODE_VERSION:-v24.20.0}"
repo="${SOURCE_REPO:-$(git -C "$(dirname "$0")" rev-parse --show-toplevel)}"
command -v powershell.exe >/dev/null || { echo "powershell.exe not found: run this in WSL" >&2; exit 1; }

profile_win="$(cmd.exe /c 'echo %USERPROFILE%' 2>/dev/null | tr -d '\r')"
profile="$(wslpath "$profile_win")"
root_win="$profile_win\\agent-mesh-build"
root="$profile/agent-mesh-build"
mkdir -p "$root"

# What is built is what is committed: a dirty tree would be built without its changes.
if [ -n "$(git -C "$repo" status --porcelain -- desktop)" ]; then
  echo "note: desktop/ has uncommitted changes, which are not in the build" >&2
fi
git -C "$repo" archive --format=tar -o "$root/src.tar" HEAD desktop

cat > "$root/build.ps1" <<PS
\$ErrorActionPreference = 'Continue'
\$ver = '$NODE_VERSION'
\$root = '$root_win'
\$tools = Join-Path \$root 'tools'
\$node = "\$tools\\node-\$ver-win-x64"
New-Item -ItemType Directory -Force \$tools | Out-Null
if (-not (Test-Path "\$node\\node.exe")) {
  \$zip = "\$tools\\node-\$ver-win-x64.zip"
  Invoke-WebRequest "https://nodejs.org/dist/\$ver/node-\$ver-win-x64.zip" -OutFile \$zip -UseBasicParsing
  \$sums = (Invoke-WebRequest "https://nodejs.org/dist/\$ver/SHASUMS256.txt" -UseBasicParsing).Content
  \$want = ((\$sums -split "\`n" | Where-Object { \$_ -match "node-\$ver-win-x64.zip" } | Select-Object -First 1) -split ' ')[0]
  \$got = (Get-FileHash \$zip -Algorithm SHA256).Hash.ToLower()
  if (\$want -ne \$got) { throw "Node's checksum does not match: \$want vs \$got" }
  Expand-Archive \$zip -DestinationPath \$tools -Force
}
\$env:Path = "\$node;\$tools\\pnpm\\node_modules\\.bin;\$env:Path"
if (-not (Test-Path "\$tools\\pnpm\\node_modules\\.bin\\pnpm.cmd")) {
  New-Item -ItemType Directory -Force "\$tools\\pnpm" | Out-Null
  & "\$node\\npm.cmd" install --prefix "\$tools\\pnpm" pnpm@11 2>&1 | Out-Null
}
if (Test-Path "\$root\\src") { Remove-Item "\$root\\src" -Recurse -Force }
New-Item -ItemType Directory "\$root\\src" | Out-Null
tar -xf "\$root\\src.tar" -C "\$root\\src"
Set-Location "\$root\\src\\desktop"
\$env:CSC_IDENTITY_AUTO_DISCOVERY = 'false'
pnpm install --frozen-lockfile
if (\$LASTEXITCODE -ne 0) { exit 1 }
pnpm run pack
exit \$LASTEXITCODE
PS

powershell.exe -NoProfile -ExecutionPolicy Bypass -File "$root_win\\build.ps1"

out="$root/src/desktop/release"
downloads="$profile/Downloads"
mkdir -p "$downloads"
cp -f "$out"/agent-mesh*Setup*.exe "$out"/agent-mesh-*-win.zip "$downloads"/
echo "Installer and zip are in $profile_win\\Downloads:"
ls -l "$downloads"/agent-mesh*Setup*.exe "$downloads"/agent-mesh-*-win.zip
