// @vitest-environment node
import { describe, expect, it } from 'vitest'
import { DEFAULT_PREFS } from '../src/prefs'
import { DEFAULT_CLOSE_TO_TRAY, shouldHideOnClose, trayTooltip, updateNotice, validNotice } from './shell-rules'

const ok = { tag: 'h:a1', title: 'x needs you', body: 'b', sound: true, hostId: 'h', agentId: 'a1', tab: 'terminal' }

describe('validNotice', () => {
  it('passes a well-formed notification through', () => {
    expect(validNotice(ok)).toEqual(ok)
  })
  it('refuses anything else from the renderer', () => {
    for (const bad of [undefined, null, 'x', {}, { ...ok, tab: 'evil' }, { ...ok, sound: 'yes' }, { ...ok, title: '' }, { ...ok, tag: '' }, { ...ok, title: 'x'.repeat(201) }, { ...ok, body: 'x'.repeat(601) }, { ...ok, agentId: 5 }]) {
      expect(validNotice(bad)).toBeUndefined()
    }
  })
})

describe('tray', () => {
  it('says how many wait', () => {
    expect(trayTooltip(0)).toBe('agent-mesh')
    expect(trayTooltip(3)).toBe('agent-mesh: 3 waiting on you')
  })
  it('really closes before the renderer has configured it: main starts from the setting\'s default', () => {
    expect(DEFAULT_CLOSE_TO_TRAY).toBe(DEFAULT_PREFS.closeToTray)
    expect(shouldHideOnClose({ closeToTray: DEFAULT_CLOSE_TO_TRAY, trayReady: true, quitting: false })).toBe(false)
  })
  it('hides the window on close only with the setting, a tray to return from, and no quit under way', () => {
    expect(shouldHideOnClose({ closeToTray: true, trayReady: true, quitting: false })).toBe(true)
    expect(shouldHideOnClose({ closeToTray: false, trayReady: true, quitting: false })).toBe(false)
    expect(shouldHideOnClose({ closeToTray: true, trayReady: false, quitting: false })).toBe(false)
    expect(shouldHideOnClose({ closeToTray: true, trayReady: true, quitting: true })).toBe(false)
  })
  it('says an update is ready, with its version when it has one', () => {
    expect(updateNotice('0.2.0').body).toBe('Version 0.2.0 is downloaded and installs when you quit.')
    expect(updateNotice('').body).toBe('A new version is downloaded and installs when you quit.')
  })
})
