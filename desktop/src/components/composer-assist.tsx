import { useCallback, useEffect, useId, useLayoutEffect, useRef, useState, type KeyboardEvent, type RefObject } from 'react'
import { getPromptSuggestion, listAgentCommands } from '@/actions'
import type { AgentCommand } from '@/gen/agentmesh/v1/chat_pb'
import { AgentState } from '@/gen/agentmesh/v1/types_pb'
import { complete, filterCommands, popupKey, slashQuery } from '@/composer-commands'
import type { HostRuntime } from '@/model'
import { CommandPopup } from './command-popup'

/** An agent's commands, kept for a while per agent: the host's probe of the CLI is slow the first time, so the composer asks before the user types a `/`. */
const KEEP_MS = 20_000
const cache = new Map<string, { at: number; commands: AgentCommand[]; loading?: Promise<void> }>()

function fetchCommands(host: HostRuntime, agentId: string, force = false): Promise<void> | undefined {
  const key = `${host.info.id}\n${agentId}`
  const hit = cache.get(key)
  if (hit?.loading) return hit.loading
  if (hit && !force && Date.now() - hit.at < KEEP_MS) return undefined
  const entry = { at: hit?.at ?? 0, commands: hit?.commands ?? [] } as { at: number; commands: AgentCommand[]; loading?: Promise<void> }
  entry.loading = Promise.resolve()
    .then(() => listAgentCommands(host.client, agentId))
    .then((res) => {
      cache.set(key, { at: Date.now(), commands: res.supported ? res.commands : [] })
    })
    .catch(() => {
      // Not kept for long: the next `/` asks again.
      cache.set(key, { at: Date.now() - KEEP_MS + 3_000, commands: hit?.commands ?? [] })
    })
  cache.set(key, entry)
  return entry.loading
}

/** The commands of an agent's CLI, asked for on first use and again when they are older than a short while. */
export function useAgentCommands(host: HostRuntime, agentId: string): { commands: AgentCommand[]; loading: boolean; refresh: () => void } {
  const [, bump] = useState(0)
  const get = useCallback(
    (force: boolean) => {
      const p = fetchCommands(host, agentId, force)
      if (p) void p.then(() => bump((n) => n + 1))
    },
    [host, agentId],
  )
  useEffect(() => get(false), [get])
  const hit = cache.get(`${host.info.id}\n${agentId}`)
  return { commands: hit?.commands ?? [], loading: !!hit?.loading, refresh: () => get(false) }
}

/** When to look for a suggestion after a turn ended: the CLI makes it a moment later, so a few reads, the first soon (a test shortens them). */
export const suggestionTiming = { readsMs: [800, 2_500, 6_000, 12_000] }

/** Forgets the commands kept per agent (tests). */
export const forgetCommands = () => cache.clear()

/** Claude Code's suggested next prompt: read from the agent's screen when it is at its prompt with nothing typed, and shown as the box's grey text. */
export function usePromptSuggestion(host: HostRuntime, agentId: string, state: AgentState | undefined, boxEmpty: boolean, enabled: boolean): { suggestion: string; dismiss: () => void } {
  const [found, setFound] = useState<{ agentId: string; text: string }>()
  const [dismissed, setDismissed] = useState('')
  const idle = state === AgentState.IDLE
  // A suggestion dropped with Esc stays dropped for the idle it was shown in: the next one, even the same words, shows.
  useEffect(() => {
    if (!idle) setDismissed('')
  }, [idle])
  useEffect(() => {
    if (!enabled || !idle || !boxEmpty) return
    let cancelled = false
    const timers: ReturnType<typeof setTimeout>[] = []
    const read = (i: number) => {
      timers[i] = setTimeout(async () => {
        try {
          const res = await getPromptSuggestion(host.client, agentId)
          if (cancelled) return
          if (res.text) return setFound({ agentId, text: res.text })
        } catch {
          return // no suggestion is not an error worth showing
        }
        if (i + 1 < suggestionTiming.readsMs.length) read(i + 1)
      }, suggestionTiming.readsMs[i])
    }
    read(0)
    return () => {
      cancelled = true
      timers.forEach(clearTimeout)
    }
  }, [host, agentId, enabled, idle, boxEmpty])
  // A suggestion belongs to the moment the agent was idle and the box empty: it goes when either stops being true.
  const text = enabled && idle && boxEmpty && found?.agentId === agentId && found.text !== dismissed ? found.text : ''
  return { suggestion: text, dismiss: () => setDismissed(found?.text ?? '') }
}

export interface ComposerAssist {
  /** Offer the key to the popup and the suggestion first: true when it took it. */
  onKeyDown: (e: KeyboardEvent<HTMLTextAreaElement>) => boolean
  /** Props for the field: where the caret is, focus, and what the popup is. */
  field: { onSelect: (e: React.SyntheticEvent<HTMLTextAreaElement>) => void; onFocus: () => void; onBlur: () => void; 'aria-controls'?: string; 'aria-activedescendant'?: string }
  popup: React.ReactNode
  /** The box's placeholder when a suggestion is showing; undefined otherwise. */
  suggestion: string
  commands: AgentCommand[]
}

/**
 * What the composer's box offers beyond typing: the `/` popup of the agent's commands and Claude's suggested prompt
 * as grey text (Tab takes it). Everything it shows is the host's (`ListAgentCommands`, `GetPromptSuggestion`).
 */
export function useComposerAssist({ host, agentId, kind, state, text, setText, field, disabled }: { host: HostRuntime; agentId: string; kind: string; state: AgentState | undefined; text: string; setText: (v: string) => void; field: RefObject<HTMLTextAreaElement | null>; disabled: boolean }): ComposerAssist {
  const { commands, loading } = useAgentCommands(host, agentId)
  const id = useId()
  const [caret, setCaret] = useState(0)
  const [focused, setFocused] = useState(false)
  const [active, setActive] = useState(0)
  const [closedFor, setClosedFor] = useState<string>()
  const pendingCaret = useRef<number>(undefined)
  const typed = slashQuery(text, caret)
  const items = typed === undefined ? [] : filterCommands(commands, typed)
  const open = !disabled && focused && typed !== undefined && closedFor !== typed && (items.length > 0 || (loading && commands.length === 0))
  // The highlight starts at the top again whenever what was typed changes.
  useEffect(() => setActive(0), [typed])
  // The caret follows the text (the field's own `select` events don't always come with an edit).
  useLayoutEffect(() => {
    if (pendingCaret.current === undefined && field.current) setCaret(field.current.selectionStart)
  }, [text, field])
  // The caret of a completion is set once the new text is in the field.
  useLayoutEffect(() => {
    const c = pendingCaret.current
    if (c === undefined) return
    pendingCaret.current = undefined
    field.current?.setSelectionRange(c, c)
    setCaret(c)
  })
  const { suggestion, dismiss } = usePromptSuggestion(host, agentId, state, text === '', kind === 'claude' && !disabled)
  const pick = useCallback(
    (c: AgentCommand) => {
      const next = complete(text, c)
      pendingCaret.current = next.caret
      setText(next.value)
      field.current?.focus()
    },
    [text, setText, field],
  )
  const onKeyDown = (e: KeyboardEvent<HTMLTextAreaElement>): boolean => {
    if (e.nativeEvent.isComposing) return false
    const mods = { shift: e.shiftKey, mod: e.ctrlKey || e.metaKey, alt: e.altKey }
    if (open && items.length > 0) {
      const top = items[Math.min(active, items.length - 1)]!
      const exact = [top.name, ...top.aliases].some((n) => n.toLowerCase() === typed!.toLowerCase())
      const k = popupKey(e.key, mods, active, items.length, exact)
      if (k) {
        e.preventDefault()
        if (k.kind === 'move') setActive(k.to)
        else if (k.kind === 'complete') pick(top)
        else setClosedFor(typed)
        return true
      }
    }
    if (suggestion && text === '' && !mods.mod && !mods.alt) {
      if (e.key === 'Tab' && !mods.shift) {
        e.preventDefault()
        setText(suggestion)
        return true
      }
      if (e.key === 'Escape') {
        e.preventDefault()
        dismiss()
        return true
      }
    }
    return false
  }
  return {
    onKeyDown,
    field: {
      onSelect: (e) => setCaret(e.currentTarget.selectionStart),
      onFocus: () => setFocused(true),
      onBlur: () => setFocused(false),
      ...(open && items.length > 0 ? { 'aria-controls': id, 'aria-activedescendant': `${id}-${Math.min(active, items.length - 1)}` } : {}),
    },
    popup: open ? <CommandPopup anchor={field.current} id={id} items={items} loading={loading} active={Math.min(active, Math.max(items.length - 1, 0))} onActive={setActive} onPick={pick} /> : null,
    suggestion,
    commands,
  }
}
