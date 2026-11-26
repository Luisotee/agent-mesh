// Opt-in check of the Linux package (slow: it builds the app, downloads Electron and fpm on a first run, and
// writes desktop/release/): builds the deb and fails unless its desktop entry names the `agentmesh://` handler and passes the URL (%U) to the app.
// Run: pnpm pack:check. The unit test in electron/package-config.test.ts is the
// part of this that `scripts/check.sh` runs.
import { execFileSync } from 'node:child_process'
import { readdirSync, rmSync } from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

const root = path.join(path.dirname(fileURLToPath(import.meta.url)), '..')
const run = (cmd, args, opts = {}) => execFileSync(cmd, args, { cwd: root, encoding: 'utf8', stdio: ['ignore', 'pipe', 'inherit'], ...opts })

rmSync(path.join(root, 'release'), { recursive: true, force: true })
run('node', ['scripts/make-icons.mjs', '--app', 'build/icon.png'])
run('pnpm', ['build'], { stdio: 'inherit' })
run('pnpm', ['exec', 'electron-builder', '--linux', 'deb'], { stdio: 'inherit' })

const deb = readdirSync(path.join(root, 'release')).find((f) => f.endsWith('.deb'))
if (!deb) throw new Error('electron-builder made no .deb')
const entry = run('sh', ['-c', `dpkg-deb --fsys-tarfile "release/${deb}" | tar -xO --wildcards './usr/share/applications/*.desktop'`])
if (!/^MimeType=.*x-scheme-handler\/agentmesh;/m.test(entry)) throw new Error(`the deb's desktop entry does not handle agentmesh://:\n${entry}`)
if (!/^Exec=.* %U$/m.test(entry)) throw new Error(`the deb's desktop entry passes no URL (%U) to the app:\n${entry}`)
console.log(`${deb}: its desktop entry handles agentmesh://`)
