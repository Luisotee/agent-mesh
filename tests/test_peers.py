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
        data["agents"]["repo-fix-typo"]["collect"] = "repo-lead"
    out = mesh.deliver(run, "repo-fix-typo", "repo-lead", "done", "Answer.")
    assert out["collected"] is True and mesh.delivered(out) and not fake.called("agent", "prompt")
    assert mesh.unread(run, "repo-lead")
    # a question still rings: the root has to answer it
    mesh.deliver(run, "repo-fix-typo", "repo-lead", "question", "Which branch?")
    assert fake.called("agent", "prompt")


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
