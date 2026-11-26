import { type ChildProcess, execFileSync, spawn } from 'node:child_process'
import { existsSync, mkdirSync, mkdtempSync, readFileSync, rmSync, symlinkSync, writeFileSync } from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import net from 'node:net'
import { createClient } from '@connectrpc/connect'
import { createConnectTransport } from '@connectrpc/connect-node'
import { MeshService } from '../src/gen/agentmesh/v1/mesh_pb'

/**
 * A real meshd for the end-to-end tests: built from daemon/, on a state dir of its own under the temp dir, with
 * the fake Claude CLI (the same one the daemon's tests act out), a throwaway git repo, and a HOME of its own.
 * Nothing here reads or writes the user's real state, herdr or Claude config.
 */

const root = path.resolve(import.meta.dirname, '../..')
const daemonDir = path.join(root, 'daemon')

const goBin = [process.env.GOROOT && path.join(process.env.GOROOT, 'bin'), path.join(os.homedir(), '.local/go/bin')].filter(Boolean) as string[]
const baseEnv = (): NodeJS.ProcessEnv => {
  const env: NodeJS.ProcessEnv = { ...process.env, PATH: [...goBin, process.env.PATH ?? ''].join(path.delimiter) }
  // Never aim a test at a real daemon.
  delete env.AGENT_MESH_STATE
  delete env.AGENT_MESH_TOKEN
  return env
}

/** Builds meshd, mesh-term, mesh, mesh-relay and the fake claude into `dir`. */
export function buildBinaries(dir: string): void {
  mkdirSync(dir, { recursive: true })
  const targets: Record<string, string> = {
    meshd: './cmd/meshd',
    'mesh-term': './cmd/mesh-term',
    mesh: './cmd/mesh',
    'mesh-relay': './cmd/mesh-relay',
    claude: './internal/testutil/fakeclaude',
  }
  for (const [out, pkg] of Object.entries(targets)) {
    execFileSync('go', ['build', '-o', path.join(dir, out), pkg], { cwd: daemonDir, env: { ...baseEnv(), CGO_ENABLED: '0' }, stdio: 'pipe' })
  }
}

export interface Host {
  state: string
  repo: string
  home: string
  /** Starts (or restarts) meshd on the same state dir. */
  start(): Promise<void>
  /** Stops meshd the way a service manager would; the terminal host and its agents keep running. */
  stopDaemon(): Promise<void>
  /** Stops everything and removes the temp dirs. */
  dispose(): Promise<void>
  /** A client of the host's API, as the device (the host's own token), or as an agent with its token. */
  client(token?: string): ReturnType<typeof createClient<typeof MeshService>>
  deviceToken(): string
  agentToken(id: string): string
}

/** A TCP port nobody listens on right now (the OS picks it; another process could take it before it is used). */
export async function freePort(): Promise<number> {
  const s = net.createServer()
  await new Promise<void>((r) => s.listen(0, '127.0.0.1', r))
  const port = (s.address() as net.AddressInfo).port
  await new Promise((r) => s.close(r))
  return port
}

/** A real mesh-relay (docs/relay.md) on loopback: its HTTP/WebSocket port, its raw TCP port for clients, its secret. */
export interface Relay {
  url: string
  tcp: string
  tokenFile: string
  stop(): Promise<void>
}

export async function startRelay(bins: string, dir: string): Promise<Relay> {
  mkdirSync(dir, { recursive: true })
  const tokenFile = path.join(dir, 'relay.token')
  const token = execFileSync(path.join(bins, 'mesh-relay'), ['--gen-token'], { stdio: 'pipe' }).toString().trim()
  writeFileSync(tokenFile, `${token}\n`, { mode: 0o600 })
  const http = await freePort()
  const tcp = await freePort()
  const proc = spawn(path.join(bins, 'mesh-relay'), ['--listen', `127.0.0.1:${http}`, '--client-listen', `127.0.0.1:${tcp}`, '--token-file', tokenFile, '--max-lifetime', '10m'], { stdio: ['ignore', 'ignore', 'pipe'] })
  let err = ''
  proc.stderr?.on('data', (b: Buffer) => (err += b.toString()))
  await until('mesh-relay to listen', async () => {
    if (proc.exitCode != null) throw new Error(`mesh-relay exited: ${err}`)
    return fetch(`http://127.0.0.1:${http}/healthz`).then((r) => r.ok, () => false)
  }, 10_000)
  return {
    url: `http://127.0.0.1:${http}`,
    tcp: `127.0.0.1:${tcp}`,
    tokenFile,
    async stop() {
      proc.kill('SIGTERM')
      await new Promise((r) => (proc.exitCode != null ? r(null) : proc.once('exit', r)))
    },
  }
}

/** What a host is started with besides the basics: a direct TLS listener, a relay. */
export interface HostOptions {
  /** meshd's `--listen` address (TLS, loopback in tests). */
  listen?: string
  relay?: Pick<Relay, 'url' | 'tcp' | 'tokenFile'>
  hostName?: string
  /** Gives meshd the repo's own `daemon/share` (the protocol and the setup agent's instructions). */
  share?: boolean
  /** Environment for the fake claude of this host (meshd passes agents no FAKECLAUDE_* of its own): a wrapper script sets it. */
  fakeClaudeEnv?: Record<string, string>
}

export function makeHost(bins: string, opts: HostOptions = {}): Host {
  // A unix socket path holds about 107 bytes, so the state dir is short.
  const base = mkdtempSync(path.join(os.tmpdir(), 'amd-'))
  const state = path.join(base, 's')
  const home = path.join(base, 'h')
  const repo = path.join(base, 'repo')
  const worktrees = path.join(base, 'w')
  for (const d of [state, home, repo, worktrees]) mkdirSync(d, { recursive: true })
  const git = (...args: string[]) =>
    execFileSync('git', ['-C', repo, ...args], { env: { ...baseEnv(), HOME: home, GIT_CONFIG_GLOBAL: '/dev/null', GIT_AUTHOR_NAME: 't', GIT_AUTHOR_EMAIL: 't@t', GIT_COMMITTER_NAME: 't', GIT_COMMITTER_EMAIL: 't@t' }, stdio: 'pipe' })
  git('init', '-q', '-b', 'main')
  writeFileSync(path.join(repo, 'README.md'), 'a throwaway repo\n')
  git('add', '.')
  git('commit', '-q', '-m', 'init')

  // The fake claude, or a wrapper that sets its environment first.
  let claude = path.join(bins, 'claude')
  if (opts.fakeClaudeEnv) {
    claude = path.join(base, 'claude-env')
    const exports = Object.entries(opts.fakeClaudeEnv).map(([k, v]) => `export ${k}='${v.replace(/'/g, `'\\''`)}'`).join('\n')
    writeFileSync(claude, `#!/bin/sh\n${exports}\nexec '${path.join(bins, 'claude')}' "$@"\n`, { mode: 0o755 })
  }
  let daemon: ChildProcess | undefined
  const info = path.join(state, 'meshd.json')
  const host: Host = {
    state,
    repo,
    home,
    async start() {
      rmSync(info, { force: true })
      daemon = spawn(
        path.join(bins, 'meshd'),
        ['--state', state, '--tcp', '127.0.0.1:0', '--max-lifetime', '10m', '--term-launch', 'detached',
          '--term-binary', path.join(bins, 'mesh-term'), '--mesh', path.join(bins, 'mesh'),
          '--cli', `claude=${claude}`, '--worktrees', worktrees,
          ...(opts.listen ? ['--listen', opts.listen] : []),
          ...(opts.hostName ? ['--host-name', opts.hostName] : []),
          ...(opts.share ? ['--share', path.join(daemonDir, 'share')] : []),
          ...(opts.relay ? ['--relay-url', opts.relay.url, '--relay-tcp', opts.relay.tcp, '--relay-token-file', opts.relay.tokenFile] : [])],
        { env: { ...baseEnv(), HOME: home, GIT_CONFIG_GLOBAL: '/dev/null', GIT_AUTHOR_NAME: 't', GIT_AUTHOR_EMAIL: 't@t', GIT_COMMITTER_NAME: 't', GIT_COMMITTER_EMAIL: 't@t' }, stdio: ['ignore', 'ignore', 'pipe'] },
      )
      let err = ''
      daemon.stderr?.on('data', (b: Buffer) => (err += b.toString()))
      await until('meshd to start', () => existsSync(info) || daemon?.exitCode != null, 15_000)
      if (daemon.exitCode != null) throw new Error(`meshd exited: ${err}`)
      await until('the device token', () => existsSync(path.join(state, 'device.token')), 5000)
    },
    async stopDaemon() {
      const d = daemon
      if (!d || d.exitCode != null) return
      d.kill('SIGTERM')
      await until('meshd to stop', () => d.exitCode != null, 15_000)
    },
    async dispose() {
      await host.stopDaemon()
      // The terminal host outlives meshd by design: end it, and with it the fake agents.
      try {
        const term = JSON.parse(readFileSync(path.join(state, 'term', 'term.json'), 'utf8')) as { pid: number }
        process.kill(term.pid, 'SIGTERM')
      } catch {
        // already gone
      }
      rmSync(base, { recursive: true, force: true })
    },
    deviceToken: () => readFileSync(path.join(state, 'device.token'), 'utf8').trim(),
    agentToken: (id) => readFileSync(path.join(state, 'agents', id, 'token'), 'utf8').trim(),
    client(token) {
      const tcp = (JSON.parse(readFileSync(info, 'utf8')) as { tcp: string }).tcp
      return createClient(
        MeshService,
        createConnectTransport({ baseUrl: `http://${tcp}`, httpVersion: '1.1', interceptors: [(next) => (req) => { req.header.set('authorization', `Bearer ${token ?? host.deviceToken()}`); return next(req) }] }),
      )
    },
  }
  return host
}

export async function until(what: string, fn: () => boolean | Promise<boolean>, ms = 10_000): Promise<void> {
  const end = Date.now() + ms
  while (Date.now() < end) {
    if (await fn()) return
    await new Promise((r) => setTimeout(r, 50))
  }
  throw new Error(`timed out waiting for ${what}`)
}

/**
 * A stand-in for Windows' `wsl.exe` that runs what the app asks of a distro against `host`, as a distro would: `-l -q`
 * lists one distro, `Ubuntu`, in UTF-16LE; `-d Ubuntu -e sh -l -s` runs the script on stdin with the test meshd's state dir
 * where `mesh` looks for it and the test binaries on PATH (`withMesh: false`: a distro that has no agent-mesh). A stub
 * `systemctl` is first on PATH, so no test can ever reach the user's own services; it only logs its arguments.
 */
export function fakeWsl(dir: string, bins: string, host: Host, opts: { withMesh?: boolean } = {}): { exe: string; systemctlLog: string } {
  mkdirSync(dir, { recursive: true })
  const xdg = path.join(dir, 'xdg')
  mkdirSync(xdg, { recursive: true })
  rmSync(path.join(xdg, 'agent-mesh-daemon'), { force: true })
  symlinkSync(host.state, path.join(xdg, 'agent-mesh-daemon'))
  const stubs = path.join(dir, 'stubs')
  mkdirSync(stubs, { recursive: true })
  const systemctlLog = path.join(dir, 'systemctl.log')
  writeFileSync(path.join(stubs, 'systemctl'), `#!/bin/sh\necho "$@" >> '${systemctlLog}'\n`, { mode: 0o755 })
  const exe = path.join(dir, 'wsl.exe')
  const meshPath = opts.withMesh === false ? '' : `${bins}:`
  writeFileSync(
    exe,
    `#!/bin/sh
if [ "$1" = "-l" ]; then
  printf '\\377\\376U\\000b\\000u\\000n\\000t\\000u\\000\\r\\000\\n\\000'
  exit 0
fi
if [ "$1" = "-d" ] && [ "$2" = "Ubuntu" ] && [ "$3" = "-e" ]; then
  shift 3
  for v in $(env | sed -n 's/^\\(AGENT_MESH_[A-Z_]*\\)=.*/\\1/p'); do unset $v; done
  export HOME='${host.home}' XDG_STATE_HOME='${xdg}' PATH='${stubs}:${meshPath}/usr/bin:/bin'
  exec "$@"
fi
echo "fake wsl.exe: unexpected arguments: $*" >&2
exit 1
`,
    { mode: 0o755 },
  )
  return { exe, systemctlLog }
}
