package main

import (
	"encoding/json"
	"net/http"
	"time"
)

// gordonConfigPrefix mirrors gordon's gordonConfigPrefix — the per-gordon editable
// config the Gordons tab writes and each gordon polls. One JSON blob per persona id,
// never TTL'd and never cleared on run reset, so operator tuning survives resets and
// new dry runs. Deleting the key ("Reset to default") reverts to compiled defaults.
const gordonConfigPrefix = "gekko:gordon_config:"

// gordonConfigBlob is the sparse override stored per gordon. Empty/zero fields fall
// through to the compiled persona registry defaults inside gordon.
type gordonConfigBlob struct {
	DisplayName    string  `json:"display_name"`
	Nudge          string  `json:"nudge"`
	Stance         string  `json:"stance"`
	RiskTier       string  `json:"risk_tier"`
	MatchThreshold float64 `json:"match_threshold"`
	Model          string  `json:"model"`
	Provider       string  `json:"provider"`
	UpdatedAtMs    int64   `json:"updated_at_ms"`
}

const (
	maxNudgeLen = 2000
	maxNameLen  = 64
)

var validStances = map[string]bool{"bearish": true, "neutral": true, "bullish": true}
var validRiskTiers = map[string]bool{"conservative": true, "cautious": true, "balanced": true, "assertive": true, "aggressive": true}

// handleGordons serves (GET) and edits (POST) the per-gordon editable config. The GET
// merges the live fleet roster with each gordon's stored override + generated name, so
// the Gordons scorecard lists exactly the running fleet and knows which knobs the
// operator has customized. Effective (default-or-overridden) values for display come
// from /api/status (the running gordons publish them); this endpoint owns the override
// store + names.
func handleGordons(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	if redisClient == nil {
		w.Write([]byte(`{"gordons":[]}`))
		return
	}

	switch r.Method {
	case http.MethodGet:
		roster := gordonRoster(r.Context())
		out := make([]map[string]interface{}, 0, len(roster))
		for _, entry := range roster {
			id := entry["id"]
			g := map[string]interface{}{
				"id":         id,
				"name":       gordonName(id),
				"overridden": false,
			}
			if val, err := redisClient.Get(r.Context(), gordonConfigPrefix+id).Result(); err == nil && val != "" {
				var blob gordonConfigBlob
				if json.Unmarshal([]byte(val), &blob) == nil {
					g["overridden"] = true
					g["config"] = blob
					if blob.DisplayName != "" {
						g["name"] = blob.DisplayName
					}
				}
			}
			out = append(out, g)
		}
		blob, _ := json.Marshal(map[string]interface{}{"gordons": out})
		w.Write(blob)

	case http.MethodPost:
		var in struct {
			PersonaID string   `json:"persona_id"`
			gordonConfigBlob
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16*1024)).Decode(&in); err != nil {
			http.Error(w, "Bad request JSON", http.StatusBadRequest)
			return
		}
		if in.PersonaID == "" || len(in.PersonaID) > maxNameLen {
			http.Error(w, "invalid persona_id", http.StatusBadRequest)
			return
		}
		// Validate the sparse override. Empty fields are allowed (⇒ use the compiled
		// default); non-empty ones must be in range so a bad write can't wedge a gordon.
		if in.Stance != "" && !validStances[in.Stance] {
			http.Error(w, "invalid stance", http.StatusBadRequest)
			return
		}
		if in.RiskTier != "" && !validRiskTiers[in.RiskTier] {
			http.Error(w, "invalid risk_tier", http.StatusBadRequest)
			return
		}
		if in.MatchThreshold != 0 && (in.MatchThreshold < 0.35 || in.MatchThreshold > 0.80) {
			http.Error(w, "match_threshold out of range [0.35, 0.80]", http.StatusBadRequest)
			return
		}
		if len(in.Nudge) > maxNudgeLen {
			http.Error(w, "nudge too long", http.StatusBadRequest)
			return
		}
		if len(in.DisplayName) > maxNameLen || len(in.Model) > maxNameLen || len(in.Provider) > maxNameLen {
			http.Error(w, "field too long", http.StatusBadRequest)
			return
		}
		blob := in.gordonConfigBlob
		blob.UpdatedAtMs = time.Now().UnixMilli()
		raw, _ := json.Marshal(blob)
		if err := redisClient.Set(r.Context(), gordonConfigPrefix+in.PersonaID, string(raw), 0).Err(); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Write([]byte(`{"ok":true}`))

	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleGordonsReset deletes a gordon's stored config ("nuke to compiled default").
func handleGordonsReset(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if redisClient == nil {
		http.Error(w, "no redis", http.StatusServiceUnavailable)
		return
	}
	var in struct {
		PersonaID string `json:"persona_id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&in); err != nil {
		http.Error(w, "Bad request JSON", http.StatusBadRequest)
		return
	}
	if in.PersonaID == "" || len(in.PersonaID) > maxNameLen {
		http.Error(w, "invalid persona_id", http.StatusBadRequest)
		return
	}
	if err := redisClient.Del(r.Context(), gordonConfigPrefix+in.PersonaID).Err(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Write([]byte(`{"ok":true}`))
}
