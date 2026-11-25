import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { BrowserWindow, app, ipcMain, session, shell } from 'electron'
import type { BridgeEvent, BridgeRequest, NewHost, SocketEvent } from '../shared/api.js'
import { forward } from './bridge.js'
import { Hosts, toInfo } from './hosts.js'
import { openSocket, type Socket } from './socket.js'

const here = path.dirname(fileURLToPath(import.meta.url))

// Dev and tests give the app a state dir of its own and one fixed host; nothing else is read from the user's
// real directories then.
const userData = process.env.AGENT_MESH_DESKTOP_USERDATA
if (userData) app.setPath('userData', userData)
const hosts = new Hosts(path.join(app.getPath('userData'), 'hosts.json'), process.env.AGENT_MESH_DESKTOP_STATE)

const aborts = new Map<string, () => void>()
const sockets = new Map<string, Socket>()

/** Ends every host request a renderer has open (it reloaded or closed): its ids start over, and nobody reads the chunks. */
function abortAll(senderId: number): void {
  for (const [key, abort] of aborts) {
    if (key.startsWith(`${senderId}:`)) {
      abort()
      aborts.delete(key)
    }
  }
  for (const [key, s] of sockets) {
    if (key.startsWith(`${senderId}:`)) {
      s.close()
      sockets.delete(key)
    }
  }
}

function wire(): void {
  ipcMain.on('hosts:fixed', (e) => {
    e.returnValue = hosts.fixed
  })
  ipcMain.handle('hosts:list', () => hosts.all().map(toInfo))
  ipcMain.handle('hosts:add', async (_e, n: NewHost) => toInfo(await hosts.add(n)))
  ipcMain.handle('hosts:remove', (_e, id: string) => hosts.remove(id))
  ipcMain.on('bridge:request', (e, id: unknown, hostId: unknown, req: BridgeRequest) => {
    if (typeof id !== 'string' || typeof hostId !== 'string') return
    const key = `${e.sender.id}:${id}`
    let finished = false
    const emit = (ev: BridgeEvent) => {
      if (!e.sender.isDestroyed()) e.sender.send('bridge:event', id, ev)
      if (ev.type === 'end' || ev.type === 'error') {
        finished = true
        aborts.delete(key)
      }
    }
    const abort = forward(hosts.get(hostId), req, emit)
    // A request refused on the spot has finished before it returns: there is nothing to keep.
    if (!finished) aborts.set(key, abort)
  })
  ipcMain.on('socket:open', (e, id: unknown, hostId: unknown, p: unknown) => {
    if (typeof id !== 'string' || typeof hostId !== 'string') return
    const key = `${e.sender.id}:${id}`
    let finished = false
    const emit = (ev: SocketEvent) => {
      if (!e.sender.isDestroyed()) e.sender.send('socket:event', id, ev)
      if (ev.type === 'close' || ev.type === 'error') {
        finished = true
        sockets.delete(key)
      }
    }
    const sock = openSocket(hosts.get(hostId), p, emit)
    // A refusal on the spot has finished before it returns: nothing to keep.
    if (!finished) sockets.set(key, sock)
  })
  ipcMain.on('socket:send', (e, id: unknown, data: unknown) => {
    if (typeof id === 'string' && data instanceof Uint8Array) sockets.get(`${e.sender.id}:${id}`)?.send(data)
  })
  ipcMain.on('socket:close', (e, id: unknown) => {
    if (typeof id !== 'string') return
    const key = `${e.sender.id}:${id}`
    sockets.get(key)?.close()
    sockets.delete(key)
  })
  ipcMain.on('shell:openExternal', (_e, url: unknown) => {
    // Links in a terminal open in the user's browser; only web links.
    if (typeof url === 'string' && /^https?:\/\//i.test(url) && url.length < 2048) void shell.openExternal(url)
  })
  ipcMain.on('bridge:abort', (e, id: unknown) => {
    if (typeof id !== 'string') return
    const key = `${e.sender.id}:${id}`
    aborts.get(key)?.()
    aborts.delete(key)
  })
}

function createWindow(): BrowserWindow {
  const win = new BrowserWindow({
    width: 1280,
    height: 800,
    minWidth: 720,
    minHeight: 480,
    title: 'agent-mesh',
    backgroundColor: '#282a36',
    webPreferences: {
      preload: path.join(here, 'preload.cjs'),
      contextIsolation: true,
      sandbox: true,
      nodeIntegration: false,
    },
  })
  win.setMenuBarVisibility(false)
  // The app is one page: no navigation, no new windows.
  win.webContents.on('will-navigate', (e) => e.preventDefault())
  win.webContents.setWindowOpenHandler(() => ({ action: 'deny' }))
  const wc = win.webContents
  const id = wc.id
  wc.on('did-start-navigation', (_e, _url, _inPlace, isMainFrame) => {
    if (isMainFrame) abortAll(id)
  })
  wc.once('destroyed', () => abortAll(id))
  void win.loadFile(path.join(here, '../../dist/index.html'))
  return win
}

if (!app.requestSingleInstanceLock()) {
  app.quit()
} else {
  void app.whenReady().then(async () => {
    session.defaultSession.setPermissionRequestHandler((_wc, _perm, cb) => cb(false))
    await hosts.load()
    wire()
    createWindow()
  })
  app.on('window-all-closed', () => app.quit())
}
