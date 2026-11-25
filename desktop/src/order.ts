import type { Message } from './gen/agentmesh/v1/types_pb'

/** A message's time in nanoseconds, as the host stores it. */
const ns = (m: Message): bigint => (m.createdAt?.seconds ?? 0n) * 1_000_000_000n + BigInt(m.createdAt?.nanos ?? 0)

/**
 * The host's order of messages: time to the nanosecond, then id as bytes (SQLite's order). Every client-side sort
 * and comparison of messages uses this one, so the cursor of a resumed load names the message the host put last.
 */
export function compareMessages(a: Message, b: Message): number {
  const x = ns(a)
  const y = ns(b)
  if (x !== y) return x < y ? -1 : 1
  return a.id === b.id ? 0 : a.id < b.id ? -1 : 1
}

export const isBefore = (a: Message, b: Message): boolean => compareMessages(a, b) < 0
