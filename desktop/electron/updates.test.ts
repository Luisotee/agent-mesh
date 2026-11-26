import { afterEach, describe, expect, it, vi } from 'vitest'
import { startUpdates, type Updater } from './updates'

afterEach(() => vi.useRealTimers())

const fake = (fail?: Error) => {
  const updater: Updater = { checkForUpdatesAndNotify: vi.fn(async () => { if (fail) throw fail }) }
  return { updater, load: vi.fn(async () => updater) }
}

describe('startUpdates', () => {
  it('does nothing in a dev run, or one with its own user-data dir (the tests)', () => {
    for (const [packaged, isolated] of [[false, false], [true, true], [false, true]] as const) {
      const f = fake()
      startUpdates({ packaged, isolated, load: f.load, log: vi.fn() })()
      expect(f.load).not.toHaveBeenCalled()
    }
  })

  it('checks at start and then every interval, until stopped', async () => {
    vi.useFakeTimers()
    const f = fake()
    const stop = startUpdates({ packaged: true, isolated: false, load: f.load, log: vi.fn(), everyMs: 1000 })
    await vi.advanceTimersByTimeAsync(0)
    expect(f.updater.checkForUpdatesAndNotify).toHaveBeenCalledTimes(1)
    await vi.advanceTimersByTimeAsync(2500)
    expect(f.updater.checkForUpdatesAndNotify).toHaveBeenCalledTimes(3)
    stop()
    await vi.advanceTimersByTimeAsync(5000)
    expect(f.updater.checkForUpdatesAndNotify).toHaveBeenCalledTimes(3)
  })

  it('does not start a check while the last one is still running', async () => {
    vi.useFakeTimers()
    let release!: () => void
    const updater: Updater = { checkForUpdatesAndNotify: vi.fn(() => new Promise<void>((r) => { release = r })) }
    startUpdates({ packaged: true, isolated: false, load: async () => updater, log: vi.fn(), everyMs: 1000 })
    await vi.advanceTimersByTimeAsync(3500)
    expect(updater.checkForUpdatesAndNotify).toHaveBeenCalledTimes(1)
    release()
    await vi.advanceTimersByTimeAsync(1000)
    expect(updater.checkForUpdatesAndNotify).toHaveBeenCalledTimes(2)
  })

  it('logs a failed check and keeps going', async () => {
    vi.useFakeTimers()
    const f = fake(new Error('offline'))
    const log = vi.fn()
    startUpdates({ packaged: true, isolated: false, load: f.load, log, everyMs: 1000 })
    await vi.advanceTimersByTimeAsync(1500)
    expect(log).toHaveBeenCalledTimes(2)
    expect(log).toHaveBeenCalledWith('update check failed: offline')
  })
})
