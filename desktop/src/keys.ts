/**
 * Key chords: parsing the host's `app.keys.*` settings (`Mod+Shift+P`), matching keyboard events against them, and
 * turning a pressed combination back into one. The host checks every chord it stores (`daemon/internal/settings`,
 * `ValidShortcut`); this is only what the window needs to act on one.
 */
export interface Chord {
  ctrl: boolean
  alt: boolean
  shift: boolean
  meta: boolean
  /** A single letter in upper case, a character, or a name such as `Tab`, `ArrowLeft`, `F5`. */
  key: string
}

/** Whether `Mod` means Cmd (macOS) rather than Ctrl. */
export const isMac = typeof navigator !== 'undefined' && /Mac|iPhone|iPad/.test(navigator.platform || navigator.userAgent)

const NAMED = new Set(['Enter', 'Tab', 'Space', 'Escape', 'Backspace', 'Delete', 'Insert', 'Home', 'End', 'PageUp', 'PageDown', 'ArrowUp', 'ArrowDown', 'ArrowLeft', 'ArrowRight', 'Plus'])

/** Characters of the keys that give another character with Shift, by physical key. */
const CODE_CHARS: Record<string, string> = { Comma: ',', Period: '.', Slash: '/', Semicolon: ';', Quote: "'", BracketLeft: '[', BracketRight: ']', Backslash: '\\', Minus: '-', Equal: '=', Backquote: '`' }

/** A chord from its text, or undefined for "" (none) and for text that isn't one. */
export function parseChord(text: string, mac = isMac): Chord | undefined {
  if (!text) return undefined
  const parts = text.split('+')
  const key = parts.pop()!
  const c: Chord = { ctrl: false, alt: false, shift: false, meta: false, key: key.length === 1 ? key.toUpperCase() : key }
  if (!key || (key.length > 1 && !NAMED.has(key) && !/^F([1-9]|1\d|2[0-4])$/.test(key))) return undefined
  for (const m of parts) {
    if (m === 'Mod') c[mac ? 'meta' : 'ctrl'] = true
    else if (m === 'Ctrl') c.ctrl = true
    else if (m === 'Alt') c.alt = true
    else if (m === 'Shift') c.shift = true
    else if (m === 'Meta') c.meta = true
    else return undefined
  }
  return c
}

/** The key a keyboard event is, in the chord's vocabulary. */
export function keyOf(e: Pick<KeyboardEvent, 'key' | 'code' | 'shiftKey'>): string {
  if (e.key === ' ') return 'Space'
  if (e.key === '+') return 'Plus'
  // A letter key that doesn't type a Latin letter (Option+K types ˚ on a Mac, a Russian layout types Cyrillic, a dead key
  // types nothing): it is still that physical key.
  if (/^Key[A-Z]$/.test(e.code) && (e.key.length === 1 || e.key === 'Dead' || e.key === 'Unidentified') && !/^[a-z]$/i.test(e.key)) return e.code.slice(3)
  if (e.key.length === 1) {
    if (/[a-z]/i.test(e.key)) return e.key.toUpperCase()
    // Shift changes what a symbol or digit key types: name the key by what it is without it.
    if (e.shiftKey) return CODE_CHARS[e.code] ?? (/^Digit\d$/.test(e.code) ? e.code.slice(5) : e.key)
    return e.key
  }
  return e.key
}

export const sameChord = (a: Chord, b: Chord) => a.ctrl === b.ctrl && a.alt === b.alt && a.shift === b.shift && a.meta === b.meta && a.key === b.key

/** Whether a keyboard event is exactly this chord (no modifier more or less). */
export function matches(e: Pick<KeyboardEvent, 'key' | 'code' | 'shiftKey' | 'ctrlKey' | 'altKey' | 'metaKey'>, c: Chord): boolean {
  return e.ctrlKey === c.ctrl && e.altKey === c.alt && e.shiftKey === c.shift && e.metaKey === c.meta && keyOf(e) === c.key
}

const MODIFIER_KEYS = new Set(['Control', 'Shift', 'Alt', 'Meta', 'AltGraph', 'OS'])

/** The chord text a pressed combination is, or undefined while only modifiers are down. Uses `Mod` for the platform's own. */
export function chordFromEvent(e: Pick<KeyboardEvent, 'key' | 'code' | 'shiftKey' | 'ctrlKey' | 'altKey' | 'metaKey'>, mac = isMac): string | undefined {
  if (MODIFIER_KEYS.has(e.key)) return undefined
  const mods: string[] = []
  const main = mac ? e.metaKey : e.ctrlKey
  if (main) mods.push('Mod')
  if (mac ? e.ctrlKey : false) mods.push('Ctrl')
  if (!mac ? e.metaKey : false) mods.push('Meta')
  if (e.altKey) mods.push('Alt')
  if (e.shiftKey) mods.push('Shift')
  return [...mods, keyOf(e)].join('+')
}

/** A chord's text as a person reads it: the platform's own name for Mod. */
export function displayChord(text: string, mac = isMac): string {
  return text
    .split('+')
    .map((p) => (p === 'Mod' ? (mac ? 'Cmd' : 'Ctrl') : p === 'Plus' ? '+' : p))
    .join('+')
}

/** Whether a keyboard event comes from a chord recorder (where a key is being chosen, not used). */
export const isRecorder = (t: EventTarget | null): boolean => t instanceof Element && !!t.closest('[data-shortcut-recorder]')
