import { create } from '@bufbuild/protobuf'
import { timestampFromMs } from '@bufbuild/protobuf/wkt'
import { describe, expect, it, vi } from 'vitest'
import { messageLoader } from './messages'
import { MessageSchema } from './gen/agentmesh/v1/types_pb'
import { createHostStore } from './store'

const m = (id: string, run = 'r1', ms = 1000) => create(MessageSchema, { id, runId: run, body: id, createdAt: timestampFromMs(ms) })

function client(pages: Record<string, { messages: ReturnType<typeof m>[]; more: boolean }>) {
  const calls: Array<{ runId: string; afterId: string }> = []
  return {
    calls,
    c: {
      listMessages: vi.fn(async (req: { runId: string; afterId: string }) => {
        calls.push({ runId: req.runId, afterId: req.afterId })
        const p = pages[`${req.runId}:${req.afterId}`]
        if (!p) throw new Error(`no page ${req.runId}:${req.afterId}`)
        return p
      }),
    },
  }
}

describe('messageLoader', () => {
  it('pages through a run when asked, and touches no other run', async () => {
    const store = createHostStore()
    const { c, calls } = client({
      'r1:': { messages: [m('a', 'r1', 1), m('b', 'r1', 2)], more: true },
      'r1:b': { messages: [m('c', 'r1', 3)], more: false },
    })
    const loader = messageLoader(c as never, store, () => {})
    // A snapshot alone loads nothing.
    await loader.resync([{ id: 'r1' }, { id: 'r2' }])
    expect(calls).toEqual([])
    const done = loader.ensure('r1')
    expect(store.getState().messageLoad).toEqual({ r1: 'loading' })
    await done
    expect(calls).toEqual([{ runId: 'r1', afterId: '' }, { runId: 'r1', afterId: 'b' }])
    expect(Object.keys(store.getState().messages).sort()).toEqual(['a', 'b', 'c'])
    expect(store.getState().messageLoad).toEqual({ r1: 'loaded' })
  })

  it('loads a run once: asking again, or while it loads, shares the one load', async () => {
    const store = createHostStore()
    const { c, calls } = client({ 'r1:': { messages: [m('a')], more: false } })
    const loader = messageLoader(c as never, store, () => {})
    await Promise.all([loader.ensure('r1'), loader.ensure('r1')])
    await loader.ensure('r1')
    expect(calls).toHaveLength(1)
  })

  it('on a later snapshot tops up a run that was asked for, after its newest message, and leaves the others', async () => {
    const store = createHostStore()
    const { c, calls } = client({
      'r1:': { messages: [m('a', 'r1', 1)], more: false },
      'r1:b': { messages: [m('c', 'r1', 3)], more: false },
    })
    const loader = messageLoader(c as never, store, () => {})
    await loader.ensure('r1')
    // An event brought a newer message while we were connected.
    store.setState((s) => ({ messages: { ...s.messages, b: m('b', 'r1', 2) } }))
    await loader.resync([{ id: 'r1' }, { id: 'r2' }])
    expect(calls.at(-1)).toEqual({ runId: 'r1', afterId: 'b' })
    expect(calls.some((x) => x.runId === 'r2')).toBe(false)
    expect(Object.keys(store.getState().messages).sort()).toEqual(['a', 'b', 'c'])
    expect(store.getState().messageLoad).toEqual({ r1: 'loaded' })
  })

  it('forgets a run the host no longer lists, so asking for it again starts over', async () => {
    const store = createHostStore()
    const { c, calls } = client({ 'r1:': { messages: [m('a')], more: false } })
    const loader = messageLoader(c as never, store, () => {})
    await loader.ensure('r1')
    await loader.resync([{ id: 'r2' }])
    await loader.ensure('r1')
    expect(calls.map((x) => x.afterId)).toEqual(['', ''])
  })

  it('retries a failed load with a growing wait, showing it failed in between', async () => {
    vi.useFakeTimers()
    try {
      const store = createHostStore()
      const pages: Record<string, { messages: ReturnType<typeof m>[]; more: boolean }> = {}
      const { c, calls } = client(pages)
      await messageLoader(c as never, store, () => {}).ensure('r1')
      expect(calls).toHaveLength(1)
      expect(store.getState().messageLoad).toEqual({ r1: 'failed' })
      pages['r1:'] = { messages: [m('a')], more: false }
      await vi.advanceTimersByTimeAsync(1000)
      expect(calls).toHaveLength(2)
      expect(Object.keys(store.getState().messages)).toEqual(['a'])
      expect(store.getState().messageLoad).toEqual({ r1: 'loaded' })
      await vi.advanceTimersByTimeAsync(60_000)
      expect(calls).toHaveLength(2) // loaded: no more tries
    } finally {
      vi.useRealTimers()
    }
  })

  it('a failed load is logged, and a manual retry starts at once, from the start', async () => {
    const store = createHostStore()
    const log = vi.fn()
    const pages: Record<string, { messages: ReturnType<typeof m>[]; more: boolean }> = {}
    const { c, calls } = client(pages)
    const loader = messageLoader(c as never, store, log)
    await loader.ensure('r1')
    expect(log).toHaveBeenCalledWith(expect.stringContaining("couldn't load the messages of run r1"))
    pages['r1:'] = { messages: [m('a')], more: false }
    await loader.retry('r1')
    expect(calls.map((x) => x.afterId)).toEqual(['', ''])
    expect(Object.keys(store.getState().messages)).toEqual(['a'])
    loader.stop()
  })

  it('asking again while a retry waits loads at once and leaves no second try behind', async () => {
    vi.useFakeTimers()
    try {
      const store = createHostStore()
      const pages: Record<string, { messages: ReturnType<typeof m>[]; more: boolean }> = {}
      const { c, calls } = client(pages)
      const loader = messageLoader(c as never, store, () => {})
      await loader.ensure('r1') // fails, a retry is queued
      pages['r1:'] = { messages: [m('a')], more: false }
      await loader.ensure('r1')
      expect(calls).toHaveLength(2)
      await vi.advanceTimersByTimeAsync(60_000)
      expect(calls).toHaveLength(2)
    } finally {
      vi.useRealTimers()
    }
  })

  it('a snapshot while a retry waits loads at once and leaves one retry chain, not two', async () => {
    vi.useFakeTimers()
    try {
      const store = createHostStore()
      const pages: Record<string, { messages: ReturnType<typeof m>[]; more: boolean }> = {}
      const { c, calls } = client(pages)
      const loader = messageLoader(c as never, store, () => {})
      await loader.ensure('r1') // fails: a retry is queued
      await loader.resync([{ id: 'r1' }]) // fails again: the queued retry is replaced
      expect(calls).toHaveLength(2)
      await vi.advanceTimersByTimeAsync(1000) // the first retry's time: nothing is left of it
      expect(calls).toHaveLength(2)
      pages['r1:'] = { messages: [m('a')], more: false }
      await vi.advanceTimersByTimeAsync(1000) // the replacing retry (its wait doubled)
      expect(calls).toHaveLength(3)
      expect(store.getState().messageLoad).toEqual({ r1: 'loaded' })
      await vi.advanceTimersByTimeAsync(60_000)
      expect(calls).toHaveLength(3)
    } finally {
      vi.useRealTimers()
    }
  })

  it('a load that finishes after its run was dropped by a snapshot records nothing, and asking again starts over', async () => {
    const store = createHostStore()
    let release!: () => void
    const page = { messages: [m('a')], more: false }
    const listMessages = vi.fn().mockImplementationOnce(() => new Promise((r) => (release = () => r(page)))).mockResolvedValue(page)
    const loader = messageLoader({ listMessages } as never, store, () => {})
    const loading = loader.ensure('r1')
    await loader.resync([{ id: 'r2' }]) // r1 is gone from the host
    release()
    await loading
    expect(store.getState().messageLoad.r1).not.toBe('loaded')
    await loader.ensure('r1')
    expect(listMessages).toHaveBeenCalledTimes(2) // not trusted as complete
    expect(store.getState().messageLoad.r1).toBe('loaded')
  })

  it('stops retrying once stopped', async () => {
    vi.useFakeTimers()
    try {
      const store = createHostStore()
      const { c, calls } = client({})
      const loader = messageLoader(c as never, store, () => {})
      await loader.ensure('r1')
      loader.stop()
      await vi.advanceTimersByTimeAsync(60_000)
      expect(calls).toHaveLength(1)
    } finally {
      vi.useRealTimers()
    }
  })
})
