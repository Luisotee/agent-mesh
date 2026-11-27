import { create } from '@bufbuild/protobuf'
import { describe, expect, it } from 'vitest'
import { ShellChangeSchema, ShellSchema } from './gen/agentmesh/v1/mesh_pb'
import { applyShellChange, shellTabId } from './shells'

const shell = (id: string, o: { exited?: boolean } = {}) => create(ShellSchema, { id, repo: '/p/app', ...o })
const change = (id: string, closed: boolean) => create(ShellChangeSchema, { shell: shell(id), closed })

describe('applyShellChange', () => {
  const state = (shells: Record<string, ReturnType<typeof shell>[]>, tabs: { hostId: string; agentId: string }[] = []) => ({ shells, tabs })

  it('adds an opened shell to its host, once', () => {
    const s = state({ h1: [shell('a')] })
    expect(applyShellChange(s, 'h1', change('b', false)).shells?.h1?.map((x) => x.id)).toEqual(['a', 'b'])
    const twice = { ...s, ...applyShellChange(s, 'h1', change('a', false)) }
    expect(twice.shells.h1?.map((x) => x.id)).toEqual(['a'])
  })

  it('drops an ended shell no tab shows, and marks one a tab shows as exited', () => {
    const s = state({ h1: [shell('a'), shell('b')] }, [{ hostId: 'h1', agentId: shellTabId('b') }])
    expect(applyShellChange(s, 'h1', change('a', true)).shells?.h1?.map((x) => x.id)).toEqual(['b'])
    const kept = applyShellChange(s, 'h1', change('b', true)).shells?.h1
    expect(kept?.map((x) => [x.id, x.exited])).toEqual([['a', false], ['b', true]])
  })

  it('forgets an exited shell once no tab shows it', () => {
    const s = state({ h1: [shell('a', { exited: true }), shell('b')] })
    expect(applyShellChange(s, 'h1', change('c', false)).shells?.h1?.map((x) => x.id)).toEqual(['b', 'c'])
  })

  it('leaves a host whose list is not loaded yet to that load, and touches no other host', () => {
    const s = state({ h2: [shell('x')] })
    expect(applyShellChange(s, 'h1', change('b', false))).toEqual({})
    expect(applyShellChange(s, 'h2', change('b', false)).shells?.h1).toBeUndefined()
  })
})
