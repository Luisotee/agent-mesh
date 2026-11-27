import { describe, expect, it } from 'vitest'
import { cliKinds } from './cli'

const tiers = (...kinds: string[]) => Object.fromEntries(kinds.map((k) => [`tiers.${k}.mid.model`, {}]))

describe('cliKinds', () => {
  it('lists the CLIs the host has tiers for, in the usual order', () => {
    expect(cliKinds(tiers('copilot', 'agy', 'claude', 'opencode'))).toEqual(['claude', 'agy', 'opencode', 'copilot'])
  })
  it('leaves out one the host says is not installed', () => {
    expect(cliKinds(tiers('claude', 'agy', 'opencode', 'copilot'), ['claude', 'opencode'])).toEqual(['claude', 'opencode'])
  })
  it('takes a host that names none (an older daemon) to have every CLI it has tiers for', () => {
    expect(cliKinds(tiers('claude', 'agy'), [])).toEqual(['claude', 'agy'])
  })
  it('offers what is installed before the settings are loaded, else Claude and agy', () => {
    expect(cliKinds({}, ['opencode', 'claude'])).toEqual(['claude', 'opencode'])
    expect(cliKinds({})).toEqual(['claude', 'agy'])
  })
})
