import { describe, expect, it } from 'vitest'
import { DEFAULT_KEYS } from './prefs'
import { commandFor } from './shortcuts'

const keys = { ...DEFAULT_KEYS }
const press = (target: Element, key: string, o: Partial<KeyboardEventInit> = {}) => {
  const e = new KeyboardEvent('keydown', { key, bubbles: true, cancelable: true, ...o })
  Object.defineProperty(e, 'target', { value: target })
  return e
}

describe('commandFor', () => {
  const term = document.createElement('div')
  term.className = 'xterm'
  const inTerm = term.appendChild(document.createElement('textarea'))
  const field = document.createElement('input')
  const page = document.createElement('div')

  it('runs a Mod+letter chord on the page, and leaves it to the shell in a focused terminal', () => {
    expect(commandFor(press(page, 'w', { ctrlKey: true }), keys)).toBe('tab_close')
    expect(commandFor(press(inTerm, 'w', { ctrlKey: true }), keys)).toBeUndefined()
    expect(commandFor(press(inTerm, 'k', { ctrlKey: true }), keys)).toBeUndefined()
  })

  it('runs a Cmd chord in a terminal too: a macOS terminal never sends Cmd keys to the shell', () => {
    const mac = { ...keys, tab_close: 'Meta+W' }
    expect(commandFor(press(inTerm, 'w', { metaKey: true }), mac)).toBe('tab_close')
  })

  it('leaves Ctrl+B and Ctrl+K to a markdown field, and runs them with Shift added there', () => {
    const md = document.createElement('textarea')
    md.setAttribute('data-markdown-field', '')
    expect(commandFor(press(md, 'b', { ctrlKey: true }), keys)).toBeUndefined()
    expect(commandFor(press(md, 'k', { ctrlKey: true }), keys)).toBeUndefined()
    expect(commandFor(press(md, 'B', { ctrlKey: true, shiftKey: true }), keys)).toBe('sidebar')
    // Other Ctrl chords run as in any field.
    expect(commandFor(press(md, 'w', { ctrlKey: true }), keys)).toBe('tab_close')
  })

  it('takes the same chord with Shift added in a terminal (Ctrl+Shift+W closes there)', () => {
    expect(commandFor(press(inTerm, 'W', { ctrlKey: true, shiftKey: true }), keys)).toBe('tab_close')
    // A chord that has Shift already keeps it, in a terminal too.
    expect(commandFor(press(inTerm, 'A', { ctrlKey: true, shiftKey: true }), keys)).toBe('waiting_next')
    expect(commandFor(press(inTerm, 'Tab', { ctrlKey: true }), keys)).toBe('tab_next')
    // Ctrl+Alt+B (the panel) is the shell's too, so Ctrl+Alt+Shift+B runs it there.
    expect(commandFor(press(inTerm, 'b', { ctrlKey: true, altKey: true }), keys)).toBeUndefined()
    expect(commandFor(press(inTerm, 'B', { ctrlKey: true, altKey: true, shiftKey: true }), keys)).toBe('panel')
  })

  it('gives the Shift form in a terminal to the Ctrl+letter command first (Ctrl+Shift+T there is a new lead)', () => {
    expect(commandFor(press(inTerm, 'T', { ctrlKey: true, shiftKey: true }), keys)).toBe('tab_new')
    expect(commandFor(press(page, 'T', { ctrlKey: true, shiftKey: true }), keys)).toBe('tab_reopen')
  })

  it('leaves a chord without Ctrl to a field or terminal being typed in', () => {
    expect(commandFor(press(page, 'F2'), keys)).toBe('rename')
    expect(commandFor(press(field, 'F2'), keys)).toBeUndefined()
    expect(commandFor(press(field, 'ArrowDown', { altKey: true }), keys)).toBeUndefined()
    expect(commandFor(press(page, 'ArrowDown', { altKey: true }), keys)).toBe('sidebar_next')
    expect(commandFor(press(field, 'b', { ctrlKey: true }), keys)).toBe('sidebar')
  })

  it('Mod+L focuses the message box, but in a terminal Ctrl+L stays the shell\'s clear-screen (Ctrl+Shift+L runs it)', () => {
    expect(commandFor(press(page, 'l', { ctrlKey: true }), keys)).toBe('composer')
    expect(commandFor(press(field, 'l', { ctrlKey: true }), keys)).toBe('composer')
    expect(commandFor(press(inTerm, 'l', { ctrlKey: true }), keys)).toBeUndefined()
    expect(commandFor(press(inTerm, 'L', { ctrlKey: true, shiftKey: true }), keys)).toBe('composer')
  })
})
