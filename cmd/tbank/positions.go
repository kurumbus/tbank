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
	"tbank/internal/scanner"
	"tbank/internal/state"
)

const positionsUsage = `usage:
  tbank positions list [-state var/scanner.sqlite] [-source production|sandbox|fixture] [-status open|awaiting_settlement|closed]
  tbank positions show [-state var/scanner.sqlite] -id N
  tbank positions settle [-state var/scanner.sqlite] -id N -input settlement.json

Reads or updates local paper positions only. No broker connection is opened.
Use the exact .sandbox or .fixture database path when inspecting those runs.
Settlement requires verified terminal prices, a timestamp, and a reference.`

func cmdPositions(args []string) error {
	if len(args) == 0 {
		return usageError{msg: positionsUsage}
	}
	sub := args[0]
	if sub != "list" && sub != "show" && sub != "settle" {
		return usageError{msg: positionsUsage}
	}
	fs := flag.NewFlagSet("positions "+sub, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	path := fs.String("state", "var/scanner.sqlite", "exact state database path")
	var source, status, input string
	var id int64
	if sub == "list" {
		fs.StringVar(&source, "source", "", "filter source")
		fs.StringVar(&status, "status", "", "filter status")
	} else {
		fs.Int64Var(&id, "id", 0, "position id")
		if sub == "settle" {
			fs.StringVar(&input, "input", "", "verified settlement JSON")
		}
	}
	if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 0 {
		return usageError{msg: positionsUsage}
	}
	if sub != "list" && id <= 0 {
		return fmt.Errorf("a positive position -id is required")
	}
	if status != "" && status != scanner.PositionOpen && status != scanner.PositionAwaitingSettlement && status != scanner.PositionClosed {
		return fmt.Errorf("unknown position status %q", status)
	}
	if sub == "settle" && input == "" {
		return fmt.Errorf("settlement -input is required")
	}
	// A mistyped inspection path must not silently create an empty database.
	if _, err := os.Stat(*path); err != nil {
		return fmt.Errorf("position database: %w", err)
	}
	store, err := state.Open(*path)
	if err != nil {
		return err
	}
	defer store.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var output any
	switch sub {
	case "list":
		positions, err := store.ListPositions(ctx, source)
		if err != nil {
			return err
		}
		filtered := make([]scanner.Position, 0, len(positions))
		for _, p := range positions {
			if status == "" || p.Status == status {
				filtered = append(filtered, p)
			}
		}
		output = filtered
	case "show":
		p, err := store.GetPosition(ctx, id)
		if err != nil {
			return err
		}
		history, err := store.Observations(ctx, id)
		if err != nil {
			return err
		}
		output = struct {
			Position     scanner.Position `json:"position"`
			Observations any              `json:"observations"`
		}{p, history}
	case "settle":
		p, err := store.GetPosition(ctx, id)
		if err != nil {
			return err
		}
		data, err := readSettlement(input)
		if err != nil {
			return err
		}
		if p.Source != "fixture" && data.ObservedAt.After(time.Now().UTC()) {
			return fmt.Errorf("settlement timestamp cannot be in the future")
		}
		valuation, err := scanner.SettlePosition(p, data)
		if err != nil {
			return err
		}
		runID := uuid.NewString()
		if err := store.SaveRun(ctx, runID, time.Now().UTC(), p.Source, nil, []string{fmt.Sprintf("Manual verified mock settlement for position %d: %s", id, data.Reference)}); err != nil {
			return err
		}
		updated, changed, err := store.SettlePosition(ctx, runID, id, valuation)
		if err != nil {
			return err
		}
		output = struct {
			Position scanner.Position `json:"position"`
			Changed  bool             `json:"changed"`
		}{updated, changed}
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(output)
}

func readSettlement(path string) (scanner.SettlementInput, error) {
	var value scanner.SettlementInput
	f, err := os.Open(path)
	if err != nil {
		return value, err
	}
	defer f.Close()
	d := json.NewDecoder(io.LimitReader(f, 1<<20))
	d.DisallowUnknownFields()
	if err := d.Decode(&value); err != nil {
		return value, fmt.Errorf("settlement input: %w", err)
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return value, fmt.Errorf("settlement input must be one JSON object")
	}
	return value, nil
}
