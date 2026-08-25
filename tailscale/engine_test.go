package tailscale

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadOrCreateLogIdPersists(t *testing.T) {
	dir := t.TempDir()

	id1, err := loadOrCreateLogID(dir)
	if err != nil {
		t.Fatalf("first load: %v", err)
	}

	id2, err := loadOrCreateLogID(dir)
	if err != nil {
		t.Fatalf("second load: %v", err)
	}

	if id1 != id2 {
		t.Fatalf("log id not stable across loads: %v != %v", id1, id2)
	}
}

func TestLoadOrCreateLogIdNoDirGeneratesFresh(t *testing.T) {
	id, err := loadOrCreateLogID("")
	if err != nil {
		t.Fatalf("load with empty dir: %v", err)
	}
	if id.IsZero() {
		t.Fatal("expected non-zero generated log id")
	}
}

func TestDeadLogId(t *testing.T) {
	id := deadLogID()
	if id.IsZero() {
		t.Fatal("dead log id must not be zero")
	}
	txt, _ := id.MarshalText()
	if len(txt) == 0 {
		t.Fatal("dead log id must marshal")
	}
}

func TestStateDirIsCreated(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "nested", "ts")
	if _, err := loadOrCreateLogID(dir); err != nil {
		// creation of the log id may fail only if the dir cannot be made
		t.Logf("logid err: %v", err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("state dir not created: %v", err)
	}
}
