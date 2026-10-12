#!/bin/bash
# combined stress for $1 seconds: cpu x4, mem 2 GB, disk writer, internet download, loopback flood
T=${1:-120}; S=${WORK:-/tmp/whyslow-stress}; mkdir -p $S
cat > $S/memhog.py <<'EOF'
import sys, time
b = bytearray(int(sys.argv[1]) << 20)
for i in range(0, len(b), 4096): b[i] = 1
time.sleep(float(sys.argv[2]))
EOF
cat > $S/diskwriter.py <<'EOF'
import os, sys, time
fd = os.open(sys.argv[1], os.O_WRONLY | os.O_CREAT); buf = b"x" * (1 << 20); e = time.time() + float(sys.argv[2])
while time.time() < e:
    os.lseek(fd, 0, 0)
    for i in range(200): os.write(fd, buf)
    os.fsync(fd)
EOF
cat > $S/stresscpu.py <<'EOF'
import multiprocessing as mp, sys, time
def burn(t):
    e = time.time() + t
    while time.time() < e: pass
if __name__ == '__main__':
    t = float(sys.argv[1]); ps = [mp.Process(target=burn, args=(t,)) for _ in range(4)]
    [p.start() for p in ps]; [p.join() for p in ps]
EOF
head -c 100000000 /dev/urandom > $S/blob
(cd $S && exec timeout $((T+5)) python3 -m http.server 7790 --bind 127.0.0.1 >/dev/null 2>&1) &
sleep 1
echo "start $(date +%s)" > $S/times
timeout $T python3 $S/stresscpu.py $T &
timeout $T python3 $S/memhog.py 2048 $T &
timeout $T python3 $S/diskwriter.py $S/fill $T &
timeout $T bash -c 'while :; do curl -s --limit-rate 20M -o /dev/null "https://proof.ovh.net/files/100Mb.dat"; done' &
timeout $T bash -c 'while :; do curl -s -o /dev/null http://127.0.0.1:7790/blob; done' &
wait
echo "end $(date +%s)" >> $S/times
rm -f $S/blob $S/fill $S/*.py
