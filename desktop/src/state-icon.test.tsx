import { create } from '@bufbuild/protobuf'
import { render, screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import { StateIcon } from './components/state-icon'
import { AgentSchema, AgentState } from './gen/agentmesh/v1/types_pb'

const exited = (exitCode: number, stateDetail: string) => create(AgentSchema, { id: 'a', name: 'a', state: AgentState.EXITED, exitCode, stateDetail })

describe('StateIcon for an exited agent', () => {
  it('tells a lost terminal, as the host stores it, from a stop and a clean exit', () => {
    const label = (a: ReturnType<typeof exited>) => {
      const { unmount } = render(<StateIcon agent={a} />)
      const l = (screen.getByLabelText(/stopped|done|failed/) as Element).getAttribute('aria-label')
      unmount()
      return l
    }
    expect(label(exited(-1, ''))).toBe('failed') // lost: recordExit(…, -1, 0, "lost") and the reducer clears the detail
    expect(label(exited(-1, 'killed by signal'))).toBe('stopped')
    expect(label(exited(0, ''))).toBe('done')
    expect(label(exited(3, ''))).toBe('failed')
  })
})
