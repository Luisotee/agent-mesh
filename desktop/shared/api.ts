// The contract between Electron's main process and the renderer: types only, so both sides compile against it.
// The renderer has no other way out of its sandbox: it reaches a host only through `request`, and the bridge
// forwards only the registry's paths (electron/bridge.ts).

/** A host the app can connect to, as the renderer sees it. A token never leaves the main process. */
export interface HostInfo {
  id: string
  name: string
  kind: 'state-dir' | 'tcp' | 'paired'
  /** The state dir, a host:port, or where a paired host is reached. */
  target: string
  /** Main's word that this is the daemon `api.local` scans for (this machine's own), not just one at a loopback address. */
  here?: boolean
}

export type NewHost =
  | { name: string; kind: 'state-dir'; path: string }
  /** A host by its pairing link (`agentmesh://pair?...`): main redeems it, and keeps the device token it gets. */
  | { name: string; kind: 'pair'; link: string; deviceName: string }

/** What a scan for a daemon on this machine found. */
export type LocalState = 'ready' | 'not-installed' | 'not-running' | 'no-listen' | 'unreachable'

/** A fix the app can run for the user, by name: main runs fixed commands for it, never one the renderer wrote. */
export type LocalFix = 'start' | 'enable-listen'

export interface LocalCandidate {
  /** `native`, `wsl:<distro>` or `wsl` (no WSL at all). */
  id: string
  label: string
  state: LocalState
  /** What was found, in a sentence the user can act on. */
  detail: string
  fixes: LocalFix[]
  /** What the user has to run themselves, when the app can't (installing). */
  commands?: string[]
}

export interface BridgeRequest {
  method: string
  /** A registry path, such as /agentmesh.v1.MeshService/Hello. */
  path: string
  headers: Record<string, string>
  body?: Uint8Array
}

/** Why a request never got an HTTP answer. */
export type BridgeErrorCode = 'unreachable' | 'no-token' | 'unknown-host' | 'refused' | 'failed'

export type BridgeEvent =
  | { type: 'head'; status: number; headers: Record<string, string> }
  | { type: 'data'; chunk: Uint8Array }
  | { type: 'end' }
  | { type: 'error'; code: BridgeErrorCode; message: string }

/** What a terminal stream's socket reports; frames are binary (agentmesh.term.v1.TermFrame). */
export type SocketEvent =
  | { type: 'open' }
  | { type: 'message'; data: Uint8Array }
  | { type: 'close'; code: number; reason: string }
  | { type: 'error'; code: BridgeErrorCode; message: string }

export interface SocketHandle {
  send(data: Uint8Array): void
  close(): void
}

export type AgentTabId = 'overview' | 'messages' | 'terminal'

/** A desktop notification the renderer asks main to show. A later one with the same tag replaces the earlier. */
export interface NoticeWire {
  tag: string
  title: string
  body: string
  /** Play the system sound with it. */
  sound: boolean
  /** Where a click goes. */
  hostId: string
  agentId: string
  tab: AgentTabId
}

export interface NoticeClick {
  hostId: string
  agentId: string
  tab: AgentTabId
}

/** What only the main process can do for the app: notifications, the tray, what closing the window means. */
export interface ShellApi {
  notify(n: NoticeWire): void
  /** How many things wait on the user, for the tray's icon and tooltip. */
  setWaiting(count: number): void
  configure(c: { closeToTray: boolean }): void
  /** A click on a notification; returns how to stop listening. */
  onNotificationClick(cb: (n: NoticeClick) => void): () => void
}

/** A pairing link the operating system opened the app with (`agentmesh://pair?...`), already read and checked by main. */
export interface OpenedLink {
  link: string
  /** The host's own name, as the link says it. */
  hostName: string
}

export interface DesktopApi {
  hosts: {
    list(): Promise<HostInfo[]>
    add(host: NewHost): Promise<HostInfo>
    remove(id: string): Promise<void>
    /** True when the host list is fixed by the environment (dev and tests): add and remove are refused. */
    readonly fixed: boolean
  }
  /** Send one request to a host; events arrive in order, ending with `end` or `error`. Returns an abort. */
  request(hostId: string, req: BridgeRequest, onEvent: (e: BridgeEvent) => void): () => void
  /** Open a terminal's WebSocket (a path from OpenTerminal, ticket included). Events end with `close` or `error`. */
  openSocket(hostId: string, path: string, onEvent: (e: SocketEvent) => void): SocketHandle
  /** Pairing links opened from outside the app. They never act on their own: the renderer shows them for the user to confirm. */
  links: {
    /** The link that arrived before anyone asked (one that started the app), once. */
    take(): Promise<OpenedLink | undefined>
    /** A link that arrives while the app runs; returns how to stop listening. */
    onOpen(cb: (l: OpenedLink) => void): () => void
  }
  /** Finding a daemon on this machine (the first-run screen, and "Check this machine"). Never takes a command. */
  local: {
    detect(): Promise<LocalCandidate[]>
    /** Connects to a candidate that was `ready` and saves it as a host. */
    connect(id: string): Promise<HostInfo>
    /** Runs one fix on a candidate and returns the scan after it. */
    fix(id: string, fix: LocalFix): Promise<LocalCandidate[]>
  }
  /** Open an http(s) link in the user's browser. */
  openExternal(url: string): void
  shell: ShellApi
}

declare global {
  interface Window {
    desktop: DesktopApi
  }
}
