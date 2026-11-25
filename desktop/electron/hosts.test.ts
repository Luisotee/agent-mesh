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
    await expect(h.add({ name: 'a', kind: 'tcp', address: 'nohost', token: 't' })).rejects.toThrow(/host:port/)
    await expect(h.add({ name: 'a', kind: 'tcp', address: 'h:1', token: ' ' })).rejects.toThrow(/token/)
    expect(h.all()).toEqual([])
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
