import { getOption } from '@bufbuild/protobuf'
import { readdirSync, readFileSync, statSync } from 'node:fs'
import path from 'node:path'
import { describe, expect, it } from 'vitest'
import { UI_ACTIONS } from './actions'
import { ActorClass } from './gen/agentmesh/v1/actor_pb'
import { MeshService } from './gen/agentmesh/v1/mesh_pb'
import { action } from './gen/agentmesh/v1/options_pb'

function sources(dir: string): string[] {
  return readdirSync(dir).flatMap((f) => {
    const p = path.join(dir, f)
    if (statSync(p).isDirectory()) return f === 'gen' ? [] : sources(p)
    return /\.tsx?$/.test(f) && !/\.test\.tsx?$/.test(f) ? [p] : []
  })
}

describe('the UI and the registry', () => {
  it('every call the UI makes is an action of the registry that a device may call', () => {
    for (const name of UI_ACTIONS) {
      const method = MeshService.method[name]
      expect(method, `${name} is not an action of MeshService`).toBeDefined()
      const opts = getOption(method, action)
      expect(opts.actors, `${name} may not be called by a device`).toContain(ActorClass.DEVICE)
    }
  })

  it('the code calls no host method outside that list', () => {
    const used = new Set<string>()
    for (const f of sources(path.resolve(import.meta.dirname))) {
      for (const m of readFileSync(f, 'utf8').matchAll(/\b(?:client|c)\.(\w+)\(/g)) used.add(m[1]!)
    }
    const known = new Set<string>(UI_ACTIONS)
    expect([...used].filter((u) => !known.has(u))).toEqual([])
    expect(used.size).toBeGreaterThan(0)
  })

  it('only the transport reaches the network', () => {
    const offenders = sources(path.resolve(import.meta.dirname))
      .filter((f) => path.basename(f) !== 'transport.ts')
      .filter((f) => /\bfetch\(|XMLHttpRequest|new WebSocket|EventSource|sendBeacon|window\.desktop\.request/.test(readFileSync(f, 'utf8')))
    expect(offenders).toEqual([])
  })
})
