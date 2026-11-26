import { create } from '@bufbuild/protobuf'
import { render, screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import { statusSentence } from '@/derive'
import { AgentSchema, AgentState } from '@/gen/agentmesh/v1/types_pb'
import { StateIcon } from './state-icon'

const agent = (state: AgentState) => create(AgentSchema, { id: 'a', name: 'a', state })

describe('every agent state has its own icon and sentence', () => {
  // The enum's values, so a state the host adds later fails here until the app shows it.
  const states = Object.values(AgentState).filter((v): v is AgentState => typeof v === 'number' && v !== AgentState.UNSPECIFIED)

  it('an icon whose label differs per state, and none falls back to unknown unless it is unknown', () => {
    const labels = new Map<string, AgentState>()
    for (const s of states) {
      const { container, unmount } = render(<StateIcon agent={agent(s)} />)
      const label = container.querySelector('svg')?.getAttribute('aria-label') ?? ''
      expect(label, AgentState[s]).not.toBe('')
      if (s !== AgentState.UNKNOWN) expect(label, `${AgentState[s]} fell through to the unknown icon`).not.toBe('unknown')
      // Exited shows done, failed or stopped by its code; every other state has one label.
      if (s !== AgentState.EXITED) expect(labels.has(label), `${AgentState[s]} shares ${label}`).toBe(false)
      labels.set(label, s)
      unmount()
    }
  })

  it('a sentence per state, and the new ones say what to do', () => {
    for (const s of states) expect(statusSentence(agent(s)), AgentState[s]).not.toBe('Unknown')
    expect(statusSentence(agent(AgentState.RETIRED))).toBe('Retired: stopped on purpose, a message wakes it')
    expect(statusSentence(create(AgentSchema, { state: AgentState.LIMITED, stateDetail: 'usage limit, resets 9pm' }))).toBe('Hit a usage limit: usage limit, resets 9pm; switch it to another model to go on')
    expect(statusSentence(agent(AgentState.LIMITED))).toBe('Hit a usage limit; switch it to another model to go on')
    expect(statusSentence(create(AgentSchema, { state: AgentState.ERROR, stateDetail: 'API error 500' }))).toBe('Its last turn ended in an error: API error 500')
  })

  it('the retired and limited states are announced to a screen reader', () => {
    render(<><StateIcon agent={agent(AgentState.RETIRED)} /><StateIcon agent={agent(AgentState.LIMITED)} /><StateIcon agent={agent(AgentState.ERROR)} /></>)
    expect(screen.getByLabelText('retired')).toBeInTheDocument()
    expect(screen.getByLabelText('at its limit')).toBeInTheDocument()
    expect(screen.getByLabelText('error')).toBeInTheDocument()
  })
})
