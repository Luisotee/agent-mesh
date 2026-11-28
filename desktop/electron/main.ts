import { existsSync, readFileSync, writeFileSync } from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { allowsPermission } from './permissions.js'
import { BrowserWindow, Menu, app, clipboard, ipcMain, session, shell, type MenuItemConstructorOptions } from 'electron'
import type { BridgeEvent, BridgeRequest, LocalFix, NewHost, SocketEvent } from '../shared/api.js'
import { forward } from './bridge.js'
import { Hosts } from './hosts.js'
import { LinkInbox, PROTOCOL, linkIn, openedLink } from './links.js'
import { Local } from './local.js'
import { redeemPairing } from './pair.js'
import { Shell } from './shell.js'
import { validNotice } from './shell-rules.js'
import { openSocket, type Socket } from './socket.js'
import { NSIS_UNINSTALLER, startUpdates } from './updates.js'
import { menuFor, nextZoom, zoomFrom, zoomOf } from './window-input.js'

const here = path.dirname(fileURLToPath(import.meta.url))

// Dev and tests give the app a state dir of its own and one fixed host; nothing else is read from the user's
// real directories then.
const userData = process.env.AGENT_MESH_DESKTOP_USERDATA
if (userData) app.setPath('userData', userData)
const hosts = new Hosts(path.join(app.getPath('userData'), 'hosts.json'), process.env.AGENT_MESH_DESKTOP_STATE, redeemPairing)
const local = new Local(hosts)

let mainWindow: BrowserWindow | undefined
const appShell = new Shell(
  () => mainWindow,
  (click) => mainWindow?.webContents.send('shell:click', click),
  process.env.AGENT_MESH_DESKTOP_SHELL_LOG,
)

// Pairing links from outside the app (agentmesh://pair?...) wait here for the window; none is acted on without the user.
const links = new LinkInbox(() => {
  if (mainWindow && !mainWindow.isDestroyed()) mainWindow.webContents.send('links:open')
})
function offerLink(link: ReturnType<typeof openedLink>): void {
  if (!link) return
  links.offer(link)
  appShell.show()
}

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
  ipcMain.handle('hosts:list', () => hosts.all().map((h) => local.info(h)))
  ipcMain.handle('hosts:add', async (_e, n: NewHost) => local.info(await hosts.add(n)))
  ipcMain.handle('hosts:remove', (_e, id: string) => hosts.remove(id))
  ipcMain.handle('local:detect', () => (hosts.fixed ? [] : local.detect()))
  ipcMain.handle('local:connect', async (_e, id: unknown) => {
    if (typeof id !== 'string') throw new Error('no daemon given')
    return local.connect(id)
  })
  ipcMain.handle('local:fix', (_e, id: unknown, fix: unknown) => {
    if (typeof id !== 'string' || (fix !== 'start' && fix !== 'enable-listen')) throw new Error('no such fix')
    return local.fix(id, fix as LocalFix)
  })
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
  ipcMain.handle('links:take', () => links.take())
  ipcMain.on('find:start', (e, text: unknown, forward: unknown, again: unknown) => {
    if (typeof text === 'string' && text) e.sender.findInPage(text.slice(0, 500), { forward: forward !== false, findNext: again !== true })
  })
  ipcMain.on('find:stop', (e) => e.sender.stopFindInPage('keepSelection'))
  ipcMain.on('shell:notify', (_e, n: unknown) => {
    const valid = validNotice(n)
    if (valid) appShell.notify(valid)
  })
  ipcMain.on('shell:waiting', (_e, n: unknown) => {
    if (typeof n === 'number' && Number.isInteger(n) && n >= 0 && n < 100_000) appShell.setWaiting(n)
  })
  ipcMain.on('shell:configure', (_e, c: unknown) => {
    if (typeof c === 'object' && c !== null && typeof (c as { closeToTray?: unknown }).closeToTray === 'boolean') appShell.configure({ closeToTray: (c as { closeToTray: boolean }).closeToTray })
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
  wireInput(win)
  // The app is one page: no navigation, no new windows.
  win.webContents.on('will-navigate', (e) => e.preventDefault())
  win.webContents.setWindowOpenHandler(() => ({ action: 'deny' }))
  const wc = win.webContents
  const id = wc.id
  wc.on('did-start-navigation', (_e, _url, _inPlace, isMainFrame) => {
    if (isMainFrame) abortAll(id)
  })
  wc.once('destroyed', () => abortAll(id))
  win.on('close', (e) => {
    if (appShell.closing()) {
      e.preventDefault()
      win.hide()
    }
  })
  void win.loadFile(path.join(here, '../../dist/index.html'))
  win.on('closed', () => {
    if (mainWindow === win) mainWindow = undefined
  })
  mainWindow = win
  return win
}

/** The window's zoom (kept in the user-data folder: it is this device's), the text menu and find in page. */
function wireInput(win: BrowserWindow): void {
  const wc = win.webContents
  const file = path.join(app.getPath('userData'), 'zoom.json')
  let level = 0
  try {
    level = zoomFrom(readFileSync(file, 'utf8'))
  } catch {
    // No saved zoom: the default.
  }
  wc.on('did-finish-load', () => wc.setZoomLevel(level))
  // The renderer says when a terminal has the focus: its Ctrl keys are the shell's then.
  let inTerminal = false
  wc.ipc.on('input:terminal', (_e, on: unknown) => (inTerminal = on === true))
  wc.on('did-start-navigation', () => (inTerminal = false))
  wc.on('before-input-event', (e, input) => {
    const z = zoomOf(input, process.platform === 'darwin', inTerminal)
    if (!z) return
    e.preventDefault()
    level = nextZoom(level, z)
    wc.setZoomLevel(level)
    try {
      writeFileSync(file, JSON.stringify({ level }))
    } catch {
      // Not kept: the zoom still applies now.
    }
  })
  wc.on('context-menu', (_e, p) => {
    const items = menuFor(p)
    if (items.length === 0) return
    const template = items.map((i): MenuItemConstructorOptions => {
      switch (i.kind) {
        case 'role':
          return { role: i.role, label: i.label, enabled: i.enabled }
        case 'replace':
          return { label: i.word, click: () => wc.replaceMisspelling(i.word) }
        case 'learn':
          return { label: 'Add to dictionary', click: () => wc.session.addWordToSpellCheckerDictionary(i.word) }
        case 'copyLink':
          return { label: 'Copy link', click: () => clipboard.writeText(i.url) }
        case 'separator':
          return { type: 'separator' }
      }
    })
    Menu.buildFromTemplate(template).popup({ window: win })
  })
  // Find in page: the renderer's find bar sends the text; main finds and tells it how many matched.
  // Only the final result of a search: the ones before it (a large page) would flash "No match" or step twice.
  wc.on('found-in-page', (_e, r) => {
    if (r.finalUpdate) wc.send('find:result', { active: r.activeMatchOrdinal, total: r.matches })
  })
}

if (!app.requestSingleInstanceLock()) {
  app.quit()
} else {
  void app.whenReady().then(async () => {
    session.defaultSession.setPermissionRequestHandler((_wc, perm, cb) => cb(allowsPermission(perm)))
    // The OS opens this app on a pairing link only once the app is installed that way: a dev or test run (its own
    // user-data dir, or not packaged) must never take the scheme from the user's real install.
    if (app.isPackaged && !userData) app.setAsDefaultProtocolClient(PROTOCOL)
    await hosts.load()
    wire()
    createWindow()
    appShell.start()
    // Launched by a link (Windows and Linux hand it over as an argument).
    offerLink(linkIn(process.argv))
    // An installed app looks for a newer release on GitHub (electron-builder.json, "publish"): the installer downloads it
    // and applies it when the app quits. electron-updater is loaded only here, so dev and test runs never read it.
    startUpdates({
      packaged: app.isPackaged,
      isolated: !!userData,
      platform: process.platform,
      env: process.env,
      nsis: existsSync(path.join(path.dirname(process.execPath), NSIS_UNINSTALLER)),
      load: async () => (await import('electron-updater')).default.autoUpdater,
      log: (m) => console.warn(m),
      notify: (version) => appShell.notifyUpdate(version),
    })
  })
  // A second launch (another link clicked) reaches this running app; macOS opens links through this event instead.
  app.on('second-instance', (_e, argv) => {
    appShell.show()
    offerLink(linkIn(argv))
  })
  app.on('open-url', (e, url) => {
    e.preventDefault()
    offerLink(openedLink(url))
  })
  app.on('window-all-closed', () => app.quit())
}
