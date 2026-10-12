# whyslow extreme test log

Overnight run, Oct 12-13 2026, in the order the tests ran. Paths under /tmp/wsx were scratch space on the test VM; the scripts are in test/extreme.

Box: 4 vCPU, 8 GB, Debian 12, kernel 6.1, no BPF JIT. Binary /tmp/wsx/whyslow (go build -trimpath).

## Tests
### T1 race + eBPF root test (17:46 UTC) PASS
- `CGO_ENABLED=1 go test -race -count=1 ./...` -> all 13 packages ok, no race reports.
- `CGO_ENABLED=1 go test -race -count=1 -tags integration ./test/integration/` -> ok (10.8 s).
- `go test -race -c -tags ebpf ./internal/netbpf && sudo ./netbpf.test -test.v` -> all PASS; curl rx 5,329,318 tx 1,117,378 comm "curl". Ran while the soak instance also had its eBPF programs attached (two loaders coexist).
### T9 dashboard/API (17:49-17:52 UTC) 1 BUG FIXED
- 33 malformed/edge URLs against the soak instance (curl loop). All bad params -> 400 with JSON error; unknown /api -> 404; POST -> 405; path traversal -> 404.
- BUG: `/api/report?date=9999-12-31` returned 200 with an EMPTY body (report end time is year 10000; json encode failed after the header was written). Fix c15b9a5: writeJSON marshals first and returns 500 on failure; report rejects years outside 1970..9998 with 400; window/since below 1s (e.g. 1ns) now 400. Regression tests: TestWriteJSONEncodeError, TestBadWindows, extra dates in TestReport.
- Note (not a bug): `window=%zz` is dropped by Go's query parser, so the default window is used (200).
- SSE: 50 concurrent `curl -N /api/live` for 60 s + 5 raw-socket clients that never read (4 KB rcvbuf). All 50 got 1 history + 61 frames (none missed); /api/status during load 200 in 0.4 ms. whyslow fds 20 -> 75 -> 20 after; RSS 23.5 -> 27.0 -> 26.3 MB.
### T7 non-root / no battery / no thermal / doctor (17:53-17:56 UTC) PASS + 1 small fix
- `whyslow doctor` (user): warns 129/177 processes hide /proc/[pid]/io, not root -> --ebpf falls back, BPF JIT missing; battery none, temperature none (VM). Correct.
- `sudo whyslow doctor`: all io readable, can load eBPF, eBPF load works in socket-owner mode with the fentry ENOMEM reason. Correct.
- Non-root `whyslow run --ebpf`: warns, starts in proc mode; /api/status net.mode=proc with the memlock reason; battery=false thermal=false; /api/apps works.
- New test TestBrokenSysfs (garbage battery values, battery with only type, capacity 250, energy above full, zone without temp, N/A / -5000 / 999999999 temps): passes, no change needed (commit 'Test battery and thermal readers against broken sysfs trees').
- Fix: doctor always checked the default DB path; added `doctor --db` (commit 'Let doctor check the database given with --db').
### T4 process churn + weird names + zombies (17:56-18:01 UTC) 1 CRITICAL BUG FIXED
- `/tmp/wsx/churn.sh 120` (4 loops spawning /bin/true for 120 s; PIDs advanced ~138k, about 1,100 spawns/s) together with `/tmp/wsx/weird.py 40` (9 CPU burners renamed via prctl: "a) b (", "x y z", unicode, "new\nline", 26-char name, ")", "((", " ", tab; plus 300 unreaped zombies). Soak instance: 116 ticks in the 120 s window, max gap 1 s (no tick lag), no warnings in the log. Churn charged to app "churn.sh" (86.6% CPU), weird burners to "python:weird.py" (children group with their parent, by design).
- PID wraparound: not feasible here (pid_max 4194304); PID reuse is covered by the unit test TestPIDReuseIsNotCharged.
- CRITICAL BUG: a process whose command line is a single whitespace-only argument (`sleep 15 | bash -c 'exec -a " " cat'`, any unprivileged user) crashed whyslow with index out of range in apps.Exe (strings.Fields(" ")[0]). It killed the ROOT soak instance too. Fix 992212d: fall back to comm; regression test TestBlankTitle (fails before, passes after) + FuzzGroup fuzz target.
- Hardening: pipeline.Run now recovers a panic in one sampling tick, logs it (stack once) and keeps going (commit 'Recover from a panic in one sampling tick instead of exiting', test TestRunSurvivesPanickingTick). Re-ran the blank-title process against the new root soak: survived.
### T6 crash safety, kill -9 (18:02-18:08 UTC) PASS
- `/tmp/wsx/killtest.py`: 15 cycles of start `whyslow run --db /tmp/wsx/kt/w.db`, random life 2-75 s (seeded), SIGKILL, then python sqlite3 `PRAGMA integrity_check` + row counts; then a final restart.
- integrity_check = ok after all 15 kills. Rows only ever grew (25, 34, 78, ... 321). Final restart: /api/status sampling=true, kept appending (331 rows), integrity ok.
- Data lost per kill = the unflushed batch: e.g. 49.5 s life -> 44 rows, a 4.7 s life -> 0 rows (first tick is aligned to +1.5 s and the sink flushes in batches). So a hard kill loses at most the last few seconds, never the database.
### T5 blame accuracy, part 1: harness + bug found (18:10-18:20 UTC)
- Harness /tmp/wsx/blamebench.py: per trial a known culprit runs 20 s next to a decoy (python decoy.py, ~30% of one core), then the newest mystery of that metric starting after the trial is read from /api/mysteries; correct = top culprit app == expected app. Every command is started under `timeout` (a launcher) so it is its own app, as from a terminal (first try without it blamed the harness script itself, which is by design: children of a script group with the script).
- BUG (blame): a shell loop `bash -c 'while :; do dd ... count=200 oflag=direct; done'` wrote at 313 MB/s; mystery said "66% from bash, 32% from ddwrite". Each dd lives < 1 s, so most are never sampled and their I/O and CPU only appear in the shell's counters when it reaps them. Fix 455bd9e: for a wrapper (shell running inline code or reading commands, xargs, timeout, env, nice, ...; NOT init/sshd/terminals, which adopt unrelated orphans) the unsampled-children CPU (cutime jump minus credits) and its I/O jump go to the app of the child it was last seen running within 3 samples. Tests: TestWrapperLoopChargesItsChild (fails before: bash 60% CPU / 150 MB; after: dd 65% / 155 MB, bash 0), TestIsWrapper.
### T2 fuzzing (17:48-18:33 UTC, round 2 to ~18:43) 1 BUG FIXED
- 9 Go native fuzz targets in internal/procfs/fuzz_test.go (ProcStat, Cmdline, Stat, MemInfo, DiskStats, NetDev, IO, Statm, Uptime), seeds = fixtures; invariants: no panic, comm is a substring of the input, cmdline round-trips, used <= total, parsed io has write_bytes. `go test -run '^$' -fuzz ^X$ -fuzztime 300s -parallel 2 ./internal/procfs` each (log /tmp/wsx/fuzz.log). /proc/[pid]/io parsing was split out as ParseIO to be fuzzable.
- PASS (no failure in 300 s each): ProcStat, Cmdline, Stat, MemInfo, DiskStats, NetDev, Statm, Uptime. Round 2 after the fix: FuzzParseIO 300 s PASS, FuzzGroup 300 s PASS.
- FuzzParseIO FOUND: "read_bytes:0\nread_bytes:0" parsed OK with write_bytes silently 0 (it counted lines, not distinct keys). Fix in 285102f (bitmask of keys seen); the minimised input is committed as testdata/fuzz/FuzzParseIO/23b721420345c31d (runs as a regression case in every go test).
- FuzzGroup (internal/apps): random cmdline/comm/parent shapes incl. self-parents and cycles; checks every pid gets a non-empty name. While writing it: a process with an empty comm and no cmdline got the app name "" -> now "(unnamed)" (commit 'Name processes with an empty comm and no command line', test TestNoEmptyName).
### T8b fast sampler (18:25-18:27 UTC) 1 BUG FIXED
- `whyslow run --interval 100ms` (non-root) for ~95 s: 9% of one core, RSS 23.9 MB, no lag; BUT the DB held 95 rows for 95 s: rows are keyed by the second (INSERT OR REPLACE), so 9 of 10 samples were overwritten; and detector windows are in samples, so the 2-min baseline became 12 s. Fix: `--interval` must be whole seconds (commit 'Accept only whole seconds for --interval', TestCheckInterval; DECISIONS #17).

### T5 blame accuracy, part 2: results (18:42-19:28 UTC, soak instance run 3, eBPF on, spikes.sh paused)
- `python3 blamebench.py 5` (cpu, mem, disk_write, disk_read, net_rx, net_tx x5) + `python3 blamebench.py 5 disk_write_loop,disk_read_loop`. Log /tmp/wsx/bench/blame.log.
- Every DETECTED mystery blamed the right app first: cpu 4/4 (shares 0.80-0.92), mem 5/5 (1.0), disk_write 5/5 (0.96-1.0), disk_read 5/5 (0.92-1.0), net_rx 2/2 (1.0), net_tx 5/5 (1.0), disk_write_loop 5/5 ddwrite (0.87-0.94), disk_read_loop 4/4 ddread (0.91-0.98).
- Not detected: cpu rep0 (ran seconds after fuzzing ended: prior-120 s CPU median 62%, so threshold = min(2x62, cap 90) = 90 and the trial peaked at 88.2% -> correct per design, listed as a limit); disk_read_loop 1 rep (not yet explained); net_rx reps 2-4 invalid: the remote test server started answering 429 (rate limit), so no load ran. net_rx is being rerun with other mirrors.

- net_rx rerun with other mirrors (19:29-19:34): 5/5 detected, 5/5 curl (share 1.0).
- TOTAL T5: 42 valid trials (3 invalidated by the remote 429); 40 detected (95.2%); top-blamed app correct in 40/40 detected (100%); end-to-end 40/42 = 95.2%.

### T5 part 3: rerun on the final binary (19:49-20:15 UTC, commit 1d8fa83 code, soak run 4)
- `python3 blamebench.py 3` + `python3 blamebench.py 3 disk_write_loop,disk_read_loop` (spikes paused): 24 trials, 23 detected, 23/23 blamed correctly. The miss is again the first cpu trial, started 14 s after a `make fuzz` CPU spike (mystery #4) closed at 19:49:09.5: the 30 s cooldown ran to 19:49:39.5, leaving too little of the 20 s load to meet the 3-sample sustain. (Its detector baseline was 1.5%, not the raw 72% median: samples inside open spikes do not enter the baseline.)
- COMBINED T5 (both runs): 66 valid trials, 63 detected (95.5%), top-blamed app correct in 63/63 detected (100%). Misses: 2 cpu trials that started right after another CPU spike (one at a 62% baseline under the 90% cap, one inside the 30 s cooldown; both by design), 1 disk_read_loop (not explained; its database was replaced at the soak restart).

### T3 combined stress (19:37-19:39 UTC) 1 BUG FIXED
- `/tmp/wsx/stress.sh 120`: stresscpu.py (4 burners, all cores), memhog 2 GB, diskwriter (fsync loop), internet curl loop at 20 MB/s, loopback curl flood against python http.server. Watched by the soak (root, eBPF, 7781) and a second non-root instance (7785). (spikes.sh soakburn overlapped the first 40 s.)
- No tick lag: 125 points in the 124 s window, max gap 1 s, on both instances. whyslow CPU over the stress: 1.37% (eBPF) and 1.35% (non-root) of one core; RSS 27.5 -> 27.7 MB and 19.6 -> 20.1 MB. No warnings/errors in either log.
- Mysteries on both instances: mem -> python:memhog.py (0.95), disk_write -> python:diskwriter.py (0.99 / 1.0), cpu (opened by soakburn 40 s before) -> stresscpu.py 0.37 tied with "systemd" 0.37.
- BUG: app "systemd" (init) got 30,095% CPU in ONE second at the end of the stress. timeout killed the multiprocessing parent, its 4 children were reparented to init, finished and were reaped by init, whose cutime jumped by their whole lives (300 s); the credit for what was already charged went to timeout instead. Fix 1d8fa83: credits that find no matching jump go to a pool that processes with an unexplained cutime jump (the reaper) draw from; I/O likewise but only for such a reaper. Test TestOrphansReapedByInitAreNotChargedTwice (fails before: init 20200% / 4 GB; passes after).

- T3 rerun on the fixed binary (19:45-19:47 UTC, same script, soak run 4 + non-root 7785): 125 points / 124 s, max gap 1 s on both; mysteries on both: cpu -> python:stresscpu.py 0.66 (curl 0.15, http.server 0.07), mem -> memhog 0.95, disk_write -> diskwriter 0.99. Highest app CPU in any second: stresscpu 277% (no init spike). whyslow CPU over the run: 1.66% (eBPF) / 1.31% (non-root) of one core; RSS 21.0 -> 21.5 MB / 20.2 -> 21.2 MB. (The internet download leg got 429 from the remote server; loopback traffic is not part of system network, by design.)

### T4b many apps (20:16-20:21 UTC)
- `/tmp/wsx/manyapps.sh 300`: 1,000 sleeping processes, each its own app name (`exec -a appN sleep`). Soak instance over 60 s: 6.1% of one core (about 1.2% normally), RSS 28.4 MB, 60 points / 60 s, max gap 1 s; /api/live first byte 1.2 ms. (/api/apps returns the top 15.)

### T8 soak, interim (21:18 UTC) 1 LEAK FIXED (found by reading the code behind a slowly rising RSS)
- Soak run 4 RSS: 26.5 MB at 32 min, 28.8 MB at 64 min, 29.3 MB at 94 min; threads 11, fds 20 flat; no warnings.
- LEAK: the collector's PID -> app map (kept to name the network counters of exited PIDs) was pruned only inside the eBPF path. Without eBPF (non-root, the default) it gained one entry per process ever seen and was never pruned. Fix (commit 'Stop the PID to app map growing without eBPF'): only filled when eBPF is on. Test TestAppMapDoesNotGrowWithoutNet (51 entries for 2 live processes before; <= 2 after). The soak itself runs with eBPF, where the map is pruned.

- Leak check with churn (21:19-21:22 UTC): old binary vs fixed binary, both non-root, side by side, `test/extreme/churn.sh 180` = 519,601 processes in 180 s (about 2,900/s). PID WRAPAROUND happened during this run (PIDs went past 4,194,303 back to low numbers; new pids like 22246). RSS old 19.1 -> 20.1 MB, fixed 19.0 -> 20.2 MB: no measurable difference, because only processes alive at a sample instant enter the map (a few per second, ~100 bytes each). The leak is real but slow (one entry per process ever sampled). Soak run 4 (root) went through the wraparound: alive, RSS 28.6 MB, no warnings.

### T8 soak, final (run 4: 19:43-22:45 UTC, 3.03 h, root, eBPF on, 1 s interval) PASS
- Sampled every 30 s (363 samples, /tmp/wsx/soak/samples.log). Load during it: the T3 rerun, the 24-trial blame rerun, 1,000 extra processes for 5 min, the 519,601-process churn with PID wraparound, and a 60 s CPU burn or 400 MB direct write every 10 min.
- RSS: median 27.8 MB (max 28.8) in the first hour vs median 28.2 MB (max 28.8) in the last hour: flat. Threads 10-11. Open fds 18-21 throughout (no fd leak; SSE clients came and went). CPU: 1.22% of one core over the 3 h (1.51% in the first hour with the heavy tests, 1.02% in the last). DB grew linearly ~2 MB/h (2.1 MB at 1 h, 4.0 MB at 2 h, 6.1 MB at 3 h; bounded by the 24 h / 6 h raw retention), WAL steady at 4.3 MB (checkpoints keep up). No warnings or errors in the log. Survived PID wraparound.
- Earlier soak runs: run 1 died at 14 min from the blank-title crash (T4); runs 2 and 3 were restarted on purpose to put fixes in.

### Final verification (22:48-22:52 UTC)
- gofmt -l: empty. go vet ./... and with -tags integration,ebpf: clean. staticcheck v0.8.2 -tags integration,ebpf: clean.
- go test -count=1 ./...: 111 top-level tests (+30 subtests, incl. fuzz seed corpora) pass, 0 fail, 0 skip (was 90 before tonight).
- go test -race ./... and -race -tags integration: pass. Root eBPF test: all PASS (curl rx 5,329,006 tx 1,104,534).
