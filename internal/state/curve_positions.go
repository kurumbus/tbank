package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"tbank/internal/curve"
)

// OpenCurvePosition freezes the domain-validated entry and its policy. Opposite
// directions and different quantities share the same active instrument pair.
func (s *Store) OpenCurvePosition(ctx context.Context, runID string, candidate curve.Candidate) (curve.Position, bool, error) {
	pairKey, err := curveCandidatePairKey(candidate)
	if err != nil {
		return curve.Position{}, false, err
	}
	if candidate.Snapshot.ObservedAt.IsZero() {
		return curve.Position{}, false, errors.New("curve entry observation time is required")
	}
	tx, err := s.beginPositionWrite(ctx)
	if err != nil {
		return curve.Position{}, false, err
	}
	defer tx.close()
	source, err := sourceForRun(ctx, tx.conn, runID)
	if err != nil {
		return curve.Position{}, false, err
	}
	p, _, _, err := readStoredCurvePosition(tx.conn.QueryRowContext(ctx, `SELECT position_json,last_event_ns,version FROM curve_positions WHERE source=? AND strategy=? AND pair_key=? AND status IN ('open','awaiting_settlement')`, source, candidate.Strategy, pairKey))
	if err == nil {
		return p, false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return curve.Position{}, false, fmt.Errorf("read active curve position: %w", err)
	}
	p, lastEvent, _, err := readStoredCurvePosition(tx.conn.QueryRowContext(ctx, `SELECT position_json,last_event_ns,version FROM curve_positions WHERE source=? AND strategy=? AND pair_key=? AND status='closed' ORDER BY last_event_ns DESC,id DESC LIMIT 1`, source, candidate.Strategy, pairKey))
	if err == nil && candidate.Snapshot.ObservedAt.UnixNano() <= lastEvent {
		return p, false, nil
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return curve.Position{}, false, fmt.Errorf("read closed curve position: %w", err)
	}
	p, err = curve.NewPosition(candidate, source)
	if err != nil {
		return curve.Position{}, false, fmt.Errorf("construct curve position: %w", err)
	}
	entryJSON, err := json.Marshal(p.Entry)
	if err != nil {
		return curve.Position{}, false, err
	}
	policyJSON, err := json.Marshal(p.Policy)
	if err != nil {
		return curve.Position{}, false, err
	}
	pJSON, err := json.Marshal(p)
	if err != nil {
		return curve.Position{}, false, err
	}
	res, err := tx.conn.ExecContext(ctx, `INSERT INTO curve_positions(entry_run_id,source,strategy,pair_key,opened_at,last_event_ns,status,entry_json,policy_json,position_json) VALUES(?,?,?,?,?,?,?,?,?,?)`, runID, source, candidate.Strategy, pairKey, timestamp(p.OpenedAt), p.OpenedAt.UnixNano(), p.Status, string(entryJSON), string(policyJSON), string(pJSON))
	if err != nil {
		return curve.Position{}, false, fmt.Errorf("insert curve position: %w", err)
	}
	p.ID, err = res.LastInsertId()
	if err != nil {
		return curve.Position{}, false, err
	}
	pJSON, err = json.Marshal(p)
	if err != nil {
		return curve.Position{}, false, err
	}
	if _, err := tx.conn.ExecContext(ctx, `UPDATE curve_positions SET position_json=? WHERE id=?`, string(pJSON), p.ID); err != nil {
		return curve.Position{}, false, err
	}
	if err := appendCurveObservation(ctx, tx.conn, p.ID, runID, p.OpenedAt, "open", entryJSON); err != nil {
		return curve.Position{}, false, err
	}
	if err := tx.commit(ctx); err != nil {
		return curve.Position{}, false, err
	}
	return p, true, nil
}

func (s *Store) ListCurvePositions(ctx context.Context, source string) ([]curve.Position, error) {
	query := `SELECT position_json,last_event_ns,version FROM curve_positions`
	args := []any{}
	if source != "" {
		query += ` WHERE source=?`
		args = append(args, source)
	}
	rows, err := s.db.QueryContext(ctx, query+` ORDER BY id`, args...)
	if err != nil {
		return nil, fmt.Errorf("list curve positions: %w", err)
	}
	defer rows.Close()
	positions := []curve.Position{}
	for rows.Next() {
		p, _, _, err := readStoredCurvePosition(rows)
		if err != nil {
			return nil, err
		}
		positions = append(positions, p)
	}
	return positions, rows.Err()
}

func (s *Store) GetCurvePosition(ctx context.Context, id int64) (curve.Position, error) {
	p, _, _, err := readStoredCurvePosition(s.db.QueryRowContext(ctx, `SELECT position_json,last_event_ns,version FROM curve_positions WHERE id=?`, id))
	if err != nil {
		return curve.Position{}, fmt.Errorf("get curve position: %w", err)
	}
	return p, nil
}

type CurvePositionObservation struct {
	ID         int64            `json:"id"`
	RunID      string           `json:"run_id"`
	ObservedAt time.Time        `json:"observed_at"`
	Kind       string           `json:"kind"`
	Valuation  *curve.Valuation `json:"valuation,omitempty"`
	Error      string           `json:"error,omitempty"`
}

func (s *Store) CurveObservations(ctx context.Context, id int64) ([]CurvePositionObservation, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,run_id,observed_at,kind,observation_json FROM curve_position_observations WHERE position_id=? ORDER BY observed_at_ns,id`, id)
	if err != nil {
		return nil, fmt.Errorf("list curve position observations: %w", err)
	}
	defer rows.Close()
	observations := []CurvePositionObservation{}
	for rows.Next() {
		var o CurvePositionObservation
		var date, raw string
		if err := rows.Scan(&o.ID, &o.RunID, &date, &o.Kind, &raw); err != nil {
			return nil, err
		}
		o.ObservedAt, err = time.Parse(time.RFC3339Nano, date)
		if err != nil {
			return nil, fmt.Errorf("decode curve observation timestamp: %w", err)
		}
		switch o.Kind {
		case "mark", "settlement":
			if err := json.Unmarshal([]byte(raw), &o.Valuation); err != nil {
				return nil, err
			}
		case "error":
			var payload struct {
				Reason string `json:"reason"`
			}
			if err := json.Unmarshal([]byte(raw), &payload); err != nil {
				return nil, err
			}
			o.Error = payload.Reason
		}
		observations = append(observations, o)
	}
	return observations, rows.Err()
}

// RecordCurveMark computes a valuation while holding the database writer lock.
// A caller cannot overwrite current state using stale precomputed financials.
func (s *Store) RecordCurveMark(ctx context.Context, runID string, id int64, snapshot curve.Snapshot) (curve.Position, bool, error) {
	return s.writeCurveEvent(ctx, runID, id, snapshot.ObservedAt, "mark", &snapshot, "")
}

func (s *Store) AwaitCurveSettlement(ctx context.Context, runID string, id int64, at time.Time) (curve.Position, bool, error) {
	return s.writeCurveEvent(ctx, runID, id, at, "awaiting_settlement", nil, "")
}

func (s *Store) RecordCurvePositionError(ctx context.Context, runID string, id int64, at time.Time, reason string) (curve.Position, bool, error) {
	if strings.TrimSpace(reason) == "" {
		return curve.Position{}, false, errors.New("curve observation failure requires a reason")
	}
	return s.writeCurveEvent(ctx, runID, id, at, "error", nil, reason)
}

func (s *Store) writeCurveEvent(ctx context.Context, runID string, id int64, at time.Time, kind string, snapshot *curve.Snapshot, reason string) (curve.Position, bool, error) {
	if id <= 0 || at.IsZero() {
		return curve.Position{}, false, errors.New("curve observation requires position ID and timestamp")
	}
	tx, err := s.beginPositionWrite(ctx)
	if err != nil {
		return curve.Position{}, false, err
	}
	defer tx.close()
	p, lastEvent, version, err := readStoredCurvePosition(tx.conn.QueryRowContext(ctx, `SELECT position_json,last_event_ns,version FROM curve_positions WHERE id=?`, id))
	if err != nil {
		return curve.Position{}, false, fmt.Errorf("read curve position for observation: %w", err)
	}
	source, err := sourceForRun(ctx, tx.conn, runID)
	if err != nil {
		return p, false, err
	}
	if source != p.Source {
		return p, false, errors.New("curve observation source differs from entry source")
	}
	if p.Status == "closed" || at.UnixNano() <= lastEvent {
		return p, false, nil
	}
	original := p
	var payload any
	switch kind {
	case "mark":
		v, err := curve.MarkPosition(p, *snapshot)
		if err != nil {
			return p, false, fmt.Errorf("value current curve position: %w", err)
		}
		p.Latest = &v
		if v.ShouldClose {
			p.Status = "closed"
			closedAt := at.UTC()
			p.ClosedAt = &closedAt
			p.CloseReason = v.CloseReason
		}
		payload = v
	case "awaiting_settlement":
		lastTrade := curve.LastTrade(p)
		if lastTrade.IsZero() || at.Before(lastTrade) {
			return p, false, errors.New("curve position has not reached its last trading time")
		}
		p.Status = "awaiting_settlement"
		payload = struct {
			ObservedAt time.Time `json:"observed_at"`
			Kind       string    `json:"kind"`
		}{at.UTC(), kind}
	case "error":
		payload = struct {
			ObservedAt time.Time `json:"observed_at"`
			Kind       string    `json:"kind"`
			Reason     string    `json:"reason"`
		}{at.UTC(), kind, reason}
	default:
		return p, false, errors.New("unsupported curve observation kind")
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return original, false, err
	}
	if err := appendCurveObservation(ctx, tx.conn, id, runID, at, kind, raw); err != nil {
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
	res, err := tx.conn.ExecContext(ctx, `UPDATE curve_positions SET last_event_ns=?,status=?,position_json=?,closed_at=?,version=version+1 WHERE id=? AND version=? AND last_event_ns=?`, at.UnixNano(), p.Status, string(pJSON), closedAt, id, version, lastEvent)
	if err != nil {
		return original, false, fmt.Errorf("update curve observation: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return original, false, err
	}
	if n != 1 {
		return original, false, errors.New("curve position changed during observation")
	}
	if err := tx.commit(ctx); err != nil {
		return original, false, err
	}
	return p, true, nil
}

func readStoredCurvePosition(row rowScanner) (curve.Position, int64, int64, error) {
	var raw string
	var at, version int64
	if err := row.Scan(&raw, &at, &version); err != nil {
		return curve.Position{}, 0, 0, err
	}
	var p curve.Position
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return curve.Position{}, 0, 0, fmt.Errorf("decode stored curve position: %w", err)
	}
	return p, at, version, nil
}

func appendCurveObservation(ctx context.Context, conn *sql.Conn, id int64, runID string, at time.Time, kind string, raw []byte) error {
	_, err := conn.ExecContext(ctx, `INSERT INTO curve_position_observations(position_id,run_id,observed_at,observed_at_ns,kind,observation_json) VALUES(?,?,?,?,?,?)`, id, runID, timestamp(at), at.UnixNano(), kind, string(raw))
	if err != nil {
		return fmt.Errorf("append curve observation: %w", err)
	}
	return nil
}
