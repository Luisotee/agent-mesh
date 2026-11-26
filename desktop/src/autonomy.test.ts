import { create } from '@bufbuild/protobuf'
import { describe, expect, it } from 'vitest'
import { fromJson } from '@bufbuild/protobuf'
import { ValueSchema } from '@bufbuild/protobuf/wkt'
import { SettingSchema$ as SettingDescriptor, type Setting } from './gen/agentmesh/v1/types_pb'
import { MERGES_KEY, PLANS_KEY, hasLevels, levelOf, nameEntries, withLevel } from './autonomy'

const setting = (key: string, value: string[]): Setting => create(SettingDescriptor, { key, value: fromJson(ValueSchema, value) })
const settings = (merges: string[], plans: string[]): Record<string, Setting> => ({ [MERGES_KEY]: setting(MERGES_KEY, merges), [PLANS_KEY]: setting(PLANS_KEY, plans) })

const withLevelApplied = (s: Record<string, Setting>, repo: string, level: Parameters<typeof withLevel>[2]) => {
  const out = { ...s }
  for (const w of withLevel(s, repo, level)) out[w.key] = setting(w.key, w.value)
  return out
}

describe('autonomy levels', () => {
  it('is the stricter list a repo is in, and "as before" when it is in none; a folder name or a trailing slash names a repo too', () => {
    const s = settings(['/p/shop', 'api', '/p/web/'], ['/p/api'])
    expect(levelOf(s, '/p/shop')).toBe('merges')
    expect(levelOf(s, '/p/api')).toBe('plans')
    expect(levelOf(s, '/p/web')).toBe('merges')
    expect(levelOf(s, '/p/other')).toBe('default')
    expect(levelOf({}, '/p/shop')).toBe('default')
  })

  it('moves a repo between the lists, taking it out of the other, and sets only what changes', () => {
    const s = settings(['/p/shop', 'api'], ['/p/pay'])
    expect(withLevel(s, '/p/shop', 'plans')).toEqual([{ key: MERGES_KEY, value: ['api'] }, { key: PLANS_KEY, value: ['/p/pay', '/p/shop'] }])
    expect(withLevel(s, '/p/shop', 'default')).toEqual([{ key: MERGES_KEY, value: ['api'] }])
    expect(withLevel(s, '/p/shop', 'merges')).toEqual([])
    expect(withLevel(settings([], []), '/p/new', 'merges')).toEqual([{ key: MERGES_KEY, value: ['/p/new'] }])
  })

  it('never drops a folder-name entry, which covers every repo of that name, when one of them changes', () => {
    // `api` covers /p/api and /q/api: setting /p/api to "as before" must not strip /q/api of "I merge".
    const s = settings(['api'], [])
    expect(levelOf(s, '/q/api')).toBe('merges')
    expect(withLevel(s, '/p/api', 'default')).toEqual([])
    expect(nameEntries(s, '/p/api')).toEqual(['api'])
    // A level above it is still set, by the repo's path, with the name entry left in place.
    expect(withLevel(s, '/p/api', 'plans')).toEqual([{ key: PLANS_KEY, value: ['/p/api'] }])
    const t = settings(['api', '/p/api'], [])
    expect(withLevel(t, '/p/api', 'default')).toEqual([{ key: MERGES_KEY, value: ['api'] }])
    expect(levelOf(withLevelApplied(t, '/p/api', 'default'), '/q/api')).toBe('merges')
    expect(nameEntries(settings(['/p/api'], []), '/p/api')).toEqual([])
  })

  it('needs both settings on the host', () => {
    expect(hasLevels(settings([], []))).toBe(true)
    expect(hasLevels({})).toBe(false)
  })
})
