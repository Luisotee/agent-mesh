# One week's per-task, message and output-style figures, for the ledger's checks.
# Run beside msgs.jsonl / sessions.jsonl (from extract.py) and agg.py / classify.py:
#   mesh cost --since A --until B --agents --json > cost.json
#   SINCE=A UNTIL=B python3 window.py [cost.json]
# Without cost.json it skips $ and calls per task. SKIP_RUNS (comma-separated) leaves runs out; mtest is always out.
exec(open('agg.py').read())
import statistics,sys
T0=SINCE; T1=UNTIL or float('inf')
assert T0, 'set SINCE (and UNTIL)'
SKIP={'mtest'}|set(filter(None,os.environ.get('SKIP_RUNS','').split(',')))
def ts(x): return datetime.fromisoformat(x.replace('Z','+00:00')).timestamp()
def med(v): return int(statistics.median(v)) if v else 0
def mean(v): return int(sum(v)/len(v)) if v else 0

# ── output style and output per call, by the study's kinds ──
print('== calls by kind: share with the Concise output style, output per call (with / without it), first-call prefix')
by=collections.defaultdict(lambda: collections.defaultdict(list)); ctx=collections.defaultdict(list)
first={}
for r in M:
    if not T0<=r['t']<T1: continue
    k=K.get(r['s'],'?'); k='subagent' if k.startswith('subagent') else k
    ctx[k].append(r['i']+r['w']+r['r'])
    by[k]['concise' if 'outputStyle:Concise' in (r.get('q') or '') else 'other'].append(r['o'])
for r in sorted(M,key=lambda r:r['t']):
    if r['s'] not in first: first[r['s']]=r
pref=collections.defaultdict(list)
for sid,r in first.items():
    if T0<=r['t']<T1 and not sess[sid]['sub']: pref[K[sid]].append(r['i']+r['w']+r['r'])
for k in sorted(by,key=lambda k:-len(by[k]['concise'])-len(by[k]['other'])):
    c,o=by[k]['concise'],by[k]['other']; n=len(c)+len(o)
    print(f"  {k:22} calls {n:5}  concise {100*len(c)/n:5.1f}%  out/call {mean(c+o):5} (concise {mean(c):5}, other {mean(o):5})  ctx/call {mean(ctx[k])//1000}k  prefix median {med(pref.get(k,[]))//1000}k (n={len(pref.get(k,[]))})")

# ── run state: tasks, messages, reviews ──
ST=os.path.expanduser('~/.local/state/agent-mesh')
runs={}
for f in glob.glob(ST+'/*/run.json')+glob.glob(ST+'/archive/*/run.json'):
    try: d=json.load(open(f))
    except Exception: continue
    if d['id'] in SKIP: continue
    runs[d['id']]=(d,os.path.dirname(f))
def cat(a,name):
    if a.get('role')=='root': return 'root'
    if a.get('peer') or name.endswith('-research'): return 'peer'
    if a.get('review_heads') or re.search(r'-review-\d+',name): return 'review'
    if a.get('kind')=='copilot' or a.get('view')=='headless': return 'asker' if a.get('read_only') else 'copilot'
    if a.get('read_only'): return 'asker'
    return 'change '+str(a.get('size'))
def body(rdir,to,fn):
    for p in (f'{rdir}/inbox/{to}/read/{fn}',f'{rdir}/inbox/{to}/{fn}'):
        if os.path.exists(p): return re.sub(r'\A---\n.*?\n---\n','',open(p,errors='replace').read(),flags=re.S)  # the header mesh adds
    return None
FIND=re.compile(r'^\s*\d+\.\s+\*\*',re.M)
cnt=collections.defaultdict(collections.Counter); sizes=collections.defaultdict(list)
agent_cat={}; prs=collections.defaultdict(lambda: {'reviewers':set(),'kinds':collections.Counter(),'tasks':0,'wakes':0,'collected':0,'findings':0,'dones':0})
answers=collections.Counter(); events=collections.Counter(); files=collections.defaultdict(list)
for rid,(d,rdir) in runs.items():
    ag=d.get('agents',{}); root=next((n for n,a in ag.items() if a.get('role')=='root'),None)
    for n,a in ag.items(): agent_cat[(rid,n)]=cat(a,n)
    try: lines=open(rdir+'/log.jsonl',errors='replace').read().splitlines()
    except Exception: continue
    for l in lines:
        try: e=json.loads(l)
        except Exception: continue
        if not T0<=ts(e['time'])<T1: continue
        ev=e.get('event')
        if ev in('reset','reset_requested','reserve','nudge','alert','auto_retire','retire','resume','swap','copilot','spawn','init','doorbell_failed'): events[ev]+=1
        if ev=='alert': events['alert: '+str(e.get('why') or e.get('reason') or e.get('code'))[:40]]+=1
        pr=None
        if ev in('send','rung','native','collected'):
            s,t=e.get('sender'),e.get('to')
            m=re.search(r'-review-(\d+)-',(s or '')+'|'+(t or ''))
            if m: pr=(rid,re.sub(r'-review-\d+-.*','',s if '-review-' in (s or '') else t),m.group(1))
        if ev=='send':
            ty=e.get('type'); s,t=e['sender'],e['to']
            worker=t if ty in('task','answer') else s
            c=agent_cat.get((rid,worker),'?')
            cnt[c][ty]+=1
            b=body(rdir,t,e.get('file','')) if e.get('file') else None
            if b is not None: sizes[(c,ty)].append(len(b))
            if ty=='answer' and b is not None and c.startswith('change'):
                answers['approved' if re.match(r'\s*approved\b',b,re.I) else 'other']+=1
            if pr and ty=='task': prs[pr]['tasks']+=1; prs[pr]['reviewers'].add(t); prs[pr]['kinds'][ag.get(t,{}).get('kind','?')]+=1
            if pr and ty=='done' and b is not None: prs[pr]['findings']+=len(FIND.findall(b)); prs[pr]['dones']+=1
        if pr and e.get('to')==root and ev in('rung','native') and e.get('file','').endswith('-done.md'): prs[pr]['wakes']+=1
        if pr and ev=='collected': prs[pr]['collected']+=1
    for sub in('plans','reports'):
        for p in glob.glob(f'{rdir}/{sub}/*.md'):
            if T0<=os.path.getmtime(p)<T1:
                b=os.path.basename(p)
                k='answers file' if re.match(r'(review|peers)-',b) else sub
                files[k].append(os.path.getsize(p))
print(f"\n== runs with events in the window: {events['init']} started; agents spawned {events['spawn']}")
print('== messages by worker category (count; median / mean chars of the body)')
for c in sorted(cnt):
    print(f'  {c:13}', '  '.join(f"{ty} {n} ({med(sizes[(c,ty)])}/{mean(sizes[(c,ty)])})" for ty,n in sorted(cnt[c].items())))
ch=collections.Counter(); [ch.update(v) for c,v in cnt.items() if c.startswith('change')]
nt=max(ch['task'],1)
print(f"== change tasks: {ch['task']} tasks, per task: plans {ch['plan']/nt:.2f}, questions {ch['question']/nt:.2f}, failed {ch['failed']/nt:.2f}, done {ch['done']/nt:.2f}; root answers {dict(answers)} ('other': a plan sent back or a question answered)")
print('== files written (bytes, median / mean / n):', {k:(med(v),mean(v),len(v)) for k,v in files.items()})
n=len(prs) or 1
rv=[len(p['reviewers']) for p in prs.values()]; kinds=collections.Counter(); [kinds.update(p['kinds']) for p in prs.values()]
print(f"== reviews: {len(prs)} PRs; reviewer agents per PR {sum(rv)/n:.2f} (dist {dict(sorted(collections.Counter(rv).items()))}); review tasks per PR {sum(p['tasks'] for p in prs.values())/n:.2f}; tasks by CLI {dict(kinds)}")
print(f"   root wakes by a reviewer's done per PR {sum(p['wakes'] for p in prs.values())/n:.2f}; collected (no wake) per PR {sum(p['collected'] for p in prs.values())/n:.2f}; numbered findings per reviewer done {sum(p['findings'] for p in prs.values())/max(sum(p['dones'] for p in prs.values()),1):.1f}")
print('== events:', dict(sorted(events.items())))

# ── $ and calls per task, from mesh cost --agents --json ──
if len(sys.argv)>1:
    cj=json.load(open(sys.argv[1]))
    usd=collections.Counter(); calls=collections.Counter()
    for a in cj['agents']:
        c=agent_cat.get((a['run'],a['name']))
        if c is None: continue
        usd[c]+=a['usd']; calls[c]+=a['calls']
    print('== $ and calls per task (agents of runs in run state; a task is a `task` message to that category)')
    for c in sorted(usd):
        t=cnt[c]['task']
        if c=='root': continue
        print(f"  {c:13} ${usd[c]:7.2f}  calls {calls[c]:5}  tasks {t:3}  $/task {usd[c]/max(t,1):5.2f}  calls/task {calls[c]/max(t,1):5.1f}")
    repo=collections.Counter()
    for a in cj['agents']:
        if agent_cat.get((a['run'],a['name']),'').startswith('change'): repo[re.sub(r'-[^-]+$','',a['name'])]+=a['usd']
    print('  change $ by agent name prefix (repo, roughly):',{k:round(v) for k,v in repo.most_common(6)})
    tasks_all=sum(cnt[c]['task'] for c in cnt if c!='root')
    mesh_usd=sum(a['usd'] for a in cj['agents'] if a['run'])
    print(f"  mesh $ (every agent of a run) {mesh_usd:.2f} of {cj['usd']:.2f} → ${mesh_usd/max(tasks_all,1):.2f} per worker task")
    print(f"  all worker tasks {tasks_all}; root $ {usd['root']:.2f} → ${usd['root']/max(tasks_all,1):.2f} per task")
