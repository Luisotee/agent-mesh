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
  notifyEvents: new Set(['input', 'plan', 'question', 'report', 'failed', 'error']),
  closeToTray: false,
  dockNewAgents: 'dock',
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
  }
}

/** The keys of the commands that can be bound, with the host's defaults (daemon/internal/settings/schema.go, AppLeaves). */
export const DEFAULT_KEYS = {
  palette: 'Mod+Shift+P',
  settings: 'Mod+,',
  chat_toggle: 'Mod+Shift+J',
  dock: 'Mod+Shift+D',
  tab_next: 'Ctrl+Tab',
  tab_prev: 'Ctrl+Shift+Tab',
  tab_close: 'Mod+Shift+W',
  waiting_next: 'Mod+Shift+A',
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
