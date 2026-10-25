package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/google/uuid"
	"tbank/internal/curve"
	"tbank/internal/state"
)

const curvePositionsUsage = `usage:
  tbank curve-positions list [-state var/scanner.sqlite] [-source production|sandbox|fixture] [-status open|awaiting_settlement|closed]
  tbank curve-positions show [-state var/scanner.sqlite] -id N
  tbank curve-positions settle [-state var/scanner.sqlite] -id N -input settlement.json

Read local curve/relative paper positions and their observation history.
Use the exact .sandbox or .fixture database path for those sources.
Settlement records verified full acceptance and actual payment for cash offers only.
Missed futures expiry remains awaiting_settlement; no futures settlement is invented.`

func cmdCurvePositions(args []string) error {
	if len(args) == 0 || (args[0] != "list" && args[0] != "show" && args[0] != "settle") {
		return usageError{msg: curvePositionsUsage}
	}
	fs := flag.NewFlagSet("curve-positions "+args[0], flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	path := fs.String("state", "var/scanner.sqlite", "exact database path")
	var source, status, input string
	var id int64
	if args[0] == "list" {
		fs.StringVar(&source, "source", "", "filter source")
		fs.StringVar(&status, "status", "", "filter status")
	} else {
		fs.Int64Var(&id, "id", 0, "position id")
		if args[0] == "settle" {
			fs.StringVar(&input, "input", "", "verified cash-offer settlement JSON")
		}
	}
	if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 0 {
		return usageError{msg: curvePositionsUsage}
	}
	if status != "" && status != "open" && status != "awaiting_settlement" && status != "closed" {
		return fmt.Errorf("unknown curve position status %q", status)
	}
	if args[0] != "list" && id <= 0 {
		return fmt.Errorf("a positive position -id is required")
	}
	if args[0] == "settle" && input == "" {
		return fmt.Errorf("settlement -input is required")
	}
	if _, err := os.Stat(*path); err != nil {
		return fmt.Errorf("curve position database: %w", err)
	}
	store, err := state.Open(*path)
	if err != nil {
		return err
	}
	defer store.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var output any
	if args[0] == "list" {
		positions, e := store.ListCurvePositions(ctx, source)
		if e != nil {
			return e
		}
		filtered := []curve.Position{}
		for _, p := range positions {
			if status == "" || p.Status == status {
				filtered = append(filtered, p)
			}
		}
		output = filtered
	} else if args[0] == "settle" {
		p, e := store.GetCurvePosition(ctx, id)
		if e != nil {
			return e
		}
		data, e := readRelativeSettlement(input)
		if e != nil {
			return e
		}
		if p.Source != "fixture" && data.ObservedAt.After(time.Now().UTC()) {
			return fmt.Errorf("settlement timestamp cannot be in the future")
		}
		if p.Status != curve.PositionClosed {
			if _, e = curve.SettleRelativePosition(p, data); e != nil {
				return e
			}
		}
		runID := uuid.NewString()
		if e = store.SaveRun(ctx, runID, time.Now().UTC(), p.Source, nil, []string{fmt.Sprintf("Verified mock cash-offer settlement for position %d: %s", id, data.Reference)}); e != nil {
			return e
		}
		updated, changed, e := store.SettleRelativePosition(ctx, runID, id, data)
		if e != nil {
			return e
		}
		output = struct {
			Position curve.Position `json:"position"`
			Changed  bool           `json:"changed"`
		}{updated, changed}
	} else {
		p, e := store.GetCurvePosition(ctx, id)
		if e != nil {
			return e
		}
		history, e := store.CurveObservations(ctx, id)
		if e != nil {
			return e
		}
		output = struct {
			Position     curve.Position `json:"position"`
			Observations any            `json:"observations"`
		}{p, history}
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(output)
}

func readRelativeSettlement(path string) (curve.RelativeSettlement, error) {
	var value curve.RelativeSettlement
	f, err := os.Open(path)
	if err != nil {
		return value, err
	}
	defer f.Close()
	d := json.NewDecoder(io.LimitReader(f, 1<<20))
	d.DisallowUnknownFields()
	if err = d.Decode(&value); err != nil {
		return value, fmt.Errorf("cash-offer settlement input: %w", err)
	}
	var extra any
	if err = d.Decode(&extra); err != io.EOF {
		return value, fmt.Errorf("settlement input must be one JSON object")
	}
	return value, nil
}
