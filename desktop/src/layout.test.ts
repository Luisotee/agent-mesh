import { create } from '@bufbuild/protobuf'
import { describe, expect, it } from 'vitest'
import { layoutOf, parseLayout, pruneHost, restoreOf, type LayoutSource } from './layout'
import { AgentSchema, type Agent } from './gen/agentmesh/v1/types_pb'
import { emptyWorkspace } from './workspace'

const src = (o: Partial<LayoutSource> = {}): LayoutSource => ({ ...emptyWorkspace(), tab: 'overview', panelOpen: true, ...o })
const ref = (hostId: string, agentId: string) => ({ hostId, agentId })
const agents = (...ids: string[]): Record<string, Agent> => Object.fromEntries(ids.map((id) => [id, create(AgentSchema, { id })]))

describe('layout', () => {
  const state = src({ tabs: [ref('h', 'a'), ref('h', 'b'), ref('g', 'c')], hostId: 'h', agentId: 'b', tab: 'chat', views: { 'h/a': 'terminal' }, side: [{ ...ref('g', 'c'), tab: 'messages' }], splitDir: 'column', sidebarCollapsed: true, hidden: { h: ['/w/old'] }, dockOpen: false, panelOpen: false })

  it('round-trips through JSON, with the open tab\'s view among the views', () => {
    const l = parseLayout(JSON.stringify(layoutOf(state)))!
    expect(l.active).toEqual({ hostId: 'h', agentId: 'b', tab: 'chat' })
    expect(l.views).toEqual({ 'h/a': 'terminal', 'h/b': 'chat' })
    expect(restoreOf(l, new Set(['h', 'g']))).toMatchObject({ tabs: state.tabs, hostId: 'h', agentId: 'b', tab: 'chat', side: state.side, splitDir: 'column', sidebarCollapsed: true, hidden: { h: ['/w/old'] }, dockOpen: false, panelOpen: false })
  })

  it('restores nothing from text that is not a layout, or one of another version', () => {
    for (const t of [null, '', 'nope', '[]', '{"v":2}']) expect(parseLayout(t)).toBeUndefined()
    expect(parseLayout('{"v":1,"tabs":"x"}')?.tabs).toEqual([])
  })

  it('drops what is damaged: duplicate tabs, an active tab that is not open, panes without a tab, unknown views', () => {
    const l = parseLayout(JSON.stringify({ v: 1, tabs: [ref('h', 'a'), ref('h', 'a'), { hostId: 'h' }, ref('h', 'b')], views: { 'h/a': 'bogus', 'h/zz': 'chat' }, active: { ...ref('h', 'gone'), tab: 'chat' }, side: [{ ...ref('h', 'b'), tab: 'chat' }, { ...ref('h', 'nope'), tab: 'chat' }], splitDir: 'diagonal', sidebarGroup: 'x' }))!
    expect(l.tabs).toEqual([ref('h', 'a'), ref('h', 'b')])
    expect(l.views).toEqual({ 'h/a': 'overview' })
    expect(l.active).toBeUndefined()
    expect(l.side).toEqual([{ ...ref('h', 'b'), tab: 'chat' }])
    expect(l).toMatchObject({ splitDir: 'row', sidebarCollapsed: false, hidden: {}, dockOpen: true, panelOpen: true })
  })

  it('leaves out the tabs of a host that is no longer there', () => {
    const l = parseLayout(JSON.stringify(layoutOf(state)))!
    const r = restoreOf(l, new Set(['g']))
    expect(r.tabs).toEqual([ref('g', 'c')])
    expect(r.agentId).toBeUndefined()
    expect(r.views).toEqual({})
  })
})

describe('pruneHost', () => {
  it('drops the tabs and panes of agents the host lost, and moves a pane over when the open agent is gone', () => {
    const s = src({ tabs: [ref('h', 'a'), ref('h', 'b'), ref('h', 'c'), ref('g', 'z')], hostId: 'h', agentId: 'a', views: { 'h/a': 'chat', 'h/b': 'chat', 'h/c': 'chat' }, side: [{ ...ref('h', 'b'), tab: 'messages' }, { ...ref('h', 'c'), tab: 'chat' }] })
    const p = pruneHost(s, 'h', agents('b'))!
    expect(p.tabs).toEqual([ref('h', 'b'), ref('g', 'z')])
    expect(p).toMatchObject({ hostId: 'h', agentId: 'b', tab: 'messages', side: [] })
    expect(Object.keys(p.views!)).toEqual(['h/b'])
  })

  it('clears the open agent when it is gone and nothing is beside it, and does nothing when all are there', () => {
    const s = src({ tabs: [ref('h', 'a')], hostId: 'h', agentId: 'a' })
    expect(pruneHost(s, 'h', agents('a'))).toBeUndefined()
    expect(pruneHost(s, 'h', agents())).toMatchObject({ tabs: [], agentId: undefined })
  })
})
