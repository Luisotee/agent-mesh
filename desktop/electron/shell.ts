import { appendFileSync } from 'node:fs'
import { BrowserWindow, Menu, Notification, Tray, app, nativeImage } from 'electron'
import type { NoticeClick, NoticeWire } from '../shared/api.js'
import { ICON, ICON_WAITING } from './icons.js'
import { DEFAULT_CLOSE_TO_TRAY, shouldHideOnClose, trayTooltip, updateNotice } from './shell-rules.js'

const image = (b64: string) => nativeImage.createFromDataURL(`data:image/png;base64,${b64}`)

/**
 * What only the main process can do: notifications, the tray, and what closing the window means. It knows nothing
 * of agents or hosts; the renderer decides what to say and when. With a `logFile` (tests) it records what it was
 * asked to do, and shows no notification.
 */
export class Shell {
  private tray?: Tray
  private waiting = 0
  private closeToTray = DEFAULT_CLOSE_TO_TRAY
  private quitting = false
  private readonly shown = new Map<string, Notification>()

  constructor(
    private readonly window: () => BrowserWindow | undefined,
    private readonly onClick: (c: NoticeClick) => void,
    private readonly logFile?: string,
  ) {
    app.on('before-quit', () => (this.quitting = true))
  }

  private log(entry: Record<string, unknown>): void {
    if (this.logFile) appendFileSync(this.logFile, `${JSON.stringify(entry)}\n`)
  }

  start(): void {
    try {
      this.tray = new Tray(image(ICON))
      this.tray.setToolTip(trayTooltip(0))
      this.tray.on('click', () => this.show())
      this.refreshMenu()
    } catch {
      // A desktop without a tray: the app works without it (and closing the window really closes it).
      this.tray = undefined
    }
  }

  show(): void {
    const w = this.window()
    if (!w) return
    if (w.isMinimized()) w.restore()
    w.show()
    w.focus()
  }

  /** The window's close handler: true when the close was turned into a hide. */
  closing(): boolean {
    return shouldHideOnClose({ closeToTray: this.closeToTray, trayReady: !!this.tray && !this.tray.isDestroyed(), quitting: this.quitting })
  }

  configure(c: { closeToTray: boolean }): void {
    this.closeToTray = c.closeToTray
    this.log({ type: 'configure', closeToTray: c.closeToTray })
  }

  setWaiting(n: number): void {
    this.waiting = n
    this.log({ type: 'waiting', count: n })
    app.setBadgeCount(n)
    if (this.tray && !this.tray.isDestroyed()) {
      this.tray.setImage(image(n > 0 ? ICON_WAITING : ICON))
      this.tray.setToolTip(trayTooltip(n))
      this.refreshMenu()
    }
  }

  notify(n: NoticeWire): void {
    this.log({ type: 'notify', tag: n.tag, title: n.title, body: n.body, sound: n.sound, hostId: n.hostId, agentId: n.agentId, tab: n.tab })
    if (this.logFile || !Notification.isSupported()) return
    // A newer notification about the same agent replaces the older one.
    this.shown.get(n.tag)?.close()
    const note = new Notification({ title: n.title, body: n.body, silent: !n.sound })
    note.on('click', () => {
      this.show()
      this.onClick({ hostId: n.hostId, agentId: n.agentId, tab: n.tab })
    })
    note.on('close', () => {
      if (this.shown.get(n.tag) === note) this.shown.delete(n.tag)
    })
    this.shown.set(n.tag, note)
    note.show()
  }

  /** A downloaded update is ready (it installs when the app quits): a click brings the window back, from the tray too. */
  notifyUpdate(version: string): void {
    const n = updateNotice(version)
    this.log({ type: 'update', version })
    if (this.logFile || !Notification.isSupported()) return
    this.shown.get('update')?.close()
    const note = new Notification(n)
    note.on('click', () => this.show())
    note.on('close', () => {
      if (this.shown.get('update') === note) this.shown.delete('update')
    })
    this.shown.set('update', note)
    note.show()
  }

  private refreshMenu(): void {
    this.tray?.setContextMenu(
      Menu.buildFromTemplate([
        { label: 'Show agent-mesh', click: () => this.show() },
        { label: this.waiting > 0 ? `${this.waiting} waiting on you` : 'Nothing waiting on you', enabled: false },
        { type: 'separator' },
        { label: 'Quit', click: () => app.quit() },
      ]),
    )
  }
}
