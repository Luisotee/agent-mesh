exec(open('classify.py').read())
from datetime import datetime,timezone
# Window: SINCE / UNTIL (ISO dates, UTC) set it for the scripts that exec this file; unset, each keeps its old default start.
def _when(v): return datetime.fromisoformat(v).replace(tzinfo=timezone.utc).timestamp() if v else None
SINCE=_when(os.environ.get('SINCE')); UNTIL=_when(os.environ.get('UNTIL'))
def in_win(t): return (SINCE is None or t>=SINCE) and (UNTIL is None or t<UNTIL)
M=[json.loads(l) for l in open('msgs.jsonl')]
sess={s['s']:s for s in S.values()}
K={sid:kind(s) for sid,s in sess.items()}
