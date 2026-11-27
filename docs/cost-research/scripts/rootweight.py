#!/usr/bin/env python3
"""The root's weight per worker task, from the root's own Claude Code transcripts (stdlib only).

    python3 rootweight.py <transcript.jsonl> [<transcript.jsonl> ...]

Each argument is one root session's transcript; its subagents (`<session>/subagents/*.jsonl` next to it) are
counted apart. Per session it prints the calls, the tokens by kind, the API-equivalent dollars (prices of
README.md, "Weighting"), the context per call, and what the root did for its workers: the tasks it sent (`mesh send
--type task`, `mesh spawn --task`, `mesh resume --task`), the answers (follow-up work), the reports it read (`done`
and `failed` in what `mesh inbox` printed), and the reviews it ran itself (`mesh ask --review`). Then per task:
calls, dollars, and tokens read, written and output. A session that sent no task is a session of something else.

Records are deduplicated by `message.id` (Claude Code writes one record per content block, each with the whole
usage). Dollars are what the usage would cost at API prices, not what a subscription counts."""
import glob
import json
import os
import re
import sys
from collections import Counter, defaultdict

# $ per million tokens: input, 1h cache write, 5m cache write, cache read, output (README.md, "Weighting")
PRICES = {
    "opus": (4.0, 8.0, 5.0, 0.20, 20.0),
    "sonnet": (2.0, 4.0, 2.5, 0.10, 10.0),
}


def family(model):
    m = (model or "").lower()
    return "sonnet" if "sonnet" in m else "opus"  # haiku and unknown models are rare in a root: priced as opus


def calls(path):
    """The deduplicated API calls of one transcript: usage rows, in order."""
    seen, out = set(), []
    for line in open(path, errors="replace"):
        try:
            d = json.loads(line)
        except ValueError:
            continue
        if d.get("type") != "assistant":
            continue
        m = d.get("message") or {}
        u = m.get("usage")
        if not u or m.get("id") in seen:
            continue
        seen.add(m.get("id"))
        cc = u.get("cache_creation") or {}
        w1h = cc.get("ephemeral_1h_input_tokens", 0)
        w5m = cc.get("ephemeral_5m_input_tokens", 0)
        total_w = u.get("cache_creation_input_tokens", 0)
        if total_w and not (w1h or w5m):
            w1h = total_w  # no breakdown: a main session writes the 1h cache
        out.append({"model": family(m.get("model")), "in": u.get("input_tokens", 0), "w1h": w1h, "w5m": w5m,
                    "read": u.get("cache_read_input_tokens", 0), "out": u.get("output_tokens", 0),
                    "t": d.get("timestamp", "")})
    return out


def dollars(rows):
    total = 0.0
    for r in rows:
        i, w1, w5, rd, o = PRICES[r["model"]]
        total += (r["in"] * i + r["w1h"] * w1 + r["w5m"] * w5 + r["read"] * rd + r["out"] * o) / 1e6
    return total


def split(rows):
    """Dollars by kind: cache read, cache write, output, uncached input."""
    out = {"read": 0.0, "write": 0.0, "output": 0.0, "input": 0.0}
    for r in rows:
        i, w1, w5, rd, o = PRICES[r["model"]]
        out["read"] += r["read"] * rd / 1e6
        out["write"] += (r["w1h"] * w1 + r["w5m"] * w5) / 1e6
        out["output"] += r["out"] * o / 1e6
        out["input"] += r["in"] * i / 1e6
    return out


def commands(path):
    """The Bash commands the root ran, and the text of what came back from its tools."""
    cmds, results = [], []
    for line in open(path, errors="replace"):
        try:
            d = json.loads(line)
        except ValueError:
            continue
        c = (d.get("message") or {}).get("content")
        if not isinstance(c, list):
            continue
        for b in c:
            if b.get("type") == "tool_use" and b.get("name") == "Bash":
                cmds.append((b.get("input") or {}).get("command", ""))
            elif b.get("type") == "tool_result":
                t = b.get("content")
                results.append(t if isinstance(t, str) else " ".join(x.get("text", "") for x in t or [] if isinstance(x, dict)))
    return cmds, results


TASK = re.compile(r"mesh send\s+\S+\s+--type\s+task\b|mesh spawn\b[^\n]*--task\b|mesh resume\b[^\n]*--task\b")
ANSWER = re.compile(r"mesh send\s+\S+\s+--type\s+answer\b")
REVIEW = re.compile(r"mesh ask\b[^\n]*--review\b")
REPORT = re.compile(r"^── (?:done|failed) from ", re.M)


def row(path):
    main = calls(path)
    sub = []
    for f in glob.glob(os.path.join(path[:-len(".jsonl")], "subagents", "*.jsonl")):
        sub += calls(f)
    cmds, results = commands(path)
    joined = "\n".join(cmds)
    c = Counter(tasks=len(TASK.findall(joined)), answers=len(ANSWER.findall(joined)), reviews=len(REVIEW.findall(joined)),
                reports=sum(len(REPORT.findall(r)) for r in results))
    ctx = [r["in"] + r["w1h"] + r["w5m"] + r["read"] for r in main]
    return {"name": os.path.basename(path)[:8], "main": main, "sub": sub, "ctx": ctx, **c,
            "first": main[0]["t"][:16] if main else "", "last": main[-1]["t"][:16] if main else ""}


def main():
    rows = [row(p) for p in sys.argv[1:]]
    print("| session | from → to (UTC) | calls | context/call | $ main | $ subagents | tasks | answers | reports read | reviews run |")
    print("|---|---|---|---|---|---|---|---|---|---|")
    tot = defaultdict(float)
    for r in rows:
        n = len(r["main"])
        print(f"| {r['name']} | {r['first']} → {r['last']} | {n} | {sum(r['ctx']) // max(n, 1) // 1000}k | "
              f"{dollars(r['main']):.0f} | {dollars(r['sub']):.0f} | {r['tasks']} | {r['answers']} | {r['reports']} | {r['reviews']} |")
        for k, v in dict(calls=n, usd=dollars(r["main"]) + dollars(r["sub"]), tasks=r["tasks"], answers=r["answers"], reports=r["reports"],
                         read=sum(x["read"] for x in r["main"]), write=sum(x["w1h"] + x["w5m"] for x in r["main"]),
                         output=sum(x["out"] for x in r["main"])).items():
            tot[k] += v
    print()
    print("| session | first call | $ read | $ write | $ output | calls over 120k | tokens read over 120k | over 150k |")
    print("|---|---|---|---|---|---|---|---|")
    for r in rows:
        sp, ctx, reads = split(r["main"]), r["ctx"], [x["read"] for x in r["main"]]
        tr = sum(reads) or 1
        over = lambda lim: sum(rd for c, rd in zip(ctx, reads) if c > lim) / tr  # noqa: E731
        n = max(len(ctx), 1)
        print(f"| {r['name']} | {ctx[0] // 1000 if ctx else 0}k | {sp['read']:.1f} | {sp['write']:.1f} | {sp['output']:.1f} | "
              f"{sum(c > 120000 for c in ctx) * 100 // n}% | {over(120000):.0%} | {over(150000):.0%} |")
    print()
    print("| session | $ of its first 8 calls (a reset's start) | context at call 9 | $ per call after | calls to reach 150k |")
    print("|---|---|---|---|---|")
    for r in rows:
        m, ctx = r["main"], r["ctx"]
        if len(m) < 20:
            continue
        head, tail = dollars(m[:8]), dollars(m[8:]) / (len(m) - 8)
        reach = next((i for i, c in enumerate(ctx) if c >= 150000), None)
        print(f"| {r['name']} | {head:.2f} | {ctx[8] // 1000}k | {tail:.3f} | {reach if reach is not None else 'never'} |")
    print()
    # What an earlier reset would have cost, from the same calls: a session cut where its context first reaches L repeats its
    # first calls (the start of a reset, measured above, priced as its first 8 calls) for each extra session. The calls after
    # the cut are those of the sessions' own first part, so the figure is $ per call of a session that stops at L.
    print("| reset at | calls per session | $ per call, no reset cost | $ per call with a reset's start | $ per session |")
    print("|---|---|---|---|---|")
    for lim in (100000, 120000, 150000):
        n = usd = 0
        starts = []
        for r in rows:
            m, ctx = r["main"], r["ctx"]
            if len(m) < 20:
                continue
            cut = next((i for i, c in enumerate(ctx) if c >= lim), len(m))
            n += cut
            usd += dollars(m[:cut])
            starts.append(dollars(m[:8]) - 8 * dollars(m[8:cut]) / max(cut - 8, 1))
        k = len([r for r in rows if len(r["main"]) >= 20])
        extra = sum(starts) / len(starts)  # a reset's start over the calls it replaces
        print(f"| {lim // 1000}k | {n / k:.0f} | {usd / n:.4f} | {(usd + k * extra) / n:.4f} | {(usd + k * extra) / k:.2f} |")
    print()
    if tot["tasks"]:
        t = tot["tasks"]
        print(f"All {len(rows)} sessions: {tot['calls']:.0f} calls, ${tot['usd']:.0f} (main and subagents), {t:.0f} tasks sent, "
              f"{tot['answers']:.0f} answers, {tot['reports']:.0f} reports read.")
        print(f"Per task sent: {tot['calls'] / t:.0f} calls, ${tot['usd'] / t:.1f}, {tot['read'] / t / 1e6:.1f}M tokens read, "
              f"{tot['write'] / t / 1e3:.0f}k written, {tot['output'] / t / 1e3:.0f}k output.")
        w = t + tot["answers"]
        print(f"Per piece of work sent to a worker (task or answer): {tot['calls'] / w:.0f} calls, ${tot['usd'] / w:.1f}.")


if __name__ == "__main__":
    main()
