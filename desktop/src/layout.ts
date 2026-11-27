import type { SidebarGroup } from './model'
import type { AgentTab } from './model'
import { MAX_SIDE_PANES, tabKey, type PaneRef, type SplitDir, type TabRef } from './workspace'
import type { Agent, AgentState } from './gen/agentmesh/v1/types_pb'

/**
 * What the window restores after a restart: which agents were open as tabs, the view of each, the one that was open,
 * the panes beside it, and how the sidebar and dock were set. Device-local (it is the window, not the host), kept in
 * the renderer's own storage under the app's user-data folder.
 */
export interface Layout {
  v: 1
  tabs: TabRef[]
  views: Record<string, AgentTab>
  active?: { hostId: string; agentId: string; tab: AgentTab }
  side: PaneRef[]
  splitDir: SplitDir
  sidebarGroup: SidebarGroup
  dockOpen: boolean
  panelOpen: boolean
  /** Dock tiles the user dismissed, by tab key, with the agent state they were dismissed in. Optional: a layout from before had none. */
  dismissed?: Record<string, AgentState>
  /** The projects the user pinned in the New workspace dialog, by host id (paths on that host). */
  pins?: Record<string, string[]>
}

export const LAYOUT_KEY = 'agent-mesh.layout.v1'

const VIEWS: readonly string[] = ['overview', 'messages', 'chat', 'terminal']
const GROUPS: readonly string[] = ['run', 'project', 'state']

const str = (v: unknown): v is string => typeof v === 'string' && v.length > 0
const view = (v: unknown): AgentTab => (VIEWS.includes(v as string) ? (v as AgentTab) : 'overview')
const ref = (v: unknown): TabRef | undefined => {
  const o = v as Partial<TabRef> | null
  return o && str(o.hostId) && str(o.agentId) ? { hostId: o.hostId, agentId: o.agentId } : undefined
}

/** Reads a saved layout, with whatever is wrong in it dropped: a layout from another version, or damaged, restores what it can. */
export function parseLayout(text: string | null | undefined): Layout | undefined {
  if (!text) return undefined
  let raw: Record<string, unknown>
  try {
    raw = JSON.parse(text) as Record<string, unknown>
  } catch {
    return undefined
  }
  if (!raw || typeof raw !== 'object' || raw.v !== 1) return undefined
  const seen = new Set<string>()
  const unique = <T extends TabRef>(list: T[]): T[] => list.filter((r) => !seen.has(tabKey(r)) && seen.add(tabKey(r)))
  const tabs = unique((Array.isArray(raw.tabs) ? raw.tabs : []).flatMap((t) => ref(t) ?? []))
  const views: Record<string, AgentTab> = {}
  if (raw.views && typeof raw.views === 'object') for (const [k, v] of Object.entries(raw.views)) if (tabs.some((t) => tabKey(t) === k)) views[k] = view(v)
  const a = ref(raw.active)
  const active = a && tabs.some((t) => tabKey(t) === tabKey(a)) ? { ...a, tab: view((raw.active as { tab?: unknown }).tab) } : undefined
  const inTabs = (r: TabRef) => tabs.some((t) => tabKey(t) === tabKey(r))
  seen.clear()
  if (active) seen.add(tabKey(active))
  const side = unique((Array.isArray(raw.side) ? raw.side : []).flatMap((p) => { const r = ref(p); return r && inTabs(r) ? [{ ...r, tab: view((p as { tab?: unknown }).tab) }] : [] })).slice(0, MAX_SIDE_PANES)
  return {
    v: 1,
    tabs,
    views,
    active,
    side,
    splitDir: raw.splitDir === 'column' ? 'column' : 'row',
    sidebarGroup: GROUPS.includes(raw.sidebarGroup as string) ? (raw.sidebarGroup as SidebarGroup) : 'run',
    dockOpen: raw.dockOpen !== false,
    panelOpen: raw.panelOpen !== false,
    dismissed: dismissedOf(raw.dismissed),
    pins: pinsOf(raw.pins),
  }
}

const dismissedOf = (v: unknown): Record<string, AgentState> => {
  const out: Record<string, AgentState> = {}
  if (v && typeof v === 'object') for (const [k, n] of Object.entries(v)) if (k.includes('/') && Number.isInteger(n)) out[k] = n as AgentState
  return out
}

const pinsOf = (v: unknown): Record<string, string[]> => {
  const out: Record<string, string[]> = {}
  if (v && typeof v === 'object') {
    for (const [host, list] of Object.entries(v)) {
      const paths = Array.isArray(list) ? [...new Set(list.filter(str))] : []
      if (paths.length > 0) out[host] = paths
    }
  }
  return out
}

export interface LayoutSource {
  tabs: TabRef[]
  views: Record<string, AgentTab>
  hostId?: string
  agentId?: string
  tab: AgentTab
  side: PaneRef[]
  splitDir: SplitDir
  sidebarGroup: SidebarGroup
  dockOpen: boolean
  panelOpen: boolean
  dismissed?: Record<string, AgentState>
  pins?: Record<string, string[]>
}

export function layoutOf(s: LayoutSource): Layout {
  const views = { ...s.views }
  if (s.hostId && s.agentId) views[tabKey({ hostId: s.hostId, agentId: s.agentId })] = s.tab
  return {
    v: 1,
    tabs: s.tabs,
    views,
    active: s.hostId && s.agentId ? { hostId: s.hostId, agentId: s.agentId, tab: s.tab } : undefined,
    side: s.side,
    splitDir: s.splitDir,
    sidebarGroup: s.sidebarGroup,
    dockOpen: s.dockOpen,
    panelOpen: s.panelOpen,
    dismissed: s.dismissed ?? {},
    pins: s.pins ?? {},
  }
}

/** What a layout says to put back, for the hosts the app has now (a host removed since drops its tabs). */
export function restoreOf(l: Layout, hostIds: ReadonlySet<string>): Partial<LayoutSource> {
  const mine = <T extends TabRef>(r: T) => hostIds.has(r.hostId)
  const tabs = l.tabs.filter(mine)
  const active = l.active && mine(l.active) ? l.active : undefined
  return {
    tabs,
    views: Object.fromEntries(Object.entries(l.views).filter(([k]) => tabs.some((t) => tabKey(t) === k))),
    hostId: active?.hostId,
    agentId: active?.agentId,
    tab: active?.tab ?? 'overview',
    side: l.side.filter(mine),
    splitDir: l.splitDir,
    sidebarGroup: l.sidebarGroup,
    dockOpen: l.dockOpen,
    panelOpen: l.panelOpen,
    dismissed: Object.fromEntries(Object.entries(l.dismissed ?? {}).filter(([k]) => hostIds.has(k.slice(0, k.indexOf('/'))))),
    pins: Object.fromEntries(Object.entries(l.pins ?? {}).filter(([h]) => hostIds.has(h))),
  }
}

/**
 * Drops from the open tabs and panes the agents a host no longer has, once its first snapshot says which it has (before
 * that every agent looks missing). Returns what to change, or nothing when every agent is still there.
 */
export function pruneHost(s: LayoutSource, hostId: string, agents: Record<string, Agent>): Partial<LayoutSource> | undefined {
  const gone = (r: TabRef) => r.hostId === hostId && !(r.agentId in agents)
  // A dismissal of an agent the host lost is dead weight in the kept layout.
  const deadKeys = Object.keys(s.dismissed ?? {}).filter((k) => k.startsWith(`${hostId}/`) && !(k.slice(hostId.length + 1) in agents))
  if (!s.tabs.some(gone) && !s.side.some(gone) && !(s.agentId && gone({ hostId: s.hostId ?? '', agentId: s.agentId })) && deadKeys.length === 0) return undefined
  const tabs = s.tabs.filter((t) => !gone(t))
  const side = s.side.filter((p) => !gone(p))
  const patch: Partial<LayoutSource> = { tabs, side, views: Object.fromEntries(Object.entries(s.views).filter(([k]) => tabs.some((t) => tabKey(t) === k))) }
  if (deadKeys.length > 0) patch.dismissed = Object.fromEntries(Object.entries(s.dismissed ?? {}).filter(([k]) => !deadKeys.includes(k)))
  if (s.agentId && gone({ hostId: s.hostId ?? '', agentId: s.agentId })) {
    // The open agent is gone: a pane beside it takes over, else nothing is open (the tabs stay for the user to pick).
    const next = side[0]
    Object.assign(patch, next ? { hostId: next.hostId, agentId: next.agentId, tab: next.tab, side: side.slice(1) } : { agentId: undefined, tab: 'overview' as AgentTab })
  }
  return patch
}
