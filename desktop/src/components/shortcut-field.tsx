import { useState } from 'react'
import { Button } from '@/components/ui/button'
import { useHostState } from '@/context'
import { jsonOf } from '@/prefs'
import { chordFromEvent, displayChord, parseChord, sameChord } from '@/keys'
import type { HostRuntime } from '@/model'

/**
 * A shortcut setting: press the keys to set it. Escape cancels, Backspace unbinds. The host checks what is saved;
 * a chord another command already has is refused here, since two commands on one chord would leave the second
 * dead (the first, in the host's order, wins).
 */
export function ShortcutField({ host, id, value, busy, save }: { host: HostRuntime; id: string; value: string; busy: boolean; save: (v: unknown) => Promise<void> }) {
  const settings = useHostState(host, (s) => s.settings)
  const [recording, setRecording] = useState(false)
  const [clash, setClash] = useState('')
  const taken = (chord: string): string | undefined => {
    const want = parseChord(chord)
    if (!want) return undefined
    for (const [key, s] of Object.entries(settings)) {
      const other = key !== id && key.startsWith('app.keys.') ? jsonOf(s) : undefined
      const have = typeof other === 'string' ? parseChord(other) : undefined
      if (have && sameChord(have, want)) return (s.schema?.summary ?? key).replace(/^shortcut: /, '')
    }
    return undefined
  }
  function onKeyDown(e: React.KeyboardEvent) {
    if (!recording) return
    e.preventDefault()
    e.stopPropagation()
    if (e.key === 'Escape') return setRecording(false)
    if (e.key === 'Backspace' && !e.ctrlKey && !e.altKey && !e.metaKey && !e.shiftKey) {
      setRecording(false)
      setClash('')
      return void save('')
    }
    const chord = chordFromEvent(e.nativeEvent)
    if (!chord) return // only modifiers so far
    const other = taken(chord)
    if (other) return setClash(`${displayChord(chord)} already does "${other}": choose another, or unbind that one first.`)
    setClash('')
    setRecording(false)
    void save(chord)
  }
  return (
    <div className="flex flex-col gap-1">
      <div className="flex items-center gap-2">
        {value ? <kbd className="rounded border px-1.5 py-0.5 font-mono text-xs">{displayChord(value)}</kbd> : <span className="text-xs text-muted-foreground">Not bound</span>}
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
          {recording ? 'Press the keys… (Esc cancels, Backspace unbinds)' : 'Change'}
        </Button>
      </div>
      {clash && <p role="alert" className="text-xs text-destructive">{clash}</p>}
    </div>
  )
}
