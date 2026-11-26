import { describe, expect, it } from 'vitest'
import { safeLoginUrl, tailnetAddresses } from './tailnet'

describe('tailnetAddresses', () => {
  it('offers the direct address, then the MagicDNS name on the same port', () => {
    expect(tailnetAddresses({ directUrl: 'https://100.64.0.7:8443', dnsName: 'm.tail1.ts.net.' })).toEqual(['https://100.64.0.7:8443', 'https://m.tail1.ts.net:8443'])
  })

  it('offers an address once when the direct address already is the MagicDNS name', () => {
    expect(tailnetAddresses({ directUrl: 'https://m.tail1.ts.net:8443', dnsName: 'm.tail1.ts.net.' })).toEqual(['https://m.tail1.ts.net:8443'])
  })

  it('offers only what parsed', () => {
    expect(tailnetAddresses({ directUrl: 'not a url', dnsName: 'm.ts.net.' })).toEqual(['not a url'])
    expect(tailnetAddresses({ directUrl: '', dnsName: 'm.ts.net.' })).toEqual([])
  })
})

describe('safeLoginUrl', () => {
  it('passes only https', () => {
    expect(safeLoginUrl('https://login.tailscale.com/a/1')).toBe('https://login.tailscale.com/a/1')
    expect(safeLoginUrl('http://login.tailscale.com/a/1')).toBeUndefined()
    expect(safeLoginUrl('javascript:alert(1)')).toBeUndefined()
  })
})
