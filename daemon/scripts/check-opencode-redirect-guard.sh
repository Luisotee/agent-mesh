#!/usr/bin/env bash
# Live check of share/opencode/readonly-guard.js against the real OpenCode, with no model account: a local fake
# OpenAI-compatible server answers the first request with a bash tool call (`> victim.txt` by default) and the second
# with "ok", and logs what the tool returned. Run it by hand (never from a test or CI: it runs the real `opencode`, in a
# temp HOME, and needs the network once to fetch OpenCode's provider package). Without the guard the file is created;
# with it the call is refused and the model gets the refusal as the tool's result.
#
#   daemon/scripts/check-opencode-redirect-guard.sh ['> victim.txt']
set -euo pipefail
cmd="${1:-> victim.txt}"
here="$(cd "$(dirname "$0")/.." && pwd)"
guard="$here/share/opencode/readonly-guard.js"
work="$(mktemp -d /tmp/oc-guard.XXXXXX)"
srv=
trap '[ -z "$srv" ] || kill "$srv" 2>/dev/null || true; rm -rf "$work"' EXIT
mkdir -p "$work/home" "$work/w"
cat > "$work/fake.py" <<'PY'
import json, sys
from http.server import BaseHTTPRequestHandler, HTTPServer
CMD, LOG = sys.argv[2], sys.argv[3]
class H(BaseHTTPRequestHandler):
    def log_message(self, *a): pass
    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers.get('content-length', 0))) or b'{}')
        tool_msgs = [m for m in body.get('messages', []) if m.get('role') == 'tool']
        def chunk(delta, fin=None):
            return 'data: ' + json.dumps({"id": "c1", "object": "chat.completion.chunk", "created": 0, "model": "m",
                "choices": [{"index": 0, "delta": delta, "finish_reason": fin}]}) + '\n\n'
        if body.get('tools') and not tool_msgs:
            args = json.dumps({"command": CMD, "description": "t"})
            out = [chunk({"role": "assistant", "tool_calls": [{"index": 0, "id": "call_1", "type": "function", "function": {"name": "bash", "arguments": args}}]}), chunk({}, "tool_calls")]
        else:
            if tool_msgs: open(LOG, 'a').write(json.dumps(tool_msgs[-1].get('content')) + '\n')
            out = [chunk({"role": "assistant", "content": "ok"}), chunk({}, "stop")]
        self.send_response(200); self.send_header('content-type', 'text/event-stream'); self.end_headers()
        for o in out + ['data: [DONE]\n\n']: self.wfile.write(o.encode())
HTTPServer(('127.0.0.1', int(sys.argv[1])), H).serve_forever()
PY
port=$((20000 + RANDOM % 20000))
python3 "$work/fake.py" "$port" "$cmd" "$work/tool.log" & srv=$!
sleep 1
provider="\"provider\":{\"fake\":{\"npm\":\"@ai-sdk/openai-compatible\",\"name\":\"fake\",\"options\":{\"baseURL\":\"http://127.0.0.1:$port/v1\",\"apiKey\":\"x\"},\"models\":{\"m\":{\"name\":\"m\",\"tool_call\":true,\"limit\":{\"context\":100000,\"output\":4000}}}}}"
for mode in without with; do
  rm -f "$work/w/victim.txt" "$work/tool.log"
  plugin=""; [ "$mode" = with ] && plugin="\"plugin\":[\"file://$guard\"],"
  (cd "$work/w" && HOME="$work/home" XDG_CONFIG_HOME="$work/home/.config" XDG_DATA_HOME="$work/home/.local/share" \
    XDG_STATE_HOME="$work/home/.local/state" XDG_CACHE_HOME="$work/home/.cache" OPENCODE_DISABLE_AUTOUPDATE=1 \
    OPENCODE_DISABLE_PROJECT_CONFIG=1 \
    OPENCODE_PERMISSION='{"bash":{"*":"deny","ls *":"allow","pwd":"allow","*>*":"deny"}}' \
    OPENCODE_CONFIG_CONTENT="{$plugin$provider}" timeout 120 opencode run -m fake/m go >/dev/null 2>&1) || true
  echo "== $mode the guard: files now: $(cd "$work/w" && ls | tr '\n' ' ')"
  echo "   the tool's answer: $(cat "$work/tool.log" 2>/dev/null || echo none)"
done
