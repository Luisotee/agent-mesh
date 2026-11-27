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

  it('takes the same chord with Shift added in a terminal (Ctrl+Shift+W closes there)', () => {
    expect(commandFor(press(inTerm, 'W', { ctrlKey: true, shiftKey: true }), keys)).toBe('tab_close')
    // A chord that has Shift already keeps it, in a terminal too.
    expect(commandFor(press(inTerm, 'A', { ctrlKey: true, shiftKey: true }), keys)).toBe('waiting_next')
    expect(commandFor(press(inTerm, 'Tab', { ctrlKey: true }), keys)).toBe('tab_next')
  })

  it('leaves a chord without Ctrl to a field or terminal being typed in', () => {
    expect(commandFor(press(page, 'F2'), keys)).toBe('rename')
    expect(commandFor(press(field, 'F2'), keys)).toBeUndefined()
    expect(commandFor(press(field, 'ArrowDown', { altKey: true }), keys)).toBeUndefined()
    expect(commandFor(press(page, 'ArrowDown', { altKey: true }), keys)).toBe('sidebar_next')
    expect(commandFor(press(field, 'b', { ctrlKey: true }), keys)).toBe('sidebar')
  })
})
