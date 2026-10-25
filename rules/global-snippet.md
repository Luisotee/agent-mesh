## agent-mesh

- A prompt starting with `📬 [mesh]` (typed into your pane, or sent by your parent with SendMessage) means you are an agent in an **agent mesh**. Read `~/.config/agent-mesh/PROTOCOL.md` (once per session), run `mesh inbox`, and follow the protocol: report to your parent with `mesh send`.
- Start a mesh (`mesh init` / `mesh spawn` / `mesh ask`) only through the `delegate-agy` / `delegate-claude` skills, which you may use on your own when a request fits their description (work, a question or a review handed to agents in other repos), not only when the user types the command. A mesh needs herdr (`HERDR_ENV=1`): outside it, answer a read-only question about another repo by running agy or Claude headless from that repo (`cd <repo> && agy -p "<prompt>" --dangerously-skip-permissions`) and using its output.
- A mesh worker never spawns agents, headless or not.
