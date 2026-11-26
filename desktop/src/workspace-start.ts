/** Where a new workspace's worktree starts: the project's default branch, a branch, or a pull request's branch. */
export type StartFrom = { kind: 'default' } | { kind: 'branch'; ref?: string } | { kind: 'pr'; ref?: string }

/**
 * The `base` of `SpawnAgent` for a choice: empty for the default branch (the host starts from the remote's default); a
 * branch as the ref `ListProjects` gave it; a pull request's head branch as the remote's (`origin/<head>`, which
 * `ListProjects` fetched).
 */
export function startPoint(from: StartFrom): string {
  switch (from.kind) {
    case 'branch':
      return from.ref ?? ''
    case 'pr':
      return from.ref ? `origin/${from.ref}` : ''
    default:
      return ''
  }
}

/**
 * The pull requests to offer, one for each head branch (two PRs of one branch start the same worktree, and an option's
 * value is the branch). A fork's PR stays in the list, to be shown as unavailable: its `head_ref` names the fork's
 * branch, which is no ref of the repo's remote.
 */
export function startablePullRequests<T extends { number: number; headRef: string; crossRepository: boolean }>(prs: T[]): T[] {
  const seen = new Set<string>()
  return prs.filter((p) => {
    if (p.crossRepository) return true
    if (seen.has(p.headRef)) return false
    seen.add(p.headRef)
    return true
  })
}
