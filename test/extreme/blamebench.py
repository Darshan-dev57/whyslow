"""Blame accuracy: run known culprits next to a decoy and check that the
newest mystery of that metric blames the culprit first.

usage: API=http://127.0.0.1:7777 python3 blamebench.py [reps] [scenario,...]
Scenarios: cpu mem disk_write disk_read net_rx net_tx disk_write_loop
disk_read_loop. Results: one JSON line per trial in $WORK/blame.log.
Run whyslow (with --ebpf as root for the net scenarios) and let it warm up
for two minutes first. Needs internet for net_rx/net_tx.
"""
import calendar, json, os, subprocess, sys, time, urllib.request, shutil
B = os.environ.get("WORK", "/tmp/whyslow-extreme"); os.makedirs(B, exist_ok=True)
API = os.environ.get("API", "http://127.0.0.1:7777")
OUT = open(B + "/blame.log", "a")
open(B + "/cpuhog.py", "w").write("""import multiprocessing as mp, sys, time
def burn(t):
    e = time.time() + t
    while time.time() < e: pass
if __name__ == '__main__':
    t = float(sys.argv[1]); ps = [mp.Process(target=burn, args=(t,)) for _ in range(3)]
    [p.start() for p in ps]; [p.join() for p in ps]
""")
open(B + "/memhog.py", "w").write("""import sys, time
b = bytearray(int(sys.argv[1]) << 20)
for i in range(0, len(b), 4096): b[i] = 1
time.sleep(float(sys.argv[2]))
""")
open(B + "/decoy.py", "w").write("""import time, sys
e = time.time() + float(sys.argv[1])
while time.time() < e:
    s = time.time()
    while time.time() - s < 0.03: pass
    time.sleep(0.07)
""")
open(B + "/diskwriter.py", "w").write("""import os, sys, time
fd = os.open(sys.argv[1], os.O_WRONLY | os.O_CREAT); buf = b"x" * (1 << 20); e = time.time() + float(sys.argv[2])
while time.time() < e:
    os.lseek(fd, 0, 0)
    for i in range(200): os.write(fd, buf)
    os.fsync(fd)
""")
open(B + "/diskreader.py", "w").write("""import os, sys, time
fd = os.open(sys.argv[1], os.O_RDONLY); e = time.time() + float(sys.argv[2])
while time.time() < e:
    os.posix_fadvise(fd, 0, 0, os.POSIX_FADV_DONTNEED); os.lseek(fd, 0, 0)
    while os.read(fd, 1 << 20): pass
""")
for n in ("ddwrite", "ddread"):
    shutil.copy("/usr/bin/dd", B + "/" + n)
URL = "https://speed.cloudflare.com/__down?bytes=50000000"
RXURLS = ["https://ash-speed.hetzner.com/100MB.bin", "https://proof.ovh.net/files/100Mb.dat"] * 2
def run_cpu(t):
    return [subprocess.Popen(["timeout", "60", "python3", B + "/cpuhog.py", str(t)])], "python:cpuhog.py"
def run_mem(t):
    return [subprocess.Popen(["timeout", "60", "python3", B + "/memhog.py", "2048", str(t)])], "python:memhog.py"
def run_dw(t):
    return [subprocess.Popen(["timeout", str(t), "bash", "-c", "while :; do %s/ddwrite if=/dev/zero of=%s/fill bs=1M count=200 oflag=direct status=none; done" % (B, B)])], "ddwrite"
def run_dw1(t):
    return [subprocess.Popen(["timeout", "60", "python3", B + "/diskwriter.py", B + "/fill", str(t)])], "python:diskwriter.py"
def run_dr1(t):
    return [subprocess.Popen(["timeout", "60", "python3", B + "/diskreader.py", B + "/rfile", str(t)])], "python:diskreader.py"
def run_dr(t):
    return [subprocess.Popen(["timeout", str(t), "bash", "-c", "while :; do %s/ddread if=%s/rfile of=/dev/null bs=1M iflag=direct status=none; done" % (B, B)])], "ddread"
def run_rx(t):
    args = ["timeout", "60", "curl", "-s", "--limit-rate", "20M", "--max-time", str(t)]
    for u in RXURLS: args += ["-o", "/dev/null", u]
    return [subprocess.Popen(args)], "curl"
def run_tx(t):
    return [subprocess.Popen(["timeout", "60", "curl", "-s", "-o", "/dev/null", "--limit-rate", "10M", "--max-time", str(t), "-X", "POST", "--data-binary", "@" + B + "/rfile", "https://speed.cloudflare.com/__up"])], "curl"
SC = {"cpu": run_cpu, "mem": run_mem, "disk_write": run_dw1, "disk_read": run_dr1, "net_rx": run_rx, "net_tx": run_tx,
      "disk_write_loop": run_dw, "disk_read_loop": run_dr}
METRIC = {"disk_write_loop": "disk_write", "disk_read_loop": "disk_read"}
if not os.path.exists(B + "/rfile"):
    subprocess.run(["dd", "if=/dev/urandom", "of=" + B + "/rfile", "bs=1M", "count=200", "status=none"])
def mysteries():
    return json.load(urllib.request.urlopen(API + "/api/mysteries?since=10m&limit=100", timeout=5))
reps = int(sys.argv[1]) if len(sys.argv) > 1 else 5
order = sys.argv[2].split(",") if len(sys.argv) > 2 else ["cpu", "mem", "disk_write", "disk_read", "net_rx", "net_tx"]
for r in range(reps):
    for scen in order:
        metric = METRIC.get(scen, scen)
        t0 = time.time()
        decoy = subprocess.Popen(["timeout", "60", "python3", B + "/decoy.py", "22"])
        procs, want = SC[scen](20)
        for p in procs: p.wait()
        decoy.wait()
        # wait for the spike to close (release + a margin)
        found = None
        for _ in range(30):
            time.sleep(2)
            ms = [m for m in mysteries() if m["metric"] == metric and calendar.timegm(time.strptime(m["start"][:19], "%Y-%m-%dT%H:%M:%S")) >= t0 - 3]
            if ms:
                found = ms[-1] if len(ms) == 1 else sorted(ms, key=lambda m: m["id"])[0]
                break
        top = found["culprits"][0]["app"] if found and found["culprits"] else None
        share = found["culprits"][0]["share"] if found and found["culprits"] else None
        OUT.write(json.dumps({"t0": round(t0), "rep": r, "scenario": scen, "metric": metric, "want": want, "detected": bool(found), "top": top,
                              "share": share, "correct": top == want, "summary": found["summary"][:160] if found else None}) + "\n")
        OUT.flush()
        try: os.remove(B + "/fill")
        except FileNotFoundError: pass
        time.sleep(35)  # cooldown 30 s + baseline settle
OUT.write("DONE\n")
