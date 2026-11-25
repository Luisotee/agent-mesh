// @vitest-environment node
import { mkdirSync, mkdtempSync, readFileSync, statSync, writeFileSync } from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { describe, expect, it } from 'vitest'
import { Hosts, toInfo, type Redeemed, type Redeemer } from './hosts'

const FP = `sha256:${'ab'.repeat(32)}`
const link = (extra = '', host = 'h_1') => `agentmesh://pair?v=1&h=${host}&fp=${FP}&t=amp_one&hn=Home&d=${encodeURIComponent('https://192.168.1.5:9443')}${extra}`
/** A host that accepts any link: the token it hands back is `amt_<host id>`. */
const redeemer = (log: string[] = []): Redeemer => async (l, deviceName): Promise<Redeemed> => {
  log.push(`${l.route.hostId}:${l.token}:${deviceName}`)
  return { deviceId: 'dev_1', deviceName, token: `amt_${l.route.hostId}`, hostId: l.route.hostId, hostName: l.hostName, fingerprint: l.route.fingerprint }
}
const tmp = () => path.join(mkdtempSync(path.join(os.tmpdir(), 'amh-')), 'sub', 'hosts.json')

describe('Hosts', () => {
  it('saves a paired host privately and loads it again, with the token kept out of what the renderer sees', async () => {
    const file = tmp()
    const log: string[] = []
    const h = new Hosts(file, undefined, redeemer(log))
    const added = await h.add({ name: '', kind: 'pair', link: link(), deviceName: ' My laptop ' })
    await h.add({ name: 'home', kind: 'state-dir', path: '/home/u/state' })
    expect(log).toEqual(['h_1:amp_one:My laptop'])
    expect(added.name).toBe('Home') // the host's own name, when none is given
    expect(statSync(file).mode & 0o777).toBe(0o600)
    expect(JSON.stringify(toInfo(added))).not.toContain('amt_')
    expect(toInfo(added)).toMatchObject({ kind: 'paired', target: '192.168.1.5:9443' })

    const again = new Hosts(file)
    await again.load()
    expect(again.all().map((x) => x.name)).toEqual(['Home', 'home'])
    expect(again.get(added.id)).toMatchObject({ kind: 'paired', token: 'amt_h_1', deviceId: 'dev_1', route: { hostId: 'h_1', fingerprint: FP, direct: { host: '192.168.1.5', port: 9443 } } })
    await again.remove(added.id)
    expect(JSON.parse(readFileSync(file, 'utf8'))).toHaveLength(1)
  })

  it('still loads a host saved with a loopback address and a pasted token, which can no longer be added', async () => {
    const file = tmp()
    mkdirSync(path.dirname(file), { recursive: true })
    writeFileSync(file, JSON.stringify([{ id: 't', name: 'old', kind: 'tcp', address: '127.0.0.1:7777', token: 'tok' }]))
    const h = new Hosts(file)
    await h.load()
    expect(h.all()).toEqual([{ id: 't', name: 'old', kind: 'tcp', address: '127.0.0.1:7777', token: 'tok' }])
    expect(toInfo(h.all()[0]!)).toMatchObject({ kind: 'tcp', target: '127.0.0.1:7777' })
  })

  it('rejects a host that can\'t work, before any token is spent', async () => {
    const log: string[] = []
    const h = new Hosts(tmp(), undefined, redeemer(log))
    await expect(h.add({ name: '', kind: 'state-dir', path: '/x' })).rejects.toThrow(/name/)
    await expect(h.add({ name: 'a', kind: 'state-dir', path: 'relative' })).rejects.toThrow(/absolute/)
    await expect(h.add({ name: 'a', kind: 'pair', link: 'https://example.com', deviceName: '' })).rejects.toThrow(/pairing link/)
    await expect(h.add({ name: 'a', kind: 'pair', link: 'agentmesh://pair?v=1&h=x&fp=nope&t=amp_x&d=https://a:1', deviceName: '' })).rejects.toThrow(/fingerprint/)
    await expect(h.add({ name: 'a', kind: 'pair', link: `agentmesh://pair?v=1&h=x&fp=${FP}&t=amp_x`, deviceName: '' })).rejects.toThrow(/no way to reach/)
    await expect(h.add({ name: 'a', kind: 'tcp', address: '127.0.0.1:1', token: 't' } as never)).rejects.toThrow(/unknown kind/)
    await expect(h.add({ name: 'a', kind: 'pair', link: 5 } as never)).rejects.toThrow(/unknown kind/)
    expect(log).toEqual([])
    expect(h.all()).toEqual([])
  })

  it('refuses to save a host that answers as another host than the link named', async () => {
    const h = new Hosts(tmp(), undefined, async (l, n) => ({ ...(await redeemer()(l, n)), hostId: 'someone-else' }))
    await expect(h.add({ name: 'a', kind: 'pair', link: link(), deviceName: '' })).rejects.toThrow(/isn't the one/)
    expect(h.all()).toEqual([])
  })

  it('refuses a paired host already in the list without spending the link', async () => {
    const log: string[] = []
    const h = new Hosts(tmp(), undefined, redeemer(log))
    await h.add({ name: 'a', kind: 'pair', link: link(), deviceName: '' })
    await expect(h.add({ name: 'b', kind: 'pair', link: link('&x=1'), deviceName: '' })).rejects.toThrow(/already/)
    await h.add({ name: 'c', kind: 'pair', link: link('', 'h_2'), deviceName: '' })
    expect(log).toHaveLength(2)
  })

  it('overlapping adds all reach the file', async () => {
    const file = tmp()
    const h = new Hosts(file)
    await Promise.all(['a', 'b', 'c', 'd'].map((n) => h.add({ name: n, kind: 'state-dir', path: `/s/${n}` })))
    expect(JSON.parse(readFileSync(file, 'utf8')).map((x: { name: string }) => x.name).sort()).toEqual(['a', 'b', 'c', 'd'])
  })

  it('refuses a host that is already in the list', async () => {
    const h = new Hosts(tmp())
    await h.add({ name: 'a', kind: 'state-dir', path: '/s/a' })
    await expect(h.add({ name: 'b', kind: 'state-dir', path: '/s/a/' })).rejects.toThrow(/already/)
    expect(h.all()).toHaveLength(1)
  })
  it('keeps entries it does not understand when it saves', async () => {
    const file = tmp()
    const h = new Hosts(file)
    await h.add({ name: 'a', kind: 'state-dir', path: '/s/a' })
    const saved = JSON.parse(readFileSync(file, 'utf8')) as unknown[]
    const newer = { id: 'x', name: 'new', kind: 'ssh', host: 'vps', token: 'secret' }
    writeFileSync(file, JSON.stringify([...saved, newer]))
    const again = new Hosts(file)
    await again.load()
    expect(again.all()).toHaveLength(1)
    await again.add({ name: 'b', kind: 'state-dir', path: '/s/b' })
    expect(JSON.parse(readFileSync(file, 'utf8'))).toContainEqual(newer)
  })

  it('leaves a damaged file alone and starts empty', async () => {
    const file = tmp()
    const h = new Hosts(file)
    await h.add({ name: 'a', kind: 'state-dir', path: '/x' })
    writeFileSync(file, '{not json')
    const again = new Hosts(file)
    await again.load()
    expect(again.all()).toEqual([])
    expect(readFileSync(file, 'utf8')).toBe('{not json')
  })

  it('is one fixed host when the environment says so', async () => {
    const h = new Hosts(tmp(), '/tmp/state')
    await h.load()
    expect(h.all()).toEqual([{ id: 'local', name: 'local', kind: 'state-dir', path: '/tmp/state' }])
    await expect(h.add({ name: 'a', kind: 'state-dir', path: '/x' })).rejects.toThrow(/fixed/)
    await expect(h.remove('local')).rejects.toThrow(/fixed/)
  })
})
