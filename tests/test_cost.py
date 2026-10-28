"""`mesh cost`: spend per agent, kind and phase from Claude session files and agy conversations."""
import json
import sqlite3
from datetime import datetime, timedelta, timezone

import pytest

T0 = datetime(2026, 10, 1, 12, 0, tzinfo=timezone.utc)  # in the past: run mode skips files older than a run


def iso(minutes: float) -> str:
    return (T0 + timedelta(minutes=minutes)).isoformat()


def reply(minutes, msg_id, model="claude-opus-5-5", input=0, w1=0, w5=0, read=0, output=0, blocks=1,
          sidechain=False, bash=None):
    """One reply as Claude Code writes it: one record per content block, each with the whole usage."""
    usage = {"input_tokens": input, "cache_creation_input_tokens": w1 + w5, "cache_read_input_tokens": read,
             "output_tokens": output,
             "cache_creation": {"ephemeral_1h_input_tokens": w1, "ephemeral_5m_input_tokens": w5}}
    content = [{"type": "tool_use", "name": "Bash", "input": {"command": bash}}] if bash else [{"type": "text"}]
    return [{"type": "assistant", "timestamp": iso(minutes), "isSidechain": sidechain,
             "message": {"id": msg_id, "model": model, "usage": usage, "content": content}}] * blocks


def prompt(minutes, text, entrypoint="cli", cwd="/x"):
    return [{"type": "user", "timestamp": iso(minutes), "entrypoint": entrypoint, "cwd": cwd,
             "message": {"role": "user", "content": text}}]


def transcript(mesh, cwd, session, *records, sub=None):
    folder = mesh.claude_sessions_dir(cwd)
    path = folder / f"{session}.jsonl" if sub is None else folder / session / "subagents" / f"{sub}.jsonl"
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text("".join(json.dumps(r) + "\n" for group in records for r in group))
    return path


def write_run(mesh, run_id, agents, events=(), created=0, archived=False):
    folder = mesh.mesh_home() / ("archive" if archived else "") / run_id
    folder.mkdir(parents=True, exist_ok=True)
    root = next(n for n, a in agents.items() if a.get("role") == "root")
    (folder / "run.json").write_text(json.dumps({"id": run_id, "created": iso(created), "root": root,
                                                 "agents": agents}))
    (folder / "log.jsonl").write_text("".join(json.dumps(e) + "\n" for e in events))


def worker(session, spawned, cwd="/wt", **extra):
    return {"role": "worker", "kind": "claude", "session": session, "cwd": cwd, "spawned": iso(spawned),
            "size": "large", "view": "pane", "models": [{"at": iso(spawned), "why": "spawn"}], **extra}


def root(session, cwd="/repo"):
    return {"role": "root", "kind": "claude", "session": session, "cwd": cwd}


def cost(mesh, capsys, *argv):
    capsys.readouterr()
    assert mesh.main(["cost", *argv, "--json"]) == 0
    return json.loads(capsys.readouterr().out)


def by(rows, key, value):
    return next(r for r in rows if r[key] == value)


def test_prices_one_call_per_family(mesh):
    call = {"model": "", "input": 1e6, "w5": 1e6, "w1": 1e6, "read": 1e6, "output": 1e6}
    assert mesh.call_cost({**call, "model": "claude-opus-5-5"}) == pytest.approx(4 + 5 + 8 + 0.2 + 20)
    assert mesh.call_cost({**call, "model": "claude-sonnet-5-5"}) == pytest.approx(2 + 2.5 + 4 + 0.2 + 10)
    assert mesh.call_cost({**call, "model": "claude-fable-5-1"}) == pytest.approx(10 + 12.5 + 20 + 0.25 + 50)
    assert mesh.call_cost({**call, "model": "mystery"}) == mesh.call_cost({**call, "model": "claude-opus-5-5"})
    assert mesh.call_cost({**call, "model": "agy"}) == 0


def test_calls_fold_by_message_id_and_read_long_lines(mesh, tmp_path):
    big = {"type": "user", "timestamp": iso(1), "message": {"content": [{"type": "tool_result",
                                                                         "content": "x" * (2 << 20)}]}}
    path = transcript(mesh, "/x", "s1", prompt(0, "hi"), reply(0.5, "m1", input=10, output=5, blocks=3), [big],
                      reply(2, "m2", read=100, output=1, blocks=2), reply(3, "m3", model="<synthetic>", output=9))
    calls, info = mesh.claude_calls(path)
    assert [(c["input"], c["read"], c["output"]) for c in calls] == [(10, 0, 5), (0, 100, 1)]
    assert info["entrypoint"] == "cli" and not info["doorbell"]


def test_write_without_ttl_split_counts_as_1h(mesh):
    path = transcript(mesh, "/x", "s1", [{"type": "assistant", "timestamp": iso(0), "message": {
        "id": "m", "model": "claude-opus-5-5", "usage": {"cache_creation_input_tokens": 500}}}])
    assert mesh.claude_calls(path)[0][0]["w1"] == 500


def test_run_splits_plan_and_exec_at_approval(mesh, capsys):
    write_run(mesh, "mrun", {
        "repo-lead": root("r1"),
        "repo-fix": worker("w1", 1, phase="exec", exec_model="opus",
                           models=[{"at": iso(1), "why": "spawn"}, {"at": iso(10), "why": "plan approved"}]),
    }, events=[{"time": iso(60), "event": "stop"}])
    transcript(mesh, "/wt", "w1", reply(2, "a", output=1000), reply(20, "b", output=3000))
    data = cost(mesh, capsys, "mrun")
    fix = by(data["agents"], "name", "repo-fix")
    assert fix["kind"] == "worker large pane"
    assert fix["plan_usd"] == pytest.approx(0.02) and fix["exec_usd"] == pytest.approx(0.06)


def test_direct_task_has_no_phase_and_planned_task_starts_one(mesh):
    entry = worker("w", 0, size="small", exec_model="opus",
                   models=[{"at": iso(0), "why": "spawn"}, {"at": iso(5), "why": "planned task"},
                           {"at": iso(9), "why": "plan approved"}])
    marks = mesh.phase_marks("repo-fix", entry, [])
    assert [mesh.phase_at(marks, mesh.epoch(iso(m))) for m in (1, 6, 10)] == ["", "plan", "exec"]
    ended = mesh.phase_marks("repo-fix", worker("w", 0, exec_model="opus", phase=None),
                             [{"time": iso(3), "event": "plan_phase_ended", "name": "repo-fix"}])
    assert [mesh.phase_at(ended, mesh.epoch(iso(m))) for m in (1, 4)] == ["plan", ""]


def test_peer_session_splits_across_runs(mesh, capsys):
    """One peer conversation in two runs: each run gets its own calls, and the compaction a resume runs before
    the reopen is recorded goes to the run that resumed it."""
    peer = dict(peer="x-research", read_only=True)
    write_run(mesh, "mone", {"a-lead": root("r1"), "x-research": worker("p", 1, **peer)},
              events=[{"time": iso(30), "event": "stop"}], archived=True)
    write_run(mesh, "mtwo", {"a-lead": root("r2"), "x-research": worker("p", 120, **peer)}, created=100)
    transcript(mesh, "/wt", "p", reply(2, "a", output=1000), reply(118, "compact", output=2000),
               reply(121, "b", output=4000))
    assert by(cost(mesh, capsys, "mone")["agents"], "name", "x-research")["usd"] == pytest.approx(0.02)
    two = by(cost(mesh, capsys, "mtwo")["agents"], "name", "x-research")
    assert two["kind"] == "peer" and two["usd"] == pytest.approx(0.12)


def test_root_counts_only_while_its_run_is_open(mesh, capsys, monkeypatch):
    write_run(mesh, "mrun", {"repo-lead": root("r1")}, created=10, events=[
        {"time": iso(10), "event": "init"},
        {"time": iso(20), "event": "reset", "name": "repo-lead", "old_session": "r1", "new_session": "r2"},
        {"time": iso(40), "event": "stop"}])
    transcript(mesh, "/repo", "r1", prompt(0, "plain work"), reply(1, "before", output=1000),
               reply(15, "in", output=2000))
    transcript(mesh, "/repo", "r2", reply(25, "after-reset", output=4000), reply(50, "after-stop", output=8000))
    data = cost(mesh, capsys, "mrun")
    assert by(data["agents"], "name", "repo-lead")["usd"] == pytest.approx(0.12)
    window = cost(mesh, capsys, "--since", iso(-5), "--until", iso(100))
    assert by(window["kinds"], "kind", "root")["usd"] == pytest.approx(0.12)
    assert by(window["kinds"], "kind", "user")["usd"] == pytest.approx(0.18)


def test_root_fallback_for_a_session_cleared_by_hand(mesh, capsys):
    write_run(mesh, "mrun", {"repo-lead": root("r1")}, created=0, events=[{"time": iso(60), "event": "stop"}])
    transcript(mesh, "/repo", "hand", prompt(5, "go on", cwd="/repo"),
               reply(6, "a", output=1000, bash="mesh approve repo-fix"))
    assert by(cost(mesh, capsys, "mrun")["agents"], "name", "repo-lead")["usd"] == pytest.approx(0.02)


def test_cold_resume_flagged_only_after_the_ttl(mesh, capsys):
    write_run(mesh, "mrun", {"repo-lead": root("r1"), "repo-ask": worker("w", 0, read_only=True)})
    transcript(mesh, "/wt", "w", reply(1, "first", w1=50_000), reply(30, "warm", w1=30_000, read=50_000),
               reply(200, "cold", w1=80_000), reply(205, "small", w1=1_000))
    ask = by(cost(mesh, capsys, "mrun")["agents"], "name", "repo-ask")
    assert ask["kind"] == "asker" and ask["cold"] == 1
    assert ask["cold_usd"] == pytest.approx(80_000 * 8 / 1e6)


def test_subagents_count_under_their_parents_kind(mesh, capsys):
    write_run(mesh, "mrun", {"repo-lead": root("r1"),
                             "repo-review-7": worker("v", 0, reviews=["https://github.com/o/repo/pull/7"])})
    transcript(mesh, "/wt", "v", reply(1, "a", output=1000))
    transcript(mesh, "/wt", "v", reply(2, "s", w5=10_000, sidechain=True), sub="agent-1")
    data = cost(mesh, capsys, "mrun")
    assert by(data["kinds"], "kind", "reviewer subagents")["usd"] == pytest.approx(0.05)
    agent = by(data["agents"], "name", "repo-review-7")
    assert agent["usd"] == pytest.approx(0.02) and agent["sub_usd"] == pytest.approx(0.05)
    assert by(data["kinds"], "kind", "reviewer subagents")["write_5m_share"] == 100.0


def test_window_counts_sessions_no_run_names(mesh, capsys):
    transcript(mesh, "/home/u/p", "u1", prompt(0, "hello"), reply(1, "a", read=50_000, output=10))
    transcript(mesh, "/home/u/p", "h1", prompt(0, "x", entrypoint="sdk-cli"), reply(1, "b", output=10))
    transcript(mesh, "/home/u/p", "d1", prompt(0, "📬 [mesh] You are q (worker) in agent-mesh run m1"),
               reply(1, "c", output=10))
    transcript(mesh, "/home/u/p", "old", reply(-600, "d", output=10))
    data = cost(mesh, capsys, "--since", iso(-1))
    kinds = {k["kind"]: k for k in data["kinds"]}
    assert set(kinds) == {"user", "headless", "worker (unmapped)"}
    assert kinds["user"]["first_ctx_median"] == 50_000


def test_since_takes_a_span_or_iso(mesh):
    now = datetime(2026, 11, 11, tzinfo=timezone.utc)
    assert mesh.parse_since("7d", now) == datetime(2026, 11, 4, tzinfo=timezone.utc).timestamp()
    assert mesh.parse_since("2026-11-04T00:00+00:00") == datetime(2026, 11, 4, tzinfo=timezone.utc).timestamp()
    with pytest.raises(mesh.MeshError):
        mesh.parse_since("last week")


def varint(n: int) -> bytes:
    out = b""
    while True:
        byte, n = n & 0x7F, n >> 7
        out += bytes([byte | (0x80 if n else 0)])
        if not n:
            return out


def field(number: int, value) -> bytes:
    if isinstance(value, int):
        return varint(number << 3) + varint(value)
    return varint(number << 3 | 2) + varint(len(value)) + value


def test_agy_steps_give_tokens_by_time(mesh, capsys, tmp_path):
    folder = tmp_path / "agy" / "conversations"
    folder.mkdir(parents=True)
    db = sqlite3.connect(folder / "conv.db")
    db.execute("CREATE TABLE steps (idx integer, step_type integer, status integer, metadata blob)")
    seconds = int(T0.timestamp()) + 120
    usage = field(2, 1000) + field(5, 20_000) + field(3, 300)
    db.execute("INSERT INTO steps VALUES (1, 15, 3, ?)", (field(1, field(1, seconds) + field(2, 5)) + field(9, usage),))
    db.execute("INSERT INTO steps VALUES (2, 17, 3, ?)", (field(1, field(1, seconds + 1)),))
    db.commit()
    db.close()
    assert mesh.agy_calls("conv") == [{"id": None, "t": float(seconds), "model": "agy", "input": 1000, "w5": 0, "w1": 0,
                                       "read": 20_000, "output": 300}]
    write_run(mesh, "mrun", {"repo-lead": root("r1"),
                             "repo-fix": {**worker("conv", 1), "kind": "agy"}})
    data = cost(mesh, capsys, "mrun")
    assert data["agy"] == [{"kind": "agy worker large pane", "sessions": 1, "calls": 1, "input": 1000,
                            "read": 20_000, "output": 300}]
    assert data["usd"] == 0


def test_a_background_copy_counts_its_copied_calls_once(mesh, capsys):
    """A bg swap or resume copies the conversation, message ids and all, into a new session file: the copied calls
    stay the original's (and its run's), the copy's own first call is a cold resume, and neither is a prefix."""
    write_run(mesh, "mone", {"a-lead": root("r1"), "a-fix": worker("orig", 1, view="bg")},
              events=[{"time": iso(30), "event": "stop"}], archived=True)
    write_run(mesh, "mtwo", {"a-lead": root("r2"), "a-fix": worker("copy", 200, view="bg", sessions=["orig"])},
              created=190)
    calls = (reply(2, "a", w1=30_000, output=1000), reply(3, "b", output=1000))
    transcript(mesh, "/wt", "orig", *calls)
    transcript(mesh, "/wt", "copy", *calls, reply(201, "c", w1=40_000, output=1000))
    one = by(cost(mesh, capsys, "mone")["agents"], "name", "a-fix")
    two = by(cost(mesh, capsys, "mtwo")["agents"], "name", "a-fix")
    assert one["calls"] == 2 and two["calls"] == 1 and two["cold"] == 1
    window = cost(mesh, capsys, "--since", iso(-5))
    assert window["usd"] == pytest.approx(one["usd"] + two["usd"])
    assert by(window["kinds"], "kind", "worker large bg")["first_ctx_median"] == 30_000


def test_a_run_with_no_claude_calls_prints_its_tables(mesh, capsys):
    write_run(mesh, "mrun", {"repo-lead": root("r1")})
    assert mesh.main(["cost", "mrun"]) == 0
    assert "run mrun: $0.00" in capsys.readouterr().out


def test_an_inverted_window_is_refused(mesh, capsys):
    assert mesh.main(["cost", "--since", "2026-11-04", "--until", "2026-11-01"]) == 1
    assert "not before --until" in capsys.readouterr().err


def test_root_commands_only_count_at_a_command_start(mesh):
    assert mesh.ROOT_COMMAND_RE.search("cd /repo && mesh spawn --task x")
    assert mesh.ROOT_COMMAND_RE.search("mesh approve repo-fix")
    assert mesh.ROOT_COMMAND_RE.search("ls\nmesh watch --any")
    assert mesh.ROOT_COMMAND_RE.search("for r in a b; do mesh ask $r --body x; done")
    assert mesh.ROOT_COMMAND_RE.search("if true; then mesh approve repo-fix; fi")
    assert mesh.ROOT_COMMAND_RE.search("timeout 900 mesh watch --any")
    assert mesh.ROOT_COMMAND_RE.search("MESH_MIN_QUOTA=20 mesh spawn --repo x")
    assert mesh.ROOT_COMMAND_RE.search("time mesh ask x --body y")
    assert not mesh.ROOT_COMMAND_RE.search('grep -n "mesh spawn" README.md')
    assert not mesh.ROOT_COMMAND_RE.search("echo see mesh approve in the docs")


def test_an_unstopped_runs_window_ends_at_its_last_event(mesh, capsys):
    write_run(mesh, "mold", {"repo-lead": root("r1")}, events=[{"time": iso(20), "event": "init"}])
    transcript(mesh, "/repo", "r1", reply(10, "in", output=1000), reply(500, "later", output=1000))
    assert by(cost(mesh, capsys, "mold")["agents"], "name", "repo-lead")["calls"] == 1


def test_a_reviewer_resumed_in_a_later_run_is_one_reviewer(mesh, capsys):
    review = dict(reviews=["https://github.com/o/repo/pull/7"], repo="/src/repo")
    write_run(mesh, "mone", {"a-lead": root("r1"), "repo-review-7": worker("v", 1, **review)},
              events=[{"time": iso(30), "event": "stop"}], archived=True)
    write_run(mesh, "mtwo", {"a-lead": root("r2"), "repo-review-7": worker("v", 60, **review)}, created=50)
    transcript(mesh, "/wt", "v", reply(2, "a", output=1000), reply(61, "b", output=1000))
    reviews = cost(mesh, capsys, "--since", iso(-5))["reviews"]
    assert reviews["prs"] == 1 and reviews["reviewers_per_pr"] == 1.0


def test_a_worker_resumed_in_its_own_run_keeps_its_earlier_calls(mesh, capsys):
    """A resume in the same run writes a fresh entry (spawned now); the log still says when it first started."""
    write_run(mesh, "mrun", {"repo-lead": root("r1"), "repo-fix": worker("w", 50)}, events=[
        {"time": iso(1), "event": "spawn", "name": "repo-fix"}, {"time": iso(50), "event": "resume", "name": "repo-fix"}])
    transcript(mesh, "/wt", "w", reply(5, "a", output=1000), reply(55, "b", output=1000))
    assert by(cost(mesh, capsys, "mrun")["agents"], "name", "repo-fix")["calls"] == 2


def test_calls_before_the_first_phase_mark_have_no_phase(mesh):
    marks = mesh.phase_marks("repo-fix", worker("w", 50, exec_model="opus", phase="plan"), [])
    assert [mesh.phase_at(marks, mesh.epoch(iso(m))) for m in (10, 60)] == ["", "plan"]


def test_a_reviewer_renamed_on_resume_is_one_reviewer(mesh, capsys):
    review = dict(reviews=["https://github.com/o/repo/pull/7"], repo="/src/repo")
    write_run(mesh, "mone", {"a-lead": root("r1"), "repo-review-7-claude": worker("v", 1, **review)},
              events=[{"time": iso(30), "event": "stop"}], archived=True)
    write_run(mesh, "mtwo", {"a-lead": root("r2"), "repo-review-7-claude-2": worker(
        "v2", 60, resumed_from={"run": "mone", "name": "repo-review-7-claude"}, **review)}, created=50)
    transcript(mesh, "/wt", "v", reply(2, "a", output=1000))
    transcript(mesh, "/wt", "v2", reply(61, "b", output=1000))
    reviews = cost(mesh, capsys, "--since", iso(-5))["reviews"]
    assert reviews["prs"] == 1 and reviews["reviewers_per_pr"] == 1.0


def test_an_unstopped_run_ends_where_the_next_run_of_its_root_starts(mesh, capsys):
    write_run(mesh, "mold", {"repo-lead": root("r1")}, events=[{"time": iso(20), "event": "init"}])
    write_run(mesh, "mnew", {"repo-lead": root("r1")}, created=40, events=[{"time": iso(40), "event": "init"}])
    transcript(mesh, "/repo", "r1", reply(10, "a", output=1000), reply(45, "b", output=1000))
    assert by(cost(mesh, capsys, "mold")["agents"], "name", "repo-lead")["calls"] == 1
    assert by(cost(mesh, capsys, "mnew")["agents"], "name", "repo-lead")["calls"] == 1
