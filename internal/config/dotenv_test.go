package config

import (
	"strings"
	"testing"
)

func TestParseDotEnv(t *testing.T) {
	input := `
# comment
TBANK_API_TOKEN=abc
export TBANK_ENDPOINT="sandbox.example:443"
EMPTY=
`
	got, err := parseDotEnv(strings.NewReader(input))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got["TBANK_API_TOKEN"] != "abc" {
		t.Fatalf("token = %q", got["TBANK_API_TOKEN"])
	}
	if got["TBANK_ENDPOINT"] != "sandbox.example:443" {
		t.Fatalf("endpoint = %q", got["TBANK_ENDPOINT"])
	}
	if got["EMPTY"] != "" {
		t.Fatalf("empty = %q", got["EMPTY"])
	}
}

func TestParseDotEnvRejectsMissingEquals(t *testing.T) {
	_, err := parseDotEnv(strings.NewReader("NOT_A_PAIR\n"))
	if err == nil {
		t.Fatal("expected error")
	}
}
