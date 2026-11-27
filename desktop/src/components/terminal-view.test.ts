import { describe, expect, it } from 'vitest'
import { sharedSize } from './terminal-view'

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
