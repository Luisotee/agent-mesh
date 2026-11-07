# First-call context (the static prefix plus the first message) per repo and kind, per window: WINDOWS=A..B,C..D python3 prefix_repo.py
exec(open('agg.py').read())
import statistics
WINS=[w.split('..') for w in os.environ['WINDOWS'].split(',')]
REPOS=os.environ.get('REPOS','city-tour-app,agent-mesh,walklore-landing,curupira-bot,castanha-bot,ai-boilerplate,career-ops,caminho-do-fogo,app-fogo').split(',')
first={}
for r in sorted(M,key=lambda r:r['t']):
    if r['s'] not in first: first[r['s']]=r
out=collections.defaultdict(list)
for sid,r in first.items():
    s=sess[sid]
    if s['sub']: continue
    w=next((a for a,b in WINS if _when(a)<=r['t']<_when(b)),None)
    if not w: continue
    cwd=(s.get('cwd') or '')+'|'+s['proj']
    repo=next((x for x in REPOS if x in cwd),None)
    if not repo: continue
    k=K[sid]; k='worker' if k.startswith('worker') else k
    out[(repo,k,w)].append(r['i']+r['w']+r['r'])
for repo in REPOS:
    for k in ['worker','reviewer','mesh root','plain user']:
        row=[(w,out.get((repo,k,w))) for w,_ in WINS]
        if not any(v for _,v in row): continue
        print(f"{repo:18} {k:11}", '  '.join(f"{w}: {int(statistics.median(v))//1000 if v else '-'}k (n={len(v or [])})" for w,v in row))
