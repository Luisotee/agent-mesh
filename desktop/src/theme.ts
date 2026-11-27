/**
 * A theme is one JSON file of semantic tokens (docs/apps.md, "Settings, search and keyboard"), the same files the
 * Android app reads (`../shared/themes/*.json`). `applyTheme` writes the tokens as CSS variables on the page, and
 * Tailwind's theme (src/index.css) reads those, so more themes are more data, not more CSS. Dracula is the default.
 */
export interface Theme {
  name: string
  dark: boolean
  font: { sans: string; mono: string }
  density: 'compact' | 'comfortable'
  tokens: Record<string, string>
  /** The terminal's colours as xterm.js names them: the screen, the cursor, the selection and the 16 ANSI colours. */
  terminal: Record<string, string>
}

interface ThemeFile {
  name: string
  dark: boolean
  font?: { sans?: string; mono?: string }
  density?: string
  tokens: Record<string, string>
  /** The file's own spelling: `selection`, and any of the 16 ANSI colours (a missing one is derived). */
  terminal?: Record<string, string>
}

export const DEFAULT_THEME = 'Dracula'

const files = import.meta.glob<ThemeFile>('../../shared/themes/*.json', { eager: true, import: 'default' })

type Rgb = [number, number, number]
const toRgb = (hex: string): Rgb => {
  const h = hex.replace('#', '')
  return [0, 2, 4].map((i) => parseInt(h.slice(i, i + 2), 16)) as Rgb
}
const toHex = (rgb: Rgb) => '#' + rgb.map((v) => Math.round(v).toString(16).padStart(2, '0')).join('')
/** The same lightening Android's resolver applies to the bright ANSI colours a file leaves out. */
const lighten = (hex: string, by: number) => toHex(toRgb(hex).map((v) => v + (255 - v) * by) as Rgb)

/**
 * The terminal's colours with every one a file leaves out derived from its tokens, as Android's `Themes.resolve`
 * does: the ANSI colours from the state colours, the bright ones lightened by a fifth (bright black is the idle state).
 */
export function resolveTerminal(f: ThemeFile): Record<string, string> {
  const t = f.tokens
  const term = f.terminal ?? {}
  const tc = (key: string, fallback: string) => term[key] ?? fallback
  const bg = tc('background', t.surface ?? t.background!)
  const fg = tc('foreground', t.foreground!)
  const normal: [string, string][] = [
    ['black', bg],
    ['red', tc('red', t['state-failed'] ?? t.primary!)],
    ['green', tc('green', t['state-done'] ?? t.primary!)],
    ['yellow', tc('yellow', t['state-unknown'] ?? t.foreground!)],
    ['blue', tc('blue', t.primary!)],
    ['magenta', tc('magenta', t.primary!)],
    ['cyan', tc('cyan', t['state-working'] ?? t.primary!)],
    ['white', fg],
  ]
  const out: Record<string, string> = {
    background: bg,
    foreground: fg,
    cursor: tc('cursor', fg),
    selectionBackground: tc('selection', t.border ?? t.muted ?? bg),
  }
  for (const [name, fallback] of normal) out[name] = tc(name, fallback)
  const bright = (name: string) => name[0]!.toUpperCase() + name.slice(1)
  out.brightBlack = tc('brightBlack', t['state-idle'] ?? t['muted-foreground'] ?? fg)
  for (const [name] of normal.slice(1)) out[`bright${bright(name)}`] = tc(`bright${bright(name)}`, lighten(out[name]!, 0.2))
  return out
}

export function resolveTheme(f: ThemeFile): Theme {
  return {
    name: f.name,
    dark: f.dark,
    font: { sans: f.font?.sans ?? 'Geist', mono: f.font?.mono ?? 'JetBrains Mono' },
    density: f.density === 'comfortable' ? 'comfortable' : 'compact',
    tokens: f.tokens,
    terminal: resolveTerminal(f),
  }
}

export const themes: Theme[] = Object.values(files)
  .map(resolveTheme)
  .sort((a, b) => (a.name === DEFAULT_THEME ? -1 : b.name === DEFAULT_THEME ? 1 : a.name.localeCompare(b.name)))

export const themeNames = (): string[] => themes.map((t) => t.name)

/** The theme called `name`, or Dracula for an unknown or empty name (a host may name a theme this build lacks). */
export function themeByName(name: string | undefined): Theme {
  return themes.find((t) => t.name === name) ?? themes.find((t) => t.name === DEFAULT_THEME) ?? themes[0]!
}

/** The theme in use, for what can't read CSS variables (the terminal's canvas colours and font). */
export let activeTheme: Theme = themeByName(DEFAULT_THEME)

const listeners = new Set<(t: Theme) => void>()
/** Called when another theme is applied (an open terminal repaints). */
export function onThemeChange(fn: (t: Theme) => void): () => void {
  listeners.add(fn)
  return () => void listeners.delete(fn)
}

/**
 * A font stack for a theme's family. Geist and JetBrains Mono are bundled (`@fontsource-variable/*`, imported in
 * src/index.css) under the family name plus " Variable"; that name goes first, then the plain one an installed copy has.
 */
const stack = (family: string, rest: string) => `'${family} Variable', '${family}', ${rest}`
export const monoStack = (t: Theme) => stack(t.font.mono, "ui-monospace, 'Cascadia Code', Menlo, monospace")

/**
 * What density sets: Tailwind's spacing step and the body text size. The type scale around it is 12/13/14/15/18/22px:
 * meta 12 (nothing smaller), sidebar rows 13, body 14 (15 comfortable), chat prose 15.
 */
const DENSITY = { compact: { spacing: '0.25rem', text: '14px' }, comfortable: { spacing: '0.28rem', text: '15px' } }

const lum = (hex: string) => {
  const [r, g, b] = [1, 3, 5].map((i) => parseInt(hex.slice(i, i + 2), 16) / 255).map((c) => (c <= 0.03928 ? c / 12.92 : ((c + 0.055) / 1.055) ** 2.4))
  return 0.2126 * r! + 0.7152 * g! + 0.0722 * b!
}
/** WCAG's contrast ratio of two #rrggbb colours. */
export function contrast(a: string, b: string): number {
  const [hi, lo] = [lum(a), lum(b)].sort((x, y) => y - x)
  return (hi! + 0.05) / (lo! + 0.05)
}
const mix = (a: string, b: string, t: number) =>
  '#' + [1, 3, 5].map((i) => Math.round(parseInt(a.slice(i, i + 2), 16) * (1 - t) + parseInt(b.slice(i, i + 2), 16) * t).toString(16).padStart(2, '0')).join('')

/**
 * The colours of highlighted code in the chat (`code-theme.ts`): the terminal's six, so a reply's code matches its
 * terminal, each moved toward the text colour just as far as it takes to read at 4.5:1 on a code block's surface
 * (a light theme's terminal yellow is too pale for text on its page).
 */
export function codeColours(theme: Theme): Record<string, string> {
  const out: Record<string, string> = {}
  const bg = theme.tokens.surface!
  const fg = theme.tokens.foreground!
  for (const k of ['red', 'green', 'yellow', 'blue', 'magenta', 'cyan']) {
    const base = theme.terminal[k]
    if (!base || !/^#[0-9a-f]{6}$/i.test(base)) continue
    let t = 0
    while (t < 1 && contrast(mix(base, fg, t), bg) < 4.5) t += 0.05
    out[k] = mix(base, fg, Math.min(1, t))
  }
  return out
}

export function applyTheme(theme: Theme, root: HTMLElement = document.documentElement): void {
  for (const [k, v] of Object.entries(theme.tokens)) root.style.setProperty(`--${k}`, v)
  for (const [k, v] of Object.entries(codeColours(theme))) root.style.setProperty(`--code-${k}`, v)
  root.style.setProperty('--font-ui', stack(theme.font.sans, 'ui-sans-serif, system-ui, sans-serif'))
  root.style.setProperty('--font-code', monoStack(theme))
  root.style.setProperty('--spacing', DENSITY[theme.density].spacing)
  root.style.setProperty('--ui-text', DENSITY[theme.density].text)
  root.style.colorScheme = theme.dark ? 'dark' : 'light'
  root.dataset.theme = theme.name
  root.dataset.density = theme.density
  if (theme === activeTheme) return
  activeTheme = theme
  for (const fn of listeners) fn(theme)
}
