---
name: harvest-peers
description: >-
  agent-mesh (daemon): runs a research round by the registered peer projects for ideas, improvements and bugs we may have: a root
  the daemon runs briefs them all at once (a first brief, or a follow-up on what changed here and our riskiest
  mechanisms), checks every finding against our code, ranks them, and files the survivors in the tracker on your yes.
  Use it, typed or not, when the user wants ideas or likely bugs from the peers ("harvest the peers", "brief the new
  peers", "have the peers hunt for our bugs"). Not for one question to a peer (`delegate`), finding new peers
  (`find-peers`), or a mesh agent (your task came by a `📬 [mesh]` doorbell).
argument-hint: "[peers or group (default: this repo's)] [what to focus on]"
---

# harvest-peers

Get from the peer projects what would change this repo: features we lack, better ways to do what we do, and bugs their code or history shows we likely have. `$ARGUMENTS` when the user typed the command names the peers and the focus; otherwise the request that brought you here. The repo is the current one. You are the user's own session, not an agent of the mesh: a question to the peers answers to an agent, so a **root** the daemon runs asks them, checks what they say and reports to the user.

The value is in the checking: a peer reads its own project well and ours from a distance, so every claim it makes about our code is checked before the user sees it.

1. **Pick the peers.** `mesh peers` lists them by group, with what each is worth asking about. Default to the group this repo's `CLAUDE.md` / `AGENTS.md` or a group's `about` names for it; the user's words narrow it. No peers: say so and point to `find-peers`.
2. **What's new since last time.** In `mesh peers`, a peer whose `sessionId` is empty has never been briefed on the daemon (one imported from bin/mesh's `peers.json` is unbriefed too) and gets the **first-round** brief; the rest get the **follow-up**. For the follow-up:
   - "since" is the oldest `asked` of those peers: `git log --since=<it> --oneline` here, and read what matters of it;
   - the open themes of the repo's tracker (its `TODO.md`, issues, or whatever its `CLAUDE.md` / `AGENTS.md` names);
   - each peer's last answers, so it isn't asked again what it already said and the brief can list what's settled: its `lastAnswers` file (a `reports/peers-<stamp>.md` of the run that asked it, under the daemon's run folders: `mesh dir --run <run id>`).
3. **Bug-hunt targets** (follow-up): 3 to 5 of our riskiest mechanisms, each with its file and function and the cases worth checking: what changed most since last time, what is concurrent or crash-sensitive (locks, retries, delivery, state files), what open issues or recent fixes keep touching. Pick ones the peers share with us (their `about`s say which).
4. **Start the run and its root** as the `delegate` skill's steps 1 to 4 say (`mesh hello`, `mesh start-run`, the request file, `mesh spawn --role root`, in this repo's checkout). Write the brief, from [HARVEST.md](HARVEST.md) (next to this file), into `<run folder>/tasks/harvest-<first|follow-up>.md`, filling the variant and a Focus line per peer (one file per variant when a round mixes both), and make the request file say:
   - run `mesh ask --peers <name>,<name>,… --tier <strong for a first brief, mid for a follow-up> --timeout 3600 --body @<brief file>` (one call per variant: a first brief takes a peer 10 to 20 minutes and a follow-up reads both codebases, so the default 30 minutes is tight), then end its turn: the answers come back as **one** message with the path of one file;
   - a peer that is not asked says why in that message (for one running in another run: one process per conversation, so wait for that run's answer or retire it there); ask it again with `mesh ask --peers <name> …` once it can be;
   - **check every finding in our code** before reporting it: read the counterpart the peer named (or find it) and label the finding: **real bug** or **real gap** (seen, with `file:line` and the input or sequence that goes wrong; for a bug, a reproduction or the test that would show it), **already tracked** (where), **already rejected** (the design doc's section that says why, and whether the peer's evidence changes that) or **wrong** (what the code actually does); ask a peer back (`mesh ask --peers <name> --tier mid --body …`) when its claim is about its own project and the root can't settle it from the clone;
   - report to the user the surviving findings ranked, most valuable first, in three lists (bugs, improvements, features), each with its label, the cost, and the peer's name; count what was dropped and why in one line; where an answer shows a peer's `about` was wrong, `mesh edit-peer --target <name> --about …`;
   - **on the user's yes** (the root asks in the app), add the chosen findings to the repo's tracker in its own format, each naming the round's answers file and the peer; after a first round, offer to note the group in the repo's `CLAUDE.md` / `AGENTS.md` (which peers exist and what to ask each), so later sessions ask them before designing. A bug the user wants fixed now is ordinary work: a worker's task, never a peer's, which only answers.
   Pass the repo's checkout as `--repo`.
5. **Tell the user** the run id and the root's name, and that the root reports the checked findings in the app.
