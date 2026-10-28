exec(open('agg.py').read())
ROOT=os.path.expanduser('~/.claude/projects')
T0=datetime(2026,10,21,tzinfo=timezone.utc).timestamp()
CPT=3.3   # prose chars per token (assumption)
MESHCMD=re.compile(r'(?:^|[;&|(\s])mesh (\w+)')
FIXED_PATH=re.compile(r'\.config/agent-mesh/|/skills/(delegate-|check-plan|find-peers|harvest-peers)')
RUNDIR=re.compile(r'agent-mesh/m[0-9a-f]{4}/(plans|reports|tasks|inbox)|\$\(mesh dir\)/(plans|reports|tasks)|board\.md|notes\.md')
def ts(x): return datetime.fromisoformat(x.replace('Z','+00:00')).timestamp()
agg=collections.Counter(); out_agent=0.0
for sid,s in sess.items():
    p=os.path.join(ROOT,s['file'])
    if os.path.getmtime(p)<T0: continue
    items=[]  # (call_index_at_arrival, chars, class)
    pend={}; ncall=0; seen=set(); calls_ts=[]; compact_at=[]
    for l in open(p,errors='replace'):
        if '"type":"api-request' in l[:60]: continue
        try: d=json.loads(l)
        except: continue
        t=d.get('type')
        if t=='system' and d.get('subtype')=='compact_boundary': compact_at.append(ncall); continue
        if t=='assistant':
            m=d['message']
            if m.get('id') not in seen and m.get('usage') and m.get('model')!='<synthetic>':
                seen.add(m.get('id')); ncall+=1; calls_ts.append(ts(d['timestamp']))
            for b in m.get('content') or []:
                if not isinstance(b,dict) or b.get('type')!='tool_use': continue
                inp=b.get('input') or {}; name=b.get('name')
                cls=None
                if name=='Bash':
                    cmd=inp.get('command','')
                    mm=MESHCMD.search(cmd)
                    if FIXED_PATH.search(cmd): cls='fixed: protocol/skill read'
                    elif RUNDIR.search(cmd) and re.match(r'\s*(cat|sed|head|tail|less)',cmd): cls='agent: plan/report/task read'
                    elif mm: cls='agent: mesh inbox' if mm.group(1)=='inbox' else 'fixed: mesh cmd output'
                    if mm and mm.group(1)=='send':
                        bm=re.search(r'--body\s+(["\'])(.*?)\1',cmd,re.S)
                        if bm and ts(d['timestamp'])>=T0: out_agent+=len(bm.group(2))
                elif name=='Read':
                    fp=inp.get('file_path','')
                    if FIXED_PATH.search(fp): cls='fixed: protocol/skill read'
                    elif RUNDIR.search(fp): cls='agent: plan/report/task read'
                elif name=='Write':
                    fp=inp.get('file_path','')
                    if RUNDIR.search(fp) and ts(d['timestamp'])>=T0: out_agent+=len(inp.get('content',''))
                if cls: pend[b['id']]=cls
        elif t=='user':
            if ts(d['timestamp'])<T0: continue
            c=d['message'].get('content')
            if isinstance(c,list):
                for b in c:
                    if isinstance(b,dict) and b.get('type')=='tool_result' and b.get('tool_use_id') in pend:
                        cc=b.get('content'); n=len(cc) if isinstance(cc,str) else len(json.dumps(cc))
                        items.append((ncall,n,pend.pop(b['tool_use_id'])))
                tx=' '.join(b.get('text','') for b in c if isinstance(b,dict) and b.get('type')=='text')
            else: tx=c or ''
            if '📬 [mesh]' in tx: items.append((ncall,len(tx),'fixed: doorbell'))
            elif tx.startswith('Stop hook feedback') : items.append((ncall,len(tx),'fixed: hook output'))
            elif 'Base directory for this skill' in tx and re.search(r'delegate-|check-plan|find-peers|harvest-peers',tx[:300]): items.append((ncall,len(tx),'fixed: protocol/skill read'))
        elif t=='attachment':
            a=d.get('attachment') or {}
            if 'agent-mesh reminder' in json.dumps(a)[:3000]:
                items.append((ncall,len(json.dumps(a.get('content',''))),'fixed: hook output'))
    # cost each item
    bounds=compact_at+[10**9]
    for (ci,n,cls) in items:
        nxt=min(b for b in bounds if b>ci) if any(b>ci for b in bounds) else 10**9
        later=min(ncall,nxt)-ci
        tok=n/CPT
        agg[cls]+=tok*(8+0.2*max(later,0))/1e6
        agg[cls+' tok']+=tok
# static prefix share of mesh text: global snippet + mesh skill descriptions, every main/worker call
pref_chars=1274+6673
calls7=sum(1 for r in M if r['t']>=T0 and not K[r['s']].startswith('subagent') and K[r['s']]!='headless -p')
agg['fixed: prefix (skill descriptions, CLAUDE.md snippet)']=calls7*pref_chars/CPT*0.2/1e6
agg['agent: written (output tokens)']=out_agent/CPT*20/1e6
tot=1614
f=sum(v for k,v in agg.items() if k.startswith('fixed') and not k.endswith('tok'))
a=sum(v for k,v in agg.items() if k.startswith('agent') and not k.endswith('tok'))
for k,v in sorted(agg.items(),key=lambda x:-x[1]):
    if not k.endswith('tok'): print(f'{k:55} ${v:6.1f}  {100*v/tot:4.1f}%  tokens {agg.get(k+" tok",0)/1e6:.2f}M')
print(f'mesh-written ${f:.0f} ({100*f/tot:.1f}%)  agent-written ${a:.0f} ({100*a/tot:.1f}%)  total {100*(f+a)/tot:.1f}%')
for cut in (0.2,0.4,0.6): print(f'cut {int(cut*100)}% of those tokens -> saves {100*cut*(f+a)/tot:.1f}% of total (${cut*(f+a):.0f}/wk)')
