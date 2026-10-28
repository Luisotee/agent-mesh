# hidden checker: python3 -I check.py <repo copy>
import sys, importlib
sys.path.insert(0, sys.argv[1] + "/src")
h = importlib.import_module("humanize")
p = getattr(h, "parse_size", None)
if p is None:
    print("no humanize.parse_size"); sys.exit()
ok = [("3.0 MB", 3_000_000), ("2.9 KiB", round(2.9 * 1024)), ("2.9K", round(2.9 * 1024)), ("300B", 300),
      ("1 Byte", 1), ("10 Bytes", 10), ("-4.0 KiB", -4096), ("  1.5GB ", 1_500_000_000), ("1 KB", 1000),
      ("1kB", 1000), ("2048", 2048), ("1.0 QB", 10**30), ("1.0 QiB", 1024**10), ("3M", 3 * 1024**2), ("0B", 0)]
bad = ["", "abc", "1 XB", "1 kb", "1 mib", "1.2.3 MB", "MB", "1 K iB"]
n = 0
for s, want in ok:
    try:
        got = p(s)
    except Exception as e:
        got = repr(e)
    n += got == want
    if got != want: print("  FAIL", repr(s), "want", want, "got", got)
for s in bad:
    try:
        print("  FAIL no error", repr(s), p(s))
    except ValueError as e:
        n += 1
        if s and s.strip() and s not in str(e): print("  note: message lacks input", repr(s), str(e)[:60])
    except Exception as e:
        print("  FAIL wrong exc", repr(s), repr(e))
print("score", f"{n}/{len(ok)+len(bad)}", "in __all__:", "parse_size" in getattr(h, "__all__", []))
