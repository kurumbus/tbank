package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHeartbeatLineStaysSingleLineAndRecordsQuietPasses(t *testing.T) {
	line := heartbeatLine("relative-scan", "sandbox", "error", "metrics=12 candidates=11 opened=0 marked=0 closed=0 awaiting=0 notifications=0 open=3 errors=3", "verified perpetual\nfunding coverage")
	if strings.Contains(line, "\n") || !strings.Contains(line, "notifications=0") || !strings.Contains(line, "open=3") || !strings.Contains(line, "error=verified perpetual funding coverage") {
		t.Fatalf("heartbeat %q", line)
	}
	long := heartbeatLine("scan", "sandbox", "ok", "evaluated=0 notifications=0 open=0 errors=0", strings.Repeat("x", 400))
	if strings.Contains(long, "\n") || !strings.HasSuffix(strings.TrimSpace(long), "…") && !strings.Contains(long, "…") {
		t.Fatal("long error was not bounded to one line")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "heartbeat.log")
	if err := appendHeartbeat(path, line); err != nil {
		t.Fatal(err)
	}
	if err := appendHeartbeat(path, line); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(body), "relative-scan sandbox error") != 2 {
		t.Fatalf("log %s", body)
	}
	open, awaiting := positionActivity([]string{"open", "closed", "awaiting_settlement", "open"})
	if open != 2 || awaiting != 1 {
		t.Fatalf("open=%d awaiting=%d", open, awaiting)
	}
}
