---
name: harvest-peers
description: >-
  Runs a round of research by the registered peer projects (agent-mesh's standing peer askers) for ideas, improvements
  and bugs we may have: picks the peers, works out what changed on our side since they were last asked, names our
  riskiest mechanisms for them to compare against theirs, asks them all at once with one brief (the first-round brief
  for peers that never researched anything), checks every finding against our own code, and reports them ranked as bugs, improvements and
  features, filing the survivors in the repo's tracker on your yes. Use it, whether the user types /harvest-peers or
  not, when the user wants ideas, features, improvements or likely bugs gathered from the peer projects ("harvest the
  peers", "what can we learn from the peers now", "brief the new peers", "have the peers hunt for our bugs"). Not for
  one question to a peer (that's /delegate-claude or /delegate-agy), not for finding new peers (that's /find-peers),
  and never when you are a mesh worker (your task came by a `📬 [mesh]` doorbell).
argument-hint: "[peers or group (default: this repo's)] [what to focus on]"
---

# /harvest-peers

Get from the peer projects what would change this repo: features we lack, better ways to do what we do, and bugs their code or history shows we likely have. `$ARGUMENTS` when the user typed the command names the peers and the focus; otherwise the request that brought you here. The repo is the current one.

The value is in step 5: a peer reads its own project well and ours from a distance, so every claim it makes about our code is checked here before the user sees it.

1. **Pick the peers.** `mesh peers` lists them by group, with what each is worth asking about. Default to the group this repo's `CLAUDE.md` / `AGENTS.md` or a group's `about` names for it; the user's words narrow it. No peers: say so and point to `/find-peers`.
2. **What's new since last time.** `mesh peers --json`: a peer whose `session` is `none yet` has never researched anything and gets the **first-round** brief; the rest get the **follow-up**, even with an empty `asked` (a peer adopted from an asker that researched it by hand). For the follow-up:
   - "since" is the oldest `asked` of those peers, a UTC time cut to minutes: `git log --since=<it>Z --oneline` here (without the `Z` git reads it as local time and skips the commits right after the last round), and read what matters of it. A peer with a session but no `asked` (no `run` in `$MESH_HOME/peers.json` either): `mesh agents <peer>` lists the runs its conversation worked in; date it from the newest of those, or ask the user;
   - the open themes of the repo's tracker (its `TODO.md`, issues, or whatever its `CLAUDE.md` / `AGENTS.md` names);
   - each peer's last answers, so it isn't asked again what it already said and the brief can list what's settled. `answers` in the JSON is only the newest `ask --peers` file of any group or question, so look per peer: its `run` in `$MESH_HOME/peers.json` (`MESH_HOME` defaults to `~/.local/state/agent-mesh`; none for an adopted peer never asked through mesh: the runs `mesh agents <peer>` lists), then that run's `reports/peers-*.md` naming it, under `$MESH_HOME/<run>/` or, once `mesh stop` archived the run, `$MESH_HOME/archive/<run>/`. A peer asked alone (`mesh ask <peer>`) left no answers file: its answer is in that run's mailbox, or ask it what it last reported.
3. **Bug-hunt targets** (follow-up): 3 to 5 of our riskiest mechanisms, each with its file and function and the cases worth checking: what changed most since last time, what is concurrent or crash-sensitive (locks, retries, delivery, state files), what open issues or recent fixes keep touching. Pick ones the peers share with us (their `about`s say which).
4. **Ask.** The brief's file goes under a run, not in the repo: if `mesh whoami` fails, start one with `mesh init --mode <claude | agy: your own CLI> --request "harvest peers for <repo>"`. Write `$(mesh dir)/tasks/harvest-<first|follow-up>.md` from [HARVEST.md](HARVEST.md) (next to this file), filling the variant and a Focus line per peer. Then
   - `mesh ask --peers <name>,<name>,… --timeout 3600 --file "$(mesh dir)/tasks/harvest-<variant>.md"`, one call per variant: a first brief takes a peer 10 to 20 minutes and a follow-up reads both codebases, so the default 30 minutes is tight, and a peer given up on answers later in your inbox instead of the file. Claude Code: run it with `run_in_background`; agy: in the foreground.
   - The answers come in **one file**, whose path the command prints: never read N inboxes. A line that says a peer didn't answer says what to do (see "Standing peer askers" in `~/.config/agent-mesh/ORCHESTRATE.md`).
   - **No herdr** (`HERDR_ENV` unset): fill the brief, give it to the user with that `mesh ask --peers` line to run from a herdr session, and stop.
5. **Check every finding in our code** before reporting it. Read the counterpart the peer named (or find it), and label the finding:
   - **real bug** or **real gap**: you saw it, with `file:line` and the input or sequence that goes wrong; for a bug, a reproduction or the test that would show it;
   - **already tracked**: where;
   - **already rejected**: the design doc's section that says why, and whether the peer's evidence changes that;
   - **wrong**: what the code actually does.
   Ask a peer back (`mesh ask <name> --body …`) when its claim is about its own project and you can't settle it from the clone yourself.
6. **Report:** the surviving findings ranked, most valuable first, in three lists (bugs, improvements, features), each with its label, the cost, and the peer's name so the user can ask it again. Count what you dropped and why in one line. Where an answer shows a peer's `about` was wrong, fix it with `mesh peers edit <name> --about …`. Then **on the user's yes**:
   - add the chosen findings to the repo's tracker, in its own format, each naming the round's answers file and the peer;
   - after a first round, offer to note the group in the repo's `CLAUDE.md` / `AGENTS.md` (which peers exist and what to ask each), so later sessions ask them before designing.
   A bug the user wants fixed now is ordinary work: in this repo, fix it or hand it to a worker as usual; never to a peer, which only answers.
