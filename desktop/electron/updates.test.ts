import { afterEach, describe, expect, it, vi } from 'vitest'
import { canSelfUpdate, startUpdates, type Updater } from './updates'

afterEach(() => vi.useRealTimers())

const win = { platform: 'win32', env: {}, nsis: true }
const fake = (fail?: Error) => {
  const updater: Updater = { checkForUpdates: vi.fn(async () => { if (fail) throw fail; return null }) }
  return { updater, load: vi.fn(async () => updater) }
}

describe('canSelfUpdate', () => {
  it('is for the NSIS installer and the AppImage only: not a deb (root install), not a dmg (no zip artifact)', () => {
    expect(canSelfUpdate('win32', {}, true)).toBe(true)
    // The Windows zip carries the installer's update config, but no uninstaller: it never updates into a second copy.
    expect(canSelfUpdate('win32', {}, false)).toBe(false)
    expect(canSelfUpdate('linux', { APPIMAGE: '/home/u/agent-mesh.AppImage' }, false)).toBe(true)
    expect(canSelfUpdate('linux', {}, false)).toBe(false)
    expect(canSelfUpdate('darwin', {}, false)).toBe(false)
    expect(canSelfUpdate('darwin', { APPIMAGE: 'x' }, false)).toBe(false)
  })
})

describe('startUpdates', () => {
  it('does nothing in a dev run, or one with its own user-data dir (the tests)', () => {
    for (const [packaged, isolated] of [[false, false], [true, true], [false, true]] as const) {
      const f = fake()
      startUpdates({ packaged, isolated, ...win, load: f.load, log: vi.fn() })()
      expect(f.load).not.toHaveBeenCalled()
    }
  })

  it('does nothing in an install that cannot update itself (a deb, a dmg, the Windows zip)', () => {
    for (const p of [{ platform: 'linux', env: {}, nsis: false }, { platform: 'darwin', env: {}, nsis: false }, { platform: 'win32', env: {}, nsis: false }]) {
      const f = fake()
      startUpdates({ packaged: true, isolated: false, ...p, load: f.load, log: vi.fn() })()
      expect(f.load).not.toHaveBeenCalled()
    }
  })

  it('checks at start and then every interval, until stopped', async () => {
    vi.useFakeTimers()
    const f = fake()
    const stop = startUpdates({ packaged: true, isolated: false, ...win, load: f.load, log: vi.fn(), everyMs: 1000 })
    await vi.advanceTimersByTimeAsync(0)
    expect(f.updater.checkForUpdates).toHaveBeenCalledTimes(1)
    await vi.advanceTimersByTimeAsync(2500)
    expect(f.updater.checkForUpdates).toHaveBeenCalledTimes(3)
    stop()
    await vi.advanceTimersByTimeAsync(5000)
    expect(f.updater.checkForUpdates).toHaveBeenCalledTimes(3)
  })

  it('does not start a check while the last one is still running', async () => {
    vi.useFakeTimers()
    let release!: () => void
    const updater: Updater = { checkForUpdates: vi.fn(() => new Promise<null>((r) => { release = () => r(null) })) }
    startUpdates({ packaged: true, isolated: false, ...win, load: async () => updater, log: vi.fn(), everyMs: 1000 })
    await vi.advanceTimersByTimeAsync(3500)
    expect(updater.checkForUpdates).toHaveBeenCalledTimes(1)
    release()
    await vi.advanceTimersByTimeAsync(1000)
    expect(updater.checkForUpdates).toHaveBeenCalledTimes(2)
  })

  it('logs a failed check and keeps going', async () => {
    vi.useFakeTimers()
    const f = fake(new Error('offline'))
    const log = vi.fn()
    startUpdates({ packaged: true, isolated: false, ...win, load: f.load, log, everyMs: 1000 })
    await vi.advanceTimersByTimeAsync(1500)
    expect(log).toHaveBeenCalledTimes(2)
    expect(log).toHaveBeenCalledWith('update check failed: offline')
  })

  it('logs a failed download instead of leaving the rejection unhandled, and holds the next check until it settles', async () => {
    vi.useFakeTimers()
    const unhandled = vi.fn()
    process.on('unhandledRejection', unhandled)
    let fail!: (e: Error) => void
    const downloadPromise = new Promise<unknown>((_, rej) => { fail = rej })
    const updater: Updater = { checkForUpdates: vi.fn(async () => ({ downloadPromise, updateInfo: { version: '0.2.0' } })) }
    const log = vi.fn()
    const notify = vi.fn()
    startUpdates({ packaged: true, isolated: false, ...win, load: async () => updater, log, notify, everyMs: 1000 })
    await vi.advanceTimersByTimeAsync(2500)
    expect(updater.checkForUpdates).toHaveBeenCalledTimes(1)
    fail(new Error('sha512 mismatch'))
    await vi.advanceTimersByTimeAsync(0)
    expect(log).toHaveBeenCalledWith('update check failed: sha512 mismatch')
    expect(notify).not.toHaveBeenCalled()
    await vi.advanceTimersByTimeAsync(0)
    process.off('unhandledRejection', unhandled)
    expect(unhandled).not.toHaveBeenCalled()
  })

  it('says a finished download is ready', async () => {
    vi.useFakeTimers()
    const updater: Updater = { checkForUpdates: vi.fn(async () => ({ downloadPromise: Promise.resolve(['x']), updateInfo: { version: '0.2.0' } })) }
    const notify = vi.fn()
    startUpdates({ packaged: true, isolated: false, ...win, load: async () => updater, log: vi.fn(), notify, everyMs: 1000 })
    await vi.advanceTimersByTimeAsync(0)
    expect(notify).toHaveBeenCalledWith('0.2.0')
  })

  it('says a download is ready once, not at every later check that finds it again', async () => {
    vi.useFakeTimers()
    const updater: Updater = { checkForUpdates: vi.fn(async () => ({ downloadPromise: Promise.resolve(['x']), updateInfo: { version: '0.2.0' } })) }
    const notify = vi.fn()
    startUpdates({ packaged: true, isolated: false, ...win, load: async () => updater, log: vi.fn(), notify, everyMs: 1000 })
    await vi.advanceTimersByTimeAsync(3500)
    expect(updater.checkForUpdates).toHaveBeenCalledTimes(4)
    expect(notify).toHaveBeenCalledTimes(1)
  })

  it('says a download with no version once too', async () => {
    vi.useFakeTimers()
    const updater: Updater = { checkForUpdates: vi.fn(async () => ({ downloadPromise: Promise.resolve(['x']) })) }
    const notify = vi.fn()
    startUpdates({ packaged: true, isolated: false, ...win, load: async () => updater, log: vi.fn(), notify, everyMs: 1000 })
    await vi.advanceTimersByTimeAsync(3500)
    expect(notify).toHaveBeenCalledTimes(1)
  })

  it('says nothing for a download that finishes after the updater was stopped', async () => {
    vi.useFakeTimers()
    let done!: () => void
    const downloadPromise = new Promise<unknown>((r) => { done = () => r(['x']) })
    const updater: Updater = { checkForUpdates: vi.fn(async () => ({ downloadPromise, updateInfo: { version: '0.2.0' } })) }
    const notify = vi.fn()
    const stop = startUpdates({ packaged: true, isolated: false, ...win, load: async () => updater, log: vi.fn(), notify, everyMs: 1000 })
    await vi.advanceTimersByTimeAsync(0)
    stop()
    done()
    await vi.advanceTimersByTimeAsync(0)
    expect(notify).not.toHaveBeenCalled()
  })
})
