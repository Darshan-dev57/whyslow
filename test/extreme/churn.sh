#!/bin/bash
# Process churn: four loops spawning /bin/true for $1 seconds (default 120).
# Prints how many processes were started.
end=$((SECONDS+${1:-120})); tmp=$(mktemp -d)
for j in 1 2 3 4; do ( c=0; while [ $SECONDS -lt $end ]; do /bin/true; c=$((c+1)); done; echo $c > $tmp/$j ) & done
wait; cat $tmp/* | awk '{s+=$1} END {print s}'; rm -rf $tmp
# Blank title: a process whose whole command line is one space (crashed whyslow before the fix).
sleep 5 | bash -c 'exec -a " " cat'
