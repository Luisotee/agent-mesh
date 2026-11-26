import { readdirSync, readFileSync } from 'node:fs'
import path from 'node:path'
import { describe, expect, it } from 'vitest'
import { DEFAULT_THEME, applyTheme, resolveTerminal, themeByName, themeNames, themes } from './theme'

const dir = path.resolve(import.meta.dirname, '../../shared/themes')
const ANSI = ['black', 'red', 'green', 'yellow', 'blue', 'magenta', 'cyan', 'white']

describe('themes', () => {
  it('ships the six shared files, Dracula first', () => {
    expect(themeNames()).toEqual(['Dracula', 'Alucard', 'Catppuccin Latte', 'Catppuccin Mocha', 'Nord', 'Tokyo Night'])
    expect(readdirSync(dir).filter((f) => f.endsWith('.json'))).toHaveLength(themes.length)
    expect(DEFAULT_THEME).toBe('Dracula')
  })

  it('gives every theme the same tokens as Dracula, 16 ANSI colours, a font and a density', () => {
    const required = Object.keys(themeByName('Dracula').tokens).sort()
    for (const t of themes) {
      expect(Object.keys(t.tokens).sort(), t.name).toEqual(required)
      for (const c of [...ANSI, ...ANSI.map((n) => `bright${n[0]!.toUpperCase()}${n.slice(1)}`)]) expect(t.terminal[c], `${t.name} ${c}`).toMatch(/^#[0-9a-f]{6}$/)
      expect(t.font.mono, t.name).not.toBe('')
      expect(['compact', 'comfortable']).toContain(t.density)
    }
    // Light themes draw a light screen, dark ones a dark one.
    expect(themeByName('Alucard').dark).toBe(false)
    expect(themeByName('Catppuccin Latte').dark).toBe(false)
  })

  it('keeps Dracula as the desktop drew it', () => {
    const d = themeByName('Dracula')
    expect(d.tokens.background).toBe('#282a36')
    expect(d.terminal).toMatchObject({ background: '#21222c', red: '#ff5555', brightRed: '#ff6e6e', selectionBackground: '#44475a', brightBlack: '#6272a4' })
  })

  it('derives the terminal colours a file leaves out the way Android does', () => {
    const t = resolveTerminal({
      name: 'Mini',
      dark: true,
      tokens: { background: '#101010', foreground: '#eeeeee', primary: '#ff00ff', 'state-failed': '#ff0000', 'state-idle': '#666666', border: '#333333' },
    })
    expect(t).toMatchObject({ background: '#101010', foreground: '#eeeeee', cursor: '#eeeeee', selectionBackground: '#333333', red: '#ff0000', blue: '#ff00ff', brightBlack: '#666666', brightRed: '#ff3333' })
  })

  it('draws ANSI black and white on the light themes in colours of their own (not the background and the text)', () => {
    for (const t of themes.filter((t) => !t.dark)) {
      expect(t.terminal.black, `${t.name} black`).not.toBe(t.terminal.background)
      expect(t.terminal.white, `${t.name} white`).not.toBe(t.terminal.foreground)
    }
  })

  it('falls back to Dracula for a name it lacks', () => {
    expect(themeByName('Solarized').name).toBe('Dracula')
    expect(themeByName(undefined).name).toBe('Dracula')
  })

  it('writes the tokens, fonts and density as CSS variables', () => {
    const root = document.createElement('div')
    applyTheme(themeByName('Nord'), root)
    expect(root.style.getPropertyValue('--primary')).toBe('#88c0d0')
    expect(root.style.getPropertyValue('--font-code')).toContain('Cascadia Code')
    expect(root.style.getPropertyValue('--ui-text')).toBe('14px')
    expect(root.dataset).toMatchObject({ theme: 'Nord', density: 'comfortable' })
    applyTheme(themeByName('Dracula'), root)
    expect(root.style.getPropertyValue('--ui-text')).toBe('13px')
  })

  it('has the same token names in the Android files (one shared source)', () => {
    for (const f of readdirSync(dir)) {
      const j = JSON.parse(readFileSync(path.join(dir, f), 'utf8')) as { name: string; tokens: Record<string, string> }
      expect(Object.keys(j.tokens).sort(), f).toEqual(Object.keys(themeByName('Dracula').tokens).sort())
    }
  })
})
