import { toJson } from '@bufbuild/protobuf'
import { ValueSchema } from '@bufbuild/protobuf/wkt'
import type { Setting } from './gen/agentmesh/v1/types_pb'

/** What the host says about how this app should behave (`app.*` settings), with the host's defaults. */
export interface AppPrefs {
  notifyEnabled: boolean
  notifySound: boolean
  notifyEvents: ReadonlySet<string>
  closeToTray: boolean
  /** What a worker that starts while the app is open does (`app.dock.new_agents`). */
  dockNewAgents: DockNewAgents
  /** The host's choice of theme (a name from `shared/themes`; an unknown one falls back to the default). */
  theme: string
  /** An agent or the CLI may open a view even when the window isn't focused (`app.open_views`). */
  openViews: boolean
}

export type DockNewAgents = 'dock' | 'badge' | 'tab' | 'none'
const DOCK_MODES: readonly string[] = ['dock', 'badge', 'tab', 'none']

export function jsonOf(s: Setting | undefined): unknown {
  return s?.value ? toJson(ValueSchema, s.value) : undefined
}

/**
 * Until the settings arrive the app acts as the host's defaults say (daemon/internal/settings/schema.go, AppLeaves),
 * so a first event isn't lost to a race.
 */
export const DEFAULT_PREFS: AppPrefs = {
  notifyEnabled: true,
  notifySound: true,
  notifyEvents: new Set(['input', 'plan', 'question', 'report', 'failed', 'error', 'finished', 'ready']),
  closeToTray: false,
  dockNewAgents: 'dock',
  theme: 'Dracula',
  openViews: false,
}

export function appPrefs(settings: Record<string, Setting>): AppPrefs {
  const bool = (key: string, d: boolean) => {
    const v = jsonOf(settings[key])
    return typeof v === 'boolean' ? v : d
  }
  const events = jsonOf(settings['app.notify.events'])
  const dock = jsonOf(settings['app.dock.new_agents'])
  return {
    notifyEnabled: bool('app.notify.enabled', DEFAULT_PREFS.notifyEnabled),
    notifySound: bool('app.notify.sound', DEFAULT_PREFS.notifySound),
    notifyEvents: Array.isArray(events) ? new Set(events.filter((e): e is string => typeof e === 'string')) : DEFAULT_PREFS.notifyEvents,
    closeToTray: bool('app.tray.close_to_tray', DEFAULT_PREFS.closeToTray),
    dockNewAgents: DOCK_MODES.includes(dock as string) ? (dock as DockNewAgents) : DEFAULT_PREFS.dockNewAgents,
    openViews: bool('app.open_views', DEFAULT_PREFS.openViews),
    theme: typeof jsonOf(settings['app.theme']) === 'string' ? (jsonOf(settings['app.theme']) as string) : DEFAULT_PREFS.theme,
  }
}

/** The keys of the commands that can be bound, with the host's defaults (daemon/internal/settings/schema.go, AppLeaves). */
export const DEFAULT_KEYS = {
  palette: 'Mod+Shift+P',
  search: 'Mod+K',
  settings: 'Mod+,',
  chat_toggle: 'Mod+Shift+J',
  dock: 'Mod+Shift+D',
  tab_next: 'Ctrl+Tab',
  tab_prev: 'Ctrl+Shift+Tab',
  tab_next_page: 'Ctrl+PageDown',
  tab_prev_page: 'Ctrl+PageUp',
  tab_close: 'Mod+W',
  tab_new: 'Mod+T',
  tab_reopen: 'Mod+Shift+T',
  tab_1: 'Mod+1',
  tab_2: 'Mod+2',
  tab_3: 'Mod+3',
  tab_4: 'Mod+4',
  tab_5: 'Mod+5',
  tab_6: 'Mod+6',
  tab_7: 'Mod+7',
  tab_8: 'Mod+8',
  tab_last: 'Mod+9',
  waiting_next: 'Mod+Shift+A',
  sidebar: 'Mod+B',
  sidebar_next: 'Alt+ArrowDown',
  sidebar_prev: 'Alt+ArrowUp',
  panel: 'Ctrl+Alt+B',
  find: 'Mod+F',
  rename: 'F2',
} as const

export type KeyedCommand = keyof typeof DEFAULT_KEYS

/** Each bindable command's chord as the host has it ("" is unbound); the default until the host's settings arrive. */
export function appKeys(settings: Record<string, Setting>): Record<KeyedCommand, string> {
  const out = { ...DEFAULT_KEYS } as Record<KeyedCommand, string>
  for (const id of Object.keys(DEFAULT_KEYS) as KeyedCommand[]) {
    const v = jsonOf(settings[`app.keys.${id}`])
    if (typeof v === 'string') out[id] = v
  }
  return out
}

/** The chat composer's own keys, with the host's defaults (`app.chat_keys.*`): they act only while the composer has focus. */
export const DEFAULT_CHAT_KEYS = { interrupt: 'Escape', recall: 'ArrowUp' } as const

export type ChatKey = keyof typeof DEFAULT_CHAT_KEYS

/** The composer's keys as the host has them ("" is unbound); the default until the host's settings arrive. */
export function chatKeys(settings: Record<string, Setting>): Record<ChatKey, string> {
  const out = { ...DEFAULT_CHAT_KEYS } as Record<ChatKey, string>
  for (const id of Object.keys(DEFAULT_CHAT_KEYS) as ChatKey[]) {
    const v = jsonOf(settings[`app.chat_keys.${id}`])
    if (typeof v === 'string') out[id] = v
  }
  return out
}

