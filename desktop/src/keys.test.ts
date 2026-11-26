import { describe, expect, it } from 'vitest'
import { chordFromEvent, displayChord, keyOf, matches, parseChord, sameChord } from './keys'
import { appKeys, DEFAULT_KEYS } from './prefs'

const ev = (key: string, o: Partial<Record<'ctrlKey' | 'altKey' | 'shiftKey' | 'metaKey', boolean>> & { code?: string } = {}) => ({ key, code: o.code ?? '', ctrlKey: false, altKey: false, shiftKey: false, metaKey: false, ...o })

describe('parseChord', () => {
  it('reads modifiers and a key, with Mod as the platform\'s own', () => {
    expect(parseChord('Mod+Shift+P', false)).toEqual({ ctrl: true, alt: false, shift: true, meta: false, key: 'P' })
    expect(parseChord('Mod+Shift+P', true)).toEqual({ ctrl: false, alt: false, shift: true, meta: true, key: 'P' })
    expect(parseChord('Ctrl+Tab', true)).toMatchObject({ ctrl: true, meta: false, key: 'Tab' })
    expect(parseChord('Mod+,', false)).toMatchObject({ ctrl: true, key: ',' })
    expect(parseChord('Alt+k', false)).toMatchObject({ alt: true, key: 'K' })
    expect(parseChord('Shift+F12', false)).toMatchObject({ shift: true, key: 'F12' })
  })
  it('gives nothing for "none" or text that is not a chord', () => {
    for (const bad of ['', 'Mod+', 'Hyper+P', 'Mod+Nope', 'Mod+F25']) expect(parseChord(bad)).toBeUndefined()
  })
  it('knows every default', () => {
    for (const [id, text] of Object.entries(DEFAULT_KEYS)) expect(parseChord(text), id).toBeDefined()
  })
})

describe('matching events', () => {
  const palette = parseChord('Mod+Shift+P', false)!
  it('needs exactly the chord\'s modifiers', () => {
    expect(matches(ev('P', { ctrlKey: true, shiftKey: true }), palette)).toBe(true)
    expect(matches(ev('p', { ctrlKey: true, shiftKey: true }), palette)).toBe(true)
    expect(matches(ev('P', { ctrlKey: true }), palette)).toBe(false)
    expect(matches(ev('P', { ctrlKey: true, shiftKey: true, altKey: true }), palette)).toBe(false)
    expect(matches(ev('P', { shiftKey: true }), palette)).toBe(false)
  })
  it('names a key by what it is with Shift down, and special keys by name', () => {
    expect(keyOf(ev('<', { shiftKey: true, code: 'Comma' }))).toBe(',')
    expect(keyOf(ev('!', { shiftKey: true, code: 'Digit1' }))).toBe('1')
    // A letter key that types something else (Option on a Mac, another alphabet, a dead key) is still its letter.
    expect(keyOf(ev('˚', { altKey: true, code: 'KeyK' }))).toBe('K')
    expect(keyOf(ev('з', { code: 'KeyP' }))).toBe('P')
    expect(keyOf(ev('Dead', { altKey: true, code: 'KeyE' }))).toBe('E')
    expect(keyOf(ev('p', { code: 'KeyL' }))).toBe('P') // the layout's own letter wins where it is Latin (Dvorak)
    expect(matches(ev('˚', { altKey: true, code: 'KeyK' }), parseChord('Alt+K', true)!)).toBe(true)
    expect(keyOf(ev(' '))).toBe('Space')
    expect(keyOf(ev('+', { shiftKey: true }))).toBe('Plus')
    expect(matches(ev('Tab', { ctrlKey: true }), parseChord('Ctrl+Tab', false)!)).toBe(true)
    expect(matches(ev('Tab', { ctrlKey: true, shiftKey: true }), parseChord('Ctrl+Shift+Tab', false)!)).toBe(true)
    expect(matches(ev(',', { ctrlKey: true }), parseChord('Mod+,', false)!)).toBe(true)
  })
})

describe('recording', () => {
  it('turns a pressed combination into chord text, and waits while only modifiers are down', () => {
    expect(chordFromEvent(ev('Control', { ctrlKey: true }), false)).toBeUndefined()
    expect(chordFromEvent(ev('K', { ctrlKey: true, shiftKey: true }), false)).toBe('Mod+Shift+K')
    expect(chordFromEvent(ev('k', { metaKey: true }), true)).toBe('Mod+K')
    expect(chordFromEvent(ev('k', { ctrlKey: true }), true)).toBe('Ctrl+K')
    expect(chordFromEvent(ev('k', { metaKey: true }), false)).toBe('Meta+K')
    expect(chordFromEvent(ev('ArrowLeft', { altKey: true }), false)).toBe('Alt+ArrowLeft')
  })
  it('round-trips, and shows Mod by platform', () => {
    const text = chordFromEvent(ev('Tab', { ctrlKey: true, shiftKey: true }), false)!
    expect(sameChord(parseChord(text, false)!, parseChord('Ctrl+Shift+Tab', false)!)).toBe(true)
    expect(displayChord('Mod+Shift+P', false)).toBe('Ctrl+Shift+P')
    expect(displayChord('Mod+Shift+P', true)).toBe('Cmd+Shift+P')
    expect(displayChord('Mod+Plus', false)).toBe('Ctrl++')
  })
})

describe('appKeys', () => {
  it('is the default until the host says otherwise, and "" is unbound', () => {
    expect(appKeys({})).toEqual(DEFAULT_KEYS)
  })
})
