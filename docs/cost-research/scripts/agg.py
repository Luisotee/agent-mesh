exec(open('classify.py').read())
from datetime import datetime,timezone
M=[json.loads(l) for l in open('msgs.jsonl')]
sess={s['s']:s for s in S.values()}
K={sid:kind(s) for sid,s in sess.items()}
