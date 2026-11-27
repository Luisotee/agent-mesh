import { describe, expect, it } from 'vitest'
import { controlFor, fromText, groupOf, labelOf, matches, sectionsOf, toText, type Listed } from './settings-form'

describe('controlFor', () => {
  it('picks a control by the schema type', () => {
    expect(controlFor('bool', [])).toEqual({ kind: 'bool' })
    expect(controlFor('percent', [])).toEqual({ kind: 'number', integer: true, min: 0, max: 100 })
    expect(controlFor('int', [])).toMatchObject({ kind: 'number', integer: true })
    expect(controlFor('seconds', [])).toMatchObject({ kind: 'number', integer: false })
    expect(controlFor('effort', ['low', 'high'])).toEqual({ kind: 'select', choices: ['low', 'high'] })
    expect(controlFor('effort', [])).toMatchObject({ kind: 'select', choices: expect.arrayContaining(['low', 'max']) })
    expect(controlFor('kinds', ['claude', 'agy'])).toEqual({ kind: 'multi', choices: ['claude', 'agy'] })
    expect(controlFor('chain', [])).toEqual({ kind: 'lines' })
    expect(controlFor('string', [])).toEqual({ kind: 'text' })
    expect(controlFor('string', ['dock', 'tab'])).toEqual({ kind: 'select', choices: ['dock', 'tab'] })
    expect(controlFor('mb_or_auto', [])).toEqual({ kind: 'text' })
    expect(controlFor('shortcut', [])).toEqual({ kind: 'shortcut' })
    expect(controlFor('chat_key', [])).toEqual({ kind: 'shortcut', bare: true })
    expect(controlFor('int_or_auto', [])).toEqual({ kind: 'text' })
  })
  it('falls back to JSON for slots, json and any type it does not know', () => {
    for (const t of ['slots', 'json', 'something-newer']) expect(controlFor(t, [])).toEqual({ kind: 'json' })
  })
})

describe('text and values', () => {
  it('round-trips lines, JSON and numbers', () => {
    const lines = controlFor('chain', [])
    expect(toText(lines, ['a', 'b'])).toBe('a\nb')
    expect(fromText('chain', lines, ' a \n\n b ')).toEqual({ ok: true, value: ['a', 'b'] })
    const j = controlFor('slots', [])
    expect(fromText('slots', j, toText(j, [['claude'], ['agy']]))).toEqual({ ok: true, value: [['claude'], ['agy']] })
    expect(fromText('slots', j, '[')).toMatchObject({ ok: false, error: expect.stringContaining('not valid JSON') })
    const n = controlFor('percent', [])
    expect(fromText('percent', n, '90')).toEqual({ ok: true, value: 90 })
    expect(fromText('percent', n, 'x')).toEqual({ ok: false, error: 'not a number' })
    expect(fromText('percent', n, '')).toEqual({ ok: false, error: 'enter a number' })
  })
  it('mb_or_auto is auto or a number', () => {
    const t = controlFor('mb_or_auto', [])
    expect(fromText('mb_or_auto', t, 'auto')).toEqual({ ok: true, value: 'auto' })
    expect(fromText('mb_or_auto', t, '2048')).toEqual({ ok: true, value: 2048 })
    expect(fromText('string', controlFor('string', []), '2048')).toEqual({ ok: true, value: '2048' })
  })
  it('int_or_auto is auto or a number', () => {
    const t = controlFor('int_or_auto', [])
    expect(fromText('int_or_auto', t, 'auto')).toEqual({ ok: true, value: 'auto' })
    expect(fromText('int_or_auto', t, '12')).toEqual({ ok: true, value: 12 })
  })
})

const item = (key: string, label: string, group: string, o: { advanced?: boolean; isSet?: boolean; summary?: string } = {}): Listed => ({
  key,
  isSet: o.isSet ?? false,
  schema: { label, group, advanced: o.advanced ?? false, summary: o.summary ?? `about ${key}` },
})

describe('the screen\'s sections', () => {
  const all = [
    item('quota_stop', 'Stop starting agents at quota', 'Limits and quota'),
    item('app.theme', 'Theme', 'Appearance'),
    item('compact.agy', 'Compact an agy conversation at', 'Limits and quota', { advanced: true }),
    item('app.keys.tab_2', 'Go to the second tab', 'Shortcuts'),
    item('app.keys.tab_10', 'Go to the tenth tab', 'Shortcuts'),
    item('app.keys.palette', 'Command palette', 'Shortcuts'),
    item('old.thing', 'old.thing', ''),
  ]
  it('puts a setting under the group the host names, an unnamed one under Other, and orders the groups', () => {
    expect(sectionsOf(all, '', false).map((x) => x.group)).toEqual(['Appearance', 'Shortcuts', 'Limits and quota', 'Other'])
    expect(groupOf(all[6]!)).toBe('Other')
    expect(labelOf(all[6]!)).toBe('old.thing')
  })
  it('folds the advanced ones, and lists them when searching or filtering to the changed', () => {
    const limits = (q: string, changed = false) => sectionsOf(all, q, changed).find((x) => x.group === 'Limits and quota')!
    expect(limits('')).toMatchObject({ count: 2, advanced: [{ key: 'compact.agy' }] })
    expect(limits('').shown.map((s) => s.key)).toEqual(['quota_stop'])
    expect(limits('compact').advanced).toEqual([])
    expect(limits('compact').shown.map((s) => s.key)).toEqual(['compact.agy'])
  })
  it('searches the label, the description and the key, every word of the query', () => {
    expect(matches(all[0]!, 'quota')).toBe(true)
    expect(matches(all[0]!, 'agents QUOTA')).toBe(true)
    expect(matches(all[0]!, 'about quota_stop')).toBe(true)
    expect(matches(all[0]!, 'quota theme')).toBe(false)
    expect(matches(item('a', 'A', 'Appearance', { summary: 'draws the colours' }), 'colours')).toBe(true)
  })
  it('keeps the shortcuts in key order and the rest by label', () => {
    expect(sectionsOf(all, '', false).find((x) => x.group === 'Shortcuts')!.shown.map((s) => s.key)).toEqual(['app.keys.palette', 'app.keys.tab_2', 'app.keys.tab_10'])
  })
  it('lists only what was set when asked', () => {
    const some = [...all, item('x', 'Retire', 'Agents and models', { isSet: true })]
    expect(sectionsOf(some, '', true).map((x) => x.group)).toEqual(['Agents and models'])
  })
})
