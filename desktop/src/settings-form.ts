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

/** The group a key is shown under: its first segment (`tiers`, `review`, `app`), or "general" for a lone word. */
export function groupOf(key: string): string {
  const i = key.indexOf('.')
  return i < 0 ? 'general' : key.slice(0, i)
}
