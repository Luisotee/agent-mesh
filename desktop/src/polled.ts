import { useEffect, useRef, useState } from 'react'

/** A value read from a host, again every `everyMs` (and when `key` changes): `error` is the last failure, kept with the last value. */
export interface Polled<T> {
  value?: T
  error?: string
  loading: boolean
}

export function usePolled<T>(read: () => Promise<T>, key: string, everyMs: number, enabled = true): Polled<T> {
  const [state, setState] = useState<Polled<T>>({ loading: true })
  const reader = useRef(read)
  reader.current = read
  useEffect(() => {
    if (!enabled) return
    let alive = true
    setState({ loading: true })
    const load = () =>
      reader.current().then(
        (value) => alive && setState({ value, loading: false }),
        (e: unknown) => alive && setState((s) => ({ ...s, error: e instanceof Error ? e.message : String(e), loading: false })),
      )
    void load()
    const t = setInterval(() => void load(), everyMs)
    return () => {
      alive = false
      clearInterval(t)
    }
  }, [key, everyMs, enabled])
  return state
}
