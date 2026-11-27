import { readFileSync } from 'node:fs'
import path from 'node:path'
import { describe, expect, it } from 'vitest'
import { applyEdit, composerKey, type Field, type KeyPress } from './composer-keys'

/** A field from text with `|` as the cursor, or `«` `»` around the selection. */
function field(s: string): Field {
  const start = s.includes('«') ? s.indexOf('«') : s.indexOf('|')
  const value = s.replace(/[|«»]/g, '')
  const end = s.includes('»') ? s.indexOf('»') - 1 : start
  return { value, start, end }
}
/** The field as text, the same way. */
function show(f: Field): string {
  if (f.start === f.end) return f.value.slice(0, f.start) + '|' + f.value.slice(f.start)
  return f.value.slice(0, f.start) + '«' + f.value.slice(f.start, f.end) + '»' + f.value.slice(f.end)
}
const k = (key: string, o: Partial<KeyPress> = {}): KeyPress => ({ key, shift: false, mod: false, alt: false, ...o })
function press(s: string, key: KeyPress, sends = true): string | 'send' | undefined {
  const f = field(s)
  const out = composerKey(f, key, sends)
  if (!out) return undefined
  if (out.kind === 'send') return 'send'
  return show(applyEdit(f, out.edit))
}

describe('composerKey', () => {
  it('sends on Enter, adds a line on Shift+Enter (left to the field), and only adds one in a field that does not send', () => {
    expect(press('hello|', k('Enter'))).toBe('send')
    expect(press('hello|', k('Enter', { shift: true }))).toBeUndefined()
    expect(press('hello|', k('Enter'), false)).toBeUndefined()
  })

  it('continues a list on Enter: the same bullet, the next number, an empty task box; an empty item ends the list', () => {
    expect(press('- one|', k('Enter'))).toBe('- one\n- |')
    expect(press('  * one|', k('Enter'))).toBe('  * one\n  * |')
    expect(press('1. one|', k('Enter'))).toBe('1. one\n2. |')
    expect(press('9) nine|', k('Enter'))).toBe('9) nine\n10) |')
    expect(press('- [x] done|', k('Enter'))).toBe('- [x] done\n- [ ] |')
    expect(press('- one\n- |', k('Enter'))).toBe('- one\n|')
    expect(press('- one\n2. |', k('Enter'))).toBe('- one\n|')
    // The cursor inside the marker, or a selection, is not a new item: Enter sends.
    expect(press('-| one', k('Enter'))).toBe('send')
    expect(press('- «on»e', k('Enter'))).toBe('send')
  })

  it('indents and outdents a list item, or the lines the selection touches, by two spaces', () => {
    expect(press('- a|', k('Tab'))).toBe('  - a|')
    expect(press('  - a|', k('Tab', { shift: true }))).toBe('- a|')
    expect(press('«a\nb»', k('Tab'))).toBe('  «a\n  b»')
    expect(press('  «a\n b»', k('Tab', { shift: true }))).toBe('«a\nb»')
    // Nothing to outdent: the key is left alone (focus moves on).
    expect(press('a|', k('Tab', { shift: true }))).toBeUndefined()
    // Not a list item and nothing selected: Tab moves the focus on, as in any field.
    expect(press('a message|', k('Tab'))).toBeUndefined()
    expect(press('  code|', k('Tab', { shift: true }))).toBeUndefined()
  })

  it('wraps the selection on Ctrl+B, I and E (and unwraps it), and makes a link on Ctrl+K', () => {
    expect(press('a «word» b', k('b', { mod: true }))).toBe('a **«word»** b')
    expect(press('a **«word»** b', k('b', { mod: true }))).toBe('a «word» b')
    expect(press('a «**word**» b', k('b', { mod: true }))).toBe('a «word» b')
    expect(press('«**a** and **b**»', k('b', { mod: true }))).toBe('**«**a** and **b**»**')
    expect(press('«__init__»', k('i', { mod: true }))).toBe('_«__init__»_')
    expect(press('|', k('i', { mod: true }))).toBe('_|_')
    expect(press('run «ls»', k('e', { mod: true }))).toBe('run `«ls»`')
    expect(press('see «docs»', k('k', { mod: true }))).toBe('see [docs](|)')
    expect(press('|', k('k', { mod: true }))).toBe('[|]()')
    // Other chords are the app's, or the field's own.
    expect(press('a|', k('z', { mod: true }))).toBeUndefined()
    expect(press('a|', k('B', { mod: true, shift: true }))).toBeUndefined()
  })
})

/**
 * The rules this field shares with Android's `MarkdownEdit.kt` are `shared/markdown-edit/cases.json`, which that app's
 * `MarkdownEditTest` reads too: a rule changed on one side fails the other. `expected: null` is a key the field leaves
 * alone, and here that includes Enter, which sends.
 */
describe('the shared Markdown cases', () => {
  const file = path.resolve(import.meta.dirname, '../../shared/markdown-edit/cases.json')
  const cases = (JSON.parse(readFileSync(file, 'utf8')) as { cases: Array<{ name: string; action: string; input: string; expected: string | null }> }).cases
  const keyOf: Record<string, KeyPress> = {
    enter: k('Enter'),
    tab: k('Tab'),
    'shift-tab': k('Tab', { shift: true }),
    bold: k('b', { mod: true }),
    italic: k('i', { mod: true }),
    code: k('e', { mod: true }),
    link: k('k', { mod: true }),
  }
  it('has the cases and no action this test does not know', () => {
    expect(cases.length).toBeGreaterThanOrEqual(20)
    for (const c of cases) expect(keyOf[c.action], `${c.name}: action ${c.action}`).toBeDefined()
  })
  for (const c of cases) {
    it(c.name, () => {
      const got = press(c.input, keyOf[c.action]!)
      expect(got === 'send' ? null : (got ?? null)).toBe(c.expected)
    })
  }
})
