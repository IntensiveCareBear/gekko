package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// handleLouLessons serves Lou's lesson book (runs/lou/, written by the lou CLI —
// see docs/lou-learning-loop-proposal.md) to the dashboard: current lessons,
// the tail of the auto-tune history, and any active overrides. Everything is
// optional — before Lou's first pass the response is just empty collections, so
// the frontend can render a "hasn't run yet" state instead of erroring.
func handleLouLessons(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Content-Type", "application/json")

	louDir := filepath.Join(getEnv("RUN_EXPORT_DIR", "./runs"), "lou")
	resp := struct {
		Lessons     json.RawMessage   `json:"lessons"`
		Tuning      []json.RawMessage `json:"tuning"`
		Overrides   map[string]string `json:"overrides"`
		GeneratedAt string            `json:"generated_at,omitempty"`
	}{Lessons: json.RawMessage("[]"), Tuning: []json.RawMessage{}, Overrides: map[string]string{}}

	if raw, err := os.ReadFile(filepath.Join(louDir, "lessons.json")); err == nil && json.Valid(raw) {
		resp.Lessons = raw
		if st, err := os.Stat(filepath.Join(louDir, "lessons.json")); err == nil {
			resp.GeneratedAt = st.ModTime().UTC().Format("2006-01-02T15:04:05Z")
		}
	}

	// Last 20 auto-tune events, newest first.
	if raw, err := os.ReadFile(filepath.Join(louDir, "tuning_history.jsonl")); err == nil {
		lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
		for i := len(lines) - 1; i >= 0 && len(resp.Tuning) < 20; i-- {
			if json.Valid([]byte(lines[i])) {
				resp.Tuning = append(resp.Tuning, json.RawMessage(lines[i]))
			}
		}
	}

	if raw, err := os.ReadFile(filepath.Join(louDir, "overrides.env")); err == nil {
		for _, line := range strings.Split(string(raw), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			if k, v, ok := strings.Cut(line, "="); ok {
				resp.Overrides[k] = v
			}
		}
	}

	_ = json.NewEncoder(w).Encode(resp)
}

// louAutonomyLevels is the ladder the dashboard offers, lowest rope first. Kept in
// sync with lou/autonomy.go — Lou is the authority on what each level permits; this
// list only decides what the UI may ask for.
var louAutonomyLevels = map[string]bool{"observe": true, "tune": true, "intervene": true}

// handleLouAutonomy gets/sets how much rope Lou has: "observe" (analyse only), "tune"
// (bounded numeric knobs) or "intervene" (also bench personas and blacklist families).
//
// The level is stored as runs/lou/autonomy, a one-word file — the same directory Lou
// already owns, and readable by a CLI that has no Redis client and runs from launchd
// once a night. Which also means a change here takes effect at Lou's NEXT pass, not
// immediately; the UI says so.
func handleLouAutonomy(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Content-Type", "application/json")

	louDir := filepath.Join(getEnv("RUN_EXPORT_DIR", "./runs"), "lou")
	path := filepath.Join(louDir, "autonomy")

	if r.Method == http.MethodPost {
		var body struct {
			Level string `json:"level"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, `{"error":"bad request"}`, http.StatusBadRequest)
			return
		}
		lvl := strings.ToLower(strings.TrimSpace(body.Level))
		if !louAutonomyLevels[lvl] {
			http.Error(w, `{"error":"level must be observe, tune or intervene"}`, http.StatusBadRequest)
			return
		}
		if err := os.MkdirAll(louDir, 0o755); err != nil {
			http.Error(w, `{"error":"could not write level"}`, http.StatusInternalServerError)
			return
		}
		if err := os.WriteFile(path, []byte(lvl+"\n"), 0o644); err != nil {
			http.Error(w, `{"error":"could not write level"}`, http.StatusInternalServerError)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"level": lvl, "applies": "next nightly pass"})
		return
	}

	// GET. No file yet means nobody has chosen: report "observe", which is also what
	// Lou falls back to when the legacy -autotune flag is absent.
	level := "observe"
	if raw, err := os.ReadFile(path); err == nil {
		if v := strings.ToLower(strings.TrimSpace(string(raw))); louAutonomyLevels[v] {
			level = v
		}
	}
	json.NewEncoder(w).Encode(map[string]any{"level": level, "applies": "next nightly pass"})
}
