import { describe, expect, it } from 'vitest'
import { controlFor, fromText, groupOf, toText } from './settings-form'

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
  it('groups a key by its first segment', () => {
    expect(groupOf('tiers.claude.mid.model')).toBe('tiers')
    expect(groupOf('quota_stop')).toBe('general')
  })
})
