import { afterEach, describe, expect, it, vi } from 'vitest'
import type { DesktopApi } from '../shared/api'

afterEach(() => vi.resetModules())

describe('boot', () => {
  it('says why in the page when the app cannot start (a build with no theme files), instead of leaving it blank', async () => {
    // What the Windows installer had: the glob over shared/themes matched nothing, so there was no theme to apply.
    vi.doMock('./theme', () => ({ DEFAULT_THEME: 'Dracula', themeByName: () => undefined, applyTheme: (t: { tokens: unknown }) => Object.entries(t.tokens as object) }))
    const { boot } = await import('./boot')
    const root = document.createElement('div')
    expect(() => boot(root, {} as DesktopApi)).toThrow(/tokens/)
    expect(root.querySelector('[role=alert]')?.textContent).toMatch(/agent-mesh could not start: .*tokens/)
  })
})
