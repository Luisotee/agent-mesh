// @vitest-environment node
import { mkdtempSync, readFileSync, statSync, writeFileSync } from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { describe, expect, it } from 'vitest'
import { Hosts, toInfo } from './hosts'

const tmp = () => path.join(mkdtempSync(path.join(os.tmpdir(), 'amh-')), 'sub', 'hosts.json')

describe('Hosts', () => {
  it('saves hosts privately and loads them again, with the token kept out of what the renderer sees', async () => {
    const file = tmp()
    const h = new Hosts(file)
    const added = await h.add({ name: 'vps', kind: 'tcp', address: '127.0.0.1:7777', token: ' secret ' })
    await h.add({ name: 'home', kind: 'state-dir', path: '/home/u/state' })
    expect(statSync(file).mode & 0o777).toBe(0o600)
    expect(JSON.stringify(toInfo(added))).not.toContain('secret')

    const again = new Hosts(file)
    await again.load()
    expect(again.all().map((x) => x.name)).toEqual(['vps', 'home'])
    expect(again.get(added.id)).toMatchObject({ token: 'secret' })
    await again.remove(added.id)
    expect(JSON.parse(readFileSync(file, 'utf8'))).toHaveLength(1)
  })

  it('rejects a host that can\'t work', async () => {
    const h = new Hosts(tmp())
    await expect(h.add({ name: '', kind: 'state-dir', path: '/x' })).rejects.toThrow(/name/)
    await expect(h.add({ name: 'a', kind: 'state-dir', path: 'relative' })).rejects.toThrow(/absolute/)
    await expect(h.add({ name: 'a', kind: 'tcp', address: 'nohost', token: 't' })).rejects.toThrow(/loopback/)
    // A bearer token over plain HTTP stays on this machine.
    await expect(h.add({ name: 'a', kind: 'tcp', address: '203.0.113.5:7777', token: 't' })).rejects.toThrow(/loopback/)
    await expect(h.add({ name: 'a', kind: 'tcp', address: '127.0.0.1:99999', token: 't' })).rejects.toThrow(/port/)
    await expect(h.add({ name: 'a', kind: 'tcp', address: 'localhost:1', token: ' ' })).rejects.toThrow(/token/)
    expect(h.all()).toEqual([])
  })

  it('accepts loopback addresses, IPv6 included', async () => {
    const h = new Hosts(tmp())
    for (const address of ['localhost:7777', '127.0.0.1:1', '[::1]:7777']) await h.add({ name: address, kind: 'tcp', address, token: 't' })
    expect(h.all()).toHaveLength(3)
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
    await h.add({ name: 'c', kind: 'tcp', address: '127.0.0.1:1', token: 't' })
    await expect(h.add({ name: 'd', kind: 'tcp', address: '127.0.0.1:1', token: 'u' })).rejects.toThrow(/already/)
    expect(h.all()).toHaveLength(2)
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
