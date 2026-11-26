/** What the app needs of an updater (`electron-updater`'s `autoUpdater`): look for a newer release, download it, tell the user. */
export interface Updater {
  checkForUpdatesAndNotify(): Promise<unknown>
}

export const CHECK_EVERY_MS = 6 * 60 * 60 * 1000

/**
 * Looks for a newer release now and then every few hours, in an installed app only: a dev or test run (not packaged, or
 * with a user-data dir of its own) must never fetch an update or touch the user's real install. A failed check (offline,
 * no release yet, an installer type that can't update itself) is logged and nothing more. Returns how to stop.
 */
export function startUpdates(o: { packaged: boolean; isolated: boolean; load: () => Promise<Updater>; log: (m: string) => void; everyMs?: number }): () => void {
  if (!o.packaged || o.isolated) return () => {}
  let stopped = false
  let checking = false
  const check = async () => {
    // A slow check or download is not started over by the next tick.
    if (checking) return
    checking = true
    try {
      const updater = await o.load()
      if (!stopped) await updater.checkForUpdatesAndNotify()
    } catch (e) {
      o.log(`update check failed: ${e instanceof Error ? e.message : String(e)}`)
    } finally {
      checking = false
    }
  }
  void check()
  const timer = setInterval(() => void check(), o.everyMs ?? CHECK_EVERY_MS)
  timer.unref()
  return () => {
    stopped = true
    clearInterval(timer)
  }
}
