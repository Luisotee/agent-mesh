import { createStore, type StoreApi } from 'zustand/vanilla'
import type { DesktopApi, HostInfo, NewHost, SocketEvent, SocketHandle } from '../shared/api'
import { joinWaiting } from './derive'
import { createMeshClient, type MeshClient } from './actions'
import { HostConnection } from './connection'
import { messageLoader } from './messages'
import { hostRefresher } from './refresh'
import { noticeFor } from './notify'
import { appPrefs } from './prefs'
import { listSettings } from './actions'
import { applyFrame, createHostStore, setSettings, storeSink, type HostStore } from './store'
import type { Frame } from './connection'
import { hostTransport } from './transport'

/** A host the app is connected to (or trying to be): its state, its client, its connection. */
export interface HostRuntime {
  info: HostInfo
  store: HostStore
  client: MeshClient
  connection: HostConnection
  /** Stops the host's timers and calls (its connection is stopped separately). */
  stop(): void
  /** A terminal's stream, by the path `OpenTerminal` gave. */
  openSocket(path: string, onEvent: (e: SocketEvent) => void): SocketHandle
}

export function connectHost(api: Pick<DesktopApi, 'request' | 'openSocket' | 'shell'>, info: HostInfo): HostRuntime {
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
      if (f.case === 'event') refresher.event(f.value)
      else if (f.case === 'snapshot' || f.case === 'resumed') refresher.synced()
      if (f.case === 'event') {
        // The host's own preferences say whether and what to notify about its events.
        const prefs = appPrefs(before.settings)
        const n = prefs.notifyEnabled ? noticeFor(f.value, before.agents, Date.now(), before.clockSkewMs) : undefined
        if (n && prefs.notifyEvents.has(n.kind)) {
          api.shell.notify({ tag: `${info.id}:${n.tag}`, title: n.title, body: n.body, sound: prefs.notifySound, hostId: info.id, agentId: n.agentId, tab: n.tab })
        }
      }
      if (f.case === 'snapshot') {
        void loader.load(f.value.runs)
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
  return { info, store, client, connection, stop: () => refresher.stop(), openSocket: (path, onEvent) => api.openSocket(info.id, path, onEvent) }
}

export interface AppState {
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
}

export type AgentTab = 'overview' | 'messages' | 'terminal'

export interface AppModel {
  store: StoreApi<AppState>
  api: DesktopApi
  load(): Promise<void>
  addHost(h: NewHost): Promise<void>
  removeHost(id: string): Promise<void>
  selectHost(id: string): void
  selectAgent(hostId: string, agentId: string | undefined, tab?: AgentTab): void
  setTab(tab: AgentTab): void
  openSettings(open: boolean): void
  openPairing(open: boolean): void
}

/** The app's own state: which hosts, and what is selected. Everything else lives on a host. */
export function createAppModel(api: DesktopApi): AppModel {
  const store = createStore<AppState>(() => ({ loaded: false, hosts: [], tab: 'overview', settingsOpen: false, pairOpen: false }))
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
  const unwatch = new Map<string, () => void>()
  const watch = (rt: HostRuntime) =>
    unwatch.set(
      rt.info.id,
      rt.store.subscribe((s, prev) => {
        if (s.waiting !== prev.waiting || s.agents !== prev.agents || s.runs !== prev.runs) recount()
        if (s.settings !== prev.settings && store.getState().hosts[0] === rt) configure()
      }),
    )
  api.shell.onNotificationClick((c) => {
    // A click on a notification of a host that has since been removed goes nowhere.
    if (store.getState().hosts.some((h) => h.info.id === c.hostId)) store.setState({ hostId: c.hostId, agentId: c.agentId, tab: c.tab, settingsOpen: false, pairOpen: false })
  })
  let loading: Promise<void> | undefined
  async function loadHosts() {
    try {
      const infos = await api.hosts.list()
      const hosts = infos.map((i) => connectHost(api, i))
      hosts.forEach(watch)
      set({ loaded: true, hosts, hostId: hosts[0]?.info.id, error: undefined })
      recount()
      configure()
    } catch (e) {
      set({ loaded: true, error: String(e) })
    }
  }
  return {
    store,
    api,
    // Once: a second call (React's strict mode runs effects twice) must not open every connection again.
    load: () => (loading ??= loadHosts()),
    async addHost(h) {
      const info = await api.hosts.add(h)
      const rt = connectHost(api, info)
      watch(rt)
      set((s) => ({ hosts: [...s.hosts, rt], hostId: info.id, agentId: undefined }))
      recount()
      configure()
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
        return { hosts, hostId: s.hostId === id ? hosts[0]?.info.id : s.hostId, agentId: s.hostId === id ? undefined : s.agentId }
      })
      recount()
      configure()
    },
    selectHost: (id) => set({ hostId: id, agentId: undefined, settingsOpen: false, pairOpen: false }),
    selectAgent: (hostId, agentId, tab = 'overview') => set({ hostId, agentId, tab, settingsOpen: false, pairOpen: false }),
    openSettings: (open) => set({ settingsOpen: open, pairOpen: false }),
    openPairing: (open) => set({ pairOpen: open, settingsOpen: false }),
    setTab: (tab) => set({ tab }),
  }
}
