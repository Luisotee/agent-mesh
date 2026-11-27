import { useState } from 'react'
import { Button } from '@/components/ui/button'
import { useAppState, useHostState } from '@/context'
import { jsonOf } from '@/prefs'
import { chordFromEvent, displayChord, parseChord, sameChord } from '@/keys'
import type { HostRuntime } from '@/model'

/** The keys a chat key may be alone (the daemon's `bareChatKeys`). */
const BARE_KEYS = new Set(['Escape', 'ArrowUp', 'ArrowDown', 'PageUp', 'PageDown'])

/**
 * A shortcut setting: press the keys to set it. Escape cancels, Backspace unbinds. A `bare` one (a key of the chat's
 * composer, which may be Escape or an arrow alone) records those keys too: a click cancels, and Unbind unbinds. The host checks what is saved;
 * a chord another command already has is refused here, since two commands on one chord would leave the second
 * dead (the first, in the host's order, wins). The app follows the first host's keys only, so another host's rows are
 * shown, not recorded: a rebinding there would save and do nothing.
 */
export function ShortcutField({ host, id, value, busy, save, bare = false }: { host: HostRuntime; id: string; value: string; busy: boolean; save: (v: unknown) => Promise<void>; bare?: boolean }) {
  const settings = useHostState(host, (s) => s.settings)
  const first = useAppState((s) => s.hosts[0])
  const [recording, setRecording] = useState(false)
  const [clash, setClash] = useState('')
  const taken = (chord: string): string | undefined => {
    const want = parseChord(chord)
    if (!want) return undefined
    for (const [key, s] of Object.entries(settings)) {
      // A composer key and a window shortcut act in different places: each only clashes with its own kind.
      const other = key !== id && key.startsWith(bare ? 'app.chat_keys.' : 'app.keys.') ? jsonOf(s) : undefined
      const have = typeof other === 'string' ? parseChord(other) : undefined
      if (have && sameChord(have, want)) return (s.schema?.summary ?? key).replace(/^shortcut: /, '')
    }
    return undefined
  }
  function onKeyDown(e: React.KeyboardEvent) {
    if (!recording) return
    e.preventDefault()
    e.stopPropagation()
    if (e.key === 'Escape' && !bare) return setRecording(false)
    if (!bare && e.key === 'Backspace' && !e.ctrlKey && !e.altKey && !e.metaKey && !e.shiftKey) {
      setRecording(false)
      setClash('')
      return void save('')
    }
    const chord = chordFromEvent(e.nativeEvent)
    if (!chord) return // only modifiers so far
    // A composer key alone must be one that doesn't type or edit text (the host's list): say so here, not by a refusal.
    if (bare && !chord.includes('+') && !BARE_KEYS.has(chord) && !/^F\d+$/.test(chord)) return setClash(`${displayChord(chord)} alone would fire while typing: use ${[...BARE_KEYS].join(', ')}, or add Ctrl or Alt.`)
    const other = taken(chord)
    if (other) return setClash(`${displayChord(chord)} already does "${other}": choose another, or unbind that one first.`)
    setClash('')
    setRecording(false)
    void save(chord)
  }
  const shown = value ? <kbd className="rounded border px-1.5 py-0.5 font-mono text-xs">{displayChord(value)}</kbd> : <span className="text-xs text-muted-foreground">Not bound</span>
  if (first && first !== host) {
    return (
      <div className="flex flex-col gap-1">
        {shown}
        <p className="text-xs text-muted-foreground">The app uses the keys of {first.info.name}, its first host: change them there.</p>
      </div>
    )
  }
  return (
    <div className="flex flex-col gap-1">
      <div className="flex items-center gap-2">
        {shown}
        <Button
          size="sm"
          variant={recording ? 'secondary' : 'outline'}
          disabled={busy}
          data-shortcut-recorder=""
          aria-label={`Record a shortcut for ${id}`}
          onClick={() => {
            setClash('')
            setRecording((r) => !r)
          }}
          onKeyDown={onKeyDown}
          onBlur={() => setRecording(false)}
        >
          {recording ? (bare ? 'Press the key… (click to cancel)' : 'Press the keys… (Esc cancels, Backspace unbinds)') : 'Change'}
        </Button>
        {bare && value && !recording && (
          <Button size="sm" variant="ghost" disabled={busy} onClick={() => void save('')}>
            Unbind
          </Button>
        )}
      </div>
      {clash && <p role="alert" className="text-xs text-destructive">{clash}</p>}
    </div>
  )
}
