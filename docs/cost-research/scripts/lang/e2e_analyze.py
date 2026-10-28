import glob, json, os, re, subprocess, sys
E = sys.argv[1]
CJK = re.compile(r"[㐀-鿿　-〿＀-￯]")
def cjk_share(t):
    letters = [c for c in t if c.isalpha() or CJK.match(c)]
    return sum(1 for c in letters if CJK.match(c)) / max(1, len(letters))
# API-equivalent prices, Opus 5.5 per token (cost study): in 4, 1h write 8, 5m write 5, read 0.2, out 20 per MTok
rows = []
for d in sorted(glob.glob(f"{E}/runs/*")):
    rid = os.path.basename(d)
    try:
        out = json.load(open(f"{d}/out.json"))
    except Exception as e:
        print(rid, "no out.json", e); continue
    sid = open(f"{d}/session").read().strip()
    tr = glob.glob(os.path.expanduser(f"~/.claude/projects/*/{sid}.jsonl"))
    calls, seen, tools, text, think_chars = {}, set(), {}, "", 0
    for line in open(tr[0]) if tr else []:
        try: m = json.loads(line)
        except ValueError: continue
        if m.get("type") != "assistant": continue
        msg = m["message"]; mid = msg.get("id")
        if msg.get("usage"): calls[mid] = msg["usage"]
        for b in msg.get("content", []):
            if b.get("type") == "tool_use": tools[b["name"]] = tools.get(b["name"], 0) + 1
            elif b.get("type") == "text": text += b["text"]
            elif b.get("type") == "thinking": think_chars += len(b.get("thinking", ""))
    inp = sum(u.get("input_tokens", 0) for u in calls.values())
    w1h = sum(u.get("cache_creation", {}).get("ephemeral_1h_input_tokens", 0) for u in calls.values())
    w5m = sum(u.get("cache_creation", {}).get("ephemeral_5m_input_tokens", 0) for u in calls.values())
    rd = sum(u.get("cache_read_input_tokens", 0) for u in calls.values())
    outp = sum(u.get("output_tokens", 0) for u in calls.values())
    usd = (inp * 4 + w1h * 8 + w5m * 5 + rd * 0.2 + outp * 20) / 1e6
    rep = f"{E}/reports/{rid}.md"
    rtext = open(rep).read() if os.path.exists(rep) else ""
    chk = subprocess.run([sys.executable, "-I", f"{E}/check.py", f"{d}/repo"], capture_output=True, text=True).stdout.strip()
    rows.append(dict(id=rid, calls=len(calls), turns=out.get("num_turns"), inp=inp, w=w1h + w5m, rd=rd, out=outp, usd=usd,
                     cli_usd=out.get("total_cost_usd"), tools=sum(tools.values()), text_cjk=cjk_share(text),
                     report_chars=len(rtext), report_cjk=cjk_share(rtext), score=chk.splitlines()[-1] if chk else "?",
                     fails=[l for l in chk.splitlines() if "FAIL" in l]))
for r in rows:
    print(f"{r['id']:7} calls {r['calls']:3} tools {r['tools']:3} write {r['w']:7} read {r['rd']:9} out {r['out']:6} "
          f"${r['usd']:.2f} (cli ${r['cli_usd'] or 0:.2f})  text CJK {r['text_cjk']:.0%}  report {r['report_chars']}ch CJK {r['report_cjk']:.0%}  {r['score']}")
    for f in r["fails"]: print("        ", f)
json.dump(rows, open(f"{E}/rows.json", "w"), indent=1)
