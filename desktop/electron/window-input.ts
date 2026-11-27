/**
 * What the window does with keys and right-clicks itself, in main: zoom (the app has no menu on Windows and Linux, so
 * Electron's own zoom accelerators are gone) and the text menu (cut, copy, paste, spelling). Pure, so it is tested
 * without Electron; `main.ts` wires it to the window.
 */

export type Zoom = 'in' | 'out' | 'reset'

/**
 * The zoom a key is: Mod with = or + (with or without Shift, either key), with -, or with 0. In a focused terminal
 * (`inTerminal`) the Ctrl keys are the shell's (Ctrl+_ is readline's undo), as the renderer's shortcuts leave them;
 * a Mac's Cmd zoom still applies there, since a shell never gets Cmd.
 */
export function zoomOf(input: { type: string; key: string; code?: string; control: boolean; meta: boolean; alt: boolean }, mac: boolean, inTerminal = false): Zoom | undefined {
  if (input.type !== 'keyDown' || input.alt || !(mac ? input.meta : input.control)) return undefined
  if (inTerminal && !mac) return undefined
  if (input.key === '=' || input.key === '+' || input.code === 'Equal' || input.code === 'NumpadAdd') return 'in'
  if (input.key === '-' || input.key === '_' || input.code === 'Minus' || input.code === 'NumpadSubtract') return 'out'
  if (input.key === '0' || input.code === 'Digit0' || input.code === 'Numpad0') return 'reset'
  return undefined
}

/** The zoom level after a step, kept within what Chromium allows and stepped by halves (about 10% each). */
export function nextZoom(level: number, z: Zoom): number {
  if (z === 'reset') return 0
  return Math.min(5, Math.max(-5, Math.round((level + (z === 'in' ? 0.5 : -0.5)) * 2) / 2))
}

/** A zoom level read back from the saved file: anything else is the default. */
export function zoomFrom(text: string | undefined): number {
  try {
    const v = (JSON.parse(text ?? '') as { level?: unknown }).level
    return typeof v === 'number' && Number.isFinite(v) ? Math.min(5, Math.max(-5, v)) : 0
  } catch {
    return 0
  }
}

/** What a right-click gives: `context-menu`'s params, the parts this reads. */
export interface MenuParams {
  isEditable: boolean
  selectionText: string
  misspelledWord: string
  dictionarySuggestions: string[]
  linkURL: string
  editFlags: { canCut: boolean; canCopy: boolean; canPaste: boolean; canSelectAll: boolean }
}

export type MenuItem =
  | { kind: 'role'; role: 'cut' | 'copy' | 'paste' | 'selectAll'; label: string; enabled: boolean }
  | { kind: 'replace'; word: string }
  | { kind: 'learn'; word: string }
  | { kind: 'copyLink'; url: string }
  | { kind: 'separator' }

/**
 * The text menu: in a text field, spelling suggestions and Add to dictionary for a misspelled word, then Cut, Copy,
 * Paste and Select all; on page text, Copy (and Copy link on a link). Nothing elsewhere, so a right-click on a row
 * keeps the app's own menu.
 */
export function menuFor(p: MenuParams): MenuItem[] {
  const out: MenuItem[] = []
  if (p.isEditable && p.misspelledWord) {
    for (const w of p.dictionarySuggestions.slice(0, 5)) out.push({ kind: 'replace', word: w })
    out.push({ kind: 'learn', word: p.misspelledWord }, { kind: 'separator' })
  }
  if (p.isEditable) {
    out.push(
      { kind: 'role', role: 'cut', label: 'Cut', enabled: p.editFlags.canCut },
      { kind: 'role', role: 'copy', label: 'Copy', enabled: p.editFlags.canCopy },
      { kind: 'role', role: 'paste', label: 'Paste', enabled: p.editFlags.canPaste },
      { kind: 'separator' },
      { kind: 'role', role: 'selectAll', label: 'Select all', enabled: p.editFlags.canSelectAll },
    )
    return out
  }
  if (p.selectionText.trim()) out.push({ kind: 'role', role: 'copy', label: 'Copy', enabled: true })
  if (/^https?:\/\//.test(p.linkURL)) out.push({ kind: 'copyLink', url: p.linkURL })
  return out
}
