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
