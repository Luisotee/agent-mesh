import { createStore, type StoreApi } from 'zustand/vanilla'
import type { DesktopApi, HostInfo, NewHost, SocketEvent, SocketHandle } from '../shared/api'
import { createMeshClient, type MeshClient } from './actions'
import { HostConnection } from './connection'
import { messageLoader } from './messages'
import { createHostStore, storeSink, type HostStore } from './store'
import { hostTransport } from './transport'

/** A host the app is connected to (or trying to be): its state, its client, its connection. */
export interface HostRuntime {
  info: HostInfo
  store: HostStore
  client: MeshClient
  connection: HostConnection
  /** A terminal's stream, by the path `OpenTerminal` gave. */
  openSocket(path: string, onEvent: (e: SocketEvent) => void): SocketHandle
}

export function connectHost(api: Pick<DesktopApi, 'request' | 'openSocket'>, info: HostInfo): HostRuntime {
  const store = createHostStore()
  const client = createMeshClient(hostTransport(api, info.id))
  const sink = storeSink(store)
  const loader = messageLoader(client, store, sink.log)
  const connection = new HostConnection({
    client,
    ...sink,
    onFrame: (f) => {
      sink.onFrame(f)
      if (f.case === 'snapshot') void loader.load(f.value.runs)
    },
  })
  connection.start()
  return { info, store, client, connection, openSocket: (path, onEvent) => api.openSocket(info.id, path, onEvent) }
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
}

/** The app's own state: which hosts, and what is selected. Everything else lives on a host. */
export function createAppModel(api: DesktopApi): AppModel {
  const store = createStore<AppState>(() => ({ loaded: false, hosts: [], tab: 'overview' }))
  const set = store.setState
  let loading: Promise<void> | undefined
  async function loadHosts() {
    try {
      const infos = await api.hosts.list()
      const hosts = infos.map((i) => connectHost(api, i))
      set({ loaded: true, hosts, hostId: hosts[0]?.info.id, error: undefined })
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
      set((s) => ({ hosts: [...s.hosts, rt], hostId: info.id, agentId: undefined }))
    },
    async removeHost(id) {
      await api.hosts.remove(id)
      store.getState().hosts.find((h) => h.info.id === id)?.connection.stop()
      set((s) => {
        const hosts = s.hosts.filter((h) => h.info.id !== id)
        return { hosts, hostId: s.hostId === id ? hosts[0]?.info.id : s.hostId, agentId: s.hostId === id ? undefined : s.agentId }
      })
    },
    selectHost: (id) => set({ hostId: id, agentId: undefined }),
    selectAgent: (hostId, agentId, tab = 'overview') => set({ hostId, agentId, tab }),
    setTab: (tab) => set({ tab }),
  }
}
