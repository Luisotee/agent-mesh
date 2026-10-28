# per main session: ordered events for re-read analysis
exec(open('agg.py').read())
import pickle
ROOT=os.path.expanduser('~/.claude/projects')
READ_BASH=re.compile(r'^\s*(?:cd [^;&]+(?:&&|;)\s*)?(cat|sed|head|tail|grep|rg|ugrep|ls|find|git (?:show|diff|log|grep|blame)|wc|awk|nl|less)\b')
PATH_RE=re.compile(r'(?:[\w.-]+/)+[\w.-]+\.\w+|\b[\w-]+\.(?:py|md|ts|tsx|js|json|sh|yml|yaml|toml|txt)\b')
def keys_of(name,inp):
    if name=='Read': return ('read',[inp.get('file_path','')])
    if name in('Grep','Glob'): return ('search',[json.dumps(inp,sort_keys=True)])
    if name=='Bash':
        cmd=inp.get('command','')
        m=READ_BASH.match(cmd)
        if m and not re.search(r'mesh |pytest|npm |pnpm |uvx|>\s*\S',cmd):
            ps=PATH_RE.findall(cmd)
            return ('bashread',ps or [cmd.strip()[:200]])
        return ('other',[])
    return ('other',[])
S2={}
for sid,s in sess.items():
    if s['sub']: continue
    ev=[]; seen=set()
    for l in open(os.path.join(ROOT,s['file']),errors='replace'):
        if '"type":"api-request' in l[:60]: continue
        try: d=json.loads(l)
        except: continue
        t=d.get('type')
        if t=='system' and d.get('subtype')=='compact_boundary':
            md=d.get('compactMetadata') or {}
            ev.append(('compact',d['timestamp'],md.get('trigger'),md.get('preTokens'),md.get('postTokens'),d.get('entrypoint')))
        elif t=='assistant':
            m=d['message']; u=m.get('usage')
            tus=[]
            for b in m.get('content') or []:
                if isinstance(b,dict) and b.get('type')=='tool_use':
                    k,ps=keys_of(b.get('name'),b.get('input') or {})
                    done = b.get('name')=='Bash' and re.search(r'mesh send\b.*--type (done|failed)',(b.get('input') or {}).get('command',''))
                    tus.append((b['id'],b.get('name'),k,ps,bool(done)))
            mid=m.get('id')
            if u and mid not in seen and m.get('model')!='<synthetic>':
                seen.add(mid)
                r=dict(m=m.get('model'),i=u.get('input_tokens',0),w=u.get('cache_creation_input_tokens',0),w5=(u.get('cache_creation') or {}).get('ephemeral_5m_input_tokens',0),w1=(u.get('cache_creation') or {}).get('ephemeral_1h_input_tokens',0),r=u.get('cache_read_input_tokens',0),o=u.get('output_tokens',0))
                ev.append(('call',d['timestamp'],cost(r),r['r']+r['w']+r['i'],r['o'],tus))
            elif tus and ev and ev[-1][0]=='call':
                ev[-1][5].extend(tus)
        elif t=='user':
            c=d.get('message',{}).get('content')
            if isinstance(c,list):
                for b in c:
                    if isinstance(b,dict) and b.get('type')=='tool_result':
                        cc=b.get('content'); n=len(cc) if isinstance(cc,str) else len(json.dumps(cc))
                        if isinstance(cc,list) and any(isinstance(x,dict) and x.get('type')=='image' for x in cc): n=4500
                        ev.append(('result',d['timestamp'],b.get('tool_use_id'),n))
            tx=c if isinstance(c,str) else ' '.join(b.get('text','') for b in (c or []) if isinstance(b,dict) and b.get('type')=='text')
            if '📬 [mesh]' in (tx or ''):
                ev.append(('bell',d['timestamp'],'again' in tx, tx[:150]))
            elif tx and not d.get('isMeta') and (d.get('origin') or {}).get('kind')=='human':
                ev.append(('human',d['timestamp']))
    S2[sid]=ev
pickle.dump(S2,open('events.pkl','wb'))
print(len(S2))
