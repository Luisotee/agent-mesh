// @vitest-environment node
import { mkdirSync, mkdtempSync } from 'node:fs'
import net from 'node:net'
import os from 'node:os'
import path from 'node:path'
import { afterEach, describe, expect, it } from 'vitest'
import { Hosts, type Redeemed, type Redeemer } from './hosts'
import { Local, defaultStateDir, parseDistros, parseProbe, type LocalDeps, type RunResult } from './local'

const FP = `sha256:${'cd'.repeat(32)}`
const tmp = () => mkdtempSync(path.join(os.tmpdir(), 'amh-local-'))
const utf16 = (s: string) => Buffer.concat([Buffer.from([0xff, 0xfe]), Buffer.from(s, 'utf16le')])
const out = (s: string): RunResult => ({ code: 0, stdout: Buffer.from(s) })
const MESH = '@@MESH '
const redeemer: Redeemer = async (l, deviceName): Promise<Redeemed> => ({ deviceId: 'dev_1', deviceName, token: 'amt_x', hostId: l.route.hostId, hostName: l.hostName, fingerprint: l.route.fingerprint })
const pairLink = `agentmesh://pair?v=1&h=h_wsl&fp=${FP}&t=amp_one&hn=Box&d=${encodeURIComponent('https://127.0.0.1:9443')}`

/** What the probe script prints for a distro. */
function probeOut(o: { installed?: boolean; systemd?: boolean; listen?: string; hello?: string[]; listenconf?: boolean } = {}): string {
  const lines = [`Welcome to a login shell`, `${MESH}installed=${o.installed === false ? 0 : 1}`, `${MESH}systemd=${o.systemd === false ? 0 : 1}`]
  if (o.listen !== undefined) lines.push(`${MESH}info=${JSON.stringify({ listen: o.listen, cert_fingerprint: FP })}`)
  if (o.hello) lines.push(`${MESH}hello=${JSON.stringify({ capabilities: o.hello })}`)
  if (o.listenconf) lines.push(`${MESH}listenconf=1`)
  return lines.join('\n') + '\n'
}

interface Fake {
  deps: LocalDeps
  calls: Array<{ file: string; args: string[]; stdin?: string }>
  /** What each distro's probe prints (changed by a test as the world changes). */
  probes: Record<string, string>
  scripts: Array<(stdin: string) => RunResult | undefined>
  reachable: boolean
  helloOk: boolean
}

function fake(platform: NodeJS.Platform, distros: string[] = []): Fake {
  const f: Fake = {
    calls: [],
    probes: {},
    scripts: [],
    reachable: true,
    helloOk: true,
    deps: undefined as unknown as LocalDeps,
  }
  const home = tmp()
  f.deps = {
    platform,
    home,
    env: {},
    wslExe: 'wsl.exe',
    hostname: 'laptop',
    async run(file, args, stdin) {
      f.calls.push({ file, args, stdin })
      if (file === 'wsl.exe' && args[0] === '-l') return { code: 0, stdout: utf16(distros.join('\r\n') + '\r\n') }
      if (file === 'wsl.exe' && args[0] === '-d' && stdin) {
        for (const s of f.scripts) {
          const r = s(stdin)
          if (r) return r
        }
        if (stdin.includes('mesh hello')) return out(f.probes[args[1]!] ?? probeOut({ installed: false }))
      }
      return { code: 0, stdout: Buffer.alloc(0) }
    },
    reach: async () => f.reachable,
    hello: async () => f.helloOk,
  }
  return f
}

const newHosts = () => new Hosts(path.join(tmp(), 'hosts.json'), undefined, redeemer)

describe('parsing', () => {
  it('reads wsl.exe\'s UTF-16 list, leaving out docker\'s own distros', () => {
    expect(parseDistros(utf16('Ubuntu\r\ndocker-desktop\r\nDebian\r\n'))).toEqual(['Ubuntu', 'Debian'])
    expect(parseDistros(Buffer.from('Ubuntu\nDebian\n'))).toEqual(['Ubuntu', 'Debian'])
    expect(parseDistros(Buffer.alloc(0))).toEqual([])
  })

  it('takes only the probe\'s own lines out of a login shell\'s chatter', () => {
    expect(parseProbe(`hello there\n${MESH}a=1\r\n${MESH}b=x=y\nnoise\n`)).toEqual({ a: '1', b: 'x=y' })
  })

  it('finds the state dir the way mesh does', () => {
    expect(defaultStateDir({}, '/home/u')).toBe('/home/u/.local/state/agent-mesh-daemon')
    expect(defaultStateDir({ XDG_STATE_HOME: '/x' }, '/home/u')).toBe('/x/agent-mesh-daemon')
  })
})

describe('Local, on Linux', () => {
  const servers: net.Server[] = []
  afterEach(() => servers.splice(0).forEach((s) => s.close()))

  it('says there is no daemon when there is no state dir, and where the install steps are', async () => {
    const f = fake('linux')
    const [c] = await new Local(newHosts(), f.deps).detect()
    expect(c).toMatchObject({ id: 'native', state: 'not-installed', fixes: [] })
    expect(c!.commands?.join(' ')).toContain('install.sh')
  })

  it('offers to start a daemon whose state dir has no socket', async () => {
    const f = fake('linux')
    mkdirSync(defaultStateDir(f.deps.env, f.deps.home), { recursive: true })
    expect((await new Local(newHosts(), f.deps).detect())[0]).toMatchObject({ state: 'not-running', fixes: ['start'] })
  })

  it('is ready when a socket answers, and connects as a state-dir host with nothing pasted', async () => {
    const f = fake('linux')
    const dir = defaultStateDir(f.deps.env, f.deps.home)
    mkdirSync(dir, { recursive: true })
    await new Promise<void>((res) => servers.push(net.createServer().listen(path.join(dir, 'meshd.sock'), res)))
    const hosts = newHosts()
    const local = new Local(hosts, f.deps)
    expect((await local.detect())[0]).toMatchObject({ state: 'ready' })
    const info = await local.connect('native')
    expect(info).toMatchObject({ name: 'laptop', kind: 'state-dir', target: dir })
    expect(hosts.all()).toHaveLength(1)
    // A socket that doesn't answer Hello is a daemon that isn't there.
    f.helloOk = false
    expect((await local.detect())[0]).toMatchObject({ state: 'not-running' })
    await expect(local.connect('native')).rejects.toThrow(/doesn't answer/)
  })
})

describe('which saved host is this machine\'s daemon', () => {
  const stored = (o: object) => ({ id: 'x', name: 'x', ...o }) as never
  const direct = (host: string) => ({ hostId: 'h', fingerprint: FP, direct: { host, port: 9443 } })

  it('on Linux is the state dir a scan looks at, not any state dir', () => {
    const f = fake('linux')
    const local = new Local(newHosts(), f.deps)
    const dir = defaultStateDir(f.deps.env, f.deps.home)
    expect(local.info(stored({ kind: 'state-dir', path: dir }))).toMatchObject({ here: true, target: dir })
    expect(local.info(stored({ kind: 'state-dir', path: path.join(tmp(), 'test-daemon') })).here).toBe(false)
    expect(local.info(stored({ kind: 'paired', deviceId: 'd', token: 't', route: direct('127.0.0.1') })).here).toBe(false)
  })

  it('on Windows is a pairing at the distro\'s loopback address', () => {
    const local = new Local(newHosts(), fake('win32').deps)
    expect(local.isHere(stored({ kind: 'paired', deviceId: 'd', token: 't', route: direct('127.0.0.1') }))).toBe(true)
    expect(local.isHere(stored({ kind: 'paired', deviceId: 'd', token: 't', route: direct('192.168.1.5') }))).toBe(false)
    expect(local.isHere(stored({ kind: 'paired', deviceId: 'd', token: 't', route: { hostId: 'h', fingerprint: FP, relay: { host: 'r.example.com' } } }))).toBe(false)
  })
})

describe('Local, on Windows', () => {
  it('says so when WSL is not there, or has no distro', async () => {
    const none = fake('win32')
    none.deps.run = async () => ({ code: -1, stdout: Buffer.alloc(0) })
    expect((await new Local(newHosts(), none.deps).detect())[0]).toMatchObject({ id: 'wsl', state: 'not-installed', commands: ['wsl --install'] })
    // WSL answering "no distributions" (not a list) with a failing exit code is the same.
    const noDistro = fake('win32')
    noDistro.deps.run = async () => ({ code: 1, stdout: utf16('Windows Subsystem for Linux has no installed distributions.\r\n') })
    expect((await new Local(newHosts(), noDistro.deps).detect())[0]).toMatchObject({ id: 'wsl', state: 'not-installed', commands: ['wsl --install -d Ubuntu'] })
    const empty = fake('win32', [])
    expect((await new Local(newHosts(), empty.deps).detect())[0]).toMatchObject({ id: 'wsl', state: 'not-installed' })
  })

  it('tells each state apart, per distro', async () => {
    const f = fake('win32', ['B', 'C', 'D', 'E', 'F'])
    f.probes = {
      B: probeOut({ hello: undefined }),
      C: probeOut({ hello: ['mutations'], listen: '' }),
      D: probeOut({ hello: ['mutations', 'tls'], listen: '127.0.0.1:9443' }),
      E: probeOut({ systemd: false }),
      F: probeOut({ hello: ['mutations'], listenconf: true }),
    }
    const got = Object.fromEntries((await new Local(newHosts(), f.deps).detect()).map((c) => [c.id, c]))
    expect(got['wsl:B']).toMatchObject({ state: 'not-running', fixes: ['start'] })
    expect(got['wsl:C']).toMatchObject({ state: 'no-listen', fixes: ['enable-listen'] })
    expect(got['wsl:D']).toMatchObject({ state: 'ready', fixes: [] })
    expect(got['wsl:E']).toMatchObject({ state: 'not-running', fixes: [] }) // no systemd: run meshd by hand
    // A listen.conf already there is the user's: the app doesn't overwrite it.
    expect(got['wsl:F']).toMatchObject({ state: 'no-listen', fixes: [] })
    f.reachable = false
    const again = Object.fromEntries((await new Local(newHosts(), f.deps).detect()).map((c) => [c.id, c]))
    expect(again['wsl:D']).toMatchObject({ state: 'unreachable', fixes: [] })
    expect(again['wsl:D']!.detail).toContain('mirrored')
  })

  it('says where to install when no distro has agent-mesh', async () => {
    const f = fake('win32', ['A'])
    f.probes = { A: probeOut({ installed: false }) }
    const [c] = await new Local(newHosts(), f.deps).detect()
    expect(c).toMatchObject({ id: 'wsl:A', state: 'not-installed', fixes: [] })
    expect(c!.commands?.join(' ')).toContain('install.sh')
  })

  it('leaves out the distros that have no agent-mesh once another one has it', async () => {
    const f = fake('win32', ['Debian', 'Ubuntu'])
    f.probes = { Debian: probeOut({ installed: false }), Ubuntu: probeOut({ hello: ['tls'], listen: '127.0.0.1:9443' }) }
    expect((await new Local(newHosts(), f.deps).detect()).map((c) => c.id)).toEqual(['wsl:Ubuntu'])
  })

  it('runs the probe as a script on stdin, never as arguments built from anything the renderer sent', async () => {
    const f = fake('win32', ['Ubuntu'])
    f.probes = { Ubuntu: probeOut({ installed: false }) }
    await new Local(newHosts(), f.deps).detect()
    const probe = f.calls.find((c) => c.stdin?.includes('mesh hello'))!
    expect(probe.args).toEqual(['-d', 'Ubuntu', '-e', 'sh', '-l', '-s'])
  })

  it('pairs through the distro: mesh pair inside it, then the same redeem a pasted link takes', async () => {
    const f = fake('win32', ['Ubuntu'])
    f.probes = { Ubuntu: probeOut({ hello: ['tls'], listen: '[::]:9443' }) }
    f.scripts.push((stdin) => (stdin.includes('mesh pair') ? out(`${MESH}pair=${JSON.stringify({ uri: pairLink })}\n`) : undefined))
    const hosts = newHosts()
    const info = await new Local(hosts, f.deps).connect('wsl:Ubuntu')
    expect(info).toMatchObject({ name: 'Box', kind: 'paired', target: '127.0.0.1:9443' })
    const pair = f.calls.find((c) => c.stdin?.includes('mesh pair'))!
    expect(pair.stdin).toContain('--format json')
    expect(pair.stdin).toContain('--direct-url https://127.0.0.1:9443')
    expect(pair.stdin).toContain('--ttl-seconds 120')
    expect(hosts.all()).toHaveLength(1)
  })

  it('refuses to connect to a distro that is not ready, or that is not in the list', async () => {
    const f = fake('win32', ['Ubuntu'])
    f.probes = { Ubuntu: probeOut({ hello: ['tls'], listen: '127.0.0.1:9443' }) }
    const local = new Local(newHosts(), f.deps)
    await expect(local.connect('wsl:Other')).rejects.toThrow(/no longer there/)
    await expect(local.connect('wsl:Ubuntu; rm -rf ~')).rejects.toThrow(/no longer there/)
    f.probes = { Ubuntu: probeOut({ hello: ['mutations'], listen: '' }) }
    await expect(local.connect('wsl:Ubuntu')).rejects.toThrow(/only for programs inside WSL/)
    // And a mesh pair that fails says so, rather than saving anything.
    f.probes = { Ubuntu: probeOut({ hello: ['tls'], listen: '127.0.0.1:9443' }) }
    f.scripts.push((stdin) => (stdin.includes('mesh pair') ? out(`${MESH}err=mesh pair failed\n`) : undefined))
    await expect(local.connect('wsl:Ubuntu')).rejects.toThrow(/mesh pair failed/)
  })

  it('runs only the fixes a candidate offers, and scans again once the daemon settles', async () => {
    const f = fake('win32', ['Ubuntu'])
    f.probes = { Ubuntu: probeOut({ hello: ['mutations'], listen: '' }) }
    f.scripts.push((stdin) => {
      if (!stdin.includes('daemon-reload')) return undefined
      // The fix takes effect: the daemon now listens.
      f.probes.Ubuntu = probeOut({ hello: ['mutations', 'tls'], listen: '127.0.0.1:9443' })
      return out(`${MESH}ok=1\n`)
    })
    const local = new Local(newHosts(), f.deps, { tries: 2, everyMs: 0 })
    await expect(local.fix('wsl:Ubuntu', 'start')).rejects.toThrow(/not on offer/)
    await expect(local.fix('wsl:Nope', 'enable-listen')).rejects.toThrow(/not on offer/)
    const after = await local.fix('wsl:Ubuntu', 'enable-listen')
    expect(after[0]).toMatchObject({ state: 'ready' })
    const run = f.calls.find((c) => c.stdin?.includes('daemon-reload'))!
    expect(run.stdin).toContain('AGENT_MESH_LISTEN=127.0.0.1:9443')
    expect(run.stdin).toContain('daemon-reload')
  })

  it('says so when a fix ran but the daemon is still where it was', async () => {
    const f = fake('win32', ['Ubuntu'])
    f.probes = { Ubuntu: probeOut({ hello: ['mutations'], listen: '' }) }
    f.scripts.push((stdin) => (stdin.includes('daemon-reload') ? out(`${MESH}ok=1\n`) : undefined))
    const local = new Local(newHosts(), f.deps, { tries: 2, everyMs: 0 })
    await expect(local.fix('wsl:Ubuntu', 'enable-listen')).rejects.toThrow(/isn't listening yet/)
  })

  it('reports a fix that failed instead of scanning on', async () => {
    const f = fake('win32', ['Ubuntu'])
    f.probes = { Ubuntu: probeOut({ hello: undefined }) }
    f.scripts.push((stdin) => (stdin.includes('enable --now') ? out(`${MESH}err=systemctl --user enable --now agent-mesh failed\n`) : undefined))
    await expect(new Local(newHosts(), f.deps).fix('wsl:Ubuntu', 'start')).rejects.toThrow(/enable --now agent-mesh failed/)
  })

  it('refuses when the host list is fixed by the environment', async () => {
    const f = fake('win32', ['Ubuntu'])
    const fixed = new Hosts(path.join(tmp(), 'h.json'), '/state')
    await expect(new Local(fixed, f.deps).connect('wsl:Ubuntu')).rejects.toThrow(/fixed/)
  })
})
