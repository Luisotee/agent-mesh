/**
 * A theme is one flat set of semantic tokens (docs/apps.md, "Settings, search and keyboard"). `applyTheme` writes
 * them as CSS variables on the page, and Tailwind's theme (src/index.css) reads those, so more themes are more
 * data, not more CSS. Dracula is the default.
 */
export interface Theme {
  name: string
  tokens: Record<string, string>
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
}

export function applyTheme(theme: Theme, root: HTMLElement = document.documentElement): void {
  for (const [k, v] of Object.entries(theme.tokens)) root.style.setProperty(`--${k}`, v)
  root.dataset.theme = theme.name
}
