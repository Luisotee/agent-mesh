import { readFileSync } from 'node:fs'
import path from 'node:path'
import { describe, expect, it } from 'vitest'
import { PROTOCOL } from './links'

const read = (f: string) => JSON.parse(readFileSync(path.resolve(import.meta.dirname, '..', f), 'utf8')) as Record<string, any>
const builder = read('electron-builder.json')
const pkg = read('package.json')

describe('the installers (electron-builder.json)', () => {
  it('register the scheme the app answers to, on every platform it packages', () => {
    // `protocols` is what electron-builder turns into macOS CFBundleURLTypes, the NSIS installer's registry keys and the
    // deb's `MimeType=x-scheme-handler/<scheme>` (checked on a real deb: `pnpm pack:check`).
    expect(builder.protocols).toHaveLength(1)
    expect(builder.protocols[0].schemes).toEqual([PROTOCOL])
    expect(builder.mac.target).toContain('dmg')
    expect(builder.win.target).toContain('nsis')
    expect(builder.linux.target).toContain('deb')
  })

  it('ship the built app: the main entry, the preload, the page', () => {
    expect(builder.files).toEqual(expect.arrayContaining(['dist/**', 'dist-electron/**']))
    expect(pkg.main).toBe('dist-electron/electron/main.js')
    // The window loads ../../dist/index.html and preload.cjs from main's folder (electron/main.ts).
    expect(readFileSync(path.resolve(import.meta.dirname, 'main.ts'), 'utf8')).toContain("'../../dist/index.html'")
  })

  it('carry only what the main process needs at run time (the page is bundled)', () => {
    expect(Object.keys(pkg.dependencies).sort()).toEqual(['electron-updater', 'ws'])
  })

  it('publish to GitHub Releases, where the app looks for updates', () => {
    expect(builder.publish).toEqual([{ provider: 'github', owner: 'Luisotee', repo: 'agent-mesh' }])
    expect(builder.win.target).toEqual(expect.arrayContaining(['nsis']))
  })

  it('name the desktop entry the deb installs, so a window groups under its icon', () => {
    expect(pkg.desktopName).toBe(`${pkg.name}.desktop`)
    expect(builder.linux.syncDesktopName).toBe(true)
  })
})
