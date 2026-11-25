/**
 * A theme is one flat set of semantic tokens (docs/apps.md, "Settings, search and keyboard"). `applyTheme` writes
 * them as CSS variables on the page, and Tailwind's theme (src/index.css) reads those, so more themes are more
 * data, not more CSS. Dracula is the default.
 */
export interface Theme {
  name: string
  tokens: Record<string, string>
  /** The terminal's colours: the screen, and the 16 ANSI colours. */
  terminal: Record<string, string>
}

export const dracula: Theme = {
  name: 'Dracula',
  tokens: {
    background: '#282a36',
    foreground: '#f8f8f2',
    surface: '#21222c',
    card: '#2d2f3d',
    popover: '#343746',
    muted: '#343746',
    'muted-foreground': '#9aa5ce',
    border: '#44475a',
    input: '#44475a',
    ring: '#bd93f9',
    primary: '#bd93f9',
    'primary-foreground': '#21222c',
    secondary: '#44475a',
    'secondary-foreground': '#f8f8f2',
    accent: '#3b3e52',
    'accent-foreground': '#f8f8f2',
    destructive: '#ff5555',
    'destructive-foreground': '#21222c',
    'state-working': '#8be9fd',
    'state-needs': '#ffb86c',
    'state-idle': '#6272a4',
    'state-done': '#50fa7b',
    'state-failed': '#ff5555',
    'state-unknown': '#f1fa8c',
  },
  terminal: {
    background: '#21222c',
    foreground: '#f8f8f2',
    cursor: '#f8f8f2',
    selectionBackground: '#44475a',
    black: '#21222c',
    red: '#ff5555',
    green: '#50fa7b',
    yellow: '#f1fa8c',
    blue: '#bd93f9',
    magenta: '#ff79c6',
    cyan: '#8be9fd',
    white: '#f8f8f2',
    brightBlack: '#6272a4',
    brightRed: '#ff6e6e',
    brightGreen: '#69ff94',
    brightYellow: '#ffffa5',
    brightBlue: '#d6acff',
    brightMagenta: '#ff92df',
    brightCyan: '#a4ffff',
    brightWhite: '#ffffff',
  },
}

/** The theme in use, for what can't read CSS variables (the terminal's canvas colours). */
export let activeTheme: Theme = dracula

export function applyTheme(theme: Theme, root: HTMLElement = document.documentElement): void {
  for (const [k, v] of Object.entries(theme.tokens)) root.style.setProperty(`--${k}`, v)
  root.dataset.theme = theme.name
  activeTheme = theme
}
