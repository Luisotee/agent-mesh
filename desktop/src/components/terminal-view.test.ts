import { describe, expect, it } from 'vitest'
import { clampSize, sharedSize } from './terminal-view'

describe('sharedSize', () => {
  it('says nothing when the host terminal is as large as this window or larger, or unknown', () => {
    expect(sharedSize(undefined, { cols: 100, rows: 30 })).toBe('')
    expect(sharedSize({ cols: 100, rows: 30 }, { cols: 100, rows: 30 })).toBe('')
    expect(sharedSize({ cols: 120, rows: 40 }, { cols: 100, rows: 30 })).toBe('')
  })

  it('names the size another client made it when it is smaller in either direction', () => {
    expect(sharedSize({ cols: 80, rows: 24 }, { cols: 100, rows: 30 })).toBe('80×24, as another client has it')
    expect(sharedSize({ cols: 120, rows: 20 }, { cols: 100, rows: 30 })).toBe('120×20, as another client has it')
  })
})

describe('clampSize', () => {
  it('is the range the host accepts, so a window beyond it is not blamed on another client', () => {
    expect(clampSize(600, 400)).toEqual({ cols: 500, rows: 300 })
    expect(clampSize(10, 2)).toEqual({ cols: 20, rows: 5 })
    expect(clampSize(100, 30)).toEqual({ cols: 100, rows: 30 })
    expect(sharedSize({ cols: 500, rows: 300 }, clampSize(600, 400))).toBe('')
  })
})
