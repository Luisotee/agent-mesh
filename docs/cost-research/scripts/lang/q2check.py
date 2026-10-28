import re, sys
from datetime import datetime
src = open(sys.argv[1]).read()
m = re.search(r"```(?:python)?\n(.*?)```", src, re.S)
code = m.group(1) if m else src
ns = {}
exec(code, ns)
f = ns["wait_seconds"]
now = datetime(2026, 10, 27, 14, 0, 0)
cases = [("30m", 1800), ("2h", 7200), ("1h30m", 5400), ("5h", 14400), ("14:30", 1800), ("13:00", 14400),
         ("2026-10-27T15:00:00", 3600), ("2026-10-28T09:00", 14400)]
bad = ["2026-10-27T13:00:00", "soon", "", "25:00", "0x"]
ok = 0
for spec, want in cases:
    try:
        got = f(spec, now)
    except Exception as e:
        got = repr(e)
    ok += got == want
    if got != want: print("  FAIL", spec, "want", want, "got", got)
for spec in bad:
    try:
        got = f(spec, now); print("  FAIL no error for", repr(spec), got)
    except ValueError:
        ok += 1
    except Exception as e:
        print("  FAIL wrong exc", repr(spec), repr(e))
print(sys.argv[1].rsplit("/",1)[-1], f"{ok}/{len(cases)+len(bad)}")
