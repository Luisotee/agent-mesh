---
name: find-peers
description: >-
  Finds open-source projects that share a repo's core mechanisms, to learn from their code, history and issues:
  characterizes the repo, searches GitHub, awesome lists, plugin directories and the web, ranks the candidates by
  mechanism overlap rather than stars, shows you the shortlist, and on your yes registers each one as a standing
  agent-mesh peer (`mesh peers add`, with an `about`) and sends them one first research brief. Use it, whether the user
  types /find-peers or not, when the user wants similar projects, competitors or prior art to copy ideas from ("find peer
  projects", "who else solves this", "survey tools like this one", "find projects to learn from"). Not for asking peers
  already registered (that's /delegate-claude or /delegate-agy), and never when you are a mesh worker (your task came by
  a `📬 [mesh]` doorbell).
argument-hint: "[repo path (default: this one)] [what to focus on]"
---

# /find-peers

Find the projects worth learning from for the user's request: `$ARGUMENTS` when the user typed the command, otherwise the request that brought you here. The repo is the one named, else the current one.

The goal is a few projects that solve the **same problems by similar mechanisms**, read from their code, not a popularity list. A peer is worth registering when asking it would change a design decision here.

1. **Characterize the repo.** Read its README, `CLAUDE.md` / `AGENTS.md` and design docs. Write down, for yourself:
   - the problem it solves, in one sentence;
   - its 4 to 6 **core mechanisms** (how it does what it does, e.g. "delivers messages into running agent sessions", "a worktree per worker"), each named in plain words someone else's README would use;
   - its constraints (language, no dependencies, platform) and the alternatives it already rejected.
   Run `mesh peers` (if `mesh` is installed): skip projects already registered, and note the group this repo's peers are in, if any: it is the default group for new ones. The user's focus, when given, narrows the mechanisms.
2. **Search several surfaces**, a few queries per mechanism, not one for the whole repo:
   - `gh search repos "<mechanism words>" --sort updated --limit 30`, and by topic (`--topic <t>`, from the topics of the first good hits);
   - `gh search code` for a distinctive API, file name or config key the mechanism implies;
   - the awesome lists for the domain (`gh search repos awesome <domain>`), and the plugin directories or marketplaces when the repo is itself a skill, plugin or extension;
   - the package registry for its language;
   - a web search: "alternatives to <closest known project>", the problem in the user's words, and the "how it compares" sections of the hits, which name their competitors.
   Collect 10 to 25 candidates.
3. **Signs of life:** `gh repo view <owner/repo> --json description,stargazerCount,pushedAt,isArchived,licenseInfo,repositoryTopics`. Drop dead ones (archived, or no push in a year) unless one is worth keeping as a contrast, and say so. Note a missing license: such a project is fine to read, not to copy from.
4. **Read before ranking.** For each survivor, read its README, then its source tree: a shallow clone (`git clone --depth 50`) in a scratch directory, never inside the repo. Find the file that implements each shared mechanism. A README claims; the code shows.
5. **Rank by mechanism overlap,** not stars: a table with a row per candidate and a column per mechanism (yes / partial / no, with the file that shows it), plus what it would teach that the current peers don't, and its signs of life. Stars only break ties. Keep 3 to 8.
6. **Show the shortlist and wait.** The table, then for each pick one line on what to ask it, and the group name you suggest. Say the cost: each peer's first brief is a large research task (a few hundred thousand tokens), and its clone lives under `~/research/<group>/`. Ask which to add. **Clone and register nothing before the user's yes.**
7. **On the yes,** register each with mesh (needs agent-mesh installed; `peers add` itself needs no herdr):

   ```bash
   mesh peers add <git-url> --group <group> --about "<what it is; what to ask it about>"   # at most 400 characters
   mesh peers edit <group> --about "<what the group is, whose peers they are>"             # a new group only
   ```

   Give no `--body` there: it would ask each peer in turn and wait for it. Instead write one brief from [BRIEF.md](BRIEF.md) (next to this file), with the repo's mechanics and a Focus line per peer, and ask them all at once:
   - **The brief's file:** under a run, not in the repo. If `mesh whoami` fails, start one with `mesh init --mode <claude | agy: your own CLI> --request "find peers for <repo>"`, then write `$(mesh dir)/tasks/peers-brief.md`.
   - **Ask:** `mesh ask --peers <name>,<name>,… --size large --file "$(mesh dir)/tasks/peers-brief.md"`. Claude Code: run it with `run_in_background`; agy: in the foreground. It needs herdr (`HERDR_ENV=1`); outside it, stop after `peers add` and tell the user to send the brief from a herdr session.
   - **The answers** come in **one file**, whose path the command prints: never read N inboxes. A line that says a peer didn't answer says what to do (see "Standing peer askers" in `~/.config/agent-mesh/ORCHESTRATE.md`).
8. **Report:** a ranked summary of what the peers found, across all of them, with each peer's name so the user can ask it again (`mesh ask <name> --body …`, or `--peers <group>`). Where an answer shows an `about` was wrong, fix it with `mesh peers edit <name> --about …`. Offer to note the group in the repo's `CLAUDE.md` / `AGENTS.md` (which peers exist and what to ask each), so later sessions ask them before designing; write it only on a yes.

Without agent-mesh, do steps 1 to 6 and give the shortlist with the clone commands: the search and the ranking are the part that works anywhere.
