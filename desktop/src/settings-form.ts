import type { JsonValue } from '@bufbuild/protobuf'

/** How a setting is edited, from its schema's type. Nothing here knows any setting by name. */
export type Control =
  | { kind: 'bool' }
  | { kind: 'number'; integer: boolean; min: number; max?: number }
  | { kind: 'text' }
  | { kind: 'select'; choices: string[] }
  /** One entry per line: a list of strings where order matters (a chain of models). */
  | { kind: 'lines' }
  /** A set of the choices (checkboxes). */
  | { kind: 'multi'; choices: string[] }
  /** A key chord, recorded by pressing it; `bare`: a named key alone (Escape, ArrowUp) may be one too (the chat composer's keys). */
  | { kind: 'shortcut'; bare?: boolean }
  | { kind: 'json' }

const EFFORTS = ['low', 'medium', 'high', 'xhigh', 'max']

export function controlFor(type: string, choices: string[]): Control {
  switch (type) {
    case 'bool':
      return { kind: 'bool' }
    case 'int':
      return { kind: 'number', integer: true, min: 0 }
    case 'percent':
      return { kind: 'number', integer: true, min: 0, max: 100 }
    case 'seconds':
      return { kind: 'number', integer: false, min: 0, max: 1e9 }
    case 'effort':
      return { kind: 'select', choices: choices.length ? choices : EFFORTS }
    case 'kinds':
      return choices.length ? { kind: 'multi', choices } : { kind: 'lines' }
    case 'strings':
    case 'chain':
      return { kind: 'lines' }
    case 'string':
      // A string with choices is one of them (the host refuses any other).
      return choices.length ? { kind: 'select', choices } : { kind: 'text' }
    case 'mb_or_auto':
    case 'int_or_auto':
      return { kind: 'text' }
    case 'shortcut':
      return { kind: 'shortcut' }
    case 'chat_key':
      return { kind: 'shortcut', bare: true }
    default:
      // slots, json, and any type a newer host has that this app doesn't know: raw JSON is always right.
      return { kind: 'json' }
  }
}

/** A value as the text of a field. */
export function toText(c: Control, v: unknown): string {
  switch (c.kind) {
    case 'lines':
      return Array.isArray(v) ? v.join('\n') : ''
    case 'json':
      return JSON.stringify(v ?? null, null, 2)
    case 'number':
    case 'text':
    case 'select':
    case 'shortcut':
      return v === undefined || v === null ? '' : String(v)
    default:
      return ''
  }
}

export type Parsed = { ok: true; value: JsonValue } | { ok: false; error: string }

/**
 * A field's text as the JSON value to send. Only what can't be sent at all is refused here (not a number, not
 * JSON); whether the value is allowed is the host's check, whose message the form shows.
 */
export function fromText(type: string, c: Control, text: string): Parsed {
  switch (c.kind) {
    case 'number': {
      if (text.trim() === '') return { ok: false, error: 'enter a number' }
      const n = Number(text)
      return Number.isFinite(n) ? { ok: true, value: n } : { ok: false, error: 'not a number' }
    }
    case 'lines':
      return { ok: true, value: text.split('\n').map((l) => l.trim()).filter(Boolean) }
    case 'json':
      try {
        return { ok: true, value: JSON.parse(text) as JsonValue }
      } catch (e) {
        return { ok: false, error: `not valid JSON: ${e instanceof Error ? e.message : String(e)}` }
      }
    case 'text':
      // mb_or_auto and int_or_auto are "auto" or a whole number.
      if (autoOrNumber(type) && text.trim() !== '' && text.trim() !== 'auto' && Number.isFinite(Number(text))) return { ok: true, value: Number(text) }
      return { ok: true, value: autoOrNumber(type) ? text.trim() : text }
    default:
      return { ok: true, value: text }
  }
}

/** A type whose value is "auto" or a number. */
function autoOrNumber(type: string): boolean {
  return type === 'mb_or_auto' || type === 'int_or_auto'
}

/** The groups of the settings screen, in its order (the daemon's `settings.Groups`); a setting of a group not listed here goes last, under "Other". */
export const GROUPS = ['Appearance', 'Notifications', 'Shortcuts', 'Agents and models', 'Reviews', 'Projects and autonomy', 'Limits and quota', 'Delivery'] as const
export const OTHER_GROUP = 'Other'

/** What the screen needs of a setting: its key and where it was set, and what the host's schema says of it. */
export interface Listed {
  key: string
  isSet: boolean
  schema?: { label: string; group: string; advanced: boolean; summary: string }
}

/** The name a setting is listed under: the host's label, or the key for a host that sends none. */
export const labelOf = (s: Listed): string => s.schema?.label || s.key

/** The group a setting is under: the host's, or "Other" for a host that sends none (or one this app doesn't know). */
export function groupOf(s: Listed): string {
  const g = s.schema?.group ?? ''
  return (GROUPS as readonly string[]).includes(g) ? g : OTHER_GROUP
}

/** Whether a search matches a setting: every word of it is in the label, the description or the key. */
export function matches(s: Listed, query: string): boolean {
  const words = query.toLowerCase().split(/\s+/).filter(Boolean)
  const hay = `${labelOf(s)}\n${s.schema?.summary ?? ''}\n${s.key}`.toLowerCase()
  return words.every((w) => hay.includes(w))
}

const natural = { numeric: true } as const
/** A group's order: by label, the shortcuts by key (which keeps "tab 1 … tab 8" in order). */
const inGroup = (group: string) => (a: Listed, b: Listed): number =>
  group === 'Shortcuts' ? a.key.localeCompare(b.key, undefined, natural) : labelOf(a).localeCompare(labelOf(b), undefined, natural)

/** The per-repo lists: the screen shows them as one row of links to each project's settings, not as rows of their own. */
export const PER_PROJECT = new Set(['repos.user_merges', 'repos.user_approves_plans', 'repos.no_free_models'])

export interface Section<T extends Listed> {
  group: string
  /** What is listed in the open part: everything, or without the advanced ones while they are folded. */
  shown: T[]
  /** The advanced settings of the group, folded until asked for (listed with the rest while searching). */
  advanced: T[]
  /** How many rows the group has under this search (the per-repo lists count as the one row of links). */
  count: number
}

/**
 * The screen's sections: the settings that pass the search (and, when asked, were set), by group, each in the order of
 * its label. A search lists the advanced ones with the rest, since the user asked for them by name.
 */
export function sectionsOf<T extends Listed>(all: T[], query: string, changedOnly: boolean): Array<Section<T>> {
  const q = query.trim()
  const by = new Map<string, T[]>()
  for (const s of all) {
    if (changedOnly && !s.isSet) continue
    if (q && !matches(s, q)) continue
    const g = groupOf(s)
    by.set(g, [...(by.get(g) ?? []), s])
  }
  const order = [...GROUPS, OTHER_GROUP] as string[]
  return order
    .filter((g) => by.has(g))
    .map((g) => {
      const list = by.get(g)!.sort(inGroup(g))
      const folded = !q && !changedOnly
      let shown = folded ? list.filter((s) => !s.schema?.advanced) : list
      let advanced = folded ? list.filter((s) => s.schema?.advanced) : []
      // A group with nothing but advanced settings opens with them listed, not to an empty list over a closed fold.
      if (shown.length === 0) [shown, advanced] = [advanced, []]
      const links = list.some((s) => PER_PROJECT.has(s.key))
      return { group: g, shown, advanced, count: list.filter((s) => !PER_PROJECT.has(s.key)).length + (links ? 1 : 0) }
    })
}
