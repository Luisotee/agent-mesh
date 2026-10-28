## agent-mesh

- A prompt starting with `📬 [mesh]` (typed into your pane, or arriving as a cross-session message) means you are an agent in an **agent mesh**. Read `~/.config/agent-mesh/PROTOCOL.md` (once per session), run `mesh inbox`, and follow the protocol: report to your parent with `mesh send`.
- Start a mesh (`mesh init` / `mesh spawn` / `mesh ask`) only through the `delegate-agy` / `delegate-claude` / `delegate-review` / `find-peers` / `harvest-peers` skills. You may use them on your own, not only when the user types the command, when a request fits their description: work or a question for agents in other repos, a PR review, finding peer projects to learn from, or a research round by them. A mesh needs herdr (`HERDR_ENV=1`): outside it, answer a read-only question about another repo by running agy or Claude headless from that repo (`cd <repo> && agy -p "<prompt>" --dangerously-skip-permissions`) and using its output.
- A `📬 [mesh]` prompt saying you are a run's **root** and `mesh reset` reset your session is not a worker's doorbell: you have no parent to report to. Run `mesh brief` and carry on as the root. As a root, also run `mesh brief` first after your context was compacted.
- A mesh worker never spawns agents, headless or not.

### Writing

Write plainly and briefly, in full sentences: chat, reports, plans, commit messages and docs.
- Lead with the result. No preamble, no narration of your steps, no closing recap.
- Keep every fact, reason and caveat that changes what the reader does; cut the rest.
- Quote code, paths, commands and error messages verbatim.
