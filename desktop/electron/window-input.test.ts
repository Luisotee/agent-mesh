import { describe, expect, it } from 'vitest'
import { menuFor, nextZoom, zoomFrom, zoomOf, type MenuParams } from './window-input'

const key = (key: string, o: Partial<{ code: string; control: boolean; meta: boolean; alt: boolean; type: string }> = {}) => ({ type: 'keyDown', key, control: true, meta: false, alt: false, ...o })

describe('zoom keys', () => {
  it('takes both keys for zoom in, with or without Shift, and Mod on a Mac', () => {
    expect(zoomOf(key('='), false)).toBe('in')
    expect(zoomOf(key('+'), false)).toBe('in')
    expect(zoomOf(key('+', { code: 'NumpadAdd' }), false)).toBe('in')
    expect(zoomOf(key('-'), false)).toBe('out')
    expect(zoomOf(key('0'), false)).toBe('reset')
    expect(zoomOf(key('=', { control: false, meta: true }), true)).toBe('in')
    expect(zoomOf(key('=', { control: false, meta: true }), false)).toBeUndefined()
  })
  it('leaves other keys, Alt chords and key-ups alone', () => {
    expect(zoomOf(key('w'), false)).toBeUndefined()
    expect(zoomOf(key('=', { alt: true }), false)).toBeUndefined()
    expect(zoomOf(key('=', { type: 'keyUp' }), false)).toBeUndefined()
    expect(zoomOf(key('=', { control: false }), false)).toBeUndefined()
  })
  it('steps by halves within bounds and reads a saved level back', () => {
    expect(nextZoom(0, 'in')).toBe(0.5)
    expect(nextZoom(5, 'in')).toBe(5)
    expect(nextZoom(-5, 'out')).toBe(-5)
    expect(nextZoom(2, 'reset')).toBe(0)
    expect(zoomFrom('{"level":1.5}')).toBe(1.5)
    expect(zoomFrom('{"level":99}')).toBe(5)
    expect(zoomFrom('nope')).toBe(0)
    expect(zoomFrom(undefined)).toBe(0)
  })
})

const params = (o: Partial<MenuParams> = {}): MenuParams => ({ isEditable: false, selectionText: '', misspelledWord: '', dictionarySuggestions: [], linkURL: '', editFlags: { canCut: false, canCopy: false, canPaste: true, canSelectAll: true }, ...o })

describe('the text menu', () => {
  it('offers spelling first in a field with a misspelled word', () => {
    const m = menuFor(params({ isEditable: true, misspelledWord: 'teh', dictionarySuggestions: ['the', 'ten'] }))
    expect(m.slice(0, 3)).toEqual([{ kind: 'replace', word: 'the' }, { kind: 'replace', word: 'ten' }, { kind: 'learn', word: 'teh' }])
    expect(m.filter((i) => i.kind === 'role').map((i) => i.kind === 'role' && i.role)).toEqual(['cut', 'copy', 'paste', 'selectAll'])
  })
  it('gives Copy and Copy link on page text, and nothing on a bare row', () => {
    expect(menuFor(params({ selectionText: 'hi', linkURL: 'https://x.dev' })).map((i) => i.kind)).toEqual(['role', 'copyLink'])
    expect(menuFor(params({ linkURL: 'javascript:alert(1)' }))).toEqual([])
    expect(menuFor(params())).toEqual([])
  })
})
