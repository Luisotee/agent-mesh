import { toJson } from '@bufbuild/protobuf'
import { ValueSchema } from '@bufbuild/protobuf/wkt'
import type { Setting } from './gen/agentmesh/v1/types_pb'

/** What the host says about how this app should behave (`app.*` settings), with the host's defaults. */
export interface AppPrefs {
  notifyEnabled: boolean
  notifySound: boolean
  notifyEvents: ReadonlySet<string>
  closeToTray: boolean
}

export function jsonOf(s: Setting | undefined): unknown {
  return s?.value ? toJson(ValueSchema, s.value) : undefined
}

/** Until the settings arrive the app notifies as the defaults say, so a first event isn't lost to a race. */
export const DEFAULT_PREFS: AppPrefs = {
  notifyEnabled: true,
  notifySound: true,
  notifyEvents: new Set(['input', 'plan', 'question', 'report', 'failed']),
  closeToTray: true,
}

export function appPrefs(settings: Record<string, Setting>): AppPrefs {
  const bool = (key: string, d: boolean) => {
    const v = jsonOf(settings[key])
    return typeof v === 'boolean' ? v : d
  }
  const events = jsonOf(settings['app.notify.events'])
  return {
    notifyEnabled: bool('app.notify.enabled', DEFAULT_PREFS.notifyEnabled),
    notifySound: bool('app.notify.sound', DEFAULT_PREFS.notifySound),
    notifyEvents: Array.isArray(events) ? new Set(events.filter((e): e is string => typeof e === 'string')) : DEFAULT_PREFS.notifyEvents,
    closeToTray: bool('app.tray.close_to_tray', DEFAULT_PREFS.closeToTray),
  }
}
