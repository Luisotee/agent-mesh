import { Code, ConnectError } from '@connectrpc/connect'
import { createStore, type StoreApi } from 'zustand/vanilla'
import type { DesktopApi, HostInfo, NewHost, SocketEvent, SocketHandle } from '../shared/api'
import { waitingCount } from './derive'
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
import type { Agent, Event } from './gen/agentmesh/v1/types_pb'
import type { ViewRequest } from './gen/agentmesh/v1/mesh_pb'
import { AgentRole } from './gen/agentmesh/v1/types_pb'
import { activate, cycleTab, closeTab, projectTabs, emptyWorkspace, focusPane, forgetHost, noteSpawned, pinSplit, pruneDismissed, setPaneTab, splitAtEdge, tabKey, unsplit, type Edge, type SplitDir, type TabRef, type WorkspaceState } from './workspace'
import { LAYOUT_KEY, layoutOf, parseLayout, pruneHost, restoreOf } from './layout'
import { agentProject } from './sidebar'
import { applyShellEvent, isShellTab, shellIdOf, shellProject, shellTabId } from './shells'
import { closeShell as callCloseShell, listShells, openShell } from './actions'
import type { Shell } from './gen/agentmesh/v1/mesh_pb'
import type { DockNewAgents } from './prefs'

/** A host the app is connected to (or trying to be): its state, its client, its connection. */
export interface HostRuntime {
  info: HostInfo
  store: HostStore
  client: MeshClient
  connection: HostConnection
  /** Stops the host's timers and calls (its connection is stopped separately). */
  stop(): void
  /** Loads a run's stored messages (once; `retry` after a failure) for its agents' chats: they are read lazily, not at connect. */
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
  /** A project shell of the host opened or ended (`ShellOpened`, `ShellClosed`). */
  shell(hostId: string, event: Event): void
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
      if (f.case === 'event' && (f.value.payload.case === 'shellOpened' || f.value.payload.case === 'shellClosed')) hooks?.shell(info.id, f.value)
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
  /** The find bar is open (find in the window, Mod+F). */
  findOpen: boolean
  /** The app started with no host and hasn't yet looked for one on this machine: the first-run screen connects by itself, once. */
  firstRun: boolean
  /** Why the setup agent couldn't be opened, and on which host (shown in that host's empty pane only). */
  setupError?: { hostId: string; message: string }
  /** The sidebar is folded to a strip of icons (Ctrl+B). */
  sidebarCollapsed: boolean
  /** Projects the user took off the sidebar, by host id (device-local; one comes back while something in it waits on the user). */
  hidden: Record<string, string[]>
  /** The project whose settings dialog is open. */
  projectSettings?: { hostId: string; repo: string }
  /** The run whose title is being edited in the sidebar (F2, or its menu's Rename). */
  renaming?: { hostId: string; runId: string }
  /** Tabs closed in this session, the latest last (Ctrl+Shift+T opens them again; not kept). */
  closedTabs: TabRef[]
  /** The side panel beside the open agent is shown. */
  panelOpen: boolean
  /** The Send a message dialog, to this agent. */
  messageTo?: { hostId: string; agentId: string }
  /** The Rewind dialog, for this agent (its chat shows it: the prompts are the chat's). */
  rewindFor?: { hostId: string; agentId: string }
  /** Set to show the open agent's model control (the panel opens and unfolds its Details); cleared once its Details unfolded, so a later mount of the panel opens folded. */
  modelShown: boolean
  /** The New workspace dialog is open. */
  workspaceOpen: boolean
  /** The run the New workspace dialog opens on (the palette's "Add an agent to the run"); unset: a new run. */
  workspaceRun?: string
  /** The project the New workspace dialog opens on (a project's "New lead here"), and its host (unset: the selected one). */
  workspaceRepo?: string
  workspaceHost?: string
  /** What the settings screen is filtered by when it opens (the palette's "Keyboard shortcuts"). */
  settingsQuery: string
  /** A queued prompt taken back to edit, for its agent's composer to take (and clear). */
  composerFill?: { agentId: string; text: string; /** Images to put back in its strip (a rewound prompt's), by host path. */ images?: string[] }
  /** The main pane shows the host's quota usage and spend. */
  usageOpen: boolean
  /** The tab being dragged (not kept: it only shows the edges a tab can be dropped on). */
  dragTab?: TabRef
  /** Views an agent or the CLI asked the app to show, waiting for the user to open or dismiss them (the toasts). */
  viewRequests: ViewRequestRow[]
  /** The projects the user pinned (the sidebar and the New workspace dialog list them first), by host id (device-local, with the layout). */
  pins: Record<string, string[]>
  /** A message an `OpenView` plan names: its card scrolls into view and is marked for a moment, then the card clears this. */
  focusMessage?: { hostId: string; id: string }
  /** The project shells of each host, once listed (absent: not read yet, so a restored shell tab is not judged gone). */
  shells: Record<string, Shell[]>
  /** Why a host's shells could not be read (its tabs are then not judged, and say so). */
  shellsError: Record<string, string>
  /** A shell tab being closed: the user is asked whether to end the shell or keep it running. */
  closingShell?: TabRef
}

export type AgentTab = 'chat' | 'terminal'

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
  closeTab(ref: TabRef, force?: boolean): void
  /** Reads a host's project shells (on connect, and when the + menu opens). */
  loadShells(hostId: string): Promise<void>
  /** Starts a shell in a project's folder and opens its tab. */
  newShell(hostId: string, repo: string): Promise<void>
  /** Ends a shell (the host stops it) and closes its tab. */
  endShell(hostId: string, shellId: string): Promise<void>
  /** Answers the close question: `keep` closes the tab and leaves the shell running; else the question goes away. */
  closeShellTab(ref: TabRef, keep: boolean): void
  /** Moves a tab to where another is in the strip (a tab dragged onto another). */
  moveTab(ref: TabRef, to: TabRef): void
  /** The tab after (1) or before (-1) the open one, in its project's tab bar. */
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
  openFind(open: boolean): void
  setSidebarCollapsed(collapsed: boolean): void
  /** Takes a project off the sidebar (or puts it back). */
  /** Takes a project off the sidebar or puts it back: `on` sets it (an explicit hide or show), else it flips. */
  toggleHidden(hostId: string, repo: string, on?: boolean): void
  openProjectSettings(project: { hostId: string; repo: string } | undefined): void
  /** Starts (or ends) editing a run's title in the sidebar. */
  renameRun(run: { hostId: string; runId: string } | undefined): void
  /** Opens the last tab closed in this session again. */
  reopenTab(): void
  setPanel(open: boolean): void
  /** Opens the side panel on the open agent's model control. */
  showModel(): void
  /** Opens (or, undefined, closes) the Send a message dialog to an agent. */
  openMessage(to: { hostId: string; agentId: string } | undefined): void
  /** Opens (or, undefined, closes) the Rewind dialog of an agent. */
  openRewind(of: { hostId: string; agentId: string } | undefined): void
  /** The Details unfolded for `showModel`: done. */
  modelRevealed(): void
  /** Opens the New workspace dialog: on a run (to add an agent to it) or a project (to start a lead there), on a host. */
  openWorkspace(open: boolean, runId?: string, at?: { hostId: string; repo: string }): void
  openPairing(open: boolean): void
  /** Shows or hides the grid of every live agent. */
  openWatch(open: boolean): void
  openUsage(open: boolean): void
  /** Shows what a toast names and takes the toast away. */
  openViewRequest(id: number): void
  clearFocusMessage(): void
  /** Mark a message for the chat's card of it to show. If no card claims it within 10 s (its messages did not load), it is forgotten. */
  focusOnMessage(focus: { hostId: string; id: string }): void
  /** The card for the focused message is showing: its own timer takes over from the 10 s fallback. */
  claimFocusMessage(): void
  /** Takes a toast away. */
  dismissViewRequest(id: number): void
  /** Pins a project of a host (or unpins it): pinned projects come first in the New workspace dialog. */
  /** Pins a project or unpins it: `on` sets it (pinning twice keeps it pinned), else it flips. */
  togglePin(hostId: string, path: string, on?: boolean): void
}

export interface ViewRequestRow {
  id: number
  hostId: string
  request: ViewRequest
}

let viewSeq = 0

/**
 * An agent or the CLI asked the app to show a view. It never switches the user's view by itself: it is a toast they open or
 * dismiss. Only when the user turned `app.open_views` on does it switch at once, and even then never while they are typing (a
 * terminal, a text field): a switch there would send their keystrokes somewhere else.
 */
export function offerView(app: AppModel, hostId: string, v: ViewRequest, openViews: boolean): void {
  if (openViews && !typingNow()) return showView(app, hostId, v)
  app.store.setState((s) => ({
    viewRequests: [...s.viewRequests.filter((r) => !(r.hostId === hostId && r.request.view === v.view && r.request.target === v.target)), { id: ++viewSeq, hostId, request: v }].slice(-3),
  }))
}

/** Whether the user is typing now: a text field or a terminal has the focus, so a view switched under them would take their keystrokes. */
export function typingNow(): boolean {
  if (typeof document === 'undefined') return false
  const el = document.activeElement as HTMLElement | null
  return !!el && (el.tagName === 'INPUT' || el.tagName === 'TEXTAREA' || el.tagName === 'SELECT' || el.isContentEditable)
}

/**
 * Shows what an `OpenView` request names: an agent's chat or terminal, its messages (in its chat, or a plan there), the agent a pull request
 * belongs to (with the side panel, where its pull requests are), the host's settings, or a run (its lead). Does nothing for an
 * agent or run the host doesn't have.
 */
export function showView(app: AppModel, hostId: string, v: ViewRequest): void {
  const host = app.store.getState().hosts.find((h) => h.info.id === hostId)
  if (!host) return
  const s = host.store.getState()
  switch (v.view) {
    case 'agent':
    case 'terminal':
      if (s.agents[v.target]) app.selectAgent(hostId, v.target, v.view === 'agent' ? 'chat' : 'terminal')
      return
    case 'messages':
    case 'plan':
      if (!s.agents[v.target]) return
      if (v.view === 'plan' && v.messageId) {
        app.focusOnMessage({ hostId, id: v.messageId })
      }
      // Its messages are in its chat, where they were sent.
      app.selectAgent(hostId, v.target, 'chat')
      return
    case 'pr':
      if (!s.agents[v.agentId]) return
      app.selectAgent(hostId, v.agentId)
      app.setPanel(true)
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
  const store = createStore<AppState>(() => ({ loaded: false, hosts: [], tab: 'chat', settingsOpen: false, pairOpen: false, watchOpen: false, usageOpen: false, paletteOpen: false, findOpen: false, firstRun: false, sidebarCollapsed: false, hidden: {}, closedTabs: [], panelOpen: true, modelShown: false, workspaceOpen: false, settingsQuery: '', pins: {}, viewRequests: [], shells: {}, shellsError: {}, ...emptyWorkspace() }))
  const set = store.setState
  // What the tray shows: how many things wait on the user across the hosts, and what closing the window does
  // (the first host's `app.tray.close_to_tray`: the tray is the app's, not one host's).
  let waiting = -1
  const recount = () => {
    const n = store.getState().hosts.reduce((sum, h) => sum + waitingCount(h.store.getState().waiting, h.store.getState().permissions, h.store.getState().runs, h.store.getState().agents), 0)
    if (n !== waiting) {
      waiting = n
      api.shell.setWaiting(n)
    }
  }
  const configure = () => {
    const first = store.getState().hosts[0]
    api.shell.configure({ closeToTray: appPrefs(first?.store.getState().settings ?? {}).closeToTray })
  }
  const shellEvents: Record<string, number> = {} // per host: the shell events seen, to notice one that came while ListShells was on its way
  const hooks: HostHooks = {
    spawned: (hostId, agent, mode) => set((s) => noteSpawned(s, { hostId, agentId: agent.id }, mode, Date.now())),
    view: (hostId, v, openViews) => offerView(model, hostId, v, openViews),
    shell: (hostId, e) => {
      shellEvents[hostId] = (shellEvents[hostId] ?? 0) + 1 // a listing on its way may predate it: loadShells asks again
      set((s) => {
        const was = s.shells[hostId]
        const now = applyShellEvent(was, e.payload)
        return now && now !== was ? { shells: { ...s.shells, [hostId]: now } } : s
      })
    },
  }
  // The window's layout is kept for the next start (`layout.ts`): written a moment after it last changed.
  let saved = ''
  let saveTimer: ReturnType<typeof setTimeout> | undefined
  let focusFallback: ReturnType<typeof setTimeout> | undefined
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
    if (restored && (s.tabs !== prev.tabs || s.views !== prev.views || s.hostId !== prev.hostId || s.agentId !== prev.agentId || s.tab !== prev.tab || s.side !== prev.side || s.splitDir !== prev.splitDir || s.sidebarCollapsed !== prev.sidebarCollapsed || s.hidden !== prev.hidden || s.panelOpen !== prev.panelOpen || s.dockOpen !== prev.dockOpen || s.dismissed !== prev.dismissed || s.pins !== prev.pins)) save()
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
          void model.loadShells(rt.info.id)
        }
        if (s.waiting !== prev.waiting || s.permissions !== prev.permissions || s.agents !== prev.agents || s.runs !== prev.runs) recount()
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
        const { [id]: _hidden, ...hidden } = s.hidden
        const { [id]: _shells, ...shells } = s.shells
        const { [id]: _shellsError, ...shellsError } = s.shellsError
        return { ...forgetHost(s, id), shells, shellsError, pins, hidden, closedTabs: s.closedTabs.filter((t) => t.hostId !== id), viewRequests: s.viewRequests.filter((r) => r.hostId !== id), hosts, hostId: s.hostId === id ? hosts[0]?.info.id : s.hostId, agentId: s.hostId === id ? undefined : s.agentId }
      })
      recount()
      configure()
    },
    selectHost: (id) =>
      set((s) => ({ hostId: id, agentId: undefined, settingsOpen: false, pairOpen: false, watchOpen: false, usageOpen: false, views: s.hostId && s.agentId ? { ...s.views, [tabKey({ hostId: s.hostId, agentId: s.agentId })]: s.tab } : s.views })),
    selectAgent: (hostId, agentId, tab) =>
      set((s) => (agentId ? { ...activate(s, hostId, agentId, tab), settingsOpen: false, pairOpen: false, watchOpen: false, usageOpen: false } : { hostId, agentId: undefined, tab: tab ?? 'chat', settingsOpen: false, pairOpen: false, watchOpen: false, usageOpen: false })),
    openSettings: (open, query = '') => set({ settingsOpen: open, pairOpen: false, watchOpen: false, usageOpen: false, settingsQuery: open ? query : '' }),
    openPalette: (open) => set({ paletteOpen: open }),
    openFind: (findOpen) => set({ findOpen }),
    setSidebarCollapsed: (sidebarCollapsed) => set({ sidebarCollapsed }),
    toggleHidden: (hostId, repo, on) =>
      set((s) => {
        const cur = s.hidden[hostId] ?? []
        const next = (on ?? !cur.includes(repo)) ? [...cur.filter((p) => p !== repo), repo] : cur.filter((p) => p !== repo)
        const { [hostId]: _old, ...rest } = s.hidden
        return { hidden: next.length > 0 ? { ...rest, [hostId]: next } : rest }
      }),
    openProjectSettings: (projectSettings) => set({ projectSettings }),
    renameRun: (renaming) => set({ renaming }),
    reopenTab: () => {
      const s = store.getState()
      const last = s.closedTabs.at(-1)
      if (!last) return
      set({ closedTabs: s.closedTabs.slice(0, -1) })
      const h = s.hosts.find((x) => x.info.id === last.hostId)
      const shellBack = isShellTab(last.agentId) && s.shells[last.hostId]?.some((x) => shellTabId(x.id) === last.agentId && !x.exited)
      if (shellBack || h?.store.getState().agents[last.agentId]) model.selectAgent(last.hostId, last.agentId)
      else model.reopenTab()
    },
    setPanel: (panelOpen) => set({ panelOpen }),
    showModel: () => set({ panelOpen: true, modelShown: true }),
    modelRevealed: () => set({ modelShown: false }),
    openMessage: (messageTo) => set({ messageTo }),
    openRewind: (rewindFor) => set({ rewindFor }),
    openWorkspace: (workspaceOpen, runId, at) => set({ workspaceOpen, workspaceRun: workspaceOpen ? runId : undefined, workspaceRepo: workspaceOpen ? at?.repo : undefined, workspaceHost: workspaceOpen ? at?.hostId : undefined }),
    openPairing: (open) => set({ pairOpen: open, settingsOpen: false, watchOpen: false, usageOpen: false }),
    openWatch: (open) => set({ watchOpen: open, settingsOpen: false, pairOpen: false, usageOpen: false }),
    openUsage: (open) => set({ usageOpen: open, settingsOpen: false, pairOpen: false, watchOpen: false }),
    setTab: (tab) => set({ tab }),
    loadShells: async (hostId) => {
      const h = store.getState().hosts.find((x) => x.info.id === hostId)
      if (!h) return
      try {
        // An event that came while the list was on its way may be missing from it (opened after the host read its shells): ask again.
        let seen = shellEvents[hostId] ?? 0
        let res = await listShells(h.client)
        for (let tries = 0; tries < 3 && (shellEvents[hostId] ?? 0) !== seen; tries++) {
          seen = shellEvents[hostId] ?? 0
          res = await listShells(h.client)
        }
        set((s) => {
          const shells = { ...s.shells, [hostId]: res.shells }
          const { [hostId]: _was, ...shellsError } = s.shellsError
          // A restored shell tab whose shell the host no longer has goes, as an agent's does.
          const patch = pruneHost({ ...s, shells }, hostId, h.store.getState().agents)
          return { shells, shellsError, ...patch }
        })
      } catch (e) {
        // Not listed now (a host without the call, a failed request): the tabs stay as they were, and say why.
        set((s) => ({ shellsError: { ...s.shellsError, [hostId]: e instanceof ConnectError ? e.rawMessage : String(e) } }))
      }
    },
    newShell: async (hostId, repo) => {
      const h = store.getState().hosts.find((x) => x.info.id === hostId)
      if (!h) return
      const res = await openShell(h.client, repo)
      const shell = res.shell
      if (!shell) return
      set((s) => ({ shells: { ...s.shells, [hostId]: [...(s.shells[hostId] ?? []).filter((x) => x.id !== shell.id), shell] } }))
      model.selectAgent(hostId, shellTabId(shell.id))
    },
    endShell: async (hostId, shellId) => {
      const h = store.getState().hosts.find((x) => x.info.id === hostId)
      if (!h) return
      try {
        await callCloseShell(h.client, shellId)
      } catch (e) {
        // A shell that ended on its own is already forgotten by the host: ending it is done.
        if (!(e instanceof ConnectError && e.code === Code.NotFound)) throw e
      }
      set((s) => ({ shells: { ...s.shells, [hostId]: (s.shells[hostId] ?? []).filter((x) => x.id !== shellId) } }))
      model.closeTab({ hostId, agentId: shellTabId(shellId) }, true)
    },
    closeShellTab: (ref, keep) => {
      set({ closingShell: undefined })
      if (keep) model.closeTab(ref, true)
    },
    closeTab: (ref, force) => {
      // A shell tab asks first: end the shell, or keep it running (the + menu opens it again).
      if (!force && isShellTab(ref.agentId) && store.getState().tabs.some((t) => tabKey(t) === tabKey(ref))) {
        set({ closingShell: ref })
        return
      }
      set((s) => ({ ...closeTab(s, ref, projectOfTab(s)), closedTabs: s.tabs.some((t) => tabKey(t) === tabKey(ref)) ? [...s.closedTabs.filter((t) => tabKey(t) !== tabKey(ref)), ref].slice(-20) : s.closedTabs }))
    },
    moveTab: (ref, to) =>
      set((s) => {
        const from = s.tabs.findIndex((t) => tabKey(t) === tabKey(ref))
        const at = s.tabs.findIndex((t) => tabKey(t) === tabKey(to))
        if (from < 0 || at < 0 || from === at) return {}
        const tabs = [...s.tabs]
        const [moved] = tabs.splice(from, 1)
        tabs.splice(at, 0, moved!)
        return { tabs, dragTab: undefined }
      }),
    cycleTab: (by) => {
      const next = cycleTab(store.getState(), by, shownTabs(store.getState()))
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
    openViewRequest: (id) => {
      const r = store.getState().viewRequests.find((x) => x.id === id)
      set((s) => ({ viewRequests: s.viewRequests.filter((x) => x.id !== id) }))
      if (r) showView(model, r.hostId, r.request)
    },
    clearFocusMessage: () => {
      clearTimeout(focusFallback)
      set({ focusMessage: undefined })
    },
    focusOnMessage: (focus) => {
      clearTimeout(focusFallback)
      set({ focusMessage: focus })
      focusFallback = setTimeout(() => {
        if (store.getState().focusMessage === focus) set({ focusMessage: undefined })
      }, 10_000)
    },
    claimFocusMessage: () => clearTimeout(focusFallback),
    dismissViewRequest: (id) => set((s) => ({ viewRequests: s.viewRequests.filter((x) => x.id !== id) })),
    togglePin: (hostId, path, on) =>
      set((s) => {
        const cur = s.pins[hostId] ?? []
        const next = (on ?? !cur.includes(path)) ? (cur.includes(path) ? cur : [...cur, path]) : cur.filter((p) => p !== path)
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

/** A tab's project (`sidebar.ts` `agentProject`), from the hosts' agents now. */
function projectOfTab(s: AppState): (t: TabRef) => string | undefined {
  const agentsOf = (hostId: string) => s.hosts.find((h) => h.info.id === hostId)?.store.getState().agents
  return (t) => {
    if (!isShellTab(t.agentId)) return agentProject(t.hostId, agentsOf(t.hostId), t.agentId)
    const shell = s.shells[t.hostId]?.find((x) => x.id === shellIdOf(t.agentId))
    return shell ? shellProject(t.hostId, shell) : undefined
  }
}

/** The tabs the tab bar shows now: the open agent's project's (`workspace.ts` `projectTabs`). */
export function shownTabs(s: AppState): TabRef[] {
  return projectTabs(s.tabs, projectOfTab(s), s.hostId && s.agentId ? { hostId: s.hostId, agentId: s.agentId } : undefined)
}
