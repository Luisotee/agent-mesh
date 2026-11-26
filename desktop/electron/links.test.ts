import { describe, expect, it, vi } from 'vitest'
import { LinkInbox, linkIn, openedLink } from './links'

const FP = 'ab'.repeat(32)
const LINK = `agentmesh://pair?v=1&h=h_1&fp=sha256:${FP}&t=amp_one&d=${encodeURIComponent('https://10.0.0.2:9443')}&hn=home`

describe('openedLink', () => {
  it('reads a pairing link and says which host it names', () => {
    expect(openedLink(LINK)).toEqual({ link: LINK, hostName: 'home' })
    expect(openedLink(`  ${LINK}\n`)?.link).toBe(LINK)
    expect(openedLink(LINK.replace('agentmesh://', 'AgentMesh://'))?.link).toContain('AgentMesh://') // a scheme's case doesn't matter
  })

  it('drops what is not a readable pairing link', () => {
    expect(openedLink('https://example.com/')).toBeUndefined()
    expect(openedLink('agentmesh://settings')).toBeUndefined()
    expect(openedLink('agentmesh://pair?v=1')).toBeUndefined()
    expect(openedLink(`agentmesh://pair?v=2&h=h&fp=${FP}&t=x&d=https://a:1`)).toBeUndefined()
    expect(openedLink(`${LINK}&x=${'a'.repeat(5000)}`)).toBeUndefined()
  })
})

describe('linkIn', () => {
  it('finds the link among a process\'s arguments, the last one when there are several', () => {
    const other = LINK.replace('amp_one', 'amp_two')
    expect(linkIn(['/usr/bin/agent-mesh', '--flag', LINK])?.link).toBe(LINK)
    expect(linkIn(['electron', '.', LINK, other])?.link).toBe(other)
    expect(linkIn(['electron', '.', 'agentmesh://nope'])).toBeUndefined()
    expect(linkIn([])).toBeUndefined()
  })
})

describe('LinkInbox', () => {
  it('keeps the newest link until it is taken, and rings once per link', () => {
    const ping = vi.fn()
    const inbox = new LinkInbox(ping)
    expect(inbox.take()).toBeUndefined()
    const a = { link: LINK, hostName: 'a' }
    const b = { link: LINK.replace('amp_one', 'amp_two'), hostName: 'b' }
    inbox.offer(a)
    inbox.offer(b)
    expect(ping).toHaveBeenCalledTimes(2)
    expect(inbox.take()).toEqual(b)
    expect(inbox.take()).toBeUndefined()
  })
})
