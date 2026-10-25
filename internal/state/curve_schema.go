package state

// Curve research and simulated positions are isolated from cash-and-carry
// tables. Repeated scans remain auditable without multiplying baseline samples.
const curveSchema = `
CREATE TABLE IF NOT EXISTS curve_scans (
  run_id TEXT PRIMARY KEY REFERENCES runs(run_id),
  source TEXT NOT NULL,
  snapshot_json TEXT NOT NULL,
  metrics_json TEXT NOT NULL,
  candidates_json TEXT NOT NULL,
  skips_json TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS curve_metrics (
  id INTEGER PRIMARY KEY,
  run_id TEXT NOT NULL REFERENCES curve_scans(run_id),
  source TEXT NOT NULL,
  family TEXT NOT NULL,
  pair_key TEXT NOT NULL,
  observed_at TEXT NOT NULL,
  observed_at_ns INTEGER NOT NULL,
  quote_key TEXT NOT NULL,
  metric_json TEXT NOT NULL,
  UNIQUE(source,family,pair_key,quote_key)
);
CREATE INDEX IF NOT EXISTS curve_metric_history
  ON curve_metrics(source,family,pair_key,observed_at_ns);
CREATE TABLE IF NOT EXISTS curve_positions (
  id INTEGER PRIMARY KEY,
  entry_run_id TEXT NOT NULL REFERENCES runs(run_id),
  source TEXT NOT NULL,
  strategy TEXT NOT NULL,
  pair_key TEXT NOT NULL,
  opened_at TEXT NOT NULL,
  last_event_ns INTEGER NOT NULL,
  status TEXT NOT NULL CHECK(status IN ('open','awaiting_settlement','closed')),
  entry_json TEXT NOT NULL,
  policy_json TEXT NOT NULL,
  position_json TEXT NOT NULL,
  closed_at TEXT,
  version INTEGER NOT NULL DEFAULT 0
);
CREATE UNIQUE INDEX IF NOT EXISTS curve_positions_one_active_pair
  ON curve_positions(source,strategy,pair_key)
  WHERE status IN ('open','awaiting_settlement');
CREATE INDEX IF NOT EXISTS curve_positions_history
  ON curve_positions(source,strategy,pair_key,id);
CREATE TABLE IF NOT EXISTS curve_position_observations (
  id INTEGER PRIMARY KEY,
  position_id INTEGER NOT NULL REFERENCES curve_positions(id),
  run_id TEXT NOT NULL REFERENCES runs(run_id),
  observed_at TEXT NOT NULL,
  observed_at_ns INTEGER NOT NULL,
  kind TEXT NOT NULL CHECK(kind IN ('open','mark','settlement','awaiting_settlement','error')),
  observation_json TEXT NOT NULL,
  UNIQUE(position_id,observed_at_ns,kind)
);
`
