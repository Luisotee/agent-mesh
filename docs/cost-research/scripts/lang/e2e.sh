#!/bin/bash
# one run: e2e.sh <lang> <n>
E=${WORK:-$PWD}/e2e
id=$1$2; D=$E/runs/$id; rm -rf $D; mkdir -p $D; cp -r $E/base $D/repo
sid=$(python3 -c 'import uuid;print(uuid.uuid4())'); echo $sid > $D/session
sed "s#REPORT_PATH#$E/reports/$id.md#" $E/tasks/$1.md > $D/prompt.md
cd $D/repo
env -u MESH_RUN -u MESH_SELF -u MESH_PARENT -u MESH_ROLE -u CLAUDECODE -u CLAUDE_CODE_SESSION_ID -u CLAUDE_CODE_CHILD_SESSION \
    -u CLAUDE_CODE_MESSAGING_SOCKET -u CLAUDE_CODE_MESSAGING_TOKEN -u CLAUDE_PID -u CLAUDE_EFFORT -u CLAUDE_CODE_SESSION_ATTENDED \
    -u CLAUDE_CODE_ENTRYPOINT -u CLAUDE_CODE_EXECPATH \
  claude -p --model opus --effort medium --output-format json --dangerously-skip-permissions --session-id $sid < $D/prompt.md > $D/out.json 2> $D/err.txt
echo "$id exit $?"
