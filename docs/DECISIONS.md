# Design decisions and deviations

The plan (written before any code) is the starting point. This file records
every place the build went a different way, and why. Each entry is something
that was tried or measured, not a guess.

## eBPF

### 1. Socket-owner fallback next to fentry/fexit

**Plan:** count network bytes with fentry/fexit programs on `tcp_sendmsg`,
`tcp_cleanup_rbuf` and the UDP send/receive functions. Without them,
whyslow would fall back to system-level network only.

**What happened:** on the build machine (Debian 12 VM, kernel 6.1.158), loading
even an empty fentry program fails with `ENOMEM` before the verifier runs.
The kernel has BTF and the functions are traceable, but it is built without
`CONFIG_BPF_JIT`. `/proc/sys/net/core/bpf_jit_enable` does not exist, and
trampolines need the JIT. Kprobes are not an option on this kernel either,
because `CONFIG_KPROBES` is off.

**Decision:** keep fentry/fexit as the first choice, since it is the accurate
method on normal distro kernels, and add a second method that works without
the JIT. A `cgroup/sock_create` program records the creating process in
socket-local storage (`BPF_MAP_TYPE_SK_STORAGE`, cloned to accepted
sockets). `cgroup_skb` ingress and egress programs on the root cgroup add
each packet's length to that owner. `netbpf.Load` tries fentry first and
falls back on its own. `doctor` and `/api/status` say which method is in use
and why.

**Cost:** the fallback counts IP packet lengths, so headers and retransmits
are included. Sockets opened before whyslow started are not counted, and a
socket handed to another process stays with its creator. These limits are
in the README.

**Verified:** the fallback ran live on the build machine (see
`docs/evidence/ebpf.md`). The fentry path has unit tests for its bytecode
and its BTF slot lookup. It has not been loaded on a real kernel; the CI
eBPF job on GitHub's runners (which have the JIT) is where it will first run.

### 2. whyslow's own listener uses plain TCP, not MPTCP

Since Go 1.24, listeners default to Multipath TCP. The kernel then creates
the subflow sockets itself, so `sock_create` never runs for them, and
socket-owner mode could not attribute their traffic. The eBPF test caught
this: the test server's bytes were missing. whyslow's web listener and the
test server call `ListenConfig.SetMultipathTCP(false)`.

### 3. The byte-count slot comes from BTF, and its type is checked

The plan read the argument count from BTF to find fexit's return slot. The
loader also checks that the byte count really is a 32-bit signed int. A
kernel that changed the type would otherwise read garbage without warning.
The programs compare with `JSLE32` and zero-extend with `mov32`, because
only the low 32 bits of an `int` slot are defined.

## Collector

### 4. Work of short-lived children is counted on their parent

A process that starts and exits between two samples never shows up in
`/proc`. Its CPU and I/O reach the parent only through `cutime`/`cstime`
and `/proc/[pid]/io` once the parent reaps it. The collector reads these,
and credits back the last-seen totals of children it did see, so nothing is
counted twice.

### 5. The credit follows whole chains that exit together

The integration test found a bug in decision 4: with `test -> timeout -> dd`,
if `dd` and `timeout` exit in the same second, the test process's `cutime`
jumps by both. The first version credited back only `timeout`'s own total,
so `dd`'s full CPU time landed on the test process. The burner was blamed
for 49% of the spike and the test runner for 51%. Now each dead process
passes on its own last-seen totals plus those of its dead children, and the
same test blames `dd` for 96% of the spike.

## Detection and reports

### 6. Repeat offenders look back seven days, and come in two kinds

A single day rarely has enough spikes to show a pattern. The report draws
repeat offenders from the last seven days, in two kinds:
- **culprit:** the main cause (share of 50% or more) of two or more spikes
  of the same metric;
- **opener:** the app had just started before two or more spikes. This is
  the video's "my machine chokes every time this app opens".

### 6b. Per-metric thresholds instead of one formula

The plan used `max(median + 6·1.4826·MAD, 3·median, floor)` for every metric.
Two metrics did not fit it. CPU on an idle laptop sits near 3%, so 3x median
is 9% and the floor alone decided everything; CPU now uses 2x median, a
minimum jump of 20 points and a cap of 90% (a box that idles at 60% could
otherwise never reach a 3x threshold). Used memory never triples, so memory
uses only an absolute jump: at least 512 MB or 10% of RAM, whichever is
larger. Disk and network keep the plan's 3x with 5 MB/s and 1 MB/s floors.

### 6c. "Jumped to X (normally Y)" when the baseline is near zero

"Disk read spiked 9000x" is true when the baseline is 1 KB/s and means
nothing. Below a small baseline the sentence gives the level instead:
"Disk read jumped to 8.8 MB/s at 17:23:28 for 3 s (normally 0 B/s)". CPU and
memory always use the level form ("CPU jumped to 32% (normally 3%)"); disk
and network keep the plan's ratio form when the baseline is large enough to
make the multiple meaningful.

### 7. Reports say how long whyslow was watching

"chrome used 2 h of CPU" means little if whyslow ran for ten minutes. The
report counts covered time from minute rollups plus raw sample intervals, and
shows it.

## Web

### 8. A small history buffer in the live hub

The Live page needs the last few minutes to draw anything. The store writes
in 5-second batches, so asking the database would show a gap. The hub keeps
the last 300 system points in memory and sends them as the first SSE event.

### 9. Slow clients lose frames, then get disconnected

Each browser has an 8-frame buffer. A full buffer drops frames for that
client only. After 30 drops in a row the client is disconnected, and
EventSource reconnects to a fresh stream. A lost `mystery` event also
disconnects the client, so it reloads the feed instead of silently missing
one. The pipeline goroutine never blocks on a browser.

### 10. Host header check and CSP

The dashboard shows every process on the machine and listens on loopback.
A web page on another site could still reach it through DNS rebinding, so the
server answers only to `localhost` and loopback addresses unless it was
started on a non-loopback address. Responses also carry a strict
Content-Security-Policy, and the page has no inline scripts.

### 11. CPU lines on the mystery chart share one axis

App CPU is per core (200% = two cores) while system CPU is a share of all
cores. On the detail chart, app lines are divided by the core count so
they can be compared with the system line.

## Tests

### 12. The integration test scales the detector to a 200 ms interval

The production windows are counted in samples of one second (120-sample
baseline, 60-sample warmup). The burner test samples every 200 ms with
proportionally shorter windows, so it takes about ten seconds. The burners
are `dd` processes started under `timeout`, a launcher, so they form their
own app the way a user's command would.

### 13. The store-sink test no longer depends on writer speed

Under `-race` the database writer is slower, the 120-frame queue filled, and
frames were dropped as designed. The test then failed on timing. The test
now gives the sink a queue big enough for its 200 frames and asserts that
nothing was dropped.
