import type { Project } from './gen/agentmesh/v1/mesh_pb'

/**
 * The projects the New workspace dialog lists for what the user typed: pinned ones first (in the order they were pinned),
 * then those under the host's `repos.dir` folders, the most recently used first, then the rest by name. A query matches a
 * project when every word of it is in the project's name or path (any case).
 */
export function rankProjects(projects: Project[], query: string, pinned: string[]): Project[] {
  const words = query.toLowerCase().split(/\s+/).filter(Boolean)
  const hit = (p: Project) => words.every((w) => p.name.toLowerCase().includes(w) || p.path.toLowerCase().includes(w))
  const used = (p: Project) => (p.lastUsed ? Number(p.lastUsed.seconds) : 0)
  const order = (p: Project) => {
    const i = pinned.indexOf(p.path)
    return i >= 0 ? i : pinned.length + (p.inReposDir ? 0 : 1)
  }
  return projects
    .filter(hit)
    .sort((a, b) => order(a) - order(b) || used(b) - used(a) || a.name.localeCompare(b.name) || a.path.localeCompare(b.path))
}
