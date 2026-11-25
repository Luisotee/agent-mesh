import { create } from '@bufbuild/protobuf'
import { timestampFromMs } from '@bufbuild/protobuf/wkt'
import { describe, expect, it, vi } from 'vitest'
import { messageLoader } from './messages'
import { MessageSchema, RunStatus } from './gen/agentmesh/v1/types_pb'
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
  it('pages through an open run and skips a closed one', async () => {
    const store = createHostStore()
    const { c, calls } = client({
      'r1:': { messages: [m('a', 'r1', 1), m('b', 'r1', 2)], more: true },
      'r1:b': { messages: [m('c', 'r1', 3)], more: false },
    })
    await messageLoader(c as never, store, () => {}).load([{ id: 'r1', status: RunStatus.OPEN }, { id: 'r2', status: RunStatus.CLOSED }])
    expect(calls).toEqual([{ runId: 'r1', afterId: '' }, { runId: 'r1', afterId: 'b' }])
    expect(Object.keys(store.getState().messages).sort()).toEqual(['a', 'b', 'c'])
  })

  it('on a later snapshot resumes after the newest message of a run that loaded completely', async () => {
    const store = createHostStore()
    const { c, calls } = client({
      'r1:': { messages: [m('a', 'r1', 1)], more: false },
      'r1:b': { messages: [m('c', 'r1', 3)], more: false },
    })
    const loader = messageLoader(c as never, store, () => {})
    await loader.load([{ id: 'r1', status: RunStatus.OPEN }])
    // An event brought a newer message while we were connected.
    store.setState((s) => ({ messages: { ...s.messages, b: m('b', 'r1', 2) } }))
    await loader.load([{ id: 'r1', status: RunStatus.OPEN }])
    expect(calls.at(-1)).toEqual({ runId: 'r1', afterId: 'b' })
    expect(Object.keys(store.getState().messages).sort()).toEqual(['a', 'b', 'c'])
  })

  it('retries a failed load with a growing wait', async () => {
    vi.useFakeTimers()
    try {
      const store = createHostStore()
      const pages: Record<string, { messages: ReturnType<typeof m>[]; more: boolean }> = {}
      const { c, calls } = client(pages)
      await messageLoader(c as never, store, () => {}).load([{ id: 'r1', status: RunStatus.OPEN }])
      expect(calls).toHaveLength(1)
      pages['r1:'] = { messages: [m('a')], more: false }
      await vi.advanceTimersByTimeAsync(1000)
      expect(calls).toHaveLength(2)
      expect(Object.keys(store.getState().messages)).toEqual(['a'])
      await vi.advanceTimersByTimeAsync(60_000)
      expect(calls).toHaveLength(2) // loaded: no more tries
    } finally {
      vi.useRealTimers()
    }
  })

  it('a failed load is logged and tried from the start next time', async () => {
    const store = createHostStore()
    const log = vi.fn()
    const pages: Record<string, { messages: ReturnType<typeof m>[]; more: boolean }> = {}
    const { c, calls } = client(pages)
    const loader = messageLoader(c as never, store, log)
    await loader.load([{ id: 'r1', status: RunStatus.OPEN }])
    expect(log).toHaveBeenCalledWith(expect.stringContaining("couldn't load the messages of run r1"))
    pages['r1:'] = { messages: [m('a')], more: false }
    await loader.load([{ id: 'r1', status: RunStatus.OPEN }])
    expect(calls.map((x) => x.afterId)).toEqual(['', ''])
    expect(Object.keys(store.getState().messages)).toEqual(['a'])
  })
})
