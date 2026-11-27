import { Code, ConnectError } from '@connectrpc/connect'
import { createStore, type StoreApi } from 'zustand/vanilla'
import type { DesktopApi, HostInfo, NewHost, SocketEvent, SocketHandle } from '../shared/api'
import { joinWaiting } from './derive'
import { createMeshClient, type MeshClient } from './actions'
import { HostConnection } from './connection'
import { messageLoader } from './messages'
import { hostRefresher } from './refresh'
import { isLive, noticeFor } from './notify'
import { appPrefs } from './prefs'
import { listSettings, startSetup } from './actions'
import { applyFrame, createHostStore, setSettings, storeSink, type HostStore } from './store'
import type { Frame } from './connection'
import { hostTransport } from './transport'
import { ActorClass } from './gen/agentmesh/v1/actor_pb'
import type { Agent } from './gen/agentmesh/v1/types_pb'
import type { ViewRequest } from './gen/agentmesh/v1/mesh_pb'
import { AgentRole } from './gen/agentmesh/v1/types_pb'
import { activate, cycleTab, closeTab, emptyWorkspace, focusPane, forgetHost, noteSpawned, pinSplit, pruneDismissed, setPaneTab, splitAtEdge, tabKey, unsplit, type Edge, type SplitDir, type TabRef, type WorkspaceState } from './workspace'
import { LAYOUT_KEY, layoutOf, parseLayout, pruneHost, restoreOf } from './layout'
import type { DockNewAgents } from './prefs'

/** A host the app is connected to (or trying to be): its state, its client, its connection. */
export interface HostRuntime {
  info: HostInfo
  store: HostStore
  client: MeshClient
  connection: HostConnection
  /** Stops the host's timers and calls (its connection is stopped separately). */
  stop(): void
  /** Loads a run's stored messages (once; `retry` after a failure) for its Messages tab: they are read lazily, not at connect. */
  loadMessages(runId: string, retry?: boolean): void
  /** A terminal's stream, by the path `OpenTerminal` gave. */
  openSocket(path: string, onEvent: (e: SocketEvent) => void): SocketHandle
}

/** What a host tells the app about, beyond its own state. */
export interface HostHooks {
  /** A worker started while the app was open (not one the user started, not history replayed): what the host's `app.dock.new_agents` says to do. */
  spawned(hostId: string, agent: Agent, mode: DockNewAgents): void
  /** An agent or the CLI asked the app to show a view (`OpenView`), on a host whose `app.open_views` is `openViews`. */
  view(hostId: string, request: ViewRequest, openViews: boolean): void
}

export function connectHost(api: Pick<DesktopApi, 'request' | 'openSocket' | 'shell'>, info: HostInfo, hooks?: HostHooks): HostRuntime {
  const store = createHostStore()
  const client = createMeshClient(hostTransport(api, info.id))
  const sink = storeSink(store)
  const loader = messageLoader(client, store, sink.log)
  const refresher = hostRefresher(client, store, sink.log)
  // Setting changes that arrive while the list is on its way are applied on top of it, not lost to it (and a late
  // list of an older connection is dropped).
  let settingsLoad = 0
  let pendingSettings: Frame[] = []
  const connection = new HostConnection({
    client,
    ...sink,
    onFrame: (f) => {
      const before = store.getState()
      sink.onFrame(f)
      if (f.case === 'event' && f.value.payload.case === 'settingChanged') pendingSettings.push(f)
      if (f.case === 'view') hooks?.view(info.id, f.value, appPrefs(before.settings).openViews)
      if (f.case === 'event') refresher.event(f.value)
      else if (f.case === 'snapshot' || f.case === 'resumed') refresher.synced()
      if (f.case === 'event') {
        const prefs = appPrefs(before.settings)
        const p = f.value.payload
        // A new agent the user didn't start themselves, seen as it happens: the dock's to show (and never to focus).
        if (p.case === 'agentSpawned' && p.value.agent && !before.agents[p.value.agent.id] && f.value.actor?.class !== ActorClass.DEVICE && isLive(f.value, Date.now(), before.clockSkewMs)) {
          hooks?.spawned(info.id, p.value.agent, prefs.dockNewAgents)
        }
        // The host's own preferences say whether and what to notify about its events.
        const n = prefs.notifyEnabled ? noticeFor(f.value, before.agents, Date.now(), before.clockSkewMs) : undefined
        if (n && prefs.notifyEvents.has(n.kind)) {
          api.shell.notify({ tag: `${info.id}:${n.tag}`, title: n.title, body: n.body, sound: prefs.notifySound, hostId: info.id, agentId: n.agentId, tab: n.tab })
        }
      }
      if (f.case === 'snapshot') {
        void loader.resync(f.value.runs)
        const mine = ++settingsLoad
        pendingSettings = []
        void listSettings(client)
          .then((r) => {
            if (mine !== settingsLoad) return
            store.setState(setSettings(r.settings))
            for (const e of pendingSettings) store.setState((s) => applyFrame(s, e))
            pendingSettings = []
          })
          .catch((e: unknown) => sink.log(`couldn't load the settings: ${e instanceof Error ? e.message : String(e)}`))
      }
    },
  })
  connection.start()
  refresher.start()
  return {
    info,
    store,
    client,
    connection,
    stop: () => {
      refresher.stop()
      loader.stop()
    },
    loadMessages: (runId, retry) => void (retry ? loader.retry(runId) : loader.ensure(runId)),
    openSocket: (path, onEvent) => api.openSocket(info.id, path, onEvent),
  }
}

export interface AppState extends WorkspaceState {
  loaded: boolean
  error?: string
  hosts: HostRuntime[]
  /** The host whose runs the sidebar shows. */
  hostId?: string
  /** The agent open in the main pane, with the host it belongs to. */
  agentId?: string
  /** Which view of that agent is showing. */
  tab: AgentTab
  /** The main pane shows the host's settings instead of an agent. */
  settingsOpen: boolean
  /** The main pane shows the host's pairing screen. */
  pairOpen: boolean
  /** The main pane shows every live agent as a grid ("Watch all"). */
  watchOpen: boolean
  /** The command palette is open. */
  paletteOpen: boolean
  /** The app started with no host and hasn't yet looked for one on this machine: the first-run screen connects by itself, once. */
  firstRun: boolean
  /** Why the setup agent couldn't be opened, and on which host (shown in that host's empty pane only). */
  setupError?: { hostId: string; message: string }
  /** How the sidebar groups the agents. */
  sidebarGroup: SidebarGroup
  /** The side panel beside the open agent is shown. */
  panelOpen: boolean
  /** The New workspace dialog is open. */
  workspaceOpen: boolean
  /** The run the New workspace dialog opens on (the palette's "Add an agent to the run"); unset: a new run. */
  workspaceRun?: string
  /** What the settings screen is filtered by when it opens (the palette's "Keyboard shortcuts"). */
  settingsQuery: string
  /** The main pane shows the host's quota usage and spend. */
  usageOpen: boolean
  /** The tab being dragged (not kept: it only shows the edges a tab can be dropped on). */
  dragTab?: TabRef
  /** The projects the user pinned in the New workspace dialog, by host id (device-local, with the layout). */
  pins: Record<string, string[]>
}

/** How the sidebar groups the agents: by run (each as a tree from its lead), by repo, or by status bucket. */
export type SidebarGroup = 'run' | 'project' | 'state'

export type AgentTab = 'overview' | 'messages' | 'chat' | 'terminal'

export interface AppModel {
  store: StoreApi<AppState>
  api: DesktopApi
  load(): Promise<void>
  addHost(h: NewHost): Promise<void>
  /** Connects to the daemon found on this machine (`api.local`) and saves it as a host; `setup`: opens the setup chat on it. */
  connectLocal(id: string, setup?: boolean): Promise<void>
  /** Opens the chat with the setup agent of a host, starting it if it isn't running. */
  openSetup(hostId: string): Promise<void>
  removeHost(id: string): Promise<void>
  selectHost(id: string): void
  selectAgent(hostId: string, agentId: string | undefined, tab?: AgentTab): void
  setTab(tab: AgentTab): void
  /** Closes an agent's tab; when it was the open one, the next to its right shows. */
  closeTab(ref: TabRef): void
  /** The tab after (1) or before (-1) the open one. */
  cycleTab(by: 1 | -1): void
  /** Shows or hides the live dock; showing it clears its count. */
  setDock(open: boolean): void
  /** Shows an agent in a pane beside the main one (the dock's "pin as a split", the tab's split button). */
  pinSplit(hostId: string, agentId: string): void
  /** A tab is being dragged (or the drag ended: `undefined`). */
  setDragTab(ref: TabRef | undefined): void
  /** A tab dropped on an edge of the main pane: it opens as a split there. */
  splitAtEdge(ref: TabRef, edge: Edge): void
  /** Takes a pane away from beside the main one; its tab stays open. */
  unsplit(ref: TabRef): void
  /** Changes the view a pane beside the main one is on. */
  setPaneTab(ref: TabRef, tab: AgentTab): void
  /** Makes a pane beside the main one the main pane. */
  focusPane(ref: TabRef): void
  setSplitDir(dir: SplitDir): void
  /** Takes a tile off the dock until its agent's state changes. */
  dismissTile(hostId: string, agentId: string): void
  openSettings(open: boolean, query?: string): void
  openPalette(open: boolean): void
  setSidebarGroup(g: SidebarGroup): void
  setPanel(open: boolean): void
  openWorkspace(open: boolean, runId?: string): void
  openPairing(open: boolean): void
  /** Shows or hides the grid of every live agent. */
  openWatch(open: boolean): void
  openUsage(open: boolean): void
  /** Pins a project of a host (or unpins it): pinned projects come first in the New workspace dialog. */
  togglePin(hostId: string, path: string): void
}

/** Shows what an `OpenView` request names: an agent's chat or terminal, the host's settings, or a run (its lead). Does nothing for an agent or run the host doesn't have. */
export function showView(app: AppModel, hostId: string, v: ViewRequest): void {
  const host = app.store.getState().hosts.find((h) => h.info.id === hostId)
  if (!host) return
  const s = host.store.getState()
  switch (v.view) {
    case 'agent':
    case 'terminal':
      if (s.agents[v.target]) app.selectAgent(hostId, v.target, v.view === 'agent' ? 'chat' : 'terminal')
      return
    case 'run': {
      const lead = Object.values(s.agents).find((a) => a.runId === v.target && a.role === AgentRole.ROOT)
      if (lead) app.selectAgent(hostId, lead.id)
      else if (s.runs[v.target]) app.selectHost(hostId)
      return
    }
    case 'settings':
      app.selectHost(hostId)
      app.openSettings(true, v.target)
  }
}

/** What went wrong opening the setup agent, in words the user can act on. */
export function setupMessage(e: unknown): string {
  const err = ConnectError.from(e)
  if (err.code === Code.Unimplemented) return "This host's daemon is older than the app and doesn't have the setup assistant yet: update it (run ./install.sh in the agent-mesh checkout), then try again."
  return err.rawMessage || err.message
}

/** The app's own state: which hosts, and what is selected. Everything else lives on a host. */
export function createAppModel(api: DesktopApi): AppModel {
  const store = createStore<AppState>(() => ({ loaded: false, hosts: [], tab: 'overview', settingsOpen: false, pairOpen: false, watchOpen: false, usageOpen: false, paletteOpen: false, firstRun: false, sidebarGroup: 'run', panelOpen: true, workspaceOpen: false, settingsQuery: '', pins: {}, ...emptyWorkspace() }))
  const set = store.setState
  // What the tray shows: how many things wait on the user across the hosts, and what closing the window does
  // (the first host's `app.tray.close_to_tray`: the tray is the app's, not one host's).
  let waiting = -1
  const recount = () => {
    const n = store.getState().hosts.reduce((sum, h) => sum + joinWaiting(h.store.getState().waiting, h.store.getState().runs, h.store.getState().agents).length, 0)
    if (n !== waiting) {
      waiting = n
      api.shell.setWaiting(n)
    }
  }
  const configure = () => {
    const first = store.getState().hosts[0]
    api.shell.configure({ closeToTray: appPrefs(first?.store.getState().settings ?? {}).closeToTray })
  }
  const hooks: HostHooks = {
    spawned: (hostId, agent, mode) => set((s) => noteSpawned(s, { hostId, agentId: agent.id }, mode, Date.now())),
    // An agent asking to show the user something acts only while they are looking at the window, unless they said it may anyway.
    view: (hostId, v, openViews) => {
      if (!openViews && !(typeof document !== 'undefined' && document.hasFocus())) return
      showView(model, hostId, v)
    },
  }
  // The window's layout is kept for the next start (`layout.ts`): written a moment after it last changed.
  let saved = ''
  let saveTimer: ReturnType<typeof setTimeout> | undefined
  const save = () => {
    clearTimeout(saveTimer)
    saveTimer = setTimeout(() => {
      try {
        const text = JSON.stringify(layoutOf(store.getState()))
        if (text !== saved) {
          saved = text
          localStorage.setItem(LAYOUT_KEY, text)
        }
      } catch {
        // No storage: the layout isn't kept, and nothing else is affected.
      }
    }, 300)
  }
  let restored = false
  store.subscribe((s, prev) => {
    if (restored && (s.tabs !== prev.tabs || s.views !== prev.views || s.hostId !== prev.hostId || s.agentId !== prev.agentId || s.tab !== prev.tab || s.side !== prev.side || s.splitDir !== prev.splitDir || s.sidebarGroup !== prev.sidebarGroup || s.panelOpen !== prev.panelOpen || s.dockOpen !== prev.dockOpen || s.dismissed !== prev.dismissed || s.pins !== prev.pins)) save()
  })
  const unwatch = new Map<string, () => void>()
  const watch = (rt: HostRuntime) =>
    unwatch.set(
      rt.info.id,
      rt.store.subscribe((s, prev) => {
        // Its first snapshot says which agents it has: the tabs and panes of ones it lost since the last start go.
        if (s.synced && !prev.synced) {
          const patch = pruneHost(store.getState(), rt.info.id, s.agents)
          if (patch) set(patch)
        }
        if (s.waiting !== prev.waiting || s.agents !== prev.agents || s.runs !== prev.runs) recount()
        if (s.agents !== prev.agents) {
          // A dismissed tile that came back when its agent's state changed stays back.
          const held = store.getState().dismissed
          const kept = pruneDismissed(held, rt.info.id, s.agents)
          if (kept !== held) set({ dismissed: kept })
        }
        if (s.settings !== prev.settings && store.getState().hosts[0] === rt) configure()
      }),
    )
  api.shell.onNotificationClick((c) => {
    // A click on a notification of a host that has since been removed goes nowhere.
    if (store.getState().hosts.some((h) => h.info.id === c.hostId)) set((s) => ({ ...activate(s, c.hostId, c.agentId, c.tab), settingsOpen: false, pairOpen: false, watchOpen: false, usageOpen: false }))
  })
  /** Takes a host the main process saved and connects to it; `openSetup`: opens the setup chat. */
  async function adopt(info: HostInfo, openSetup = false) {
    const rt = connectHost(api, info, hooks)
    watch(rt)
    set((s) => ({ hosts: [...s.hosts, rt], hostId: info.id, agentId: undefined, firstRun: false }))
    recount()
    configure()
    // The first run's connection is a new install: the first thing the user sees is the assistant. A host added any
    // other way (by hand, or through the add dialog's "Look for a daemon") may have weeks of runs: starting an agent
    // there is the user's call (the empty pane offers it).
    if (openSetup) void model.openSetup(info.id)
  }
  let loading: Promise<void> | undefined
  async function loadHosts() {
    try {
      const infos = await api.hosts.list()
      const hosts = infos.map((i) => connectHost(api, i, hooks))
      hosts.forEach(watch)
      let layout: ReturnType<typeof parseLayout>
      try {
        layout = parseLayout(localStorage.getItem(LAYOUT_KEY))
      } catch {
        layout = undefined
      }
      const back = layout ? restoreOf(layout, new Set(hosts.map((h) => h.info.id))) : {}
      saved = layout ? JSON.stringify(layout) : ''
      set({ ...back, loaded: true, hosts, hostId: back.hostId ?? hosts[0]?.info.id, error: undefined, firstRun: hosts.length === 0 && !api.hosts.fixed })
      restored = true
      recount()
      configure()
    } catch (e) {
      set({ loaded: true, error: String(e) })
    }
  }
  const model: AppModel = {
    store,
    api,
    // Once: a second call (React's strict mode runs effects twice) must not open every connection again.
    load: () => (loading ??= loadHosts()),
    async addHost(h) {
      await adopt(await api.hosts.add(h))
    },
    async connectLocal(id, setup = false) {
      await adopt(await api.local.connect(id), setup)
    },
    async openSetup(hostId) {
      const host = store.getState().hosts.find((h) => h.info.id === hostId)
      if (!host) return
      try {
        const { agent, run } = await startSetup(host.client)
        if (!agent) throw new Error('the host started no agent')
        // Put them in the host's state now, so the tab doesn't say "not on this host" until the event arrives.
        host.store.setState((st) => ({ agents: { ...st.agents, [agent.id]: agent }, runs: run ? { ...st.runs, [run.id]: run } : st.runs }))
        set((st) => ({ setupError: st.setupError?.hostId === hostId ? undefined : st.setupError }))
        set((st) => ({ ...activate(st, hostId, agent.id, 'chat'), hostId, settingsOpen: false, pairOpen: false, watchOpen: false, usageOpen: false }))
      } catch (e) {
        set({ setupError: { hostId, message: setupMessage(e) } })
      }
    },
    async removeHost(id) {
      await api.hosts.remove(id)
      const gone = store.getState().hosts.find((h) => h.info.id === id)
      gone?.connection.stop()
      gone?.stop()
      unwatch.get(id)?.()
      unwatch.delete(id)
      set((s) => {
        const hosts = s.hosts.filter((h) => h.info.id !== id)
        const { [id]: _pins, ...pins } = s.pins
        return { ...forgetHost(s, id), pins, hosts, hostId: s.hostId === id ? hosts[0]?.info.id : s.hostId, agentId: s.hostId === id ? undefined : s.agentId }
      })
      recount()
      configure()
    },
    selectHost: (id) =>
      set((s) => ({ hostId: id, agentId: undefined, settingsOpen: false, pairOpen: false, watchOpen: false, usageOpen: false, views: s.hostId && s.agentId ? { ...s.views, [tabKey({ hostId: s.hostId, agentId: s.agentId })]: s.tab } : s.views })),
    selectAgent: (hostId, agentId, tab) =>
      set((s) => (agentId ? { ...activate(s, hostId, agentId, tab), settingsOpen: false, pairOpen: false, watchOpen: false, usageOpen: false } : { hostId, agentId: undefined, tab: tab ?? 'overview', settingsOpen: false, pairOpen: false, watchOpen: false, usageOpen: false })),
    openSettings: (open, query = '') => set({ settingsOpen: open, pairOpen: false, watchOpen: false, usageOpen: false, settingsQuery: open ? query : '' }),
    openPalette: (open) => set({ paletteOpen: open }),
    setSidebarGroup: (sidebarGroup) => set({ sidebarGroup }),
    setPanel: (panelOpen) => set({ panelOpen }),
    openWorkspace: (workspaceOpen, runId) => set({ workspaceOpen, workspaceRun: workspaceOpen ? runId : undefined }),
    openPairing: (open) => set({ pairOpen: open, settingsOpen: false, watchOpen: false, usageOpen: false }),
    openWatch: (open) => set({ watchOpen: open, settingsOpen: false, pairOpen: false, usageOpen: false }),
    openUsage: (open) => set({ usageOpen: open, settingsOpen: false, pairOpen: false, watchOpen: false }),
    setTab: (tab) => set({ tab }),
    closeTab: (ref) => set((s) => closeTab(s, ref)),
    cycleTab: (by) => {
      const next = cycleTab(store.getState(), by)
      if (next) set((s) => ({ ...activate(s, next.hostId, next.agentId), settingsOpen: false, pairOpen: false, watchOpen: false, usageOpen: false }))
    },
    setDock: (open) => set(open ? { dockOpen: true, dockBadge: 0 } : { dockOpen: false }),
    pinSplit: (hostId, agentId) => set((s) => pinSplit(s, { hostId, agentId })),
    unsplit: (ref) => set((s) => unsplit(s, ref)),
    setDragTab: (dragTab) => set({ dragTab }),
    splitAtEdge: (ref, edge) => set((s) => ({ ...splitAtEdge(s, ref, edge), dragTab: undefined })),
    setPaneTab: (ref, tab) => set((s) => setPaneTab(s, ref, tab)),
    focusPane: (ref) => set((s) => focusPane(s, ref)),
    setSplitDir: (splitDir) => set({ splitDir }),
    togglePin: (hostId, path) =>
      set((s) => {
        const cur = s.pins[hostId] ?? []
        const next = cur.includes(path) ? cur.filter((p) => p !== path) : [...cur, path]
        const { [hostId]: _old, ...rest } = s.pins
        return { pins: next.length > 0 ? { ...rest, [hostId]: next } : rest }
      }),
    dismissTile: (hostId, agentId) => {
      const state = store.getState().hosts.find((h) => h.info.id === hostId)?.store.getState().agents[agentId]?.state
      if (state !== undefined) set((s) => ({ dismissed: { ...s.dismissed, [tabKey({ hostId, agentId })]: state } }))
    },
  }
  return model
}
