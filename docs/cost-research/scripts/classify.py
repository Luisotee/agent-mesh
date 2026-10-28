import json,glob,os,collections,re
ST=os.path.expanduser('~/.local/state/agent-mesh')
PRICE={'opus':(4,5,8,0.2,20),'sonnet':(2,2.5,4,0.2,10),'haiku':(0.1,0.125,0.2,0.01,0.5),'fable':(10,12.5,20,0.25,50)}
def fam(m):
    m=m or ''
    for k in PRICE:
        if k in m: return k
    return 'opus'
def cost(r):
    p=PRICE[fam(r['m'])]
    w5=r['w5']; w1=r['w1']
    if w5+w1==0: w1=r['w']  # unknown split -> 1h
    return (r['i']*p[0]+w5*p[1]+w1*p[2]+r['r']*p[3]+r['o']*p[4])/1e6
agents={}
for f in glob.glob(ST+'/*/run.json')+glob.glob(ST+'/archive/*/run.json'):
    try: d=json.load(open(f))
    except: continue
    for n,a in d.get('agents',{}).items():
        ss=[a.get('session')]+[x if isinstance(x,str) else (x or {}).get('session') for x in (a.get('sessions') or [])]
        for s in ss:
            if s: agents[s]=dict(a,name=n,run=d['id'])
S={}
for l in open('sessions.jsonl'):
    s=json.loads(l); S[(s['s'],s['file'])]=s
bysid={}
for (sid,f),s in S.items():
    if not s['sub']: bysid[sid]=s
def kind(s):
    if s['sub']:
        parent=s['file'].split('/')[1]
        p=bysid.get(parent)
        return 'subagent of '+(kind(p) if p else '?')
    a=agents.get(s['s']); proj=s['proj']; first=s['first'] or ''
    if a:
        if a.get('role')=='root': return 'mesh root'
        if a.get('peer') or a['name'].endswith('-research'): return 'peer asker'
        if a.get('review_heads') or re.search(r'-review-\d+',a['name']): return 'reviewer'
        if a.get('read_only'): return 'asker (question)'
        return 'worker '+(a.get('view') or 'pane')+' '+str(a.get('size'))
    if 'worktrees-peers-' in proj or proj.endswith('-research'): return 'peer asker'
    if re.search(r'-review-\d+',proj): return 'reviewer'
    if 'agent-mesh-worktrees' in proj or first.startswith('📬 [mesh]'): return 'worker (unmapped)'
    if s['ep']=='sdk-cli': return 'headless -p'
    if any(s['mesh'].get(k) for k in('spawn','init','ask','approve','watch')): return 'mesh root'
    if proj.startswith('-tmp-'): return 'tmp/test'
    return 'plain user'
