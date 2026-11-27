import type { CSSProperties } from 'react'

/** How many identity hues a theme has (`hue-1`…`hue-8` in each file of `shared/themes/`). */
export const HUES = 8

/**
 * A project's identity hue, 1 to 8: a hash (FNV-1a) of its repo path, so a project keeps its colour across restarts
 * and devices, and two projects rarely share one. An empty path gets the first.
 */
export function hueOf(repo: string): number {
  if (!repo) return 1
  let h = 0x811c9dc5
  for (let i = 0; i < repo.length; i++) {
    h ^= repo.charCodeAt(i)
    h = Math.imul(h, 0x01000193)
  }
  return ((h >>> 0) % HUES) + 1
}

/**
 * The style that gives an element and its children a project's hue: Tailwind's `identity` colour (`text-identity`,
 * `border-identity`, `ring-identity`, `bg-identity/10`) reads `--hue` (src/index.css).
 */
export const hueStyle = (repo: string): CSSProperties => ({ '--hue': `var(--hue-${hueOf(repo)})` }) as CSSProperties
