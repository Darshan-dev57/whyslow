// whyslow dashboard: plain JavaScript, no build step.
"use strict";

const $ = (sel, el = document) => el.querySelector(sel);
const view = $("#view");

// ---------- formatting (matches the Go side: decimal units) ----------
function fmtBytes(v) {
  const units = ["B", "KB", "MB", "GB", "TB"];
  let i = 0;
  while (v >= 1000 && i < units.length - 1) { v /= 1000; i++; }
  if (i === 0) return Math.round(v) + " B";
  return (v >= 100 ? Math.round(v) : v.toFixed(1)) + " " + units[i];
}
const fmtRate = (v) => fmtBytes(v) + "/s";
const fmtPct = (v) => (v >= 10 ? Math.round(v) : v.toFixed(1)) + "%";
function fmtDur(s) {
  s = Math.round(s);
  if (s < 60) return s + " s";
  if (s < 3600) return Math.floor(s / 60) + " min" + (s % 60 ? " " + (s % 60) + " s" : "");
  return Math.floor(s / 3600) + " h " + Math.floor((s % 3600) / 60) + " min";
}
function fmtAgo(date) {
  const s = (Date.now() - date.getTime()) / 1000;
  if (s < 60) return "just now";
  if (s < 3600) return Math.floor(s / 60) + " min ago";
  if (s < 86400) return Math.floor(s / 3600) + " h ago";
  return date.toLocaleDateString([], { month: "short", day: "numeric" });
}
const fmtClock = (d) => d.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", second: "2-digit" });
const fmtDay = (d) => d.toLocaleDateString([], { month: "short", day: "numeric" });

const METRICS = {
  cpu: { label: "CPU", fmt: fmtPct },
  mem: { label: "Memory", fmt: fmtBytes },
  disk_read: { label: "Disk read", fmt: fmtRate },
  disk_write: { label: "Disk write", fmt: fmtRate },
  net_rx: { label: "Download", fmt: fmtRate },
  net_tx: { label: "Upload", fmt: fmtRate },
};
const COLORS = { cpu: "#f85149", mem: "#bc8cff", disk_read: "#39c5cf", disk_write: "#d29922", net_rx: "#3fb950", net_tx: "#58a6ff" };
const APP_COLORS = ["#ffa657", "#79c0ff", "#d2a8ff", "#7ee787"];

function esc(s) {
  return String(s).replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
}
const badge = (m) => `<span class="badge m-${esc(m)}">${esc(METRICS[m] ? METRICS[m].label : m)}</span>`;

async function getJSON(url) {
  const r = await fetch(url, { headers: { Accept: "application/json" } });
  if (!r.ok) {
    let msg = r.status + " " + r.statusText;
    try { msg = (await r.json()).error || msg; } catch (_) { /* not JSON */ }
    throw new Error(msg);
  }
  return r.json();
}

function toast(html) {
  const t = $("#toast");
  t.innerHTML = html;
  t.hidden = false;
  clearTimeout(toast.timer);
  toast.timer = setTimeout(() => { t.hidden = true; }, 8000);
}

// ---------- status ----------
async function loadStatus() {
  try {
    const st = await getJSON("api/status");
    const pill = $("#netmode");
    if (st.net.mode === "ebpf") {
      pill.textContent = "per-app network: eBPF";
      pill.className = "pill on";
      pill.title = "Network bytes are counted per process in the kernel";
    } else {
      pill.textContent = "per-app network: off";
      pill.className = "pill off";
      pill.title = st.net.reason || "Run with --ebpf as root for per-app network numbers";
    }
    return st;
  } catch (e) {
    return null;
  }
}

// ---------- live stream (one EventSource for the whole page) ----------
const live = { es: null, hist: null, last: null, listeners: new Set(), mysteries: [] };

function connect() {
  if (live.es) return;
  const es = new EventSource("api/live");
  live.es = es;
  const conn = $("#conn");
  es.onopen = () => { conn.textContent = "live"; conn.className = "pill on"; };
  es.onerror = () => { conn.textContent = "reconnecting"; conn.className = "pill bad"; };
  es.addEventListener("history", (e) => {
    live.hist = JSON.parse(e.data);
    live.listeners.forEach((fn) => fn("history"));
  });
  es.addEventListener("frame", (e) => {
    const f = JSON.parse(e.data);
    live.last = f;
    const h = live.hist;
    if (h && (h.ts.length === 0 || f.ts > h.ts[h.ts.length - 1])) {
      h.ts.push(f.ts); h.cpu.push(f.sys.cpu); h.mem.push(f.sys.mem_used);
      h.disk_read.push(f.sys.disk_read); h.disk_write.push(f.sys.disk_write);
      h.net_rx.push(f.sys.net_rx); h.net_tx.push(f.sys.net_tx);
      if (h.ts.length > 300) for (const k in h) h[k].shift();
    }
    live.listeners.forEach((fn) => fn("frame"));
  });
  es.addEventListener("mystery", (e) => {
    const m = JSON.parse(e.data);
    live.mysteries.unshift(m);
    toast(`<b>New mystery</b><a href="#/mystery/${m.id}">${esc(m.summary)}</a>`);
    live.listeners.forEach((fn) => fn("mystery"));
  });
}

// ---------- charts ----------
function axisStyle(fmt) {
  return {
    stroke: "#8b949e",
    grid: { stroke: "rgba(48,54,61,.6)", width: 1 },
    ticks: { stroke: "rgba(48,54,61,.6)", width: 1 },
    values: (u, vals) => vals.map((v) => (v == null ? "" : fmt(v))),
    // fit the widest label, so "100.0 MB/s" is never clipped
    size: (u, values) => {
      if (!values) return 50;
      u.ctx.font = "12px sans-serif";
      return Math.ceil(Math.max(...values.map((v) => u.ctx.measureText(v).width))) + 20;
    },
  };
}
const timeAxis = () => ({
  stroke: "#8b949e",
  grid: { stroke: "rgba(48,54,61,.6)", width: 1 },
  ticks: { stroke: "rgba(48,54,61,.6)", width: 1 },
  values: (u, vals) => vals.map((v) => new Date(v * 1000).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", second: "2-digit", hour12: false })),
  size: 32,
});

// bands: () => [{from, to, color}] in unix seconds; drawn under the series.
function bandsPlugin(bands) {
  return {
    hooks: {
      drawClear: (u) => {
        const ctx = u.ctx;
        const { left, top, width, height } = u.bbox;
        ctx.save();
        for (const b of bands()) {
          let x0 = u.valToPos(b.from, "x", true);
          let x1 = u.valToPos(b.to, "x", true);
          x0 = Math.max(x0, left); x1 = Math.min(x1, left + width);
          if (x1 <= x0) continue;
          ctx.fillStyle = b.color;
          ctx.fillRect(x0, top, x1 - x0, height);
        }
        ctx.restore();
      },
    },
  };
}

// hlines: () => [{value, color}] dashed horizontal lines (thresholds).
function hlinesPlugin(lines) {
  return {
    hooks: {
      draw: (u) => {
        const ctx = u.ctx;
        const { left, width } = u.bbox;
        ctx.save();
        ctx.setLineDash([6, 4]);
        ctx.lineWidth = 1.5 * devicePixelRatio;
        for (const l of lines()) {
          const y = u.valToPos(l.value, "y", true);
          if (!isFinite(y)) continue;
          ctx.strokeStyle = l.color;
          ctx.beginPath(); ctx.moveTo(left, y); ctx.lineTo(left + width, y); ctx.stroke();
        }
        ctx.restore();
      },
    },
  };
}

function makeChart(el, series, fmt, opts = {}) {
  const width = Math.max(el.clientWidth, 200);
  const u = new uPlot({
    width, height: opts.height || 190,
    cursor: { drag: { x: false, y: false } },
    legend: { show: series.length > 1 || !!opts.legend, live: !!opts.legend },
    scales: { x: { time: true }, y: { range: opts.range || ((u, min, max) => [0, Math.max(max * 1.15, opts.minMax || 1)]) } },
    axes: [timeAxis(), axisStyle(fmt)],
    series: [{}, ...series.map((s) => ({
      label: s.label, stroke: s.color, width: s.width || 2,
      fill: s.fill === false ? undefined : s.color + "22",
      dash: s.dash, points: { show: false },
      value: (u, v) => (v == null ? "-" : fmt(v)),
    }))],
    plugins: opts.plugins || [],
  }, opts.data || [[], ...series.map(() => [])], el);
  const ro = new ResizeObserver(() => {
    const w = el.clientWidth;
    if (w > 0 && Math.abs(w - u.width) > 2) u.setSize({ width: w, height: u.height });
  });
  ro.observe(el);
  u._ro = ro;
  return u;
}

let cleanups = [];
function cleanup() {
  cleanups.forEach((fn) => fn());
  cleanups = [];
}

// ---------- Live page ----------
const LIVE_CHARTS = [
  { id: "cpu", title: "CPU", fmt: fmtPct, series: [{ key: "cpu", label: "CPU", color: COLORS.cpu }], metrics: ["cpu"], range: () => [0, 100] },
  { id: "mem", title: "Memory", fmt: fmtBytes, series: [{ key: "mem", label: "Used", color: COLORS.mem }], metrics: ["mem"] },
  { id: "disk", title: "Disk", fmt: fmtRate, series: [
    { key: "disk_read", label: "Read", color: COLORS.disk_read }, { key: "disk_write", label: "Write", color: COLORS.disk_write }],
    metrics: ["disk_read", "disk_write"], minMax: 1e6 },
  { id: "net", title: "Network", fmt: fmtRate, series: [
    { key: "net_rx", label: "Download", color: COLORS.net_rx }, { key: "net_tx", label: "Upload", color: COLORS.net_tx }],
    metrics: ["net_rx", "net_tx"], minMax: 1e5 },
];

const appCols = [
  { key: "name", label: "App", n: false },
  { key: "cpu", label: "CPU", fmt: (a) => fmtPct(a.cpu), title: "Percent of one core; 200% is two cores" },
  { key: "rss", label: "Memory", fmt: (a) => fmtBytes(a.rss) },
  { key: "disk_read", label: "Disk read", fmt: (a) => (a.io_hidden && a.io_hidden === a.procs ? '<span class="dim" title="Run as root to see other users\' disk I/O">?</span>' : fmtRate(a.disk_read)) },
  { key: "disk_write", label: "Disk write", fmt: (a) => (a.io_hidden && a.io_hidden === a.procs ? '<span class="dim">?</span>' : fmtRate(a.disk_write)) },
  { key: "net_rx", label: "Download", fmt: (a) => (a.net_active ? fmtRate(a.net_rx) : '<span class="dim" title="Per-app network needs --ebpf">-</span>'), cls: "hide-sm" },
  { key: "net_tx", label: "Upload", fmt: (a) => (a.net_active ? fmtRate(a.net_tx) : '<span class="dim">-</span>'), cls: "hide-sm" },
  { key: "procs", label: "Procs", fmt: (a) => a.procs, cls: "hide-sm" },
  { key: "started", label: "Running for", fmt: (a) => fmtDur((Date.now() - new Date(a.started)) / 1000), sortVal: (a) => -new Date(a.started), cls: "hide-sm" },
];
const sortState = { key: "cpu", asc: false };

function renderLive() {
  view.innerHTML = `
    <div class="grid tiles" id="tiles"></div>
    <div class="grid charts">
      ${LIVE_CHARTS.map((c) => `<section class="panel chart"><h2>${c.title}<span class="now" id="now-${c.id}"></span></h2><div class="plot" id="plot-${c.id}"></div></section>`).join("")}
    </div>
    <section class="panel" style="margin-bottom:16px"><h2>Latest mysteries</h2><div class="mini" id="recent"></div></section>
    <section class="panel"><h2>Apps right now</h2><div class="table-wrap"><table id="apps"></table></div></section>`;

  // closed spikes in view, shaded lightly; open ones in red
  const bandsFor = (metrics) => () => {
    const out = [];
    for (const m of live.mysteries) {
      if (metrics.includes(m.metric)) out.push({ from: new Date(m.start) / 1000, to: new Date(m.end) / 1000, color: "rgba(248,81,73,.10)" });
    }
    if (live.last) {
      for (const o of live.last.open) {
        if (metrics.includes(o.metric)) out.push({ from: new Date(o.onset) / 1000, to: live.last.ts + 1, color: "rgba(248,81,73,.25)" });
      }
    }
    return out;
  };
  const charts = LIVE_CHARTS.map((c) => {
    const u = makeChart($("#plot-" + c.id), c.series, c.fmt, { range: c.range, minMax: c.minMax, plugins: [bandsPlugin(bandsFor(c.metrics))] });
    return { c, u };
  });
  cleanups.push(() => charts.forEach(({ u }) => { u._ro.disconnect(); u.destroy(); }));

  const draw = () => {
    const h = live.hist;
    if (!h) return;
    for (const { c, u } of charts) {
      u.setData([h.ts.slice(), ...c.series.map((s) => h[s.key].slice())]);
      const last = c.series.map((s) => c.fmt(h[s.key][h[s.key].length - 1] || 0));
      $("#now-" + c.id).textContent = h.ts.length ? last.join(" / ") : "";
    }
  };
  const drawTiles = () => {
    const f = live.last;
    if (!f) { $("#tiles").innerHTML = '<div class="panel empty">Waiting for the first sample...</div>'; return; }
    const s = f.sys;
    const open = new Set(f.open.map((o) => o.metric));
    const tile = (label, value, extra, metrics) =>
      `<div class="panel tile${metrics.some((m) => open.has(m)) ? " alert" : ""}"><div class="label">${label}</div><div class="value">${value}</div><div class="extra">${extra}</div></div>`;
    let html = tile("CPU", fmtPct(s.cpu), `${s.ncpu} cores`, ["cpu"]) +
      tile("Memory", fmtBytes(s.mem_used), `of ${fmtBytes(s.mem_total)}`, ["mem"]) +
      tile("Disk", fmtRate(s.disk_read + s.disk_write), `read ${fmtRate(s.disk_read)} · write ${fmtRate(s.disk_write)}`, ["disk_read", "disk_write"]) +
      tile("Network", fmtRate(s.net_rx + s.net_tx), `down ${fmtRate(s.net_rx)} · up ${fmtRate(s.net_tx)}`, ["net_rx", "net_tx"]);
    if (s.battery) {
      const b = s.battery;
      html += tile("Battery", fmtPct(b.pct), esc(b.status) + (b.has_power ? ` · ${b.watts.toFixed(1)} W` : ""), []);
    }
    if (s.temp_c != null) html += tile("Temperature", Math.round(s.temp_c) + " °C", "hottest sensor", []);
    $("#tiles").innerHTML = html;
  };
  const drawApps = () => {
    const f = live.last;
    const t = $("#apps");
    if (!f) return;
    const col = appCols.find((c) => c.key === sortState.key) || appCols[1];
    const val = col.sortVal || ((a) => a[col.key]);
    const apps = f.apps.slice().sort((a, b) => {
      const x = val(a), y = val(b);
      const r = typeof x === "string" ? x.localeCompare(y) : x - y;
      return sortState.asc ? r : -r;
    }).slice(0, 15);
    t.innerHTML = `<thead><tr>${appCols.map((c) => `<th class="sortable ${c.key === "name" ? "" : "n"} ${c.cls || ""} ${c.key === sortState.key ? "sorted" + (sortState.asc ? " asc" : "") : ""}" data-key="${c.key}" ${c.title ? `title="${esc(c.title)}"` : ""}>${c.label}</th>`).join("")}</tr></thead>
      <tbody>${apps.map((a) => `<tr>${appCols.map((c) => c.key === "name"
        ? `<td>${esc(a.name)}</td>` : `<td class="n ${c.cls || ""}">${c.fmt(a)}</td>`).join("")}</tr>`).join("")}</tbody>`;
  };
  $("#apps").addEventListener("click", (e) => {
    const th = e.target.closest("th[data-key]");
    if (!th) return;
    const k = th.dataset.key;
    if (sortState.key === k) sortState.asc = !sortState.asc;
    else { sortState.key = k; sortState.asc = k === "name"; }
    drawApps();
  });
  const drawRecent = () => {
    const el = $("#recent");
    const ms = live.mysteries.slice(0, 4);
    el.innerHTML = ms.length ? ms.map((m) => `<a href="#/mystery/${m.id}"><time>${badge(m.metric)} ${fmtClock(new Date(m.start))}</time>${esc(m.summary)}</a>`).join("")
      : '<p class="empty">No spikes yet. Mysteries appear here the moment one is explained.</p>';
  };

  const onEvent = (kind) => {
    if (kind === "history" || kind === "frame") { draw(); drawTiles(); if (kind === "frame") drawApps(); }
    if (kind === "mystery") drawRecent();
  };
  live.listeners.add(onEvent);
  cleanups.push(() => live.listeners.delete(onEvent));
  draw(); drawTiles(); drawApps(); drawRecent();
}

// ---------- Mysteries page ----------
const feedState = { metric: "", since: "24h" };

async function renderMysteries() {
  view.innerHTML = `
    <h1>Mysteries</h1>
    <p class="sub">Every spike whyslow saw, with the apps that added the load and the apps that started just before.</p>
    <div class="filters" id="filters">
      <button class="chip" data-metric="">All</button>
      ${Object.keys(METRICS).map((m) => `<button class="chip" data-metric="${m}">${METRICS[m].label}</button>`).join("")}
      <select id="since" aria-label="Time range">
        <option value="1h">Last hour</option><option value="24h">Last 24 hours</option><option value="168h">Last 7 days</option><option value="720h">Last 30 days</option>
      </select>
    </div>
    <div class="feed" id="feed"><p class="empty">Loading...</p></div>`;
  $("#since").value = feedState.since;
  const paint = (all) => {
    document.querySelectorAll("#filters .chip").forEach((c) => c.classList.toggle("active", c.dataset.metric === feedState.metric));
    const ms = all.filter((m) => !feedState.metric || m.metric === feedState.metric);
    $("#feed").innerHTML = ms.length ? ms.map(card).join("") : '<p class="empty">No mysteries in this range. Run something heavy, or wait for your machine to misbehave.</p>';
  };
  let all = [];
  const load = async () => {
    try {
      all = await getJSON(`api/mysteries?since=${feedState.since}&limit=500`);
      paint(all);
    } catch (e) {
      $("#feed").innerHTML = `<p class="empty">Could not load mysteries: ${esc(e.message)}</p>`;
    }
  };
  $("#filters").addEventListener("click", (e) => {
    const c = e.target.closest(".chip");
    if (!c) return;
    feedState.metric = c.dataset.metric;
    paint(all);
  });
  $("#since").addEventListener("change", (e) => { feedState.since = e.target.value; load(); });
  const onEvent = (kind) => { if (kind === "mystery") load(); };
  live.listeners.add(onEvent);
  cleanups.push(() => live.listeners.delete(onEvent));
  await load();
}

function card(m) {
  const start = new Date(m.start);
  const dur = (new Date(m.end) - start) / 1000;
  const shares = (m.culprits || []).slice(0, 3).map((c) => `
    <span>${esc(c.app)}${c.newcomer ? '<span class="tag">new</span>' : ""}</span>
    <div class="bar"><i style="width:${(c.share * 100).toFixed(1)}%"></i></div>
    <span class="pct">${Math.round(c.share * 100)}%</span>`).join("");
  return `<a class="card" href="#/mystery/${m.id}">
    <div class="head">${badge(m.metric)}<span>${fmtDay(start)} ${fmtClock(start)}</span><span>${fmtDur(dur)}</span>
      ${m.ratio ? `<span>${m.ratio.toFixed(1)}x normal</span>` : ""}<span class="dim">#${m.id}</span></div>
    <div class="summary">${esc(m.summary)}</div>
    ${shares ? `<div class="shares">${shares}</div>` : ""}
  </a>`;
}

// ---------- Mystery detail ----------
async function renderMystery(id) {
  view.innerHTML = `<a class="back" href="#/mysteries">&larr; All mysteries</a><div id="detail"><p class="empty">Loading...</p></div>`;
  let d;
  try {
    d = await getJSON("api/mysteries/" + encodeURIComponent(id));
  } catch (e) {
    $("#detail").innerHTML = `<p class="empty">Mystery #${esc(id)}: ${esc(e.message)}</p>`;
    return;
  }
  const m = d.mystery;
  const meta = METRICS[m.metric];
  const start = new Date(m.start), end = new Date(m.end);
  const dur = (end - start) / 1000;
  const appNote = m.metric === "cpu" ? "App lines are scaled to all cores so they share the system's axis." : "";
  $("#detail").innerHTML = `
    <div class="panel">
      <div class="card head" style="padding:0;border:0;background:none">${badge(m.metric)}<span>${fmtDay(start)} ${fmtClock(start)}</span><span class="dim">#${m.id}</span>${m.truncated ? '<span class="tag">still high when closed</span>' : ""}</div>
      <p class="big">${esc(m.summary)}</p>
      <dl class="facts">
        <div><dt>Duration</dt><dd>${fmtDur(dur)}</dd></div>
        <div><dt>Normal</dt><dd>${meta.fmt(m.baseline)}</dd></div>
        <div><dt>Threshold</dt><dd>${meta.fmt(m.threshold)}</dd></div>
        <div><dt>Average</dt><dd>${meta.fmt(m.mean)}</dd></div>
        <div><dt>Peak</dt><dd>${meta.fmt(m.peak)}</dd></div>
        <div><dt>vs normal</dt><dd>${m.ratio ? m.ratio.toFixed(1) + "x" : "from ~0"}</dd></div>
      </dl>
      <h2>${meta.label} around the spike</h2>
      <div class="plot" id="dplot"></div>
      <p class="note">Shaded: the spike. Dashed: the threshold it crossed. ${appNote}</p>
    </div>
    <div class="grid two" style="margin-top:16px">
      <section class="panel"><h2>Who added the load</h2>${culpritTable(m, meta)}</section>
      <section class="panel"><h2>Started just before</h2>${newcomerList(m)}</section>
    </div>`;
  const series = [{ label: "System", color: COLORS[m.metric] }];
  const data = [d.system.ts, d.system.v];
  d.apps.forEach((a, i) => {
    series.push({ label: a.app, color: APP_COLORS[i % APP_COLORS.length], fill: false, width: 1.5 });
    data.push(a.v);
  });
  const u = makeChart($("#dplot"), series, meta.fmt, {
    height: 260, data, legend: true, range: m.metric === "cpu" ? () => [0, 100] : undefined,
    plugins: [
      bandsPlugin(() => [{ from: start / 1000, to: end / 1000, color: "rgba(248,81,73,.16)" }]),
      hlinesPlugin(() => [{ value: m.threshold, color: "rgba(248,81,73,.8)" }]),
    ],
  });
  cleanups.push(() => { u._ro.disconnect(); u.destroy(); });
}

function culpritTable(m, meta) {
  const cs = m.culprits || [];
  if (!cs.length) return '<p class="empty">No app stood out: the extra load was spread thin, or came from the kernel or something whyslow cannot see.</p>';
  return `<div class="table-wrap"><table><thead><tr><th>App</th><th>Share</th><th class="n">Before</th><th class="n">During</th></tr></thead><tbody>
    ${cs.map((c) => `<tr><td>${esc(c.app)}${c.newcomer ? '<span class="tag">new</span>' : ""}</td>
      <td><div class="bar" title="${Math.round(c.share * 100)}%"><i style="width:${(c.share * 100).toFixed(1)}%"></i></div></td>
      <td class="n">${appFmt(m.metric, c.before)}</td><td class="n">${appFmt(m.metric, c.during)}</td></tr>`).join("")}
  </tbody></table></div><p class="note">Share = the app's increase over the minute before, divided by all apps' increases. It is correlation in time, not proof.</p>`;
}
const appFmt = (metric, v) => (metric === "cpu" ? fmtPct(v) + " core" : METRICS[metric].fmt(v));

function newcomerList(m) {
  const ns = m.newcomers || [];
  if (!ns.length) return '<p class="empty">Nothing new started in the 15 seconds before this spike.</p>';
  return `<ul class="offenders">${ns.map((n) => `<li><b>${esc(n.app)}</b> ${n.started_ago >= 0 ? `started ${fmtDur(n.started_ago)} before` : `started ${fmtDur(-n.started_ago)} after`} the spike began</li>`).join("")}</ul>`;
}

// ---------- Report page ----------
async function renderReport(date) {
  const today = new Date();
  const iso = (d) => `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, "0")}-${String(d.getDate()).padStart(2, "0")}`;
  date = date || iso(today);
  view.innerHTML = `
    <h1>Daily report</h1>
    <div class="filters"><input type="date" id="date" value="${esc(date)}" max="${iso(today)}" aria-label="Day"></div>
    <div id="report"><p class="empty">Loading...</p></div>`;
  $("#date").addEventListener("change", (e) => { if (e.target.value) location.hash = "#/report/" + e.target.value; });
  let r;
  try {
    r = await getJSON("api/report?date=" + encodeURIComponent(date));
  } catch (e) {
    $("#report").innerHTML = `<p class="empty">Could not build the report: ${esc(e.message)}</p>`;
    return;
  }
  const watched = r.watched_ns / 1e9;
  if (!watched) {
    $("#report").innerHTML = '<p class="empty">No samples for this day. Was whyslow running?</p>';
    return;
  }
  const fmtVal = (unit, v) => (unit === "cpu_s" ? fmtDur(v) : fmtBytes(v));
  const section = (s) => {
    const max = s.apps.length ? s.apps[0].value : 1;
    return `<section class="panel"><h2>${esc(s.label)}</h2>${s.apps.length ? `<div class="rank">${s.apps.map((a, i) => `
      <span class="i">${i + 1}</span><span>${esc(a.app)}</span><div class="bar"><i style="width:${(a.value / max * 100).toFixed(1)}%"></i></div><span class="v">${fmtVal(s.unit, a.value)}</span>`).join("")}</div>`
      : '<p class="empty">Nothing recorded.</p>'}</section>`;
  };
  const counts = Object.entries(r.by_metric || {}).map(([m, n]) => `${badge(m)} ${n}`).join(" &nbsp; ");
  const b = r.battery;
  const battery = b ? `<p class="note">${fmtDur(b.on_battery_ns / 1e9)} on battery, ${b.drain_pct.toFixed(1)}% used${b.drain_wh ? ` (${b.drain_wh.toFixed(1)} Wh)` : ""}. Estimated from each app's CPU, disk and network while discharging; Linux has no per-app energy counter.</p>
      <div class="rank">${b.hogs.map((h, i) => `<span class="i">${i + 1}</span><span>${esc(h.app)}</span><div class="bar"><i style="width:${(h.pct / (b.hogs[0].pct || 1) * 100).toFixed(1)}%"></i></div><span class="v">~${h.pct.toFixed(1)}%</span>`).join("")}</div>`
    : '<p class="empty">No time on battery this day (or no battery).</p>';
  const repeat = r.repeat && r.repeat.length
    ? `<ul class="offenders">${r.repeat.map((o) => `<li>${o.kind === "culprit" ? badge(o.metric) + " " : '<span class="tag">opens, then spikes</span> '}${esc(o.text)} <span class="dim">last ${fmtAgo(new Date(o.last))}</span> ${o.ids.slice(-3).map((id) => `<a href="#/mystery/${id}">#${id}</a>`).join(" ")}</li>`).join("")}</ul>`
    : '<p class="empty">No app has caused two spikes yet.</p>';
  $("#report").innerHTML = `
    <div class="grid tiles">
      <div class="panel tile"><div class="label">Watched</div><div class="value">${fmtDur(watched)}</div><div class="extra">${esc(r.date)}</div></div>
      <div class="panel tile"><div class="label">Mysteries</div><div class="value">${(r.mysteries || []).length}</div><div class="extra">${counts || "none"}</div></div>
      <div class="panel tile"><div class="label">Busiest app</div><div class="value" style="font-size:20px">${esc((r.sections[0].apps[0] || {}).app || "-")}</div><div class="extra">by CPU time</div></div>
    </div>
    <div class="grid report">${r.sections.map(section).join("")}</div>
    <div class="grid two" style="margin-top:16px">
      <section class="panel"><h2>Repeat offenders <span class="dim" style="text-transform:none">since ${fmtDay(new Date(r.repeat_since))}</span></h2>${repeat}</section>
      <section class="panel"><h2>Battery hogs (estimate)</h2>${battery}</section>
    </div>`;
}

// ---------- router ----------
async function route() {
  cleanup();
  const parts = (location.hash.replace(/^#\/?/, "") || "live").split("/");
  const page = parts[0];
  document.querySelectorAll("nav a").forEach((a) => a.classList.toggle("active", a.dataset.nav === (page === "mystery" ? "mysteries" : page)));
  window.scrollTo(0, 0);
  if (page === "mysteries") return renderMysteries();
  if (page === "mystery" && parts[1]) return renderMystery(parts[1]);
  if (page === "report") return renderReport(parts[1]);
  document.title = "whyslow";
  return renderLive();
}

async function start() {
  loadStatus();
  try { live.mysteries = await getJSON("api/mysteries?since=1h&limit=50"); } catch (_) { /* shown empty */ }
  connect();
  window.addEventListener("hashchange", route);
  route();
}
start();
