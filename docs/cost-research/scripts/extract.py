import json,glob,os,sys,re
from datetime import datetime
ROOT=os.path.expanduser('~/.claude/projects')
out=open('msgs.jsonl','w'); sess_out=open('sessions.jsonl','w')
def ts(s):
    return datetime.fromisoformat(s.replace('Z','+00:00')).timestamp()
def text_of(c):
    if isinstance(c,str): return c
    if isinstance(c,list):
        return ' '.join(b.get('text','') for b in c if isinstance(b,dict) and b.get('type')=='text')
    return ''
files=glob.glob(ROOT+'/*/*.jsonl')+glob.glob(ROOT+'/*/*/subagents/*.jsonl')
seen=set()  # across files: a copied or forked session repeats its original's message ids
for f in files:
    rel=os.path.relpath(f,ROOT); proj=rel.split('/')[0]
    sub='/subagents/' in rel
    sid=os.path.basename(f)[:-6]
    first_user=None; ep=None; n=0; prev_t=None; compacts=[]; reqsrc={}
    user_turns=0; meshcmd={}; bells=0; cwd=None; tool_results_chars=0; model_set=set()
    for l in open(f,errors='replace'):
        h=l[:60]
        if '"type":"api-request"' in h:
            try: d=json.loads(l)
            except: continue
            reqsrc[d.get('id')]=(d.get('querySource'), (d.get('params') or {}).get('output_config',{}).get('effort'))
            continue
        if '"type":"api-request-blob"' in h or '"type":"api-request-shape"' in h: continue
        try: d=json.loads(l)
        except: continue
        t=d.get('type')
        if ep is None and d.get('entrypoint'): ep=d['entrypoint']
        if t=='user':
            m=d.get('message',{}); c=m.get('content')
            if d.get('isCompactSummary'): compacts.append(d.get('timestamp'))
            if isinstance(c,list) and any(isinstance(b,dict) and b.get('type')=='tool_result' for b in c):
                for b in c:
                    if isinstance(b,dict) and b.get('type')=='tool_result':
                        cc=b.get('content'); tool_results_chars+=len(cc) if isinstance(cc,str) else len(json.dumps(cc))
                continue
            if d.get('isMeta'): continue
            tx=text_of(c)
            if cwd is None: cwd=d.get('cwd')
            if '📬 [mesh]' in (tx or ''): bells+=1
            if tx and not tx.startswith('<local-command') and not tx.startswith('<command-name>/'):
                user_turns+=1
                if first_user is None: first_user=tx[:400]
        elif t=='assistant':
            m=d.get('message',{}); u=m.get('usage')
            for b in (m.get('content') or []):
                if isinstance(b,dict) and b.get('type')=='tool_use' and b.get('name')=='Bash':
                    cmd=(b.get('input') or {}).get('command','')
                    for mm in re.finditer(r'(?:^|[;&|(\s])mesh (\w+)',cmd):
                        meshcmd[mm.group(1)]=meshcmd.get(mm.group(1),0)+1
            mid=m.get('id') or d.get('requestId')
            if not u or mid in seen: continue
            seen.add(mid)
            if m.get('model')=='<synthetic>': continue
            cc=u.get('cache_creation') or {}
            rs=reqsrc.get(d.get('requestRef'),(None,None))
            r=dict(s=sid,t=ts(d['timestamp']),m=m.get('model'),e=d.get('effort') or rs[1],q=rs[0],
                   i=u.get('input_tokens',0),w=u.get('cache_creation_input_tokens',0),
                   w1=cc.get('ephemeral_1h_input_tokens',0),w5=cc.get('ephemeral_5m_input_tokens',0),
                   r=u.get('cache_read_input_tokens',0),o=u.get('output_tokens',0))
            out.write(json.dumps(r)+'\n'); n+=1; model_set.add(m.get('model'))
    sess_out.write(json.dumps(dict(s=sid,proj=proj,sub=sub,file=rel,ep=ep,first=first_user,n=n,compacts=compacts,user_turns=user_turns,trc=tool_results_chars,size=os.path.getsize(f),mesh=meshcmd,bells=bells,cwd=cwd))+'\n')
