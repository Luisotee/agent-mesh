import { useEffect } from 'react'
import { keyedCommands } from './commands'
import { useApp } from './context'
import { isRecorder, matches, parseChord, type Chord } from './keys'
import { appKeys, type KeyedCommand } from './prefs'

/** Whether the focus is in a terminal (xterm's input), where the shell owns Ctrl+letter keys. */
const inTerminal = (t: EventTarget | null) => t instanceof Element && !!t.closest('.xterm')

/** Whether the focus is where keys type text: a field, or a terminal. */
const typing = (t: EventTarget | null) =>
  inTerminal(t) || (t instanceof HTMLElement && (t.tagName === 'INPUT' || t.tagName === 'TEXTAREA' || t.tagName === 'SELECT' || t.isContentEditable))

const letter = (c: Chord) => /^[A-Z]$/.test(c.key)

/**
 * The command a key press runs, if any. In a focused terminal a Mod+letter chord (Ctrl+W, Ctrl+C, Ctrl+K) goes to the
 * shell, and the same chord with Shift added runs the command (Ctrl+Shift+W closes the tab), as Windows Terminal does,
 * ahead of a command bound to that Shift form (Ctrl+Shift+T there is Ctrl+T's new lead, not Ctrl+Shift+T's reopen).
 * Only Ctrl is the shell's: a macOS terminal never sends Cmd keys to it, so a Cmd chord always runs its command.
 * A chord without Ctrl or Meta (F2, Alt+ArrowDown) does nothing while the user types in a field or a terminal.
 */
export function commandFor(e: KeyboardEvent, keys: Record<KeyedCommand, string>): KeyedCommand | undefined {
  const term = inTerminal(e.target)
  const ids = Object.keys(keys) as KeyedCommand[]
  // In a terminal the Shift form stands for the Ctrl+letter command first, so each of those keeps a key there even
  // when another command is bound to the Shift form itself: Ctrl+Shift+T is a new lead (Ctrl+T), not reopen the tab.
  if (term && e.shiftKey) {
    for (const id of ids) {
      const chord = parseChord(keys[id])
      if (chord && letter(chord) && chord.ctrl && !chord.shift && matches(e, { ...chord, shift: true })) return id
    }
  }
  for (const id of ids) {
    const chord = parseChord(keys[id])
    if (!chord || !matches(e, chord)) continue
    if (term && letter(chord) && chord.ctrl && !chord.shift) continue
    if (!chord.ctrl && !chord.meta && typing(e.target)) continue
    return id
  }
  return undefined
}

/**
 * Runs the command a key chord is bound to. One listener on the window, in the capture phase, so a bound chord never
 * reaches the page or the terminal under it (but see `commandFor` for the terminal's own keys). The bindings are the
 * first host's `app.keys.*` settings (the keyboard is the app's, not one host's, as the tray is), read at each key, so
 * a rebinding by another device or an agent applies at once. `/` outside a text field opens the search.
 */
export function useShortcuts(): void {
  const app = useApp()
  useEffect(() => {
    const run = keyedCommands(app)
    const onKey = (e: KeyboardEvent) => {
      // A recorder is choosing a key; an input method is composing text.
      if (e.isComposing || isRecorder(e.target)) return
      const keys = appKeys(app.store.getState().hosts[0]?.store.getState().settings ?? {})
      const id = commandFor(e, keys)
      if (id) {
        e.preventDefault()
        e.stopPropagation()
        run[id]()
        return
      }
      if (e.key === '/' && !e.ctrlKey && !e.metaKey && !e.altKey && !typing(e.target) && !app.store.getState().paletteOpen) {
        e.preventDefault()
        app.openPalette(true)
      }
    }
    // Main zooms on Ctrl+-/=/0 before the page sees the key: it is told when a terminal has the focus, to leave them.
    let term = false
    const tell = (t: EventTarget | null) => {
      const now = inTerminal(t)
      if (now !== term) app.api.terminalFocus?.((term = now))
    }
    const onIn = (e: FocusEvent) => tell(e.target)
    const onOut = (e: FocusEvent) => tell(e.relatedTarget)
    window.addEventListener('keydown', onKey, true)
    window.addEventListener('focusin', onIn)
    window.addEventListener('focusout', onOut)
    return () => {
      window.removeEventListener('keydown', onKey, true)
      window.removeEventListener('focusin', onIn)
      window.removeEventListener('focusout', onOut)
    }
  }, [app])
}
