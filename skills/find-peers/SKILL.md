---
name: find-peers
description: >-
  Finds open-source projects that share a repo's core mechanisms, to learn from their code and history: searches GitHub,
  awesome lists and the web, ranks by mechanism overlap rather than stars, shows you the shortlist, and on your yes
  registers each as an agent-mesh peer, unbriefed. Use it, typed or not, when the user wants similar projects,
  competitors or prior art ("find peer projects", "who else solves this", "find projects to learn from"). Not for asking
  registered peers (/harvest-peers for a round, /delegate-claude or /delegate-agy for one question), or a mesh worker
  (your task came by a `📬 [mesh]` doorbell).
argument-hint: "[repo path (default: this one)] [what to focus on]"
---

# /find-peers

Find the projects worth learning from for the user's request: `$ARGUMENTS` when the user typed the command, otherwise the request that brought you here. The repo is the one named, else the current one.

The goal is a few projects that solve the **same problems by similar mechanisms**, read from their code, not a popularity list. A peer is worth registering when asking it would change a design decision here.

1. **Characterize the repo.** Read its README, `CLAUDE.md` / `AGENTS.md` and design docs. Write down, for yourself:
   - the problem it solves, in one sentence;
   - its 4 to 6 **core mechanisms** (how it does what it does, e.g. "delivers messages into running agent sessions", "a worktree per worker"), each named in plain words someone else's README would use;
   - its constraints (language, no dependencies, platform) and the alternatives it already rejected.
   Run `mesh peers` (if `mesh` is installed): skip projects already registered. The default group for new ones is the one the repo's `CLAUDE.md` / `AGENTS.md` or a group's `about` names for this repo; else ask in step 6. The user's focus, when given, narrows the mechanisms.
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
6. **Show the shortlist and wait.** The table, then for each pick one line on what to ask it, and the group name you suggest. Say the cost: a peer's clone lives under `~/research/<group>/`, and its first research round (`/harvest-peers`) is a large task (a few hundred thousand tokens). Ask which to add. **Register nothing and clone nothing under `~/research` before the user's yes**: step 4's scratch clones are for reading only (delete them once you've ranked).
7. **On the yes,** register each with mesh (needs agent-mesh installed; `peers add` needs no herdr):

   ```bash
   mesh peers add https://github.com/<owner>/<repo> --group <group> --about "<what it is; what to ask it about>"   # at most 400 characters
   mesh peers edit <group> --about "<what the group is, whose peers they are>"                                   # a new group only
   ```

   - **Names:** each `peers add` prints the asker's `name` (`<repo>-research`, the repo part cut to 20 characters): report that name, never one you built from the repo's name. Pass `--name <short>-research` when a basename is long, or when two shortlisted projects share one (the second add would fail).
   - Give no `--body`: it would ask each peer in turn and wait for it.
8. **Report and stop:** the peers added, by name and group, with what each is for. Say they haven't been briefed yet, and that `/harvest-peers` gives them their first research round when the user wants it. Don't start it yourself: the user decides when to spend that.

Without agent-mesh, do steps 1 to 6 and give the shortlist with the clone commands: the search and the ranking are the part that works anywhere.
