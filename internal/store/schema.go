package store

// schemaVersion is bumped with every change to schema.
const schemaVersion = 1

// Rates in the raw tables are per second over the interval dt that ended
// at ts. The minute rollups keep averages and peaks for the system and
// totals for apps (CPU-seconds and bytes), so a daily report is a SUM.
const schema = `
CREATE TABLE IF NOT EXISTS meta (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS apps (
	id         INTEGER PRIMARY KEY,
	name       TEXT NOT NULL UNIQUE,
	first_seen INTEGER NOT NULL,
	last_seen  INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS sys_samples (
	ts          INTEGER PRIMARY KEY,
	dt          REAL NOT NULL,
	cpu         REAL NOT NULL,
	mem_used    INTEGER NOT NULL,
	mem_total   INTEGER NOT NULL,
	disk_r      REAL NOT NULL,
	disk_w      REAL NOT NULL,
	net_rx      REAL NOT NULL,
	net_tx      REAL NOT NULL,
	batt_pct    REAL,
	batt_w      REAL,
	batt_status TEXT,
	temp_c      REAL
) WITHOUT ROWID;

CREATE TABLE IF NOT EXISTS app_samples (
	ts      INTEGER NOT NULL,
	app_id  INTEGER NOT NULL REFERENCES apps(id),
	cpu     REAL NOT NULL,
	rss     INTEGER NOT NULL,
	disk_r  REAL NOT NULL,
	disk_w  REAL NOT NULL,
	net_rx  REAL NOT NULL,
	net_tx  REAL NOT NULL,
	procs   INTEGER NOT NULL,
	sockets INTEGER NOT NULL,
	PRIMARY KEY (app_id, ts)
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS app_samples_ts ON app_samples(ts);

CREATE TABLE IF NOT EXISTS sys_samples_1m (
	ts         INTEGER PRIMARY KEY,
	n          INTEGER NOT NULL,
	cpu        REAL NOT NULL,
	cpu_max    REAL NOT NULL,
	mem_used   REAL NOT NULL,
	disk_r     REAL NOT NULL,
	disk_r_max REAL NOT NULL,
	disk_w     REAL NOT NULL,
	disk_w_max REAL NOT NULL,
	net_rx     REAL NOT NULL,
	net_rx_max REAL NOT NULL,
	net_tx     REAL NOT NULL,
	net_tx_max REAL NOT NULL,
	batt_pct   REAL,
	temp_c     REAL
) WITHOUT ROWID;

CREATE TABLE IF NOT EXISTS app_samples_1m (
	ts       INTEGER NOT NULL,
	app_id   INTEGER NOT NULL REFERENCES apps(id),
	cpu_s    REAL NOT NULL,
	rss_max  INTEGER NOT NULL,
	disk_r_b REAL NOT NULL,
	disk_w_b REAL NOT NULL,
	net_rx_b REAL NOT NULL,
	net_tx_b REAL NOT NULL,
	PRIMARY KEY (app_id, ts)
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS app_samples_1m_ts ON app_samples_1m(ts);

CREATE TABLE IF NOT EXISTS mysteries (
	id             INTEGER PRIMARY KEY,
	metric         TEXT NOT NULL,
	started_at     REAL NOT NULL,
	ended_at       REAL NOT NULL,
	baseline       REAL NOT NULL,
	threshold      REAL NOT NULL,
	peak           REAL NOT NULL,
	mean           REAL NOT NULL,
	ratio          REAL NOT NULL,
	top_app        TEXT,
	top_share      REAL,
	culprits_json  TEXT NOT NULL,
	newcomers_json TEXT NOT NULL,
	summary        TEXT NOT NULL,
	truncated      INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS mysteries_started ON mysteries(started_at);
CREATE INDEX IF NOT EXISTS mysteries_top_app ON mysteries(top_app);

CREATE TABLE IF NOT EXISTS battery_windows (
	start            INTEGER PRIMARY KEY,
	end              INTEGER NOT NULL,
	drain_wh         REAL NOT NULL,
	drain_pct        REAL NOT NULL,
	avg_w            REAL NOT NULL,
	attribution_json TEXT NOT NULL
);
`
