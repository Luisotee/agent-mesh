---
name: delegate
description: >-
  agent-mesh (daemon): hands work to agents, supervised by a root agent the daemon runs: each repo's part of a change, a task list, or a question about
  another repo; large tasks get their plan reviewed first, each PR gets self-review and outside reviewers, and every
  agent's model and effort is a tier the root picks per task. Use it, typed or not, when the user wants work or a question handed to agents,
  in this repo or others ("have Claude do it", "delegate this", "ask the shop-api agent"), or when you judge workers
  would do it better (several repos, a task list, parallel work, a long job you would otherwise do inline). Claude is
  the default CLI; agy when the request names it (agy, Gemini, Hydrogen, Flash), and the root picks each worker's CLI (agy and OpenCode's free models included) by the work. Not for a PR review the user asks
  for on its own (`delegate-review`), or for a mesh agent (your task came by a `📬 [mesh]` doorbell).
argument-hint: <what to build, fix or ask, in which repos, or a task list>
---

# delegate

Run an **agent-mesh** for the user's request: `$ARGUMENTS` when the user typed the command, otherwise the request that brought you here. You are the user's own session, not an agent of the mesh: you start the run and its **root**, and the user follows it in the app. You do not supervise, and you do not do the work yourself.

1. **The daemon:** `mesh hello` must answer (it names the daemon and who you are to it). If it doesn't, tell the user the daemon isn't running (`systemctl --user status agent-mesh.service`, or `./install.sh` in the agent-mesh checkout) and stop.
2. **Start the run:** `mesh start-run --title "<one-line summary of the request>"`. Its output has the run id; `mesh dir --run <run id>` prints its folder.
3. **Write the request** to `<run folder>/tasks/request.md`, for the root to read: the request in the user's own words, what you know that it needs (the repos and where they are, the contract between them, what you already tried, the files that matter), what is out of scope, any standing instruction ("run autonomously", "no Claude"), and the CLI to use for the root (`claude` unless the request names agy; the root picks each worker's CLI by "Routing" unless the request names one). Say what you know and what you only assume.
4. **Start the root,** in this repo's checkout (or the repo the request is mostly about):

   ```bash
   mesh spawn --role root --run-id <run id> --kind <claude|agy> --repo <path> --slug lead \
     --task "You are this run's root. The user's request is in <run folder>/tasks/request.md: read it, then follow ORCHESTRATE.md (Root) from step 1 (`mesh protocol root.1`, and so on: the card says how)."
   ```

   A root runs on the CLI's own default model; add `--tier` or `--model` only if the user named one. The root is `<repo>-lead`.
   **Or be the root yourself (only when the user asks to run it from this session: "from here", "in this session"; Claude Code only).** Instead of this step, run `mesh adopt --run-id <run id>`: it makes this session the run's root in place. Your `mesh` calls then act as the root, the workers' reports arrive here as `📬 [mesh]` doorbells, and the user keeps talking to you. It needs `"crossSessionInbound": "accept"` in the user's own Claude settings: when `mesh adopt` refuses, tell the user the line to add (mesh never edits their settings) and spawn the daemon root as above. Once adopted you are the root: read the card (`mesh protocol root-card`) and the request file you wrote, then follow `mesh protocol root.1` onward; steps 5 and 6 don't apply. The spawned root stays the default: it starts lean and can reset itself, while this session carries its whole conversation. Commands meant for the user (`mesh stop-run`, `mesh pair`) are denied to the root: the user runs those in another terminal.
5. **Tell the user** in one line: the run id and the root's name, and that the root proposes the work (sizes, tiers, repos) and waits for their go in the app (`mesh waiting` lists what waits on them). To watch an agent in a terminal instead, they run `mesh open <root name>` (`Ctrl-]` detaches). They can answer there, or tell the root by `mesh send <root name> --type answer --body "…"`. If they asked to run autonomously, the request file says so and the root starts at once.
6. **Then stop.** Don't spawn workers, review PRs or poll: the root and the daemon do that, and tell the user.

Read `~/.config/agent-mesh/ORCHESTRATE.md` ("Root") only if the user asks how the run works; it is the root's file, not yours.
