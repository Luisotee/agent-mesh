exec(open('agg.py').read())
import sys
def text_of(c):
    if isinstance(c,str): return c
    if isinstance(c,list): return ' '.join(b.get('text','') for b in c if isinstance(b,dict) and b.get('type')=='text')
    return ''
T0=datetime(2026,10,14,tzinfo=timezone.utc).timestamp()
ROOT=os.path.expanduser('~/.claude/projects')
# per session: attribute each API call to the trigger of its turn
out=collections.defaultdict(lambda: collections.Counter())
samples=collections.defaultdict(list)
for sid,s in sess.items():
    k=K[sid]
    if s['sub']: continue
    trig='?'
    seen=set()
    for l in open(os.path.join(ROOT,s['file']),errors='replace'):
        h=l[:60]
        if '"type":"api-request' in h: continue
        try: d=json.loads(l)
        except: continue
        if d.get('type')=='user':
            if True:
                c=d.get('message',{}).get('content')
                if isinstance(c,list) and any(isinstance(b,dict) and b.get('type')=='tool_result' for b in c): continue
                tx=text_of(c) if not isinstance(c,str) else c
                o=(d.get('origin') or {}).get('kind') or d.get('turnOrigin') or '-'
                if '<task-notification>' in tx: trig='task-notification'
                elif '📬 [mesh]' in tx: trig='doorbell'
                elif 'Stop hook' in tx or 'hook' in o: trig='hook:'+o
                elif d.get('isMeta'): continue
                else: trig='origin:'+str(o)
                if len(samples[trig])<3: samples[trig].append(tx[:120])
        elif d.get('type')=='assistant':
            m=d['message']; u=m.get('usage')
            if not u or m.get('id') in seen or m.get('model')=='<synthetic>': continue
            seen.add(m.get('id'))
            if datetime.fromisoformat(d['timestamp'].replace('Z','+00:00')).timestamp()<T0: continue
            r=dict(m=m.get('model'),i=u.get('input_tokens',0),w=u.get('cache_creation_input_tokens',0),w5=(u.get('cache_creation') or {}).get('ephemeral_5m_input_tokens',0),w1=(u.get('cache_creation') or {}).get('ephemeral_1h_input_tokens',0),r=u.get('cache_read_input_tokens',0),o=u.get('output_tokens',0))
            out[k][trig]+=cost(r); out[k]['n:'+trig]+=1
for k in ['mesh root','worker pane large','worker bg small','reviewer','peer asker','plain user']:
    c=out[k]; t=sum(v for x,v in c.items() if not x.startswith('n:'))
    print(k, f'${t:.0f}', {x:(round(v),c['n:'+x]) for x,v in c.most_common() if not x.startswith('n:')})
for k,v in samples.items(): print(k, v)
