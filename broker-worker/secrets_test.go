package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestBrokerSecret(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good.txt")
	loose := filepath.Join(dir, "loose.txt")
	if err := os.WriteFile(good, []byte("  s3cret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(loose, []byte("s3cret"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("T_SECRET_FILE", good)

	t.Setenv("LIVE_TRADING", "false")
	t.Setenv("T_SECRET", "from-env")
	if got := brokerSecret("T_SECRET"); got != "" {
		t.Errorf("LIVE_TRADING off: env secret must not load, got %q", got)
	}
	t.Setenv("T_SECRET", "")
	if got := brokerSecret("T_SECRET"); got != "" {
		t.Errorf("LIVE_TRADING off: file must not be read, got %q", got)
	}
	t.Setenv("LIVE_TRADING", "")
	if got := brokerSecret("T_SECRET"); got != "" {
		t.Errorf("LIVE_TRADING unset: nothing loads, got %q", got)
	}

	t.Setenv("LIVE_TRADING", "true")
	if got := brokerSecret("T_SECRET"); got != "s3cret" {
		t.Errorf("file secret should load trimmed, got %q", got)
	}
	t.Setenv("T_SECRET", "from-env")
	if got := brokerSecret("T_SECRET"); got != "from-env" {
		t.Errorf("env should win over file, got %q", got)
	}
	t.Setenv("T_SECRET", "")
	t.Setenv("T_SECRET_FILE", loose)
	if got := brokerSecret("T_SECRET"); got != "" {
		t.Errorf("group/world-readable file must be refused, got %q", got)
	}
	t.Setenv("T_SECRET_FILE", filepath.Join(dir, "missing.txt"))
	if got := brokerSecret("T_SECRET"); got != "" {
		t.Errorf("missing file means not configured, got %q", got)
	}
}
