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
6. **Show the shortlist and wait.** The table, then for each pick one line on what to ask it, and the group name you suggest. Say the cost: each peer's first brief is a large research task (a few hundred thousand tokens), and its clone lives under `~/research/<group>/`. Ask which to add. **Register nothing and clone nothing under `~/research` before the user's yes**: step 4's scratch clones are for reading only (delete them once you've ranked).
7. **On the yes,** register each with mesh (needs agent-mesh installed; `peers add` itself needs no herdr):

   ```bash
   mesh peers add https://github.com/<owner>/<repo> --group <group> --about "<what it is; what to ask it about>"   # at most 400 characters
   mesh peers edit <group> --about "<what the group is, whose peers they are>"                                   # a new group only
   ```

   - **Names:** each `peers add` prints the asker's `name` (`<repo>-research`, the repo part cut to 20 characters): use that name from here on, never one you built from the repo's name. Pass `--name <short>-research` when a basename is long, or when two shortlisted projects share one (the second add would fail).
   - Give no `--body` there: it would ask each peer in turn and wait for it. Instead send one brief to all of them at once, below.
   - **No herdr** (`HERDR_ENV` unset): stop here. Fill the brief from [BRIEF.md](BRIEF.md), give it to the user, and tell them to send it from a herdr session with the `mesh ask --peers` line below.

   Inside herdr:
   - **The brief's file:** under a run, not in the repo. If `mesh whoami` fails, start one with `mesh init --mode <claude | agy: your own CLI> --request "find peers for <repo>"`. Write `$(mesh dir)/tasks/peers-brief.md` from [BRIEF.md](BRIEF.md) (next to this file): only what is below its `---` line, with the repo's mechanics and a Focus line per peer.
   - **Ask:** `mesh ask --peers <name>,<name>,… --size large --timeout 3600 --file "$(mesh dir)/tasks/peers-brief.md"`. Claude Code: run it with `run_in_background`; agy: in the foreground. A first brief takes a peer 10 to 20 minutes; the default 30-minute timeout is too tight for a slow one, and a peer given up on answers later in your inbox instead of the file.
   - **The answers** come in **one file**, whose path the command prints: never read N inboxes. A line that says a peer didn't answer says what to do (see "Standing peer askers" in `~/.config/agent-mesh/ORCHESTRATE.md`).
8. **Report:** a ranked summary of what the peers found, across all of them, with each peer's name so the user can ask it again (`mesh ask <name> --body …`, or `--peers <group>`). Where an answer shows an `about` was wrong, fix it with `mesh peers edit <name> --about …`. Offer to note the group in the repo's `CLAUDE.md` / `AGENTS.md` (which peers exist and what to ask each), so later sessions ask them before designing; write it only on a yes.

Without agent-mesh, do steps 1 to 6 and give the shortlist with the clone commands: the search and the ranking are the part that works anywhere.
