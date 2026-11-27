import { useEffect, useRef, useState } from 'react'

/**
 * A value read from a host, again every `everyMs` (and when `key` changes, which starts over): `error` is the last failure, kept
 * with the last value. A change of `refreshKey` reads again at once and keeps what is shown meanwhile (an agent's state changing
 * is a reason to read, not to blank the panel).
 */
export interface Polled<T> {
  value?: T
  error?: string
  loading: boolean
}

export function usePolled<T>(read: () => Promise<T>, key: string, everyMs: number, enabled = true, refreshKey = ''): Polled<T> {
  const [state, setState] = useState<Polled<T>>({ loading: true })
  const reader = useRef(read)
  reader.current = read
  const again = useRef<() => void>(undefined)
  const first = useRef(true)
  useEffect(() => {
    if (!enabled) return
    let alive = true
    setState({ loading: true })
    const load = () =>
      reader.current().then(
        (value) => alive && setState({ value, loading: false }),
        (e: unknown) => alive && setState((s) => ({ ...s, error: e instanceof Error ? e.message : String(e), loading: false })),
      )
    again.current = () => void load()
    void load()
    const t = setInterval(() => void load(), everyMs)
    return () => {
      again.current = undefined
      alive = false
      clearInterval(t)
    }
  }, [key, everyMs, enabled])
  useEffect(() => {
    if (first.current) {
      first.current = false
      return
    }
    again.current?.()
  }, [refreshKey])
  return state
}
