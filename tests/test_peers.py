"""Standing peer askers: the registry, the upstream note, and `mesh ask` to one peer or many (design §14)."""
import json
import subprocess

from test_mesh import SETTINGS_WARNING, WELCOME, repo, stub_new_pane  # noqa: F401 (repo is a fixture)


def git(*args, cwd):
    return subprocess.run(["git", *args], cwd=cwd, check=True, capture_output=True, text=True).stdout.strip()


def commit(repo, message, file="notes.md"):
    (repo / file).write_text(message + "\n")
    git("add", file, cwd=repo)
    git("commit", "-q", "-m", message, cwd=repo)
    return git("rev-parse", "HEAD", cwd=repo)


def upstream_and_clone(tmp_path, name="swarm"):
    """A project upstream and our clone of it, which follows origin/main."""
    up = tmp_path / "upstream" / name
    up.mkdir(parents=True)
    for cmd in (["init", "-q", "-b", "main"], ["config", "user.email", "t@t"], ["config", "user.name", "t"]):
        git(*cmd, cwd=up)
    commit(up, "first")
    clone = tmp_path / "research" / name
    clone.parent.mkdir(parents=True, exist_ok=True)
    git("clone", "-q", str(up), str(clone), cwd=tmp_path)
    return up, clone


def real_fetch(cli):
    """`git fetch` runs through run_cli, which tests fake: let it really fetch from the local upstream."""
    cli.on("fetch", effect=lambda argv: subprocess.run(argv, cwd=cli.calls[-1]["cwd"], check=True,
                                                       capture_output=True))


def archived_peer_worker(mesh, clone, name, session, run_id="mold", base=None):
    """A past Claude asker of `clone`, as run mac82 left them: worktree removed, branch deleted."""
    path = mesh.worktree_root() / run_id / name
    entry = {"role": "worker", "kind": "claude", "model": "opus", "effort": "high", "repo": str(clone),
             "cwd": str(path), "branch": f"mesh/{run_id}/research", "base": base or git("rev-parse", "HEAD", cwd=clone),
             "session": session, "pane": "w7:p1", "parent": "old-lead", "status": "retired", "view": "pane"}
    folder = mesh.mesh_home() / "archive" / run_id
    (folder / "inbox" / "old-lead" / "read").mkdir(parents=True, exist_ok=True)
    data = {"id": run_id, "created": "2026-01-01T00:00:00+00:00", "mode": "claude", "root": "old-lead",
            "agents": {"old-lead": {"role": "root", "pane": "w7:p0"}}, "worktrees": []}
    if (folder / "run.json").exists():
        data = json.loads((folder / "run.json").read_text())
    data["agents"][name] = entry
    data["worktrees"].append({"path": str(path), "repo": str(clone), "branch": entry["branch"]})
    (folder / "run.json").write_text(json.dumps(data))
    return path


def answer_all_on_sleep(mesh, monkeypatch, run, answers, mtype="done"):
    """Each named worker answers the first time anything waits once it is in the run."""
    def sleep(_):
        for worker, body in answers.items():
            entry = mesh.load_run(run)["agents"].get(worker)
            if entry and entry.get("status") != "retired" and not mesh.sends_from(run, worker, mesh.REPORT_TYPES):
                mesh.deliver(run, worker, "repo-lead", mtype, body)

    monkeypatch.setattr(mesh.time, "sleep", sleep)


def test_add_with_a_missing_brief_registers_nothing(mesh, fake, repo, tmp_path, monkeypatch, capsys):
    _, clone = upstream_and_clone(tmp_path)
    monkeypatch.chdir(repo)
    assert mesh.main(["peers", "add", str(clone), "--file", str(tmp_path / "nope.md")]) == 1
    assert "can't read" in capsys.readouterr().err
    assert not mesh.load_peers()["peers"]
    assert not (mesh.worktree_root() / "peers").exists()


def test_add_registers_a_peer_that_any_repo_sees(mesh, fake, repo, tmp_path, monkeypatch, capsys):
    _, clone = upstream_and_clone(tmp_path)
    monkeypatch.chdir(repo)
    assert mesh.main(["peers", "add", str(clone), "--group", "orch"]) == 0
    capsys.readouterr()
    peer = mesh.load_peers()["peers"]["swarm-research"]
    assert peer["group"] == "orch" and peer["kind"] == "claude"
    assert peer["upstream"]["remote"] == "origin" and peer["upstream"]["branch"] == "main"
    cwd = mesh.worktree_root() / "peers" / "swarm-research"
    assert peer["cwd"] == str(cwd) and peer["seen"] == git("rev-parse", "HEAD", cwd=clone)
    assert git("rev-parse", "HEAD", cwd=cwd) == peer["seen"]
    assert subprocess.run(["git", "symbolic-ref", "-q", "HEAD"], cwd=cwd).returncode == 1  # detached: no branch

    monkeypatch.chdir(tmp_path)  # the registry is machine-wide
    assert mesh.main(["peers"]) == 0
    out = capsys.readouterr().out
    assert "swarm-research" in out and "origin/main" in out and "none yet" in out
    assert mesh.main(["peers", "add", str(clone)]) == 1  # already a peer
    assert "a peer already" in capsys.readouterr().err

    assert mesh.main(["peers", "remove", "swarm-research"]) == 0
    assert not cwd.exists() and clone.exists() and not mesh.load_peers()["peers"]
    assert mesh.main(["peers"]) == 0
    assert "No peers yet" in capsys.readouterr().out


def test_add_clones_a_url_into_its_group(mesh, fake, cli, tmp_path, monkeypatch, capsys):
    up, _ = upstream_and_clone(tmp_path, "town")
    monkeypatch.setenv("MESH_PEERS_DIR", str(tmp_path / "peers"))
    cli.on("clone", effect=lambda argv: subprocess.run(argv, check=True, capture_output=True))
    url = f"file://{up}.git"
    (up.parent / "town.git").symlink_to(up)
    assert mesh.main(["peers", "add", url, "--group", "orch"]) == 0
    assert (tmp_path / "peers" / "orch" / "town" / ".git").exists()
    assert "town-research" in mesh.load_peers()["peers"]


def test_add_adopts_a_past_asker_with_its_conversation(mesh, fake, repo, tmp_path, monkeypatch, capsys):
    up, clone = upstream_and_clone(tmp_path)
    old = git("rev-parse", "HEAD", cwd=clone)
    path = archived_peer_worker(mesh, clone, "swarm-research", "s1", base=old)
    monkeypatch.chdir(repo)
    assert mesh.main(["peers", "add", str(clone)]) == 0
    assert json.loads(capsys.readouterr().out)["adopted"] is True
    peer = mesh.load_peers()["peers"]["swarm-research"]
    # the path Claude keys its session by, back at the commit it researched
    assert peer["cwd"] == str(path) and peer["seen"] == old and git("rev-parse", "HEAD", cwd=path) == old


def test_the_upstream_note_says_what_changed_and_moves_a_clean_checkout(mesh, fake, cli, repo, tmp_path,
                                                                         monkeypatch, capsys):
    up, clone = upstream_and_clone(tmp_path)
    monkeypatch.chdir(repo)
    assert mesh.main(["peers", "add", str(clone)]) == 0
    real_fetch(cli)
    peer = mesh.load_peers()["peers"]["swarm-research"]
    prep = mesh.prepare_peer("swarm-research", peer)
    assert "hasn't changed since you last looked" in prep["note"] and prep["new"] == 0

    new = commit(up, "Add the merge queue")
    prep = mesh.prepare_peer("swarm-research", peer)
    assert prep["new"] == 1 and prep["upstream"] == new and prep["head"] == new
    assert "Upstream moved since you last looked: 1 commit on origin/main" in prep["note"]
    assert "Add the merge queue" in prep["note"] and "1 file changed" in prep["note"]
    assert git("rev-parse", "HEAD", cwd=peer["cwd"]) == new
    assert git("rev-parse", "HEAD", cwd=clone) != new  # the clone's own checkout is never touched

    (mesh.Path(peer["cwd"]) / "scratch.txt").write_text("left by a test run\n")
    newer = commit(up, "Rename the refinery")
    prep = mesh.prepare_peer("swarm-research", {**peer, "seen": new})
    assert prep["head"] == new and prep["upstream"] == newer
    assert f"has local changes, so it stays at {new[:9]}" in prep["note"]
    assert git("rev-parse", "HEAD", cwd=peer["cwd"]) == new

    cli.on("fetch", returncode=1, stderr="fatal: unable to access")
    prep = mesh.prepare_peer("swarm-research", {**peer, "seen": newer})
    assert "Couldn't fetch" in prep["note"] and "fatal: unable to access" in prep["note"]

    assert mesh.main(["peers", "refresh"]) == 0  # a failed fetch is reported, not raised
    assert "fetch failed" in capsys.readouterr().out


def test_asking_a_peer_tells_it_the_upstream_and_leaves_its_worktree_to_outlive_the_run(
        mesh, fake, cli, run, repo, tmp_path, monkeypatch, capsys):
    up, clone = upstream_and_clone(tmp_path)
    path = archived_peer_worker(mesh, clone, "swarm-research", "s1")
    monkeypatch.chdir(repo)
    assert mesh.main(["peers", "add", str(clone)]) == 0
    real_fetch(cli)
    new = commit(up, "Add the merge queue")
    stub_new_pane(fake)
    fake.on("workspace", "create", result={"workspace": {"workspace_id": "w9"}})
    fake.on("agent", "get", result={"agent": {"agent_status": "working"}})
    answer_all_on_sleep(mesh, monkeypatch, run, {"swarm-research": "It merges in a refinery."})
    capsys.readouterr()
    assert mesh.main(["ask", "swarm-research", "--body", "How do they merge?", "--wait"]) == 0
    out = capsys.readouterr().out
    assert "It merges in a refinery." in out and "swarm-research is closed" in out
    [task] = mesh.inbox_dir(run, "swarm-research").glob("**/*-task.md")
    text = mesh.parse_message(task)[1]
    assert text.startswith(mesh.ASK_PREFIX) and "How do they merge?" in text and "Add the merge queue" in text
    data = mesh.load_run(run)
    entry = data["agents"]["swarm-research"]
    assert entry["peer"] == "swarm-research" and entry["branch"] is None and entry["read_only"] is True
    assert not any(w["path"] == str(path) for w in data.get("worktrees", []))  # `mesh stop` leaves it
    assert "--resume" in fake.called("agent", "start")[-1] and "s1" in fake.called("agent", "start")[-1]
    peer = mesh.load_peers()["peers"]["swarm-research"]
    assert (peer["seen"], peer["run"], peer["as"]) == (new, run, "swarm-research")
    assert git("rev-parse", "HEAD", cwd=path) == new

    # a change goes to a worker in that repo, not to the asker
    assert mesh.main(["resume", "swarm-research", "--task", "Direct task (no plan)\n\nFix it."]) == 1
    assert "only answers questions" in capsys.readouterr().err
    assert mesh.main(["ask", "swarm-research", "--review", "https://github.com/a/b/pull/1"]) == 1


def test_a_peer_compacts_at_the_plain_threshold_with_its_own_focus(mesh):
    claude = {"kind": "claude"}
    assert not mesh.should_compact("auto", claude, 400_000, True, True)
    assert mesh.should_compact("auto", claude, 400_000, True, True, peer=True)
    assert not mesh.should_compact("auto", claude, 200_000, True, True, peer=True)
    assert not mesh.should_compact("auto", {"kind": "agy"}, None, True, True, peer=True)  # no size: as a question


def test_a_peer_gets_past_a_settings_warning_on_continue_and_others_hold(mesh, fake):
    fake.on("agent", "get", result={"agent": {"agent_status": "idle"}})
    fake.screen("w9:p10", SETTINGS_WARNING, WELCOME)
    assert mesh.clear_startup("swarm-research", "w9:p10", peer=True) == {}
    assert [c[3] for c in fake.called("pane", "send-keys")] == ["enter"]

    fake.screen("w9:p11", SETTINGS_WARNING)
    assert mesh.clear_startup("proj-fix", "w9:p11")["held"] == "Settings Warning"
    # a cursor moved off "Continue" is the user's choice: held too
    moved = SETTINGS_WARNING.replace("❯ 1. Continue", "  1. Continue").replace("    3. Exit", "  ❯ 3. Exit")
    fake.screen("w9:p12", moved)
    assert mesh.clear_startup("swarm-research", "w9:p12", peer=True)["held"] == "Settings Warning"
    assert len(fake.called("pane", "send-keys")) == 1


def test_a_collected_report_is_stored_without_a_doorbell(mesh, fake, run):
    with mesh.editing_run(run) as data:
        data["agents"]["repo-fix-typo"]["collect"] = mesh.collector("repo-lead")
    out = mesh.deliver(run, "repo-fix-typo", "repo-lead", "done", "Answer.")
    assert out["collected"] is True and mesh.delivered(out) and not fake.called("agent", "prompt")
    assert mesh.unread(run, "repo-lead")
    # a question still rings: the root has to answer it
    mesh.deliver(run, "repo-fix-typo", "repo-lead", "question", "Which branch?")
    assert fake.called("agent", "prompt")


def test_a_dead_fan_outs_mark_is_ignored(mesh, fake, run):
    with mesh.editing_run(run) as data:  # a SIGKILLed `ask --peers`: its pid is gone
        data["agents"]["repo-fix-typo"]["collect"] = {"to": "repo-lead", "pid": 2 ** 22 + 9, "start": "1"}
    out = mesh.deliver(run, "repo-fix-typo", "repo-lead", "done", "Answer.")
    assert not out.get("collected") and fake.called("agent", "prompt")


def peer_in_run(mesh, fake, cli, repo, tmp_path, monkeypatch):
    up, clone = upstream_and_clone(tmp_path)
    archived_peer_worker(mesh, clone, "swarm-research", "s1")
    monkeypatch.chdir(repo)
    assert mesh.main(["peers", "add", str(clone)]) == 0
    stub_new_pane(fake)
    fake.on("workspace", "create", result={"workspace": {"workspace_id": "w9"}})
    fake.on("agent", "get", result={"agent": {"agent_status": "working"}})
    return up, clone


def test_ask_peers_skips_a_peer_still_on_its_previous_question(mesh, fake, cli, run, repo, tmp_path,
                                                                 monkeypatch, capsys):
    peer_in_run(mesh, fake, cli, repo, tmp_path, monkeypatch)
    assert mesh.main(["ask", "--peers", "all", "--body", "Q1?", "--no-refresh", "--timeout", "1"]) == 6
    capsys.readouterr()

    def sleep(_):  # its answer to Q1 lands while Q2 runs
        if not mesh.sends_from(run, "swarm-research", mesh.FINAL_TYPES):
            mesh.deliver(run, "swarm-research", "repo-lead", "done", "ANSWER TO Q1")

    monkeypatch.setattr(mesh.time, "sleep", sleep)
    assert mesh.main(["ask", "--peers", "all", "--body", "Q2?", "--no-refresh", "--timeout", "5"]) == 1
    out = capsys.readouterr().out
    assert "busy with its previous question; ask again once it answers" in out
    text = mesh.Path(out.splitlines()[-2].removeprefix("answers: ")).read_text()
    assert "ANSWER TO Q1" not in text and "busy with its previous question" in text
    tasks = list(mesh.inbox_dir(run, "swarm-research").glob("**/*-task.md"))
    assert len(tasks) == 1  # Q2 never went out


def test_an_interrupted_fan_out_clears_its_marks(mesh, fake, cli, run, repo, tmp_path, monkeypatch):
    peer_in_run(mesh, fake, cli, repo, tmp_path, monkeypatch)

    def sleep(_):
        raise KeyboardInterrupt

    monkeypatch.setattr(mesh.time, "sleep", sleep)
    try:
        mesh.main(["ask", "--peers", "all", "--body", "Q1?", "--no-refresh"])
    except KeyboardInterrupt:
        pass
    assert "collect" not in mesh.load_run(run)["agents"]["swarm-research"]
    assert not mesh.deliver(run, "swarm-research", "repo-lead", "done", "late").get("collected")


def test_a_single_ask_clears_a_stale_mark(mesh, fake, cli, run, repo, tmp_path, monkeypatch, capsys):
    peer_in_run(mesh, fake, cli, repo, tmp_path, monkeypatch)
    assert mesh.main(["ask", "swarm-research", "--body", "Q1?", "--no-refresh", "--no-wait"]) == 0
    mesh.deliver(run, "swarm-research", "repo-lead", "done", "A1")
    fake.on("agent", "get", result={"agent": {"agent_status": "idle"}})
    with mesh.editing_run(run) as data:  # a live fan-out's mark: a single ask leaves it and refuses
        data["agents"]["swarm-research"]["collect"] = mesh.collector("repo-lead")
    capsys.readouterr()
    assert mesh.main(["ask", "swarm-research", "--body", "Q2?", "--no-refresh", "--no-wait"]) == 1
    assert "is answering a `mesh ask --peers`" in capsys.readouterr().err
    assert mesh.load_run(run)["agents"]["swarm-research"]["collect"]["to"] == "repo-lead"
    with mesh.editing_run(run) as data:  # a killed one's: cleared, so the answer rings
        data["agents"]["swarm-research"]["collect"] = {"to": "repo-lead", "pid": 2 ** 22 + 9, "start": "1"}
    assert mesh.main(["ask", "swarm-research", "--body", "Q2?", "--no-refresh", "--no-wait"]) == 0
    assert "collect" not in mesh.load_run(run)["agents"]["swarm-research"]


def test_a_peer_cant_be_given_a_change_by_send_or_as_a_renamed_copy(mesh, fake, cli, run, repo, tmp_path,
                                                                    monkeypatch, capsys):
    peer_in_run(mesh, fake, cli, repo, tmp_path, monkeypatch)
    assert mesh.main(["ask", "swarm-research", "--body", "Q1?", "--no-refresh", "--no-wait"]) == 0
    fake.on("agent", "get", result={"agent": {"agent_status": "idle"}})
    capsys.readouterr()
    assert mesh.main(["send", "swarm-research", "--type", "task", "--body", "Direct task (no plan)\n\nFix it."]) == 1
    assert "only answers questions" in capsys.readouterr().err
    assert mesh.load_run(run)["agents"]["swarm-research"]["read_only"] is True
    with mesh.editing_run(run) as data:  # a copy resumed under a -2 name, in another run
        entry = {**data["agents"].pop("swarm-research"), "status": "retired"}
        data["agents"]["swarm-research-2"] = entry
    assert mesh.main(["resume", "swarm-research-2", "--task", "Direct task (no plan)\n\nFix it."]) == 1
    assert "only answers questions" in capsys.readouterr().err


def test_an_asker_that_ran_in_the_clone_itself_never_moves_it(mesh, fake, cli, repo, tmp_path, monkeypatch, capsys):
    up, clone = upstream_and_clone(tmp_path)
    archived_peer_worker(mesh, clone, "swarm-research", "s1")
    f = mesh.mesh_home() / "archive" / "mold" / "run.json"
    data = json.loads(f.read_text())
    data["agents"]["swarm-research"].update(cwd=str(clone), branch=None)
    data["worktrees"] = []
    f.write_text(json.dumps(data))
    monkeypatch.chdir(repo)
    assert mesh.main(["peers", "add", str(clone)]) == 0
    peer = mesh.load_peers()["peers"]["swarm-research"]
    assert peer["in_clone"] is True and peer["cwd"] == str(clone)
    real_fetch(cli)
    old, new = git("rev-parse", "HEAD", cwd=clone), commit(up, "new")
    prep = mesh.prepare_peer("swarm-research", peer)
    assert git("symbolic-ref", "HEAD", cwd=clone) == "refs/heads/main" and git("rev-parse", "HEAD", cwd=clone) == old
    assert "clone's own checkout, which stays at" in prep["note"] and f"git show {new[:9]}" in prep["note"]
    capsys.readouterr()
    assert mesh.main(["peers", "remove", "swarm-research"]) == 0
    assert "it is the clone's own checkout" in capsys.readouterr().out and clone.exists()


def test_adopting_from_a_live_run_takes_the_worktree_off_its_stop_list(mesh, fake, run, repo, tmp_path,
                                                                       monkeypatch, capsys):
    _, clone = upstream_and_clone(tmp_path)
    path = mesh.worktree_root() / run / "swarm-research"
    base = git("rev-parse", "HEAD", cwd=clone)
    git("worktree", "add", "-q", "-b", "mesh/mtest/research", str(path), base, cwd=clone)
    with mesh.editing_run(run) as data:
        data["agents"]["swarm-research"] = {"role": "worker", "kind": "claude", "repo": str(clone), "cwd": str(path),
                                            "base": base, "status": "running"}
        data.setdefault("worktrees", []).append({"path": str(path), "repo": str(clone),
                                                 "branch": "mesh/mtest/research", "base": base})
    monkeypatch.chdir(repo)
    assert mesh.main(["peers", "add", str(clone)]) == 1  # still open: retire it first
    assert "`mesh retire swarm-research` there first" in capsys.readouterr().err
    assert not mesh.load_peers()["peers"] and mesh.load_run(run)["worktrees"]
    with mesh.editing_run(run) as data:
        data["agents"]["swarm-research"]["status"] = "retired"
    assert mesh.main(["peers", "add", str(clone)]) == 0
    assert not any(w["path"] == str(path) for w in mesh.load_run(run)["worktrees"])
    # its untouched run branch goes, as `mesh stop` would have deleted it; the worktree is detached at the same commit
    assert not git("branch", "--list", "mesh/mtest/research", cwd=clone)
    assert git("rev-parse", "HEAD", cwd=path) == base


def test_ask_peers_skips_a_peer_that_owes_a_report_by_the_log(mesh, fake, cli, run, repo, tmp_path, monkeypatch,
                                                               capsys):
    peer_in_run(mesh, fake, cli, repo, tmp_path, monkeypatch)
    assert mesh.main(["ask", "swarm-research", "--body", "Q1?", "--no-refresh", "--no-wait"]) == 0
    fake.on("agent", "get", result={"agent": {"agent_status": "idle"}})  # a blip: it reads idle mid-turn
    capsys.readouterr()
    assert mesh.main(["ask", "--peers", "all", "--body", "Q2?", "--no-refresh"]) == 1
    assert "busy with its previous question" in capsys.readouterr().out


def test_ask_peers_reads_the_log_and_the_status_once_per_peer(mesh, fake, cli, run, repo, tmp_path, monkeypatch,
                                                                capsys):
    peer_in_run(mesh, fake, cli, repo, tmp_path, monkeypatch)
    assert mesh.main(["ask", "swarm-research", "--body", "Q1?", "--no-refresh", "--no-wait"]) == 0
    mesh.deliver(run, "swarm-research", "repo-lead", "done", "ANSWER TO Q1")
    fake.on("agent", "get", result={"agent": {"agent_status": "idle"}})
    got = {}
    owes, reach = mesh.owes_answer, mesh.reach_worker

    def owes_spy(run_id, name, entry, events=None, state=None):
        got["owes"] = events, state
        return owes(run_id, name, entry, events, state)

    def reach_spy(*a, events=None, state=None, **kw):
        got["reach"] = events, state
        return reach(*a, events=events, state=state, **kw)

    statuses = []
    status = mesh.asker_status
    monkeypatch.setattr(mesh, "asker_status", lambda *a: statuses.append(a[0]) or status(*a))

    monkeypatch.setattr(mesh, "owes_answer", owes_spy)
    monkeypatch.setattr(mesh, "reach_worker", reach_spy)
    assert mesh.main(["ask", "--peers", "all", "--body", "Q2?", "--no-refresh", "--timeout", "1"]) in (0, 6)
    (events, state), (events2, state2) = got["owes"], got["reach"]
    assert events is events2 and any(e.get("type") == "done" for e in events)
    assert state == state2 == "idle" and statuses == ["swarm-research"]


def test_same_url_takes_the_two_github_forms_as_one(mesh):
    assert mesh.same_url("git@github.com:Acme/swarm.git", "https://github.com/acme/swarm")
    assert mesh.same_url("https://example.com/a/b.git/", "https://example.com/a/b")
    assert not mesh.same_url("https://github.com/acme/swarm", "https://github.com/other/swarm")
    assert not mesh.same_url(None, "https://github.com/acme/swarm")


def test_add_refuses_a_url_whose_clone_path_holds_another_repo(mesh, fake, cli, tmp_path, monkeypatch, capsys):
    _, clone = upstream_and_clone(tmp_path, "swarm")
    monkeypatch.setenv("MESH_PEERS_DIR", str(tmp_path))
    assert clone == tmp_path / "research" / "swarm"
    assert mesh.main(["peers", "add", "https://github.com/other/swarm.git", "--group", "research"]) == 1
    err = capsys.readouterr().err
    assert "is already a clone of" in err and "another --group" in err
    assert not cli.called("clone") and not mesh.load_peers()["peers"]
    origin = git("remote", "get-url", "origin", cwd=clone)
    assert mesh.main(["peers", "add", origin + ".git", "--group", "research"]) == 0  # the same repo: reused
    assert not cli.called("clone")


def test_a_worker_asking_a_peer_moves_nothing(mesh, fake, cli, run, repo, tmp_path, monkeypatch, capsys):
    peer_in_run(mesh, fake, cli, repo, tmp_path, monkeypatch)
    monkeypatch.setenv("MESH_RUN", run)
    monkeypatch.setenv("MESH_SELF", "repo-fix-typo")
    assert mesh.main(["ask", "swarm-research", "--body", "Q?"]) == 1
    assert "only the root asks" in capsys.readouterr().err
    assert not cli.called("fetch")


def test_ask_peers_fans_out_and_collects_one_file(mesh, fake, cli, run, repo, tmp_path, monkeypatch, capsys):
    clones = {}
    for project in ("swarm", "town"):
        _, clones[project] = upstream_and_clone(tmp_path, project)
        archived_peer_worker(mesh, clones[project], f"{project}-research", f"s-{project}")
    _, lone = upstream_and_clone(tmp_path, "lone")
    monkeypatch.chdir(repo)
    for project in ("swarm", "town"):
        assert mesh.main(["peers", "add", str(clones[project]), "--group", "orch"]) == 0
    assert mesh.main(["peers", "add", str(lone)]) == 0  # in the default group: not asked below
    real_fetch(cli)
    stub_new_pane(fake)
    fake.on("workspace", "create", result={"workspace": {"workspace_id": "w9"}})
    fake.on("agent", "get", result={"agent": {"agent_status": "working"}})
    answer_all_on_sleep(mesh, monkeypatch, run, {"swarm-research": "Swarm keeps it in a ledger.",
                                                  "town-research": "Town keeps a patrol."})
    capsys.readouterr()
    assert mesh.main(["ask", "--peers", "orch", "--body", "How do they track state?"]) == 0
    out = capsys.readouterr().out
    assert out.startswith("2 of 2 peers answered: How do they track state?")
    assert "swarm-research" in out and "town-research" in out and "lone-research" not in out
    assert len(out.splitlines()) == 4  # one line per peer and the file: bounded, whatever the answers' size
    path = mesh.Path(out.splitlines()[-1].removeprefix("answers: "))
    text = path.read_text()
    assert "## swarm-research" in text and "Swarm keeps it in a ledger." in text and "Town keeps a patrol." in text
    data = mesh.load_run(run)
    assert all(data["agents"][n]["status"] == "retired" and "collect" not in data["agents"][n]
               for n in ("swarm-research", "town-research"))
    events = mesh.read_log(run)
    assert sum(e["event"] == "collected" for e in events) == 2  # no doorbell rang the root for them
    assert not mesh.unread(run, "repo-lead")
    assert mesh.load_peers()["answers"] == str(path)
    assert mesh.main(["peers"]) == 0
    assert f"newest answers: {path}" in capsys.readouterr().out


def test_ask_peers_reports_a_peer_that_asks_back_instead(mesh, fake, cli, run, repo, tmp_path, monkeypatch, capsys):
    _, clone = upstream_and_clone(tmp_path)
    archived_peer_worker(mesh, clone, "swarm-research", "s1")
    monkeypatch.chdir(repo)
    assert mesh.main(["peers", "add", str(clone)]) == 0
    stub_new_pane(fake)
    fake.on("workspace", "create", result={"workspace": {"workspace_id": "w9"}})
    fake.on("agent", "get", result={"agent": {"agent_status": "working"}})
    answer_all_on_sleep(mesh, monkeypatch, run, {"swarm-research": "Which version?"}, mtype="question")
    capsys.readouterr()
    assert mesh.main(["ask", "--peers", "all", "--body", "How?", "--no-refresh"]) == 3
    captured = capsys.readouterr()
    assert captured.out.startswith("0 of 1 peers answered") and "sent a question instead" in captured.out
    assert "sent a question instead" in captured.err  # said as soon as it's known
    assert mesh.load_run(run)["agents"]["swarm-research"]["status"] != "retired"
    assert not cli.called("fetch")


def test_ask_peers_needs_peers_and_no_other_target(mesh, fake, run, repo, capsys):
    assert mesh.main(["ask", "--peers", "all", "--body", "How?"]) == 1
    assert "no peers yet" in capsys.readouterr().err
    assert mesh.main(["ask", "--peers", "all", "--repo", str(repo), "--body", "How?"]) == 1
    assert "drop the worker, --repo" in capsys.readouterr().err


def test_peers_and_groups_say_what_they_are(mesh, fake, cli, repo, tmp_path, monkeypatch, capsys):
    _, swarm = upstream_and_clone(tmp_path, "swarm")
    _, town = upstream_and_clone(tmp_path, "town")
    git("remote", "set-url", "origin", "https://github.com/acme/town.git", cwd=town)
    cli.on("repo", "view", stdout=json.dumps({"description": "The  best\norchestrator ever"}))
    monkeypatch.chdir(repo)
    assert mesh.main(["peers", "add", str(swarm), "--group", "orch", "--about", "A Rust daemon.\nAsk about delivery."]) == 0
    assert mesh.main(["peers", "add", str(town), "--group", "orch"]) == 0
    peers = mesh.load_peers()["peers"]
    assert peers["swarm-research"]["about"] == "A Rust daemon. Ask about delivery."
    assert "about_from" not in peers["swarm-research"]  # swarm's upstream isn't on GitHub: gh isn't asked
    assert (peers["town-research"]["about"], peers["town-research"]["about_from"]) == ("The best orchestrator ever",
                                                                                        "github")
    assert cli.called("repo", "view", "acme/town", "--json", "description")

    assert mesh.main(["peers", "edit", "town-research", "--about", "Dropped for Gas City."]) == 0
    assert mesh.main(["peers", "edit", "orch", "--about", "Tools like agent-mesh."]) == 0
    data = mesh.load_peers()
    assert data["peers"]["town-research"]["about"] == "Dropped for Gas City."
    assert "about_from" not in data["peers"]["town-research"]
    assert data["groups"]["orch"]["about"] == "Tools like agent-mesh."
    capsys.readouterr()
    assert mesh.main(["peers", "edit", "orch", "--about", "x" * 401]) == 1
    assert "keep it to 400" in capsys.readouterr().err
    assert mesh.main(["peers", "edit", "nobody", "--about", "x"]) == 1
    assert mesh.main(["peers", "edit", "orch", "--group", "other"]) == 1

    assert mesh.main(["peers"]) == 0
    out = capsys.readouterr().out
    assert out.startswith("orch: Tools like agent-mesh.\n")
    assert "About:\n  swarm-research: A Rust daemon. Ask about delivery.\n  town-research: Dropped for Gas City." in out
    assert mesh.main(["peers", "--json"]) == 0
    listed = json.loads(capsys.readouterr().out)
    assert listed["groups"] == {"orch": {"about": "Tools like agent-mesh."}}
    assert {r["name"]: r["about"] for r in listed["peers"]}["swarm-research"] == "A Rust daemon. Ask about delivery."

    assert mesh.main(["peers", "edit", "swarm-research", "--group", "later"]) == 0
    assert mesh.load_peers()["peers"]["swarm-research"]["group"] == "later"


def test_the_answers_file_says_who_answered(mesh, fake, cli, run, repo, tmp_path, monkeypatch, capsys):
    peer_in_run(mesh, fake, cli, repo, tmp_path, monkeypatch)
    assert mesh.main(["peers", "edit", "swarm-research", "--about", "A Rust daemon."]) == 0
    assert mesh.main(["peers", "edit", "peers", "--about", "Tools like agent-mesh."]) == 0
    answer_all_on_sleep(mesh, monkeypatch, run, {"swarm-research": "In a ledger."})
    capsys.readouterr()
    assert mesh.main(["ask", "--peers", "all", "--body", "How?", "--no-refresh"]) == 0
    text = mesh.Path(capsys.readouterr().out.splitlines()[-1].removeprefix("answers: ")).read_text()
    assert "\npeers: Tools like agent-mesh.\n" in text and "## swarm-research\n\n*swarm at" in text
    assert "*A Rust daemon.*" in text


def test_mesh_peers_lists_a_broken_entry_instead_of_failing(mesh, fake, repo, tmp_path, monkeypatch, capsys):
    _, clone = upstream_and_clone(tmp_path)
    monkeypatch.chdir(repo)
    assert mesh.main(["peers", "add", str(clone)]) == 0
    with mesh.editing_peers() as data:  # hand-edited: no cwd, a repo that's gone, not even a dict
        data["peers"]["half-peer"] = {"group": "peers", "repo": str(tmp_path / "gone"), "upstream": {}}
        data["peers"]["no-cwd-peer"] = {"group": "peers", "repo": str(clone), "upstream": {"branch": "main"}}
        data["peers"]["junk-peer"] = "oops"
    capsys.readouterr()
    assert mesh.main(["peers"]) == 0
    out = capsys.readouterr().out
    assert "swarm-research" in out and "half-peer" in out and "no-cwd-peer" in out
    assert "broken (MeshError: no cwd, upstream branch recorded)" in out and "broken (MeshError: no cwd recorded)" in out
    assert "junk-peer" in out and "broken (MeshError: not an entry but str 'oops')" in out

    # every other command copes with them too
    assert mesh.main(["peers", "refresh"]) == 0
    out = capsys.readouterr().out
    assert "half-peer: broken entry (no cwd, upstream branch recorded)" in out and "swarm-research: unchanged" in out
    run_root(mesh, fake, monkeypatch)
    assert mesh.main(["ask", "--peers", "no-cwd-peer", "--body", "Q?", "--no-refresh"]) == 1
    assert "no-cwd-peer's registry entry is broken (no cwd recorded)" in capsys.readouterr().out
    for name in ("junk-peer", "half-peer", "no-cwd-peer"):
        assert mesh.main(["peers", "remove", name]) == 0
    out = capsys.readouterr().out
    assert "junk-peer: removed its broken entry (not an entry" in out
    assert "no-cwd-peer: removed its broken entry (no cwd recorded)" in out
    assert list(mesh.load_peers()["peers"]) == ["swarm-research"]


def run_root(mesh, fake, monkeypatch):
    """A run whose root is this pane, for a test that didn't take the `run` fixture."""
    monkeypatch.setenv("HERDR_ENV", "1")
    monkeypatch.setenv("HERDR_PANE_ID", "w1:p1")
    fake.on("agent", "list", result={"agents": [{"pane_id": "w1:p1", "agent": "claude", "tab_id": "w1:t1",
                                                  "workspace_id": "w1", "cwd": "/repo"}]})
    assert mesh.main(["init", "--mode", "claude", "--run-id", "mtest"]) == 0


def test_a_refused_add_releases_nothing(mesh, fake, run, repo, tmp_path, monkeypatch, capsys):
    _, clone = upstream_and_clone(tmp_path)
    path = mesh.worktree_root() / run / "swarm-research"
    base = git("rev-parse", "HEAD", cwd=clone)
    git("worktree", "add", "-q", "-b", "mesh/mtest/research", str(path), base, cwd=clone)
    with mesh.editing_run(run) as data:
        data["agents"]["swarm-research"] = {"role": "worker", "kind": "claude", "repo": str(clone), "cwd": str(path),
                                            "base": base, "status": "retired"}
        data.setdefault("worktrees", []).append({"path": str(path), "repo": str(clone),
                                                 "branch": "mesh/mtest/research", "base": base})
    monkeypatch.chdir(repo)
    assert mesh.main(["peers", "add", str(clone), "--about", "x" * 401]) == 1
    assert mesh.load_run(run)["worktrees"] and git("branch", "--list", "mesh/mtest/research", cwd=clone)
    assert not mesh.load_peers()["peers"]


def test_a_gone_asker_doesnt_count_as_busy(mesh, fake, cli, run, repo, tmp_path, monkeypatch, capsys):
    peer_in_run(mesh, fake, cli, repo, tmp_path, monkeypatch)
    assert mesh.main(["ask", "swarm-research", "--body", "Q1?", "--no-refresh", "--no-wait"]) == 0
    entry = mesh.load_run(run)["agents"]["swarm-research"]
    fake.on("agent", "get", error="agent_not_found")  # it died with Q1 unanswered
    assert mesh.owes_answer(run, "swarm-research", entry) is False
    fake.on("agent", "get", result={"agent": {"agent_status": "idle"}})
    assert mesh.owes_answer(run, "swarm-research", entry) is True


def test_an_empty_peer_spec_is_refused(mesh, fake, run, repo, tmp_path, monkeypatch, capsys):
    _, clone = upstream_and_clone(tmp_path)
    monkeypatch.chdir(repo)
    assert mesh.main(["peers", "add", str(clone)]) == 0
    capsys.readouterr()
    assert mesh.main(["ask", "--peers", " , ", "--body", "Q?"]) == 1
    assert "which peers?" in capsys.readouterr().err
    assert mesh.main(["peers", "refresh", " "]) == 1
    assert "which peers?" in capsys.readouterr().err


def test_a_removed_peers_conversation_can_still_be_asked(mesh, fake, cli, run, repo, tmp_path, monkeypatch, capsys):
    peer_in_run(mesh, fake, cli, repo, tmp_path, monkeypatch)
    assert mesh.main(["ask", "swarm-research", "--body", "Q1?", "--no-refresh", "--no-wait"]) == 0
    with mesh.editing_run(run) as data:
        data["agents"]["swarm-research"]["status"] = "retired"
    cwd = mesh.Path(mesh.load_peers()["peers"]["swarm-research"]["cwd"])
    capsys.readouterr()
    assert mesh.main(["peers", "remove", "swarm-research"]) == 0
    assert not cwd.exists() and "`mesh ask swarm-research` finds it" in capsys.readouterr().out
    assert mesh.main(["ask", "swarm-research", "--body", "Q2?", "--no-wait"]) == 0
    assert cwd.exists() and subprocess.run(["git", "symbolic-ref", "-q", "HEAD"], cwd=cwd).returncode == 1


def test_peers_remove_refuses_a_running_peer(mesh, fake, cli, run, repo, tmp_path, monkeypatch, capsys):
    peer_in_run(mesh, fake, cli, repo, tmp_path, monkeypatch)
    assert mesh.main(["ask", "swarm-research", "--body", "Q1?", "--no-refresh", "--no-wait"]) == 0
    cwd = mesh.load_peers()["peers"]["swarm-research"]["cwd"]
    fake.on("agent", "list", result={"agents": [{"name": "swarm-research", "pane_id": "w9:p10", "cwd": cwd}]})
    capsys.readouterr()
    assert mesh.main(["peers", "remove", "swarm-research"]) == 1
    assert "is still running" in capsys.readouterr().err
    assert "swarm-research" in mesh.load_peers()["peers"] and mesh.Path(cwd).exists()


def test_a_peer_whose_clone_is_gone_fails_cleanly(mesh, fake, cli, run, repo, tmp_path, monkeypatch, capsys):
    import shutil
    _, clone = peer_in_run(mesh, fake, cli, repo, tmp_path, monkeypatch)
    shutil.rmtree(clone)
    capsys.readouterr()
    assert mesh.main(["ask", "swarm-research", "--body", "Q?"]) == 1
    assert "swarm-research's clone" in capsys.readouterr().err
    assert mesh.main(["ask", "--peers", "all", "--body", "Q?"]) == 1
    assert "is gone" in capsys.readouterr().out
    assert mesh.main(["peers", "refresh"]) == 0
    assert "fetch failed (its clone" in capsys.readouterr().out
    assert mesh.main(["peers"]) == 0
    assert "broken (MeshError: clone gone" in capsys.readouterr().out


def test_after_a_peers_answer_the_hint_points_a_change_elsewhere(mesh, fake, cli, run, repo, tmp_path, monkeypatch,
                                                                capsys):
    peer_in_run(mesh, fake, cli, repo, tmp_path, monkeypatch)
    answer_all_on_sleep(mesh, monkeypatch, run, {"swarm-research": "A."})
    assert mesh.main(["ask", "swarm-research", "--body", "Q?", "--no-refresh", "--wait"]) == 0
    out = capsys.readouterr().out
    assert "a change to that project goes to a worker there (`mesh spawn --repo" in out and "mesh resume" not in out


def test_a_remote_without_a_url_still_registers(mesh, tmp_path, monkeypatch):
    _, clone = upstream_and_clone(tmp_path)
    real = mesh.git

    def git_without_url(*args, cwd):
        if args[:2] == ("remote", "get-url"):
            raise mesh.MeshError("git remote get-url origin: error: No such remote 'origin'")
        return real(*args, cwd=cwd)

    monkeypatch.setattr(mesh, "git", git_without_url)
    up = mesh.peer_upstream(clone)
    assert up["remote"] == "origin" and up["url"] is None


def test_a_removed_fresh_peer_comes_back_at_the_code_it_last_read(mesh, fake, cli, run, repo, tmp_path, monkeypatch,
                                                                   capsys):
    up, clone = upstream_and_clone(tmp_path)
    monkeypatch.chdir(repo)
    assert mesh.main(["peers", "add", str(clone)]) == 0  # fresh: no past worker to adopt
    real_fetch(cli)
    stub_new_pane(fake)
    fake.on("workspace", "create", result={"workspace": {"workspace_id": "w9"}})
    fake.on("agent", "get", result={"agent": {"agent_status": "working"}})
    commit(up, "two")
    new = commit(up, "three")
    assert mesh.main(["ask", "swarm-research", "--body", "Q1?", "--no-wait"]) == 0
    assert mesh.load_run(run)["agents"]["swarm-research"]["base"] == new
    with mesh.editing_run(run) as data:
        data["agents"]["swarm-research"]["status"] = "retired"
    cwd = mesh.Path(mesh.load_peers()["peers"]["swarm-research"]["cwd"])
    assert mesh.main(["peers", "remove", "swarm-research"]) == 0
    capsys.readouterr()
    assert mesh.main(["ask", "swarm-research", "--body", "Q2?", "--no-wait"]) == 0
    assert git("rev-parse", "HEAD", cwd=cwd) == new != git("rev-parse", "HEAD", cwd=clone)
    # and its conversation is still a peer's: no change task, and the hint says so
    entry = mesh.load_run(run)["agents"]["swarm-research"]
    assert entry["peer"] == "swarm-research" and entry["read_only"] is True
    with mesh.editing_run(run) as data:
        data["agents"]["swarm-research"]["status"] = "retired"
    assert mesh.main(["resume", "swarm-research", "--task", "Direct task (no plan)\n\nAdd a feature."]) == 1
    err = capsys.readouterr().err
    assert "only answers questions" in err and f"mesh spawn --repo {clone}" in err


def test_set_worker_base_writes_an_archived_run(mesh, tmp_path):
    folder = mesh.mesh_home() / "archive" / "mold"
    folder.mkdir(parents=True)
    (folder / "run.json").write_text(json.dumps({"id": "mold", "agents": {"a": {"base": "old", "branch": "b"}}}))
    mesh.set_worker_base(folder, "a", "new", "a-peer")
    mesh.set_worker_base(folder, "missing", "x", "x")
    assert json.loads((folder / "run.json").read_text())["agents"] == {
        "a": {"base": "new", "peer": "a-peer", "branch": None}}


def test_a_racing_add_of_the_same_name_is_refused_under_the_lock(mesh, fake, repo, tmp_path, monkeypatch, capsys):
    _, one = upstream_and_clone(tmp_path, "swarm")
    other = tmp_path / "elsewhere"
    other.mkdir()
    _, two = upstream_and_clone(other, "swarm")
    monkeypatch.chdir(repo)
    assert mesh.main(["peers", "add", str(one)]) == 0
    real_load = mesh.load_peers
    calls = {"n": 0}

    def stale_first_read():  # the second add's early check reads the registry before the first add wrote it
        calls["n"] += 1
        return {"peers": {}} if calls["n"] == 1 else real_load()

    monkeypatch.setattr(mesh, "load_peers", stale_first_read)
    capsys.readouterr()
    assert mesh.main(["peers", "add", str(two)]) == 1
    assert "a peer already" in capsys.readouterr().err
    monkeypatch.setattr(mesh, "load_peers", real_load)
    assert mesh.load_peers()["peers"]["swarm-research"]["repo"] == str(one.resolve())


def test_a_bg_peer_reads_gone_only_when_claude_agents_answers(mesh, fake, cli, run, monkeypatch):
    entry = {"role": "worker", "kind": "claude", "view": "bg", "bg_id": "abc123", "status": "running"}
    with mesh.editing_run(run) as data:
        data["agents"]["swarm-research"] = entry
    mesh.deliver(run, "repo-lead", "swarm-research", "task", "Q1?")
    seen = []
    monkeypatch.setattr(mesh, "agent_status", lambda name, entry, rows=None: seen.append(rows) or "gone")
    reads = []

    def agents(rows):
        return lambda: reads.append(rows) or rows

    monkeypatch.setattr(mesh, "claude_agents", agents([]))  # `claude agents` failed: gone means nothing
    assert mesh.owes_answer(run, "swarm-research", entry) is True
    monkeypatch.setattr(mesh, "claude_agents", agents([{"id": "other"}]))
    assert mesh.owes_answer(run, "swarm-research", entry) is False
    # no rows are read once more; the status is judged on the rows the guard saw
    assert reads == [[], [], [{"id": "other"}]] and seen == [[], [{"id": "other"}]]
    assert seen[0] is reads[1] and seen[1] is reads[2]


def test_a_bg_askers_status_is_unknown_when_claude_agents_cant_answer(mesh, fake, cli, run, monkeypatch):
    entry = {"role": "worker", "kind": "claude", "view": "bg", "session": "s1", "status": "running"}
    monkeypatch.setattr(mesh, "claude_agents", lambda: [])
    assert mesh.asker_status("swarm-research", entry) == "unknown"
    monkeypatch.setattr(mesh, "claude_agents", lambda: [{"sessionId": "other"}])
    assert mesh.asker_status("swarm-research", entry) == "gone"
    monkeypatch.setattr(mesh, "claude_agents", lambda: [{"sessionId": "s1", "status": "idle"}])
    assert mesh.asker_status("swarm-research", entry) == "idle"


def test_a_fan_out_gives_up_on_a_peer_held_too_long(mesh, fake, run, monkeypatch):
    fake.on("agent", "get", result={"agent": {"agent_status": "blocked"}})  # at a screen mesh doesn't answer
    mesh.deliver(run, "repo-lead", "repo-fix-typo", "task", "Q?")
    old = (mesh.datetime.now(mesh.timezone.utc) - mesh.timedelta(seconds=61)).isoformat()
    with mesh.editing_run(run) as data:
        data["agents"]["repo-fix-typo"]["held"] = {"sender": "repo-lead", "type": "task", "screen": "Weird", "at": old}
    monkeypatch.setattr(mesh.time, "sleep", lambda _: None)
    out = mesh.watch_each(run, {"repo-fix-typo": 0}, timeout=3600, interval=0.01, held_wait=60)
    status, line = out["repo-fix-typo"]
    assert status == 4 and "still waiting at a startup screen (Weird)" in line and "mesh ring repo-fix-typo" in line

    with mesh.editing_run(run) as data:  # held just now: still waited for, until the fan-out's own timeout
        data["agents"]["repo-fix-typo"]["held"]["at"] = mesh.now_iso()
    assert mesh.watch_each(run, {"repo-fix-typo": 0}, timeout=0.05, interval=0.01, held_wait=60)["repo-fix-typo"][0] == 6


def test_a_bg_askers_status_retries_claude_agents_once(mesh, monkeypatch):
    entry = {"role": "worker", "kind": "claude", "view": "bg", "session": "s1", "status": "running"}
    answers = [[], [{"sessionId": "other"}]]  # a passing failure, then rows without it: gone
    monkeypatch.setattr(mesh, "claude_agents", lambda: answers.pop(0))
    assert mesh.asker_status("swarm-research", entry) == "gone" and not answers


def test_junk_entries_are_reported_by_every_peers_command(mesh, fake, run, repo, monkeypatch, capsys):
    with mesh.editing_peers() as data:
        data["peers"]["junk-peer"] = "oops"
        data["peers"]["norepo-peer"] = {"group": "orch", "cwd": "/x", "upstream": {"branch": "main"}}
    capsys.readouterr()
    assert mesh.main(["ask", "--peers", "all", "--body", "Q?", "--no-refresh"]) == 1  # no traceback
    out = capsys.readouterr().out
    assert "junk-peer: junk-peer's registry entry is broken (not an entry" in out
    assert "norepo-peer's registry entry is broken (no repo recorded)" in out
    assert mesh.main(["ask", "--peers", "junk-peer", "--body", "Q?", "--no-refresh"]) == 1
    assert "registry entry is broken" in capsys.readouterr().out  # selected, not "no peer named"
    for name in ("junk-peer", "norepo-peer"):
        assert mesh.main(["ask", name, "--body", "Q?"]) == 1
        assert f"{name}'s registry entry is broken" in capsys.readouterr().err
    assert mesh.main(["peers", "edit", "junk-peer", "--about", "x"]) == 1
    assert "registry entry is broken" in capsys.readouterr().err
    assert mesh.peer_of("norepo-peer", {"repo": "/x"}) is None
    with mesh.editing_peers() as data:  # only junk left: `all` still reports it, no traceback
        data["peers"] = {"junk-peer": "oops"}
    assert mesh.main(["ask", "--peers", "all", "--body", "Q?", "--no-refresh"]) == 1
    assert "0 of 1 peers answered" in capsys.readouterr().out


def test_peers_remove_copes_with_a_deleted_clone(mesh, fake, repo, tmp_path, monkeypatch, capsys):
    import shutil
    _, clone = upstream_and_clone(tmp_path)
    monkeypatch.chdir(repo)
    assert mesh.main(["peers", "add", str(clone)]) == 0
    cwd = mesh.Path(mesh.load_peers()["peers"]["swarm-research"]["cwd"])
    shutil.rmtree(clone)
    capsys.readouterr()
    assert mesh.main(["peers", "remove", "swarm-research"]) == 0
    out = capsys.readouterr().out
    assert f"its clone at {clone} is gone" in out and "git can't remove it without its clone" in out
    assert "stays at" not in out
    assert not mesh.load_peers()["peers"] and cwd.exists()


def test_a_removed_adopted_peer_never_asked_still_refuses_a_change(mesh, fake, run, repo, tmp_path, monkeypatch,
                                                                    capsys):
    _, clone = upstream_and_clone(tmp_path)
    archived_peer_worker(mesh, clone, "swarm-research", "s1")
    monkeypatch.chdir(repo)
    assert mesh.main(["peers", "add", str(clone)]) == 0
    assert mesh.main(["peers", "remove", "swarm-research"]) == 0
    entry = json.loads((mesh.mesh_home() / "archive" / "mold" / "run.json").read_text())["agents"]["swarm-research"]
    assert entry["peer"] == "swarm-research" and entry["branch"] is None
    stub_new_pane(fake)
    fake.on("workspace", "create", result={"workspace": {"workspace_id": "w9"}})
    capsys.readouterr()
    assert mesh.main(["resume", "swarm-research", "--task", "Add a feature and open a PR."]) == 1
    assert "only answers questions" in capsys.readouterr().err
    assert not git("branch", "--list", "mesh/mold/research", cwd=clone)


def test_asking_a_removed_peer_whose_clone_is_gone_is_a_clean_error(mesh, fake, run, repo, tmp_path, monkeypatch,
                                                                     capsys):
    import shutil
    _, clone = upstream_and_clone(tmp_path)
    archived_peer_worker(mesh, clone, "swarm-research", "s1")
    monkeypatch.chdir(repo)
    assert mesh.main(["peers", "add", str(clone)]) == 0
    assert mesh.main(["peers", "remove", "swarm-research"]) == 0
    shutil.rmtree(clone)
    stub_new_pane(fake)
    capsys.readouterr()
    assert mesh.main(["ask", "swarm-research", "--body", "Q2?", "--no-wait"]) == 1
    assert "and so is its clone" in capsys.readouterr().err


def test_a_renamed_copys_refusal_names_the_peer(mesh, fake, cli, run, repo, tmp_path, monkeypatch, capsys):
    peer_in_run(mesh, fake, cli, repo, tmp_path, monkeypatch)
    assert mesh.main(["ask", "swarm-research", "--body", "Q1?", "--no-refresh", "--no-wait"]) == 0
    with mesh.editing_run(run) as data:
        data["agents"]["swarm-research-2"] = {**data["agents"].pop("swarm-research"), "status": "retired"}
    capsys.readouterr()
    assert mesh.main(["resume", "swarm-research-2", "--task", "Direct task (no plan)\n\nFix it."]) == 1
    err = capsys.readouterr().err
    assert "ask it with `mesh ask swarm-research`," in err


def test_peers_remove_refuses_a_running_peer_whose_clone_is_gone(mesh, fake, cli, run, repo, tmp_path, monkeypatch,
                                                                  capsys):
    import shutil
    _, clone = peer_in_run(mesh, fake, cli, repo, tmp_path, monkeypatch)
    assert mesh.main(["ask", "swarm-research", "--body", "Q1?", "--no-refresh", "--no-wait"]) == 0
    cwd = mesh.load_peers()["peers"]["swarm-research"]["cwd"]
    fake.on("agent", "list", result={"agents": [{"name": "swarm-research", "pane_id": "w9:p10", "cwd": cwd}]})
    shutil.rmtree(clone)
    capsys.readouterr()
    assert mesh.main(["peers", "remove", "swarm-research"]) == 1
    assert "is still running" in capsys.readouterr().err
    assert "swarm-research" in mesh.load_peers()["peers"]


def test_open_on_a_removed_peer_recreates_it_detached(mesh, fake, run, repo, tmp_path, monkeypatch, capsys):
    _, clone = upstream_and_clone(tmp_path)
    path = archived_peer_worker(mesh, clone, "swarm-research", "s1")  # its old run still lists mesh/mold/research
    monkeypatch.chdir(repo)
    assert mesh.main(["peers", "add", str(clone)]) == 0
    assert mesh.main(["peers", "remove", "swarm-research"]) == 0
    assert not path.exists()
    opened = []
    monkeypatch.setattr(mesh.os, "execvpe", lambda *a: opened.append(a))
    monkeypatch.setattr(mesh.os, "chdir", lambda *a: None)
    capsys.readouterr()
    mesh.main(["open", "swarm-research"])
    assert opened and "on its checkout; the worktree was recreated" in capsys.readouterr().err
    assert subprocess.run(["git", "symbolic-ref", "-q", "HEAD"], cwd=path).returncode == 1  # detached
    assert not git("branch", "--list", "mesh/mold/research", cwd=clone)


def test_open_on_a_registered_peer_recreates_its_own_worktree(mesh, fake, run, repo, tmp_path, monkeypatch, capsys):
    import shutil
    _, clone = upstream_and_clone(tmp_path)
    path = archived_peer_worker(mesh, clone, "swarm-research", "s1")
    monkeypatch.chdir(repo)
    assert mesh.main(["peers", "add", str(clone)]) == 0  # adopted, never asked: its entry has no `peer`
    shutil.rmtree(path)
    monkeypatch.setattr(mesh.os, "execvpe", lambda *a: None)
    monkeypatch.setattr(mesh.os, "chdir", lambda *a: None)
    capsys.readouterr()
    mesh.main(["open", "swarm-research"])
    err = capsys.readouterr().err
    assert "on its checkout" in err and "git worktree remove" not in err  # the registry still owns it
    assert subprocess.run(["git", "symbolic-ref", "-q", "HEAD"], cwd=path).returncode == 1
    assert not git("branch", "--list", "mesh/mold/research", cwd=clone)


def test_a_removed_peer_asked_again_is_still_a_peer(mesh, fake, cli, run, repo, tmp_path, monkeypatch, capsys):
    _, clone = peer_in_run(mesh, fake, cli, repo, tmp_path, monkeypatch)
    assert mesh.main(["ask", "swarm-research", "--body", "Q1?", "--no-refresh", "--no-wait"]) == 0
    mesh.deliver(run, "swarm-research", "repo-lead", "done", "A1.")
    with mesh.editing_run(run) as data:
        data["agents"]["swarm-research"]["status"] = "retired"
    cwd = mesh.load_peers()["peers"]["swarm-research"]["cwd"]
    assert mesh.main(["peers", "remove", "swarm-research"]) == 0
    monkeypatch.setattr(mesh, "context_tokens", lambda entry, session: 10 ** 9)
    started = []
    real_start = mesh.start_agent
    monkeypatch.setattr(mesh, "start_agent", lambda *a, **kw: started.append(kw) or real_start(*a, **kw))
    capsys.readouterr()
    assert mesh.main(["ask", "swarm-research", "--body", "Q2?", "--no-wait"]) == 0
    compact = next(c["argv"][-1] for c in cli.calls if str(c["argv"][-1]).startswith("/compact"))
    assert "Keep what you learned about swarm" in compact  # PEER_FOCUS, not the next task
    assert started and started[-1].get("peer") is True  # a Settings Warning still takes Continue
    out = json.loads(capsys.readouterr().out)
    assert out["worktree_recreated"]  # and this run owns it now, with no branch, so its `mesh stop` removes it
    assert {"path": cwd, "repo": str(clone.resolve()), "branch": None} in mesh.load_run(run)["worktrees"]
    task = sorted(mesh.inbox_dir(run, "swarm-research").glob("**/*-task.md"))[-1].read_text()
    assert mesh.UNANSWERED_NOTE not in task  # Q1 was answered


def test_a_failed_release_keeps_the_worktree_on_its_run(mesh, fake, run, repo, tmp_path, monkeypatch, capsys):
    _, clone = upstream_and_clone(tmp_path)
    path = mesh.worktree_root() / run / "swarm-research"
    base = git("rev-parse", "HEAD", cwd=clone)
    git("worktree", "add", "-q", "-b", "mesh/mtest/research", str(path), base, cwd=clone)
    with mesh.editing_run(run) as data:
        data["agents"]["swarm-research"] = {"role": "worker", "kind": "claude", "repo": str(clone), "cwd": str(path),
                                            "base": base, "status": "retired"}
        data.setdefault("worktrees", []).append({"path": str(path), "repo": str(clone),
                                                 "branch": "mesh/mtest/research", "base": base})
    real = mesh.git

    def failing(*args, cwd):
        if args[:2] == ("branch", "-D"):
            raise mesh.MeshError("git branch -D: Unable to create index.lock")
        return real(*args, cwd=cwd)

    monkeypatch.setattr(mesh, "git", failing)
    monkeypatch.chdir(repo)
    assert mesh.main(["peers", "add", str(clone)]) == 1
    assert not mesh.load_peers()["peers"]
    assert [w["path"] for w in mesh.load_run(run)["worktrees"]] == [str(path)]  # its run's `mesh stop` still has it
    monkeypatch.setattr(mesh, "git", real)
    assert mesh.main(["peers", "add", str(clone)]) == 0  # once git works, the add goes through
    assert not mesh.load_run(run)["worktrees"] and not git("branch", "--list", "mesh/mtest/research", cwd=clone)


def test_an_ask_whose_clone_is_gone_starts_nothing(mesh, fake, cli, run, repo, tmp_path, monkeypatch, capsys):
    import shutil
    _, clone = peer_in_run(mesh, fake, cli, repo, tmp_path, monkeypatch)
    started = []
    monkeypatch.setattr(mesh, "ask_identity", lambda *a: started.append(a))  # where a run would be started
    shutil.rmtree(clone)
    capsys.readouterr()
    assert mesh.main(["ask", "swarm-research", "--body", "Q?"]) == 1
    assert "swarm-research's clone" in capsys.readouterr().err
    assert mesh.main(["peers", "remove", "swarm-research"]) == 0  # its checkout stays, but it can't be recreated
    shutil.rmtree(mesh.worktree_root() / "mold" / "swarm-research")
    assert mesh.main(["ask", "swarm-research", "--body", "Q?"]) == 1
    assert "and so is its clone" in capsys.readouterr().err
    assert not started


def test_a_non_string_repo_is_a_broken_entry(mesh, fake, run, repo, monkeypatch, capsys):
    with mesh.editing_peers() as data:
        data["peers"]["num-peer"] = {"repo": 123, "cwd": "/x", "upstream": {"branch": "main"}}
    assert mesh.peer_problem(mesh.load_peers()["peers"]["num-peer"]) == "repo 123 is not a path"
    capsys.readouterr()
    assert mesh.main(["ask", "--peers", "all", "--body", "Q?", "--no-refresh"]) == 1  # no traceback
    assert "num-peer's registry entry is broken (repo 123 is not a path)" in capsys.readouterr().out
    assert mesh.main(["ask", "num-peer", "--body", "Q?"]) == 1
    assert "registry entry is broken" in capsys.readouterr().err


def test_a_resumed_asker_that_left_a_task_unanswered_is_told(mesh, fake, cli, run, repo, tmp_path, monkeypatch,
                                                            capsys):
    peer_in_run(mesh, fake, cli, repo, tmp_path, monkeypatch)
    assert mesh.main(["ask", "swarm-research", "--body", "Q1?", "--no-refresh", "--no-wait"]) == 0
    monkeypatch.setattr(mesh, "agent_status", lambda name, entry, rows=None: "gone")  # it died with Q1 unanswered
    assert mesh.main(["ask", "swarm-research", "--body", "Q2?", "--no-refresh", "--no-wait"]) == 0
    task = sorted(mesh.inbox_dir(run, "swarm-research").glob("**/*-task.md"))[-1].read_text()
    assert "Q2?" in task and task.rstrip().endswith(mesh.UNANSWERED_NOTE)


def test_open_on_a_peer_whose_clone_and_worktree_are_gone_is_a_clean_error(mesh, fake, cli, run, repo, tmp_path,
                                                                           monkeypatch, capsys):
    import shutil
    _, clone = peer_in_run(mesh, fake, cli, repo, tmp_path, monkeypatch)
    assert mesh.main(["ask", "swarm-research", "--body", "Q1?", "--no-refresh", "--no-wait"]) == 0
    with mesh.editing_run(run) as data:
        data["agents"]["swarm-research"]["status"] = "retired"
    shutil.rmtree(mesh.load_peers()["peers"]["swarm-research"]["cwd"])
    shutil.rmtree(clone)
    monkeypatch.setattr(mesh.os, "execvpe", lambda *a: None)
    capsys.readouterr()
    assert mesh.main(["open", "swarm-research"]) == 1  # no traceback from git in a missing directory
    assert "and so is its clone" in capsys.readouterr().err


def test_a_live_idle_worker_owing_a_report_is_told_working_one_is_not(mesh, fake, run, monkeypatch, capsys):
    mesh.deliver(run, "repo-lead", "repo-fix-typo", "task", "Fix the typo.")
    fake.on("agent", "get", result={"agent": {"agent_status": "idle"}})
    assert mesh.main(["ask", "repo-fix-typo", "--body", "Q1?", "--no-wait"]) == 0
    tasks = sorted(mesh.inbox_dir(run, "repo-fix-typo").glob("*-task.md"))
    assert tasks[-1].read_text().rstrip().endswith(mesh.UNANSWERED_NOTE)
    fake.on("agent", "get", result={"agent": {"agent_status": "working"}})  # still on it: leave its work alone
    assert mesh.main(["ask", "repo-fix-typo", "--body", "Q2?", "--no-wait"]) == 0
    tasks = sorted(mesh.inbox_dir(run, "repo-fix-typo").glob("*-task.md"))
    assert "Q2?" in tasks[-1].read_text() and mesh.UNANSWERED_NOTE not in tasks[-1].read_text()


def test_a_peer_name_reused_for_another_clone_isnt_the_old_conversations_peer(mesh, fake, repo, tmp_path,
                                                                             monkeypatch, capsys):
    _, clone = upstream_and_clone(tmp_path)
    other = tmp_path / "elsewhere"
    other.mkdir()
    _, other_clone = upstream_and_clone(other)
    monkeypatch.chdir(repo)
    assert mesh.main(["peers", "add", str(other_clone)]) == 0  # swarm-research is now the other clone's
    entry = {"repo": str(clone), "cwd": "/gone", "peer": "swarm-research"}  # a removed peer's old conversation
    assert mesh.worker_peer("swarm-research", entry) == ("swarm-research", None)
    same = {**entry, "repo": str(other_clone)}
    assert mesh.worker_peer("swarm-research", same)[1]["repo"] == str(other_clone.resolve())


def test_adopting_from_a_live_run_with_its_worktree_gone_deletes_the_untouched_branch(mesh, fake, run, repo,
                                                                                      tmp_path, monkeypatch):
    import shutil
    _, clone = upstream_and_clone(tmp_path)
    path = mesh.worktree_root() / run / "swarm-research"
    base = git("rev-parse", "HEAD", cwd=clone)
    git("worktree", "add", "-q", "-b", "mesh/mtest/research", str(path), base, cwd=clone)
    with mesh.editing_run(run) as data:
        data["agents"]["swarm-research"] = {"role": "worker", "kind": "claude", "repo": str(clone), "cwd": str(path),
                                            "base": base, "status": "retired"}
        data.setdefault("worktrees", []).append({"path": str(path), "repo": str(clone),
                                                 "branch": "mesh/mtest/research", "base": base})
    shutil.rmtree(path)
    monkeypatch.chdir(repo)
    assert mesh.main(["peers", "add", str(clone)]) == 0  # recreated detached before it is released
    assert path.exists() and not mesh.load_run(run)["worktrees"]
    assert not git("branch", "--list", "mesh/mtest/research", cwd=clone)


def test_stop_removes_a_removed_peers_recreated_worktree_but_never_a_registered_one(mesh, fake, cli, run, repo,
                                                                                    tmp_path, monkeypatch, capsys):
    _, clone = peer_in_run(mesh, fake, cli, repo, tmp_path, monkeypatch)
    assert mesh.main(["ask", "swarm-research", "--body", "Q1?", "--no-refresh", "--no-wait"]) == 0
    with mesh.editing_run(run) as data:
        data["agents"]["swarm-research"]["status"] = "retired"
    cwd = mesh.Path(mesh.load_peers()["peers"]["swarm-research"]["cwd"])
    assert mesh.main(["peers", "remove", "swarm-research"]) == 0
    assert mesh.main(["ask", "swarm-research", "--body", "Q2?", "--no-wait"]) == 0
    assert cwd.exists()
    _, other = upstream_and_clone(tmp_path / "more", "town")
    assert mesh.main(["peers", "add", str(other)]) == 0
    peer_cwd = mesh.Path(mesh.load_peers()["peers"]["town-research"]["cwd"])
    with mesh.editing_run(run) as data:  # a list that names a registered peer's worktree, or a clone itself
        data["worktrees"] += [{"path": str(peer_cwd), "repo": str(other), "branch": None},
                              {"path": str(other), "repo": str(other), "branch": None}]
    capsys.readouterr()
    assert mesh.main(["stop"]) == 0
    out = json.loads(capsys.readouterr().out)
    assert not cwd.exists() and clone.exists() and {"path": str(cwd), "branch": None} in out["worktrees_removed"]
    assert peer_cwd.exists() and other.exists() and len(out["worktrees_removed"]) == 1
    assert None not in out["branches_kept"]
