#!/usr/bin/env bash
# Builds the Windows installer and zip on the Windows side of a WSL machine, natively, with the Node and pnpm on the
# Windows PATH (nvm-windows: see the error it prints if they are missing). The committed source of desktop/ is copied
# to %USERPROFILE%\agent-mesh-build\src (never built from a \\wsl$ path), `pnpm run pack` runs there, and the results are
# copied to the Downloads folder. No admin prompt.
#
#   desktop/scripts/build-windows.sh            # from the repo, in WSL: builds what is committed (HEAD)
#
# SOURCE_REPO builds another checkout's HEAD. Not code-signed: SmartScreen warns on the first run.
set -euo pipefail

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

cat > "$root/build.ps1" <<'PS'
$ErrorActionPreference = 'Continue'
# A process started from WSL keeps the Windows environment WSL began with, which lacks what was installed since (nvm's
# PATH entries): rebuild PATH the way a new terminal gets it, from the registry.
foreach ($n in 'NVM_HOME', 'NVM_SYMLINK') {
  $v = [Environment]::GetEnvironmentVariable($n, 'User')
  if ($v) { Set-Item "env:$n" $v }
}
$env:Path = [Environment]::ExpandEnvironmentVariables([Environment]::GetEnvironmentVariable('Path', 'Machine') + ';' + [Environment]::GetEnvironmentVariable('Path', 'User'))
foreach ($tool in 'node', 'pnpm') {
  if (-not (Get-Command $tool -ErrorAction SilentlyContinue)) {
    Write-Error "$tool is not on the Windows PATH. Install Node with nvm-windows (winget install -e --id CoreyButler.NVMforWindows --source winget; then, in an Administrator terminal: nvm install lts; nvm use lts; npm install -g pnpm@11)."
    exit 1
  }
}
"node " + (node -v) + ", pnpm " + (pnpm -v)
$root = Split-Path -Parent $MyInvocation.MyCommand.Path
if (Test-Path "$root\src") { Remove-Item "$root\src" -Recurse -Force }
New-Item -ItemType Directory "$root\src" | Out-Null
tar -xf "$root\src.tar" -C "$root\src"
Set-Location "$root\src\desktop"
$env:CSC_IDENTITY_AUTO_DISCOVERY = 'false'
pnpm install --frozen-lockfile
if ($LASTEXITCODE -ne 0) { exit 1 }
pnpm run pack
exit $LASTEXITCODE
PS

powershell.exe -NoProfile -ExecutionPolicy Bypass -File "$root_win\\build.ps1"

out="$root/src/desktop/release"
downloads="$profile/Downloads"
mkdir -p "$downloads"
cp -f "$out"/agent-mesh*Setup*.exe "$out"/agent-mesh-*-win.zip "$downloads"/
echo "Installer and zip are in $profile_win\\Downloads:"
ls -l "$downloads"/agent-mesh*Setup*.exe "$downloads"/agent-mesh-*-win.zip
