// The contract between Electron's main process and the renderer: types only, so both sides compile against it.
// The renderer has no other way out of its sandbox: it reaches a host only through `request`, and the bridge
// forwards only the registry's paths (electron/bridge.ts).

/** A host the app can connect to, as the renderer sees it. A token never leaves the main process. */
export interface HostInfo {
  id: string
  name: string
  kind: 'state-dir' | 'tcp'
  /** The state dir, or host:port. */
  target: string
}

export type NewHost =
  | { name: string; kind: 'state-dir'; path: string }
  | { name: string; kind: 'tcp'; address: string; token: string }

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
}

declare global {
  interface Window {
    desktop: DesktopApi
  }
}
