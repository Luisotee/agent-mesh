/** What the app needs of an updater (`electron-updater`'s `autoUpdater`): look for a newer release and download it. */
export interface Updater {
  checkForUpdates(): Promise<{ downloadPromise?: Promise<unknown> | null; updateInfo?: { version?: string } } | null | undefined>
}

export const CHECK_EVERY_MS = 6 * 60 * 60 * 1000

/** The NSIS installer's uninstaller (`Uninstall ${productFilename}.exe`, electron-builder's NSIS template) sits beside the exe of an installed app, and of nothing else. */
export const NSIS_UNINSTALLER = 'Uninstall agent-mesh.exe'

/**
 * Whether this install can update itself the way the docs say: the NSIS installer (Windows, `nsis`: the uninstaller is
 * beside the exe) and the AppImage (Linux). A deb would be installed by electron-updater as root (`dpkg -i` through
 * pkexec), there is no zip artifact on macOS for its updater, and the Windows zip carries the same update config as
 * the installer (electron-builder writes it into the folder both are made from), so it would install a second copy:
 * none of those looks for updates. A deb is updated by the package manager, a dmg or a zip by a new download.
 */
export function canSelfUpdate(platform: string, env: { APPIMAGE?: string }, nsis: boolean): boolean {
  if (platform === 'win32') return nsis
  if (platform === 'linux') return !!env.APPIMAGE
  return false
}

/**
 * Looks for a newer release now and then every few hours, in an installed app only: a dev or test run (not packaged, or
 * with a user-data dir of its own) must never fetch an update or touch the user's real install. A failed check or
 * download (offline, no release yet, a release missing this platform's installer) is logged and nothing more: every
 * promise the updater hands back is awaited here, because one left alone would crash the main process. `notify` says
 * a download is ready (it installs when the app quits). Returns how to stop.
 */
export function startUpdates(o: {
  packaged: boolean
  isolated: boolean
  platform: string
  env: { APPIMAGE?: string }
  /** Windows: this is an NSIS install (`NSIS_UNINSTALLER` beside the exe), not the unzipped zip. */
  nsis: boolean
  load: () => Promise<Updater>
  log: (m: string) => void
  notify?: (version: string) => void
  everyMs?: number
}): () => void {
  if (!o.packaged || o.isolated || !canSelfUpdate(o.platform, o.env, o.nsis)) return () => {}
  let stopped = false
  let checking = false
  let told: string | undefined
  const check = async () => {
    // A slow check or download is not started over by the next tick.
    if (checking) return
    checking = true
    try {
      const updater = await o.load()
      if (stopped) return
      const found = await updater.checkForUpdates()
      if (found?.downloadPromise) {
        await found.downloadPromise
        if (stopped) return
        // A later check finds the same cached download again: say it once (a download with no version too).
        const version = found.updateInfo?.version ?? ''
        if (version !== told) o.notify?.(version)
        told = version
      }
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
