package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"tbank/internal/curve"
	"tbank/internal/state"
)

const curvePositionsUsage = `usage:
  tbank curve-positions list [-state var/scanner.sqlite] [-source production|sandbox|fixture] [-status open|awaiting_settlement|closed]
  tbank curve-positions show [-state var/scanner.sqlite] -id N

Read local curve paper positions and their observation history.
Use the exact .sandbox or .fixture database path for those sources.
Missed expiry remains awaiting_settlement; no automatic or manual settlement
booking is implemented for multi-expiry curve positions in this version.`

func cmdCurvePositions(args []string) error {
	if len(args) == 0 || (args[0] != "list" && args[0] != "show") {
		return usageError{msg: curvePositionsUsage}
	}
	fs := flag.NewFlagSet("curve-positions "+args[0], flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	path := fs.String("state", "var/scanner.sqlite", "exact database path")
	var source, status string
	var id int64
	if args[0] == "list" {
		fs.StringVar(&source, "source", "", "filter source")
		fs.StringVar(&status, "status", "", "filter status")
	} else {
		fs.Int64Var(&id, "id", 0, "position id")
	}
	if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 0 {
		return usageError{msg: curvePositionsUsage}
	}
	if status != "" && status != "open" && status != "awaiting_settlement" && status != "closed" {
		return fmt.Errorf("unknown curve position status %q", status)
	}
	if args[0] == "show" && id <= 0 {
		return fmt.Errorf("a positive position -id is required")
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
