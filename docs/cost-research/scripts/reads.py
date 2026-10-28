exec(open('agg.py').read())
ROOT=os.path.expanduser('~/.claude/projects')
T0=datetime(2026,10,20,tzinfo=timezone.utc).timestamp()
pat=re.compile(r'(ORCHESTRATE\.md|PROTOCOL\.md|PLAN_TEMPLATE\.md|design\.md|SKILL\.md|mesh (?:status|inbox|watch|brief|peers|usage|whoami|dir|log)\b)')
res=collections.defaultdict(collections.Counter)
big=collections.defaultdict(collections.Counter)
for sid,s in sess.items():
    p=os.path.join(ROOT,s['file'])
    if os.path.getmtime(p)<T0: continue
    k=K[sid]; k='subagent' if k.startswith('subagent') else k
    pending={}; ncalls=0; calls_after=[]
    for l in open(p,errors='replace'):
        if '"type":"api-request' in l[:60]: continue
        try: d=json.loads(l)
        except: continue
        if d.get('type')=='assistant':
            ncalls+=1
            for b in d['message'].get('content') or []:
                if isinstance(b,dict) and b.get('type')=='tool_use':
                    inp=json.dumps(b.get('input'))
                    m=pat.search(inp)
                    pending[b['id']]=(m.group(1) if m else b.get('name'),ncalls)
        elif d.get('type')=='user':
            c=d.get('message',{}).get('content')
            if isinstance(c,list):
                for b in c:
                    if isinstance(b,dict) and b.get('type')=='tool_result' and b.get('tool_use_id') in pending:
                        what,at=pending.pop(b['tool_use_id'])
                        cc=b.get('content'); n=len(cc) if isinstance(cc,str) else len(json.dumps(cc))
                        calls_after.append((what,n,at))
    for what,n,at in calls_after:
        key=re.sub(r'^mesh (\w+).*',r'mesh \1',what) if what else '?'
        res[k][key+' n']+=1; res[k][key+' chars']+=n
        # chars x remaining calls -> read cost (chars/3 tokens * 0.2/M), plus write 8/M
        res[k][key+' $']+= n/3*((ncalls-at)*0.2+8)/1e6
        big[k][key]+= n/3*((ncalls-at)*0.2+8)/1e6
for k in ['mesh root','worker pane large','worker bg small','reviewer','peer asker','plain user','subagent']:
    tops=big[k].most_common(10)
    print(k, [(w, round(v,1), res[k][w+' n'], int(res[k][w+' chars']/max(res[k][w+' n'],1))) for w,v in tops])
