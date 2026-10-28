import sys
from tokenizers import Tokenizer
tok = Tokenizer.from_file(sys.argv[1])
for d in sys.argv[2:]:
    for f in ["en", "en-terse", "zh", "ja", "ko", "es", "pt", "de", "ru"]:
        t = open(f"{d}/{f}.md", encoding="utf-8").read()
        print(d.rsplit("/", 1)[-1], f, len(tok.encode(t, add_special_tokens=False).ids))
