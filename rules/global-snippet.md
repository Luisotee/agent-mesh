## agent-mesh

- A prompt starting with `📬 [mesh]` means you are an agent in an **agent mesh**. Read `~/.config/agent-mesh/PROTOCOL.md` (once per session), run `mesh inbox`, and follow the protocol: report to your parent with `mesh send`.
- Start a mesh (`mesh init` / `mesh spawn`) only when the user typed `/delegate-agy` or `/delegate-claude`. This limits meshes, not agy/claude: when the user asks you to have agy or Claude answer something about another repo without changing it (a summary, a question), run it headless from that repo (`cd <repo> && agy -p "<prompt>" --dangerously-skip-permissions`) and use its output.
- A mesh worker never spawns agents, headless or not.
