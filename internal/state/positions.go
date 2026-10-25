package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/shopspring/decimal"
	"tbank/internal/scanner"
)

const positionSchema = `
CREATE TABLE IF NOT EXISTS positions (
  id INTEGER PRIMARY KEY,
  entry_run_id TEXT NOT NULL REFERENCES runs(run_id),
  source TEXT NOT NULL,
  stock_uid TEXT NOT NULL,
  future_uid TEXT NOT NULL,
  opened_at TEXT NOT NULL,
  latest_observed_at TEXT NOT NULL,
  last_event_ns INTEGER NOT NULL,
  status TEXT NOT NULL CHECK(status IN ('open','awaiting_settlement','closed')),
  minimum_tariff TEXT NOT NULL,
  entry_json TEXT NOT NULL,
  position_json TEXT NOT NULL,
  closed_at TEXT,
  version INTEGER NOT NULL DEFAULT 0
);
CREATE UNIQUE INDEX IF NOT EXISTS positions_one_active_pair
  ON positions(source,stock_uid,future_uid)
  WHERE status IN ('open','awaiting_settlement');
CREATE INDEX IF NOT EXISTS positions_source_pair
  ON positions(source,stock_uid,future_uid,id);
CREATE TABLE IF NOT EXISTS position_observations ` + positionObservationDefinition + `;
`

const positionObservationDefinition = `(
  id INTEGER PRIMARY KEY,
  position_id INTEGER NOT NULL REFERENCES positions(id),
  run_id TEXT NOT NULL REFERENCES runs(run_id),
  observed_at TEXT NOT NULL,
  observed_at_ns INTEGER NOT NULL,
  kind TEXT NOT NULL CHECK(kind IN ('open','mark','settlement','awaiting_settlement','error')),
  observation_json TEXT NOT NULL,
  UNIQUE(position_id,observed_at_ns,kind)
)`

// Early development databases keyed every event only by its effective time.
// Preserve their audit rows while permitting an official settlement observation
// at the same effective time as an earlier awaiting-settlement check.
func (s *Store) migratePositionObservationKey() error {
	ctx := context.Background()
	tx, err := s.beginPositionWrite(ctx)
	if err != nil {
		return err
	}
	defer tx.close()
	var definition string
	if err := tx.conn.QueryRowContext(ctx, `SELECT sql FROM sqlite_master WHERE type='table' AND name='position_observations'`).Scan(&definition); err != nil {
		return err
	}
	if !strings.Contains(strings.Join(strings.Fields(definition), ""), "UNIQUE(position_id,observed_at_ns)") {
		return nil
	}
	_, err = tx.conn.ExecContext(ctx, `CREATE TABLE position_observations_new `+positionObservationDefinition+`;
INSERT INTO position_observations_new SELECT * FROM position_observations;
DROP TABLE position_observations;
ALTER TABLE position_observations_new RENAME TO position_observations;`)
	if err != nil {
		return fmt.Errorf("migrate position observation identity: %w", err)
	}
	return tx.commit(ctx)
}

// OpenPosition freezes the original entry, quantities, capture target and
// minimum eligible tariff. Position identity excludes configured quantity:
// there can be only one open/awaiting pair per feed source. Legacy mock_records
// are deliberately not consulted or migrated.
func (s *Store) OpenPosition(ctx context.Context, runID string, result scanner.Result, captureTarget decimal.Decimal) (scanner.Position, bool, error) {
	tx, err := s.beginPositionWrite(ctx)
	if err != nil {
		return scanner.Position{}, false, err
	}
	defer tx.close()
	source, err := sourceForRun(ctx, tx.conn, runID)
	if err != nil {
		return scanner.Position{}, false, err
	}
	stockUID, futureUID := result.Snapshot.Pair.Stock.UID, result.Snapshot.Pair.Future.UID
	if stockUID == "" || futureUID == "" || result.Snapshot.ObservedAt.IsZero() {
		return scanner.Position{}, false, errors.New("position entry requires instrument UIDs and an observation timestamp")
	}
	const activeQuery = `SELECT position_json,last_event_ns,version FROM positions WHERE source=? AND stock_uid=? AND future_uid=? AND status IN ('open','awaiting_settlement')`
	existing, _, _, err := readStoredPosition(tx.conn.QueryRowContext(ctx, activeQuery, source, stockUID, futureUID))
	if err == nil {
		return existing, false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return scanner.Position{}, false, fmt.Errorf("read active position: %w", err)
	}
	// A closing quote cannot immediately create a replacement entry. This also
	// blocks replaying an older fixture after a persisted position was closed.
	existing, lastEvent, _, err := readStoredPosition(tx.conn.QueryRowContext(ctx, `SELECT position_json,last_event_ns,version FROM positions WHERE source=? AND stock_uid=? AND future_uid=? AND status='closed' ORDER BY last_event_ns DESC,id DESC LIMIT 1`, source, stockUID, futureUID))
	if err == nil && result.Snapshot.ObservedAt.UnixNano() <= lastEvent {
		return existing, false, nil
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return scanner.Position{}, false, fmt.Errorf("read previous closed position: %w", err)
	}
	p, err := scanner.NewPosition(result, source, captureTarget)
	if err != nil {
		return scanner.Position{}, false, fmt.Errorf("construct mock position: %w", err)
	}
	entryJSON, err := json.Marshal(p.Entry)
	if err != nil {
		return scanner.Position{}, false, err
	}
	pJSON, err := json.Marshal(p)
	if err != nil {
		return scanner.Position{}, false, err
	}
	res, err := tx.conn.ExecContext(ctx, `INSERT INTO positions(entry_run_id,source,stock_uid,future_uid,opened_at,latest_observed_at,last_event_ns,status,minimum_tariff,entry_json,position_json) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, runID, source, stockUID, futureUID, timestamp(p.OpenedAt), timestamp(p.OpenedAt), p.OpenedAt.UnixNano(), p.Status, p.MinimumTariff, string(entryJSON), string(pJSON))
	if err != nil {
		return scanner.Position{}, false, fmt.Errorf("insert mock position: %w", err)
	}
	p.ID, err = res.LastInsertId()
	if err != nil {
		return scanner.Position{}, false, err
	}
	pJSON, err = json.Marshal(p)
	if err != nil {
		return scanner.Position{}, false, err
	}
	if _, err := tx.conn.ExecContext(ctx, `UPDATE positions SET position_json=? WHERE id=?`, string(pJSON), p.ID); err != nil {
		return scanner.Position{}, false, err
	}
	if err := appendPositionObservation(ctx, tx.conn, p.ID, runID, p.OpenedAt, "open", entryJSON); err != nil {
		return scanner.Position{}, false, err
	}
	if err := tx.commit(ctx); err != nil {
		return scanner.Position{}, false, err
	}
	return p, true, nil
}

// ListPositions returns both active and historical positions, oldest first.
// An empty source includes every feed; fixture and production remain distinct.
func (s *Store) ListPositions(ctx context.Context, source string) ([]scanner.Position, error) {
	query := `SELECT position_json,last_event_ns,version FROM positions`
	args := []any{}
	if source != "" {
		query += ` WHERE source=?`
		args = append(args, source)
	}
	rows, err := s.db.QueryContext(ctx, query+` ORDER BY id`, args...)
	if err != nil {
		return nil, fmt.Errorf("list positions: %w", err)
	}
	defer rows.Close()
	positions := []scanner.Position{}
	for rows.Next() {
		p, _, _, err := readStoredPosition(rows)
		if err != nil {
			return nil, err
		}
		positions = append(positions, p)
	}
	return positions, rows.Err()
}

func (s *Store) GetPosition(ctx context.Context, id int64) (scanner.Position, error) {
	p, _, _, err := readStoredPosition(s.db.QueryRowContext(ctx, `SELECT position_json,last_event_ns,version FROM positions WHERE id=?`, id))
	if err != nil {
		return scanner.Position{}, fmt.Errorf("get position: %w", err)
	}
	return p, nil
}

type PositionObservation struct {
	ID         int64              `json:"id"`
	RunID      string             `json:"run_id"`
	ObservedAt time.Time          `json:"observed_at"`
	Kind       string             `json:"kind"`
	Valuation  *scanner.Valuation `json:"valuation,omitempty"`
	Error      string             `json:"error,omitempty"`
}

// Observations is the chronological audit of valid marks and unavailable checks.
// The opening entry itself remains available on Position.Entry.
func (s *Store) Observations(ctx context.Context, id int64) ([]PositionObservation, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,run_id,observed_at,kind,observation_json FROM position_observations WHERE position_id=? ORDER BY observed_at_ns,id`, id)
	if err != nil {
		return nil, fmt.Errorf("list position observations: %w", err)
	}
	defer rows.Close()
	observations := []PositionObservation{}
	for rows.Next() {
		var o PositionObservation
		var date, raw string
		if err := rows.Scan(&o.ID, &o.RunID, &date, &o.Kind, &raw); err != nil {
			return nil, err
		}
		o.ObservedAt, err = time.Parse(time.RFC3339Nano, date)
		if err != nil {
			return nil, fmt.Errorf("decode observation timestamp: %w", err)
		}
		switch o.Kind {
		case "mark", "settlement":
			if err := json.Unmarshal([]byte(raw), &o.Valuation); err != nil {
				return nil, fmt.Errorf("decode position valuation: %w", err)
			}
		case "error":
			var payload struct {
				Reason string `json:"reason"`
			}
			if err := json.Unmarshal([]byte(raw), &payload); err != nil {
				return nil, fmt.Errorf("decode position error: %w", err)
			}
			o.Error = payload.Reason
		}
		observations = append(observations, o)
	}
	return observations, rows.Err()
}

// RecordValuation atomically appends a mark and updates the latest valid value.
// The caller obtains valuations through scanner.MarkPosition. Closing decisions
// are part of that immutable valuation, not recalculated with current settings.
func (s *Store) RecordValuation(ctx context.Context, runID string, id int64, v scanner.Valuation) (scanner.Position, bool, error) {
	if v.Kind != "mark" {
		return scanner.Position{}, false, errors.New("RecordValuation requires a mark valuation")
	}
	return s.writePositionEvent(ctx, runID, id, v.ObservedAt, "mark", &v, "")
}

// SettlePosition accepts a valuation produced by scanner.SettlePosition. It
// never infers settlement prices from the last quote or from entry assumptions.
func (s *Store) SettlePosition(ctx context.Context, runID string, id int64, v scanner.Valuation) (scanner.Position, bool, error) {
	if v.Kind != "settlement" {
		return scanner.Position{}, false, errors.New("SettlePosition requires a settlement valuation")
	}
	return s.writePositionEvent(ctx, runID, id, v.ObservedAt, "settlement", &v, "")
}

func (s *Store) AwaitSettlement(ctx context.Context, runID string, id int64, at time.Time) (scanner.Position, bool, error) {
	return s.writePositionEvent(ctx, runID, id, at, "awaiting_settlement", nil, "")
}

// RecordPositionError persists unavailable/stale quote checks while retaining
// the last valid valuation. Failed hourly checks are not fabricated price marks.
func (s *Store) RecordPositionError(ctx context.Context, runID string, id int64, at time.Time, reason string) (scanner.Position, bool, error) {
	if strings.TrimSpace(reason) == "" {
		return scanner.Position{}, false, errors.New("position observation error requires a reason")
	}
	return s.writePositionEvent(ctx, runID, id, at, "error", nil, reason)
}

func (s *Store) writePositionEvent(ctx context.Context, runID string, id int64, at time.Time, kind string, v *scanner.Valuation, reason string) (scanner.Position, bool, error) {
	if id <= 0 || at.IsZero() {
		return scanner.Position{}, false, errors.New("position observation requires ID and timestamp")
	}
	tx, err := s.beginPositionWrite(ctx)
	if err != nil {
		return scanner.Position{}, false, err
	}
	defer tx.close()
	p, lastEvent, version, err := readStoredPosition(tx.conn.QueryRowContext(ctx, `SELECT position_json,last_event_ns,version FROM positions WHERE id=?`, id))
	if err != nil {
		return scanner.Position{}, false, fmt.Errorf("read position for observation: %w", err)
	}
	source, err := sourceForRun(ctx, tx.conn, runID)
	if err != nil {
		return scanner.Position{}, false, err
	}
	if source != p.Source {
		return p, false, errors.New("position observation source differs from entry source")
	}
	if p.Status == "closed" {
		return p, false, nil
	}
	if kind == "settlement" {
		latestValid := p.OpenedAt
		if p.Latest != nil && p.Latest.ObservedAt.After(latestValid) {
			latestValid = p.Latest.ObservedAt
		}
		if at.Before(latestValid) {
			return p, false, nil
		}
	} else if at.UnixNano() <= lastEvent {
		return p, false, nil
	}
	original := p
	lastTrade := p.Entry.Snapshot.Pair.Future.LastTrade
	switch kind {
	case "mark":
		if p.Status != "open" {
			return p, false, errors.New("awaiting-settlement position cannot receive a trading mark")
		}
		if !lastTrade.IsZero() && !at.Before(lastTrade) {
			return p, false, errors.New("trading has ended; position requires awaiting settlement")
		}
		if v.Snapshot == nil || v.Snapshot.Pair.Stock.UID != p.Entry.Snapshot.Pair.Stock.UID || v.Snapshot.Pair.Future.UID != p.Entry.Snapshot.Pair.Future.UID || !v.Snapshot.ObservedAt.Equal(at) {
			return p, false, errors.New("mark snapshot does not match position identity or timestamp")
		}
		// A valuation may have been computed before another process committed a
		// mark. Recompute under the writer lock so book timestamps, frozen rates
		// and the closing decision use the actual latest persisted position.
		computed, err := scanner.MarkPosition(p, *v.Snapshot)
		if err != nil {
			return p, false, fmt.Errorf("validate current position mark: %w", err)
		}
		v = &computed
	case "settlement":
		if v.Settlement == nil || !v.Settlement.Verified || strings.TrimSpace(v.Settlement.Reference) == "" || !v.Settlement.StockPrice.IsPositive() || !v.Settlement.FuturePrice.IsPositive() || v.Settlement.Dividends.IsNegative() || !v.Settlement.ObservedAt.Equal(at) || at.Before(p.Entry.Snapshot.Pair.Future.Expiry) {
			return p, false, errors.New("settlement requires verified evidence at or after expiry")
		}
		computed, err := scanner.SettlePosition(p, *v.Settlement)
		if err != nil {
			return p, false, fmt.Errorf("validate current position settlement: %w", err)
		}
		v = &computed
	case "awaiting_settlement":
		if lastTrade.IsZero() || at.Before(lastTrade) {
			return p, false, errors.New("cannot await settlement before the last trading time")
		}
		p.Status = "awaiting_settlement"
	}
	var payload any
	if v != nil {
		p.Latest = v
		if v.ShouldClose {
			p.Status = "closed"
			closedAt := at.UTC()
			p.ClosedAt = &closedAt
			p.CloseReason = v.CloseReason
		}
		payload = v
	} else {
		payload = struct {
			ObservedAt time.Time `json:"observed_at"`
			Kind       string    `json:"kind"`
			Reason     string    `json:"reason,omitempty"`
		}{at.UTC(), kind, reason}
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return original, false, err
	}
	if err := appendPositionObservation(ctx, tx.conn, id, runID, at, kind, raw); err != nil {
		return original, false, err
	}
	pJSON, err := json.Marshal(p)
	if err != nil {
		return original, false, err
	}
	var closedAt any
	if p.ClosedAt != nil {
		closedAt = timestamp(*p.ClosedAt)
	}
	eventClock := at.UnixNano()
	if lastEvent > eventClock {
		eventClock = lastEvent
	}
	res, err := tx.conn.ExecContext(ctx, `UPDATE positions SET latest_observed_at=?,last_event_ns=?,status=?,position_json=?,closed_at=?,version=version+1 WHERE id=? AND version=? AND last_event_ns=?`, timestamp(time.Unix(0, eventClock)), eventClock, p.Status, string(pJSON), closedAt, id, version, lastEvent)
	if err != nil {
		return original, false, fmt.Errorf("update position observation: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return original, false, err
	}
	if n != 1 {
		return original, false, errors.New("position changed while recording observation")
	}
	if err := tx.commit(ctx); err != nil {
		return original, false, err
	}
	return p, true, nil
}

type rowScanner interface{ Scan(...any) error }

func readStoredPosition(row rowScanner) (scanner.Position, int64, int64, error) {
	var raw string
	var lastEvent, version int64
	if err := row.Scan(&raw, &lastEvent, &version); err != nil {
		return scanner.Position{}, 0, 0, err
	}
	var p scanner.Position
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return scanner.Position{}, 0, 0, fmt.Errorf("decode stored position: %w", err)
	}
	return p, lastEvent, version, nil
}

func sourceForRun(ctx context.Context, conn *sql.Conn, runID string) (string, error) {
	var source string
	if err := conn.QueryRowContext(ctx, `SELECT source FROM runs WHERE run_id=?`, runID).Scan(&source); err != nil {
		return "", fmt.Errorf("read position source run: %w", err)
	}
	return source, nil
}

func appendPositionObservation(ctx context.Context, conn *sql.Conn, id int64, runID string, at time.Time, kind string, raw []byte) error {
	_, err := conn.ExecContext(ctx, `INSERT INTO position_observations(position_id,run_id,observed_at,observed_at_ns,kind,observation_json) VALUES(?,?,?,?,?,?)`, id, runID, timestamp(at), at.UnixNano(), kind, string(raw))
	if err != nil {
		return fmt.Errorf("append position observation: %w", err)
	}
	return nil
}

// A reserved writer lock serializes read/check/write across multiple processes,
// not just goroutines using one Store. Updates also compare the stored version.
type positionWrite struct {
	conn *sql.Conn
	done bool
}

func (s *Store) beginPositionWrite(ctx context.Context) (*positionWrite, error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire position database: %w", err)
	}
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		conn.Close()
		return nil, fmt.Errorf("begin position update: %w", err)
	}
	return &positionWrite{conn: conn}, nil
}
func (tx *positionWrite) commit(ctx context.Context) error {
	if _, err := tx.conn.ExecContext(ctx, `COMMIT`); err != nil {
		return fmt.Errorf("commit position update: %w", err)
	}
	tx.done = true
	return nil
}
func (tx *positionWrite) close() {
	if !tx.done {
		_, _ = tx.conn.ExecContext(context.Background(), `ROLLBACK`)
	}
	_ = tx.conn.Close()
}
