import { execFile } from 'node:child_process'
import { existsSync, statSync } from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import type { HostInfo, LocalCandidate, LocalFix } from '../shared/api.js'
import { forward } from './bridge.js'
import { Hosts, toInfo, type StoredHost } from './hosts.js'
import { Dialer } from './route.js'

/**
 * Finding the daemon on this machine, for the first run. Nothing here takes a command from the renderer: it passes
 * an id (`native`, `wsl:<distro>`) and one of two fix names, and main runs fixed commands for them. On Windows the
 * daemon lives in a WSL distro: the app asks it (through `wsl.exe`) whether it runs and listens, and pairs with it the
 * way any other device does, with a one-time link that `mesh pair` made inside the distro, so the certificate pin
 * comes over a channel that is the user's own machine.
 */

export interface RunResult {
  code: number
  stdout: Buffer
}

export interface LocalDeps {
  platform: NodeJS.Platform
  home: string
  env: NodeJS.ProcessEnv
  /** Runs a program with no shell. `stdin` is written to it. Never rejects: a program that can't start is code -1. */
  run(file: string, args: string[], stdin?: string, timeoutMs?: number): Promise<RunResult>
  /** Whether a pinned TLS connection to host:port presents the certificate with this fingerprint. */
  reach(host: string, port: number, fingerprint: string): Promise<boolean>
  /** Whether a daemon answers `Hello` over a host's route. */
  hello(host: StoredHost): Promise<boolean>
  /** The `wsl.exe` to run (a test points it at a fake). */
  wslExe: string
  hostname: string
}

export function realDeps(): LocalDeps {
  const platform = (process.env.AGENT_MESH_DESKTOP_PLATFORM as NodeJS.Platform | undefined) || process.platform
  return {
    platform,
    home: os.homedir(),
    env: process.env,
    wslExe: process.env.AGENT_MESH_DESKTOP_WSL_EXE || 'wsl.exe',
    hostname: os.hostname(),
    run: (file, args, stdin, timeoutMs = 30_000) =>
      new Promise((resolve) => {
        const child = execFile(file, args, { encoding: 'buffer', timeout: timeoutMs, maxBuffer: 4 << 20, windowsHide: true }, (err, stdout) => {
          const code = err ? (typeof (err as NodeJS.ErrnoException & { code?: unknown }).code === 'number' ? ((err as unknown as { code: number }).code) : -1) : 0
          resolve({ code, stdout: stdout ?? Buffer.alloc(0) })
        })
        if (stdin !== undefined) child.stdin?.end(stdin)
      }),
    async reach(host, port, fingerprint) {
      const d = new Dialer({ hostId: 'probe', fingerprint, direct: { host, port } })
      try {
        const s = await d.connect()
        s.destroy()
        return true
      } catch {
        return false
      }
    },
    hello: (host) =>
      new Promise((resolve) => {
        let status = 0
        const abort = forward(host, { method: 'POST', path: '/agentmesh.v1.MeshService/Hello', headers: { 'content-type': 'application/json' }, body: new TextEncoder().encode('{}') }, (e) => {
          if (e.type === 'head') status = e.status
          else if (e.type === 'end') resolve(status === 200)
          else if (e.type === 'error') resolve(false)
        })
        setTimeout(() => {
          abort()
          resolve(false)
        }, 5_000).unref()
      }),
  }
}

const INSTALL_COMMANDS = ['git clone https://github.com/Luisotee/agent-mesh', 'cd agent-mesh && ./install.sh']
const LISTEN_ADDRESS = '127.0.0.1:9443'
const SENTINEL = '@@MESH '

/** What the probe script found, from its `@@MESH key=value` lines (the rest of a login shell's output is ignored). */
export function parseProbe(out: string): Record<string, string> {
  const found: Record<string, string> = {}
  for (const line of out.split(/\r?\n/)) {
    if (!line.startsWith(SENTINEL)) continue
    const rest = line.slice(SENTINEL.length)
    const i = rest.indexOf('=')
    if (i > 0) found[rest.slice(0, i)] = rest.slice(i + 1)
  }
  return found
}

/** `wsl.exe -l -q` prints UTF-16LE (a BOM, NULs and CRLFs) when run from Windows, plain text elsewhere. */
export function parseDistros(out: Buffer): string[] {
  const text = out.includes(0) ? out.toString('utf16le') : out.toString('utf8')
  return text
    .replace(/^﻿/, '')
    .split(/\r?\n/)
    .map((l) => l.replace(/\0/g, '').trim())
    .filter((l) => l && !/^docker-desktop/i.test(l))
}

/** The state dir a daemon started by the installed units uses (`mesh` finds it the same way). */
export function defaultStateDir(env: NodeJS.ProcessEnv, home: string): string {
  return path.join(env.XDG_STATE_HOME || path.join(home, '.local', 'state'), 'agent-mesh-daemon')
}

// The probe runs in the distro as a login shell fed on stdin (no quoting through wsl.exe's command line). It prints
// only `@@MESH key=value` lines; a profile's own chatter is ignored.
const PROBE = `
PATH="$HOME/.local/bin:$HOME/.local/lib/agent-mesh/bin:$PATH"
state="\${XDG_STATE_HOME:-$HOME/.local/state}/agent-mesh-daemon"
if command -v mesh >/dev/null 2>&1; then echo "${SENTINEL}installed=1"; else echo "${SENTINEL}installed=0"; fi
if command -v systemctl >/dev/null 2>&1 && [ -d /run/systemd/system ]; then echo "${SENTINEL}systemd=1"; else echo "${SENTINEL}systemd=0"; fi
if [ -f "$state/meshd.json" ]; then echo "${SENTINEL}info=$(tr -d '\\n' < "$state/meshd.json")"; fi
if h=$(mesh hello 2>/dev/null); then echo "${SENTINEL}hello=$(printf '%s' "$h" | tr -d '\\n')"; fi
if [ -e "$HOME/.config/systemd/user/agent-mesh.service.d/listen.conf" ]; then echo "${SENTINEL}listenconf=1"; fi
`

const START = `
systemctl --user enable --now agent-mesh >/dev/null 2>&1 && echo "${SENTINEL}ok=1" || echo "${SENTINEL}err=systemctl --user enable --now agent-mesh failed"
`

const ENABLE_LISTEN = `
d="$HOME/.config/systemd/user/agent-mesh.service.d"
f="$d/listen.conf"
if [ -e "$f" ]; then echo "${SENTINEL}err=$f exists already: edit it to set AGENT_MESH_LISTEN"; exit 0; fi
mkdir -p "$d" && printf '[Service]\\nEnvironment="AGENT_MESH_LISTEN=${LISTEN_ADDRESS}"\\n' > "$f" \\
  && systemctl --user daemon-reload && systemctl --user restart agent-mesh && echo "${SENTINEL}ok=1" \\
  || echo "${SENTINEL}err=could not write $f and restart agent-mesh"
`

const pairScript = (port: number) => `
PATH="$HOME/.local/bin:$HOME/.local/lib/agent-mesh/bin:$PATH"
e=$(mktemp)
if p=$(mesh pair --name Desktop --ttl-seconds 120 --direct-url https://127.0.0.1:${port} 2>"$e"); then echo "${SENTINEL}pair=$(printf '%s' "$p" | tr -d '\\n')"; else echo "${SENTINEL}err=mesh pair failed: $(tr -d '\\n' < "$e")"; fi
rm -f "$e"
`

interface Wsl {
  distro: string
  installed: boolean
  systemd: boolean
  info?: { listen?: string; cert_fingerprint?: string }
  capabilities?: string[]
  listenConf: boolean
}

function portOf(listen: string | undefined): number {
  const m = /:(\d{1,5})$/.exec(listen ?? '')
  const n = m ? Number(m[1]) : 0
  return n >= 1 && n <= 65535 ? n : 0
}

export class Local {
  private inflight?: Promise<LocalCandidate[]>

  constructor(
    private readonly hosts: Hosts,
    private readonly deps: LocalDeps = realDeps(),
  ) {}

  /** Single-flight: a second call while one runs gets its answer (a strict-mode effect, a double click). */
  detect(): Promise<LocalCandidate[]> {
    this.inflight ??= this.scan().finally(() => (this.inflight = undefined))
    return this.inflight
  }

  private scan(): Promise<LocalCandidate[]> {
    return this.deps.platform === 'win32' ? this.scanWsl() : this.scanNative()
  }

  // ── Linux and macOS: the state dir ──

  private stateDir(): string {
    return defaultStateDir(this.deps.env, this.deps.home)
  }

  private async scanNative(): Promise<LocalCandidate[]> {
    const dir = this.stateDir()
    const label = 'This computer'
    const sock = path.join(dir, 'meshd.sock')
    let isSocket = false
    try {
      isSocket = statSync(sock).isSocket()
    } catch {
      // No socket: a daemon that never ran, or one that isn't running.
    }
    if (!isSocket) {
      const ran = existsSync(dir)
      const canStart = this.deps.platform === 'linux' && ran
      return [
        {
          id: 'native',
          label,
          state: ran ? 'not-running' : 'not-installed',
          detail: ran
            ? `agent-mesh is installed but its daemon isn't running (no socket in ${dir}).`
            : `No agent-mesh daemon found (nothing in ${dir}). Install it, then look again.`,
          fixes: canStart ? ['start'] : [],
          commands: ran ? undefined : INSTALL_COMMANDS,
        },
      ]
    }
    const host: StoredHost = { id: 'probe', name: label, kind: 'state-dir', path: dir }
    if (!(await this.deps.hello(host))) {
      return [{ id: 'native', label, state: 'not-running', detail: `A daemon left its socket in ${dir} but doesn't answer: it isn't running (or this app can't read its device token).`, fixes: this.deps.platform === 'linux' ? ['start'] : [] }]
    }
    return [{ id: 'native', label, state: 'ready', detail: `A daemon is running here (${dir}).`, fixes: [] }]
  }

  // ── Windows: a daemon in a WSL distro ──

  private async distros(): Promise<{ names: string[]; wsl: boolean }> {
    const r = await this.deps.run(this.deps.wslExe, ['-l', '-q'], undefined, 15_000)
    if (r.code !== 0 && r.stdout.length === 0) return { names: [], wsl: false }
    return { names: parseDistros(r.stdout), wsl: true }
  }

  private async probe(distro: string): Promise<Wsl> {
    const r = await this.deps.run(this.deps.wslExe, ['-d', distro, '-e', 'sh', '-l', '-s'], PROBE, 30_000)
    const f = parseProbe(r.stdout.toString('utf8'))
    const w: Wsl = { distro, installed: f.installed === '1', systemd: f.systemd === '1', listenConf: f.listenconf === '1' }
    try {
      if (f.info) w.info = JSON.parse(f.info) as Wsl['info']
    } catch {
      // A meshd.json that isn't JSON: treat as absent.
    }
    try {
      if (f.hello) w.capabilities = ((JSON.parse(f.hello) as { capabilities?: unknown }).capabilities as string[] | undefined) ?? []
    } catch {
      // `mesh hello` printed something else: the daemon isn't answering properly.
    }
    return w
  }

  private async classify(w: Wsl): Promise<LocalCandidate> {
    const id = `wsl:${w.distro}`
    const label = `WSL: ${w.distro}`
    if (!w.installed) {
      return { id, label, state: 'not-installed', detail: `agent-mesh isn't installed in ${w.distro}. Run these inside it, then look again.`, fixes: [], commands: INSTALL_COMMANDS }
    }
    if (!w.capabilities) {
      return {
        id,
        label,
        state: 'not-running',
        detail: w.systemd ? `agent-mesh is installed in ${w.distro} but its daemon isn't running.` : `agent-mesh is installed in ${w.distro} but its daemon isn't running, and this distro has no systemd to start it: run meshd by hand.`,
        fixes: w.systemd ? ['start'] : [],
      }
    }
    const port = portOf(w.info?.listen)
    if (!w.capabilities.includes('tls') || port === 0) {
      return {
        id,
        label,
        state: 'no-listen',
        detail: `The daemon in ${w.distro} runs, but only for programs inside WSL: Windows can't reach it until it listens on a local address.`,
        fixes: w.systemd && !w.listenConf ? ['enable-listen'] : [],
        commands: w.systemd && !w.listenConf ? undefined : [`AGENT_MESH_LISTEN=${LISTEN_ADDRESS} (a drop-in of agent-mesh.service, then restart it)`],
      }
    }
    const fp = w.info?.cert_fingerprint ?? ''
    if (!(await this.deps.reach('127.0.0.1', port, fp))) {
      return {
        id,
        label,
        state: 'unreachable',
        detail: `The daemon in ${w.distro} listens on port ${port}, but Windows can't reach 127.0.0.1:${port} (or the certificate differs). Mirrored networking ("networkingMode=mirrored" in .wslconfig) makes WSL's local ports Windows' own.`,
        fixes: [],
      }
    }
    return { id, label, state: 'ready', detail: `The daemon in ${w.distro} is running and reachable at 127.0.0.1:${port}.`, fixes: [] }
  }

  private async scanWsl(): Promise<LocalCandidate[]> {
    const { names, wsl } = await this.distros()
    if (!wsl) {
      return [{ id: 'wsl', label: 'WSL', state: 'not-installed', detail: "Windows Subsystem for Linux isn't set up here, and the agent-mesh daemon runs in it. Install it (and a distro), or connect to a daemon on another machine.", fixes: [], commands: ['wsl --install'] }]
    }
    if (names.length === 0) {
      return [{ id: 'wsl', label: 'WSL', state: 'not-installed', detail: 'WSL has no Linux distro yet, and the agent-mesh daemon runs in one. Install one, then look again.', fixes: [], commands: ['wsl --install -d Ubuntu'] }]
    }
    return Promise.all(names.map(async (n) => this.classify(await this.probe(n))))
  }

  // ── acting ──

  private async candidate(id: string): Promise<{ list: LocalCandidate[]; found: LocalCandidate | undefined }> {
    const list = await this.detect()
    return { list, found: list.find((c) => c.id === id) }
  }

  /** Connects to a candidate the scan found ready, and saves it as a host. */
  async connect(id: string): Promise<HostInfo> {
    if (this.hosts.fixed) throw new Error('the host list is fixed by the environment')
    const { found } = await this.candidate(id)
    if (!found) throw new Error('that daemon is no longer there: look again')
    if (found.state !== 'ready') throw new Error(found.detail)
    if (id === 'native') {
      return toInfo(await this.hosts.add({ name: this.deps.hostname, kind: 'state-dir', path: this.stateDir() }))
    }
    const distro = id.slice('wsl:'.length)
    const w = await this.probe(distro)
    const port = portOf(w.info?.listen)
    if (!w.installed || port === 0) throw new Error(`the daemon in ${distro} isn't ready: look again`)
    const r = await this.deps.run(this.deps.wslExe, ['-d', distro, '-e', 'sh', '-l', '-s'], pairScript(port), 30_000)
    const f = parseProbe(r.stdout.toString('utf8'))
    if (!f.pair) throw new Error(f.err || `couldn't make a pairing link in ${distro}`)
    let link = ''
    try {
      link = (JSON.parse(f.pair) as { uri?: string }).uri ?? ''
    } catch {
      // Handled below.
    }
    if (!link) throw new Error(`${distro} answered, but not with a pairing link`)
    return toInfo(await this.hosts.add({ name: '', kind: 'pair', link, deviceName: `Desktop (${this.deps.hostname})` }))
  }

  /** Runs one of the two fixes on a candidate, waits for the daemon to settle, and scans again. */
  async fix(id: string, fix: LocalFix): Promise<LocalCandidate[]> {
    const { found } = await this.candidate(id)
    if (!found || !found.fixes.includes(fix)) throw new Error('that fix is not on offer: look again')
    const script = fix === 'start' ? START : ENABLE_LISTEN
    let r: RunResult
    if (id === 'native') {
      // Only `start` is ever offered natively.
      r = await this.deps.run('systemctl', ['--user', 'enable', '--now', 'agent-mesh'], undefined, 30_000)
      if (r.code !== 0) throw new Error('systemctl --user enable --now agent-mesh failed')
    } else {
      r = await this.deps.run(this.deps.wslExe, ['-d', id.slice('wsl:'.length), '-e', 'sh', '-l', '-s'], script, 60_000)
      const f = parseProbe(r.stdout.toString('utf8'))
      if (f.err || f.ok !== '1') throw new Error(f.err || 'the fix did not run')
    }
    // The daemon takes a moment to come up (and to bind its listen address).
    let list: LocalCandidate[] = []
    for (let i = 0; i < 12; i++) {
      list = await this.detect()
      const now = list.find((c) => c.id === id)
      if (now && now.state !== 'not-running' && !(fix === 'enable-listen' && now.state === 'no-listen')) break
      await new Promise((res) => setTimeout(res, 1_000))
    }
    return list
  }
}
