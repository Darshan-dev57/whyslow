"""Crash safety: start whyslow, SIGKILL it after a random 2-75 s, check
PRAGMA integrity_check, repeat N times, then restart once more.

usage: WHYSLOW=./whyslow N=15 python3 killtest.py   (results in $WORK/result.log)
"""
import os, random, signal, sqlite3, subprocess, time, json, urllib.request
random.seed(7)
D = os.environ.get("WORK", "/tmp/whyslow-killtest"); os.makedirs(D, exist_ok=True)
db = D + "/w.db"
log = open(D + "/result.log", "a")
def start():
    return subprocess.Popen([os.environ.get("WHYSLOW", "./whyslow"), "run", "--db", db, "--addr", "127.0.0.1:7784"],
                            stdout=open(D + "/run.log", "a"), stderr=subprocess.STDOUT)
def counts():
    c = sqlite3.connect(db, timeout=10)
    r = c.execute("PRAGMA integrity_check").fetchone()[0]
    n = {t: c.execute(f"select count(*) from {t}").fetchone()[0] for t in ("sys_samples", "app_samples", "mysteries")}
    mx = c.execute("select max(ts) from sys_samples").fetchone()[0]
    c.close()
    return r, n, mx
for i in range(int(os.environ.get("N", "15"))):
    p = start()
    life = random.uniform(2, 75)
    time.sleep(life)
    # optionally hammer a write-heavy moment: kill right after a second boundary + jitter
    os.kill(p.pid, signal.SIGKILL); p.wait()
    files = sorted(os.listdir(D))
    r, n, mx = counts()
    log.write(json.dumps({"i": i, "life_s": round(life, 1), "integrity": r, "rows": n, "max_ts": mx, "files": files}) + "\n"); log.flush()
# final restart: must come up and keep appending
p = start(); time.sleep(8)
try:
    st = json.load(urllib.request.urlopen("http://127.0.0.1:7784/api/status", timeout=3))
    ok = st.get("sampling")
except Exception as e:
    ok = repr(e)
time.sleep(4); r, n, mx = counts()
p.terminate(); p.wait()
log.write(json.dumps({"final_restart_sampling": ok, "integrity": r, "rows": n, "max_ts": mx}) + "\n")
log.write("DONE\n")
