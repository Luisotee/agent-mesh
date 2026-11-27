import { describe, expect, it } from 'vitest'
import { HUES, hueOf, hueStyle } from './hue'
import { themes } from './theme'

describe('identity hues', () => {
  it('gives a path the same hue every time, one of the eight', () => {
    expect(hueOf('/home/u/projects/agent-mesh')).toBe(hueOf('/home/u/projects/agent-mesh'))
    for (const p of ['/a', '/b', '/home/u/web', 'C:\\src\\api', '']) expect(hueOf(p)).toBeGreaterThanOrEqual(1), expect(hueOf(p)).toBeLessThanOrEqual(HUES)
    expect(hueOf('')).toBe(1)
  })

  it('spreads projects over the hues', () => {
    const seen = new Set(Array.from({ length: 64 }, (_, i) => hueOf(`/home/u/projects/repo-${i}`)))
    expect(seen.size).toBe(HUES)
  })

  it('sets --hue to the theme token', () => {
    expect(hueStyle('/x')).toEqual({ '--hue': `var(--hue-${hueOf('/x')})` })
  })

  it('has every hue in every theme', () => {
    for (const t of themes) for (let i = 1; i <= HUES; i++) expect(t.tokens[`hue-${i}`], `${t.name} hue-${i}`).toMatch(/^#[0-9a-f]{6}$/)
  })
})
