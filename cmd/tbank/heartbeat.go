package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"tbank/internal/curve"
	"tbank/internal/scanner"
)

// recordHeartbeat appends one UTF-8 line for a live scan. Fixture runs stay
// out of the log. A log failure does not change the scan result.
func recordHeartbeat(command, source, result, stats, firstError string) {
	line := heartbeatLine(command, source, result, stats, firstError)
	fmt.Fprintln(os.Stderr, line)
	if source == "fixture" {
		return
	}
	if err := appendHeartbeat("var/heartbeat.log", line); err != nil {
		fmt.Fprintln(os.Stderr, "heartbeat log:", err.Error())
	}
}

func heartbeatLine(command, source, result, stats, firstError string) string {
	line := fmt.Sprintf("%s %s %s %s %s", time.Now().Format(time.RFC3339), command, source, result, stats)
	if firstError != "" {
		line += " error=" + oneLine(firstError, 160)
	}
	return line
}

func appendHeartbeat(path, line string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = fmt.Fprintln(f, line)
	return err
}

func oneLine(s string, limit int) string {
	s = strings.Join(strings.Fields(s), " ")
	if s == "" {
		return "unknown"
	}
	if utf8.RuneCountInString(s) <= limit {
		return s
	}
	runes := []rune(s)
	return string(runes[:limit-1]) + "…"
}

func positionActivity(statuses []string) (open, awaiting int) {
	for _, status := range statuses {
		switch status {
		case "open":
			open++
		case "awaiting_settlement":
			awaiting++
		}
	}
	return open, awaiting
}

func scanStatuses(positions []scanner.Position) []string {
	out := make([]string, len(positions))
	for i, position := range positions {
		out[i] = position.Status
	}
	return out
}

func curveStatuses(positions []curve.Position) []string {
	out := make([]string, len(positions))
	for i, position := range positions {
		out[i] = position.Status
	}
	return out
}
