import type { NoticeWire } from '../shared/api.js'

const TABS = new Set(['chat', 'terminal'])

/** The renderer is untrusted: a notification is shown only if it has the right shape, within sane lengths. */
export function validNotice(x: unknown): NoticeWire | undefined {
  if (typeof x !== 'object' || x === null) return undefined
  const n = x as Record<string, unknown>
  const text = (v: unknown, max: number): v is string => typeof v === 'string' && v.length <= max
  if (!text(n.tag, 200) || !text(n.title, 200) || !text(n.body, 600) || !text(n.hostId, 200) || !text(n.agentId, 200)) return undefined
  if (typeof n.sound !== 'boolean' || typeof n.tab !== 'string' || !TABS.has(n.tab)) return undefined
  if (!n.tag || !n.title) return undefined
  return { tag: n.tag, title: n.title, body: n.body, sound: n.sound, hostId: n.hostId, agentId: n.agentId, tab: n.tab as NoticeWire['tab'] }
}

export function trayTooltip(waiting: number): string {
  return waiting > 0 ? `agent-mesh: ${waiting} waiting on you` : 'agent-mesh'
}

/** What closing the window does before the renderer has told main the host's setting: the setting's default (src/prefs.ts, tested equal). */
export const DEFAULT_CLOSE_TO_TRAY = false

/**
 * Closing the window hides it when the user asked for that and there is a tray icon to bring it back from; with
 * no tray (a desktop that has none), hiding would leave the app unreachable, so the window really closes.
 */
export function shouldHideOnClose(s: { closeToTray: boolean; trayReady: boolean; quitting: boolean }): boolean {
  return s.closeToTray && s.trayReady && !s.quitting
}

/** The notification for a downloaded update, which installs when the app quits. */
export function updateNotice(version: string): { title: string; body: string } {
  return { title: 'agent-mesh update ready', body: `${version ? `Version ${version} is` : 'A new version is'} downloaded and installs when you quit.` }
}
