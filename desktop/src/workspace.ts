import { AgentState, type Agent } from './gen/agentmesh/v1/types_pb'
import type { AgentTab } from './model'

/**
 * The window's workspace: the agents open as tabs, the view each tab was last on, and the live dock's marks. Pure
 * functions over a slice of the app state, so what opening, closing and cycling do can be checked without a window.
 */
export interface TabRef {
  hostId: string
  agentId: string
}

export const tabKey = (r: TabRef) => `${r.hostId}/${r.agentId}`

export interface WorkspaceState {
  /** The agents open as tabs, in order, on any host. */
  tabs: TabRef[]
  /** The view each open tab was last on (the active one's is `tab`). */
  views: Record<string, AgentTab>
  /** Agents that started while the app was open, by tab key: when (ms) they were marked new in the dock. */
  newTiles: Record<string, number>
  /** A tile the user dismissed, by tab key: what the agent's state was; it comes back when that changes. */
  dismissed: Record<string, AgentState>
  dockOpen: boolean
  /** How many workers started while the dock was off screen or the setting says badge. */
  dockBadge: number
}

export const emptyWorkspace = (): WorkspaceState => ({ tabs: [], views: {}, newTiles: {}, dismissed: {}, dockOpen: true, dockBadge: 0 })

/** How long a tile stays marked new if nobody clicks it. */
export const NEW_FOR_MS = 120_000

interface Selection {
  hostId?: string
  agentId?: string
  tab: AgentTab
}

const same = (a: TabRef, b: TabRef) => a.hostId === b.hostId && a.agentId === b.agentId

/**
 * Makes an agent the active tab, opening it if it isn't open. Its view is the one asked for, else the one it was
 * last on, else the overview; the view of the tab being left is kept. Clears the agent's new mark.
 */
export function activate(s: WorkspaceState & Selection, hostId: string, agentId: string, tab?: AgentTab): Partial<WorkspaceState & Selection> {
  const ref = { hostId, agentId }
  const key = tabKey(ref)
  const views = { ...s.views }
  if (s.hostId && s.agentId) views[tabKey({ hostId: s.hostId, agentId: s.agentId })] = s.tab
  const { [key]: _new, ...newTiles } = s.newTiles
  const shown = tab ?? views[key] ?? 'overview'
  views[key] = shown
  return { hostId, agentId, tab: shown, tabs: s.tabs.some((t) => same(t, ref)) ? s.tabs : [...s.tabs, ref], views, newTiles }
}

/** Opens an agent as a tab without making it active (and without changing what shows). */
export function openInBackground(s: WorkspaceState, ref: TabRef): Partial<WorkspaceState> {
  return s.tabs.some((t) => same(t, ref)) ? {} : { tabs: [...s.tabs, ref] }
}

/** Closes a tab. When it was the active one the next tab to its right is (else the one to its left, else none). */
export function closeTab(s: WorkspaceState & Selection, ref: TabRef): Partial<WorkspaceState & Selection> {
  const i = s.tabs.findIndex((t) => same(t, ref))
  if (i < 0) return {}
  const tabs = s.tabs.filter((_, j) => j !== i)
  const key = tabKey(ref)
  const { [key]: _view, ...views } = s.views
  const { [key]: _new, ...newTiles } = s.newTiles
  const active = s.hostId === ref.hostId && s.agentId === ref.agentId
  if (!active) return { tabs, views, newTiles }
  const next = tabs[i] ?? tabs[i - 1]
  if (!next) return { tabs, views, newTiles, agentId: undefined, tab: 'overview' }
  return { tabs, views, newTiles, hostId: next.hostId, agentId: next.agentId, tab: views[tabKey(next)] ?? 'overview' }
}

/** The tab `by` places after the active one (wrapping); with none active, the first (or last, going back). */
export function cycleTab(s: WorkspaceState & Selection, by: 1 | -1): TabRef | undefined {
  if (s.tabs.length === 0) return undefined
  const at = s.tabs.findIndex((t) => t.hostId === s.hostId && t.agentId === s.agentId)
  if (at < 0) return by === 1 ? s.tabs[0] : s.tabs[s.tabs.length - 1]
  return s.tabs[(at + by + s.tabs.length) % s.tabs.length]
}

/** Drops what belongs to a host that was removed. */
export function forgetHost(s: WorkspaceState, hostId: string): Partial<WorkspaceState> {
  const mine = (key: string) => key.startsWith(`${hostId}/`)
  const keep = <T>(o: Record<string, T>) => Object.fromEntries(Object.entries(o).filter(([k]) => !mine(k)))
  return { tabs: s.tabs.filter((t) => t.hostId !== hostId), views: keep(s.views), newTiles: keep(s.newTiles), dismissed: keep(s.dismissed) }
}

/**
 * Forgets the dismissed tiles of a host whose agent is no longer in the state it was dismissed in (or is gone): the
 * tile came back when the state changed, and a later return to that state is a new thing to see (an agent that needs
 * input again). Returns `dismissed` itself when nothing changed.
 */
export function pruneDismissed(dismissed: Record<string, AgentState>, hostId: string, agents: Record<string, Agent>): Record<string, AgentState> {
  const stale = Object.keys(dismissed).filter((k) => k.startsWith(`${hostId}/`) && agents[k.slice(hostId.length + 1)]?.state !== dismissed[k])
  if (stale.length === 0) return dismissed
  return Object.fromEntries(Object.entries(dismissed).filter(([k]) => !stale.includes(k)))
}

/** An agent that started while the app was open: what the host's `app.dock.new_agents` says to do about it. */
export function noteSpawned(s: WorkspaceState, ref: TabRef, mode: 'dock' | 'badge' | 'tab' | 'none', now: number): Partial<WorkspaceState> {
  switch (mode) {
    case 'dock':
      // With the dock off screen the mark isn't seen: the toggle counts it instead.
      return { newTiles: { ...s.newTiles, [tabKey(ref)]: now }, dockBadge: s.dockOpen ? s.dockBadge : s.dockBadge + 1 }
    case 'badge':
      return { dockBadge: s.dockBadge + 1 }
    case 'tab':
      return openInBackground(s, ref)
    default:
      return {}
  }
}
