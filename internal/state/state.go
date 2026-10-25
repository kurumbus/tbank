// Package state keeps scan audits, alert receipts and explicitly simulated
// positions. Legacy mock_records remain observations of fill intent only.
package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/shopspring/decimal"
	_ "modernc.org/sqlite"

	"tbank/internal/scanner"
)

type Store struct{ db *sql.DB }

// Open creates one SQLite database. Source, assumptions, quotes and every
// scenario are preserved in the run JSON. Historical fill-intent records are
// never reinterpreted as opened simulated positions.
func Open(path string) (*Store, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("state database path is empty")
	}
	if path != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return nil, fmt.Errorf("create state directory: %w", err)
		}
		f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
		if err != nil {
			return nil, fmt.Errorf("create state database: %w", err)
		}
		if err := f.Close(); err != nil {
			return nil, err
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open state database: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	const schema = `
PRAGMA busy_timeout = 5000;
PRAGMA foreign_keys = ON;
CREATE TABLE IF NOT EXISTS runs (
  run_id TEXT PRIMARY KEY,
  started_at TEXT NOT NULL,
  source TEXT NOT NULL,
  results_json TEXT NOT NULL,
  skips_json TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS notifications (
  candidate_key TEXT PRIMARY KEY,
  notified_at TEXT NOT NULL,
  best_excess TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS mock_records (
  id INTEGER PRIMARY KEY,
  run_id TEXT NOT NULL REFERENCES runs(run_id),
  source TEXT NOT NULL,
  candidate_key TEXT NOT NULL,
  observed_at TEXT NOT NULL,
  expires_at TEXT NOT NULL,
  status TEXT NOT NULL CHECK(status = 'snapshot_fill_intent'),
  result_json TEXT NOT NULL,
  UNIQUE(source, candidate_key, expires_at)
);
CREATE INDEX IF NOT EXISTS mock_records_candidate ON mock_records(source, candidate_key);
`
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("initialize state database: %w", err)
	}
	if _, err := db.Exec(positionSchema); err != nil {
		db.Close()
		return nil, fmt.Errorf("initialize position database: %w", err)
	}
	s := &Store{db: db}
	if err := s.migratePositionObservationKey(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

// SaveRun is immutable: reusing a run ID returns an error rather than replacing
// an earlier audit. The source should identify fixture, sandbox or production.
func (s *Store) SaveRun(ctx context.Context, runID string, started time.Time, source string, results []scanner.Result, skips []string) error {
	if runID == "" || started.IsZero() || strings.TrimSpace(source) == "" {
		return errors.New("save run requires ID, timestamp and source")
	}
	rawResults, err := json.Marshal(results)
	if err != nil {
		return fmt.Errorf("encode scan results: %w", err)
	}
	rawSkips, err := json.Marshal(skips)
	if err != nil {
		return fmt.Errorf("encode scan skips: %w", err)
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO runs(run_id, started_at, source, results_json, skips_json) VALUES(?,?,?,?,?)`, runID, timestamp(started), source, string(rawResults), string(rawSkips))
	if err != nil {
		return fmt.Errorf("save run: %w", err)
	}
	return nil
}

// ShouldNotify is read-only. Call MarkNotified only after successful delivery.
// During cooldown a strictly better result can alert again if the improvement
// reaches the configured amount. An improvement of zero still requires a gain.
func (s *Store) ShouldNotify(ctx context.Context, key string, now time.Time, bestExcess decimal.Decimal, cooldown time.Duration, improvement decimal.Decimal) (bool, error) {
	if key == "" || now.IsZero() || cooldown < 0 || improvement.IsNegative() {
		return false, errors.New("invalid notification deduplication input")
	}
	var when, previous string
	err := s.db.QueryRowContext(ctx, `SELECT notified_at,best_excess FROM notifications WHERE candidate_key=?`, key).Scan(&when, &previous)
	if errors.Is(err, sql.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("read notification state: %w", err)
	}
	last, err := time.Parse(time.RFC3339Nano, when)
	if err != nil {
		return false, fmt.Errorf("invalid stored notification timestamp: %w", err)
	}
	prior, err := decimal.NewFromString(previous)
	if err != nil {
		return false, fmt.Errorf("invalid stored notification amount: %w", err)
	}
	delta := bestExcess.Sub(prior)
	return !now.Before(last.Add(cooldown)) || (delta.IsPositive() && delta.GreaterThanOrEqual(improvement)), nil
}

func (s *Store) MarkNotified(ctx context.Context, key string, now time.Time, bestExcess decimal.Decimal) error {
	if key == "" || now.IsZero() {
		return errors.New("notification state requires key and timestamp")
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO notifications(candidate_key,notified_at,best_excess) VALUES(?,?,?) ON CONFLICT(candidate_key) DO UPDATE SET notified_at=excluded.notified_at,best_excess=excluded.best_excess`, key, timestamp(now), bestExcess.String())
	if err != nil {
		return fmt.Errorf("save notification state: %w", err)
	}
	return nil
}

// RecordMock records at most one still-unexpired snapshot for a source/candidate.
// It neither sends nor simulates fills, cash transfers, P&L or a position.
// The originating run must already exist so its source remains auditable.
// inserted=false means a matching unexpired intent was already recorded.
func (s *Store) RecordMock(ctx context.Context, runID string, result scanner.Result) (inserted bool, err error) {
	observed := result.Snapshot.ObservedAt
	expiry := result.Snapshot.Pair.Future.Expiry
	if runID == "" || result.Key == "" || observed.IsZero() || !expiry.After(observed) || result.Contracts <= 0 || result.Shares <= 0 || !result.StockLimit.IsPositive() || !result.FutureLimit.IsPositive() {
		return false, errors.New("mock intent requires a run, candidate, future expiry, quantities and known positive limit prices")
	}
	raw, err := json.Marshal(result)
	if err != nil {
		return false, fmt.Errorf("encode mock intent: %w", err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin mock intent: %w", err)
	}
	defer tx.Rollback()
	var source string
	if err := tx.QueryRowContext(ctx, `SELECT source FROM runs WHERE run_id=?`, runID).Scan(&source); err != nil {
		return false, fmt.Errorf("read mock source run: %w", err)
	}
	rows, err := tx.QueryContext(ctx, `SELECT expires_at FROM mock_records WHERE source=? AND candidate_key=?`, source, result.Key)
	if err != nil {
		return false, fmt.Errorf("read mock intents: %w", err)
	}
	for rows.Next() {
		var date string
		if err := rows.Scan(&date); err != nil {
			rows.Close()
			return false, err
		}
		existingExpiry, err := time.Parse(time.RFC3339Nano, date)
		if err != nil {
			rows.Close()
			return false, fmt.Errorf("invalid mock expiry: %w", err)
		}
		if existingExpiry.After(observed) {
			rows.Close()
			return false, nil
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return false, err
	}
	rows.Close()
	res, err := tx.ExecContext(ctx, `INSERT INTO mock_records(run_id,source,candidate_key,observed_at,expires_at,status,result_json) VALUES(?,?,?,?,?,'snapshot_fill_intent',?) ON CONFLICT(source,candidate_key,expires_at) DO NOTHING`, runID, source, result.Key, timestamp(observed), timestamp(expiry), string(raw))
	if err != nil {
		return false, fmt.Errorf("save mock intent: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit mock intent: %w", err)
	}
	return n > 0, nil
}

func timestamp(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }
