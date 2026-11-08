package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The autonomy control has to round-trip through the same file Lou reads at 01:00, or
// the dashboard is lying about what it changed.
func TestHandleLouAutonomy_RoundTrip(t *testing.T) {
	root := t.TempDir()
	t.Setenv("RUN_EXPORT_DIR", root)

	get := func() map[string]any {
		rec := httptest.NewRecorder()
		handleLouAutonomy(rec, httptest.NewRequest(http.MethodGet, "/api/lou/autonomy", nil))
		var out map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("GET returned non-JSON %q: %v", rec.Body.String(), err)
		}
		return out
	}

	// Nothing chosen yet reads as observe — the same fallback Lou uses.
	if lvl := get()["level"]; lvl != "observe" {
		t.Errorf("unset level = %v, want observe", lvl)
	}

	post := func(body string) int {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/lou/autonomy", strings.NewReader(body))
		handleLouAutonomy(rec, req)
		return rec.Code
	}

	if code := post(`{"level":"intervene"}`); code != http.StatusOK {
		t.Fatalf("POST intervene returned %d", code)
	}
	if lvl := get()["level"]; lvl != "intervene" {
		t.Errorf("after POST, level = %v, want intervene", lvl)
	}
	// The file is the contract with Lou: one word, in runs/lou/autonomy.
	raw, err := os.ReadFile(filepath.Join(root, "lou", "autonomy"))
	if err != nil {
		t.Fatalf("autonomy file not written: %v", err)
	}
	if strings.TrimSpace(string(raw)) != "intervene" {
		t.Errorf("file contains %q, want intervene", raw)
	}

	// Garbage must be refused rather than silently handing Lou a level it will ignore.
	if code := post(`{"level":"full-send"}`); code != http.StatusBadRequest {
		t.Errorf("unknown level returned %d, want 400", code)
	}
	if code := post(`not json`); code != http.StatusBadRequest {
		t.Errorf("malformed body returned %d, want 400", code)
	}
	// ...and a refused write must not have changed the stored level.
	if lvl := get()["level"]; lvl != "intervene" {
		t.Errorf("level changed on a refused POST: %v", lvl)
	}
}
