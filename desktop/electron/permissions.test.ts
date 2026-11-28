import { describe, expect, it } from 'vitest'
import { allowsPermission } from './permissions.js'

describe('allowsPermission', () => {
  it('lets the page write to the clipboard (the copy buttons) and nothing else', () => {
    expect(allowsPermission('clipboard-sanitized-write')).toBe(true)
    for (const p of ['clipboard-read', 'media', 'geolocation', 'notifications', 'openExternal', 'fullscreen', 'midi', '']) expect(allowsPermission(p), p).toBe(false)
  })
})
