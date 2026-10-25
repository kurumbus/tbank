package state

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"tbank/internal/curve"
)

// SaveCurveScan supplements an existing immutable run with its complete curve
// audit. Every valid metric contributes to baseline history independently of
// whether the scan produced an eligible candidate. Repeated quote timestamps
// remain in the scan audit but do not multiply historical baseline samples.
func (s *Store) SaveCurveScan(ctx context.Context, runID string, snapshot curve.Snapshot, metrics []curve.Metric, candidates []curve.Candidate, skips []string) error {
	snapshotJSON, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	metricsJSON, err := json.Marshal(metrics)
	if err != nil {
		return err
	}
	candidatesJSON, err := json.Marshal(candidates)
	if err != nil {
		return err
	}
	skipsJSON, err := json.Marshal(skips)
	if err != nil {
		return err
	}
	tx, err := s.beginPositionWrite(ctx)
	if err != nil {
		return err
	}
	defer tx.close()
	source, err := sourceForRun(ctx, tx.conn, runID)
	if err != nil {
		return err
	}
	_, err = tx.conn.ExecContext(ctx, `INSERT INTO curve_scans(run_id,source,snapshot_json,metrics_json,candidates_json,skips_json) VALUES(?,?,?,?,?,?)`, runID, source, string(snapshotJSON), string(metricsJSON), string(candidatesJSON), string(skipsJSON))
	if err != nil {
		return fmt.Errorf("save curve scan: %w", err)
	}
	for _, metric := range metrics {
		quoteKey, err := curveMetricQuoteKey(metric)
		if err != nil {
			return fmt.Errorf("invalid curve history metric %q: %w", metric.Key, err)
		}
		if snapshot.ObservedAt.IsZero() || metric.ObservedAt.After(snapshot.ObservedAt) {
			return errors.New("curve metric observation is later than its scan")
		}
		raw, err := json.Marshal(metric)
		if err != nil {
			return err
		}
		_, err = tx.conn.ExecContext(ctx, `INSERT INTO curve_metrics(run_id,source,family,pair_key,observed_at,observed_at_ns,quote_key,metric_json) VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(source,family,pair_key,quote_key) DO NOTHING`, runID, source, metric.Strategy, metric.Key, timestamp(metric.ObservedAt), metric.ObservedAt.UnixNano(), quoteKey, string(raw))
		if err != nil {
			return fmt.Errorf("save curve history metric: %w", err)
		}
	}
	return tx.commit(ctx)
}

// CurveHistory returns at most the latest limit samples strictly before before,
// in chronological order. Source, strategy and metric key are all mandatory so
// fixture data and distinct instrument families cannot warm each other's model.
func (s *Store) CurveHistory(ctx context.Context, source, strategy, metricKey string, before time.Time, limit int) ([]curve.Metric, error) {
	if strings.TrimSpace(source) == "" || strings.TrimSpace(strategy) == "" || metricKey == "" || before.IsZero() || limit < 1 || limit > 10000 {
		return nil, errors.New("curve history requires source, strategy, key, before time and limit 1..10000")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT metric_json FROM (SELECT metric_json,observed_at_ns,id FROM curve_metrics WHERE source=? AND family=? AND pair_key=? AND observed_at_ns<? ORDER BY observed_at_ns DESC,id DESC LIMIT ?) ORDER BY observed_at_ns,id`, source, strategy, metricKey, before.UnixNano(), limit)
	if err != nil {
		return nil, fmt.Errorf("read curve metric history: %w", err)
	}
	defer rows.Close()
	history := []curve.Metric{}
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var metric curve.Metric
		if err := json.Unmarshal([]byte(raw), &metric); err != nil {
			return nil, fmt.Errorf("decode curve metric history: %w", err)
		}
		history = append(history, metric)
	}
	return history, rows.Err()
}

func curveMetricQuoteKey(metric curve.Metric) (string, error) {
	if metric.Key == "" || metric.Strategy == "" || metric.ObservedAt.IsZero() || len(metric.InstrumentUIDs) == 0 || len(metric.InstrumentUIDs) != len(metric.QuoteTimes) {
		return "", errors.New("metric requires key, strategy, timestamp and one quote time per instrument")
	}
	type quote struct {
		UID  string `json:"uid"`
		Time string `json:"time"`
	}
	quotes := make([]quote, len(metric.InstrumentUIDs))
	seen := map[string]bool{}
	for i, uid := range metric.InstrumentUIDs {
		at := metric.QuoteTimes[i]
		if strings.TrimSpace(uid) == "" || seen[uid] || at.IsZero() || at.After(metric.ObservedAt) {
			return "", errors.New("metric has empty/duplicate instrument IDs or invalid quote times")
		}
		seen[uid] = true
		quotes[i] = quote{uid, timestamp(at)}
	}
	sort.Slice(quotes, func(i, j int) bool { return quotes[i].UID < quotes[j].UID })
	if curve.IsRelativeStrategy(metric.Strategy) {
		// Revised ownership, conversion or event terms are a distinct model
		// regime. They cannot warm each other's reference history even when the
		// underlying quote timestamps happen to be unchanged. Preserve the old
		// curve key encoding so existing history remains deduplicated on upgrade.
		raw, err := json.Marshal(struct {
			Unit        string  `json:"unit"`
			ModelSource string  `json:"model_source"`
			Quotes      []quote `json:"quotes"`
		}{metric.Unit, metric.ModelSource, quotes})
		return string(raw), err
	}
	raw, err := json.Marshal(quotes)
	return string(raw), err
}

func curveCandidatePairKey(candidate curve.Candidate) (string, error) {
	cashOffer := candidate.Strategy == curve.CashOffer
	if strings.TrimSpace(candidate.Strategy) == "" || candidate.Metric.Strategy != candidate.Strategy || (cashOffer && len(candidate.Legs) != 1) || (!cashOffer && len(candidate.Legs) < 2) {
		return "", errors.New("curve candidate requires matching strategy and instrument pair")
	}
	ids := make([]string, len(candidate.Legs))
	for i, leg := range candidate.Legs {
		ids[i] = leg.Instrument.UID
	}
	sort.Strings(ids)
	for i, id := range ids {
		if strings.TrimSpace(id) == "" || (i > 0 && id == ids[i-1]) {
			return "", errors.New("curve candidate has empty or duplicate instrument IDs")
		}
	}
	metricIDs := append([]string(nil), candidate.Metric.InstrumentUIDs...)
	sort.Strings(metricIDs)
	if len(metricIDs) != len(ids) {
		return "", errors.New("curve metric instruments differ from candidate legs")
	}
	for i, id := range ids {
		if metricIDs[i] != id {
			return "", errors.New("curve metric instruments differ from candidate legs")
		}
	}
	if cashOffer {
		if candidate.Relationship == nil || strings.TrimSpace(candidate.Relationship.ID) == "" {
			return "", errors.New("cash-offer candidate requires a stable event relationship ID")
		}
		raw, err := json.Marshal(struct {
			InstrumentUIDs []string `json:"instrument_uids"`
			EventID        string   `json:"event_id"`
		}{ids, candidate.Relationship.ID})
		return string(raw), err
	}
	raw, err := json.Marshal(ids)
	return string(raw), err
}
