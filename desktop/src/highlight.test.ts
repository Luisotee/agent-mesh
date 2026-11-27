import { describe, expect, it } from 'vitest'
import { highlighter, languageOf } from './highlight'

describe('highlighter', () => {
  it('knows the languages agents write by their short names, and none it has no grammar for', () => {
    expect(languageOf('ts')).toBe('typescript')
    expect(languageOf(' Bash ')).toBe('shellscript')
    expect(languageOf('go')).toBe('go')
    expect(languageOf('cobol')).toBeUndefined()
    expect(highlighter.supportsLanguage('cobol' as never)).toBe(false)
  })

  it('loads a grammar on first use and answers by the callback, in the theme\'s CSS variables; then at once', async () => {
    const opts = { code: 'const a = "x"', language: 'ts' as never, themes: highlighter.getThemes() }
    const later = await new Promise<NonNullable<ReturnType<typeof highlighter.highlight>>>((resolve) => {
      expect(highlighter.highlight(opts, resolve)).toBeNull()
    })
    const colours = later.tokens.flat().map((t) => t.color)
    expect(colours).toContain('var(--code-magenta)')
    expect(colours).toContain('var(--code-green)')
    expect(highlighter.highlight(opts)).not.toBeNull()
  })
})
