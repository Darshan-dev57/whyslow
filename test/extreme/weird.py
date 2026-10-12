"""Odd process names and zombies: 9 CPU burners renamed with prctl
("a) b (", unicode, a newline, 26 chars, ...) plus 300 unreaped zombies.

usage: python3 weird.py SECONDS
"""
import ctypes, os, sys, time
libc = ctypes.CDLL(None)
names = [b"a) b (", b"x y z", "na\u00efve\u2603".encode(), b"new\nline", b"abcdefghijklmnopqrstuvwxyz", b")", b"((", b" ", b"tab\there"]
pids = []
for n in names:
    pid = os.fork()
    if pid == 0:
        libc.prctl(15, ctypes.c_char_p(n), 0, 0, 0)  # PR_SET_NAME
        e = time.time() + float(sys.argv[1])
        while time.time() < e: pass
        os._exit(0)
    pids.append(pid)
# zombies: 300 children that exit at once and are not reaped until the end
z = []
for i in range(300):
    pid = os.fork()
    if pid == 0:
        os._exit(0)
    z.append(pid)
time.sleep(float(sys.argv[1]) + 2)
for p in pids + z:
    os.waitpid(p, 0)
