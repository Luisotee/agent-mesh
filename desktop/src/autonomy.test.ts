import { create } from '@bufbuild/protobuf'
import { describe, expect, it } from 'vitest'
import { fromJson } from '@bufbuild/protobuf'
import { ValueSchema } from '@bufbuild/protobuf/wkt'
import { SettingSchema$ as SettingDescriptor, type Setting } from './gen/agentmesh/v1/types_pb'
import { MERGES_KEY, PLANS_KEY, hasLevels, levelOf, withLevel } from './autonomy'

const setting = (key: string, value: string[]): Setting => create(SettingDescriptor, { key, value: fromJson(ValueSchema, value) })
const settings = (merges: string[], plans: string[]): Record<string, Setting> => ({ [MERGES_KEY]: setting(MERGES_KEY, merges), [PLANS_KEY]: setting(PLANS_KEY, plans) })

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
    expect(withLevel(s, '/p/api', 'plans')).toEqual([{ key: MERGES_KEY, value: ['/p/shop'] }, { key: PLANS_KEY, value: ['/p/pay', '/p/api'] }])
    expect(withLevel(s, '/p/shop', 'default')).toEqual([{ key: MERGES_KEY, value: ['api'] }])
    expect(withLevel(s, '/p/shop', 'merges')).toEqual([])
    expect(withLevel(settings([], []), '/p/new', 'merges')).toEqual([{ key: MERGES_KEY, value: ['/p/new'] }])
  })

  it('needs both settings on the host', () => {
    expect(hasLevels(settings([], []))).toBe(true)
    expect(hasLevels({})).toBe(false)
  })
})
