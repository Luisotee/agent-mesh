exec(open('agg.py').read())
import pickle
S2=pickle.load(open('events.pkl','rb'))
def ts(x): return datetime.fromisoformat(x.replace('Z','+00:00')).timestamp()
def window_stats(pre_ev, post_ev):
    pre=set()
    for e in pre_ev:
        if e[0]=='call':
            for (_id,n,k,ps,dn) in e[5]:
                if k!='other': pre.update((k,p) for p in ps)
    res={}
    for e in post_ev:
        if e[0]=='result': res[e[2]]=e[3]
    calls=[e for e in post_ev if e[0]=='call']
    st=collections.Counter(); st['calls']=len(calls); st['$']=sum(e[2] for e in calls)
    for i,e in enumerate(calls):
        tus=e[5]
        if not tus: continue
        rr=0
        for (_id,n,k,ps,dn) in tus:
            if k=='other': continue
            st['readops']+=1
            if any((k,p) in pre for p in ps):
                rr+=1; st['rereads']+=1
                tok=res.get(_id,0)/2.7
                st['reread_tok']+=tok
                st['$reread']+=tok*(8+0.2*(len(calls)-i-1))/1e6
            for p in ps: pre.add((k,p))
        st['$reread']+=e[2]*rr/len(tus)
        st['reread_calls']+=rr/len(tus)
    # calls/time to first done
    t0=post_ev[0][1] if post_ev else None
    for e in calls:
        if any(x[4] for x in e[5]):
            st['to_done_calls']=calls.index(e)+1; st['to_done_min']=(ts(e[1])-ts(t0))/60; break
    return st
groups=collections.defaultdict(list)
for sid,ev in S2.items():
    k=K.get(sid,'?')
    kk='root' if k=='mesh root' else 'worker' if k.startswith('worker') else 'peer' if k=='peer asker' else 'reviewer' if k=='reviewer' else 'asker' if k.startswith('asker') else 'other'
    idx=[i for i,e in enumerate(ev) if e[0]=='compact']
    for j,i in enumerate(idx):
        end=idx[j+1] if j+1<len(idx) else len(ev)
        # for mesh compaction on resume, the post window starts at the next bell
        post=ev[i+1:end]
        groups[(kk,'compacted',ev[i][2])].append(window_stats(ev[:i],post))
    # control: resume bells ('again') with no compaction in the previous 15 min
    for i,e in enumerate(ev):
        if e[0]=='bell' and e[2]:
            prevc=[x for x in ev[:i] if x[0]=='compact' and ts(e[1])-ts(x[1])<900]
            if prevc: continue
            nb=[j for j in range(i+1,len(ev)) if (ev[j][0]=='bell' and ev[j][2]) or ev[j][0]=='compact']
            end=nb[0] if nb else len(ev)
            ctx=max([x[3] for x in ev[:i] if x[0]=='call'] or [0])
            groups[(kk,'resumed, not compacted','ctx>150k' if ctx>150000 else 'ctx<=150k')].append(window_stats(ev[:i],ev[i:end]))
def show(k,v):
    n=len(v); a=collections.Counter()
    for s in v: a.update(s)
    dn=[s for s in v if 'to_done_calls' in s]
    print(f"{str(k):45} n={n:3} calls/win={a['calls']/n:5.1f} $/win={a['$']/n:5.2f} readops={a['readops']/n:5.1f} rereads={a['rereads']/n:5.1f} ({100*a['rereads']/max(a['readops'],1):.0f}%) reread$={a['$reread']/n:4.2f} ({100*a['$reread']/max(a['$'],1e-9):.0f}% of win) to-done calls={sum(s['to_done_calls'] for s in dn)/max(len(dn),1):.0f} min={sum(s['to_done_min'] for s in dn)/max(len(dn),1):.0f} (n={len(dn)})")
for k in sorted(groups): show(k,groups[k])
print('--- compactions: kinds and pre/post tokens')
pp=collections.defaultdict(list)
for sid,ev in S2.items():
    for e in ev:
        if e[0]=='compact': pp[(K.get(sid),e[2],e[5])].append((e[3] or 0,e[4] or 0))
for k,v in pp.items(): print(k,len(v),'pre median',sorted(x[0] for x in v)[len(v)//2]//1000,'k post median',sorted(x[1] for x in v)[len(v)//2]//1000,'k')
print('--- root resets (/clear): new session vs old')
for f in glob.glob(ST+'/*/log.jsonl')+glob.glob(ST+'/archive/*/log.jsonl'):
    for l in open(f):
        try: e=json.loads(l)
        except Exception: continue
        if e.get('event')=='reset':
            o,n=e.get('old_session'),e.get('new_session')
            if o in S2 and n in S2:
                st=window_stats(S2[o],S2[n]); oc=[x for x in S2[o] if x[0]=='call']
                print(e['time'][:16], 'old ctx',max(x[3] for x in oc)//1000,'k', 'new: calls',st['calls'],f"${st['$']:.2f}",'readops',st['readops'],'rereads',st['rereads'],f"reread$ {st['$reread']:.2f}", 'first ctx', next((x[3] for x in S2[n] if x[0]=='call'),0)//1000,'k')
            else: print(e['time'][:16],'sessions not found',o in S2,n in S2)
