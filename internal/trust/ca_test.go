package trust

import (
	"os"
	"testing"
)

func TestBundledRootFingerprint(t *testing.T) {
	if err := CheckRoot(); err != nil {
		t.Fatal(err)
	}
}

func TestCAFileWritesBundle(t *testing.T) {
	path, cleanup, err := CAFile("")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
}

func TestCAFileOverride(t *testing.T) {
	path, cleanup, err := CAFile("custom.pem")
	if err != nil {
		t.Fatal(err)
	}
	cleanup()
	if path != "custom.pem" {
		t.Fatalf("path = %s", path)
	}
}
