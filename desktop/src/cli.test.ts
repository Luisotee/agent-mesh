import { describe, expect, it } from 'vitest'
import { cliKinds, hostClis } from './cli'

const tiers = (...kinds: string[]) => Object.fromEntries(kinds.map((k) => [`tiers.${k}.mid.model`, {}]))

describe('cliKinds', () => {
  it('lists the CLIs the host has tiers for, in the usual order, from a daemon that does not say what is installed', () => {
    expect(cliKinds(tiers('copilot', 'agy', 'claude', 'opencode'))).toEqual(['claude', 'agy', 'opencode', 'copilot'])
    expect(cliKinds({})).toEqual(['claude', 'agy'])
  })
  it('lists what the host says is installed, whatever tiers it has or has not loaded', () => {
    expect(cliKinds(tiers('claude', 'agy', 'opencode', 'copilot'), ['opencode', 'claude'])).toEqual(['claude', 'opencode'])
    expect(cliKinds({}, ['opencode'])).toEqual(['opencode'])
    expect(cliKinds(tiers('claude', 'agy'), ['opencode'])).toEqual(['opencode'])
  })
  it('lists none for a host that says it has none', () => {
    expect(cliKinds(tiers('claude', 'agy'), [])).toEqual([])
  })
})

describe('hostClis', () => {
  it('is the list only from a daemon with the clis capability, an empty one included', () => {
    expect(hostClis({ capabilities: ['clis'], clis: [] })).toEqual([])
    expect(hostClis({ capabilities: ['clis'], clis: ['claude'] })).toEqual(['claude'])
    expect(hostClis({ capabilities: ['terminals'], clis: [] })).toBeUndefined()
    expect(hostClis(undefined)).toBeUndefined()
  })
})
