#!/bin/bash
# usage: count.sh <model> <abs file|-> ; prints total prompt tokens (prompt via stdin)
S=${WORK:-$PWD}
cd $S/work
{ [ "$2" != "-" ] && cat "$2" && printf '\n\n'; printf 'Reply with: ok'; } | claude -p --model "$1" --output-format json --dangerously-skip-permissions --system-prompt "Reply tersely." --tools "" --strict-mcp-config --setting-sources "" --effort low \
 | python3 -c 'import json,sys; d=json.load(sys.stdin); u=d["usage"]; print(u["input_tokens"]+u["cache_creation_input_tokens"]+u["cache_read_input_tokens"], repr(d["result"][:20]))'
