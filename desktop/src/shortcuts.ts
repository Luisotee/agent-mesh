import { useEffect } from 'react'
import { keyedCommands } from './commands'
import { useApp } from './context'
import { isRecorder, matches, parseChord } from './keys'
import { appKeys, type KeyedCommand } from './prefs'

/**
 * Runs the command a key chord is bound to. One listener on the window, in the capture phase, so a bound chord never
 * reaches the page or the terminal under it. The bindings are the first host's `app.keys.*` settings (the keyboard is
 * the app's, not one host's, as the tray is), read at each key, so a rebinding by another device or an agent applies at once.
 */
export function useShortcuts(): void {
  const app = useApp()
  useEffect(() => {
    const run = keyedCommands(app)
    const onKey = (e: KeyboardEvent) => {
      // A recorder is choosing a key; an input method is composing text.
      if (e.isComposing || isRecorder(e.target)) return
      const keys = appKeys(app.store.getState().hosts[0]?.store.getState().settings ?? {})
      for (const id of Object.keys(keys) as KeyedCommand[]) {
        const chord = parseChord(keys[id])
        if (chord && matches(e, chord)) {
          e.preventDefault()
          e.stopPropagation()
          run[id]()
          return
        }
      }
    }
    window.addEventListener('keydown', onKey, true)
    return () => window.removeEventListener('keydown', onKey, true)
  }, [app])
}
