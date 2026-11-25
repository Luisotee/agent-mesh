import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { BrowserWindow, app, ipcMain, session } from 'electron'
import type { BridgeEvent, BridgeRequest, NewHost } from '../shared/api.js'
import { forward } from './bridge.js'
import { Hosts, toInfo } from './hosts.js'

const here = path.dirname(fileURLToPath(import.meta.url))

// Dev and tests give the app a state dir of its own and one fixed host; nothing else is read from the user's
// real directories then.
const userData = process.env.AGENT_MESH_DESKTOP_USERDATA
if (userData) app.setPath('userData', userData)
const hosts = new Hosts(path.join(app.getPath('userData'), 'hosts.json'), process.env.AGENT_MESH_DESKTOP_STATE)

const aborts = new Map<string, () => void>()

function wire(): void {
  ipcMain.on('hosts:fixed', (e) => {
    e.returnValue = hosts.fixed
  })
  ipcMain.handle('hosts:list', () => hosts.all().map(toInfo))
  ipcMain.handle('hosts:add', async (_e, n: NewHost) => toInfo(await hosts.add(n)))
  ipcMain.handle('hosts:remove', (_e, id: string) => hosts.remove(id))
  ipcMain.on('bridge:request', (e, id: string, hostId: string, req: BridgeRequest) => {
    const key = `${e.sender.id}:${id}`
    const emit = (ev: BridgeEvent) => {
      if (!e.sender.isDestroyed()) e.sender.send('bridge:event', id, ev)
      if (ev.type === 'end' || ev.type === 'error') aborts.delete(key)
    }
    aborts.set(key, forward(hosts.get(hostId), req, emit))
  })
  ipcMain.on('bridge:abort', (e, id: string) => {
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
