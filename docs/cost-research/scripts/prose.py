# Per kind and window: chars per call of the assistant's prose (text blocks), tool inputs (of which Write/Edit file bodies), and thinking, deduped by message.id.
# Run beside agg.py and the extract files: WINDOWS=A..B,C..D python3 prose.py (ISO dates, UTC). Thinking is redacted in transcripts, so its tokens show in output but not here.
exec(open('agg.py').read())
import statistics
ROOT=os.path.expanduser('~/.claude/projects')
WINS=[w.split('..') for w in os.environ.get('WINDOWS','2026-10-21..2026-10-28,2026-10-28..2026-11-04').split(',')]
WINS=[(a,b,_when(a),_when(b)) for a,b in WINS]
acc=collections.defaultdict(collections.Counter); done=set(); blocks=set(); seen={}  # message.id -> the file that counts it (across files, as extract.py)
for (sid,f),s in S.items():
    k=K.get(sid) if not s['sub'] else 'subagent'
    k='subagent' if (k or '').startswith('subagent') else k

    for l in open(os.path.join(ROOT,f),errors='replace'):
        if '"type":"assistant"' not in l[:200] and '"type":"assistant"' not in l: continue
        try: d=json.loads(l)
        except Exception: continue
        if d.get('type')!='assistant': continue
        m=d['message']; t=datetime.fromisoformat(d['timestamp'].replace('Z','+00:00')).timestamp()
        w=next((a for a,b,x,y in WINS if x<=t<y),None)
        if not w: continue
        key=(w,k); mid=m.get('id')
        if m.get('model')=='<synthetic>' or seen.setdefault(mid,f)!=f: continue  # a copy of a call another file counts
        if mid not in done and m.get('usage'):
            done.add(mid); acc[key]['calls']+=1; acc[key]['out']+=m['usage'].get('output_tokens',0)
        for b in m.get('content') or []:
            if not isinstance(b,dict): continue
            # one record per content block, all with the call's message.id; a rewritten record repeats a block
            bk=(mid,b.get('id') or hash(json.dumps(b,sort_keys=True)))
            if bk in blocks: continue
            blocks.add(bk)
            if b.get('type')=='text': acc[key]['text']+=len(b.get('text',''))
            elif b.get('type')=='thinking': acc[key]['think']+=len(b.get('thinking',''))
            elif b.get('type')=='tool_use':
                inp=b.get('input') or {}; n=len(json.dumps(inp)); acc[key]['tool']+=n
                if b.get('name') in('Write','Edit','MultiEdit','NotebookEdit'): acc[key]['file']+=n
for a,b,_,_ in WINS:
    print('==',a,'→',b, '(chars per call)')
    for k in ['mesh root','worker pane large','worker bg small','reviewer','subagent','plain user','peer asker']:
        c=acc[(a,k)]; n=max(c['calls'],1)
        print(f"  {k:18} calls {c['calls']:5}  out tok/call {c['out']/n:6.0f}  prose {c['text']/n:5.0f}  tool input {c['tool']/n:5.0f} (file edits {c['file']/n:5.0f})  thinking {c['think']/n:4.0f}")
