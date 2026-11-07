# First-call context (the static prefix plus the first message) per repo and kind, per window: WINDOWS=A..B,C..D python3 prefix_repo.py
exec(open('agg.py').read())
import statistics
WINS=[w.split('..') for w in os.environ.get('WINDOWS','2026-10-21..2026-10-28,2026-10-28..2026-11-04').split(',')]
REPOS=os.environ.get('REPOS','city-tour-app,agent-mesh,walklore-landing,curupira-bot,castanha-bot,ai-boilerplate,career-ops,caminho-do-fogo,app-fogo').split(',')
def repo_of(sid,s):
    """The run's agent record names the repo; else the path, matched by whole components (every mesh worktree path holds `agent-mesh`)."""
    a=agents.get(sid)
    if a and a.get('repo'): return os.path.basename(a['repo'].rstrip('/'))
    cwd=s.get('cwd') or ''
    m=re.search(r'/agent-mesh/worktrees/(?:peers/)?[^/]+/([^/]+)',cwd)
    if m:  # <run>/<repo>-<slug>: the longest repo name the folder starts with
        return max((x for x in REPOS if m.group(1)==x or m.group(1).startswith(x+'-')),key=len,default=None)
    parts=cwd.split('/')
    return next((x for x in REPOS if x in parts),None)
first={}
for r in sorted(M,key=lambda r:r['t']):
    if r['s'] not in first: first[r['s']]=r
out=collections.defaultdict(list)
for sid,r in first.items():
    s=sess[sid]
    if s['sub']: continue
    w=next((a for a,b in WINS if _when(a)<=r['t']<_when(b)),None)
    if not w: continue
    repo=repo_of(sid,s)
    if not repo: continue
    k=K[sid]; k='worker' if k.startswith('worker') else k
    out[(repo,k,w)].append(r['i']+r['w']+r['r'])
for repo in REPOS:
    for k in ['worker','reviewer','mesh root','plain user']:
        row=[(w,out.get((repo,k,w))) for w,_ in WINS]
        if not any(v for _,v in row): continue
        print(f"{repo:18} {k:11}", '  '.join(f"{w}: {int(statistics.median(v))//1000 if v else '-'}k (n={len(v or [])})" for w,v in row))
