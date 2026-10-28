#!/bin/bash
X="$(dirname "$0")/e2e.sh"
for n in 1 2 3; do
  for l in en zh terse; do $X $l $n & done; wait
done
echo ALL DONE
