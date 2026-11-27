import { create } from '@bufbuild/protobuf'
import { timestampFromMs } from '@bufbuild/protobuf/wkt'
import { describe, expect, it } from 'vitest'
import { EventSchema, UserNoticeSchema, type Event } from './gen/agentmesh/v1/types_pb'
import { LIVE_WITHIN_MS, noticeFor } from './notify'
import { DEFAULT_PREFS } from './prefs'

const NOW = 1_800_000_000_000
const notice = (o: Partial<{ kind: string; forUser: boolean; tab: string; tag: string }> = {}, age = 1000): Event =>
  create(EventSchema, {
    seq: 1n,
    agentId: 'w1',
    time: timestampFromMs(NOW - age),
    payload: { case: 'userNotice', value: create(UserNoticeSchema, { kind: 'plan', title: 'app-w1 sent a plan', body: 'Plan do x', agentId: 'w1', tab: 'chat', forUser: true, tag: 'w1', ...o }) },
  })

describe('noticeFor', () => {
  it('shows what the host says is for the user, as the host worded it', () => {
    expect(noticeFor(notice(), NOW)).toEqual({ kind: 'plan', tag: 'w1', title: 'app-w1 sent a plan', body: 'Plan do x', agentId: 'w1', tab: 'chat' })
    expect(noticeFor(notice({ kind: 'input', tab: 'terminal' }), NOW)?.tab).toBe('terminal')
  })

  it('leaves what is only for Activity, and anything that is not a notice, alone', () => {
    expect(noticeFor(notice({ forUser: false }), NOW)).toBeUndefined()
    expect(noticeFor(create(EventSchema, { seq: 1n, time: timestampFromMs(NOW), payload: { case: 'runRenamed', value: { $typeName: 'agentmesh.v1.RunRenamed', title: 'x', previous: '' } } }), NOW)).toBeUndefined()
  })

  it('stays quiet for history replayed after a reconnect, by the host\'s clock', () => {
    expect(noticeFor(notice({}, LIVE_WITHIN_MS + 1000), NOW)).toBeUndefined()
    expect(noticeFor(notice({}, LIVE_WITHIN_MS + 1000), NOW, 30_000)).toBeUndefined()
    // The host's clock is 2 minutes ahead: an event it stamped 2 minutes "ago" by ours is just now by its own.
    expect(noticeFor(notice({}, -120_000 + 1000), NOW, 120_000)).toBeDefined()
  })

  it('notifies on the new kinds out of the box, and on a usage limit only when the user asks (the host\'s defaults)', () => {
    for (const k of ['finished', 'ready', 'plan', 'question', 'input', 'error']) expect(DEFAULT_PREFS.notifyEvents.has(k)).toBe(true)
    expect(DEFAULT_PREFS.notifyEvents.has('limited')).toBe(false)
    expect(DEFAULT_PREFS.notifyEvents.has('exited')).toBe(false)
  })
})
